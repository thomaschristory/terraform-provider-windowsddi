package dns

// Script tests: run the REAL PowerShell scripts from this package in a local
// pwsh (PowerShell 7) against fake DnsServer cmdlets (testdata/stubs.ps1).
//
// The harness is the same as internal/dhcp/pwsh_test.go (see there for the
// step by step explanation): pwshRunner renders each script exactly as
// production does (psscript.Render + psscript.Command, the same base64
// bootstrap) and runs it in a fresh local pwsh process after loading the
// stubs as a module and an optional seed. The stubs append every call
// (cmdlet name plus bound parameters) as one JSON line to $env:WD_CALLS.
//
// Seeds run the stub cmdlets with $global:WdQuiet set, so seeding does not
// show up in the recorded calls.
//
// This catches PowerShell syntax errors, wrong parameter names or parameter
// sets, splatting mistakes and JSON shape problems (arrays of one, enums).
// It does not prove the real cmdlets behave like the stubs; acceptance tests
// against a real server do.
//
// Run with: go test ./internal/dns/ (skipped when pwsh is not on PATH).

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

// pwshRunner runs scripts in a local pwsh with the stub DnsServer module
// loaded. Fields: pwsh is the binary, seed is PowerShell run before each
// script, calls is the temp file the stubs append call records to.
type pwshRunner struct {
	t     *testing.T
	pwsh  string
	seed  string
	calls string
}

// newPwshRunner returns a runner, or skips the test when pwsh is missing.
func newPwshRunner(t *testing.T, seed string) *pwshRunner {
	t.Helper()
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh not installed; skipping PowerShell script tests")
	}
	return &pwshRunner{t: t, pwsh: pwsh, seed: seed, calls: filepath.Join(t.TempDir(), "calls.jsonl")}
}

// Run executes one script in a fresh pwsh process, the same way production
// does on Windows, with the stubs module and the seed run first.
func (r *pwshRunner) Run(ctx context.Context, script string, params any) ([]byte, error) {
	stubs, err := os.ReadFile(filepath.Join("testdata", "stubs.ps1"))
	if err != nil {
		r.t.Fatal(err)
	}
	rendered, err := psscript.Render(script, params)
	if err != nil {
		return nil, err
	}
	seed := "$global:WdQuiet = $true\n" + r.seed + "\n$global:WdQuiet = $false\n"
	full := "New-Module -Name DnsServerStub -ScriptBlock {\n" + string(stubs) + "\n} | Import-Module\n" + seed + rendered
	cmdline, stdin := psscript.Command(full)
	args := strings.Fields(cmdline)[1:] // drop powershell.exe
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.pwsh, args...)
	cmd.Env = append(os.Environ(), "WD_CALLS="+r.calls)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pwsh: %w: %s", err, stderr.String())
	}
	return psscript.DecodeOutput(stdout.Bytes())
}

