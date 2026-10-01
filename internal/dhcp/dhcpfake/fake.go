// Package dhcpfake provides an in-memory DHCP server that implements
// runner.Runner by dispatching on the script op marker. It mimics the
// envelopes and errors the real scripts produce closely enough to drive the
// provider end to end in unit tests.
//
// Where it fits: in production the chain is resources -> dhcp.Client ->
// runner (SSH/WinRM) -> Windows. In resource and data source unit tests, the
// runner is replaced by a *Server from this package. The dhcp.Client still
// builds its real scripts, but instead of executing PowerShell the fake reads
// the "# windowsddi:<op>" marker on the first line (e.g. "scope.add"),
// applies the operation to Go maps, and returns the same JSON envelope
// ({ok, data, error}) the real script would print. No PowerShell runs at all.
//
// What it models: scope ID derivation from start range and mask, range and
// duplicate checks, default reservation names, MAC canonicalisation, the
// "Inactive" casing quirk, cascading deletes on scope removal, and the error
// categories and DHCP error codes that drive not-found detection.
// Tests can also inject failures (Server.Fail) and inspect calls (Server.Calls).
//
// Files: fake.go only.
package dhcpfake

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dhcp"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/normalize"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/psscript"
)

// Call records one script execution: the op name and the decoded `$p`
// parameters. Tests use Server.Calls to assert what the provider did.
type Call struct {
	Op     string
	Params map[string]any
}

// Server is a fake DHCP server. The zero value is not usable; use New.
// Its exported fields are the server state; tests may read them, or change
// them through Lock to simulate drift (e.g. delete a scope behind
// Terraform's back).
//
// Go note: sync.Mutex is a lock. Terraform runs resource operations in
// parallel, so every access to the maps below goes through mu.Lock() /
// mu.Unlock() to avoid concurrent writes.
// Go note: `map[string]dhcp.Scope` is a dictionary keyed by string;
// `[]dhcp.ExclusionRange` is a slice (list).
type Server struct {
	mu           sync.Mutex
	Scopes       map[string]dhcp.Scope
	Reservations map[string]dhcp.Reservation // keyed by IP
	Exclusions   []dhcp.ExclusionRange
	Options      map[string]dhcp.OptionValue // keyed by optionKey
	Leases       map[string][]dhcp.Lease     // keyed by scope ID
	Calls        []Call
	// Fail, when set, is consulted before every op; a non-nil error is
	// returned to the caller as a script error.
	// Go note: this field holds a function value (a "func type"), so a
	// test can plug in its own logic, like passing a scriptblock.
	Fail func(op string, params map[string]any) *psscript.Error
}

// New returns an empty server with all maps initialised.
//
// Go note: map[string]dhcp.Scope{} creates an empty, writable map. A map
// field left at its zero value (nil) would panic on write, hence New.
func New() *Server {
	return &Server{
		Scopes:       map[string]dhcp.Scope{},
		Reservations: map[string]dhcp.Reservation{},
		Options:      map[string]dhcp.OptionValue{},
		Leases:       map[string][]dhcp.Lease{},
	}
}

// Ops returns the op names called so far, in order (e.g. ["scope.add",
// "scope.get"]). Handy for asserting the sequence of operations.
//
// Go note: `defer s.mu.Unlock()` runs the unlock when the function returns,
// so the lock is always released (like a finally block).
func (s *Server) Ops() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Go note: make([]string, n) allocates a slice of n empty strings.
	out := make([]string, len(s.Calls))
	// Go note: `for i, c := range list` gives the index and the element.
	for i, c := range s.Calls {
		out[i] = c.Op
	}
	return out
}

// Run implements runner.Runner. Instead of executing the script, it reads
// the op marker and applies the operation to the in-memory state.
//
// Go note: Server never declares "implements runner.Runner". Having a Run
// method with the matching signature is enough (implicit interfaces).
func (s *Server) Run(_ context.Context, script string, params any) ([]byte, error) {
	// Step 1: identify the operation from the "# windowsddi:<op>" line.
	op := psscript.Op(script)
	if op == "" {
		return nil, fmt.Errorf("dhcpfake: script without op marker")
	}
	// Step 2: round-trip params through JSON so the fake sees exactly what
	// PowerShell would: numbers become float64, slices become []any.
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	p := map[string]any{}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}

	// Step 3: record the call, give the test a chance to inject a failure,
	// then apply the op and wrap the result in a JSON envelope.
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls = append(s.Calls, Call{Op: op, Params: p})
	if s.Fail != nil {
		if e := s.Fail(op, p); e != nil {
			return envelope(nil, e), nil
		}
	}
	data, e := s.dispatch(op, p)
	return envelope(data, e), nil
}

