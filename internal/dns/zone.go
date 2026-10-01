package dns

import (
	"context"
	"errors"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/normalize"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/psscript"
)

// Zone types as reported in Zone.ZoneType by Get-DnsServerZone. The
// provider manages Primary zones and conditional forwarders (Forwarder);
// the others can still show up when listing zones.
const (
	ZoneTypePrimary   = "Primary"
	ZoneTypeSecondary = "Secondary"
	ZoneTypeStub      = "Stub"
	ZoneTypeForwarder = "Forwarder"
)

// Zone is a DNS zone as returned by Get-DnsServerZone, flattened by
// ConvertTo-WdZone below. It covers both primary zones and conditional
// forwarders; fields that do not apply to a zone type are empty.
//
// Name is the zone name as the server reports it (no trailing dot; casing
// as created). ReplicationScope is "Forest", "Domain", "Legacy" or "Custom"
// for AD-integrated zones and "" otherwise (the server says "None"; the
// script maps that to "" so callers can test IsDsIntegrated or an empty
// string interchangeably). ZoneFile is set for file-backed primary zones.
// DynamicUpdate is "None", "Secure" or "NonsecureAndSecure" for primary
// zones. MasterServers (IP strings, in server order) and ForwarderTimeout
// (seconds) are set for conditional forwarders.
type Zone struct {
	Name                string   `json:"name"`
	ZoneType            string   `json:"zone_type"`
	IsDsIntegrated      bool     `json:"is_ds_integrated"`
	IsReverseLookupZone bool     `json:"is_reverse_lookup_zone"`
	IsAutoCreated       bool     `json:"is_auto_created"`
	ReplicationScope    string   `json:"replication_scope"`
	ZoneFile            string   `json:"zone_file"`
	DynamicUpdate       string   `json:"dynamic_update"`
	MasterServers       []string `json:"master_servers"`
	ForwarderTimeout    int64    `json:"forwarder_timeout"`
}

// PrimaryZoneInput describes a primary zone to create with AddPrimaryZone.
//
// Exactly one of Name (forward zone, or a reverse zone spelled out) and
// NetworkID (CIDR, the server derives the in-addr.arpa / ip6.arpa name) must
// be set. Exactly one of ReplicationScope (AD-integrated: "Forest",
// "Domain", "Legacy") and ZoneFile (file-backed, file name under
// %windir%\System32\dns) must be set. DynamicUpdate is optional; when empty
// the server default applies (Secure for AD-integrated, None for files).
type PrimaryZoneInput struct {
	Name             string
	NetworkID        string
	ReplicationScope string
	ZoneFile         string
	DynamicUpdate    string
}

// PrimaryZoneUpdate holds the in-place changes SetPrimaryZone can make. An
// empty field is left unchanged on the server (partial update).
type PrimaryZoneUpdate struct {
	ReplicationScope string
	DynamicUpdate    string
}

// zoneFunc is the PowerShell helper shared by the zone and forwarder scripts.
// ConvertTo-WdZone turns a CIM zone object into an ordered hashtable whose
// keys match the Zone struct tags. Every string is forced with "$(...)" so
// enums serialise as names. MasterServers elements may be IPAddress objects
// (use IPAddressToString) or plain strings; both are handled.
const zoneFunc = `
function ConvertTo-WdZone($z) {
    $ds = [bool]$z.IsDsIntegrated
    [ordered]@{
        name                   = "$($z.ZoneName)"
        zone_type              = "$($z.ZoneType)"
        is_ds_integrated       = $ds
        is_reverse_lookup_zone = [bool]$z.IsReverseLookupZone
        is_auto_created        = [bool]$z.IsAutoCreated
        replication_scope      = if ($ds) { "$($z.ReplicationScope)" } else { '' }
        zone_file              = "$($z.ZoneFile)"
        dynamic_update         = "$($z.DynamicUpdate)"
        master_servers         = @($z.MasterServers | Where-Object { $_ } | ForEach-Object {
            if ($_.IPAddressToString) { $_.IPAddressToString } else { "$_" } })
        forwarder_timeout      = [int64]$z.ForwarderTimeout
    }
}
`

