// Tests for package runner. They need no Windows host:
//   - execute, Limit and CleanStderr are tested with plain Go fakes.
//   - NewWinRM is only checked for configuration validation (it does not
//     connect).
//   - The SSH transport is tested end to end against a real SSH server
//     running inside the test process (startSSHServer, built with the same
//     golang.org/x/crypto/ssh library). It accepts a password, offers both
//     ed25519 and RSA host keys, and answers "exec" requests with a handler
//     function instead of running PowerShell.
//
// Run with: go test ./internal/runner/
//
// Go note: functions named TestXxx(t *testing.T) are tests. t.Fatal stops the
// current test on failure; t.Error records a failure and keeps going.
package runner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// TestExecute checks the shared render/run/decode path with a fake execFunc:
// success, a non-zero exit code (stderr in the error) and a transport error.
// Go note: t.Run defines a named subtest; context.Background() is an empty
// context that is never cancelled.
func TestExecute(t *testing.T) {
	ctx := context.Background()
	t.Run("decodes stdout and passes rendered script on stdin", func(t *testing.T) {
		var gotStdin string
		ex := func(_ context.Context, cmdline, stdin string) ([]byte, []byte, int, error) {
			if !strings.HasPrefix(cmdline, "powershell.exe ") {
				t.Errorf("unexpected cmdline %q", cmdline)
			}
			gotStdin = stdin
			return []byte(base64.StdEncoding.EncodeToString([]byte(`{"ok":true}`)) + "\r\n"), nil, 0, nil
		}
		out, err := execute(ctx, "test", ex, "# body", map[string]any{"a": "b"})
		if err != nil || string(out) != `{"ok":true}` {
			t.Fatalf("execute() = %q, %v", out, err)
		}
		script, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(gotStdin))
		if !strings.Contains(string(script), `'{"a":"b"}'`) || !strings.HasSuffix(string(script), "# body") {
			t.Fatalf("unexpected rendered script:\n%s", script)
		}
	})
	t.Run("non-zero exit reports stderr", func(t *testing.T) {
		ex := func(context.Context, string, string) ([]byte, []byte, int, error) {
			return nil, []byte("boom"), 1, nil
		}
		_, err := execute(ctx, "test", ex, "", nil)
		if err == nil || !strings.Contains(err.Error(), "code 1: boom") {
			t.Fatalf("unexpected error %v", err)
		}
	})
	t.Run("transport error", func(t *testing.T) {
		ex := func(context.Context, string, string) ([]byte, []byte, int, error) {
			return nil, nil, 0, errors.New("dial failed")
		}
		_, err := execute(ctx, "test", ex, "", nil)
		if err == nil || !strings.Contains(err.Error(), "test: running PowerShell: dial failed") {
			t.Fatalf("unexpected error %v", err)
		}
	})
}

// TestCleanStderr checks CLIXML stderr becomes plain text (entities and
// encoded CRLF decoded), and that plain or empty stderr is handled.
func TestCleanStderr(t *testing.T) {
	clixml := `#< CLIXML
<Objs Version="1.1.0.1" xmlns="http://schemas.microsoft.com/powershell/2004/04"><S S="Error">Get-Foo : bad &amp; worse_x000D__x000A_</S><S S="Error">at line 1_x000D__x000A_</S></Objs>`
	if got := CleanStderr(clixml); got != "Get-Foo : bad & worse\nat line 1" {
		t.Fatalf("CleanStderr() = %q", got)
	}
	if got := CleanStderr("  plain  "); got != "plain" {
		t.Fatalf("CleanStderr() = %q", got)
	}
	if got := CleanStderr(""); got != "(no error output)" {
		t.Fatalf("CleanStderr() = %q", got)
	}
}

// countingRunner is a fake Runner that records the highest number of Run
// calls in progress at once. It uses atomic counters because many
// goroutines update them at the same time.
type countingRunner struct {
	cur, max atomic.Int32
}

