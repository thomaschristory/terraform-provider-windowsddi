package dnsfake

// Tests driving the fake through the real dns.Client, so the op names,
// parameters and envelopes of both sides stay in sync.

import (
	"context"
	"reflect"
	"testing"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/psscript"
)

func TestZones(t *testing.T) {
	ctx := context.Background()
	s := New()
	c := dns.New(s, "")

	if err := c.CheckAvailable(ctx); err != nil {
		t.Fatal(err)
	}
	z, err := c.AddPrimaryZone(ctx, dns.PrimaryZoneInput{Name: "Example.com", ZoneFile: "example.com.dns"})
	if err != nil || z.DynamicUpdate != "None" || z.IsDsIntegrated || z.ZoneFile != "example.com.dns" {
		t.Fatalf("AddPrimaryZone() = %+v, %v", z, err)
	}
	if _, err := c.AddPrimaryZone(ctx, dns.PrimaryZoneInput{Name: "example.com", ZoneFile: "x.dns"}); err == nil || dns.IsNotFound(err) {
		t.Fatalf("duplicate zone should fail, got %v", err)
	}
	if _, err := c.AddPrimaryZone(ctx, dns.PrimaryZoneInput{Name: "f.example", ZoneFile: "f.dns", DynamicUpdate: "Secure"}); err == nil {
		t.Fatal("Secure on a file-backed zone should fail")
	}
	z, err = c.AddPrimaryZone(ctx, dns.PrimaryZoneInput{NetworkID: "2001:db8::/32", ReplicationScope: "Forest"})
	if err != nil || z.Name != "8.b.d.0.1.0.0.2.ip6.arpa" || !z.IsReverseLookupZone || z.DynamicUpdate != "Secure" {
		t.Fatalf("AddPrimaryZone(network) = %+v, %v", z, err)
	}

	// Apex SOA and NS exist, but they are not user records.
	if has, err := c.ZoneHasUserRecords(ctx, "EXAMPLE.com"); err != nil || has {
		t.Fatalf("ZoneHasUserRecords() = %v, %v", has, err)
	}
	all, err := c.ListRecords(ctx, "example.com", "", "")
	if err != nil || len(all) != 2 || all[0].Type != "SOA" || all[1].Data != "fake." {
		t.Fatalf("ListRecords() = %+v, %v", all, err)
	}

	z, err = c.SetPrimaryZone(ctx, "example.com", dns.PrimaryZoneUpdate{ReplicationScope: "Domain", DynamicUpdate: "Secure"})
	if err != nil || !z.IsDsIntegrated || z.ZoneFile != "" || z.DynamicUpdate != "Secure" {
		t.Fatalf("SetPrimaryZone() = %+v, %v", z, err)
	}
	if zs, err := c.ListZones(ctx); err != nil || len(zs) != 2 {
		t.Fatalf("ListZones() = %+v, %v", zs, err)
	}
	if err := c.RemoveZone(ctx, "example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetZone(ctx, "example.com"); !dns.IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
	if err := c.RemoveZone(ctx, "example.com"); !dns.IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
	if len(s.Records["example.com"]) != 0 {
		t.Fatal("records must go with the zone")
	}
}

func TestForwarders(t *testing.T) {
	ctx := context.Background()
	s := New()
	c := dns.New(s, "")
	z, err := c.AddConditionalForwarder(ctx, dns.ConditionalForwarderInput{Name: "partner.example", MasterServers: []string{"10.0.0.53", "2001:DB8::53"}})
	if err != nil || z.ForwarderTimeout != 5 || !reflect.DeepEqual(z.MasterServers, []string{"10.0.0.53", "2001:db8::53"}) {
		t.Fatalf("AddConditionalForwarder() = %+v, %v", z, err)
	}
	z, err = c.SetConditionalForwarder(ctx, dns.ConditionalForwarderInput{Name: "partner.example", MasterServers: []string{"10.0.0.54"}, ReplicationScope: "Forest"})
	if err != nil || z.ForwarderTimeout != 5 || !z.IsDsIntegrated || z.MasterServers[0] != "10.0.0.54" {
		t.Fatalf("SetConditionalForwarder() = %+v, %v", z, err)
	}
	if _, err := c.SetConditionalForwarder(ctx, dns.ConditionalForwarderInput{Name: "nope", MasterServers: []string{"10.0.0.1"}}); !dns.IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
	s.SeedZone(dns.Zone{Name: "p.example", ZoneType: dns.ZoneTypePrimary})
	if _, err := c.SetConditionalForwarder(ctx, dns.ConditionalForwarderInput{Name: "p.example", MasterServers: []string{"10.0.0.1"}}); err == nil || dns.IsNotFound(err) {
		t.Fatalf("setting a primary zone as forwarder should fail, got %v", err)
	}
}

func TestRecords(t *testing.T) {
	ctx := context.Background()
	s := New()
	c := dns.New(s, "")
	s.SeedZone(dns.Zone{Name: "example.com", ZoneType: dns.ZoneTypePrimary})

	if err := c.AddRecord(ctx, "example.com", dns.Record{Name: "www", Type: dns.TypeA, Address: "10.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddRecord(ctx, "example.com", dns.Record{Name: "WWW", Type: dns.TypeA, Address: "10.0.0.1", TTL: 60}); err == nil || dns.IsNotFound(err) {
		t.Fatalf("duplicate record should fail, got %v", err)
	}
	if err := c.AddRecord(ctx, "example.com", dns.Record{Name: "www", Type: dns.TypeCNAME, HostName: "x.example.com"}); err == nil {
		t.Fatal("CNAME next to an A record should fail")
	}
	if err := c.AddRecord(ctx, "nope", dns.Record{Name: "www", Type: dns.TypeA, Address: "10.0.0.1"}); !dns.IsNotFound(err) {
		t.Fatalf("missing zone should be not found, got %v", err)
	}
	if err := c.AddRecord(ctx, "example.com", dns.Record{Name: "mx", Type: dns.TypeMX, Exchange: "Mail.example.com", Preference: 5, TTL: 300}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddRecord(ctx, "example.com", dns.Record{Name: "v6", Type: dns.TypeAAAA, Address: "2001:DB8:0::1"}); err != nil {
		t.Fatal(err)
	}

	got, err := c.GetRecords(ctx, "example.com", "www", dns.TypeA)
	want := []dns.Record{{Name: "www", Type: dns.TypeA, TTL: DefaultTTL, Address: "10.0.0.1", Data: "10.0.0.1"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("GetRecords() = %+v, %v", got, err)
	}
	if got, _ := c.GetRecords(ctx, "example.com", "mx", dns.TypeMX); len(got) != 1 || got[0].Exchange != "Mail.example.com." || got[0].TTL != 300 {
		t.Fatalf("MX = %+v", got)
	}
	if got, _ := c.GetRecords(ctx, "example.com", "v6", dns.TypeAAAA); len(got) != 1 || got[0].Address != "2001:db8::1" {
		t.Fatalf("AAAA = %+v", got)
	}
	if got, err := c.GetRecords(ctx, "example.com", "none", dns.TypeA); err != nil || len(got) != 0 {
		t.Fatalf("GetRecords(none) = %+v, %v", got, err)
	}
	if _, err := c.GetRecords(ctx, "nope", "www", dns.TypeA); !dns.IsNotFound(err) {
		t.Fatalf("missing zone should be not found, got %v", err)
	}

	// Dynamic records seeded by tests are reported as such.
	s.SeedRecord("example.com", dns.Record{Name: "pc01", Type: dns.TypeA, Address: "10.0.0.50", Dynamic: true})
	if got, _ := c.GetRecords(ctx, "example.com", "pc01", dns.TypeA); len(got) != 1 || !got[0].Dynamic || got[0].TTL != DefaultTTL {
		t.Fatalf("seeded dynamic record = %+v", got)
	}

	// TTL update and removal, matching on data regardless of spelling.
	if err := c.SetRecordTTL(ctx, "example.com", dns.Record{Name: "mx", Type: dns.TypeMX, Exchange: "mail.EXAMPLE.com.", Preference: 5}, 900); err != nil {
		t.Fatal(err)
	}
	if got := s.RecordsAt("example.com", "mx", "MX"); got[0].TTL != 900 {
		t.Fatalf("TTL not updated: %+v", got)
	}
	if err := c.RemoveRecord(ctx, "example.com", dns.Record{Name: "mx", Type: dns.TypeMX, Exchange: "mail.example.com", Preference: 6}); !dns.IsNotFound(err) {
		t.Fatalf("no match should be not found, got %v", err)
	}
	if err := c.RemoveRecord(ctx, "example.com", dns.Record{Name: "mx", Type: dns.TypeMX, Exchange: "mail.example.com", Preference: 5}); err != nil {
		t.Fatal(err)
	}
	if got := s.RecordsAt("example.com", "mx", ""); len(got) != 0 {
		t.Fatalf("record not removed: %+v", got)
	}
	if has, _ := c.ZoneHasUserRecords(ctx, "example.com"); !has {
		t.Fatal("zone has user records")
	}
}

func TestFailAndCalls(t *testing.T) {
	s := New()
	s.Fail = func(op string, _ map[string]any) *psscript.Error {
		if op == "dns.zone.get" {
			return &psscript.Error{Message: "boom", Category: "ConnectionError", Command: "Get-DnsServerZone"}
		}
		return nil
	}
	c := dns.New(s, "dns02")
	if _, err := c.GetZone(context.Background(), "x"); err == nil || dns.IsNotFound(err) {
		t.Fatalf("expected injected failure, got %v", err)
	}
	if ops := s.Ops(); len(ops) != 1 || ops[0] != "dns.zone.get" || s.Calls[0].Params["computer_name"] != "dns02" {
		t.Fatalf("unexpected calls %v %v", ops, s.Calls)
	}
}