// envelope builds the {ok, data} or {ok:false, error} JSON that real scripts
// print, so psscript.Parse treats fake and real output identically.
func envelope(data any, e *psscript.Error) []byte {
	var out []byte
	if e != nil {
		out, _ = json.Marshal(map[string]any{"ok": false, "error": e})
	} else {
		out, _ = json.Marshal(map[string]any{"ok": true, "data": data})
	}
	return out
}

// str reads a string parameter, returning "" when missing or not a string.
//
// Go note: p[k].(string) is a type assertion on an `any` value. The comma-ok
// form `v, _ :=` yields "" instead of panicking when the type is wrong.
func str(p map[string]any, k string) string {
	v, _ := p[k].(string)
	return v
}

// num reads a numeric parameter. JSON numbers decode as float64 in Go, so it
// asserts float64 and converts to int64.
func num(p map[string]any, k string) int64 {
	v, _ := p[k].(float64)
	return int64(v)
}

// notFound builds the error a real cmdlet gives for a missing scope:
// category ObjectNotFound and id "DHCP 20005,<cmdlet>". psscript maps it to
// dhcp.ErrNotFound.
func notFound(cmd, what string) *psscript.Error {
	return &psscript.Error{
		Message:  fmt.Sprintf("Failed to get %s on DHCP server FAKE.", what),
		Category: "ObjectNotFound",
		ID:       "DHCP 20005," + cmd,
		Command:  cmd,
	}
}

// invalid builds a generic "bad input" cmdlet error (InvalidArgument). It is
// never treated as not found.
func invalid(cmd, msg string) *psscript.Error {
	return &psscript.Error{Message: msg, Category: "InvalidArgument", ID: "DHCP 20000," + cmd, Command: cmd}
}

// ip4 converts a dotted IPv4 string to a number so addresses can be compared
// and sorted numerically ("10.0.0.9" < "10.0.0.10"). Invalid input gives 0.
func ip4(s string) uint32 {
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() {
		return 0
	}
	b := a.As4()
	return binary.BigEndian.Uint32(b[:])
}

// inScope reports whether ip belongs to the scope's subnet.
func inScope(sc dhcp.Scope, ip string) bool {
	n, err := normalize.NetworkAddress(ip, sc.SubnetMask)
	return err == nil && n == sc.ScopeID
}

// optionKey builds the map key for an option value from its full identity:
// "id|scope|reserved_ip|vendor_class|user_class". Index 1 is the scope ID,
// which scope removal relies on.
func optionKey(p map[string]any) string {
	return fmt.Sprintf("%d|%s|%s|%s|%s", num(p, "option_id"), str(p, "scope_id"), str(p, "reserved_ip"), str(p, "vendor_class"), str(p, "user_class"))
}

