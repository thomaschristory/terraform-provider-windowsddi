// Package dhcpresources implements the windowsddi DHCP managed resources: the things a user declares
// with a `resource "windowsddi_dhcp_..." "name" { ... }` block in HCL.
//
// # What a "resource" is, seen from the provider side
//
// Terraform core (the `terraform` binary) knows nothing about DHCP. It starts this provider
// as a plugin process and talks to it over gRPC. For every resource type the provider
// exposes, Terraform asks for a schema (the list of attributes and their rules) and then
// calls a fixed set of methods, which the terraform-plugin-framework library routes to the
// Go types in this package:
//
//   - Metadata: returns the resource type name, for example "windowsddi_dhcp_scope".
//   - Schema: describes the attributes (Required/Optional/Computed, defaults, validators,
//     plan modifiers). The same descriptions feed the generated Registry docs.
//   - Configure: hands the resource the shared *dhcp.Client built by the provider block.
//   - ModifyPlan (optional, only scope.go uses it): runs during `terraform plan` and may
//     adjust the proposed new values or flag that the resource must be replaced.
//   - Create: called on apply when the resource is new. Creates the object on the server
//     and saves what the server now holds into Terraform state.
//   - Read: called on every plan/refresh. Fetches the object from the server and updates
//     state. If the object is gone, it removes the resource from state ("drift"), so the
//     next plan proposes to create it again instead of failing.
//   - Update: called on apply when attributes changed and can be changed in place.
//   - Delete: called on destroy, or as the first half of a replacement.
//   - ImportState: called by `terraform import` (or an `import {}` block). It turns the
//     user-supplied import ID string into just enough state for Read to fill in the rest.
//
// # Where this package sits
//
//	Terraform core -> provider (internal/provider) -> resources (this package)
//	  -> dhcp (typed Go client) -> psscript (PowerShell templates) + runner (SSH/WinRM)
//	  -> Windows DHCP server running the DhcpServer cmdlets
//
// The layering is strict: resources only translate between Terraform values and the
// typed structs of the dhcp package. They never build PowerShell strings; that is the
// job of dhcp and psscript, which pass user values as a JSON parameters object so they can
// never be interpreted as PowerShell code.
//
// # Files
//
//   - common.go: shared helpers (resource list, client extraction, error text, enums).
//   - scope.go: windowsddi_dhcp_scope (Add/Set/Remove-DhcpServerv4Scope). The reference
//     resource: its comments explain the framework concepts in the most detail.
//   - reservation.go: windowsddi_dhcp_reservation (Add/Set/Remove-DhcpServerv4Reservation).
//   - exclusion_range.go: windowsddi_dhcp_exclusion_range (Add/Remove-DhcpServerv4ExclusionRange).
//   - option_value.go: windowsddi_dhcp_option_value (Set/Remove-DhcpServerv4OptionValue) at
//     server, scope or reservation level.
//   - *_test.go: tests that run real Terraform against an in-memory fake DHCP server.
//
// Go note: every .go file starts with `package <name>`. All files in one directory share
// the same package, so a function defined in common.go is directly usable in scope.go.
package dhcpresources

// Go note: `import` lists the other packages this file uses. Standard library packages
// have short names ("fmt"); others are referenced by their module path.
import (
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dhcp"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/providerdata"
)

// All returns every resource of the provider. The provider package calls it from its
// Resources() method so Terraform learns which resource types exist.
//
// Go note: names starting with an upper-case letter (All, NewScope) are "exported", that is
// visible to other packages. Lower-case names (clientFrom, scopeResource) are private to
// this package.
//
// Go note: `[]func() resource.Resource` is a slice (a growable list) of functions that each
// return a new resource. The framework calls them to get a fresh, unconfigured instance.
func All() []func() resource.Resource {
	return []func() resource.Resource{
		NewScope,
		NewReservation,
		NewExclusionRange,
		NewOptionValue,
	}
}

// clientFrom extracts the dhcp client from provider data. It returns nil
// (without error) before the provider is configured.
//
// The provider's Configure() builds one *providerdata.Clients (a DHCP and a DNS client
// sharing the SSH or WinRM connection) and passes it to every resource as an untyped value
// ("ProviderData"). Each DHCP resource's Configure method calls this helper to pick the
// typed DHCP client out of it.
// Terraform may call a resource's Configure before the provider itself is configured
// (for example while validating), in which case data is nil and there is nothing to do.
//
// Go note: `any` means "a value of any type". `*dhcp.Client` is a pointer to a Client:
// the resource shares the provider's single client instead of copying it.
//
// Go note: errors are not exceptions in Terraform providers. Problems are added to a
// "diagnostics" list (diags), which the framework shows to the user as Error: blocks.
func clientFrom(data any, diags *diag.Diagnostics) *dhcp.Client {
	return providerdata.DHCPFrom(data, diags)
}

// errSummary builds the short, first-line summary of an error diagnostic, for example
// "Unable to create scope 10.1.2.0". The detail line (the cmdlet name and PowerShell error
// message coming from the dhcp package) is supplied separately by the caller.
func errSummary(action, what string) string {
	return fmt.Sprintf("Unable to %s %s", action, what)
}

// Enum spellings accepted by the schema validators and stored in state. They match the
// casing used by the DhcpServer cmdlets' -State and -Type parameters.
//
// Note "InActive" with a capital A: that is how Set-DhcpServerv4Scope spells it. The server
// sometimes reports "Inactive" instead, so values read back are passed through
// normalize.EnumOf, which maps them case-insensitively onto these canonical spellings.
// Without that, Terraform would show a perpetual "InActive" -> "Inactive" diff.
//
// Go note: `const ( ... )` groups several constants; they cannot change at runtime.
const (
	stateActive   = "Active"
	stateInactive = "InActive"
	typeDhcp      = "Dhcp"
	typeBootp     = "Bootp"
	typeBoth      = "Both"
)

// Allowed-value lists, used both by the OneOf validators in the schemas and by
// normalize.EnumOf when canonicalising what the server returns.
//
// Go note: `var ( ... )` declares package-level variables. `[]string{...}` is a slice
// literal: a list of strings.
var (
	scopeStates = []string{stateActive, stateInactive}
	clientTypes = []string{typeDhcp, typeBootp, typeBoth}
)