// The zone scripts (script contract: `$p` inputs, `@cn` splat, result in
// `$out`):
//
//   - dns.zone.get: Get-DnsServerZone -Name. A missing zone throws
//     WIN32 9601 / ObjectNotFound, which psscript maps to ErrNotFound.
//   - dns.zone.list: every zone on the server, always an array. Includes the
//     server's auto-created zones (see Zone.IsAutoCreated).
//   - dns.zone.add_primary: Add-DnsServerPrimaryZone. The four parameter
//     sets of the cmdlet (AD or file, forward by -Name or reverse by
//     -NetworkId) are selected by which keys go into the `$a` splat.
//     -PassThru returns the new zone, whose name is what the server derived
//     from -NetworkId; `$p.lookup_name` (computed in Go) is the fallback.
//     The zone is then read back.
//   - dns.zone.set_primary: Set-DnsServerPrimaryZone. -ReplicationScope
//     lives in its own parameter set (ADZone) and cannot be combined with
//     -DynamicUpdate, so each is a separate call, made only when requested.
//     The replication scope is only set when it differs, so re-applying the
//     current scope never fails.
//   - dns.zone.remove: Remove-DnsServerZone -Force (no prompt). Works for
//     every zone type, conditional forwarders included.
//   - dns.zone.has_records: lists every record in the zone (no -Name: start
//     at the apex; no -Node: include children) and reports whether any is
//     something other than the apex SOA or NS records.
var (
	scriptZoneGet = psscript.Script{Op: "dns.zone.get", Body: zoneFunc + `
$out = ConvertTo-WdZone (Get-DnsServerZone @cn -Name $p.name)
`}
	scriptZoneList = psscript.Script{Op: "dns.zone.list", Body: zoneFunc + `
$out = @(Get-DnsServerZone @cn | ForEach-Object { ConvertTo-WdZone $_ })
`}
	scriptZoneAddPrimary = psscript.Script{Op: "dns.zone.add_primary", Body: zoneFunc + `
$a = @{}
if ($p.network_id) { $a['NetworkId'] = $p.network_id } else { $a['Name'] = $p.name }
if ($p.replication_scope) { $a['ReplicationScope'] = $p.replication_scope } else { $a['ZoneFile'] = $p.zone_file }
if ($p.dynamic_update) { $a['DynamicUpdate'] = $p.dynamic_update }
$z = Add-DnsServerPrimaryZone @cn @a -PassThru
$name = "$(@($z)[0].ZoneName)"
if (-not $name) { $name = $p.lookup_name }
$out = ConvertTo-WdZone (Get-DnsServerZone @cn -Name $name)
`}
	scriptZoneSetPrimary = psscript.Script{Op: "dns.zone.set_primary", Body: zoneFunc + `
$cur = Get-DnsServerZone @cn -Name $p.name
if ($p.replication_scope -and (-not $cur.IsDsIntegrated -or "$($cur.ReplicationScope)" -ne $p.replication_scope)) {
    Set-DnsServerPrimaryZone @cn -Name $p.name -ReplicationScope $p.replication_scope
}
if ($p.dynamic_update) { Set-DnsServerPrimaryZone @cn -Name $p.name -DynamicUpdate $p.dynamic_update }
$out = ConvertTo-WdZone (Get-DnsServerZone @cn -Name $p.name)
`}
	scriptZoneRemove = psscript.Script{Op: "dns.zone.remove", Body: `
Remove-DnsServerZone @cn -Name $p.name -Force
`}
	scriptZoneHasRecords = psscript.Script{Op: "dns.zone.has_records", Body: `
$null = Get-DnsServerZone @cn -Name $p.name
$user = @(Get-DnsServerResourceRecord @cn -ZoneName $p.name | Where-Object {
    -not ("$($_.HostName)" -eq '@' -and @('SOA', 'NS') -contains "$($_.RecordType)".ToUpperInvariant()) })
$out = $user.Count -gt 0
`}
)

