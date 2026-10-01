// Package dhcp is a typed Go client for the Windows DhcpServer PowerShell
// cmdlets (Get-DhcpServerv4Scope, Add-DhcpServerv4Reservation, and so on).
//
// Where it sits in the request flow:
//
//	Terraform CLI
//	  -> provider (internal/provider: connection settings, builds a Client)
//	    -> resources / datasources (CRUD logic, Terraform schema)
//	      -> dhcp (this package: one Go function per cmdlet operation)
//	        -> psscript (wraps scripts in the JSON envelope, parses results)
//	        -> runner   (ships the script to the host over SSH or WinRM)
//	          -> Windows host running powershell.exe and the DhcpServer module
//
// Resources never write PowerShell themselves. They call functions here such
// as GetScope or AddReservation with plain Go values, and get Go structs back.
// This package owns the PowerShell text for each operation and makes sure
// user values only reach PowerShell through the JSON parameters object `$p`
// (see docs/DESIGN.md, "Script contract"), never by string interpolation.
//
// The script contract, as used by every script in this package:
//   - The script body reads its inputs from `$p`, an object built by
//     ConvertFrom-Json from the parameters map passed in from Go.
//   - It splats `@cn` into every cmdlet. `$cn` is a hashtable that holds
//     ComputerName when the provider targets a remote DHCP server through a
//     management host, and is empty otherwise.
//   - It assigns its result to `$out`. psscript wraps the body in try/catch
//     and prints `{ok, data, error}` as JSON, where data is `$out`. Anything
//     else the body writes to the pipeline is discarded.
//   - A thrown error becomes `ok = false` with the cmdlet name, message,
//     category and FullyQualifiedErrorId. psscript turns "not found" style
//     errors into ErrNotFound. A script that finishes with `$out = $null`
//     (for example a filtered list with no match) also means "not found"
//     for the single-object getters (see Client.get).
//
// Files:
//   - client.go: the Client type, ErrNotFound, and the shared run/get helpers.
//   - scope.go: IPv4 scopes (Get/Add/Set/Remove-DhcpServerv4Scope).
//   - reservation.go: IPv4 reservations (Get/Add/Set/Remove-DhcpServerv4Reservation).
//   - exclusion.go: exclusion ranges inside a scope.
//   - option.go: option values at server, scope or reservation level.
//   - lease.go: read-only lease listing (Get-DhcpServerv4Lease).
//   - client_test.go: unit tests using a fake runner and recorded JSON in testdata/.
//   - pwsh_test.go: runs the real scripts in a local pwsh against stub cmdlets.
//   - dhcpfake/: an in-memory fake DHCP server used by resource tests.
package dhcp

// Go note: an import block lists the packages this file uses. Standard library
// packages have short paths ("context"); others use their full module path.
import (
	"context"
	"errors"

	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/psscript"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/runner"
)

// ErrNotFound is returned (possibly wrapped inside another error) when the
// requested object does not exist on the DHCP server. Resources use it in
// Read to remove the resource from state (drift) instead of failing.
// It is the same value as psscript.ErrNotFound, re-exported here so callers
// only need to import this package. Test for it with errors.Is or IsNotFound.
//
// Go note: names starting with a capital letter (ErrNotFound, Client, New)
// are "exported", meaning visible to other packages. Lowercase names
// (run, get, computerName) are private to this package.
var ErrNotFound = psscript.ErrNotFound

// IsNotFound reports whether err means the object does not exist.
//
// Go note: errors.Is walks a chain of wrapped errors and returns true if any
// of them matches the target. psscript's error type implements an Is method
// so that a PowerShell "ObjectNotFound" error matches ErrNotFound.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// module is the requirement attached to every DHCP script: if the DhcpServer
// module is missing on the host, the script fails with a message naming the
// role or RSAT feature to install instead of a cryptic "command not found".
var module = &psscript.Requirement{
	Module: "DhcpServer",
	Probe:  "Get-DhcpServerv4Scope",
	Hint:   "Install the DHCP Server role, or the RSAT DHCP tools on a management host (Install-WindowsFeature RSAT-DHCP).",
}

