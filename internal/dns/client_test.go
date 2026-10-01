package dns

// Unit tests for the dns client. No Windows host and no PowerShell needed.
//
// Like internal/dhcp/client_test.go: fixtureRunner replaces the SSH/WinRM
// runner, records what the client tried to run (op, script, `$p`) and
// replies with a recorded JSON envelope from testdata/. The tests check the
// parameters sent and how replies are decoded (structs, arrays of one,
// ErrNotFound, errors naming the cmdlet).
//
// Run with: go test ./internal/dns/

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/psscript"
)

// fixtureRunner replies with a recorded fixture and records the call. When
// err is set, Run fails with it instead (a transport error).
type fixtureRunner struct {
	t       *testing.T
	fixture string
	err     error
	calls   int
	op      string
	script  string
	params  map[string]any
}

func (f *fixtureRunner) Run(_ context.Context, script string, params any) ([]byte, error) {
	f.calls++
	f.op = psscript.Op(script)
	f.script = script
	f.params, _ = params.(map[string]any)
	if f.err != nil {
		return nil, f.err
	}
	b, err := os.ReadFile(filepath.Join("testdata", f.fixture))
	if err != nil {
		f.t.Fatal(err)
	}
	return b, nil
}

// newClient builds a Client answering every call with the given fixture.
func newClient(t *testing.T, fixture string) (*Client, *fixtureRunner) {
	r := &fixtureRunner{t: t, fixture: fixture}
	return New(r, ""), r
}

func TestGetZone(t *testing.T) {
	c, r := newClient(t, "zone_get.json")
	z, err := c.GetZone(context.Background(), "corp.example.com")
	if err != nil {
		t.Fatal(err)
	}
	want := &Zone{Name: "corp.example.com", ZoneType: ZoneTypePrimary, IsDsIntegrated: true, ReplicationScope: "Domain",
		DynamicUpdate: "Secure", MasterServers: []string{}}
	if !reflect.DeepEqual(z, want) {
		t.Fatalf("GetZone() = %+v", z)
	}
	if r.op != "dns.zone.get" || r.params["name"] != "corp.example.com" {
		t.Fatalf("unexpected call %s %v", r.op, r.params)
	}
	if _, ok := r.params["computer_name"]; ok {
		t.Fatal("computer_name must not be sent when unset")
	}
	// Every DNS script carries the DnsServer module check.
	if !strings.Contains(r.script, "Get-Command -Name 'Get-DnsServerZone'") || !strings.Contains(r.script, "Get-DnsServerZone @cn -Name $p.name") {
		t.Fatalf("unexpected script:\n%s", r.script)
	}

	c, _ = newClient(t, "zone_forwarder.json")
	z, err = c.GetZone(context.Background(), "partner.example")
	if err != nil || z.ZoneType != ZoneTypeForwarder || z.ForwarderTimeout != 5 || !reflect.DeepEqual(z.MasterServers, []string{"10.0.0.53"}) {
		t.Fatalf("GetZone(forwarder) = %+v, %v", z, err)
	}
}

func TestComputerName(t *testing.T) {
	r := &fixtureRunner{t: t, fixture: "zone_get.json"}
	if _, err := New(r, "dns02.example.local").GetZone(context.Background(), "corp.example.com"); err != nil {
		t.Fatal(err)
	}
	if r.params["computer_name"] != "dns02.example.local" {
		t.Fatalf("computer_name not sent: %v", r.params)
	}
}

func TestZoneNotFoundAndTransportError(t *testing.T) {
	c, _ := newClient(t, "zone_not_found.json")
	_, err := c.GetZone(context.Background(), "nope.example")
	if !IsNotFound(err) || !strings.Contains(err.Error(), "Get-DnsServerZone") {
		t.Fatalf("expected not found naming the cmdlet, got %v", err)
	}
	r := &fixtureRunner{t: t, err: errors.New("ssh: connection refused")}
	if _, err := New(r, "").GetZone(context.Background(), "x"); err == nil || IsNotFound(err) {
		t.Fatalf("expected transport error, got %v", err)
	}
}

