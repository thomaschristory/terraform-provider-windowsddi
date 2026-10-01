package dhcp

// Unit tests for the dhcp client. No Windows host and no PowerShell needed.
//
// How: each test swaps the real SSH/WinRM runner for fixtureRunner, which
// does not execute anything. It records what the client tried to run (the
// op name, the script text and the `$p` parameters) and replies with a
// recorded JSON envelope from testdata/ (for example scope_get.json or
// scope_not_found.json). The tests then check two things: the parameters
// sent to PowerShell, and how the reply is decoded (structs, ErrNotFound,
// error messages naming the cmdlet).
//
// Run with: go test ./internal/dhcp/
//
// Go note: test files end in _test.go and are only compiled by `go test`.
// Every func TestXxx(t *testing.T) is a test case; `t` reports failures.

// Go note: this file is in package dhcp (not dhcp_test), so tests can call
// unexported names like newClient, run and the scriptXxx variables.
import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/psscript"
)

// fixtureRunner replies with a recorded JSON fixture and records the call.
// It satisfies runner.Runner simply by having a matching Run method.
// When err is set, Run fails with it instead (simulates a transport error).
//
// Go note: interfaces in Go are satisfied implicitly. There is no
// "implements" keyword; any type with the right method set qualifies.
type fixtureRunner struct {
	t       *testing.T
	fixture string
	err     error
	op      string
	script  string
	params  map[string]any
}

// Run records the call, then returns either f.err or the fixture file bytes.
//
// Go note: `*fixtureRunner` is a pointer receiver, so the method can modify
// the struct's fields (f.op, f.params) and the test sees the changes.
// Go note: `_` as a parameter name means "this argument is ignored".
func (f *fixtureRunner) Run(_ context.Context, script string, params any) ([]byte, error) {
	f.op = psscript.Op(script)
	f.script = script
	// Go note: params.(map[string]any) is a type assertion: "treat this `any`
	// value as a map". The comma-ok form (value, ok) does not panic on a
	// mismatch; here ok is discarded with `_` and params stays nil instead.
	f.params, _ = params.(map[string]any)
	if f.err != nil {
		return nil, f.err
	}
	// Go tests run with the package directory as working directory, so the
	// relative testdata/ path works.
	b, err := os.ReadFile(filepath.Join("testdata", f.fixture))
	// Go note: `if err != nil` is the standard error check; t.Fatal marks
	// the test failed and stops it immediately.
	if err != nil {
		f.t.Fatal(err)
	}
	return b, nil
}

// newClient builds a Client wired to a fixtureRunner that answers with the
// given fixture file. It returns both so tests can inspect what was sent.
func newClient(t *testing.T, fixture string) (*Client, *fixtureRunner) {
	r := &fixtureRunner{t: t, fixture: fixture}
	return New(r, ""), r
}

// TestGetScope checks that GetScope decodes a scope, sends scope_id, omits
// computer_name when unset, and targets the right cmdlet in the script.
func TestGetScope(t *testing.T) {
	c, r := newClient(t, "scope_get.json")
	s, err := c.GetScope(context.Background(), "10.1.20.0")
	if err != nil {
		t.Fatal(err)
	}
	// Go note: reflect.DeepEqual compares two values field by field.
	want := &Scope{ScopeID: "10.1.20.0", Name: "Users VLAN 120", StartRange: "10.1.20.10", EndRange: "10.1.20.250",
		SubnetMask: "255.255.255.0", State: "Active", Type: "Dhcp", LeaseDuration: "8.00:00:00"}
	if !reflect.DeepEqual(s, want) {
		t.Fatalf("GetScope() = %+v", s)
	}
	if r.op != "scope.get" || r.params["scope_id"] != "10.1.20.0" {
		t.Fatalf("unexpected call %s %v", r.op, r.params)
	}
	// Go note: `v, ok := m[key]` is the comma-ok map lookup: ok is false
	// when the key is absent.
	if _, ok := r.params["computer_name"]; ok {
		t.Fatal("computer_name must not be sent when unset")
	}
	if !strings.Contains(r.script, "Get-DhcpServerv4Scope @cn -ScopeId $p.scope_id") {
		t.Fatalf("unexpected script:\n%s", r.script)
	}
}

// TestComputerName checks that a Client built with a computer name passes it
// to every script as computer_name (which becomes the @cn splat).
func TestComputerName(t *testing.T) {
	r := &fixtureRunner{t: t, fixture: "scope_get.json"}
	c := New(r, "dhcp02.example.local")
	if _, err := c.GetScope(context.Background(), "10.1.20.0"); err != nil {
		t.Fatal(err)
	}
	if r.params["computer_name"] != "dhcp02.example.local" {
		t.Fatalf("computer_name not sent: %v", r.params)
	}
}