// lastCalls returns the cmdlet calls recorded since the previous lastCalls.
func (r *pwshRunner) lastCalls() []map[string]any {
	b, err := os.ReadFile(r.calls)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		r.t.Fatal(err)
	}
	_ = os.Remove(r.calls)
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		line = strings.TrimPrefix(line, "\ufeff")
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			r.t.Fatalf("bad call record %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// findCalls returns every recorded call to cmd.
func findCalls(calls []map[string]any, cmd string) []map[string]any {
	var out []map[string]any
	for _, c := range calls {
		if c["cmd"] == cmd {
			out = append(out, c)
		}
	}
	return out
}

// findCall returns the only recorded call to cmd, failing the test unless
// there is exactly one.
func findCall(t *testing.T, calls []map[string]any, cmd string) map[string]any {
	t.Helper()
	got := findCalls(calls, cmd)
	if len(got) != 1 {
		t.Fatalf("want exactly one call to %s, got %d; calls: %v", cmd, len(got), calls)
	}
	return got[0]
}

// seedZone creates the file-backed zone example.com (with its apex SOA and
// NS records) in the stub state.
const seedZone = `Add-DnsServerPrimaryZone -Name example.com -ZoneFile example.com.dns
`

// seedRecords adds sample records to example.com: two A records for www, one
// dynamic A record, two MX records and a CNAME.
const seedRecords = seedZone + `
Add-DnsServerResourceRecordA -ZoneName example.com -Name www -IPv4Address 10.0.0.1, 10.0.0.2
Add-DnsServerResourceRecordA -ZoneName example.com -Name pc01 -IPv4Address 10.0.0.50 -TimeToLive 00:20:00
$global:Records | Where-Object HostName -eq 'pc01' | ForEach-Object { $_.Timestamp = [datetime]'2026-09-01' }
Add-DnsServerResourceRecordMX -ZoneName example.com -Name '@' -MailExchange mail.example.com -Preference 10
Add-DnsServerResourceRecordMX -ZoneName example.com -Name '@' -MailExchange mail.example.com -Preference 20
Add-DnsServerResourceRecordCName -ZoneName example.com -Name ftp -HostNameAlias www.example.com
`

// TestPwshZone covers add (forward file zone, AD reverse zone by network
// ID), get, not found, list, set (two separate Set calls), has-records and
// remove.
func TestPwshZone(t *testing.T) {
	ctx := context.Background()
	r := newPwshRunner(t, "")
	c := New(r, "")

	z, err := c.AddPrimaryZone(ctx, PrimaryZoneInput{Name: "example.com", ZoneFile: "example.com.dns"})
	if err != nil {
		t.Fatal(err)
	}
	want := &Zone{Name: "example.com", ZoneType: "Primary", ZoneFile: "example.com.dns", DynamicUpdate: "None", MasterServers: []string{}}
	if !reflect.DeepEqual(z, want) {
		t.Fatalf("AddPrimaryZone() = %+v\nwant %+v", z, want)
	}
	add := findCall(t, r.lastCalls(), "Add-DnsServerPrimaryZone")
	if add["Name"] != "example.com" || add["ZoneFile"] != "example.com.dns" || add["PassThru"] != true {
		t.Fatalf("unexpected Add call %v", add)
	}
	for _, k := range []string{"NetworkId", "ReplicationScope", "DynamicUpdate", "ComputerName"} {
		if _, ok := add[k]; ok {
			t.Fatalf("%s must not be passed: %v", k, add)
		}
	}

	// Reverse zone by network ID: the name comes from the server.
	z, err = c.AddPrimaryZone(ctx, PrimaryZoneInput{NetworkID: "10.1.2.0/24", ReplicationScope: "Domain", DynamicUpdate: "NonsecureAndSecure"})
	if err != nil || z.Name != "2.1.10.in-addr.arpa" || !z.IsReverseLookupZone || !z.IsDsIntegrated ||
		z.ReplicationScope != "Domain" || z.DynamicUpdate != "NonsecureAndSecure" {
		t.Fatalf("AddPrimaryZone(network) = %+v, %v", z, err)
	}
	add = findCall(t, r.lastCalls(), "Add-DnsServerPrimaryZone")
	if add["NetworkId"] != "10.1.2.0/24" || add["ReplicationScope"] != "Domain" {
		t.Fatalf("unexpected Add call %v", add)
	}

	// Get, not found, list.
	r.seed = seedZone
	if z, err = c.GetZone(ctx, "example.com"); err != nil || z.ZoneType != ZoneTypePrimary {
		t.Fatalf("GetZone() = %+v, %v", z, err)
	}
	_, err = c.GetZone(ctx, "nope.example")
	if !IsNotFound(err) || !strings.HasPrefix(err.Error(), "Get-DnsServerZone: The zone nope.example was not found") {
		t.Fatalf("expected not found naming the cmdlet, got %v", err)
	}
	if zs, err := c.ListZones(ctx); err != nil || len(zs) != 1 || zs[0].Name != "example.com" {
		t.Fatalf("ListZones() = %+v, %v", zs, err)
	}
	r.seed = ""
	if zs, err := c.ListZones(ctx); err != nil || len(zs) != 0 {
		t.Fatalf("ListZones() on empty server = %+v, %v", zs, err)
	}

	// Set: both fields means two calls (separate parameter sets).
	r.seed = seedZone
	r.lastCalls()
	z, err = c.SetPrimaryZone(ctx, "example.com", PrimaryZoneUpdate{ReplicationScope: "Forest", DynamicUpdate: "Secure"})
	if err != nil || z.ReplicationScope != "Forest" || z.DynamicUpdate != "Secure" || !z.IsDsIntegrated {
		t.Fatalf("SetPrimaryZone() = %+v, %v", z, err)
	}
	if sets := findCalls(r.lastCalls(), "Set-DnsServerPrimaryZone"); len(sets) != 2 || sets[0]["ReplicationScope"] != "Forest" || sets[1]["DynamicUpdate"] != "Secure" {
		t.Fatalf("unexpected Set calls %v", sets)
	}
	// Only dynamic update: no replication scope call.
	if _, err = c.SetPrimaryZone(ctx, "example.com", PrimaryZoneUpdate{DynamicUpdate: "None"}); err != nil {
		t.Fatal(err)
	}
	if set := findCall(t, r.lastCalls(), "Set-DnsServerPrimaryZone"); set["DynamicUpdate"] != "None" {
		t.Fatalf("unexpected Set call %v", set)
	}

	// Has records: only SOA and NS at first, then a user record.
	if has, err := c.ZoneHasUserRecords(ctx, "example.com"); err != nil || has {
		t.Fatalf("ZoneHasUserRecords(empty) = %v, %v", has, err)
	}
	r.seed = seedRecords
	if has, err := c.ZoneHasUserRecords(ctx, "example.com"); err != nil || !has {
		t.Fatalf("ZoneHasUserRecords() = %v, %v", has, err)
	}
	if _, err := c.ZoneHasUserRecords(ctx, "nope.example"); !IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}

	// Remove: -Force always; missing zone is not found.
	r.lastCalls()
	if err := c.RemoveZone(ctx, "example.com"); err != nil {
		t.Fatal(err)
	}
	if rm := findCall(t, r.lastCalls(), "Remove-DnsServerZone"); rm["Force"] != true || rm["Name"] != "example.com" {
		t.Fatalf("unexpected Remove call %v", rm)
	}
	if err := c.RemoveZone(ctx, "nope.example"); !IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
}

