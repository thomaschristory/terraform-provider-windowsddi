package dhcp

// Script tests: run the REAL PowerShell scripts from this package in a local
// pwsh (PowerShell 7) against fake DhcpServer cmdlets.
//
// How: pwshRunner implements runner.Runner by rendering the script exactly as
// production does (psscript.Render + psscript.Command, the same base64
// bootstrap), but executing it with a local pwsh instead of over SSH/WinRM.
// Before the script, it loads testdata/stubs.ps1 as a module: stub versions
// of Get/Add/Set/Remove-DhcpServerv4* that keep state in global hashtables
// and append every call (cmdlet name plus bound parameters) as one JSON line
// to the file named by $env:WD_CALLS. Tests then assert both on the decoded
// Go result and on the exact parameters the cmdlets received.
//
// Each Run is a fresh pwsh process, so state does not persist between calls;
// `seed` is extra PowerShell run before the script to pre-populate state.
//
// This catches PowerShell syntax errors, wrong parameter names, splatting
// mistakes, quoting bugs and JSON shape problems. It does not prove the real
// cmdlets behave like the stubs; acceptance tests against a real server do.
//
// Run with: go test ./internal/dhcp/ (the tests skip themselves when pwsh is
// not on PATH).
//
// Go note: test files end in _test.go and only build under `go test`; each
// func TestXxx(t *testing.T) is one test case.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/psscript"
)

// pwshRunner runs the real scripts through the real bootstrap with a local
// pwsh, after loading stub DhcpServer cmdlets (testdata/stubs.ps1) and an
// optional seed. It checks that the scripts parse, call the cmdlets with the
// expected parameters and print envelopes the Go side decodes. It does not
// prove the real cmdlets behave like the stubs: acceptance tests do that.
//
// Fields: pwsh is the path to the pwsh binary, seed is PowerShell run before
// each script, calls is the temp file the stubs append call records to.
//
// Go note: pwshRunner satisfies the runner.Runner interface implicitly, just
// by having a Run method with the right signature.
type pwshRunner struct {
	t     *testing.T
	pwsh  string
	seed  string
	calls string
}

// newPwshRunner returns a runner using the pwsh on PATH, or skips the test
// when pwsh is not installed. The calls file lives in a per-test temp dir that
// Go deletes automatically.
//
// Go note: t.Helper() marks this as a helper so failures are reported at the
// caller's line. t.Skip ends the test as "skipped" (not failed).
func newPwshRunner(t *testing.T, seed string) *pwshRunner {
	t.Helper()
	// Go note: `x, err := f()` takes two return values; err is non-nil on
	// failure and is checked right after with `if err != nil`.
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh not installed; skipping PowerShell script tests")
	}
	return &pwshRunner{t: t, pwsh: pwsh, seed: seed, calls: filepath.Join(t.TempDir(), "calls.jsonl")}
}

// Run executes one script in a fresh pwsh process, the same way production
// does on Windows: render the `$p` preamble, wrap in the base64 bootstrap,
// feed it on stdin, and decode the base64 stdout.
//
// Go note: `(r *pwshRunner)` is a pointer receiver: the method works on the
// shared runner, not a copy.
func (r *pwshRunner) Run(ctx context.Context, script string, params any) ([]byte, error) {
	// Step 1: read the stub cmdlets and render the script with its params.
	stubs, err := os.ReadFile(filepath.Join("testdata", "stubs.ps1"))
	if err != nil {
		r.t.Fatal(err)
	}
	rendered, err := psscript.Render(script, params)
	if err != nil {
		return nil, err
	}
	// Step 2: build the full source: stubs module, then seed, then script.
	// Load the stubs as a module: like the real CDXML cmdlets, they then
	// do not see the script's $ErrorActionPreference.
	full := "New-Module -Name DhcpServerStub -ScriptBlock {\n" + string(stubs) + "\n} | Import-Module\n" +
		r.seed + "\n" + rendered
	// Step 3: produce the production command line and stdin payload, then
	// swap powershell.exe for the local pwsh, keeping the same arguments.
	cmdline, stdin := psscript.Command(full)
	// Go note: s[1:] is a slice expression: everything from index 1 onward.
	args := strings.Fields(cmdline)[1:] // drop powershell.exe
	// Step 4: run pwsh with a 60 second timeout.
	// Go note: `defer cancel()` schedules cancel to run when Run returns,
	// whatever the exit path; it is Go's version of try/finally cleanup.
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// Go note: args... spreads a slice into separate arguments (like
	// splatting an array in PowerShell).
	cmd := exec.CommandContext(ctx, r.pwsh, args...)
	// The stubs write call records to the file named in WD_CALLS.
	cmd.Env = append(os.Environ(), "WD_CALLS="+r.calls)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// Go note: fmt.Errorf with %w wraps err so errors.Is can still find it.
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pwsh: %w: %s", err, stderr.String())
	}
	// Step 5: decode the base64 output back into the JSON envelope.
	return psscript.DecodeOutput(stdout.Bytes())
}