// TestGetScopeNotFound checks that an ObjectNotFound error from the cmdlet
// maps to ErrNotFound and that the message still names the cmdlet.
func TestGetScopeNotFound(t *testing.T) {
	c, _ := newClient(t, "scope_not_found.json")
	_, err := c.GetScope(context.Background(), "10.9.9.0")
	if !IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
	if !strings.Contains(err.Error(), "Get-DhcpServerv4Scope") {
		t.Fatalf("error should name the cmdlet: %v", err)
	}
}

// TestRunnerError checks that a transport failure (e.g. SSH refused) is
// returned as an error and is NOT mistaken for "not found".
func TestRunnerError(t *testing.T) {
	r := &fixtureRunner{t: t, err: errors.New("ssh: connection refused")}
	c := New(r, "")
	if _, err := c.GetScope(context.Background(), "10.1.20.0"); err == nil || IsNotFound(err) {
		t.Fatalf("expected transport error, got %v", err)
	}
}

// TestListScopes checks decoding of a two-scope list, and that both an empty
// array and a null data field give an empty result without error.
func TestListScopes(t *testing.T) {
	c, r := newClient(t, "scope_list.json")
	got, err := c.ListScopes(context.Background())
	if err != nil || len(got) != 2 || got[1].State != "Inactive" || got[1].LeaseDuration != "0.08:00:00" {
		t.Fatalf("ListScopes() = %+v, %v", got, err)
	}
	if r.op != "scope.list" {
		t.Fatal(r.op)
	}
	// Go note: `for _, x := range list` loops over a slice; `_` drops the
	// index. []string{...} is a slice literal (an inline list).
	for _, fx := range []string{"scope_list_empty.json", "ok_null.json"} {
		c, _ = newClient(t, fx)
		got, err = c.ListScopes(context.Background())
		if err != nil || len(got) != 0 {
			t.Fatalf("%s: ListScopes() = %+v, %v", fx, got, err)
		}
	}
}