// dispatch applies one operation to the state and returns either the data to
// put in the envelope or a script error. Returning (nil, nil) means success
// with no data, which the client turns into ErrNotFound for single-object
// getters (the same as a real script whose `$out` stayed $null).
// It must be called with s.mu held.
//
// Go note: `switch op { case "a": ... }` is like PowerShell's switch; cases
// do not fall through, and one case can list several values.
func (s *Server) dispatch(op string, p map[string]any) (any, *psscript.Error) {
	switch op {
	case "scope.get":
		// Go note: `v, ok := m[key]` is the comma-ok map lookup; ok is
		// false when the key does not exist.
		sc, ok := s.Scopes[str(p, "scope_id")]
		if !ok {
			return nil, notFound("Get-DhcpServerv4Scope", "scope "+str(p, "scope_id"))
		}
		return sc, nil

	case "scope.list":
		// Collect all scopes, sorted by numeric IP for stable output
		// (Go map iteration order is random).
		out := make([]dhcp.Scope, 0, len(s.Scopes))
		for _, sc := range s.Scopes {
			out = append(out, sc)
		}
		// Go note: the func(i, j int) bool {...} is an inline anonymous
		// function (a closure) telling sort.Slice how to compare items.
		sort.Slice(out, func(i, j int) bool { return ip4(out[i].ScopeID) < ip4(out[j].ScopeID) })
		return out, nil

	case "scope.add":
		// Like the server: the scope ID is start_range AND subnet_mask.
		id, err := normalize.NetworkAddress(str(p, "start_range"), str(p, "subnet_mask"))
		if err != nil {
			return nil, invalid("Add-DhcpServerv4Scope", err.Error())
		}
		if _, ok := s.Scopes[id]; ok {
			return nil, invalid("Add-DhcpServerv4Scope", "Failed to add scope "+id+". The specified subnet already exists on the DHCP server.")
		}
		if e := checkRange("Add-DhcpServerv4Scope", id, p); e != nil {
			return nil, e
		}
		sc := scopeFrom(id, p)
		s.Scopes[id] = sc
		// The real script re-reads the scope by the ID the provider computed.
		if id != str(p, "scope_id") {
			return nil, notFound("Get-DhcpServerv4Scope", "scope "+str(p, "scope_id"))
		}
		return sc, nil

	case "scope.set":
		id := str(p, "scope_id")
		old, ok := s.Scopes[id]
		if !ok {
			return nil, notFound("Set-DhcpServerv4Scope", "scope "+id)
		}
		// Set-DhcpServerv4Scope cannot change the mask: keep the old one.
		p["subnet_mask"] = old.SubnetMask
		if e := checkRange("Set-DhcpServerv4Scope", id, p); e != nil {
			return nil, e
		}
		sc := scopeFrom(id, p)
		s.Scopes[id] = sc
		return sc, nil

	case "scope.remove":
		id := str(p, "scope_id")
		if _, ok := s.Scopes[id]; !ok {
			return nil, notFound("Remove-DhcpServerv4Scope", "scope "+id)
		}
		// Refuse to remove a scope with leases unless forced (DHCP 20007).
		if len(s.Leases[id]) > 0 && p["force"] != true {
			return nil, &psscript.Error{
				Message:  "Failed to delete scope " + id + ". The specified DHCP element has been used by a client and cannot be removed.",
				Category: "PermissionDenied", ID: "DHCP 20007,Remove-DhcpServerv4Scope", Command: "Remove-DhcpServerv4Scope",
			}
		}
		// Cascade: removing a scope also removes its leases, reservations,
		// exclusion ranges and scope-level options, as on a real server.
		delete(s.Scopes, id)
		delete(s.Leases, id)
		for ip, r := range s.Reservations {
			if r.ScopeID == id {
				delete(s.Reservations, ip)
			}
		}
		// In-place filter: reuse the slice's storage, keep other scopes'
		// ranges. Go note: s[:0] is an empty slice sharing s's memory.
		kept := s.Exclusions[:0]
		for _, e := range s.Exclusions {
			if e.ScopeID != id {
				kept = append(kept, e)
			}
		}
		s.Exclusions = kept
		for k := range s.Options {
			if strings.Split(k, "|")[1] == id {
				delete(s.Options, k)
			}
		}
		return nil, nil

	case "reservation.get":
		// Missing scope is an error (like the cmdlet); missing reservation
		// is "no data" (like the list-and-filter script).
		if _, ok := s.Scopes[str(p, "scope_id")]; !ok {
			return nil, notFound("Get-DhcpServerv4Reservation", "scope "+str(p, "scope_id"))
		}
		r, ok := s.Reservations[str(p, "ip_address")]
		if !ok || r.ScopeID != str(p, "scope_id") {
			return nil, nil
		}
		return r, nil

	case "reservation.list":
		if _, ok := s.Scopes[str(p, "scope_id")]; !ok {
			return nil, notFound("Get-DhcpServerv4Reservation", "scope "+str(p, "scope_id"))
		}
		out := []dhcp.Reservation{}
		for _, r := range s.Reservations {
			if r.ScopeID == str(p, "scope_id") {
				out = append(out, r)
			}
		}
		sort.Slice(out, func(i, j int) bool { return ip4(out[i].IPAddress) < ip4(out[j].IPAddress) })
		return out, nil

	case "reservation.add":
		// Validate scope, IP in subnet, IP free, and MAC format, then store
		// with the MAC in the server's canonical dash form.
		sc, ok := s.Scopes[str(p, "scope_id")]
		if !ok {
			return nil, notFound("Add-DhcpServerv4Reservation", "scope "+str(p, "scope_id"))
		}
		ip := str(p, "ip_address")
		if !inScope(sc, ip) {
			return nil, invalid("Add-DhcpServerv4Reservation", "The specified IP address is not in the scope.")
		}
		if _, ok := s.Reservations[ip]; ok {
			return nil, invalid("Add-DhcpServerv4Reservation", "The specified IP address is currently taken by another client.")
		}
		cid, err := normalize.CanonicalMAC(str(p, "client_id"))
		if err != nil {
			return nil, invalid("Add-DhcpServerv4Reservation", err.Error())
		}
		name := str(p, "name")
		if name == "" {
			name = ip // the real server also derives a default name
		}
		r := dhcp.Reservation{ScopeID: sc.ScopeID, IPAddress: ip, ClientID: cid, Name: name, Description: str(p, "description"), Type: str(p, "type")}
		s.Reservations[ip] = r
		return r, nil

	case "reservation.set":
		// Keyed by IP. Unknown IP gives DHCP 20018 (not a reserved client).
		// An empty name keeps the old one; description is always replaced.
		ip := str(p, "ip_address")
		r, ok := s.Reservations[ip]
		if !ok {
			return nil, &psscript.Error{Message: "Failed to get reservation " + ip, Category: "ObjectNotFound", ID: "DHCP 20018,Set-DhcpServerv4Reservation", Command: "Set-DhcpServerv4Reservation"}
		}
		cid, err := normalize.CanonicalMAC(str(p, "client_id"))
		if err != nil {
			return nil, invalid("Set-DhcpServerv4Reservation", err.Error())
		}
		r.ClientID = cid
		r.Type = str(p, "type")
		r.Description = str(p, "description")
		if n := str(p, "name"); n != "" {
			r.Name = n
		}
		s.Reservations[ip] = r
		return r, nil

	case "reservation.remove":
		ip := str(p, "ip_address")
		if _, ok := s.Reservations[ip]; !ok {
			return nil, &psscript.Error{Message: "Failed to delete reservation " + ip, Category: "ObjectNotFound", ID: "DHCP 20018,Remove-DhcpServerv4Reservation", Command: "Remove-DhcpServerv4Reservation"}
		}
		delete(s.Reservations, ip)
		return nil, nil

	case "exclusion.get", "exclusion.add", "exclusion.remove":
		return s.exclusion(op, p)

	case "option.get", "option.set", "option.remove":
		return s.option(op, p)

	case "lease.list":
		if _, ok := s.Scopes[str(p, "scope_id")]; !ok {
			return nil, notFound("Get-DhcpServerv4Lease", "scope "+str(p, "scope_id"))
		}
		// Return a copy so callers cannot modify the fake's state.
		// Go note: `append(dst, src...)` appends every element of src.
		out := append([]dhcp.Lease{}, s.Leases[str(p, "scope_id")]...)
		return out, nil
	}
	return nil, &psscript.Error{Message: "dhcpfake: unknown op " + op, Category: "NotImplemented"}
}