// GetZone runs Get-DnsServerZone for one zone (any type: primary,
// conditional forwarder, ...). Returns ErrNotFound when it does not exist.
func (c *Client) GetZone(ctx context.Context, name string) (*Zone, error) {
	var z Zone
	if err := c.get(ctx, scriptZoneGet, map[string]any{"name": name}, &z); err != nil {
		return nil, err
	}
	return &z, nil
}

// ListZones returns every zone on the server, auto-created ones included.
func (c *Client) ListZones(ctx context.Context) ([]Zone, error) {
	var out []Zone
	if _, err := c.run(ctx, scriptZoneList, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// AddPrimaryZone runs Add-DnsServerPrimaryZone and returns the zone as read
// back from the server. For a NetworkID zone, the returned Name is the
// reverse zone name the server derived. The input is checked for the
// "exactly one of" rules before anything is sent.
//
// Go note: errors.New builds a plain error value; returning it early stops
// the function before any PowerShell runs.
func (c *Client) AddPrimaryZone(ctx context.Context, in PrimaryZoneInput) (*Zone, error) {
	if (in.Name == "") == (in.NetworkID == "") {
		return nil, errors.New("AddPrimaryZone: exactly one of Name and NetworkID must be set")
	}
	if (in.ReplicationScope == "") == (in.ZoneFile == "") {
		return nil, errors.New("AddPrimaryZone: exactly one of ReplicationScope and ZoneFile must be set")
	}
	params := map[string]any{
		"name":              in.Name,
		"network_id":        in.NetworkID,
		"replication_scope": in.ReplicationScope,
		"zone_file":         in.ZoneFile,
		"dynamic_update":    in.DynamicUpdate,
		"lookup_name":       in.Name,
	}
	if in.NetworkID != "" {
		// Fallback name for the read-back, should -PassThru return nothing.
		name, err := normalize.ReverseZoneName(in.NetworkID)
		if err != nil {
			return nil, err
		}
		params["lookup_name"] = name
	}
	var z Zone
	if err := c.get(ctx, scriptZoneAddPrimary, params, &z); err != nil {
		return nil, err
	}
	return &z, nil
}

// SetPrimaryZone changes the replication scope and/or dynamic update mode of
// a primary zone with Set-DnsServerPrimaryZone and returns the zone as read
// back. Empty fields of upd are not sent (left unchanged). Note that setting
// a replication scope on a file-backed zone converts it to AD-integrated;
// the zone resource treats that switch as a replacement instead.
func (c *Client) SetPrimaryZone(ctx context.Context, name string, upd PrimaryZoneUpdate) (*Zone, error) {
	var z Zone
	params := map[string]any{"name": name, "replication_scope": upd.ReplicationScope, "dynamic_update": upd.DynamicUpdate}
	if err := c.get(ctx, scriptZoneSetPrimary, params, &z); err != nil {
		return nil, err
	}
	return &z, nil
}

// RemoveZone runs Remove-DnsServerZone -Force. It removes the zone with all
// its records, whatever its type (conditional forwarders included). Returns
// ErrNotFound when the zone does not exist.
func (c *Client) RemoveZone(ctx context.Context, name string) error {
	_, err := c.run(ctx, scriptZoneRemove, map[string]any{"name": name}, nil)
	return err
}

// ZoneHasUserRecords reports whether the zone holds records other than the
// apex SOA and NS records the server creates with the zone. The zone
// resource uses it to refuse deleting a non-empty zone without
// force_destroy. Returns ErrNotFound when the zone does not exist.
func (c *Client) ZoneHasUserRecords(ctx context.Context, name string) (bool, error) {
	var has bool
	if err := c.get(ctx, scriptZoneHasRecords, map[string]any{"name": name}, &has); err != nil {
		return false, err
	}
	return has, nil
}
