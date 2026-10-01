// Tests for package psscript. They run entirely in memory: no Windows host
// and no PowerShell are needed. They check that scripts are wrapped and
// rendered as expected (including quote escaping, the injection defence),
// that the bootstrap command line round-trips through base64/UTF-16, and
// that JSON envelopes from testdata/*.json are parsed into data or errors.
//
// Run with: go test ./internal/psscript/
//
// Go note: test files end in _test.go and are only compiled by "go test".
// Every function named TestXxx(t *testing.T) is a test; t reports failures.
// This file uses "package psscript" (not psscript_test), so it can also see
// unexported names such as bootstrap.
package psscript

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

// TestWrapAndOp checks that Wrap adds the op marker and envelope, and that
// Op finds the marker only when it is on the first line.
// Go note: t.Fatalf reports a failure and stops this test immediately;
// t.Errorf (used elsewhere) reports but lets the test continue.
func TestWrapAndOp(t *testing.T) {
	s := Script{Op: "scope.get", Body: "$out = 1"}.Wrap()
	if got := Op(s); got != "scope.get" {
		t.Fatalf("Op() = %q", got)
	}
	if !strings.Contains(s, "$out = 1") || !strings.Contains(s, "ok = $true") {
		t.Fatalf("wrapped script missing body or envelope:\n%s", s)
	}
	rendered, err := Render(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := Op(rendered); got != "" {
		t.Fatalf("Op() of rendered script should be empty because the preamble comes first, got %q", got)
	}
	if got := Op("no marker"); got != "" {
		t.Fatalf("Op() = %q, want empty", got)
	}
}

// TestRenderEscapesQuotes checks that ASCII and typographic single quotes in
// user values are doubled, so they cannot close the PowerShell string.
func TestRenderEscapesQuotes(t *testing.T) {
	out, err := Render("# body", map[string]any{"name": "it's ‘quoted’"})
	if err != nil {
		t.Fatal(err)
	}
	want := `$p = ConvertFrom-Json -InputObject '{"name":"it''s ‘‘quoted’’"}'`
	if !strings.Contains(out, want) {
		t.Fatalf("rendered script does not contain %s:\n%s", want, out)
	}
	if !strings.HasSuffix(out, "# body") {
		t.Fatalf("script body not appended:\n%s", out)
	}
}

// TestRenderRejectsUnencodable checks that a value JSON cannot encode (a Go
// channel) produces an error instead of a broken script.
func TestRenderRejectsUnencodable(t *testing.T) {
	if _, err := Render("", map[string]any{"bad": make(chan int)}); err == nil {
		t.Fatal("expected error")
	}
}

// TestCommandEncoding decodes the -EncodedCommand argument (base64 of
// UTF-16LE) and the stdin payload (base64 of UTF-8) and checks both round
// trip exactly, including a non-ASCII character. It also guards the cmd.exe
// command line limit (8191 characters) with some margin.
func TestCommandEncoding(t *testing.T) {
	cmd, stdin := Command("Write-Output 'héllo'")
	enc, ok := strings.CutPrefix(cmd, "powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand ")
	if !ok {
		t.Fatalf("unexpected command line %q", cmd)
	}
	if len(cmd) > 8000 {
		t.Fatalf("command line too long for cmd.exe: %d", len(cmd))
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild UTF-16 code units from little-endian byte pairs.
	u := make([]uint16, len(raw)/2)
	for i := range u {
		u[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	if got := string(utf16.Decode(u)); got != bootstrap {
		t.Fatalf("bootstrap round trip mismatch:\n%s", got)
	}
	script, err := base64.StdEncoding.DecodeString(strings.TrimSpace(stdin))
	if err != nil {
		t.Fatal(err)
	}
	if string(script) != "Write-Output 'héllo'" {
		t.Fatalf("stdin round trip mismatch: %q", script)
	}
}

// TestDecodeOutput checks that base64 stdout (with a trailing CRLF) decodes,
// and that non-base64 output is rejected.
func TestDecodeOutput(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString([]byte(`{"ok":true}`))
	got, err := DecodeOutput([]byte(enc + "\r\n"))
	if err != nil || string(got) != `{"ok":true}` {
		t.Fatalf("DecodeOutput() = %q, %v", got, err)
	}
	if _, err := DecodeOutput([]byte("not base64!")); err == nil {
		t.Fatal("expected error")
	}
}

// fixture reads a recorded JSON envelope from testdata/.
// Go note: t.Helper() marks this as a helper, so a failure is reported at
// the caller's line rather than inside this function.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestParse feeds recorded envelopes to Parse: success with data, success
// with null, not-found errors (by category and by DHCP error code), another
// error, and malformed output.
// Go note: t.Run starts a named subtest; each can pass or fail on its own
// and can be selected with go test -run 'TestParse/ok_null'.
func TestParse(t *testing.T) {
	t.Run("ok object", func(t *testing.T) {
		var v struct {
			ScopeID string `json:"scope_id"`
		}
		present, err := Parse(fixture(t, "ok_object.json"), &v)
		if err != nil || !present || v.ScopeID != "10.1.2.0" {
			t.Fatalf("Parse() = %v, %v, %+v", present, err, v)
		}
	})
	t.Run("ok null", func(t *testing.T) {
		present, err := Parse(fixture(t, "ok_null.json"), &struct{}{})
		if err != nil || present {
			t.Fatalf("Parse() = %v, %v", present, err)
		}
	})
	t.Run("not found by category", func(t *testing.T) {
		_, err := Parse(fixture(t, "err_not_found.json"), nil)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
		want := "Get-DhcpServerv4Scope: Failed to get information for scope 10.9.9.0 on DHCP server DHCP01. (ObjectNotFound)"
		if err.Error() != want {
			t.Fatalf("Error() = %q, want %q", err.Error(), want)
		}
	})
	t.Run("not found by DHCP code", func(t *testing.T) {
		_, err := Parse(fixture(t, "err_option_not_present.json"), nil)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
	})
	// DnsServer cmdlets report Win32 DNS codes ("WIN32 9601,<cmdlet>").
	// Zone and name missing are not found; "record already exists" is not.
	t.Run("not found by DNS code", func(t *testing.T) {
		for id, want := range map[string]bool{
			"WIN32 9601,Get-DnsServerZone":              true,
			"WIN32 9714,Get-DnsServerResourceRecord":    true,
			"WIN32 9701,Remove-DnsServerResourceRecord": true,
			"WIN32 9711,Add-DnsServerResourceRecordA":   false,
			"WIN32 96010,Get-DnsServerZone":             false,
		} {
			raw := `{"ok":false,"error":{"message":"m","category":"NotSpecified","id":"` + id + `","command":"c"}}`
			_, err := Parse([]byte(raw), nil)
			if errors.Is(err, ErrNotFound) != want {
				t.Errorf("%s: not found = %v, want %v", id, !want, want)
			}
		}
	})
	t.Run("other error", func(t *testing.T) {
		_, err := Parse(fixture(t, "err_invalid.json"), nil)
		// Go note: errors.As finds a *Error in the error chain and stores it
		// in pe, so the test can read its fields.
		var pe *Error
		if !errors.As(err, &pe) || errors.Is(err, ErrNotFound) {
			t.Fatalf("expected non-not-found *Error, got %v", err)
		}
		if pe.Command != "Add-DhcpServerv4Scope" || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("unexpected error %q", err)
		}
	})
	t.Run("garbage", func(t *testing.T) {
		if _, err := Parse([]byte("WARNING: x"), nil); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("failure without details", func(t *testing.T) {
		if _, err := Parse([]byte(`{"ok":false}`), nil); err == nil {
			t.Fatal("expected error")
		}
	})
}
