// Package dnsfake provides an in-memory DNS server that implements
// runner.Runner by dispatching on the script op marker, like dhcpfake does
// for DHCP. It mimics the envelopes and errors of the real dns scripts
// closely enough to drive the DNS resources and data sources end to end in
// unit tests.
//
// Where it fits: in production the chain is resources -> dns.Client (or the
// recordset engine) -> runner (SSH/WinRM) -> Windows. In unit tests the
// runner is a *Server from this package. The dns.Client still builds its
// real scripts, but the fake reads the "# windowsddi:<op>" marker on the
// first line (for example "dns.record.add"), applies the operation to Go
// maps, and returns the JSON envelope the real script would print.
//
// What it models, as the real server does:
//   - zones: primary zones (file-backed or AD-integrated, forward by name or
//     reverse by network ID with the server's name derivation), conditional
//     forwarders, case-insensitive zone names, default dynamic update mode
//     (Secure for AD-integrated, None for files) and the refusal of Secure
//     updates on file-backed zones;
//   - apex SOA and NS records created with every primary zone;
//   - records per (zone, name, type), case-insensitive names, default TTL
//     3600 when none is given, hostnames stored fully qualified with a
//     trailing dot, IPv6 addresses in compressed form;
//   - errors with the real categories and Win32 codes: zone missing
//     (WIN32 9601, ObjectNotFound), zone exists (9609), identical record
//     exists (9711, ResourceExists), CNAME next to other data (9708, 9709).
//
// Test helpers: SeedZone and SeedRecord add state without recording a call
// (SeedRecord with Dynamic: true creates a dynamically registered record),
// RecordsAt returns the records of a tuple, Lock gives direct access for
// drift simulation, Fail injects failures, and Calls / Ops show what ran.
//
// Files: fake.go only.
package dnsfake

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/normalize"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/psscript"
)

// DefaultTTL is the TTL (seconds) given to records added without one, the
// default minimum TTL of a new Windows DNS zone.
const DefaultTTL int64 = 3600

// Call records one script execution: the op name and the decoded `$p`
// parameters.
type Call struct {
	Op     string
	Params map[string]any
}

// Server is a fake DNS server. Use New. Its exported fields are the server
// state; tests may read them, or change them through Lock to simulate drift.
//
// Zones and Records are keyed by lowercase zone name; Zone.Name keeps the
// spelling the zone was created with. Records of a zone are kept in
// insertion order.
type Server struct {
	mu      sync.Mutex
	Zones   map[string]dns.Zone
	Records map[string][]dns.Record
	Calls   []Call
	// Fail, when set, is consulted before every op; a non-nil error is
	// returned to the caller as a script error.
	Fail func(op string, params map[string]any) *psscript.Error
}

// New returns an empty server.
func New() *Server {
	return &Server{Zones: map[string]dns.Zone{}, Records: map[string][]dns.Record{}}
}

// Ops returns the op names called so far, in order.
func (s *Server) Ops() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.Calls))
	for i, c := range s.Calls {
		out[i] = c.Op
	}
	return out
}

// Lock runs fn with the server locked, for tests that read or mutate state
// directly (for example deleting a record to simulate out-of-band drift).
func (s *Server) Lock(fn func(s *Server)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}

// SeedZone adds a zone directly, bypassing Run (no Call recorded). A Primary
// zone gets its apex SOA and NS records, like one created through Run.
// MasterServers is normalised to a non-nil slice.
func (s *Server) SeedZone(z dns.Zone) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putZone(z)
}

// SeedRecord adds a record directly, bypassing Run and every server check
// (no Call recorded). Set r.Dynamic to true to simulate a record registered
// by a client or the DHCP server. A TTL of 0 becomes DefaultTTL. Hostnames
// and addresses are stored as given, so tests control the exact spelling.
// It panics when the zone does not exist, which is a bug in the test.
func (s *Server) SeedRecord(zone string, r dns.Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := strings.ToLower(zone)
	if _, ok := s.Zones[k]; !ok {
		panic("dnsfake.SeedRecord: zone " + zone + " does not exist; seed it first")
	}
	if r.TTL == 0 {
		r.TTL = DefaultTTL
	}
	r.Type = strings.ToUpper(r.Type)
	s.Records[k] = append(s.Records[k], r)
}