// checkRange validates that start and end lie in the scope's subnet and that
// start <= end, returning the server-style error otherwise.
func checkRange(cmd, id string, p map[string]any) *psscript.Error {
	// Go note: `a, b := x, y` assigns two variables at once.
	start, end := str(p, "start_range"), str(p, "end_range")
	mask := str(p, "subnet_mask")
	sc := dhcp.Scope{ScopeID: id, SubnetMask: mask}
	if !inScope(sc, start) || !inScope(sc, end) || ip4(start) > ip4(end) {
		return invalid(cmd, "The specified IP address range either overlaps with an existing range or is invalid.")
	}
	return nil
}

// scopeFrom builds the scope the server would report for these parameters:
// "InActive" comes back as "Inactive", and the lease seconds are formatted
// as d.hh:mm:ss like the real script does.
func scopeFrom(id string, p map[string]any) dhcp.Scope {
	state := str(p, "state")
	if strings.EqualFold(state, "InActive") {
		state = "Inactive" // the server's own casing differs from the parameter's
	}
	return dhcp.Scope{
		ScopeID:       id,
		Name:          str(p, "name"),
		Description:   str(p, "description"),
		StartRange:    str(p, "start_range"),
		EndRange:      str(p, "end_range"),
		SubnetMask:    str(p, "subnet_mask"),
		State:         state,
		Type:          str(p, "type"),
		LeaseDuration: normalize.FormatDuration(time.Duration(num(p, "lease_seconds")) * time.Second),
	}
}

