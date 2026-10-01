package dns

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/psscript"
)

// Record types the client can add, remove and update. Record.Type always
// uses these upper-case spellings, whatever casing the server uses.
const (
	TypeA     = "A"
	TypeAAAA  = "AAAA"
	TypeCNAME = "CNAME"
	TypePTR   = "PTR"
	TypeMX    = "MX"
	TypeSRV   = "SRV"
	TypeTXT   = "TXT"
)

// rrTypeParam maps each supported type to the spelling the DnsServer cmdlets
// document for -RRType ("CName", "Ptr", ...). PowerShell matches it
// case-insensitively anyway; using the documented form keeps the scripts
// obviously correct.
var rrTypeParam = map[string]string{
	TypeA: "A", TypeAAAA: "AAAA", TypeCNAME: "CName", TypePTR: "Ptr", TypeMX: "Mx", TypeSRV: "Srv", TypeTXT: "Txt",
}

// IsSupportedType reports whether t (upper case) is a record type this
// client can manage (A, AAAA, CNAME, PTR, MX, SRV, TXT).
func IsSupportedType(t string) bool {
	_, ok := rrTypeParam[t]
	return ok
}

// Record is one DNS resource record, as returned by
// Get-DnsServerResourceRecord and flattened by ConvertTo-WdRecord below, and
// as passed to AddRecord, RemoveRecord and SetRecordTTL.
//
// Common fields:
//   - Name: owner name relative to the zone ("www", "_sip._tcp"), "@" for
//     the zone apex. Reported as the server spells it.
//   - Type: upper case ("A", "CNAME", "SOA", ...).
//   - TTL: seconds. On AddRecord, 0 means "use the zone default".
//   - Dynamic: true when the record has a timestamp, meaning it was
//     registered by a client or the DHCP server (dynamic update) and may be
//     scavenged. Ignored on input.
//
// Type specific data (only the fields for Type are meaningful):
//   - A, AAAA: Address (IPv6 in compressed form as the server reports it).
//   - CNAME: HostName (the alias target, HostNameAlias).
//   - PTR: HostName (PtrDomainName).
//   - MX: Exchange (MailExchange) and Preference.
//   - SRV: Target (DomainName), Priority, Weight and Port.
//   - TXT: Text (DescriptiveText).
//
// Hostnames come back from the server fully qualified with a trailing dot
// ("mail.example.com."). Data is the record data in zone file form ("10
// mail.example.com." for MX). It is filled for every record read from the
// server: computed from the typed fields for the types above, and a best
// effort rendering from the script for other types (SOA, NS, ...). It is
// ignored on input.
type Record struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	TTL        int64  `json:"ttl"`
	Dynamic    bool   `json:"dynamic"`
	Address    string `json:"address"`
	HostName   string `json:"host_name"`
	Exchange   string `json:"exchange"`
	Preference int64  `json:"preference"`
	Target     string `json:"target"`
	Priority   int64  `json:"priority"`
	Weight     int64  `json:"weight"`
	Port       int64  `json:"port"`
	Text       string `json:"text"`
	Data       string `json:"data"`
}

// RData renders the record data in zone file form: the address for A and
// AAAA, the hostname for CNAME and PTR, "<preference> <exchange>" for MX,
// "<priority> <weight> <port> <target>" for SRV, and the quoted text (with
// backslash and double quote escaped) for TXT. For other types it returns
// Data as reported by the server.
//
// Go note: `(r Record)` is a value receiver; RData works on a copy.
func (r Record) RData() string {
	switch r.Type {
	case TypeA, TypeAAAA:
		return r.Address
	case TypeCNAME, TypePTR:
		return r.HostName
	case TypeMX:
		return fmt.Sprintf("%d %s", r.Preference, r.Exchange)
	case TypeSRV:
		return fmt.Sprintf("%d %d %d %s", r.Priority, r.Weight, r.Port, r.Target)
	case TypeTXT:
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(r.Text) + `"`
	}
	return r.Data
}