// RecordsAt returns a copy of the records of one (zone, name, type) tuple,
// matched case-insensitively, for assertions. An empty rrType matches every
// type.
func (s *Server) RecordsAt(zone, name, rrType string) []dns.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []dns.Record
	for _, r := range s.Records[strings.ToLower(zone)] {
		if strings.EqualFold(r.Name, name) && (rrType == "" || strings.EqualFold(r.Type, rrType)) {
			out = append(out, r)
		}
	}
	return out
}

// Run implements runner.Runner: it reads the op marker and applies the
// operation to the in-memory state.
func (s *Server) Run(_ context.Context, script string, params any) ([]byte, error) {
	op := psscript.Op(script)
	if op == "" {
		return nil, fmt.Errorf("dnsfake: script without op marker")
	}
	// Round-trip params through JSON so the fake sees what PowerShell would.
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	p := map[string]any{}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}

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

// envelope builds the {ok, data} or {ok:false, error} JSON real scripts print.
func envelope(data any, e *psscript.Error) []byte {
	var out []byte
	if e != nil {
		out, _ = json.Marshal(map[string]any{"ok": false, "error": e})
	} else {
		out, _ = json.Marshal(map[string]any{"ok": true, "data": data})
	}
	return out
}

// str reads a string parameter ("" when missing).
func str(p map[string]any, k string) string {
	v, _ := p[k].(string)
	return v
}

// num reads a numeric parameter (JSON numbers decode as float64).
func num(p map[string]any, k string) int64 {
	v, _ := p[k].(float64)
	return int64(v)
}

// winErr builds a cmdlet error the way the DnsServer cmdlets report it: id
// "WIN32 <code>,<cmdlet>" and a PowerShell error category.
func winErr(cmd string, code int, category, msg string) *psscript.Error {
	return &psscript.Error{Message: msg, Category: category, ID: fmt.Sprintf("WIN32 %d,%s", code, cmd), Command: cmd}
}

// zoneMissing is the WIN32 9601 error every cmdlet gives for an unknown zone.
func zoneMissing(cmd, name string) *psscript.Error {
	return winErr(cmd, 9601, "ObjectNotFound", "The zone "+name+" was not found on server FAKE.")
}

// putZone stores a zone and, for primary zones, creates the apex SOA and NS
// records. Must be called with s.mu held.
func (s *Server) putZone(z dns.Zone) {
	if z.MasterServers == nil {
		z.MasterServers = []string{}
	}
	k := strings.ToLower(z.Name)
	s.Zones[k] = z
	if z.ZoneType == dns.ZoneTypePrimary {
		s.Records[k] = []dns.Record{
			{Name: "@", Type: "SOA", TTL: DefaultTTL, Data: "fake. hostmaster. 1 900 600 86400 3600"},
			{Name: "@", Type: "NS", TTL: DefaultTTL, Data: "fake."},
		}
	}
}

// isReverse reports whether a zone name is a reverse lookup zone.
func isReverse(name string) bool {
	n := strings.ToLower(name)
	return strings.HasSuffix(n, ".in-addr.arpa") || strings.HasSuffix(n, ".ip6.arpa")
}

// addCmdlet is the cmdlet the add script uses for each record type, so
// errors name the same cmdlet as on a real server.
var addCmdlet = map[string]string{
	dns.TypeA: "Add-DnsServerResourceRecordA", dns.TypeAAAA: "Add-DnsServerResourceRecordAAAA",
	dns.TypeCNAME: "Add-DnsServerResourceRecordCName", dns.TypePTR: "Add-DnsServerResourceRecordPtr",
	dns.TypeMX: "Add-DnsServerResourceRecordMX", dns.TypeSRV: "Add-DnsServerResourceRecord", dns.TypeTXT: "Add-DnsServerResourceRecord",
}

