// Package psscript builds the PowerShell text that is sent to the Windows
// host, and parses the JSON answer that comes back.
//
// It implements the "Script contract" from docs/DESIGN.md: every script runs
// inside a try/catch and prints exactly one JSON envelope of the form
// {"ok": true, "data": ...} or {"ok": false, "error": {...}}.
//
// Where it sits in the request flow:
//
//	Terraform -> provider -> resources/datasources -> dhcp -> psscript + runner -> Windows host
//
// The dhcp package owns the actual cmdlet bodies (Get-DhcpServerv4Scope and
// so on). It asks this package to wrap them (Script.Wrap). The runner package
// then calls Render and Command to turn the wrapped script plus the user's
// parameters into a powershell.exe command line and a stdin payload, sends
// them over SSH or WinRM, and calls DecodeOutput. Finally dhcp calls Parse to
// turn the JSON envelope into Go values or a typed *Error.
//
// Files:
//   - psscript.go: script wrapping, parameter rendering, the base64
//     bootstrap command line, and envelope/error parsing.
//   - psscript_test.go: unit tests for all of the above, using JSON
//     fixtures in testdata/.
package psscript

// Go note: an import block lists the packages this file uses. Standard
// library packages have short paths such as "strings"; there is no implicit
// global namespace, so every helper is called as package.Function(...).
import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
)

// ErrNotFound is matched (via errors.Is) by script errors that mean the
// requested object does not exist on the DHCP server. Resources use it in
// Read to drop an object from Terraform state (drift) instead of failing.
//
// Go note: names starting with a capital letter (ErrNotFound, Script, Parse)
// are "exported", meaning other packages can use them. Lower-case names
// (notFoundIDs, truncate) are private to this package.
// Go note: errors.New creates a simple error value. A package-level error
// like this is a "sentinel": callers compare against it with errors.Is.
var ErrNotFound = errors.New("object not found")

// notFoundIDs are API error codes that mean "does not exist". The DhcpServer
// cmdlets surface them in FullyQualifiedErrorId as "DHCP <code>,<cmdlet>",
// the DnsServer cmdlets as "WIN32 <code>,<cmdlet>" (Win32 DNS error codes).
// Some cmdlets report these with a generic category instead of
// ObjectNotFound, so the category alone is not enough.
//
// Go note: []string{...} is a "slice" literal, Go's growable list type.
var notFoundIDs = []string{
	"DHCP 20005", // ERROR_DHCP_SUBNET_NOT_PRESENT
	"DHCP 20010", // ERROR_DHCP_OPTION_NOT_PRESENT
	"DHCP 20018", // ERROR_DHCP_NOT_RESERVED_CLIENT
	"WIN32 9601", // DNS_ERROR_ZONE_DOES_NOT_EXIST
	"WIN32 9701", // DNS_ERROR_RECORD_DOES_NOT_EXIST
	"WIN32 9714", // DNS_ERROR_NAME_DOES_NOT_EXIST
}

// opPrefix starts the first line of every wrapped script. The line is a
// PowerShell comment, so it does nothing on the host, but it names the
// operation (for example "# windowsddi:scope.get") so test fakes can tell
// which script they received without parsing PowerShell.
const opPrefix = "# windowsddi:"

// Script is a named PowerShell body. Body must assign its result to $out and
// may use $p (parameters) and @cn (ComputerName splat). Anything else it
// writes to the pipeline is discarded.
//
// Op is a short dotted name such as "scope.get". It ends up in the op marker
// line (see opPrefix) and is only used for identification, never executed.
//
// Go note: a "struct" is a record type with named fields, similar to a
// PowerShell [pscustomobject] with a fixed shape.
type Script struct {
	Op   string
	Body string
	// Requires, when set, makes the wrapped script check that a PowerShell
	// module is available before running Body. The service clients (dhcp,
	// dns) set it so a missing role or RSAT feature is reported clearly.
	Requires *Requirement
}

// Requirement describes the PowerShell module a script depends on. Probe is
// a cmdlet of that module: if Get-Command cannot find it, the script throws
// an error built from Module and Hint instead of running its body. The check
// uses Get-Command rather than Get-Module -ListAvailable because it also sees
// modules that are imported but not installed in a module path.
//
// All three fields are fixed strings chosen by the provider, never user
// input, so embedding them in single quotes is safe.
//
// The thrown error deliberately has a generic category (not ObjectNotFound),
// so it can never be mistaken for ErrNotFound and silently drop resources
// from state.
type Requirement struct {
	Module string
	Probe  string
	Hint   string
}

