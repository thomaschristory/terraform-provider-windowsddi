package dhcp

import (
	"context"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/psscript"
)

// Lease is an IPv4 lease as returned by Get-DhcpServerv4Lease. Leases are
// read-only in this provider (they back a data source, not a resource).
// LeaseExpiryTime is RFC 3339 in UTC (e.g. "2026-01-31T12:00:00Z"), or empty
// for reservations that have no active lease.
//
// Go note: the `json:"..."` struct tags map PowerShell keys onto fields; a
// trailing `//` comment on a field documents just that field.
type Lease struct {
	IPAddress       string `json:"ip_address"`
	ScopeID         string `json:"scope_id"`
	ClientID        string `json:"client_id"`
	HostName        string `json:"host_name"`
	AddressState    string `json:"address_state"`
	LeaseExpiryTime string `json:"lease_expiry_time"` // RFC 3339, UTC; empty for reservations without a lease
	Description     string `json:"description"`
	ClientType      string `json:"client_type"`
}

// scriptLeaseList (op lease.list) runs Get-DhcpServerv4Lease for
// $p.scope_id, adding -AllLeases when $p.all_leases is true so offered,
// declined and expired leases are included too. Each lease is flattened to
// an ordered hashtable of strings; the expiry DateTime is converted to UTC
// and formatted as an RFC 3339 string so Go can parse it without time zone
// guesswork. `@(...)` keeps `$out` an array even for zero or one lease. A
// missing scope makes the cmdlet throw, which psscript maps to ErrNotFound.
// It follows the script contract: `$p` inputs, `@cn` splat, result in `$out`.
//
// Go note: the backtick string is a raw string literal, sent to PowerShell
// byte for byte with no escape processing.
var scriptLeaseList = psscript.Script{Op: "lease.list", Body: `
$a = @{ ScopeId = $p.scope_id }
if ($p.all_leases) { $a['AllLeases'] = $true }
$out = @(Get-DhcpServerv4Lease @cn @a | ForEach-Object {
    $exp = ''
    if ($_.LeaseExpiryTime) { $exp = $_.LeaseExpiryTime.ToUniversalTime().ToString("yyyy-MM-dd'T'HH:mm:ss'Z'") }
    [ordered]@{
        ip_address        = "$($_.IPAddress)"
        scope_id          = "$($_.ScopeId)"
        client_id         = "$($_.ClientId)"
        host_name         = "$($_.HostName)"
        address_state     = "$($_.AddressState)"
        lease_expiry_time = $exp
        description       = "$($_.Description)"
        client_type       = "$($_.ClientType)"
    }
})
`}

// ListLeases returns the active leases in scopeID, or with all every lease
// (offered, declined, expired too).
//
// Go note: two return values, a slice ([]Lease, a list) and an error. `_`
// discards run's "data present" flag because an empty list is a valid answer.
// `if err != nil { return nil, err }` is the usual "pass the error up" check.
func (c *Client) ListLeases(ctx context.Context, scopeID string, all bool) ([]Lease, error) {
	var out []Lease
	if _, err := c.run(ctx, scriptLeaseList, map[string]any{"scope_id": scopeID, "all_leases": all}, &out); err != nil {
		return nil, err
	}
	return out, nil
}
