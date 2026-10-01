// Package runner executes PowerShell scripts on a remote Windows host.
//
// It is the transport layer of the provider. Nothing above it knows whether
// a script travels over SSH or WinRM; they only see the Runner interface.
//
// Where it sits in the request flow:
//
//	Terraform -> provider -> resources/datasources -> dhcp -> psscript + runner -> Windows host
//
// The dhcp package hands a wrapped script and a parameters object to
// Runner.Run. The runner renders the script (psscript.Render), turns it into
// a fixed powershell.exe command line plus a base64 stdin payload
// (psscript.Command), executes that on the host, and returns the decoded
// JSON envelope for dhcp to parse with psscript.Parse.
//
// Files:
//   - runner.go: the Runner interface, the concurrency limiter, the shared
//     render/execute/decode logic, and CLIXML stderr cleanup.
//   - ssh.go: SSH transport (OpenSSH server on Windows), with host key
//     verification and a reused connection.
//   - winrm.go: WinRM transport (NTLM, Basic or Kerberos, HTTP or HTTPS).
//   - runner_test.go: unit tests, including an in-process fake SSH server.
package runner

// Go note: imports with a full URL-like path (github.com/...) are other Go
// modules or packages from this repository; short ones are the standard
// library.
import (
	"context"
	"fmt"
	"html"
	"regexp"
	"strings"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/psscript"
)

// Runner executes a PowerShell script with a JSON parameters object and
// returns the decoded stdout (the script's JSON envelope). Implementations
// must be safe for concurrent use, because Terraform creates and reads many
// resources in parallel.
//
// Go note: an interface lists method signatures. Any type that has a
// matching Run method satisfies Runner automatically, without declaring it.
// That is how *SSH, *WinRM, the limiter below and test fakes all count as
// Runners.
// Go note: context.Context carries cancellation and deadlines. When the user
// hits Ctrl-C or a Terraform timeout fires, ctx is cancelled and the
// transport should stop waiting. By convention it is the first parameter.
type Runner interface {
	Run(ctx context.Context, script string, params any) ([]byte, error)
}

// Limit wraps r so that at most n scripts run at the same time. This keeps
// Terraform's parallelism (10 by default) from opening more remote
// PowerShell processes than the host or its SSH/WinRM quotas allow.
//
// Go note: a "chan struct{}" with a buffer of n works as a counting
// semaphore: sending takes a slot (blocks when all n are taken), receiving
// gives one back. struct{} is an empty value that uses no memory.
// make(chan T, n) creates the channel with n buffer slots.
func Limit(r Runner, n int) Runner {
	if n < 1 {
		n = 1
	}
	// Go note: "&limited{...}" builds a limited value and returns a pointer
	// to it. The pointer type *limited is what has the Run method.
	return &limited{r: r, sem: make(chan struct{}, n)}
}

// limited is the Runner returned by Limit. It forwards to the inner Runner
// r once it holds a slot in sem.
type limited struct {
	r   Runner
	sem chan struct{}
}

// Run waits for a free slot (or for ctx to be cancelled), runs the script,
// then releases the slot.
//
// Go note: "func (l *limited) Run(...)" is a method with a pointer receiver
// l, much like $this in a PowerShell class method.
func (l *limited) Run(ctx context.Context, script string, params any) ([]byte, error) {
	// Go note: select waits on several channel operations and runs whichever
	// is ready first. Here: either we get a slot, or ctx is cancelled
	// (ctx.Done() is a channel that closes on cancellation), whichever
	// happens first.
	select {
	case l.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// Go note: defer schedules a call to run when this function returns, on
	// every path (success, error or panic). Here it frees the slot.
	defer func() { <-l.sem }()
	return l.r.Run(ctx, script, params)
}

// execFunc runs cmdline on the remote host with stdin and returns stdout,
// stderr and the exit code. Each transport (SSH, WinRM) provides one; it is
// the only transport-specific piece of a Run. A non-zero exit code is not an
// error here: err is reserved for transport failures (cannot connect,
// authentication failed, cancelled).
//
// Go note: in Go, functions are values. This declares a named function type
// so execute can accept "any function with this signature".
type execFunc func(ctx context.Context, cmdline, stdin string) (stdout, stderr []byte, exitCode int, err error)

// execute is the transport independent part of Run: render, run the
// bootstrap, decode the output. transport ("ssh" or "winrm") only prefixes
// error messages so users can tell which path failed.
func execute(ctx context.Context, transport string, ex execFunc, script string, params any) ([]byte, error) {
	// Step 1: embed the parameters into the script (see psscript.Render).
	rendered, err := psscript.Render(script, params)
	// Go note: "if err != nil { return ... }" is how Go propagates errors:
	// check after every call that can fail, return early on failure.
	if err != nil {
		return nil, err
	}
	// Step 2: build the fixed powershell.exe command line and stdin payload.
	cmdline, stdin := psscript.Command(rendered)
	// Step 3: run it on the host.
	stdout, stderr, code, err := ex(ctx, cmdline, stdin)
	if err != nil {
		// Go note: %w wraps err inside the new error, so callers can still
		// match the original with errors.Is (for example context.Canceled).
		return nil, fmt.Errorf("%s: running PowerShell: %w", transport, err)
	}
	// Step 4: a script error is normally reported inside the JSON envelope
	// with exit code 0. A non-zero exit or empty stdout means PowerShell
	// itself failed (parse error, crash, missing module), so report stderr.
	if code != 0 || len(strings.TrimSpace(string(stdout))) == 0 {
		return nil, fmt.Errorf("%s: PowerShell exited with code %d: %s", transport, code, CleanStderr(string(stderr)))
	}
	// Step 5: undo the bootstrap's base64 encoding to get the JSON envelope.
	return psscript.DecodeOutput(stdout)
}

// clixmlString matches the error text items in a CLIXML stream. (?s) lets
// "." match newlines and ".*?" is non-greedy, so each <S S="Error"> element
// is captured separately.
//
// Go note: regexp.MustCompile panics (crashes) on an invalid pattern instead
// of returning an error. That is fine for a constant pattern, because it
// fails at program start, never at run time with user data.
var clixmlString = regexp.MustCompile(`(?s)<S S="Error">(.*?)</S>`)

// CleanStderr turns PowerShell's CLIXML error stream into plain text.
//
// When powershell.exe runs non-interactively with redirected streams, it
// writes its error stream as CLIXML (serialised PowerShell objects), a block
// starting with "#< CLIXML" followed by XML. That is unreadable in a
// Terraform error, so this extracts the <S S="Error"> text, turns the
// encoded CRLF (_x000D__x000A_) back into newlines and unescapes XML
// entities such as &amp;. Plain (non-CLIXML) stderr is returned as is.
func CleanStderr(s string) string {
	s = strings.TrimSpace(s)
	// Plain text case.
	if !strings.HasPrefix(s, "#< CLIXML") {
		if s == "" {
			return "(no error output)"
		}
		return s
	}
	// CLIXML case: concatenate the text of every error element.
	// FindAllStringSubmatch with -1 returns all matches; m[1] is the first
	// capture group (the text between the tags).
	var b strings.Builder
	for _, m := range clixmlString.FindAllStringSubmatch(s, -1) {
		part := strings.ReplaceAll(m[1], "_x000D__x000A_", "\n")
		b.WriteString(html.UnescapeString(part))
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "(no error output)"
	}
	return out
}
