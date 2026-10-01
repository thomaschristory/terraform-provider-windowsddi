package dhcp

import (
	"context"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/psscript"
)

// ExclusionRange is an IPv4 exclusion range within a scope: addresses the
// server will not hand out. The three fields together are its identity, so
// the same struct is used both as input and as output (no separate Input
// type), and there is no Set: changing any field means remove and add.
//
// Go note: the `json:"..."` struct tags map the PowerShell keys onto fields.
type ExclusionRange struct {
	ScopeID    string `json:"scope_id"`
	StartRange string `json:"start_range"`
	EndRange   string `json:"end_range"`
}

// params converts the range into the JSON parameters object (`$p`).
//
// Go note: a method with a value receiver `(e ExclusionRange)`, called as
// e.params(); map[string]any is a string-keyed dictionary like a hashtable.
func (e ExclusionRange) params() map[string]any {
	return map[string]any{"scope_id": e.ScopeID, "start_range": e.StartRange, "end_range": e.EndRange}
}

// exclusionFunc defines Get-WdExclusion: list the scope's exclusion ranges
// and return the one whose start and end match `$p` exactly, as an ordered
// hashtable of strings. It returns nothing when there is no exact match, so
// "not found" is simply "no output" rather than a cmdlet error code.
//
// Go note: the backtick string is a raw string literal, sent to PowerShell
// unchanged.
const exclusionFunc = `
function Get-WdExclusion {
    $e = Get-DhcpServerv4ExclusionRange @cn -ScopeId $p.scope_id |
        Where-Object { "$($_.StartRange)" -eq $p.start_range -and "$($_.EndRange)" -eq $p.end_range } |
        Select-Object -First 1
    if ($e) {
        [ordered]@{ scope_id = "$($e.ScopeId)"; start_range = "$($e.StartRange)"; end_range = "$($e.EndRange)" }
    }
}
`

// The exclusion scripts (script contract: `$p` inputs, `@cn` splat for
// -ComputerName, result in `$out`):
//
//   - exclusion.get: Get-WdExclusion; `$out` is $null when no exact match,
//     which Client.get maps to ErrNotFound.
//   - exclusion.add: Add-DhcpServerv4ExclusionRange, then read it back.
//   - exclusion.remove: Remove-DhcpServerv4ExclusionRange, no prompt.
var (
	scriptExclusionGet = psscript.Script{Op: "exclusion.get", Body: exclusionFunc + `
$out = Get-WdExclusion
`}
	scriptExclusionAdd = psscript.Script{Op: "exclusion.add", Body: exclusionFunc + `
Add-DhcpServerv4ExclusionRange @cn -ScopeId $p.scope_id -StartRange $p.start_range -EndRange $p.end_range
$out = Get-WdExclusion
`}
	scriptExclusionRemove = psscript.Script{Op: "exclusion.remove", Body: `
Remove-DhcpServerv4ExclusionRange @cn -ScopeId $p.scope_id -StartRange $p.start_range -EndRange $p.end_range -Confirm:$false
`}
)

// GetExclusionRange returns the exclusion range matching e exactly, or
// ErrNotFound.
//
// Go note: two return values (a *ExclusionRange pointer and an error). The
// `if err := ...; err != nil { return nil, err }` pattern is Go's way of
// stopping and passing an error up, since there are no exceptions.
func (c *Client) GetExclusionRange(ctx context.Context, e ExclusionRange) (*ExclusionRange, error) {
	var out ExclusionRange
	if err := c.get(ctx, scriptExclusionGet, e.params(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AddExclusionRange runs Add-DhcpServerv4ExclusionRange and returns the
// range as read back from the server.
func (c *Client) AddExclusionRange(ctx context.Context, e ExclusionRange) (*ExclusionRange, error) {
	var out ExclusionRange
	if err := c.get(ctx, scriptExclusionAdd, e.params(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RemoveExclusionRange runs Remove-DhcpServerv4ExclusionRange.
//
// Go note: `_` discards the first return value of run (the "data present"
// flag), which does not matter for a remove.
func (c *Client) RemoveExclusionRange(ctx context.Context, e ExclusionRange) error {
	_, err := c.run(ctx, scriptExclusionRemove, e.params(), nil)
	return err
}
