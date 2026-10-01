package dhcp

import (
	"context"
	"time"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/psscript"
)

// Scope is an IPv4 scope as returned by Get-DhcpServerv4Scope, flattened to
// strings by the ConvertTo-WdScope helper below. Used by the scope resource
// and data sources to fill Terraform state. LeaseDuration is a .NET TimeSpan
// string in d.hh:mm:ss form, for example "8.00:00:00" for eight days.
//
// Go note: the text in backticks after each field (`json:"scope_id"`) is a
// "struct tag". The JSON decoder uses it to map the PowerShell key scope_id
// onto the Go field ScopeID.
type Scope struct {
	ScopeID       string `json:"scope_id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	StartRange    string `json:"start_range"`
	EndRange      string `json:"end_range"`
	SubnetMask    string `json:"subnet_mask"`
	State         string `json:"state"`
	Type          string `json:"type"`
	LeaseDuration string `json:"lease_duration"` // d.hh:mm:ss
}

// ScopeInput holds the settable scope properties, as resources pass them to
// AddScope and SetScope. It has no JSON tags because it is never decoded;
// params() converts it to the `$p` map explicitly. State is "Active" or
// "Inactive" (PowerShell matches case-insensitively); Type is "Dhcp",
// "Bootp" or "Both".
//
// Go note: time.Duration is Go's built-in type for a length of time (stored
// as nanoseconds), comparable to a .NET TimeSpan.
type ScopeInput struct {
	ScopeID       string // network address; must match StartRange/SubnetMask
	Name          string
	Description   string
	StartRange    string
	EndRange      string
	SubnetMask    string
	State         string
	Type          string
	LeaseDuration time.Duration
}

// params converts the input into the JSON parameters object (`$p` in the
// script). The lease is sent as whole seconds and rebuilt on the PowerShell
// side with [TimeSpan]::FromSeconds, which avoids any duration string parsing
// differences between Go and .NET.
//
// Go note: `(in ScopeInput)` without a `*` is a value receiver: the method
// gets a copy of the struct, which is fine since it only reads it.
func (in ScopeInput) params() map[string]any {
	return map[string]any{
		"scope_id":      in.ScopeID,
		"name":          in.Name,
		"description":   in.Description,
		"start_range":   in.StartRange,
		"end_range":     in.EndRange,
		"subnet_mask":   in.SubnetMask,
		"state":         in.State,
		"type":          in.Type,
		"lease_seconds": int64(in.LeaseDuration / time.Second),
	}
}

// scopeFunc is a PowerShell helper prepended to the scope scripts.
// ConvertTo-WdScope turns a CIM scope object into an ordered hashtable with
// snake_case keys matching the Scope struct tags. Every value is forced to a
// string ("$(...)") so enums like State serialise as "Active", not as an
// integer. LeaseDuration is formatted as d.hh:mm:ss so the day part is kept
// even below one day (plain ToString() would drop it).
//
// Go note: a string in backticks is a "raw string literal": no escape
// sequences, newlines kept as-is. Ideal for embedding PowerShell, which is
// sent to the server byte for byte.
const scopeFunc = `
function ConvertTo-WdScope($s) {
    [ordered]@{
        scope_id       = "$($s.ScopeId)"
        name           = "$($s.Name)"
        description    = "$($s.Description)"
        start_range    = "$($s.StartRange)"
        end_range      = "$($s.EndRange)"
        subnet_mask    = "$($s.SubnetMask)"
        state          = "$($s.State)"
        type           = "$($s.Type)"
        lease_duration = $s.LeaseDuration.ToString('d\.hh\:mm\:ss')
    }
}
`

// The scope scripts. Each follows the script contract: inputs come from `$p`,
// `@cn` splats -ComputerName, and the result is assigned to `$out`.
//
//   - scope.get: Get-DhcpServerv4Scope -ScopeId. A missing scope makes the
//     cmdlet throw (DHCP 20005 / ObjectNotFound), which becomes ErrNotFound.
//   - scope.list: every scope on the server as an array (`@(...)` keeps it an
//     array even with zero or one scope, so JSON is always a list).
//   - scope.add: Add-DhcpServerv4Scope from a splatted hashtable `$a`, then
//     reads the scope back so Terraform stores what the server actually has.
//     Description is only passed when non-empty. Note Add takes no -ScopeId:
//     the server derives it from StartRange and SubnetMask.
//   - scope.set: Set-DhcpServerv4Scope for all mutable fields, then reads the
//     scope back. SubnetMask is not settable (the resource forces replacement).
//   - scope.remove: Remove-DhcpServerv4Scope; -Force removes a scope that
//     still has active leases, -Confirm:$false suppresses the prompt.
//     Returns no data.
//
// Go note: `var ( ... )` declares several package-level variables in one
// block. psscript.Script{Op: ..., Body: ...} builds a struct value by naming
// its fields. Op is a label used in logs and by test fakes to dispatch.
var (
	scriptScopeGet = psscript.Script{Op: "scope.get", Body: scopeFunc + `
$out = ConvertTo-WdScope (Get-DhcpServerv4Scope @cn -ScopeId $p.scope_id)
`}
	scriptScopeList = psscript.Script{Op: "scope.list", Body: scopeFunc + `
$out = @(Get-DhcpServerv4Scope @cn | ForEach-Object { ConvertTo-WdScope $_ })
`}
	scriptScopeAdd = psscript.Script{Op: "scope.add", Body: scopeFunc + `
$a = @{
    Name          = $p.name
    StartRange    = $p.start_range
    EndRange      = $p.end_range
    SubnetMask    = $p.subnet_mask
    State         = $p.state
    Type          = $p.type
    LeaseDuration = [TimeSpan]::FromSeconds($p.lease_seconds)
}
if ($p.description) { $a['Description'] = $p.description }
Add-DhcpServerv4Scope @cn @a
$out = ConvertTo-WdScope (Get-DhcpServerv4Scope @cn -ScopeId $p.scope_id)
`}
	// The Set command line is split across Go string pieces joined with `+`
	// only to keep line length reasonable; PowerShell receives one line.
	scriptScopeSet = psscript.Script{Op: "scope.set", Body: scopeFunc + `
Set-DhcpServerv4Scope @cn -ScopeId $p.scope_id -Name $p.name -Description "$($p.description)" ` +
		`-StartRange $p.start_range -EndRange $p.end_range -State $p.state -Type $p.type ` +
		`-LeaseDuration ([TimeSpan]::FromSeconds($p.lease_seconds))
$out = ConvertTo-WdScope (Get-DhcpServerv4Scope @cn -ScopeId $p.scope_id)
`}
	scriptScopeRemove = psscript.Script{Op: "scope.remove", Body: `
Remove-DhcpServerv4Scope @cn -ScopeId $p.scope_id -Force:([bool]$p.force) -Confirm:$false
`}
)

// GetScope runs Get-DhcpServerv4Scope for one scope ID (e.g. "10.0.0.0").
// Returns ErrNotFound when the scope does not exist.
//
// Go note: the function returns two values, a pointer to a Scope (*Scope) and
// an error. Exactly one is meaningful: on success err is nil.
func (c *Client) GetScope(ctx context.Context, scopeID string) (*Scope, error) {
	// Go note: `var s Scope` declares s with its "zero value" (all fields
	// empty strings). &s passes its address so the decoder can fill it in.
	var s Scope
	// Go note: `if x := ...; cond {}` runs a statement and then tests a
	// condition, keeping err scoped to the if block. A non-nil err means the
	// call failed; it is passed upward with a nil result (Go has no exceptions).
	if err := c.get(ctx, scriptScopeGet, map[string]any{"scope_id": scopeID}, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// ListScopes returns every IPv4 scope on the server. An empty server gives an
// empty (or nil) slice, not an error.
//
// Go note: `[]Scope` is a "slice", Go's growable list type (like an array in
// PowerShell). Discarding a return value is done by assigning it to `_`.
func (c *Client) ListScopes(ctx context.Context) ([]Scope, error) {
	var out []Scope
	if _, err := c.run(ctx, scriptScopeList, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// AddScope runs Add-DhcpServerv4Scope and returns the created scope as read
// back from the server.
func (c *Client) AddScope(ctx context.Context, in ScopeInput) (*Scope, error) {
	var s Scope
	if err := c.get(ctx, scriptScopeAdd, in.params(), &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// SetScope runs Set-DhcpServerv4Scope and returns the updated scope as read
// back from the server.
func (c *Client) SetScope(ctx context.Context, in ScopeInput) (*Scope, error) {
	var s Scope
	if err := c.get(ctx, scriptScopeSet, in.params(), &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// RemoveScope runs Remove-DhcpServerv4Scope. With force, scopes with active
// leases are removed too; without it the cmdlet refuses and returns an error.
func (c *Client) RemoveScope(ctx context.Context, scopeID string, force bool) error {
	_, err := c.run(ctx, scriptScopeRemove, map[string]any{"scope_id": scopeID, "force": force}, nil)
	return err
}