// SameValue reports whether r and o hold the same record data: same type
// and equal type specific fields. Name, TTL and Dynamic are ignored.
// Hostnames compare case-insensitively with or without the trailing dot,
// addresses by parsed value (so IPv6 spellings do not matter), TXT exactly.
// It is the comparison the RemoveRecord and SetRecordTTL scripts apply on
// the server, exported so the record-set engine and dnsfake match the same
// way.
func (r Record) SameValue(o Record) bool {
	if !strings.EqualFold(r.Type, o.Type) {
		return false
	}
	switch strings.ToUpper(r.Type) {
	case TypeA, TypeAAAA:
		return sameAddr(r.Address, o.Address)
	case TypeCNAME, TypePTR:
		return sameHost(r.HostName, o.HostName)
	case TypeMX:
		return r.Preference == o.Preference && sameHost(r.Exchange, o.Exchange)
	case TypeSRV:
		return r.Priority == o.Priority && r.Weight == o.Weight && r.Port == o.Port && sameHost(r.Target, o.Target)
	case TypeTXT:
		return r.Text == o.Text
	}
	return r.Data == o.Data
}

// sameHost compares two hostnames ignoring case and a trailing dot.
func sameHost(a, b string) bool {
	return strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(b, "."))
}

// sameAddr compares two IP addresses by value, falling back to a case
// insensitive string comparison when either does not parse.
func sameAddr(a, b string) bool {
	x, errA := netip.ParseAddr(a)
	y, errB := netip.ParseAddr(b)
	if errA != nil || errB != nil {
		return strings.EqualFold(a, b)
	}
	return x == y
}

// params builds the `$p.record` object for the add, remove and set_ttl
// scripts. Every field is always present, so the script never meets a
// missing property.
func (r Record) params() map[string]any {
	return map[string]any{
		"name": r.Name, "type": strings.ToUpper(r.Type), "ttl": r.TTL,
		"address": r.Address, "host_name": r.HostName,
		"exchange": r.Exchange, "preference": r.Preference,
		"target": r.Target, "priority": r.Priority, "weight": r.Weight, "port": r.Port,
		"text": r.Text,
	}
}