// exclusion handles exclusion.get/add/remove. A range is identified by its
// exact scope, start and end. Get returns no data when absent; Add rejects
// out-of-subnet or duplicate ranges; Remove of an unknown range errors.
func (s *Server) exclusion(op string, p map[string]any) (any, *psscript.Error) {
	id := str(p, "scope_id")
	sc, ok := s.Scopes[id]
	if !ok {
		return nil, notFound("Get-DhcpServerv4ExclusionRange", "scope "+id)
	}
	// Find the index of the exact range, or -1 if absent.
	want := dhcp.ExclusionRange{ScopeID: id, StartRange: str(p, "start_range"), EndRange: str(p, "end_range")}
	idx := -1
	for i, e := range s.Exclusions {
		// Go note: structs made only of comparable fields can be compared
		// with == (all fields equal).
		if e == want {
			idx = i
		}
	}
	switch op {
	case "exclusion.get":
		if idx < 0 {
			return nil, nil
		}
		return want, nil
	case "exclusion.add":
		if !inScope(sc, want.StartRange) || !inScope(sc, want.EndRange) || ip4(want.StartRange) > ip4(want.EndRange) {
			return nil, invalid("Add-DhcpServerv4ExclusionRange", "The specified IP address range either overlaps with an existing range or is invalid.")
		}
		if idx >= 0 {
			return nil, invalid("Add-DhcpServerv4ExclusionRange", "The specified IP address range is already defined on the DHCP server.")
		}
		s.Exclusions = append(s.Exclusions, want)
		return want, nil
	default:
		if idx < 0 {
			return nil, invalid("Remove-DhcpServerv4ExclusionRange", "The specified IP address range either overlaps with an existing range or is invalid.")
		}
		// Remove element idx by joining the parts before and after it.
		s.Exclusions = append(s.Exclusions[:idx], s.Exclusions[idx+1:]...)
		return nil, nil
	}
}

// option handles option.get/set/remove. A missing scope or reservation at
// the requested level errors like the cmdlet (DHCP 20005 / 20018); a missing
// option value is "no data" on get and DHCP 20010 on remove.
func (s *Server) option(op string, p map[string]any) (any, *psscript.Error) {
	// Check that the level (scope or reservation) exists.
	if id := str(p, "scope_id"); id != "" {
		if _, ok := s.Scopes[id]; !ok {
			return nil, notFound("Get-DhcpServerv4OptionValue", "scope "+id)
		}
	}
	if ip := str(p, "reserved_ip"); ip != "" {
		if _, ok := s.Reservations[ip]; !ok {
			return nil, &psscript.Error{Message: "Failed to get reservation " + ip, Category: "ObjectNotFound", ID: "DHCP 20018,Get-DhcpServerv4OptionValue", Command: "Get-DhcpServerv4OptionValue"}
		}
	}
	k := optionKey(p)
	switch op {
	case "option.get":
		v, ok := s.Options[k]
		if !ok {
			return nil, nil
		}
		return v, nil
	case "option.set":
		// Convert each JSON value to a string. JSON arrays decode as []any.
		var vals []string
		if raw, ok := p["value"].([]any); ok {
			for _, x := range raw {
				v := fmt.Sprint(x)
				// Like the real server, report numbers in decimal.
				if strings.HasPrefix(strings.ToLower(v), "0x") {
					if n, err := strconv.ParseUint(v, 0, 64); err == nil {
						v = strconv.FormatUint(n, 10)
					}
				}
				vals = append(vals, v)
			}
		}
		v := dhcp.OptionValue{
			OptionID:    num(p, "option_id"),
			Name:        fmt.Sprintf("Option %d", num(p, "option_id")),
			Value:       vals,
			VendorClass: str(p, "vendor_class"),
			UserClass:   str(p, "user_class"),
		}
		s.Options[k] = v
		return v, nil
	default:
		if _, ok := s.Options[k]; !ok {
			return nil, &psscript.Error{Message: "Failed to delete option value", Category: "ObjectNotFound", ID: "DHCP 20010,Remove-DhcpServerv4OptionValue", Command: "Remove-DhcpServerv4OptionValue"}
		}
		delete(s.Options, k)
		return nil, nil
	}
}

// SetOption seeds an option value directly (for import and drift tests),
// bypassing Run so no Call is recorded.
//
// Go note: `value ...string` is a variadic parameter: callers pass any
// number of strings, and inside the function value is a []string.
func (s *Server) SetOption(k dhcp.OptionKey, value ...string) {
	// option_id is stored as float64 to match what JSON decoding produces
	// in Run, so optionKey builds the same key either way.
	p := map[string]any{
		"option_id": float64(k.OptionID), "scope_id": k.ScopeID, "reserved_ip": k.ReservedIP,
		"vendor_class": k.VendorClass, "user_class": k.UserClass,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Options[optionKey(p)] = dhcp.OptionValue{
		OptionID: k.OptionID, Name: fmt.Sprintf("Option %d", k.OptionID), Value: value,
		VendorClass: k.VendorClass, UserClass: k.UserClass,
	}
}

// Lock runs fn with the server locked, for tests that mutate state directly
// (for example deleting a scope to simulate out-of-band drift).
//
// Go note: fn is a function passed as an argument; callers typically write
// srv.Lock(func(s *dhcpfake.Server) { delete(s.Scopes, "10.0.0.0") }).
func (s *Server) Lock(fn func(s *Server)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}
