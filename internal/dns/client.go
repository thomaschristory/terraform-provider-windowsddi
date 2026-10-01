// Package dns is a typed Go client for the Windows DnsServer PowerShell
// cmdlets (Get-DnsServerZone, Add-DnsServerResourceRecord, and so on).
//
// It mirrors the dhcp package and sits at the same place in the request flow:
//
//	Terraform CLI
//	  -> provider (internal/provider: connection settings, builds a Client)
//	    -> resources / datasources (CRUD logic, Terraform schema)
//	      -> recordset (generic record-set engine, record resources only)
//	        -> dns (this package: one Go function per cmdlet operation)
//	          -> psscript (wraps scripts in the JSON envelope, parses results)
//	          -> runner   (ships the script to the host over SSH or WinRM)
//	            -> Windows host running powershell.exe and the DnsServer module
//
// The script contract is the same as for DHCP (see docs/DESIGN.md, "Script
// contract"): user values only reach PowerShell through `$p`, every cmdlet
// gets `@cn` splatted (ComputerName when dns_server is set), and the body
// assigns its result to `$out`. DNS adds a few rules of its own, listed in
// DESIGN.md under "DNS script rules": record queries always return arrays,
// RecordData is projected explicitly, TTLs are whole seconds and records
// report whether they are dynamic.
//
// Not found: a missing zone makes the cmdlets fail with WIN32 9601, which
// psscript maps to ErrNotFound. Record queries for a name without records
// return an empty slice instead (the scripts swallow WIN32 9714), so
// "no records" and "no zone" stay distinguishable.
//
// Files:
//   - client.go: the Client type, ErrNotFound, and the shared run/get helpers.
//   - zone.go: zones (Get/Add/Set-DnsServerPrimaryZone, Remove-DnsServerZone).
//   - forwarder.go: conditional forwarders (Add/Set-DnsServerConditionalForwarderZone).
//   - record.go: the generic Record model and record operations
//     (Get/Add/Set/Remove-DnsServerResourceRecord and the per-type Add cmdlets).
//   - client_test.go: unit tests using a fake runner and recorded JSON in testdata/.
//   - pwsh_test.go: runs the real scripts in a local pwsh against stub cmdlets.
//   - dnsfake/: an in-memory fake DNS server used by resource tests.
package dns

import (
	"context"
	"errors"

	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/psscript"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/runner"
)

// ErrNotFound is returned (possibly wrapped) when the requested object does
// not exist on the DNS server. It is the same value as psscript.ErrNotFound
// and dhcp.ErrNotFound. Test for it with errors.Is or IsNotFound.
var ErrNotFound = psscript.ErrNotFound

// IsNotFound reports whether err means the object does not exist.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// module is the requirement attached to every DNS script, so a host without
// the DnsServer module fails with a message naming what to install.
var module = &psscript.Requirement{
	Module: "DnsServer",
	Probe:  "Get-DnsServerZone",
	Hint:   "Install the DNS Server role, or the RSAT DNS tools on a management host (Install-WindowsFeature RSAT-DNS-Server).",
}

// Client runs DnsServer cmdlets on one host. The provider creates a single
// Client in Configure and hands it to every DNS resource and data source.
//
// Fields:
//   - r is the transport (SSH or WinRM) that actually executes scripts. It is
//     the same runner the DHCP client uses, so both share one connection and
//     one concurrency limit.
//   - computerName, when not empty, is passed as -ComputerName to every
//     cmdlet so a management host can drive a different DNS server.
type Client struct {
	r            runner.Runner
	computerName string
}

// New returns a Client. computerName, when set, is passed as -ComputerName
// to every cmdlet.
func New(r runner.Runner, computerName string) *Client {
	return &Client{r: r, computerName: computerName}
}

// run executes script s with params and decodes the envelope's data into out.
// It returns false when the script succeeded but returned no data (`$out` was
// $null). out may be nil when the caller does not need the result.
func (c *Client) run(ctx context.Context, s psscript.Script, params map[string]any, out any) (bool, error) {
	if params == nil {
		params = map[string]any{}
	}
	if c.computerName != "" {
		params["computer_name"] = c.computerName
	}
	// Params are DNS data (zone and record names, addresses), never
	// credentials, so logging them is safe.
	tflog.Debug(ctx, "running DNS script", map[string]any{"op": s.Op, "params": params})
	s.Requires = module
	raw, err := c.r.Run(ctx, s.Wrap(), params)
	if err != nil {
		return false, err
	}
	present, err := psscript.Parse(raw, out)
	if err != nil {
		tflog.Debug(ctx, "DNS script failed", map[string]any{"op": s.Op, "error": err.Error()})
	}
	return present, err
}

// get runs a script that returns a single object and maps "no data" to
// ErrNotFound, like dhcp.Client.get.
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
var scriptAvailable = psscript.Script{Op: "dns.available", Body: `$out = $true`}

// CheckAvailable returns an error when the DnsServer module cannot be used on the host
// (role or RSAT tools missing, or the host is unreachable). Acceptance tests call it to skip
// DNS tests against a host without the role.
func (c *Client) CheckAvailable(ctx context.Context) error {
	_, err := c.run(ctx, scriptAvailable, nil, nil)
	return err
}