// TestPwshForwarder covers add (default timeout, mixed address families),
// set with a replication scope (second call) and the -ComputerName splat.
func TestPwshForwarder(t *testing.T) {
	ctx := context.Background()
	r := newPwshRunner(t, "")
	c := New(r, "dns02.example.local")

	z, err := c.AddConditionalForwarder(ctx, ConditionalForwarderInput{Name: "partner.example", MasterServers: []string{"10.0.0.53", "2001:db8::53"}})
	if err != nil || z.ZoneType != ZoneTypeForwarder || z.ForwarderTimeout != 5 || z.IsDsIntegrated ||
		!reflect.DeepEqual(z.MasterServers, []string{"10.0.0.53", "2001:db8::53"}) {
		t.Fatalf("AddConditionalForwarder() = %+v, %v", z, err)
	}
	add := findCall(t, r.lastCalls(), "Add-DnsServerConditionalForwarderZone")
	if add["ComputerName"] != "dns02.example.local" || !reflect.DeepEqual(add["MasterServers"], []any{"10.0.0.53", "2001:db8::53"}) {
		t.Fatalf("unexpected Add call %v", add)
	}
	if _, ok := add["ForwarderTimeout"]; ok {
		t.Fatalf("unset timeout must not be passed: %v", add)
	}

	// One master server: still an array on both sides.
	r.seed = `Add-DnsServerConditionalForwarderZone -Name partner.example -MasterServers 10.0.0.53`
	r.lastCalls()
	z, err = c.SetConditionalForwarder(ctx, ConditionalForwarderInput{Name: "partner.example", MasterServers: []string{"10.0.0.54"},
		ForwarderTimeout: 3, ReplicationScope: "Forest"})
	if err != nil || z.ForwarderTimeout != 3 || z.ReplicationScope != "Forest" || !reflect.DeepEqual(z.MasterServers, []string{"10.0.0.54"}) {
		t.Fatalf("SetConditionalForwarder() = %+v, %v", z, err)
	}
	sets := findCalls(r.lastCalls(), "Set-DnsServerConditionalForwarderZone")
	if len(sets) != 2 || sets[0]["ForwarderTimeout"] != float64(3) || sets[1]["ReplicationScope"] != "Forest" {
		t.Fatalf("unexpected Set calls %v", sets)
	}
	if _, err := c.GetZone(ctx, "partner.example"); err != nil {
		t.Fatal(err)
	}
}