// Run bumps the in-flight counter, updates the maximum with a
// compare-and-swap loop (retry if another goroutine changed it meanwhile),
// sleeps briefly so calls overlap, then decrements.
func (c *countingRunner) Run(context.Context, string, any) ([]byte, error) {
	n := c.cur.Add(1)
	for {
		m := c.max.Load()
		if n <= m || c.max.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(10 * time.Millisecond)
	c.cur.Add(-1)
	return nil, nil
}

// TestLimit runs 10 concurrent calls through Limit(..., 2) and checks no more
// than 2 overlapped. Then it fills the only slot of a Limit(..., 1) and
// checks that a cancelled context returns context.Canceled instead of
// blocking forever.
// Go note: sync.WaitGroup waits for a set of goroutines to finish; "go
// func() {...}()" starts one. ".(*limited)" is a type assertion that gets
// the concrete type back from the Runner interface.
func TestLimit(t *testing.T) {
	c := &countingRunner{}
	r := Limit(c, 2)
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = r.Run(context.Background(), "", nil)
		}()
	}
	wg.Wait()
	if got := c.max.Load(); got > 2 || got < 1 {
		t.Fatalf("max concurrency = %d, want <= 2", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	blocked := Limit(c, 1).(*limited)
	blocked.sem <- struct{}{}
	if _, err := blocked.Run(ctx, "", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestKerberosUser is table driven: a map of input to expected output.
func TestKerberosUser(t *testing.T) {
	for in, want := range map[string]string{
		`EXAMPLE\svc`:         "svc",
		"svc@EXAMPLE.LOCAL":   "svc",
		"svc":                 "svc",
		`EXAMPLE\svc@EXAMPLE`: "svc",
	} {
		if got := kerberosUser(in); got != want {
			t.Errorf("kerberosUser(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNewWinRMValidation checks which WinRM configurations NewWinRM accepts
// or rejects. "c := base" copies the struct, so each case starts clean.
func TestNewWinRMValidation(t *testing.T) {
	base := WinRMConfig{Host: "h", Username: "u", Password: "p", HTTPS: true}
	for _, auth := range []string{"", WinRMAuthNTLM, WinRMAuthBasic} {
		c := base
		c.Auth = auth
		if _, err := NewWinRM(c); err != nil {
			t.Errorf("auth %q: %v", auth, err)
		}
	}
	c := base
	c.HTTPS = false
	if _, err := NewWinRM(c); err != nil {
		t.Errorf("ntlm over http: %v", err)
	}
	c = base
	c.Auth = WinRMAuthKerberos
	if _, err := NewWinRM(c); err == nil || !strings.Contains(err.Error(), "kerberos_realm") {
		t.Errorf("expected realm error, got %v", err)
	}
	c.KerberosRealm = "EXAMPLE.LOCAL"
	if _, err := NewWinRM(c); err != nil {
		t.Errorf("kerberos: %v", err)
	}
	c = base
	c.Auth = "digest"
	if _, err := NewWinRM(c); err == nil {
		t.Error("expected unsupported auth error")
	}
	if _, err := NewWinRM(WinRMConfig{Host: "h"}); err == nil {
		t.Error("expected missing credentials error")
	}
}

// --- SSH ---------------------------------------------------------------

// testSSHServer is an in-process SSH server used to test the SSH transport
// without a Windows host.
type testSSHServer struct {
	addr    string
	hostKey ssh.PublicKey // ed25519
	rsaKey  ssh.PublicKey
	// handle receives the exec command and stdin and returns stdout, stderr
	// and exit status.
	handle func(cmd, stdin string) (string, string, uint32)
}

// startSSHServer generates fresh ed25519 and RSA host keys, listens on a
// random localhost port and serves connections in background goroutines
// until the test ends (t.Cleanup closes the listener). Only the given
// password is accepted.
// Go note: t.Helper() makes failures point at the caller's line.
func startSSHServer(t *testing.T, password string, handle func(cmd, stdin string) (string, string, uint32)) *testSSHServer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if string(pw) == password {
				return nil, nil
			}
			return nil, errors.New("denied")
		},
	}
	cfg.AddHostKey(signer)
	rsaPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaSigner, err := ssh.NewSignerFromKey(rsaPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddHostKey(rsaSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	srv := &testSSHServer{addr: ln.Addr().String(), hostKey: signer.PublicKey(), rsaKey: rsaSigner.PublicKey(), handle: handle}
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.serve(nc, cfg)
		}
	}()
	return srv
}

// serve handles one client connection: it accepts every session channel,
// and on an "exec" request reads the command and all of stdin, calls
// handle, writes stdout and stderr, and sends the exit status before
// closing the channel. Other request types (pty, env) are refused.
// The exec payload is an SSH string: a 4-byte big-endian length, then the
// command bytes.
func (s *testSSHServer) serve(nc net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		ch, chReqs, err := nch.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer func() { _ = ch.Close() }()
			for req := range chReqs {
				if req.Type != "exec" {
					_ = req.Reply(false, nil)
					continue
				}
				_ = req.Reply(true, nil)
				l := binary.BigEndian.Uint32(req.Payload)
				cmd := string(req.Payload[4 : 4+l])
				stdin, _ := io.ReadAll(ch)
				out, errOut, code := s.handle(cmd, string(stdin))
				_, _ = io.WriteString(ch, out)
				_, _ = io.WriteString(ch.Stderr(), errOut)
				status := make([]byte, 4)
				binary.BigEndian.PutUint32(status, code)
				_, _ = ch.SendRequest("exit-status", false, status)
				return
			}
		}()
	}
}

// hostPort splits the listener address into host and numeric port for
// SSHConfig.
func (s *testSSHServer) hostPort(t *testing.T) (string, int) {
	h, p, _ := net.SplitHostPort(s.addr)
	port, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	return h, port
}

// echoEnvelope is a handler that acts like a minimal remote PowerShell: it
// checks stdin is a base64 rendered script with the parameter preamble, and
// replies with a fixed base64 JSON envelope whose data is "pong".
func echoEnvelope(_ string, stdin string) (string, string, uint32) {
	script, err := base64.StdEncoding.DecodeString(strings.TrimSpace(stdin))
	if err != nil {
		return "", "bad stdin", 1
	}
	if !strings.Contains(string(script), "$p = ConvertFrom-Json") {
		return "", "missing preamble", 1
	}
	return base64.StdEncoding.EncodeToString([]byte(`{"ok":true,"data":"pong"}`)), "", 0
}

// TestSSHRunPinnedHostKey runs several scripts with a pinned host key,
// which also exercises connection reuse.
func TestSSHRunPinnedHostKey(t *testing.T) {
	srv := startSSHServer(t, "secret", echoEnvelope)
	host, port := srv.hostPort(t)
	r, err := NewSSH(SSHConfig{
		Host: host, Port: port, Username: "u", Password: "secret",
		HostKey: string(ssh.MarshalAuthorizedKey(srv.hostKey)),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Several runs reuse the connection.
	for range 3 {
		out, err := r.Run(context.Background(), "# body", map[string]any{"x": 1})
		if err != nil || string(out) != `{"ok":true,"data":"pong"}` {
			t.Fatalf("Run() = %q, %v", out, err)
		}
	}
}

// The server offers ed25519 and RSA host keys; only one of them is pinned
// or known, so the client must negotiate that type whatever its default
// preference.
func TestSSHRunNegotiatesKnownKeyType(t *testing.T) {
	srv := startSSHServer(t, "secret", echoEnvelope)
	host, port := srv.hostPort(t)
	for name, key := range map[string]ssh.PublicKey{"rsa": srv.rsaKey, "ed25519": srv.hostKey} {
		t.Run(name, func(t *testing.T) {
			r, err := NewSSH(SSHConfig{Host: host, Port: port, Username: "u", Password: "secret",
				HostKey: string(ssh.MarshalAuthorizedKey(key))})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Run(context.Background(), "", nil); err != nil {
				t.Fatalf("pinned key: %v", err)
			}

			kh := filepath.Join(t.TempDir(), "known_hosts")
			line := "[" + host + "]:" + strconv.Itoa(port) + " " + string(ssh.MarshalAuthorizedKey(key))
			if err := os.WriteFile(kh, []byte(line), 0o600); err != nil {
				t.Fatal(err)
			}
			r, err = NewSSH(SSHConfig{Host: host, Port: port, Username: "u", Password: "secret", KnownHostsFile: kh})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Run(context.Background(), "", nil); err != nil {
				t.Fatalf("key in known_hosts: %v", err)
			}
		})
	}
}

// TestSSHRunKnownHosts checks verification against a known_hosts file, and
// that a host missing from the file fails with the helpful "is not in"
// message.
func TestSSHRunKnownHosts(t *testing.T) {
	srv := startSSHServer(t, "secret", echoEnvelope)
	host, port := srv.hostPort(t)
	dir := t.TempDir()
	kh := filepath.Join(dir, "known_hosts")
	line := "[" + host + "]:" + strconv.Itoa(port) + " " + string(ssh.MarshalAuthorizedKey(srv.hostKey))
	if err := os.WriteFile(kh, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := NewSSH(SSHConfig{Host: host, Port: port, Username: "u", Password: "secret", KnownHostsFile: kh})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), "", nil); err != nil {
		t.Fatal(err)
	}

	// Unknown host is rejected with a helpful message.
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err = NewSSH(SSHConfig{Host: host, Port: port, Username: "u", Password: "secret", KnownHostsFile: empty})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), "", nil); err == nil || !strings.Contains(err.Error(), "is not in") {
		t.Fatalf("expected unknown host error, got %v", err)
	}
}

// TestSSHRunErrors checks that a non-zero exit surfaces cleaned CLIXML
// stderr, and that a wrong password surfaces the SSH auth error.
func TestSSHRunErrors(t *testing.T) {
	srv := startSSHServer(t, "secret", func(string, string) (string, string, uint32) {
		return "", "#< CLIXML\n<Objs><S S=\"Error\">it broke</S></Objs>", 1
	})
	host, port := srv.hostPort(t)

	r, err := NewSSH(SSHConfig{Host: host, Port: port, Username: "u", Password: "secret", Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), "", nil); err == nil || !strings.Contains(err.Error(), "code 1: it broke") {
		t.Fatalf("expected exit error, got %v", err)
	}

	r, err = NewSSH(SSHConfig{Host: host, Port: port, Username: "u", Password: "wrong", Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), "", nil); err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("expected auth error, got %v", err)
	}
}