// recordFunc holds the PowerShell helpers shared by the record scripts.
//
//   - ConvertTo-WdRecord projects a CIM record into a hashtable matching
//     the Record struct tags (DNS script rules in DESIGN.md): RecordData is
//     projected explicitly per type, TTL as whole seconds, dynamic is "has a
//     Timestamp". NS and SOA get a zone-file style data string; other types
//     a best effort join of their RecordData property values.
//   - Test-WdRecord compares a projected record with $p.record the same way
//     Record.SameValue does in Go.
//   - Get-WdRecordSet lists the records of one (zone, name, type) tuple. It
//     first reads the zone so a missing zone fails with WIN32 9601 (mapped
//     to ErrNotFound), then treats WIN32 9714 ("name does not exist") as an
//     empty result. -Node limits the result to the name itself, not its
//     children.
//   - Get-WdRecordMatch keeps the records of the tuple whose data equals
//     $p.record.
const recordFunc = `
function ConvertTo-WdRecord($r) {
    $t = "$($r.RecordType)".ToUpperInvariant()
    $d = $r.RecordData
    $o = [ordered]@{
        name = "$($r.HostName)"; type = $t; ttl = [int64]$r.TimeToLive.TotalSeconds
        dynamic = $null -ne $r.Timestamp
        address = ''; host_name = ''; exchange = ''; preference = 0
        target = ''; priority = 0; weight = 0; port = 0; text = ''; data = ''
    }
    switch ($t) {
        'A'     { $o.address = $d.IPv4Address.IPAddressToString }
        'AAAA'  { $o.address = $d.IPv6Address.IPAddressToString }
        'CNAME' { $o.host_name = "$($d.HostNameAlias)" }
        'PTR'   { $o.host_name = "$($d.PtrDomainName)" }
        'MX'    { $o.exchange = "$($d.MailExchange)"; $o.preference = [int64]$d.Preference }
        'SRV'   {
            $o.target = "$($d.DomainName)"; $o.priority = [int64]$d.Priority
            $o.weight = [int64]$d.Weight; $o.port = [int64]$d.Port
        }
        'TXT'   { $o.text = "$($d.DescriptiveText)" }
        'NS'    { $o.data = "$($d.NameServer)" }
        'SOA'   {
            $o.data = "$($d.PrimaryServer) $($d.ResponsiblePerson) $($d.SerialNumber) " +
                "$([int64]$d.RefreshInterval.TotalSeconds) $([int64]$d.RetryDelay.TotalSeconds) " +
                "$([int64]$d.ExpireLimit.TotalSeconds) $([int64]$d.MinimumTimeToLive.TotalSeconds)"
        }
        default {
            if ($d.CimInstanceProperties) {
                $o.data = @($d.CimInstanceProperties | Where-Object { $_.Name -ne 'PSComputerName' -and $null -ne $_.Value } |
                    ForEach-Object { "$($_.Value)" }) -join ' '
            }
            if (-not $o.data) { $o.data = "$d" }
        }
    }
    $o
}
function Test-WdRecord($o, $q) {
    switch ($o.type) {
        { $_ -in 'A', 'AAAA' } { return "$([ipaddress]$q.address)" -eq "$([ipaddress]$o.address)" }
        { $_ -in 'CNAME', 'PTR' } { return "$($o.host_name)".TrimEnd('.') -eq "$($q.host_name)".TrimEnd('.') }
        'MX' { return $o.preference -eq $q.preference -and "$($o.exchange)".TrimEnd('.') -eq "$($q.exchange)".TrimEnd('.') }
        'SRV' {
            return $o.priority -eq $q.priority -and $o.weight -eq $q.weight -and $o.port -eq $q.port -and
                "$($o.target)".TrimEnd('.') -eq "$($q.target)".TrimEnd('.')
        }
        'TXT' { return $o.text -ceq $q.text }
    }
    $false
}
function Get-WdRecordSet($zone, $name, $type) {
    $null = Get-DnsServerZone @cn -Name $zone
    $a = @{ ZoneName = $zone }
    if ($name) { $a['Name'] = $name; $a['Node'] = $true }
    if ($type) { $a['RRType'] = $type }
    try { @(Get-DnsServerResourceRecord @cn @a) }
    catch { $id = "$($_.FullyQualifiedErrorId)"; if ($id -notlike 'WIN32 9714,*' -and $id -notlike 'WIN32 9701,*') { throw } }
}
function Get-WdRecordMatch {
    @(Get-WdRecordSet $p.zone $p.record.name $p.rr_type | Where-Object { Test-WdRecord (ConvertTo-WdRecord $_) $p.record })
}
`