// TestAddSetScopeParams checks the exact `$p` map sent by AddScope and
// SetScope (8h lease becomes lease_seconds 28800), and that an Add returning
// no data is reported as not found.
func TestAddSetScopeParams(t *testing.T) {
	in := ScopeInput{
		ScopeID: "10.1.20.0", Name: "Users VLAN 120", StartRange: "10.1.20.10", EndRange: "10.1.20.250",
		SubnetMask: "255.255.255.0", State: "Active", Type: "Dhcp", LeaseDuration: 8 * time.Hour,
	}
	want := map[string]any{
		"scope_id": "10.1.20.0", "name": "Users VLAN 120", "description": "", "start_range": "10.1.20.10",
		"end_range": "10.1.20.250", "subnet_mask": "255.255.255.0", "state": "Active", "type": "Dhcp",
		"lease_seconds": int64(28800),
	}
	c, r := newClient(t, "scope_get.json")
	if _, err := c.AddScope(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if r.op != "scope.add" || !reflect.DeepEqual(r.params, want) {
		t.Fatalf("AddScope sent %s %#v", r.op, r.params)
	}
	if _, err := c.SetScope(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if r.op != "scope.set" || !reflect.DeepEqual(r.params, want) {
		t.Fatalf("SetScope sent %s %#v", r.op, r.params)
	}
	c, _ = newClient(t, "ok_null.json")
	if _, err := c.AddScope(context.Background(), in); !IsNotFound(err) {
		t.Fatalf("AddScope with no data should be not found, got %v", err)
	}
}

// TestRemoveScope checks the force flag is sent, and that the "scope has
// leases" failure surfaces as a real error naming Remove-DhcpServerv4Scope.
func TestRemoveScope(t *testing.T) {
	c, r := newClient(t, "ok_null.json")
	if err := c.RemoveScope(context.Background(), "10.1.20.0", true); err != nil {
		t.Fatal(err)
	}
	if r.op != "scope.remove" || r.params["force"] != true {
		t.Fatalf("unexpected call %s %v", r.op, r.params)
	}
	c, _ = newClient(t, "remove_with_leases.json")
	err := c.RemoveScope(context.Background(), "10.1.20.0", false)
	if err == nil || IsNotFound(err) || !strings.Contains(err.Error(), "Remove-DhcpServerv4Scope") {
		t.Fatalf("unexpected error %v", err)
	}
}

// TestReservation walks get/add/set/remove/list for reservations, plus both
// "not found" paths: no matching reservation (null data) and missing scope.
func TestReservation(t *testing.T) {
	ctx := context.Background()
	c, r := newClient(t, "reservation_get.json")
	got, err := c.GetReservation(ctx, "10.1.20.0", "10.1.20.5")
	if err != nil || got.ClientID != "aa-bb-cc-dd-ee-ff" || got.Name != "printer-01" {
		t.Fatalf("GetReservation() = %+v, %v", got, err)
	}
	if r.op != "reservation.get" || r.params["ip_address"] != "10.1.20.5" || r.params["scope_id"] != "10.1.20.0" {
		t.Fatalf("unexpected call %s %v", r.op, r.params)
	}

	in := ReservationInput{ScopeID: "10.1.20.0", IPAddress: "10.1.20.5", ClientID: "aa-bb-cc-dd-ee-ff", Type: "Both"}
	if _, err := c.AddReservation(ctx, in); err != nil || r.op != "reservation.add" || r.params["client_id"] != "aa-bb-cc-dd-ee-ff" {
		t.Fatalf("AddReservation: %v %s %v", err, r.op, r.params)
	}
	if _, err := c.SetReservation(ctx, in); err != nil || r.op != "reservation.set" {
		t.Fatalf("SetReservation: %v %s", err, r.op)
	}
	if err := c.RemoveReservation(ctx, "10.1.20.5"); err != nil || r.op != "reservation.remove" || r.params["ip_address"] != "10.1.20.5" {
		t.Fatalf("RemoveReservation: %v %s %v", err, r.op, r.params)
	}

	c, _ = newClient(t, "ok_null.json")
	if _, err := c.GetReservation(ctx, "10.1.20.0", "10.1.20.6"); !IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
	c, _ = newClient(t, "scope_not_found.json")
	if _, err := c.GetReservation(ctx, "10.9.9.0", "10.9.9.6"); !IsNotFound(err) {
		t.Fatalf("missing scope should be not found, got %v", err)
	}
	c, r = newClient(t, "scope_list_empty.json")
	if l, err := c.ListReservations(ctx, "10.1.20.0"); err != nil || len(l) != 0 || r.op != "reservation.list" {
		t.Fatalf("ListReservations: %v %v", l, err)
	}
}

// TestExclusionRange walks get/add/remove for exclusion ranges and the
// null-data "not found" case.
func TestExclusionRange(t *testing.T) {
	ctx := context.Background()
	e := ExclusionRange{ScopeID: "10.1.20.0", StartRange: "10.1.20.1", EndRange: "10.1.20.9"}
	c, r := newClient(t, "exclusion_get.json")
	got, err := c.GetExclusionRange(ctx, e)
	// Go note: *got dereferences the pointer to compare the struct values.
	if err != nil || *got != e || r.op != "exclusion.get" {
		t.Fatalf("GetExclusionRange() = %+v, %v", got, err)
	}
	if _, err := c.AddExclusionRange(ctx, e); err != nil || r.op != "exclusion.add" || r.params["end_range"] != "10.1.20.9" {
		t.Fatalf("AddExclusionRange: %v %v", err, r.params)
	}
	if err := c.RemoveExclusionRange(ctx, e); err != nil || r.op != "exclusion.remove" {
		t.Fatalf("RemoveExclusionRange: %v", err)
	}
	c, _ = newClient(t, "ok_null.json")
	if _, err := c.GetExclusionRange(ctx, e); !IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
}

// TestOptionValue checks a multi-valued option (6, DNS servers) decodes as a
// list, the full key map sent, set/remove ops, and the not found case.
func TestOptionValue(t *testing.T) {
	ctx := context.Background()
	k := OptionKey{OptionID: 6, ScopeID: "10.1.20.0"}
	c, r := newClient(t, "option_get.json")
	got, err := c.GetOptionValue(ctx, k)
	if err != nil || !reflect.DeepEqual(got.Value, []string{"10.0.0.53", "10.0.1.53"}) {
		t.Fatalf("GetOptionValue() = %+v, %v", got, err)
	}
	wantKey := map[string]any{"option_id": int64(6), "scope_id": "10.1.20.0", "reserved_ip": "", "vendor_class": "", "user_class": ""}
	if r.op != "option.get" || !reflect.DeepEqual(r.params, wantKey) {
		t.Fatalf("unexpected call %s %#v", r.op, r.params)
	}
	if _, err := c.SetOptionValue(ctx, k, []string{"10.0.0.53"}); err != nil || r.op != "option.set" || !reflect.DeepEqual(r.params["value"], []string{"10.0.0.53"}) {
		t.Fatalf("SetOptionValue: %v %v", err, r.params)
	}
	if err := c.RemoveOptionValue(ctx, k); err != nil || r.op != "option.remove" {
		t.Fatalf("RemoveOptionValue: %v", err)
	}
	c, _ = newClient(t, "ok_null.json")
	if _, err := c.GetOptionValue(ctx, k); !IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
}

// TestListLeases checks lease decoding (including the UTC expiry string) and
// that the all_leases flag is sent.
func TestListLeases(t *testing.T) {
	c, r := newClient(t, "lease_list.json")
	got, err := c.ListLeases(context.Background(), "10.1.20.0", true)
	if err != nil || len(got) != 1 || got[0].HostName != "pc01.example.local" || got[0].LeaseExpiryTime != "2026-10-09T08:00:00Z" {
		t.Fatalf("ListLeases() = %+v, %v", got, err)
	}
	if r.op != "lease.list" || r.params["scope_id"] != "10.1.20.0" || r.params["all_leases"] != true {
		t.Fatalf("unexpected call %s %v", r.op, r.params)
	}
}
