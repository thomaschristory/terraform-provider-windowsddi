package recordset

// Engine tests against dnsfake, driven through the real dns.Client so the
// scripts' op names and parameters stay in sync with the fake.

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns/dnsfake"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/psscript"
)

const zone = "example.test"

// setup returns a fake with one primary zone and a client talking to it.
func setup(t *testing.T) (*dnsfake.Server, *dns.Client) {
	t.Helper()
	srv := dnsfake.New()
	srv.SeedZone(dns.Zone{Name: zone, ZoneType: dns.ZoneTypePrimary, ZoneFile: zone + ".dns"})
	return srv, dns.New(srv, "")
}

// a returns an A value.
func a(addr string) dns.Record { return dns.Record{Address: addr} }

// addrs returns the sorted addresses and TTLs stored for www/A.
func addrs(srv *dnsfake.Server, name string) (out []string, ttls []int64) {
	for _, r := range srv.RecordsAt(zone, name, dns.TypeA) {
		out = append(out, r.Address)
		ttls = append(ttls, r.TTL)
	}
	sort.Strings(out)
	return out, ttls
}

func TestCreateReadUpdateDelete(t *testing.T) {
	ctx := context.Background()
	srv, c := setup(t)
	tu := Tuple{Zone: zone, Name: "www", Type: dns.TypeA}

	if _, _, err := Read(ctx, c, tu, 0); !dns.IsNotFound(err) {
		t.Fatalf("Read(empty) err = %v, want ErrNotFound", err)
	}
	if err := Create(ctx, c, tu, []dns.Record{a("10.0.0.1"), a("10.0.0.2")}, 300, false); err != nil {
		t.Fatal(err)
	}
	got, ttl, err := Read(ctx, c, tu, 0)
	if err != nil || len(got) != 2 || ttl != 300 {
		t.Fatalf("Read() = %+v, %d, %v", got, ttl, err)
	}

	// Add .3, keep .2, drop .1, new TTL.
	srv.Calls = nil
	if err := Update(ctx, c, tu, []dns.Record{a("10.0.0.2"), a("10.0.0.3")}, 600); err != nil {
		t.Fatal(err)
	}
	want := []string{"dns.record.list", "dns.record.add", "dns.record.set_ttl", "dns.record.remove"}
	if ops := srv.Ops(); !reflect.DeepEqual(ops, want) {
		t.Fatalf("Update ops = %v, want %v (add before remove)", ops, want)
	}
	gotAddrs, ttls := addrs(srv, "www")
	if !reflect.DeepEqual(gotAddrs, []string{"10.0.0.2", "10.0.0.3"}) || ttls[0] != 600 || ttls[1] != 600 {
		t.Fatalf("after Update: %v %v", gotAddrs, ttls)
	}

	// TTL 0 leaves TTLs alone.
	if err := Update(ctx, c, tu, []dns.Record{a("10.0.0.2")}, 0); err != nil {
		t.Fatal(err)
	}
	if _, ttls := addrs(srv, "www"); len(ttls) != 1 || ttls[0] != 600 {
		t.Fatalf("TTL changed by Update(ttl=0): %v", ttls)
	}

	if err := Delete(ctx, c, tu); err != nil {
		t.Fatal(err)
	}
	if n := len(srv.RecordsAt(zone, "www", "")); n != 0 {
		t.Fatalf("%d records left after Delete", n)
	}
	// Deleting again, or in a missing zone, is fine.
	if err := Delete(ctx, c, tu); err != nil {
		t.Fatal(err)
	}
	if err := Delete(ctx, c, Tuple{Zone: "gone.test", Name: "www", Type: dns.TypeA}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Read(ctx, c, Tuple{Zone: "gone.test", Name: "www", Type: dns.TypeA}, 0); !dns.IsNotFound(err) {
		t.Fatalf("Read(missing zone) err = %v", err)
	}
}

func TestCreateExisting(t *testing.T) {
	ctx := context.Background()
	srv, c := setup(t)
	tu := Tuple{Zone: zone, Name: "host", Type: dns.TypeA}

	// Dynamic only: refused without the flag, adopted with it.
	srv.SeedRecord(zone, dns.Record{Name: "host", Type: dns.TypeA, Address: "10.0.0.9", Dynamic: true})
	err := Create(ctx, c, tu, []dns.Record{a("10.0.0.1")}, 0, false)
	var ex *ExistingRecordsError
	if !errors.As(err, &ex) || !ex.Dynamic {
		t.Fatalf("Create over dynamic = %v, want dynamic ExistingRecordsError", err)
	}
	if err := Create(ctx, c, tu, []dns.Record{a("10.0.0.1")}, 0, true); err != nil {
		t.Fatal(err)
	}
	if got, ttls := addrs(srv, "host"); !reflect.DeepEqual(got, []string{"10.0.0.1"}) || ttls[0] != dnsfake.DefaultTTL {
		t.Fatalf("after adopting: %v %v", got, ttls)
	}

	// Static: refused even with the flag.
	err = Create(ctx, c, tu, []dns.Record{a("10.0.0.2")}, 0, true)
	if !errors.As(err, &ex) || ex.Dynamic || ex.Tuple.ImportID() != "example.test/host" {
		t.Fatalf("Create over static = %v", err)
	}
}

func TestCreateRollbackAndDuplicates(t *testing.T) {
	ctx := context.Background()
	srv, c := setup(t)
	tu := Tuple{Zone: zone, Name: "rb", Type: dns.TypeA}

	dup := Tuple{Zone: zone, Name: "rb", Type: dns.TypeAAAA}
	if err := Create(ctx, c, dup, []dns.Record{a("2001:db8::1"), a("2001:DB8:0::1")}, 0, false); err == nil {
		t.Fatal("duplicate values should fail")
	}
	n := 0
	srv.Fail = func(op string, _ map[string]any) *psscript.Error {
		if op == "dns.record.add" {
			n++
			if n == 2 {
				return &psscript.Error{Message: "boom", Category: "InvalidOperation"}
			}
		}
		return nil
	}
	if err := Create(ctx, c, tu, []dns.Record{a("10.0.0.1"), a("10.0.0.2")}, 0, false); err == nil {
		t.Fatal("Create should fail")
	}
	srv.Fail = nil
	if got := srv.RecordsAt(zone, "rb", ""); len(got) != 0 {
		t.Fatalf("rollback left %+v", got)
	}
}

func TestReadTTLDisagreement(t *testing.T) {
	ctx := context.Background()
	srv, c := setup(t)
	tu := Tuple{Zone: zone, Name: "mix", Type: dns.TypeA}
	srv.SeedRecord(zone, dns.Record{Name: "mix", Type: dns.TypeA, Address: "10.0.0.1", TTL: 300})
	srv.SeedRecord(zone, dns.Record{Name: "mix", Type: dns.TypeA, Address: "10.0.0.2", TTL: 60})
	for prior, want := range map[int64]int64{300: 60, 60: 300, 0: 300} {
		if _, ttl, err := Read(ctx, c, tu, prior); err != nil || ttl != want {
			t.Errorf("Read(prior %d) ttl = %d, %v, want %d", prior, ttl, err, want)
		}
	}
}

func TestUpdateRemovesOutOfBand(t *testing.T) {
	ctx := context.Background()
	srv, c := setup(t)
	tu := Tuple{Zone: zone, Name: "@", Type: dns.TypeMX}
	srv.SeedRecord(zone, dns.Record{Name: "@", Type: dns.TypeMX, Exchange: "old.example.test.", Preference: 5})
	mx := dns.Record{Exchange: "Mail.Example.test", Preference: 10}
	if err := Update(ctx, c, tu, []dns.Record{mx}, 0); err != nil {
		t.Fatal(err)
	}
	got := srv.RecordsAt(zone, "@", dns.TypeMX)
	if len(got) != 1 || got[0].Exchange != "Mail.Example.test." {
		t.Fatalf("after Update: %+v", got)
	}
	// Same value in another spelling is kept, not re-added.
	srv.Calls = nil
	if err := Update(ctx, c, tu, []dns.Record{{Exchange: "mail.example.test.", Preference: 10}}, 0); err != nil {
		t.Fatal(err)
	}
	if ops := srv.Ops(); !reflect.DeepEqual(ops, []string{"dns.record.list"}) {
		t.Fatalf("no-op Update ran %v", ops)
	}
}

func TestPreserveSpelling(t *testing.T) {
	server := []dns.Record{
		{Name: "www", Type: dns.TypeAAAA, Address: "2001:db8::1", TTL: 60},
		{Name: "www", Type: dns.TypeAAAA, Address: "2001:DB8::2", TTL: 60},
	}
	prior := []dns.Record{{Address: "2001:DB8:0:0::1"}, {Address: "2001:db8::99"}}
	got := PreserveSpelling(prior, server)
	if got[0].Address != "2001:DB8:0:0::1" || got[0].TTL != 60 || got[0].Name != "www" {
		t.Errorf("kept spelling: %+v", got[0])
	}
	if got[1].Address != "2001:db8::2" {
		t.Errorf("canonical: %+v", got[1])
	}

	cname := []dns.Record{{Type: dns.TypeCNAME, HostName: "Target.Example.test."}}
	if got := PreserveSpelling(nil, cname); got[0].HostName != "target.example.test." {
		t.Errorf("canonical host: %+v", got[0])
	}
	if got := PreserveSpelling([]dns.Record{{HostName: "TARGET.example.test"}}, cname); got[0].HostName != "TARGET.example.test" {
		t.Errorf("kept host: %+v", got[0])
	}
	srv := []dns.Record{{Type: dns.TypeSRV, Target: "SIP.example.test.", Priority: 1, Weight: 2, Port: 3}}
	if got := PreserveSpelling([]dns.Record{{Target: "sip.example.test", Priority: 1, Weight: 2, Port: 4}}, srv); got[0].Target != "sip.example.test." {
		t.Errorf("different port must not keep spelling: %+v", got[0])
	}
}

// TestLockSerialises checks that the keyed mutex serialises one tuple but
// not different tuples.
func TestLockSerialises(t *testing.T) {
	t1 := Tuple{Zone: "Z.test", Name: "WWW", Type: dns.TypeA}
	t2 := Tuple{Zone: "z.test", Name: "www", Type: dns.TypeA}
	other := Tuple{Zone: "z.test", Name: "www", Type: dns.TypeAAAA}
	unlock := lock(t1)
	// A different tuple is not blocked.
	lock(other)()
	var wg sync.WaitGroup
	acquired := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		lock(t2)()
		close(acquired)
	}()
	time.Sleep(20 * time.Millisecond)
	select {
	case <-acquired:
		t.Fatal("same tuple (other casing) was not serialised")
	default:
	}
	unlock()
	wg.Wait()
}

