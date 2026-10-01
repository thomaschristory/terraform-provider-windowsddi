package dhcp

import (
	"context"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/psscript"
)

// Reservation is an IPv4 reservation as returned by
// Get-DhcpServerv4Reservation, flattened to strings by
// ConvertTo-WdReservation below. ClientID is the MAC address as the server
// formats it (dash separated, e.g. "00-15-5d-01-02-03"); Type is "Dhcp",
// "Bootp" or "Both".
//
// Go note: the `json:"..."` text after each field is a struct tag telling the
// JSON decoder which PowerShell key fills which Go field.
type Reservation struct {
	ScopeID     string `json:"scope_id"`
	IPAddress   string `json:"ip_address"`
	ClientID    string `json:"client_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
}

// ReservationInput holds the settable reservation properties. An empty Name
// leaves the name to the server (on add) or unchanged (on set).
type ReservationInput struct {
	ScopeID     string
	IPAddress   string
	ClientID    string
	Name        string
	Description string
	Type        string
}

// params converts the input into the JSON parameters object (`$p`).
//
// Go note: this is a method with a value receiver `(in ReservationInput)`;
// it is called as in.params() and works on a copy of the struct.
func (in ReservationInput) params() map[string]any {
	return map[string]any{
		"scope_id":    in.ScopeID,
		"ip_address":  in.IPAddress,
		"client_id":   in.ClientID,
		"name":        in.Name,
		"description": in.Description,
		"type":        in.Type,
	}
}

// reservationFunc holds two PowerShell helpers prepended to the reservation
// scripts:
//   - ConvertTo-WdReservation flattens a reservation object into an ordered
//     hashtable of strings with snake_case keys (matching the struct tags).
//   - Get-WdReservation lists the reservations of $p.scope_id and keeps the
//     one whose IP equals $p.ip_address. It returns nothing when there is no
//     match. Listing and filtering avoids depending on the error the cmdlet
//     throws for an unknown IP, so "not found" is simply "no output".
//
// Go note: the backtick string is a raw string literal: its content is sent
// to PowerShell exactly as written, with no escape processing.
const reservationFunc = `
function ConvertTo-WdReservation($r) {
    [ordered]@{
        scope_id    = "$($r.ScopeId)"
        ip_address  = "$($r.IPAddress)"
        client_id   = "$($r.ClientId)"
        name        = "$($r.Name)"
        description = "$($r.Description)"
        type        = "$($r.Type)"
    }
}
function Get-WdReservation {
    Get-DhcpServerv4Reservation @cn -ScopeId $p.scope_id |
        Where-Object { "$($_.IPAddress)" -eq $p.ip_address } |
        Select-Object -First 1
}
`

// The reservation scripts (script contract: `$p` inputs, `@cn` splat for
// -ComputerName, result in `$out`):
//
//   - reservation.get: Get-WdReservation; `$out` stays $null when there is no
//     match, which Client.get turns into ErrNotFound. A missing scope makes
//     the cmdlet throw instead (also mapped to ErrNotFound by psscript).
//   - reservation.list: every reservation in the scope, always as an array.
//   - reservation.add: Add-DhcpServerv4Reservation from the splatted `$a`,
//     with Name and Description only passed when non-empty, then reads it back.
//   - reservation.set: Set-DhcpServerv4Reservation keyed by -IPAddress (the
//     cmdlet has no -ScopeId). Description is always passed, so an empty
//     string clears it; Name is only passed when set. Then reads it back.
//   - reservation.remove: Remove-DhcpServerv4Reservation by IP, no prompt.
var (
	scriptReservationGet = psscript.Script{Op: "reservation.get", Body: reservationFunc + `
$r = Get-WdReservation
if ($r) { $out = ConvertTo-WdReservation $r }
`}
	scriptReservationList = psscript.Script{Op: "reservation.list", Body: reservationFunc + `
$out = @(Get-DhcpServerv4Reservation @cn -ScopeId $p.scope_id | ForEach-Object { ConvertTo-WdReservation $_ })
`}
	scriptReservationAdd = psscript.Script{Op: "reservation.add", Body: reservationFunc + `
$a = @{ ScopeId = $p.scope_id; IPAddress = $p.ip_address; ClientId = $p.client_id; Type = $p.type }
if ($p.name) { $a['Name'] = $p.name }
if ($p.description) { $a['Description'] = $p.description }
Add-DhcpServerv4Reservation @cn @a
$r = Get-WdReservation
if ($r) { $out = ConvertTo-WdReservation $r }
`}
	scriptReservationSet = psscript.Script{Op: "reservation.set", Body: reservationFunc + `
$a = @{ IPAddress = $p.ip_address; ClientId = $p.client_id; Type = $p.type; Description = "$($p.description)" }
if ($p.name) { $a['Name'] = $p.name }
Set-DhcpServerv4Reservation @cn @a
$r = Get-WdReservation
if ($r) { $out = ConvertTo-WdReservation $r }
`}
	scriptReservationRemove = psscript.Script{Op: "reservation.remove", Body: `
Remove-DhcpServerv4Reservation @cn -IPAddress $p.ip_address -Confirm:$false
`}
)

// GetReservation returns the reservation for ipAddress in scopeID, or
// ErrNotFound when there is none.
//
// Go note: the function returns two values (a *Reservation pointer and an
// error); `if err := ...; err != nil { return nil, err }` is the standard
// "stop and pass the error up" check, since Go has no exceptions.
func (c *Client) GetReservation(ctx context.Context, scopeID, ipAddress string) (*Reservation, error) {
	var r Reservation
	if err := c.get(ctx, scriptReservationGet, map[string]any{"scope_id": scopeID, "ip_address": ipAddress}, &r); err != nil {
		return nil, err
	}
	// Go note: &r is the address of r, returned as a *Reservation pointer.
	return &r, nil
}

// ListReservations returns every reservation in scopeID.
//
// Go note: `[]Reservation` is a slice (a list); `_` discards the "data
// present" bool that run returns, since an empty list is not an error here.
func (c *Client) ListReservations(ctx context.Context, scopeID string) ([]Reservation, error) {
	var out []Reservation
	if _, err := c.run(ctx, scriptReservationList, map[string]any{"scope_id": scopeID}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// AddReservation runs Add-DhcpServerv4Reservation and returns the
// reservation as read back from the server.
func (c *Client) AddReservation(ctx context.Context, in ReservationInput) (*Reservation, error) {
	var r Reservation
	if err := c.get(ctx, scriptReservationAdd, in.params(), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// SetReservation runs Set-DhcpServerv4Reservation (keyed by IP address) and
// returns the reservation as read back from the server.
func (c *Client) SetReservation(ctx context.Context, in ReservationInput) (*Reservation, error) {
	var r Reservation
	if err := c.get(ctx, scriptReservationSet, in.params(), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// RemoveReservation runs Remove-DhcpServerv4Reservation. Only the IP is
// needed because reserved IPs are unique across the server.
func (c *Client) RemoveReservation(ctx context.Context, ipAddress string) error {
	_, err := c.run(ctx, scriptReservationRemove, map[string]any{"ip_address": ipAddress}, nil)
	return err
}
