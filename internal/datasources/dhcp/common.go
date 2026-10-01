// Package dhcpdatasources implements the windowsddi DHCP data sources.
//
// What a data source is, seen from the provider side: the `data "windowsddi_dhcp_..." {}` blocks
// in HCL. They are read-only lookups. Unlike resources they never create, change or delete
// anything, so they only implement Schema (which attributes exist) and Read (fetch the data
// and fill those attributes). They have no state of their own between runs: Terraform calls
// Read again on every plan and the result is whatever the DHCP server says right now.
//
// Each data source also implements Metadata (its name) and Configure (receive the shared
// *dhcp.Client built by the provider). The framework calls these methods; nothing in this
// package calls them directly.
//
// Files:
//   - common.go: the list of data sources (All), shared helpers and the scope model.
//   - scope.go: windowsddi_dhcp_scope, one scope by ID (Get-DhcpServerv4Scope).
//   - scopes.go: windowsddi_dhcp_scopes, every scope on the server.
//   - reservation.go: windowsddi_dhcp_reservation, one reservation by IP or client ID.
//   - leases.go: windowsddi_dhcp_leases, current leases of a scope (Get-DhcpServerv4Lease).
//   - datasources_test.go: unit tests (fake server) and acceptance tests (real server).
//
// Layering: these files only call the dhcp package. They never build PowerShell.
package dhcpdatasources

import (
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dhcp"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/providerdata"
)

// All returns every data source of the provider.
//
// The provider's DataSources method returns this list. Each entry is a constructor function
// (not an instance): the framework calls it whenever it needs a fresh data source object.
// To add a data source, write its file and add its New* function here.
//
// Go note: `NewScope` without parentheses refers to the function itself, not its result.
// `[]func() datasource.DataSource{...}` is a slice (list) of such functions.
func All() []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewScope,
		NewScopes,
		NewReservation,
		NewLeases,
	}
}

// clientFrom extracts the *dhcp.Client from the *providerdata.Clients that the provider's
// Configure stored in DataSourceData. Every data source's Configure calls it.
//
// data is nil when Terraform calls Configure before the provider itself is configured (this
// happens during early validation); returning nil then is normal, and Read is only called
// later once a real client is available. Any other type is a programming error.
//
// Go note: `any` means "a value of any type". providerdata.DHCPFrom uses a type assertion to
// check the value really is a *providerdata.Clients before handing out its DHCP client.
func clientFrom(data any, diags *diag.Diagnostics) *dhcp.Client {
	return providerdata.DHCPFrom(data, diags)
}

// scopeModel mirrors the scope attributes, shared by windowsddi_dhcp_scope and
// windowsddi_dhcp_scopes.
//
// For windowsddi_dhcp_scope it is the whole data source; for windowsddi_dhcp_scopes it is one element
// of the `scopes` list. Values are copied from dhcp.Scope, which mirrors the properties of a
// Get-DhcpServerv4Scope result.
//
// Go note: the backquoted `tfsdk:"..."` struct tags tell the framework which HCL attribute
// each Go field maps to. types.String (not plain string) can also hold null and unknown.
type scopeModel struct {
	ScopeID       types.String `tfsdk:"scope_id"`
	Name          types.String `tfsdk:"name"`
	Description   types.String `tfsdk:"description"`
	StartRange    types.String `tfsdk:"start_range"`
	EndRange      types.String `tfsdk:"end_range"`
	SubnetMask    types.String `tfsdk:"subnet_mask"`
	State         types.String `tfsdk:"state"`
	LeaseDuration types.String `tfsdk:"lease_duration"`
	Type          types.String `tfsdk:"type"`
}

// newScopeModel converts a scope returned by the dhcp client into the Terraform model, wrapping
// each plain Go string with types.StringValue (a known, non-null value).
func newScopeModel(s dhcp.Scope) scopeModel {
	return scopeModel{
		ScopeID:       types.StringValue(s.ScopeID),
		Name:          types.StringValue(s.Name),
		Description:   types.StringValue(s.Description),
		StartRange:    types.StringValue(s.StartRange),
		EndRange:      types.StringValue(s.EndRange),
		SubnetMask:    types.StringValue(s.SubnetMask),
		State:         types.StringValue(s.State),
		LeaseDuration: types.StringValue(s.LeaseDuration),
		Type:          types.StringValue(s.Type),
	}
}

// scopeAttributes returns the computed scope attributes; scope_id is added
// by the caller because its role differs.
//
// In windowsddi_dhcp_scope, scope_id is an input (Required); in windowsddi_dhcp_scopes it is an output
// (Computed). Everything else is Computed in both: set by the provider, never by the user.
// A fresh map is built on each call so callers can add scope_id without affecting each other.
func scopeAttributes() map[string]schema.Attribute {
	// c is a small local helper (a function stored in a variable) that builds a computed
	// string attribute, to avoid repeating the same three fields eight times.
	c := func(desc string) schema.Attribute {
		return schema.StringAttribute{MarkdownDescription: desc, Computed: true}
	}
	return map[string]schema.Attribute{
		"name":           c("Scope name."),
		"description":    c("Scope description."),
		"start_range":    c("First address of the range."),
		"end_range":      c("Last address of the range."),
		"subnet_mask":    c("Subnet mask."),
		"state":          c("Scope state as reported by the server (`Active` or `Inactive`)."),
		"lease_duration": c("Lease duration as `d.hh:mm:ss`."),
		"type":           c("Clients served: `Dhcp`, `Bootp` or `Both`."),
	}
}