// lastCalls returns the cmdlet invocations recorded since the last call to
// lastCalls. Each record is a map like {"cmd": "Add-DhcpServerv4Scope",
// "Name": "Users", ...}. The file is deleted after reading, so the next
// call only sees new records.
//
// Go note: []map[string]any is a slice (list) of string-keyed maps, roughly
// an array of hashtables.
func (r *pwshRunner) lastCalls() []map[string]any {
	// No file means no cmdlet was called since the last read.
	b, err := os.ReadFile(r.calls)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		r.t.Fatal(err)
	}
	// `_ =` explicitly ignores the error from Remove (a missing file is fine).
	_ = os.Remove(r.calls)
	// Parse one JSON object per line.
	var out []map[string]any
	// Go note: `for _, x := range s` iterates over a slice; `_` drops the index.
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		// Strip a UTF-8 byte order mark that Add-Content may write.
		line = strings.TrimPrefix(line, "\ufeff")
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			r.t.Fatalf("bad call record %q: %v", line, err)
		}
		// Go note: append returns a new slice with m added at the end.
		out = append(out, m)
	}
	return out
}

// findCall returns the first recorded call to cmd, or fails the test if the
// cmdlet was never called.
func findCall(t *testing.T, calls []map[string]any, cmd string) map[string]any {
	t.Helper()
	for _, c := range calls {
		if c["cmd"] == cmd {
			return c
		}
	}
	t.Fatalf("%s was not called; calls: %v", cmd, calls)
	// Unreachable (Fatalf stops the test), but Go requires a return value.
	return nil
}

// seedScope is PowerShell that pre-creates scope 10.1.20.0 in the stubs'
// state, for tests that need an existing scope.
//
// Go note: a backtick string is a raw string literal: no escapes, multi-line.
const seedScope = `
$global:Scopes['10.1.20.0'] = New-Scope @{ ScopeId = '10.1.20.0'; SubnetMask = '255.255.255.0'; Name = 'Users'
    State = 'Active'; StartRange = '10.1.20.10'; EndRange = '10.1.20.250'; LeaseDuration = '8.00:00:00'
    Description = ''; Type = 'Dhcp' }
`

