// This file implements windowsddi_dhcp_exclusion_range: a range of addresses inside a scope
// that the DHCP server must never lease (typically static servers, printers, gateways).
//
// See scope.go for how the framework pieces (model struct, Schema, Configure, the CRUD
// methods, diagnostics, drift handling, ImportState) work. What is special here: there is
// no Set-DhcpServerv4ExclusionRange cmdlet, so every attribute forces replacement and
// Update does nothing useful. The range is identified entirely by its three attributes.
//
// Cmdlets used (through the dhcp package): Get-, Add- and Remove-DhcpServerv4ExclusionRange.

package dhcpresources

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dhcp"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/normalize"
)

// Compile-time checks that exclusionRangeResource supports Configure and import.
//
// Go note: assigning a value to `_` (the blank identifier) only makes the compiler check
// that the type implements each interface; nothing is stored.
var (
	_ resource.ResourceWithConfigure   = &exclusionRangeResource{}
	_ resource.ResourceWithImportState = &exclusionRangeResource{}
)

// NewExclusionRange returns the windowsddi_dhcp_exclusion_range resource.
//
// Go note: `&exclusionRangeResource{}` creates an empty struct and returns a pointer to it.
func NewExclusionRange() resource.Resource { return &exclusionRangeResource{} }

// exclusionRangeResource holds the shared DHCP client, set by Configure.
type exclusionRangeResource struct{ client *dhcp.Client }

// exclusionRangeModel mirrors one windowsddi_dhcp_exclusion_range block. Each `tfsdk:"..."`
// struct tag names the HCL attribute the field maps to (see scopeModel in scope.go for
// why fields are types.String rather than plain Go strings).
type exclusionRangeModel struct {
	ID         types.String `tfsdk:"id"`
	ScopeID    types.String `tfsdk:"scope_id"`
	StartRange types.String `tfsdk:"start_range"`
	EndRange   types.String `tfsdk:"end_range"`
}

// Metadata sets the type name to "windowsddi_dhcp_exclusion_range".
//
// Go note: `(r *exclusionRangeResource)` before the name makes this a method of that type.
func (r *exclusionRangeResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dhcp_exclusion_range"
}

// Schema defines the attributes. All three user attributes are Required and RequiresReplace
// because exclusion ranges cannot be modified on the server: changing start_range, for
// example, shows "forces replacement" and Terraform removes the old range and adds a new
// one. id uses UseStateForUnknown so it does not show as "(known after apply)" needlessly.
func (r *exclusionRangeResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	// Shared plan modifier and validator lists, reused by the three attributes below.
	//
	// Go note: `:=` declares a local variable and infers its type from the value.
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	ipv4 := []validator.String{normalize.IPv4Validator()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an IPv4 exclusion range within a scope (`Add-DhcpServerv4ExclusionRange`, `Remove-DhcpServerv4ExclusionRange`). There is no update: every change replaces the range.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "`<scope_id>/<start_range>-<end_range>`, for example `10.1.2.0/10.1.2.1-10.1.2.20`.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"scope_id": schema.StringAttribute{
				MarkdownDescription: "Scope the exclusion range belongs to.",
				Required:            true,
				Validators:          ipv4,
				PlanModifiers:       replace,
			},
			"start_range": schema.StringAttribute{
				MarkdownDescription: "First excluded IPv4 address.",
				Required:            true,
				Validators:          ipv4,
				PlanModifiers:       replace,
			},
			"end_range": schema.StringAttribute{
				MarkdownDescription: "Last excluded IPv4 address.",
				Required:            true,
				Validators:          ipv4,
				PlanModifiers:       replace,
			},
		},
	}
}