// TestNewSSHValidation checks that NewSSH rejects bad configurations before
// any connection is attempted.
func TestNewSSHValidation(t *testing.T) {
	if _, err := NewSSH(SSHConfig{Host: "h", Username: "u", Insecure: true}); err == nil {
		t.Error("expected missing credentials error")
	}
	if _, err := NewSSH(SSHConfig{Username: "u", Password: "p"}); err == nil {
		t.Error("expected missing host error")
	}
	if _, err := NewSSH(SSHConfig{Host: "h", Username: "u", Password: "p", HostKey: "garbage"}); err == nil {
		t.Error("expected host key parse error")
	}
	if _, err := NewSSH(SSHConfig{Host: "h", Username: "u", PrivateKey: "garbage", Insecure: true}); err == nil {
		t.Error("expected private key parse error")
	}
	if _, err := NewSSH(SSHConfig{Host: "h", Username: "u", Password: "p", KnownHostsFile: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Error("expected known_hosts error")
	}
}

// TestSSHHandshakeTimeout checks that a server which accepts the TCP
// connection but never speaks SSH makes Run fail after the timeout instead
// of hanging forever.
func TestSSHHandshakeTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() }) // hold the socket open, say nothing
		}
	}()
	host, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(p)
	r, err := NewSSH(SSHConfig{Host: host, Port: port, Username: "u", Password: "p", Insecure: true, Timeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := r.Run(context.Background(), "", nil); err == nil {
		t.Fatal("expected a timeout error")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Run took %v, the handshake deadline was not applied", d)
	}
}