// dispatch applies one operation. (nil, nil) means success with no data.
// Must be called with s.mu held.
func (s *Server) dispatch(op string, p map[string]any) (any, *psscript.Error) {
	switch op {
	case "dns.available":
		return true, nil
	case "dns.zone.get", "dns.zone.list", "dns.zone.add_primary", "dns.zone.set_primary", "dns.zone.remove", "dns.zone.has_records":
		return s.zone(op, p)
	case "dns.forwarder.add", "dns.forwarder.set":
		return s.forwarder(op, p)
	case "dns.record.list":
		return s.recordList(p)
	case "dns.record.add":
		return s.recordAdd(p)
	case "dns.record.remove", "dns.record.set_ttl":
		return s.recordChange(op, p)
	}
	return nil, &psscript.Error{Message: "dnsfake: unknown op " + op, Category: "NotImplemented"}
}

// zone handles the zone ops.
func (s *Server) zone(op string, p map[string]any) (any, *psscript.Error) {
	name := str(p, "name")
	k := strings.ToLower(name)
	switch op {
	case "dns.zone.get":
		z, ok := s.Zones[k]
		if !ok {
			return nil, zoneMissing("Get-DnsServerZone", name)
		}
		return z, nil

	case "dns.zone.list":
		out := make([]dns.Zone, 0, len(s.Zones))
		for _, z := range s.Zones {
			out = append(out, z)
		}
		sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
		return out, nil

	case "dns.zone.add_primary":
		const cmd = "Add-DnsServerPrimaryZone"
		if nid := str(p, "network_id"); nid != "" {
			// The server derives the reverse zone name from the network.
			n, err := normalize.ReverseZoneName(nid)
			if err != nil {
				return nil, winErr(cmd, 87, "InvalidArgument", "Invalid network ID "+nid+": "+err.Error())
			}
			name, k = n, n
		}
		if _, ok := s.Zones[k]; ok {
			return nil, winErr(cmd, 9609, "ResourceExists", "Failed to create zone "+name+" on server FAKE. The zone already exists.")
		}
		scope := str(p, "replication_scope")
		du := str(p, "dynamic_update")
		if du == "" {
			du = "None"
			if scope != "" {
				du = "Secure"
			}
		}
		if du == "Secure" && scope == "" {
			return nil, winErr(cmd, 9611, "InvalidArgument", "Secure dynamic updates are only supported for Active Directory-integrated zones.")
		}
		z := dns.Zone{Name: name, ZoneType: dns.ZoneTypePrimary, IsDsIntegrated: scope != "", IsReverseLookupZone: isReverse(name),
			ReplicationScope: scope, DynamicUpdate: du}
		if scope == "" {
			z.ZoneFile = str(p, "zone_file")
		}
		s.putZone(z)
		return s.Zones[k], nil

	case "dns.zone.set_primary":
		z, ok := s.Zones[k]
		if !ok {
			return nil, zoneMissing("Get-DnsServerZone", name)
		}
		const cmd = "Set-DnsServerPrimaryZone"
		if z.ZoneType != dns.ZoneTypePrimary {
			return nil, winErr(cmd, 9611, "InvalidOperation", "Zone "+name+" is not a primary zone.")
		}
		if sc := str(p, "replication_scope"); sc != "" {
			// Setting a replication scope makes the zone AD-integrated.
			z.ReplicationScope, z.IsDsIntegrated, z.ZoneFile = sc, true, ""
		}
		if du := str(p, "dynamic_update"); du != "" {
			if du == "Secure" && !z.IsDsIntegrated {
				return nil, winErr(cmd, 9611, "InvalidArgument", "Secure dynamic updates are only supported for Active Directory-integrated zones.")
			}
			z.DynamicUpdate = du
		}
		s.Zones[k] = z
		return z, nil

	case "dns.zone.remove":
		if _, ok := s.Zones[k]; !ok {
			return nil, zoneMissing("Remove-DnsServerZone", name)
		}
		delete(s.Zones, k)
		delete(s.Records, k)
		return nil, nil

	default: // dns.zone.has_records
		if _, ok := s.Zones[k]; !ok {
			return nil, zoneMissing("Get-DnsServerZone", name)
		}
		for _, r := range s.Records[k] {
			if r.Name != "@" || (r.Type != "SOA" && r.Type != "NS") {
				return true, nil
			}
		}
		return false, nil
	}
}