// The record scripts (script contract: `$p` inputs, `@cn` splat, result in
// `$out`):
//
//   - dns.record.list: Get-WdRecordSet with optional name and type filters,
//     always an array (`@(...)`), empty when nothing matches. Backs both
//     GetRecords and ListRecords.
//   - dns.record.add: the per-type Add cmdlet. A, AAAA, CNAME, PTR and MX
//     have dedicated cmdlets; SRV and TXT go through the generic
//     Add-DnsServerResourceRecord with its -Srv / -Txt parameter sets.
//     -TimeToLive is a TimeSpan built from whole seconds, and only passed
//     when ttl > 0 so the zone default applies otherwise. No data returned.
//   - dns.record.remove: finds the exact record(s) with Get-WdRecordMatch
//     and removes each with Remove-DnsServerResourceRecord -InputObject
//     -Force. Matching in PowerShell and passing the object is unambiguous
//     for every type (removing by -RecordData would need a type specific
//     string format, ambiguous for MX and SRV). Returns the number removed,
//     or $null (no data, ErrNotFound in Go) when nothing matched.
//   - dns.record.set_ttl: for each matching record, clones the CIM object
//     (as the Set-DnsServerResourceRecord docs do), sets TimeToLive on the
//     clone and calls Set-DnsServerResourceRecord -OldInputObject
//     -NewInputObject. Returns the number updated, or $null when nothing
//     matched.
var (
	scriptRecordList = psscript.Script{Op: "dns.record.list", Body: recordFunc + `
$out = @(Get-WdRecordSet $p.zone $p.name $p.rr_type | ForEach-Object { ConvertTo-WdRecord $_ })
`}
	scriptRecordAdd = psscript.Script{Op: "dns.record.add", Body: `
$rr = $p.record
$a = @{ ZoneName = $p.zone; Name = $rr.name }
if ($rr.ttl -gt 0) { $a['TimeToLive'] = [TimeSpan]::FromSeconds($rr.ttl) }
switch ($rr.type) {
    'A'     { Add-DnsServerResourceRecordA @cn @a -IPv4Address $rr.address }
    'AAAA'  { Add-DnsServerResourceRecordAAAA @cn @a -IPv6Address $rr.address }
    'CNAME' { Add-DnsServerResourceRecordCName @cn @a -HostNameAlias $rr.host_name }
    'PTR'   { Add-DnsServerResourceRecordPtr @cn @a -PtrDomainName $rr.host_name }
    'MX'    { Add-DnsServerResourceRecordMX @cn @a -MailExchange $rr.exchange -Preference $rr.preference }
    'SRV'   { Add-DnsServerResourceRecord @cn @a -Srv -DomainName $rr.target -Priority $rr.priority -Weight $rr.weight -Port $rr.port }
    'TXT'   { Add-DnsServerResourceRecord @cn @a -Txt -DescriptiveText $rr.text }
    default { throw 'unsupported record type' }
}
`}
	scriptRecordRemove = psscript.Script{Op: "dns.record.remove", Body: recordFunc + `
$m = @(Get-WdRecordMatch)
foreach ($r in $m) { Remove-DnsServerResourceRecord @cn -ZoneName $p.zone -InputObject $r -Force }
if ($m.Count -gt 0) { $out = $m.Count }
`}
	scriptRecordSetTTL = psscript.Script{Op: "dns.record.set_ttl", Body: recordFunc + `
$m = @(Get-WdRecordMatch)
foreach ($old in $m) {
    $new = $old.Clone()
    $new.TimeToLive = [TimeSpan]::FromSeconds($p.ttl)
    Set-DnsServerResourceRecord @cn -ZoneName $p.zone -OldInputObject $old -NewInputObject $new
}
if ($m.Count -gt 0) { $out = $m.Count }
`}
)

// checkRecord validates a record before it is sent: supported type, valid
// TTL, and the data field(s) for its type present.
func checkRecord(r Record) error {
	t := strings.ToUpper(r.Type)
	if !IsSupportedType(t) {
		return fmt.Errorf("unsupported DNS record type %q", r.Type)
	}
	if r.Name == "" {
		return fmt.Errorf("%s record: name is empty (use \"@\" for the zone apex)", t)
	}
	if r.TTL < 0 {
		return fmt.Errorf("%s record %s: negative TTL", t, r.Name)
	}
	var missing bool
	switch t {
	case TypeA, TypeAAAA:
		missing = r.Address == ""
	case TypeCNAME, TypePTR:
		missing = r.HostName == ""
	case TypeMX:
		missing = r.Exchange == ""
	case TypeSRV:
		missing = r.Target == ""
	}
	if missing {
		return fmt.Errorf("%s record %s: record data is empty", t, r.Name)
	}
	return nil
}

// fillData sets Data on records of the supported types from their typed
// fields (RData), so every record read from the server carries it.
func fillData(rs []Record) []Record {
	if rs == nil {
		return []Record{}
	}
	for i := range rs {
		if IsSupportedType(rs[i].Type) {
			rs[i].Data = rs[i].RData()
		}
	}
	return rs
}

