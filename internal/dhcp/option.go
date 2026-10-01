package dhcp

import (
	"context"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/psscript"
)

// OptionKey identifies an option value: the option number and the level it
// is set at. The level is decided by which fields are filled:
//   - ScopeID and ReservedIP both empty: server level.
//   - ScopeID set: scope level.
//   - ReservedIP set: reservation level.
//
// VendorClass and UserClass narrow the value to a vendor or user class, the
// same as the cmdlets' -VendorClass and -UserClass parameters.
type OptionKey struct {
	OptionID    int64
	ScopeID     string
	ReservedIP  string
	VendorClass string
	UserClass   string
}

// params converts the key into the JSON parameters object (`$p`). Empty
// strings are sent as-is; the PowerShell side skips them when building `$sel`.
//
// Go note: a method with a value receiver `(k OptionKey)`, called as
// k.params(); map[string]any is a string-keyed dictionary like a hashtable.
func (k OptionKey) params() map[string]any {
	return map[string]any{
		"option_id":    k.OptionID,
		"scope_id":     k.ScopeID,
		"reserved_ip":  k.ReservedIP,
		"vendor_class": k.VendorClass,
		"user_class":   k.UserClass,
	}
}

// OptionValue is an option value as returned by Get-DhcpServerv4OptionValue.
// Value is always a list of strings, because DHCP options can be multi-valued
// (e.g. option 6, DNS servers) and every value is stringified on the server.
//
// Go note: the `json:"..."` struct tags map PowerShell keys onto fields, and
// `[]string` is a slice (list) of strings.
type OptionValue struct {
	OptionID    int64    `json:"option_id"`
	Name        string   `json:"name"`
	Value       []string `json:"value"`
	VendorClass string   `json:"vendor_class"`
	UserClass   string   `json:"user_class"`
}

// optionFunc is prepended to every option script. It does two things:
//   - Builds `$sel`, a hashtable of level selectors (ScopeId, ReservedIP,
//     VendorClass, UserClass) holding only the non-empty ones. Splatting
//     `@sel` therefore targets server, scope or reservation level with the
//     same script.
//   - Defines Get-WdOption: list the option values at that level and return
//     the one with the requested OptionId as an ordered hashtable, or nothing
//     when the option is not set. "Not set" is thus "no output", not an error.
//
// Go note: the backtick string is a raw string literal, sent to PowerShell
// byte for byte with no escape processing.
const optionFunc = `
$sel = @{}
if ($p.scope_id) { $sel['ScopeId'] = $p.scope_id }
if ($p.reserved_ip) { $sel['ReservedIP'] = $p.reserved_ip }
if ($p.vendor_class) { $sel['VendorClass'] = $p.vendor_class }
if ($p.user_class) { $sel['UserClass'] = $p.user_class }
function Get-WdOption {
    $o = Get-DhcpServerv4OptionValue @cn @sel |
        Where-Object { [int]$_.OptionId -eq [int]$p.option_id } |
        Select-Object -First 1
    if ($o) {
        [ordered]@{
            option_id    = [int]$o.OptionId
            name         = "$($o.Name)"
            value        = @($o.Value | ForEach-Object { "$_" })
            vendor_class = "$($o.VendorClass)"
            user_class   = "$($o.UserClass)"
        }
    }
}
`

// The option scripts (script contract: `$p` inputs, `@cn` splat for
// -ComputerName, result in `$out`):
//
//   - option.get: Get-WdOption; `$out` is $null when the option is not set
//     at that level, which Client.get maps to ErrNotFound.
//   - option.set: Set-DhcpServerv4OptionValue with the value forced to a
//     [string[]] (so a single value is still an array), then read it back.
//     Set creates or overwrites, so there is no separate "add".
//   - option.remove: Remove-DhcpServerv4OptionValue at that level, no prompt.
var (
	scriptOptionGet = psscript.Script{Op: "option.get", Body: optionFunc + `
$out = Get-WdOption
`}
	scriptOptionSet = psscript.Script{Op: "option.set", Body: optionFunc + `
Set-DhcpServerv4OptionValue @cn @sel -OptionId $p.option_id -Value ([string[]]@($p.value))
$out = Get-WdOption
`}
	scriptOptionRemove = psscript.Script{Op: "option.remove", Body: optionFunc + `
Remove-DhcpServerv4OptionValue @cn @sel -OptionId $p.option_id -Confirm:$false
`}
)

// GetOptionValue returns the option value at the level described by k, or
// ErrNotFound when the option is not set there.
//
// Go note: two return values (a *OptionValue pointer and an error). The
// `if err := ...; err != nil { return nil, err }` pattern stops and passes
// the error up, since Go has no exceptions. &out is the address of out.
func (c *Client) GetOptionValue(ctx context.Context, k OptionKey) (*OptionValue, error) {
	var out OptionValue
	if err := c.get(ctx, scriptOptionGet, k.params(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetOptionValue runs Set-DhcpServerv4OptionValue (create or replace) and
// returns the value as read back from the server.
func (c *Client) SetOptionValue(ctx context.Context, k OptionKey, value []string) (*OptionValue, error) {
	// Start from the key's parameters and add the value list.
	// Go note: `:=` declares a new variable p; maps are updated in place
	// with m["key"] = value.
	p := k.params()
	p["value"] = value
	var out OptionValue
	if err := c.get(ctx, scriptOptionSet, p, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RemoveOptionValue runs Remove-DhcpServerv4OptionValue.
//
// Go note: `_` discards run's "data present" flag, irrelevant for a remove.
func (c *Client) RemoveOptionValue(ctx context.Context, k OptionKey) error {
	_, err := c.run(ctx, scriptOptionRemove, k.params(), nil)
	return err
}