func TestListZones(t *testing.T) {
	c, r := newClient(t, "zone_list.json")
	zs, err := c.ListZones(context.Background())
	if err != nil || len(zs) != 2 || !zs[0].IsAutoCreated || zs[1].ReplicationScope != "Forest" || r.op != "dns.zone.list" {
		t.Fatalf("ListZones() = %+v, %v", zs, err)
	}
	for _, fx := range []string{"list_empty.json", "ok_null.json"} {
		c, _ = newClient(t, fx)
		if zs, err = c.ListZones(context.Background()); err != nil || len(zs) != 0 {
			t.Fatalf("%s: ListZones() = %+v, %v", fx, zs, err)
		}
	}
}

func TestAddPrimaryZone(t *testing.T) {
	ctx := context.Background()
	c, r := newClient(t, "zone_get.json")
	if _, err := c.AddPrimaryZone(ctx, PrimaryZoneInput{Name: "corp.example.com", ReplicationScope: "Domain"}); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"name": "corp.example.com", "network_id": "", "replication_scope": "Domain", "zone_file": "",
		"dynamic_update": "", "lookup_name": "corp.example.com"}
	if r.op != "dns.zone.add_primary" || !reflect.DeepEqual(r.params, want) {
		t.Fatalf("AddPrimaryZone sent %s %#v", r.op, r.params)
	}

	// Network ID: the Go side computes the fallback lookup name.
	c, r = newClient(t, "zone_reverse.json")
	z, err := c.AddPrimaryZone(ctx, PrimaryZoneInput{NetworkID: "10.1.2.0/24", ZoneFile: "2.1.10.in-addr.arpa.dns"})
	if err != nil || z.Name != "2.1.10.in-addr.arpa" || !z.IsReverseLookupZone {
		t.Fatalf("AddPrimaryZone(network) = %+v, %v", z, err)
	}
	if r.params["lookup_name"] != "2.1.10.in-addr.arpa" || r.params["network_id"] != "10.1.2.0/24" || r.params["name"] != "" {
		t.Fatalf("unexpected params %v", r.params)
	}

	// Invalid combinations fail before anything runs.
	bad := []PrimaryZoneInput{
		{ZoneFile: "x.dns"},
		{Name: "a", NetworkID: "10.0.0.0/8", ZoneFile: "x.dns"},
		{Name: "a"},
		{Name: "a", ZoneFile: "a.dns", ReplicationScope: "Forest"},
		{NetworkID: "10.1.0.0/20", ZoneFile: "x.dns"},
	}
	for _, in := range bad {
		c, r = newClient(t, "zone_get.json")
		if _, err := c.AddPrimaryZone(ctx, in); err == nil || r.calls != 0 {
			t.Fatalf("AddPrimaryZone(%+v) should fail without running: %v", in, err)
		}
	}
}

func TestSetAndRemoveZone(t *testing.T) {
	ctx := context.Background()
	c, r := newClient(t, "zone_get.json")
	if _, err := c.SetPrimaryZone(ctx, "corp.example.com", PrimaryZoneUpdate{DynamicUpdate: "Secure"}); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"name": "corp.example.com", "replication_scope": "", "dynamic_update": "Secure"}
	if r.op != "dns.zone.set_primary" || !reflect.DeepEqual(r.params, want) {
		t.Fatalf("SetPrimaryZone sent %s %#v", r.op, r.params)
	}

	c, r = newClient(t, "ok_null.json")
	if err := c.RemoveZone(ctx, "corp.example.com"); err != nil || r.op != "dns.zone.remove" || r.params["name"] != "corp.example.com" {
		t.Fatalf("RemoveZone: %v %s %v", err, r.op, r.params)
	}
	c, _ = newClient(t, "zone_not_found.json")
	if err := c.RemoveZone(ctx, "nope.example"); !IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestZoneHasUserRecords(t *testing.T) {
	ctx := context.Background()
	for fx, want := range map[string]bool{"ok_true.json": true, "ok_false.json": false} {
		c, r := newClient(t, fx)
		has, err := c.ZoneHasUserRecords(ctx, "corp.example.com")
		if err != nil || has != want || r.op != "dns.zone.has_records" {
			t.Fatalf("%s: ZoneHasUserRecords() = %v, %v", fx, has, err)
		}
	}
	c, _ := newClient(t, "zone_not_found.json")
	if _, err := c.ZoneHasUserRecords(ctx, "nope.example"); !IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestConditionalForwarder(t *testing.T) {
	ctx := context.Background()
	in := ConditionalForwarderInput{Name: "partner.example", MasterServers: []string{"10.0.0.53"}, ForwarderTimeout: 3}
	c, r := newClient(t, "zone_forwarder.json")
	if _, err := c.AddConditionalForwarder(ctx, in); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"name": "partner.example", "master_servers": []string{"10.0.0.53"}, "replication_scope": "", "forwarder_timeout": int64(3)}
	if r.op != "dns.forwarder.add" || !reflect.DeepEqual(r.params, want) {
		t.Fatalf("AddConditionalForwarder sent %s %#v", r.op, r.params)
	}
	if _, err := c.SetConditionalForwarder(ctx, in); err != nil || r.op != "dns.forwarder.set" {
		t.Fatalf("SetConditionalForwarder: %v %s", err, r.op)
	}
	c, r = newClient(t, "zone_forwarder.json")
	if _, err := c.AddConditionalForwarder(ctx, ConditionalForwarderInput{Name: "x"}); err == nil || r.calls != 0 {
		t.Fatalf("no master servers must fail without running: %v", err)
	}
}