// Op returns the operation name embedded in a rendered or wrapped script, or
// "" when there is none. Test fakes use it to dispatch. It only looks at the
// first line, so after Render (which puts the preamble first) it returns "".
func Op(script string) string {
	// Go note: functions can return several values. strings.Cut returns
	// (before, after, found); "_" is the blank identifier and discards the
	// values we do not need. ":=" declares a new variable and assigns it.
	first, _, _ := strings.Cut(script, "\n")
	// Go note: "if x, ok := f(); ok { ... }" declares variables that only
	// live inside the if block, then tests the condition.
	if op, ok := strings.CutPrefix(first, opPrefix); ok {
		return strings.TrimSpace(op)
	}
	return ""
}

// Wrap turns a Script into the source passed to a Runner: an op marker line,
// then the body inside the try/catch that prints the JSON envelope.
//
// The PowerShell produced looks like this:
//   - "$out = $null" so a body that never sets $out yields data = null.
//   - "$null = . { <body> }" dot-sources the body in the current scope (so
//     its assignment to $out is visible afterwards) and throws away anything
//     the body writes to the pipeline. Without this, stray output (for
//     example from an Add-* cmdlet) would corrupt the single JSON envelope.
//   - On success it prints @{ ok = $true; data = $out } as compressed JSON,
//     with -Depth 8 so nested objects are not truncated to type names.
//   - On failure the catch block prints ok = $false plus the exception
//     message, the error category (for example ObjectNotFound), the
//     FullyQualifiedErrorId (where "DHCP 20005,..." codes appear) and the
//     name of the failing cmdlet. Each value is forced to a string with
//     "$(...)" so enums serialise as names, not integers.
//
// Go note: "func (s Script) Wrap()" is a method: a function attached to the
// Script type. "s" is the receiver, like $this in a PowerShell class.
func (s Script) Wrap() string {
	// Go note: strings.Builder efficiently concatenates many strings. "var b
	// T" declares b with T's zero value, which for Builder is ready to use.
	var b strings.Builder
	b.WriteString(opPrefix + s.Op + "\n")
	// Go note: text between backticks is a raw string literal: no escape
	// sequences, and newlines are kept. Ideal for embedding PowerShell.
	b.WriteString(`try {
    $out = $null
`)
	if r := s.Requires; r != nil {
		b.WriteString("    if (-not (Get-Command -Name '" + r.Probe + "' -ErrorAction SilentlyContinue)) { throw 'The " +
			r.Module + " PowerShell module is not available on this host. " + r.Hint + "' }\n")
	}
	b.WriteString(`    $null = . {
`)
	b.WriteString(s.Body)
	b.WriteString(`
    }
    ConvertTo-Json -InputObject @{ ok = $true; data = $out } -Depth 8 -Compress
} catch {
    $e = $_
    ConvertTo-Json -InputObject @{ ok = $false; error = @{
        message  = "$($e.Exception.Message)"
        category = "$($e.CategoryInfo.Category)"
        id       = "$($e.FullyQualifiedErrorId)"
        command  = "$($e.InvocationInfo.MyCommand.Name)"
    } } -Compress
}
`)
	return b.String()
}