// TestUpdateCNAMETarget checks that a CNAME target change works even though
// the server refuses a second CNAME on the same name: the old target must be
// removed before the new one is added.
func TestUpdateCNAMETarget(t *testing.T) {
	srv, c := setup(t)
	tu := Tuple{Zone: zone, Name: "alias", Type: dns.TypeCNAME}
	ctx := context.Background()
	if err := Create(ctx, c, tu, []dns.Record{{HostName: "a.example.test."}}, 300, false); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := Update(ctx, c, tu, []dns.Record{{HostName: "b.example.test."}}, 600); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := srv.RecordsAt(zone, "alias", dns.TypeCNAME)
	if len(got) != 1 || got[0].HostName != "b.example.test." || got[0].TTL != 600 {
		t.Fatalf("after update: %+v", got)
	}

	// A failing add restores the old target: the first add (the new target)
	// fails, the second one (the rollback) succeeds.
	adds := 0
	srv.Fail = func(op string, _ map[string]any) *psscript.Error {
		if op == "dns.record.add" {
			adds++
			if adds == 1 {
				return &psscript.Error{Message: "boom", Category: "InvalidOperation"}
			}
		}
		return nil
	}
	if err := Update(ctx, c, tu, []dns.Record{{HostName: "c.example.test."}}, 600); err == nil {
		t.Fatal("expected an error")
	}
	srv.Fail = nil
	got = srv.RecordsAt(zone, "alias", dns.TypeCNAME)
	if len(got) != 1 || got[0].HostName != "b.example.test." {
		t.Fatalf("rollback: %+v", got)
	}
}