// TestPwshScope exercises add/get/list/set/remove for scopes end to end.
// The name deliberately contains quotes, typographic quotes and a non-ASCII
// character to prove user values survive the JSON `$p` path unmangled.
func TestPwshScope(t *testing.T) {
	ctx := context.Background()
	r := newPwshRunner(t, "")
	c := New(r, "")

	// Add: check the decoded result and the parameters Add received
	// (lease as 28800 seconds, no Description or ComputerName when empty).
	in := ScopeInput{
		ScopeID: "198.18.0.0", Name: "Café's \"scope\" ‘x’", StartRange: "198.18.0.10", EndRange: "198.18.0.200",
		SubnetMask: "255.255.255.0", State: "InActive", Type: "Both", LeaseDuration: 8 * time.Hour,
	}
	s, err := c.AddScope(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	want := &Scope{ScopeID: "198.18.0.0", Name: in.Name, StartRange: "198.18.0.10", EndRange: "198.18.0.200",
		SubnetMask: "255.255.255.0", State: "Inactive", Type: "Both", LeaseDuration: "0.08:00:00"}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("AddScope() = %+v\nwant %+v", s, want)
	}
	add := findCall(t, r.lastCalls(), "Add-DhcpServerv4Scope")
	if add["LeaseDuration"] != float64(28800) || add["Name"] != in.Name || add["SubnetMask"] != "255.255.255.0" {
		t.Fatalf("unexpected Add call %v", add)
	}
	if _, ok := add["Description"]; ok {
		t.Fatalf("empty description must not be passed: %v", add)
	}
	if _, ok := add["ComputerName"]; ok {
		t.Fatalf("ComputerName must not be passed: %v", add)
	}

	// Each run is a fresh process, so seed state for the remaining calls.
	r.seed = seedScope
	got, err := c.GetScope(ctx, "10.1.20.0")
	if err != nil || got.LeaseDuration != "8.00:00:00" || got.Name != "Users" {
		t.Fatalf("GetScope() = %+v, %v", got, err)
	}

	// Unknown scope: the stub writes a DHCP 20005 ObjectNotFound error.
	_, err = c.GetScope(ctx, "10.9.9.0")
	if !IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "Get-DhcpServerv4Scope: Failed to get information for scope 10.9.9.0") {
		t.Fatalf("error should name the cmdlet: %q", err)
	}

	// List with one scope, then with none (must be an empty list, not null).
	list, err := c.ListScopes(ctx)
	if err != nil || len(list) != 1 || list[0].ScopeID != "10.1.20.0" {
		t.Fatalf("ListScopes() with one scope = %+v, %v", list, err)
	}
	r.seed = ""
	if list, err = c.ListScopes(ctx); err != nil || len(list) != 0 {
		t.Fatalf("ListScopes() with no scope = %+v, %v", list, err)
	}

	// Set: clear old call records, then update range, description and lease.
	// Go note: `upd := in` copies the struct, so changing upd leaves in as-is.
	r.seed = seedScope
	r.lastCalls()
	upd := in
	upd.ScopeID, upd.StartRange, upd.EndRange, upd.Description = "10.1.20.0", "10.1.20.20", "10.1.20.240", "new"
	upd.LeaseDuration = 24 * time.Hour
	s, err = c.SetScope(ctx, upd)
	if err != nil || s.StartRange != "10.1.20.20" || s.LeaseDuration != "1.00:00:00" || s.Description != "new" {
		t.Fatalf("SetScope() = %+v, %v", s, err)
	}
	set := findCall(t, r.lastCalls(), "Set-DhcpServerv4Scope")
	if set["ScopeId"] != "10.1.20.0" || set["State"] != "InActive" || set["LeaseDuration"] != float64(86400) {
		t.Fatalf("unexpected Set call %v", set)
	}

	// Remove with and without force: -Force must follow the flag, and
	// -Confirm:$false must always be passed.
	if err := c.RemoveScope(ctx, "10.1.20.0", true); err != nil {
		t.Fatal(err)
	}
	rm := findCall(t, r.lastCalls(), "Remove-DhcpServerv4Scope")
	if rm["Force"] != true || rm["Confirm"] != false {
		t.Fatalf("unexpected Remove call %v", rm)
	}
	if err := c.RemoveScope(ctx, "10.1.20.0", false); err != nil {
		t.Fatal(err)
	}
	if rm = findCall(t, r.lastCalls(), "Remove-DhcpServerv4Scope"); rm["Force"] != false {
		t.Fatalf("unexpected Remove call %v", rm)
	}
}

// TestPwshComputerName checks the @cn splat really passes -ComputerName to
// the cmdlet when the client has a computer name.
func TestPwshComputerName(t *testing.T) {
	r := newPwshRunner(t, seedScope)
	c := New(r, "dhcp02.example.local")
	if _, err := c.GetScope(context.Background(), "10.1.20.0"); err != nil {
		t.Fatal(err)
	}
	if get := findCall(t, r.lastCalls(), "Get-DhcpServerv4Scope"); get["ComputerName"] != "dhcp02.example.local" {
		t.Fatalf("ComputerName not passed: %v", get)
	}
}