// Render prepends the parameter preamble to a wrapped script. params is
// marshalled to JSON and embedded as a single-quoted PowerShell literal with
// single quotes doubled; it is the only place user data enters the script.
//
// This is the security boundary against PowerShell injection: user values
// are never spliced into PowerShell source as code. Inside a single-quoted
// PowerShell string nothing is expanded ($var, $(...), backticks are all
// literal), and the only way to end the string is a quote character, which
// is why every quote character is doubled.
//
// Go note: "any" means a value of any type (an alias for interface{}). The
// function returns two values, the script and an error; callers must check
// the error before using the script.
func Render(script string, params any) (string, error) {
	// Step 1: treat "no parameters" as an empty JSON object so $p is always
	// a valid object on the PowerShell side.
	// Go note: nil is Go's "nothing" value, like $null.
	if params == nil {
		params = map[string]any{}
	}
	// Step 2: encode the parameters as JSON.
	raw, err := json.Marshal(params)
	// Go note: this is Go's standard error handling. Functions return an
	// error value; nil means success. There are no exceptions to catch.
	// fmt.Errorf with %w "wraps" the original error so callers can still
	// inspect it with errors.Is / errors.As.
	if err != nil {
		return "", fmt.Errorf("encoding script parameters: %w", err)
	}
	// Step 3: escape the JSON for use inside a PowerShell single-quoted
	// string by doubling every single quote.
	lit := strings.ReplaceAll(string(raw), "'", "''")
	// PowerShell also treats typographic single quotes as quote characters
	// inside single-quoted strings, so double those too.
	// Go note: "for _, q := range list" loops over a slice; the first value
	// (the index) is discarded with "_".
	for _, q := range []string{"‘", "’", "‚", "‛"} {
		lit = strings.ReplaceAll(lit, q, q+q)
	}

	// Step 4: write the preamble. In PowerShell terms it:
	//   - makes every error terminating ('Stop') so the try/catch in Wrap
	//     sees it,
	//   - hides progress bars (they would otherwise be serialised to the
	//     remote stream and slow things down),
	//   - forces -ErrorAction Stop on every cmdlet (see comment below),
	//   - parses the JSON parameters into $p,
	//   - builds $cn, a hashtable splatted into cmdlets as @cn, holding
	//     ComputerName only when the provider's dhcp_server is set (so the
	//     cmdlets target another DHCP server through this host).
	var b strings.Builder
	b.WriteString("$ErrorActionPreference = 'Stop'\n")
	b.WriteString("$ProgressPreference = 'SilentlyContinue'\n")
	// Preference variables do not reach into module functions such as the
	// CDXML DhcpServer cmdlets; an explicit default -ErrorAction does.
	b.WriteString("$PSDefaultParameterValues['*:ErrorAction'] = 'Stop'\n")
	b.WriteString("$p = ConvertFrom-Json -InputObject '" + lit + "'\n")
	b.WriteString("$cn = @{}\n")
	b.WriteString("if ($p.computer_name) { $cn['ComputerName'] = $p.computer_name }\n")
	// Step 5: append the wrapped script itself.
	b.WriteString(script)
	return b.String(), nil
}

// bootstrap is a small fixed PowerShell program that every transport runs.
// It reads a base64 UTF-8 script from stdin, runs it, and prints its output
// as base64 UTF-8 so no console code page can mangle either side.
//
// Why this design:
//   - The real script can be large, but the Windows command line (cmd.exe,
//     which OpenSSH on Windows uses as the default shell) is limited to 8191
//     characters. Passing the script on stdin has no such limit, and the
//     command line stays short and constant.
//   - Windows consoles use legacy code pages (for example 437 or 850), so
//     non-ASCII characters (accents in descriptions, for instance) would be
//     garbled in either direction. Base64 is pure ASCII, so it survives.
//
// What the PowerShell does, line by line: hide progress output; read all of
// stdin; base64-decode it to a UTF-8 string; compile it into a scriptblock
// and run it, joining the output lines with a newline ("`n", built here by
// concatenating Go strings because a raw string cannot contain a backtick);
// write the result back as base64 without a trailing newline.
const bootstrap = `$ProgressPreference = 'SilentlyContinue'
$in = [Console]::In.ReadToEnd()
$src = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($in.Trim()))
$res = (& ([scriptblock]::Create($src))) -join "` + "`n" + `"
[Console]::Out.Write([Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($res)))
`

// Command returns the fixed command line every transport executes, and the
// stdin payload carrying the rendered script.
//
// The bootstrap is passed with powershell.exe -EncodedCommand, which expects
// base64 of UTF-16LE text (the native Windows string encoding). That avoids
// any quoting issues between cmd.exe and PowerShell. The flags skip the
// banner and user profiles, refuse interactive prompts (which would hang a
// remote session) and bypass the execution policy for this process only.
//
// The stdin payload is the rendered script as base64 of UTF-8, followed by
// CRLF so the remote side sees a complete line.
//
// Go note: the return values are named (cmdline, stdin). Named results are
// pre-declared variables, so the body assigns cmdline with "=" not ":=".
func Command(rendered string) (cmdline, stdin string) {
	// Step 1: convert the bootstrap text to UTF-16 code units, then to
	// little-endian bytes, which is what -EncodedCommand expects.
	// Go note: make([]byte, 0, n) creates an empty slice with room for n
	// items, avoiding repeated reallocation as we append.
	u := utf16.Encode([]rune(bootstrap))
	buf := make([]byte, 0, len(u)*2)
	for _, c := range u {
		buf = binary.LittleEndian.AppendUint16(buf, c)
	}
	// Step 2: build the command line and the base64 stdin payload.
	cmdline = "powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand " +
		base64.StdEncoding.EncodeToString(buf)
	return cmdline, base64.StdEncoding.EncodeToString([]byte(rendered)) + "\r\n"
}

