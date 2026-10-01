// Package recordset is the generic DNS record-set engine shared by every
// record resource (windowsddi_dns_a_record_set, windowsddi_dns_cname_record,
// ...). See docs/DESIGN.md, "recordset engine".
//
// Where it fits:
//
//	resources/dns (Terraform schema, HCL values <-> dns.Record)
//	  -> recordset (this package: read, create, update, delete one record set)
//	    -> dns (one Go function per cmdlet) -> psscript + runner
//
// A record set is every record of one (zone, name, type) tuple, for example
// all A records of "www" in "example.com". The resources own their tuple
// authoritatively: values on the server that are not in the configuration
// are removed. The engine works on dns.Record values whose data fields
// (Address, HostName, Exchange, ...) carry the value; Name and Type come from
// the Tuple and TTL is handled per set.
//
// Values are compared with dns.Record.SameValue, the same comparison the
// remove and set_ttl scripts apply on the server: hostnames ignore case and
// the trailing dot, addresses compare by parsed value, TXT exactly.
//
// Concurrency: Terraform applies resources in parallel. Two resources (or a
// replace, which deletes and creates) touching the same tuple at once could
// interleave their adds and removes, so every operation holds a mutex keyed
// by the tuple for its whole duration.
//
// Nothing here builds PowerShell: every server call goes through the dns
// client.
package recordset

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/normalize"
)

// Tuple identifies one record set. Zone and Name should be canonical
// (normalize.ZoneName, normalize.RecordName with "@" for the apex) and Type
// one of the dns.Type constants.
type Tuple struct {
	Zone string
	Name string
	Type string
}

// ImportID returns the resources' import ID for the tuple, "<zone>/<name>".
func (t Tuple) ImportID() string { return t.Zone + "/" + t.Name }

// String renders the tuple for error messages, for example
// `A records "www" in zone example.com`.
func (t Tuple) String() string {
	return fmt.Sprintf("%s records %q in zone %s", t.Type, t.Name, t.Zone)
}

// key is the lock key: the tuple lowercased, since the server compares zone
// and record names case-insensitively.
func (t Tuple) key() string {
	return strings.ToLower(t.Zone) + "\x00" + strings.ToLower(t.Name) + "\x00" + strings.ToUpper(t.Type)
}

// record returns r with the tuple's name and type filled in and TTL set.
func (t Tuple) record(r dns.Record, ttl int64) dns.Record {
	r.Name, r.Type, r.TTL = t.Name, t.Type, ttl
	return r
}

// locks maps Tuple.key() to the *sync.Mutex serialising operations on that
// tuple within this provider process.
//
// Go note: sync.Map is a map that is safe for concurrent use without an
// extra lock. LoadOrStore returns the existing mutex, or stores the new one
// when the key is not there yet, atomically. Entries are never removed: a
// provider process manages a bounded number of tuples, so the map stays
// small.
var locks sync.Map