func TestGetRecords(t *testing.T) {
	ctx := context.Background()
	c, r := newClient(t, "record_list_one.json")
	got, err := c.GetRecords(ctx, "corp.example.com", "pc01", "aaaa")
	want := []Record{{Name: "pc01", Type: TypeAAAA, TTL: 1200, Dynamic: true, Address: "2001:db8::50", Data: "2001:db8::50"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("GetRecords() = %+v, %v", got, err)
	}
	wantParams := map[string]any{"zone": "corp.example.com", "name": "pc01", "rr_type": "AAAA"}
	if r.op != "dns.record.list" || !reflect.DeepEqual(r.params, wantParams) {
		t.Fatalf("GetRecords sent %s %#v", r.op, r.params)
	}

	// No records: an empty, non-nil slice.
	for _, fx := range []string{"list_empty.json", "ok_null.json"} {
		c, _ = newClient(t, fx)
		if got, err = c.GetRecords(ctx, "corp.example.com", "www", TypeA); err != nil || got == nil || len(got) != 0 {
			t.Fatalf("%s: GetRecords() = %#v, %v", fx, got, err)
		}
	}
	c, _ = newClient(t, "zone_not_found.json")
	if _, err = c.GetRecords(ctx, "nope.example", "www", TypeA); !IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
	c, r = newClient(t, "list_empty.json")
	if _, err = c.GetRecords(ctx, "corp.example.com", "www", "NS"); err == nil || r.calls != 0 {
		t.Fatalf("unsupported type must fail without running: %v", err)
	}
	if _, err = c.GetRecords(ctx, "corp.example.com", "", TypeA); err == nil || r.calls != 0 {
		t.Fatalf("empty name must fail without running: %v", err)
	}
}

func TestListRecords(t *testing.T) {
	c, r := newClient(t, "record_list.json")
	got, err := c.ListRecords(context.Background(), "corp.example.com", "", "")
	if err != nil || len(got) != 4 {
		t.Fatalf("ListRecords() = %+v, %v", got, err)
	}
	wantData := []string{
		"dns01.corp.example.com. hostmaster.corp.example.com. 42 900 600 86400 3600",
		"10 mail.corp.example.com.",
		"1 2 5060 sip.corp.example.com.",
		`"say \"hi\""`,
	}
	for i, w := range wantData {
		if got[i].Data != w {
			t.Errorf("record %d data = %q, want %q", i, got[i].Data, w)
		}
	}
	if r.params["name"] != "" || r.params["rr_type"] != "" {
		t.Fatalf("unexpected params %v", r.params)
	}
	// Known types are sent in -RRType spelling, others as given.
	for in, want := range map[string]string{"cname": "CName", "NS": "NS", "srv": "Srv"} {
		if _, err := c.ListRecords(context.Background(), "corp.example.com", "", in); err != nil || r.params["rr_type"] != want {
			t.Fatalf("ListRecords(type %s) sent %v", in, r.params["rr_type"])
		}
	}
}

func TestAddRemoveSetRecord(t *testing.T) {
	ctx := context.Background()
	mx := Record{Name: "@", Type: "mx", Exchange: "mail.corp.example.com", Preference: 10}
	wantRecord := map[string]any{"name": "@", "type": "MX", "ttl": int64(0), "address": "", "host_name": "",
		"exchange": "mail.corp.example.com", "preference": int64(10), "target": "", "priority": int64(0),
		"weight": int64(0), "port": int64(0), "text": ""}

	c, r := newClient(t, "ok_null.json")
	if err := c.AddRecord(ctx, "corp.example.com", mx); err != nil {
		t.Fatal(err)
	}
	if r.op != "dns.record.add" || r.params["zone"] != "corp.example.com" || !reflect.DeepEqual(r.params["record"], wantRecord) {
		t.Fatalf("AddRecord sent %s %#v", r.op, r.params)
	}

	c, _ = newClient(t, "record_exists.json")
	err := c.AddRecord(ctx, "corp.example.com", Record{Name: "www", Type: TypeA, Address: "10.0.0.1"})
	if err == nil || IsNotFound(err) || !strings.HasPrefix(err.Error(), "Add-DnsServerResourceRecordA:") {
		t.Fatalf("expected duplicate error, got %v", err)
	}

	c, r = newClient(t, "ok_count.json")
	if err := c.RemoveRecord(ctx, "corp.example.com", mx); err != nil || r.op != "dns.record.remove" || r.params["rr_type"] != "Mx" {
		t.Fatalf("RemoveRecord: %v %s %v", err, r.op, r.params)
	}
	if err := c.SetRecordTTL(ctx, "corp.example.com", mx, 600); err != nil || r.op != "dns.record.set_ttl" || r.params["ttl"] != int64(600) {
		t.Fatalf("SetRecordTTL: %v %s %v", err, r.op, r.params)
	}
	c, _ = newClient(t, "ok_null.json")
	if err := c.RemoveRecord(ctx, "corp.example.com", mx); !IsNotFound(err) {
		t.Fatalf("no match should be not found, got %v", err)
	}
	if err := c.SetRecordTTL(ctx, "corp.example.com", mx, 600); !IsNotFound(err) {
		t.Fatalf("no match should be not found, got %v", err)
	}

	// Invalid records fail before anything runs.
	c, r = newClient(t, "ok_null.json")
	for _, bad := range []Record{
		{Name: "www", Type: "NS"},
		{Name: "", Type: TypeA, Address: "10.0.0.1"},
		{Name: "www", Type: TypeA},
		{Name: "www", Type: TypeA, Address: "10.0.0.1", TTL: -1},
		{Name: "_x", Type: TypeSRV, Port: 1},
	} {
		if err := c.AddRecord(ctx, "corp.example.com", bad); err == nil {
			t.Errorf("AddRecord(%+v) should fail", bad)
		}
	}
	if r.calls != 0 {
		t.Fatalf("invalid records must not run scripts, got %d calls", r.calls)
	}
}

func TestRecordSameValueAndRData(t *testing.T) {
	cases := []struct {
		a, b Record
		same bool
	}{
		{Record{Type: TypeAAAA, Address: "2001:DB8::1"}, Record{Type: "aaaa", Address: "2001:db8:0::1"}, true},
		{Record{Type: TypeA, Address: "10.0.0.1"}, Record{Type: TypeA, Address: "10.0.0.2"}, false},
		{Record{Type: TypeCNAME, HostName: "WWW.example.com"}, Record{Type: TypeCNAME, HostName: "www.example.com."}, true},
		{Record{Type: TypeMX, Exchange: "m.", Preference: 10}, Record{Type: TypeMX, Exchange: "m", Preference: 20}, false},
		{Record{Type: TypeSRV, Target: "s.", Priority: 1, Weight: 2, Port: 3}, Record{Type: TypeSRV, Target: "S", Priority: 1, Weight: 2, Port: 3}, true},
		{Record{Type: TypeTXT, Text: "A"}, Record{Type: TypeTXT, Text: "a"}, false},
		{Record{Type: TypeA, Address: "10.0.0.1"}, Record{Type: TypeAAAA, Address: "10.0.0.1"}, false},
		{Record{Type: "NS", Data: "ns1."}, Record{Type: "NS", Data: "ns1."}, true},
	}
	for _, tc := range cases {
		if got := tc.a.SameValue(tc.b); got != tc.same {
			t.Errorf("%+v SameValue %+v = %v, want %v", tc.a, tc.b, got, tc.same)
		}
	}
	if got := (Record{Type: TypePTR, HostName: "h."}).RData(); got != "h." {
		t.Errorf("PTR RData = %q", got)
	}
	if got := (Record{Type: "NS", Data: "ns1."}).RData(); got != "ns1." {
		t.Errorf("NS RData = %q", got)
	}
}