// TestPwshReservation covers both not-found paths (no reservation, missing
// scope), add with an empty name (server defaults it to the IP), list, set
// (name kept when empty, description cleared) and remove.
func TestPwshReservation(t *testing.T) {
	ctx := context.Background()
	r := newPwshRunner(t, seedScope)
	c := New(r, "")

	_, err := c.GetReservation(ctx, "10.1.20.0", "10.1.20.5")
	if !IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
	if _, err = c.GetReservation(ctx, "10.9.9.0", "10.9.9.5"); !IsNotFound(err) {
		t.Fatalf("missing scope should be not found, got %v", err)
	}

	in := ReservationInput{ScopeID: "10.1.20.0", IPAddress: "10.1.20.5", ClientID: "aa-bb-cc-dd-ee-ff", Type: "Both"}
	res, err := c.AddReservation(ctx, in)
	if err != nil || res.Name != "10.1.20.5" || res.ClientID != "aa-bb-cc-dd-ee-ff" || res.Type != "Both" {
		t.Fatalf("AddReservation() = %+v, %v", res, err)
	}
	add := findCall(t, r.lastCalls(), "Add-DhcpServerv4Reservation")
	if _, ok := add["Name"]; ok {
		t.Fatalf("empty name must not be passed: %v", add)
	}

	// Seed an existing reservation named "printer" for list/set/remove.
	r.seed = seedScope + `Add-DhcpServerv4Reservation -ScopeId 10.1.20.0 -IPAddress 10.1.20.5 -ClientId aa-bb-cc-dd-ee-ff -Name printer`
	r.lastCalls()
	list, err := c.ListReservations(ctx, "10.1.20.0")
	if err != nil || len(list) != 1 || list[0].Name != "printer" {
		t.Fatalf("ListReservations() = %+v, %v", list, err)
	}
	in.ClientID, in.Description, in.Name = "aa-bb-cc-dd-ee-00", "", ""
	res, err = c.SetReservation(ctx, in)
	if err != nil || res.ClientID != "aa-bb-cc-dd-ee-00" || res.Name != "printer" {
		t.Fatalf("SetReservation() = %+v, %v", res, err)
	}
	set := findCall(t, r.lastCalls(), "Set-DhcpServerv4Reservation")
	if set["Description"] != "" || set["IPAddress"] != "10.1.20.5" {
		t.Fatalf("unexpected Set call %v", set)
	}
	if err := c.RemoveReservation(ctx, "10.1.20.5"); err != nil {
		t.Fatal(err)
	}
	findCall(t, r.lastCalls(), "Remove-DhcpServerv4Reservation")
}

// TestPwshExclusion covers not found, add, and remove, and checks Remove is
// always given the exact start and end (so it can never wipe a whole scope).
func TestPwshExclusion(t *testing.T) {
	ctx := context.Background()
	r := newPwshRunner(t, seedScope)
	c := New(r, "")
	e := ExclusionRange{ScopeID: "10.1.20.0", StartRange: "10.1.20.1", EndRange: "10.1.20.9"}
	if _, err := c.GetExclusionRange(ctx, e); !IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
	got, err := c.AddExclusionRange(ctx, e)
	if err != nil || *got != e {
		t.Fatalf("AddExclusionRange() = %+v, %v", got, err)
	}
	if err := c.RemoveExclusionRange(ctx, e); err != nil {
		t.Fatal(err)
	}
	rm := findCall(t, r.lastCalls(), "Remove-DhcpServerv4ExclusionRange")
	if rm["StartRange"] != "10.1.20.1" || rm["EndRange"] != "10.1.20.9" {
		t.Fatalf("Remove must target the exact range, never the whole scope: %v", rm)
	}
}