// GetRecords returns every record of one (zone, name, type) tuple, for
// example all A records of "www" in "example.com". name is relative to the
// zone ("@" for the apex) and rrType is one of the Type constants. A name
// without records of that type gives an empty slice, not an error; a missing
// zone gives ErrNotFound.
func (c *Client) GetRecords(ctx context.Context, zone, name, rrType string) ([]Record, error) {
	t := strings.ToUpper(rrType)
	if !IsSupportedType(t) {
		return nil, fmt.Errorf("unsupported DNS record type %q", rrType)
	}
	if name == "" {
		return nil, fmt.Errorf("GetRecords: name is empty (use \"@\" for the zone apex)")
	}
	return c.listRecords(ctx, zone, name, rrTypeParam[t])
}

// ListRecords returns the records of a zone for the records data source.
// name ("" for the whole zone) limits the result to one owner name, without
// its children; rrType ("" for all types) to one type, which may be any type
// Get-DnsServerResourceRecord -RRType accepts (for example "NS"). Records of
// types this client does not model are returned with Type and Data only. A
// missing zone gives ErrNotFound.
func (c *Client) ListRecords(ctx context.Context, zone, name, rrType string) ([]Record, error) {
	t := rrType
	if p, ok := rrTypeParam[strings.ToUpper(rrType)]; ok {
		t = p
	}
	return c.listRecords(ctx, zone, name, t)
}

// listRecords runs dns.record.list; rrType is already in -RRType spelling.
func (c *Client) listRecords(ctx context.Context, zone, name, rrType string) ([]Record, error) {
	var out []Record
	params := map[string]any{"zone": zone, "name": name, "rr_type": rrType}
	if _, err := c.run(ctx, scriptRecordList, params, &out); err != nil {
		return nil, err
	}
	return fillData(out), nil
}

// AddRecord adds one record with the Add cmdlet for its type. r.TTL == 0
// leaves the TTL to the zone default. Adding a record identical to an
// existing one fails on the server (WIN32 9711, "already exists"), as does a
// CNAME next to other data. A missing zone gives ErrNotFound.
func (c *Client) AddRecord(ctx context.Context, zone string, r Record) error {
	if err := checkRecord(r); err != nil {
		return err
	}
	_, err := c.run(ctx, scriptRecordAdd, map[string]any{"zone": zone, "record": r.params()}, nil)
	return err
}

// RemoveRecord removes the record(s) of r's (name, type) whose data equals
// r's (see Record.SameValue; TTL is ignored). It returns ErrNotFound when no
// record matched or the zone does not exist, which callers deleting a record
// can treat as success.
func (c *Client) RemoveRecord(ctx context.Context, zone string, r Record) error {
	if err := checkRecord(r); err != nil {
		return err
	}
	params := map[string]any{"zone": zone, "record": r.params(), "rr_type": rrTypeParam[strings.ToUpper(r.Type)]}
	return c.get(ctx, scriptRecordRemove, params, nil)
}

// SetRecordTTL changes the TTL of the existing record(s) matching r (by name,
// type and data, see Record.SameValue) to ttl seconds, using
// Set-DnsServerResourceRecord. ttl must be positive. Returns ErrNotFound when
// no record matched or the zone does not exist.
func (c *Client) SetRecordTTL(ctx context.Context, zone string, r Record, ttl int64) error {
	if err := checkRecord(r); err != nil {
		return err
	}
	if ttl <= 0 {
		return fmt.Errorf("SetRecordTTL: TTL must be positive, got %d", ttl)
	}
	params := map[string]any{"zone": zone, "record": r.params(), "rr_type": rrTypeParam[strings.ToUpper(r.Type)], "ttl": ttl}
	return c.get(ctx, scriptRecordSetTTL, params, nil)
}