// Client runs DhcpServer cmdlets on one host. The provider creates a single
// Client in Configure and hands it to every resource and data source.
//
// Go note: a struct is a record type with named fields, like a PowerShell
// [pscustomobject] with a fixed shape. These fields are lowercase, so code
// outside this package cannot touch them directly.
//
// Fields:
//   - r is the transport (SSH or WinRM) that actually executes scripts.
//   - computerName, when not empty, is passed as -ComputerName to every
//     cmdlet so a management host can drive a different DHCP server.
//
// Go note: runner.Runner is an interface: any type that has the right methods
// can be stored in r, which is how tests swap in a fake transport.
type Client struct {
	r            runner.Runner
	computerName string
}

// New returns a Client. computerName, when set, is passed as -ComputerName
// to every cmdlet (management host setup: you SSH/WinRM into one machine
// that has the DhcpServer module and point it at the real DHCP server).
//
// Go note: `*Client` is a pointer to a Client, and `&Client{...}` creates one
// and returns its address. Sharing a pointer avoids copying the struct and
// lets every caller use the same instance.
func New(r runner.Runner, computerName string) *Client {
	return &Client{r: r, computerName: computerName}
}

// run executes script s with params and decodes the envelope's data into out.
// It returns false when the script succeeded but returned no data
// (`$out` was $null). out may be nil when the caller does not need the
// result, for example for Remove operations.
//
// Go note: `func (c *Client) run(...)` is a method: a function attached to
// the Client type, called as c.run(...). `c` plays the role of `this`/`self`.
// Go note: functions can return several values. Here it returns a bool and
// an error; callers receive both, as in `present, err := c.run(...)`.
// Go note: `map[string]any` is a dictionary with string keys and values of
// any type, the Go equivalent of a PowerShell hashtable. It is serialised to
// JSON and becomes `$p` on the PowerShell side.
func (c *Client) run(ctx context.Context, s psscript.Script, params map[string]any, out any) (bool, error) {
	// Go note: `nil` is Go's null. A nil map cannot be written to, so make
	// an empty one before adding computer_name below.
	if params == nil {
		params = map[string]any{}
	}
	// Add the target server name; the psscript preamble turns
	// $p.computer_name into the @cn splat.
	if c.computerName != "" {
		params["computer_name"] = c.computerName
	}
	// Debug logging only. These params are DHCP data (IPs, names), not
	// credentials, so logging them is safe.
	tflog.Debug(ctx, "running DHCP script", map[string]any{"op": s.Op, "params": params})
	// Send the wrapped script (body inside the try/catch envelope) to the
	// host. The runner adds the $p preamble and returns the raw JSON output.
	//
	// Go note: `:=` declares new variables and assigns them in one step;
	// plain `=` assigns to variables that already exist.
	s.Requires = module
	raw, err := c.r.Run(ctx, s.Wrap(), params)
	// Go note: Go has no exceptions. Functions return an error value, and the
	// caller checks `if err != nil` and usually returns it upward.
	if err != nil {
		return false, err
	}
	// Decode {ok, data, error}. A PowerShell failure comes back as err here,
	// carrying the cmdlet name and message.
	present, err := psscript.Parse(raw, out)
	if err != nil {
		tflog.Debug(ctx, "DHCP script failed", map[string]any{"op": s.Op, "error": err.Error()})
	}
	return present, err
}

// get runs a script that returns a single object. It is run plus one rule:
// a successful script that returned no data maps to ErrNotFound. This is how
// list-and-filter scripts (reservations, exclusions, options) report a
// missing object without relying on cmdlet error codes.
func (c *Client) get(ctx context.Context, s psscript.Script, params map[string]any, out any) error {
	present, err := c.run(ctx, s, params, out)
	if err != nil {
		return err
	}
	if !present {
		return ErrNotFound
	}
	return nil
}

// scriptAvailable does nothing but trigger the module check every script carries.
var scriptAvailable = psscript.Script{Op: "dhcp.available", Body: `$out = $true`}

// CheckAvailable returns an error when the DhcpServer module cannot be used on the host
// (role or RSAT tools missing, or the host is unreachable). Acceptance tests call it to skip
// DHCP tests against a host without the role.
func (c *Client) CheckAvailable(ctx context.Context) error {
	_, err := c.run(ctx, scriptAvailable, nil, nil)
	return err
}