// lock acquires the mutex of t and returns the function that releases it,
// used as `defer lock(t)()`.
func lock(t Tuple) func() {
	m, _ := locks.LoadOrStore(t.key(), &sync.Mutex{})
	mu, _ := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// ExistingRecordsError is returned by Create when the tuple already holds
// records the resource would silently take over. Dynamic is true when every
// existing record is dynamic (registered by a client or the DHCP server), in
// which case allow_overwrite_dynamic lets Create replace them.
type ExistingRecordsError struct {
	Tuple   Tuple
	Records []dns.Record
	Dynamic bool
}

func (e *ExistingRecordsError) Error() string {
	vals := make([]string, len(e.Records))
	for i, r := range e.Records {
		vals[i] = r.RData()
	}
	list := strings.Join(vals, ", ")
	if e.Dynamic {
		return fmt.Sprintf("%s already exist as dynamic records (%s), registered by a client or the DHCP server. "+
			"Set allow_overwrite_dynamic = true to replace them with the configured values, or import them with "+
			"`terraform import <resource address> %s`.", e.Tuple, list, e.Tuple.ImportID())
	}
	return fmt.Sprintf("%s already exist on the server (%s). This resource is authoritative for the whole set: "+
		"import it with `terraform import <resource address> %s` (or an import block) instead of creating it.",
		e.Tuple, list, e.Tuple.ImportID())
}

// Read returns the values of the tuple and their TTL. It returns
// dns.ErrNotFound when the zone does not exist or the tuple has no records,
// which callers treat as drift (the resource is gone).
//
// When the records disagree on TTL (someone changed one record out of band),
// Read returns the first TTL that differs from priorTTL, so the next plan
// shows a TTL change and Update sets every record to the configured TTL.
// priorTTL is 0 when there is no prior state (import); the first record's
// TTL is reported then.
func Read(ctx context.Context, c *dns.Client, t Tuple, priorTTL int64) ([]dns.Record, int64, error) {
	defer lock(t)()
	recs, err := c.GetRecords(ctx, t.Zone, t.Name, t.Type)
	if err != nil {
		return nil, 0, err
	}
	if len(recs) == 0 {
		return nil, 0, dns.ErrNotFound
	}
	return recs, pickTTL(recs, priorTTL), nil
}

// pickTTL implements the TTL rule of Read: the common TTL when all records
// agree, otherwise the first one that differs from prior.
func pickTTL(recs []dns.Record, prior int64) int64 {
	ttl := recs[0].TTL
	for _, r := range recs {
		if r.TTL != prior {
			return r.TTL
		}
	}
	return ttl
}

// checkDistinct rejects desired values that are semantically equal to each
// other ("10.0.0.1" twice under different spellings, "Mail.example.com" and
// "mail.example.com."): the server would refuse the second add, and state
// could never match the configuration.
func checkDistinct(t Tuple, values []dns.Record) error {
	for i := range values {
		for j := i + 1; j < len(values); j++ {
			if t.record(values[i], 0).SameValue(t.record(values[j], 0)) {
				return fmt.Errorf("%s: values %q and %q are the same DNS record, list it once",
					t, t.record(values[i], 0).RData(), t.record(values[j], 0).RData())
			}
		}
	}
	return nil
}

// contains reports whether vs holds a value equal to r (SameValue).
func contains(vs []dns.Record, r dns.Record) bool {
	for _, v := range vs {
		if v.SameValue(r) {
			return true
		}
	}
	return false
}

// Create adds values (each with ttl seconds, 0 meaning the zone default) as
// a new record set.
//
// The tuple must be empty first:
//   - static records already present: ExistingRecordsError (the user should
//     import them);
//   - only dynamic records present: ExistingRecordsError with Dynamic set,
//     unless allowOverwriteDynamic is true, in which case they are removed
//     before the values are added.
//
// When an add fails midway, the records added by this call are removed again
// (best effort) so a retry does not trip over them; the error is returned.
func Create(ctx context.Context, c *dns.Client, t Tuple, values []dns.Record, ttl int64, allowOverwriteDynamic bool) error {
	if err := checkDistinct(t, values); err != nil {
		return err
	}
	defer lock(t)()
	existing, err := c.GetRecords(ctx, t.Zone, t.Name, t.Type)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		allDynamic := true
		for _, r := range existing {
			allDynamic = allDynamic && r.Dynamic
		}
		if !allDynamic || !allowOverwriteDynamic {
			return &ExistingRecordsError{Tuple: t, Records: existing, Dynamic: allDynamic}
		}
		for _, r := range existing {
			if err := c.RemoveRecord(ctx, t.Zone, r); err != nil && !dns.IsNotFound(err) {
				return fmt.Errorf("removing dynamic record %s: %w", r.RData(), err)
			}
		}
	}
	var added []dns.Record
	for _, v := range values {
		r := t.record(v, ttl)
		if err := c.AddRecord(ctx, t.Zone, r); err != nil {
			// Roll back what this call added. Errors are ignored: the add
			// error is the one worth reporting.
			for _, a := range added {
				_ = c.RemoveRecord(ctx, t.Zone, a)
			}
			return fmt.Errorf("adding %s record %s: %w", t.Type, r.RData(), err)
		}
		added = append(added, r)
	}
	return nil
}

// Update makes the server hold exactly values for the tuple, all with ttl
// seconds. It diffs against the records currently on the server (not the
// prior state), so values added out of band are removed too.
//
// Order, so the name never resolves empty during the change:
//  1. add the values the server does not have (with ttl);
//  2. set ttl on kept values whose TTL differs (skipped when ttl is 0,
//     meaning "not managed");
//  3. remove the server values that are not wanted.
//
// The sequence is not atomic: a failure midway leaves a mix of old and new
// values, which the next plan shows and corrects.
//
// CNAME is the exception: a name holds at most one CNAME, so the server
// refuses to add the new target while the old one exists. For CNAME the old
// value is removed first, and put back (best effort) if adding the new one
// fails. The name is briefly empty during the change.
func Update(ctx context.Context, c *dns.Client, t Tuple, values []dns.Record, ttl int64) error {
	if err := checkDistinct(t, values); err != nil {
		return err
	}
	defer lock(t)()
	current, err := c.GetRecords(ctx, t.Zone, t.Name, t.Type)
	if err != nil {
		return err
	}
	want := make([]dns.Record, len(values))
	for i, v := range values {
		want[i] = t.record(v, ttl)
	}
	if t.Type == dns.TypeCNAME {
		return replaceExclusive(ctx, c, t, current, want, ttl)
	}
	// 1. Add missing values.
	for _, r := range want {
		if contains(current, r) {
			continue
		}
		if err := c.AddRecord(ctx, t.Zone, r); err != nil {
			return fmt.Errorf("adding %s record %s: %w", t.Type, r.RData(), err)
		}
	}
	// 2. Fix the TTL of kept values.
	for _, cur := range current {
		if ttl > 0 && cur.TTL != ttl && contains(want, cur) {
			if err := c.SetRecordTTL(ctx, t.Zone, cur, ttl); err != nil {
				return fmt.Errorf("setting TTL of %s record %s: %w", t.Type, cur.RData(), err)
			}
		}
	}
	// 3. Remove unwanted values.
	for _, cur := range current {
		if contains(want, cur) {
			continue
		}
		if err := c.RemoveRecord(ctx, t.Zone, cur); err != nil && !dns.IsNotFound(err) {
			return fmt.Errorf("removing %s record %s: %w", t.Type, cur.RData(), err)
		}
	}
	return nil
}