// TestPwshRecordAdd adds one record of every supported type and checks the
// cmdlet and parameters used, the TTL handling and the decoded read back.
func TestPwshRecordAdd(t *testing.T) {
	ctx := context.Background()
	r := newPwshRunner(t, seedZone)
	c := New(r, "")

	cases := []struct {
		rec    Record
		cmd    string
		params map[string]any
	}{
		{Record{Name: "www", Type: TypeA, Address: "10.0.0.1"}, "Add-DnsServerResourceRecordA", map[string]any{"IPv4Address": []any{"10.0.0.1"}}},
		{Record{Name: "www", Type: TypeAAAA, Address: "2001:db8::1", TTL: 300}, "Add-DnsServerResourceRecordAAAA", map[string]any{"IPv6Address": []any{"2001:db8::1"}, "TimeToLive": float64(300)}},
		{Record{Name: "ftp", Type: TypeCNAME, HostName: "www.example.com."}, "Add-DnsServerResourceRecordCName", map[string]any{"HostNameAlias": "www.example.com."}},
		{Record{Name: "1", Type: TypePTR, HostName: "host.example.com"}, "Add-DnsServerResourceRecordPtr", map[string]any{"PtrDomainName": "host.example.com"}},
		{Record{Name: "@", Type: TypeMX, Exchange: "mail.example.com", Preference: 10}, "Add-DnsServerResourceRecordMX", map[string]any{"MailExchange": "mail.example.com", "Preference": float64(10)}},
		{Record{Name: "_sip._tcp", Type: TypeSRV, Target: "sip.example.com", Priority: 1, Weight: 2, Port: 5060}, "Add-DnsServerResourceRecord",
			map[string]any{"Srv": true, "DomainName": "sip.example.com", "Priority": float64(1), "Weight": float64(2), "Port": float64(5060)}},
		{Record{Name: "txt", Type: TypeTXT, Text: `v=spf1 "quoted" ‘x’ café`}, "Add-DnsServerResourceRecord", map[string]any{"Txt": true, "DescriptiveText": `v=spf1 "quoted" ‘x’ café`}},
	}
	for _, tc := range cases {
		if err := c.AddRecord(ctx, "example.com", tc.rec); err != nil {
			t.Fatalf("AddRecord(%+v): %v", tc.rec, err)
		}
		call := findCall(t, r.lastCalls(), tc.cmd)
		if call["ZoneName"] != "example.com" || call["Name"] != tc.rec.Name {
			t.Fatalf("%s: unexpected call %v", tc.rec.Type, call)
		}
		for k, v := range tc.params {
			if !reflect.DeepEqual(call[k], v) {
				t.Fatalf("%s: %s = %#v, want %#v (call %v)", tc.rec.Type, k, call[k], v, call)
			}
		}
		if _, ok := call["TimeToLive"]; ok && tc.rec.TTL == 0 {
			t.Fatalf("%s: TimeToLive must not be passed when TTL is 0: %v", tc.rec.Type, call)
		}
	}

	// Missing zone: not found. Duplicate: a real error, not not-found.
	if err := c.AddRecord(ctx, "nope.example", Record{Name: "www", Type: TypeA, Address: "10.0.0.1"}); !IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
	r.seed = seedRecords
	err := c.AddRecord(ctx, "example.com", Record{Name: "www", Type: TypeA, Address: "10.0.0.1"})
	if err == nil || IsNotFound(err) || !strings.Contains(err.Error(), "already exists") || !strings.HasPrefix(err.Error(), "Add-DnsServerResourceRecordA:") {
		t.Fatalf("expected already-exists error, got %v", err)
	}
	if err := c.AddRecord(ctx, "example.com", Record{Name: "www", Type: "NS", HostName: "x."}); err == nil {
		t.Fatal("unsupported type must fail before running")
	}
}