// TestPwshOption checks that the `$sel` splat targets the right level:
// scope (ScopeId only), server with a vendor class (no ScopeId), and
// reservation (ReservedIP only), plus not found and remove.
func TestPwshOption(t *testing.T) {
	ctx := context.Background()
	r := newPwshRunner(t, seedScope)
	c := New(r, "")

	// Scope level.
	k := OptionKey{OptionID: 3, ScopeID: "10.1.20.0"}
	if _, err := c.GetOptionValue(ctx, k); !IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
	v, err := c.SetOptionValue(ctx, k, []string{"10.1.20.1"})
	if err != nil || !reflect.DeepEqual(v.Value, []string{"10.1.20.1"}) || v.OptionID != 3 || v.Name != "Router" {
		t.Fatalf("SetOptionValue() = %+v, %v", v, err)
	}
	set := findCall(t, r.lastCalls(), "Set-DhcpServerv4OptionValue")
	if !reflect.DeepEqual(set["Value"], []any{"10.1.20.1"}) || set["ScopeId"] != "10.1.20.0" {
		t.Fatalf("unexpected Set call %v", set)
	}
	if _, ok := set["ReservedIP"]; ok {
		t.Fatalf("ReservedIP must not be passed: %v", set)
	}

	// Server level, scoped to a vendor class, with two values.
	server := OptionKey{OptionID: 6, VendorClass: "Vendor A"}
	v, err = c.SetOptionValue(ctx, server, []string{"10.0.0.53", "10.0.1.53"})
	if err != nil || len(v.Value) != 2 || v.VendorClass != "Vendor A" {
		t.Fatalf("SetOptionValue(server) = %+v, %v", v, err)
	}
	set = findCall(t, r.lastCalls(), "Set-DhcpServerv4OptionValue")
	if _, ok := set["ScopeId"]; ok {
		t.Fatalf("server level must not pass ScopeId: %v", set)
	}
	if set["VendorClass"] != "Vendor A" {
		t.Fatalf("VendorClass not passed: %v", set)
	}

	// Reservation level, then remove it.
	res := OptionKey{OptionID: 12, ReservedIP: "10.1.20.5"}
	if _, err := c.SetOptionValue(ctx, res, []string{"printer"}); err != nil {
		t.Fatal(err)
	}
	if set = findCall(t, r.lastCalls(), "Set-DhcpServerv4OptionValue"); set["ReservedIP"] != "10.1.20.5" {
		t.Fatalf("ReservedIP not passed: %v", set)
	}
	if err := c.RemoveOptionValue(ctx, res); err != nil {
		t.Fatal(err)
	}
	rm := findCall(t, r.lastCalls(), "Remove-DhcpServerv4OptionValue")
	if !reflect.DeepEqual(rm["OptionId"], []any{"12"}) || rm["ReservedIP"] != "10.1.20.5" || rm["Confirm"] != false {
		t.Fatalf("unexpected Remove call %v", rm)
	}
}

// TestPwshLeases seeds one active lease and one inactive reservation, then
// checks the UTC RFC 3339 expiry formatting, the empty expiry for the
// reservation, and that -AllLeases is passed.
func TestPwshLeases(t *testing.T) {
	r := newPwshRunner(t, seedScope+`
[void]$global:Leases.Add([pscustomobject]@{ IPAddress = [ipaddress]'10.1.20.11'; ScopeId = [ipaddress]'10.1.20.0'
    ClientId = '00-11-22-33-44-55'; HostName = 'pc01'; AddressState = 'Active'
    LeaseExpiryTime = [datetime]::new(2026, 10, 9, 8, 0, 0, [DateTimeKind]::Utc); Description = $null; ClientType = 'Dhcp' })
[void]$global:Leases.Add([pscustomobject]@{ IPAddress = [ipaddress]'10.1.20.5'; ScopeId = [ipaddress]'10.1.20.0'
    ClientId = 'aa-bb-cc-dd-ee-ff'; HostName = ''; AddressState = 'InactiveReservation'
    LeaseExpiryTime = $null; Description = ''; ClientType = 'None' })
`)
	c := New(r, "")
	got, err := c.ListLeases(context.Background(), "10.1.20.0", true)
	if err != nil || len(got) != 2 {
		t.Fatalf("ListLeases() = %+v, %v", got, err)
	}
	if got[0].LeaseExpiryTime != "2026-10-09T08:00:00Z" || got[0].HostName != "pc01" || got[1].LeaseExpiryTime != "" {
		t.Fatalf("unexpected leases %+v", got)
	}
	if get := findCall(t, r.lastCalls(), "Get-DhcpServerv4Lease"); get["AllLeases"] != true {
		t.Fatalf("AllLeases not passed: %v", get)
	}
}

// TestPwshBrokenScript: a script that fails to parse must surface the
// PowerShell error rather than an empty result.
func TestPwshBrokenScript(t *testing.T) {
	r := newPwshRunner(t, "")
	c := New(r, "")
	_, err := c.run(context.Background(), psscript.Script{Op: "broken", Body: "$out = @("}, nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
}