// replaceExclusive is Update for CNAME tuples: remove the unwanted values
// first, then add the missing ones, restoring what was removed if an add
// fails. A kept value only gets its TTL fixed.
func replaceExclusive(ctx context.Context, c *dns.Client, t Tuple, current, want []dns.Record, ttl int64) error {
	var removed []dns.Record
	for _, cur := range current {
		if contains(want, cur) {
			if ttl > 0 && cur.TTL != ttl {
				if err := c.SetRecordTTL(ctx, t.Zone, cur, ttl); err != nil {
					return fmt.Errorf("setting TTL of %s record %s: %w", t.Type, cur.RData(), err)
				}
			}
			continue
		}
		if err := c.RemoveRecord(ctx, t.Zone, cur); err != nil && !dns.IsNotFound(err) {
			return fmt.Errorf("removing %s record %s: %w", t.Type, cur.RData(), err)
		}
		removed = append(removed, cur)
	}
	for _, r := range want {
		if contains(current, r) {
			continue
		}
		if err := c.AddRecord(ctx, t.Zone, r); err != nil {
			for _, old := range removed {
				_ = c.AddRecord(ctx, t.Zone, old) // best effort rollback; the add error is what matters
			}
			return fmt.Errorf("adding %s record %s: %w", t.Type, r.RData(), err)
		}
	}
	return nil
}

// Delete removes every record of the tuple. A missing zone or an already
// empty tuple is success.
func Delete(ctx context.Context, c *dns.Client, t Tuple) error {
	defer lock(t)()
	current, err := c.GetRecords(ctx, t.Zone, t.Name, t.Type)
	if dns.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range current {
		if err := c.RemoveRecord(ctx, t.Zone, r); err != nil && !dns.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("removing %s record %s: %w", t.Type, r.RData(), err))
		}
	}
	return errors.Join(errs...)
}

// Canonical returns r with its data fields in the provider's canonical
// spelling: hostnames (CNAME/PTR target, MX exchange, SRV target) lowercase
// with a trailing dot (normalize.FQDN), IPv6 addresses compressed
// (normalize.CanonicalIPv6). Values that do not parse are left unchanged.
func Canonical(r dns.Record) dns.Record {
	host := func(s string) string {
		if c, err := normalize.FQDN(s); err == nil {
			return c
		}
		return s
	}
	switch strings.ToUpper(r.Type) {
	case dns.TypeAAAA:
		if c, err := normalize.CanonicalIPv6(r.Address); err == nil {
			r.Address = c
		}
	case dns.TypeCNAME, dns.TypePTR:
		r.HostName = host(r.HostName)
	case dns.TypeMX:
		r.Exchange = host(r.Exchange)
	case dns.TypeSRV:
		r.Target = host(r.Target)
	}
	return r
}

// PreserveSpelling implements "spelling preservation" (DESIGN.md, "Record
// sets"): for every server record, it returns the prior value (from state,
// or the plan right after Create and Update) that is semantically equal to
// it, so a user who wrote "Mail.Example.com" while the server stores
// "mail.example.com." never sees a diff. Server records without a prior
// match are returned in canonical form (see Canonical). TTL, Name, Type and
// Dynamic always come from the server record.
func PreserveSpelling(prior, server []dns.Record) []dns.Record {
	out := make([]dns.Record, len(server))
	for i, s := range server {
		out[i] = Canonical(s)
		for _, p := range prior {
			p.Type = s.Type
			if p.SameValue(s) {
				p.Name, p.TTL, p.Dynamic, p.Data = s.Name, s.TTL, s.Dynamic, s.Data
				out[i] = p
				break
			}
		}
	}
	return out
}