// DecodeOutput decodes the bootstrap's stdout (base64 of UTF-8) back into
// the raw JSON envelope text. If stdout is not valid base64, something other
// than the bootstrap wrote to stdout (a login banner, a profile, a crash
// message), so the start of it is included in the error for diagnosis.
func DecodeOutput(stdout []byte) ([]byte, error) {
	s := strings.TrimSpace(string(stdout))
	out, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("unexpected PowerShell output (not base64): %q", truncate(s, 200))
	}
	return out, nil
}

// Error is a failure reported by a script's catch block. Its fields mirror
// the "error" object the catch block in Wrap builds from the PowerShell
// ErrorRecord.
//
// Go note: the text in backticks after each field (`json:"message"`) is a
// struct tag. It tells the JSON decoder which JSON key fills which field.
type Error struct {
	Message  string `json:"message"`
	Category string `json:"category"`
	ID       string `json:"id"`
	Command  string `json:"command"`
}

// Error formats the failure for the user as "<cmdlet>: <message>
// (<category>)", following the project rule that errors name the cmdlet and
// carry the PowerShell message. The category is omitted when it adds no
// information ("NotSpecified").
//
// Go note: any type with a method "Error() string" automatically satisfies
// Go's built-in error interface; there is no "implements" keyword. The
// receiver "e *Error" is a pointer, meaning the method works on the original
// value rather than a copy.
func (e *Error) Error() string {
	cmd := e.Command
	if cmd == "" {
		cmd = "PowerShell"
	}
	msg := strings.TrimSpace(e.Message)
	if e.Category != "" && e.Category != "NotSpecified" {
		return fmt.Sprintf("%s: %s (%s)", cmd, msg, e.Category)
	}
	return fmt.Sprintf("%s: %s", cmd, msg)
}

// NotFound reports whether the error means the object does not exist: either
// PowerShell categorised it as ObjectNotFound, or the FullyQualifiedErrorId
// starts with one of the DHCP or DNS "does not exist" codes in notFoundIDs.
func (e *Error) NotFound() bool {
	if e.Category == "ObjectNotFound" {
		return true
	}
	// The ID looks like "DHCP 20010,Get-DhcpServerv4OptionValue", so match
	// the code followed by a comma, or the bare code on its own.
	for _, id := range notFoundIDs {
		if strings.HasPrefix(e.ID, id+",") || e.ID == id {
			return true
		}
	}
	return false
}

// Is makes errors.Is(err, ErrNotFound) work. errors.Is walks a chain of
// wrapped errors and, for each one that has an Is method, asks it whether it
// matches the target. This lets callers write errors.Is(err, ErrNotFound)
// without knowing about *Error at all.
func (e *Error) Is(target error) bool {
	return target == ErrNotFound && e.NotFound()
}

// envelope is the Go shape of the JSON object every script prints.
//
// Data is kept as json.RawMessage (the undecoded JSON bytes) because its
// shape depends on the script; Parse decodes it later into whatever type the
// caller asks for. Error is a pointer so it can be nil when absent.
// Go note: "*Error" is a pointer to an Error. A pointer can be nil, which
// here means "the JSON had no error object".
type envelope struct {
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data"`
	Error *Error          `json:"error"`
}

// Parse decodes a JSON envelope. On ok it unmarshals data into v (when v is
// non-nil) and reports whether data was present (not null). On failure it
// returns the *Error.
//
// v must be a pointer to the destination (for example &scope) so Parse can
// fill it in. "present == false" with a nil error is how list-and-filter
// scripts signal "not found" (they set $out to $null).
func Parse(out []byte, v any) (present bool, err error) {
	// Step 1: decode the outer envelope. If this fails, the host printed
	// something that is not our JSON (for example a warning line).
	// Go note: "&env" takes the address of env (a pointer), so Unmarshal
	// can write into it.
	var env envelope
	if err := json.Unmarshal(out, &env); err != nil {
		return false, fmt.Errorf("decoding PowerShell result: %w (output: %q)", err, truncate(string(out), 200))
	}
	// Step 2: script reported failure, return the typed error.
	if !env.OK {
		if env.Error == nil {
			return false, errors.New("PowerShell reported failure without error details")
		}
		return false, env.Error
	}
	// Step 3: success with no data ($out stayed $null).
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return false, nil
	}
	// Step 4: success with data, decode it into the caller's value.
	if v != nil {
		if err := json.Unmarshal(env.Data, v); err != nil {
			return true, fmt.Errorf("decoding PowerShell data: %w", err)
		}
	}
	return true, nil
}

// truncate shortens s to at most n bytes (plus "...") so error messages that
// quote unexpected output stay readable.
// Go note: s[:n] is a slice expression: the first n bytes of s.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
