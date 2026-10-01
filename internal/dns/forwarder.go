package dns

import (
	"context"
	"errors"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/psscript"
)

// ConditionalForwarderInput describes a conditional forwarder zone for
// AddConditionalForwarder and SetConditionalForwarder.
//
//   - Name: the forwarded domain, for example "partner.example".
//   - MasterServers: IPv4 or IPv6 addresses to forward to, in order. At least
//     one is required.
//   - ReplicationScope: "Forest", "Domain" or "Legacy" stores the forwarder
//     in Active Directory; "" stores it on this server only. On Set, ""
//     leaves the scope unchanged.
//   - ForwarderTimeout: seconds; 0 means "not set" (server default 5 on Add,
//     unchanged on Set).
//
// Conditional forwarders are read back with GetZone: Get-DnsServerZone
// reports them with ZoneType "Forwarder" and fills MasterServers and
// ForwarderTimeout.
type ConditionalForwarderInput struct {
	Name             string
	MasterServers    []string
	ReplicationScope string
	ForwarderTimeout int64
}

// params converts the input into the `$p` map. MasterServers is always sent
// as a JSON array (never null) so the script can splat it as is.
func (in ConditionalForwarderInput) params() map[string]any {
	ms := in.MasterServers
	if ms == nil {
		ms = []string{}
	}
	return map[string]any{
		"name":              in.Name,
		"master_servers":    ms,
		"replication_scope": in.ReplicationScope,
		"forwarder_timeout": in.ForwarderTimeout,
	}
}

// The conditional forwarder scripts (script contract: `$p` inputs, `@cn`
// splat, result in `$out`). Both read the zone back with ConvertTo-WdZone
// (zoneFunc, in zone.go).
//
//   - dns.forwarder.add: Add-DnsServerConditionalForwarderZone. Passing
//     -ReplicationScope selects the AD parameter set; leaving it out
//     selects the file (this server only) set. -ForwarderTimeout is only
//     passed when set, so the server default applies otherwise. The
//     addresses are strings; PowerShell converts them to the IPAddress[]
//     the cmdlet expects.
//   - dns.forwarder.set: Set-DnsServerConditionalForwarderZone with the
//     master servers (and timeout when set). Like for primary zones,
//     -ReplicationScope is in a separate parameter set (ADZone), so it is a
//     second call, made only when a scope is requested and differs from
//     the current one.
var (
	scriptForwarderAdd = psscript.Script{Op: "dns.forwarder.add", Body: zoneFunc + `
$a = @{ Name = $p.name; MasterServers = @($p.master_servers) }
if ($p.forwarder_timeout -gt 0) { $a['ForwarderTimeout'] = [uint32]$p.forwarder_timeout }
if ($p.replication_scope) { $a['ReplicationScope'] = $p.replication_scope }
Add-DnsServerConditionalForwarderZone @cn @a
$out = ConvertTo-WdZone (Get-DnsServerZone @cn -Name $p.name)
`}
	scriptForwarderSet = psscript.Script{Op: "dns.forwarder.set", Body: zoneFunc + `
$a = @{ Name = $p.name; MasterServers = @($p.master_servers) }
if ($p.forwarder_timeout -gt 0) { $a['ForwarderTimeout'] = [uint32]$p.forwarder_timeout }
Set-DnsServerConditionalForwarderZone @cn @a
if ($p.replication_scope) {
    $cur = Get-DnsServerZone @cn -Name $p.name
    if (-not $cur.IsDsIntegrated -or "$($cur.ReplicationScope)" -ne $p.replication_scope) {
        Set-DnsServerConditionalForwarderZone @cn -Name $p.name -ReplicationScope $p.replication_scope
    }
}
$out = ConvertTo-WdZone (Get-DnsServerZone @cn -Name $p.name)
`}
)

// errNoMasters is returned before any PowerShell runs when a forwarder has
// no master server; the cmdlets require at least one.
var errNoMasters = errors.New("a conditional forwarder needs at least one master server")

// AddConditionalForwarder runs Add-DnsServerConditionalForwarderZone and
// returns the zone as read back from the server.
func (c *Client) AddConditionalForwarder(ctx context.Context, in ConditionalForwarderInput) (*Zone, error) {
	if len(in.MasterServers) == 0 {
		return nil, errNoMasters
	}
	var z Zone
	if err := c.get(ctx, scriptForwarderAdd, in.params(), &z); err != nil {
		return nil, err
	}
	return &z, nil
}

// SetConditionalForwarder runs Set-DnsServerConditionalForwarderZone to
// replace the master servers (and the timeout and replication scope when
// set, see ConditionalForwarderInput) and returns the zone as read back.
// Returns ErrNotFound when the zone does not exist.
func (c *Client) SetConditionalForwarder(ctx context.Context, in ConditionalForwarderInput) (*Zone, error) {
	if len(in.MasterServers) == 0 {
		return nil, errNoMasters
	}
	var z Zone
	if err := c.get(ctx, scriptForwarderSet, in.params(), &z); err != nil {
		return nil, err
	}
	return &z, nil
}