// Configure stores the provider's shared *dhcp.Client (see clientFrom in common.go).
func (r *exclusionRangeResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

// key converts the model into the dhcp.ExclusionRange struct, which serves both as the
// input for Add and as the lookup key for Get and Remove (a range has no other properties).
func (m *exclusionRangeModel) key() dhcp.ExclusionRange {
	return dhcp.ExclusionRange{ScopeID: m.ScopeID.ValueString(), StartRange: m.StartRange.ValueString(), EndRange: m.EndRange.ValueString()}
}

// apply copies the server's exclusion range into the model and rebuilds id in the
// "<scope_id>/<start>-<end>" format.
//
// Go note: fmt.Sprintf fills the %s placeholders in order, like PowerShell's -f operator.
func (m *exclusionRangeModel) apply(e *dhcp.ExclusionRange) {
	m.ID = types.StringValue(fmt.Sprintf("%s/%s-%s", e.ScopeID, e.StartRange, e.EndRange))
	m.ScopeID = types.StringValue(e.ScopeID)
	m.StartRange = types.StringValue(e.StartRange)
	m.EndRange = types.StringValue(e.EndRange)
}

// Create adds the range (Add-DhcpServerv4ExclusionRange) and saves it as state. Same steps
// as scopeResource.Create in scope.go.
func (r *exclusionRangeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	// Read the plan.
	//
	// Go note: `&plan` passes a pointer so Get can fill the struct; `...` spreads the
	// returned diagnostics list into Append's variadic parameter.
	var plan exclusionRangeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Add it on the server.
	//
	// Go note: the call returns two values (result, error); `if err != nil` means it failed.
	e, err := r.client.AddExclusionRange(ctx, plan.key())
	if err != nil {
		resp.Diagnostics.AddError(errSummary("create", "exclusion range"), err.Error())
		return
	}
	// Save the server's view as state.
	plan.apply(e)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read checks that the range still exists. The dhcp layer lists the scope's exclusion
// ranges and filters for this one; if it is gone, the resource is removed from state
// (drift) so the next plan recreates it.
func (r *exclusionRangeResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state exclusionRangeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	e, err := r.client.GetExclusionRange(ctx, state.key())
	// Deleted outside Terraform: drop from state instead of erroring.
	if dhcp.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError(errSummary("read", "exclusion range"), err.Error())
		return
	}
	state.apply(e)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is never called with a change (every attribute requires
// replacement); it only exists to satisfy the interface.
//
// The framework's resource.Resource interface requires an Update method, so this one just
// copies the plan into state without contacting the server.
func (r *exclusionRangeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan exclusionRangeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete removes the range (Remove-DhcpServerv4ExclusionRange). An already-missing range
// counts as deleted.
func (r *exclusionRangeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state exclusionRangeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Go note: `if err := ...; err != nil` runs the call and checks the error in one line.
	if err := r.client.RemoveExclusionRange(ctx, state.key()); err != nil && !dhcp.IsNotFound(err) {
		resp.Diagnostics.AddError(errSummary("delete", "exclusion range"), err.Error())
	}
}

// ImportState accepts <scope_id>/<start_range>-<end_range>.
//
// For example 10.1.2.0/10.1.2.1-10.1.2.20. All three parts must be IPv4 addresses. The
// parts are written to state and Terraform then calls Read to confirm the range exists.
func (r *exclusionRangeResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Split "scope/start-end" into its three parts.
	//
	// Go note: strings.Cut splits at the first separator and returns (before, after, found).
	scopeID, rng, ok1 := strings.Cut(req.ID, "/")
	start, end, ok2 := strings.Cut(rng, "-")
	valid := ok1 && ok2
	// Check each part is an IPv4 address.
	//
	// Go note: `for _, s := range list` loops over a slice (like foreach); `_` drops the
	// index and s is each element in turn.
	for _, s := range []string{scopeID, start, end} {
		if _, err := normalize.ParseIPv4(s); err != nil {
			valid = false
		}
	}
	if !valid {
		resp.Diagnostics.AddError("Invalid Import ID", fmt.Sprintf("expected <scope_id>/<start_range>-<end_range> such as 10.1.2.0/10.1.2.1-10.1.2.20, got %q", req.ID))
		return
	}
	// Seed state with the key attributes; Read fills in the rest.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("scope_id"), scopeID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("start_range"), start)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("end_range"), end)...)
}