// TestPwshRecordRead covers GetRecords (typed fields, data, dynamic flag,
// arrays of one, empty name, missing zone) and ListRecords.
func TestPwshRecordRead(t *testing.T) {
	ctx := context.Background()
	r := newPwshRunner(t, seedRecords)
	c := New(r, "")

	got, err := c.GetRecords(ctx, "example.com", "www", TypeA)
	if err != nil || len(got) != 2 || got[0].Address != "10.0.0.1" || got[1].Address != "10.0.0.2" || got[0].TTL != 3600 || got[0].Dynamic {
		t.Fatalf("GetRecords(www A) = %+v, %v", got, err)
	}
	get := findCall(t, r.lastCalls(), "Get-DnsServerResourceRecord")
	if get["Name"] != "www" || get["RRType"] != "A" || get["Node"] != true || get["ZoneName"] != "example.com" {
		t.Fatalf("unexpected Get call %v", get)
	}

	// One record: still a list. Dynamic and a custom TTL.
	r.lastCalls()
	got, err = c.GetRecords(ctx, "example.com", "pc01", TypeA)
	want := []Record{{Name: "pc01", Type: TypeA, TTL: 1200, Dynamic: true, Address: "10.0.0.50", Data: "10.0.0.50"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("GetRecords(pc01 A) = %+v, %v", got, err)
	}

	r.lastCalls()
	got, err = c.GetRecords(ctx, "example.com", "@", TypeMX)
	if err != nil || len(got) != 2 || got[0].Exchange != "mail.example.com." || got[1].Preference != 20 || got[0].Data != "10 mail.example.com." {
		t.Fatalf("GetRecords(@ MX) = %+v, %v", got, err)
	}
	if get := findCall(t, r.lastCalls(), "Get-DnsServerResourceRecord"); get["RRType"] != "Mx" {
		t.Fatalf("unexpected Get call %v", get)
	}

	// Name without records, or without records of that type: empty, no error.
	for _, q := range [][2]string{{"nothing", TypeA}, {"www", TypeTXT}} {
		if got, err := c.GetRecords(ctx, "example.com", q[0], q[1]); err != nil || got == nil || len(got) != 0 {
			t.Fatalf("GetRecords(%v) = %#v, %v", q, got, err)
		}
	}
	if _, err := c.GetRecords(ctx, "nope.example", "www", TypeA); !IsNotFound(err) {
		t.Fatalf("missing zone should be not found, got %v", err)
	}

	// Whole zone: SOA and NS come back with best-effort data.
	r.lastCalls()
	all, err := c.ListRecords(ctx, "example.com", "", "")
	if err != nil || len(all) != 8 {
		t.Fatalf("ListRecords() = %+v, %v", all, err)
	}
	if all[0].Type != "SOA" || all[0].Data != "stub. hostmaster. 1 900 600 86400 3600" || all[1].Data != "stub." {
		t.Fatalf("unexpected SOA/NS %+v %+v", all[0], all[1])
	}
	if get := findCall(t, r.lastCalls(), "Get-DnsServerResourceRecord"); get["Name"] != nil || get["Node"] != nil || get["RRType"] != nil {
		t.Fatalf("whole-zone listing must not filter: %v", get)
	}
	if ns, err := c.ListRecords(ctx, "example.com", "@", "ns"); err != nil || len(ns) != 1 || ns[0].Type != "NS" {
		t.Fatalf("ListRecords(@, ns) = %+v, %v", ns, err)
	}
	if cn, err := c.ListRecords(ctx, "example.com", "ftp", ""); err != nil || len(cn) != 1 || cn[0].HostName != "www.example.com." {
		t.Fatalf("ListRecords(ftp) = %+v, %v", cn, err)
	}
}

// TestPwshRecordRemoveAndTTL removes one MX of two (matching on preference
// and exchange) and one A by IPv6-insensitive address, updates a TTL, and
// checks not-found when nothing matches.
func TestPwshRecordRemoveAndTTL(t *testing.T) {
	ctx := context.Background()
	r := newPwshRunner(t, seedRecords)
	c := New(r, "")

	// Exchange spelled without the trailing dot and in another case.
	if err := c.RemoveRecord(ctx, "example.com", Record{Name: "@", Type: TypeMX, Exchange: "MAIL.example.com", Preference: 20}); err != nil {
		t.Fatal(err)
	}
	rm := findCall(t, r.lastCalls(), "Remove-DnsServerResourceRecord")
	if rm["Force"] != true || rm["ZoneName"] != "example.com" || !strings.HasPrefix(rm["InputObject"].(string), "@/MX/") {
		t.Fatalf("unexpected Remove call %v", rm)
	}
	// Stub record Ids follow seed order (SOA 1, NS 2, www 3-4, pc01 5, MX 6-7,
	// ftp 8), so the preference 20 MX is Id 7.
	if rm["InputObject"] != "@/MX/7" {
		t.Fatalf("removed the wrong MX: %v", rm)
	}
	err := c.RemoveRecord(ctx, "example.com", Record{Name: "@", Type: TypeMX, Exchange: "mail.example.com", Preference: 30})
	if !IsNotFound(err) {
		t.Fatalf("no match should be not found, got %v", err)
	}
	if len(findCalls(r.lastCalls(), "Remove-DnsServerResourceRecord")) != 0 {
		t.Fatal("Remove must not be called when nothing matches")
	}
	if err := c.RemoveRecord(ctx, "example.com", Record{Name: "nothing", Type: TypeA, Address: "10.0.0.9"}); !IsNotFound(err) {
		t.Fatalf("missing name should be not found, got %v", err)
	}

	// TTL update of one of the two www A records.
	if err := c.SetRecordTTL(ctx, "example.com", Record{Name: "www", Type: TypeA, Address: "10.0.0.2"}, 600); err != nil {
		t.Fatal(err)
	}
	set := findCall(t, r.lastCalls(), "Set-DnsServerResourceRecord")
	if set["OldInputObject"] != "www/A/4" || set["NewInputObject"] != "www/A/4" || set["ZoneName"] != "example.com" {
		t.Fatalf("unexpected Set call %v", set)
	}
	if err := c.SetRecordTTL(ctx, "example.com", Record{Name: "www", Type: TypeA, Address: "10.9.9.9"}, 600); !IsNotFound(err) {
		t.Fatalf("no match should be not found, got %v", err)
	}
	if err := c.SetRecordTTL(ctx, "example.com", Record{Name: "www", Type: TypeA, Address: "10.0.0.2"}, 0); err == nil {
		t.Fatal("TTL 0 must be rejected")
	}
}

// TestPwshBrokenScript: a script that fails to parse must surface an error.
func TestPwshBrokenScript(t *testing.T) {
	r := newPwshRunner(t, "")
	c := New(r, "")
	if _, err := c.run(context.Background(), psscript.Script{Op: "broken", Body: "$out = @("}, nil, nil); err == nil {
		t.Fatal("expected an error")
	}
}

// TestPwshAvailable checks the module probe passes against the stubs.
func TestPwshAvailable(t *testing.T) {
	r := newPwshRunner(t, "")
	if err := New(r, "").CheckAvailable(context.Background()); err != nil {
		t.Fatal(err)
	}
}