// forwarder handles dns.forwarder.add and dns.forwarder.set.
func (s *Server) forwarder(op string, p map[string]any) (any, *psscript.Error) {
	name := str(p, "name")
	k := strings.ToLower(name)
	cmd := "Add-DnsServerConditionalForwarderZone"
	if op == "dns.forwarder.set" {
		cmd = "Set-DnsServerConditionalForwarderZone"
	}
	// Master servers come back as the server formats them (canonical IPs).
	var masters []string
	if raw, ok := p["master_servers"].([]any); ok {
		for _, x := range raw {
			ip, err := normalize.CanonicalIP(fmt.Sprint(x))
			if err != nil {
				return nil, winErr(cmd, 87, "InvalidArgument", err.Error())
			}
			masters = append(masters, ip)
		}
	}
	if len(masters) == 0 {
		return nil, winErr(cmd, 87, "InvalidArgument", "At least one master server is required.")
	}
	scope := str(p, "replication_scope")
	timeout := num(p, "forwarder_timeout")

	if op == "dns.forwarder.add" {
		if _, ok := s.Zones[k]; ok {
			return nil, winErr(cmd, 9609, "ResourceExists", "Failed to create zone "+name+" on server FAKE. The zone already exists.")
		}
		if timeout == 0 {
			timeout = 5
		}
		z := dns.Zone{Name: name, ZoneType: dns.ZoneTypeForwarder, IsDsIntegrated: scope != "", ReplicationScope: scope,
			MasterServers: masters, ForwarderTimeout: timeout}
		s.putZone(z)
		return s.Zones[k], nil
	}
	z, ok := s.Zones[k]
	if !ok {
		return nil, zoneMissing(cmd, name)
	}
	if z.ZoneType != dns.ZoneTypeForwarder {
		return nil, winErr(cmd, 9611, "InvalidOperation", "Zone "+name+" is not a conditional forwarder.")
	}
	z.MasterServers = masters
	if timeout > 0 {
		z.ForwarderTimeout = timeout
	}
	if scope != "" {
		z.ReplicationScope, z.IsDsIntegrated = scope, true
	}
	s.Zones[k] = z
	return z, nil
}

// recordList handles dns.record.list: a missing zone is an error, a name
// without records an empty list. Name "" lists the whole zone; type "" all
// types. Records are returned sorted by name (stable, so values keep their
// insertion order).
func (s *Server) recordList(p map[string]any) (any, *psscript.Error) {
	zone := str(p, "zone")
	k := strings.ToLower(zone)
	if _, ok := s.Zones[k]; !ok {
		return nil, zoneMissing("Get-DnsServerZone", zone)
	}
	name, typ := str(p, "name"), str(p, "rr_type")
	out := []dns.Record{}
	for _, r := range s.Records[k] {
		if (name == "" || strings.EqualFold(r.Name, name)) && (typ == "" || strings.EqualFold(r.Type, typ)) {
			if dns.IsSupportedType(r.Type) {
				r.Data = "" // the script only fills data for other types
			}
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// decodeRecord reads `$p.record` back into a dns.Record.
func decodeRecord(p map[string]any) dns.Record {
	var r dns.Record
	b, _ := json.Marshal(p["record"])
	_ = json.Unmarshal(b, &r)
	r.Type = strings.ToUpper(r.Type)
	return r
}

// recordAdd handles dns.record.add with the server's checks and storage
// conventions.
func (s *Server) recordAdd(p map[string]any) (any, *psscript.Error) {
	zone := str(p, "zone")
	k := strings.ToLower(zone)
	r := decodeRecord(p)
	cmd := addCmdlet[r.Type]
	if cmd == "" {
		return nil, &psscript.Error{Message: "unsupported record type", Category: "OperationStopped"}
	}
	if _, ok := s.Zones[k]; !ok {
		return nil, zoneMissing(cmd, zone)
	}
	// Store data the way the server reports it.
	var err error
	switch r.Type {
	case dns.TypeA:
		_, err = normalize.ParseIPv4(r.Address)
	case dns.TypeAAAA:
		r.Address, err = normalize.CanonicalIPv6(r.Address)
	case dns.TypeCNAME, dns.TypePTR:
		r.HostName = absolute(r.HostName)
	case dns.TypeMX:
		r.Exchange = absolute(r.Exchange)
	case dns.TypeSRV:
		r.Target = absolute(r.Target)
	}
	if err != nil {
		return nil, &psscript.Error{Message: "Cannot process argument transformation: " + err.Error(), Category: "InvalidData",
			ID: "ParameterArgumentTransformationError," + cmd, Command: cmd}
	}
	failed := "Failed to create resource record " + r.Name + " in zone " + zone + " on server FAKE."
	for _, o := range s.Records[k] {
		if !strings.EqualFold(o.Name, r.Name) {
			continue
		}
		if r.Type == dns.TypeCNAME && o.Type != dns.TypeCNAME {
			return nil, winErr(cmd, 9709, "InvalidOperation", failed+" A CNAME record cannot be added where other data exists.")
		}
		if r.Type == dns.TypeCNAME && o.Type == dns.TypeCNAME && !o.SameValue(r) {
			return nil, winErr(cmd, 9709, "InvalidOperation", failed+" A CNAME record already exists for this name.")
		}
		if r.Type != dns.TypeCNAME && o.Type == dns.TypeCNAME {
			return nil, winErr(cmd, 9708, "InvalidOperation", failed+" The node is a CNAME DNS record.")
		}
		if o.SameValue(r) {
			return nil, winErr(cmd, 9711, "ResourceExists", failed+" The resource record already exists.")
		}
	}
	if r.TTL == 0 {
		r.TTL = DefaultTTL
	}
	r.Dynamic, r.Data = false, ""
	s.Records[k] = append(s.Records[k], r)
	return nil, nil
}

// absolute appends the trailing dot the server adds to stored hostnames.
func absolute(h string) string {
	if h == "" || strings.HasSuffix(h, ".") {
		return h
	}
	return h + "."
}

// recordChange handles dns.record.remove and dns.record.set_ttl: find the
// records of the tuple with the same data, then remove them or change their
// TTL. No match returns no data (ErrNotFound in the client), like the
// script.
func (s *Server) recordChange(op string, p map[string]any) (any, *psscript.Error) {
	zone := str(p, "zone")
	k := strings.ToLower(zone)
	if _, ok := s.Zones[k]; !ok {
		return nil, zoneMissing("Get-DnsServerZone", zone)
	}
	want := decodeRecord(p)
	kept := make([]dns.Record, 0, len(s.Records[k]))
	n := 0
	for _, r := range s.Records[k] {
		if strings.EqualFold(r.Name, want.Name) && r.SameValue(want) {
			n++
			if op == "dns.record.remove" {
				continue
			}
			r.TTL = num(p, "ttl")
		}
		kept = append(kept, r)
	}
	if n == 0 {
		return nil, nil
	}
	s.Records[k] = kept
	return n, nil
}
