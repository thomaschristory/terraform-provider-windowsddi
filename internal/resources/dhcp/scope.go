// This file implements windowsddi_dhcp_scope, an IPv4 DHCP scope. It is the reference resource
// of the package: the comments here explain the terraform-plugin-framework concepts in
// detail, and the other resource files refer back to it.
//
// Cmdlets used (through the dhcp package, never directly): Get-DhcpServerv4Scope,
// Add-DhcpServerv4Scope, Set-DhcpServerv4Scope, Remove-DhcpServerv4Scope.

package dhcpresources

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dhcp"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/normalize"
)

// defaultLeaseDuration is the lease_duration used when the user does not set one. It is
// the same 8 days that the DHCP MMC console and Add-DhcpServerv4Scope default to, written
// in the TimeSpan "d.hh:mm:ss" form that the server reports back.
const defaultLeaseDuration = "8.00:00:00"

// Compile-time interface checks.
//
// Go note: Go has no "implements" keyword; a type satisfies an interface simply by having
// the right methods. These lines assign a scopeResource pointer to a throwaway variable
// (`_`) of each interface type, so the build fails right here if a required method is
// missing or has the wrong signature. They have no runtime effect.
//
// The framework looks for these optional interfaces at runtime: ResourceWithConfigure
// means "has a Configure method", ResourceWithImportState means "supports terraform
// import", ResourceWithModifyPlan means "wants to adjust the plan".
var (
	_ resource.ResourceWithConfigure   = &scopeResource{}
	_ resource.ResourceWithImportState = &scopeResource{}
	_ resource.ResourceWithModifyPlan  = &scopeResource{}
)

// NewScope returns the windowsddi_dhcp_scope resource.
//
// Listed in All() (common.go). The framework calls it to get a new, empty instance; the
// client is filled in later by Configure.
//
// Go note: `&scopeResource{}` creates a new scopeResource with all fields at their zero
// value (client is nil) and returns a pointer to it. The `{ return ... }` on one line is
// just a short function body.
func NewScope() resource.Resource { return &scopeResource{} }

// scopeResource is the resource implementation. It holds no per-scope data (that lives in
// Terraform state); it only keeps the shared DHCP client so its methods can reach the
// server.
//
// Go note: `struct{ ... }` defines a record type with named fields, like a PowerShell
// [pscustomobject] with a fixed shape.
type scopeResource struct{ client *dhcp.Client }

// scopeModel is the Go mirror of one windowsddi_dhcp_scope block. The framework copies the
// plan, the state or the config into this struct (Get) and back out (Set).
//
// Each field's `tfsdk:"..."` tag names the HCL attribute it maps to; the tags must match
// the keys of the schema in Schema() exactly, or the framework reports an error.
//
// Go note: the text in backquotes after a field is a "struct tag": metadata that libraries
// read at runtime. Here the framework uses it to match fields to attribute names.
//
// Fields use types.String / types.Bool rather than plain Go string / bool because a
// Terraform value has two extra states a Go string cannot express:
//   - null: the attribute is not set at all (omitted in HCL);
//   - unknown: the value will only be known after apply ("(known after apply)" in a plan),
//     for example because it depends on another resource not created yet.
//
// .ValueString() / .ValueBool() return the plain Go value (empty string / false for null
// or unknown); .IsNull() / .IsUnknown() test the special states.
//
// LeaseDuration uses a custom type (normalize.DurationValue) that knows "8.00:00:00" and
// "192h" are the same duration, so changing the spelling in HCL
// never shows a diff ("semantic equality").
type scopeModel struct {
	ID            types.String            `tfsdk:"id"`
	ScopeID       types.String            `tfsdk:"scope_id"`
	Name          types.String            `tfsdk:"name"`
	Description   types.String            `tfsdk:"description"`
	StartRange    types.String            `tfsdk:"start_range"`
	EndRange      types.String            `tfsdk:"end_range"`
	SubnetMask    types.String            `tfsdk:"subnet_mask"`
	State         types.String            `tfsdk:"state"`
	LeaseDuration normalize.DurationValue `tfsdk:"lease_duration"`
	Type          types.String            `tfsdk:"type"`
	ForceDestroy  types.Bool              `tfsdk:"force_destroy"`
}

// Metadata tells Terraform the resource type name. ProviderTypeName is "windowsddi", so
// this resource is "windowsddi_dhcp_scope".
//
// Go note: `func (r *scopeResource) Metadata(...)` is a method: a function attached to the
// scopeResource type, called as r.Metadata(...). `r` is the receiver (like $this).
// `_` as a parameter name means "this argument is not used".
//
// Go note: resp is a pointer (`*resource.MetadataResponse`). Writing resp.TypeName changes
// the framework's response object itself, which is how results are returned to it.
func (r *scopeResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dhcp_scope"
}

// Schema describes every attribute of windowsddi_dhcp_scope: its type, whether the user must
// set it, defaults, validation and plan behaviour. Terraform uses it to validate HCL and to
// compute plans; tfplugindocs turns the MarkdownDescription texts into the Registry docs.
//
// Attribute flags, in Terraform user terms:
//   - Required: the user must set it in HCL.
//   - Optional: the user may set it.
//   - Computed: the provider may supply the value (shown as "(known after apply)" or filled
//     by a Default). Optional+Computed with a Default means "if omitted, use the default".
//   - Default: the value the plan uses when the attribute is omitted. Because the default
//     is in the plan, an omitted attribute still gets enforced against server drift.
//   - Validators: checked during `terraform validate`/plan, before anything touches the
//     server, so bad input fails early with a clear message.
//   - PlanModifiers: adjust the planned value. RequiresReplace makes a change to this
//     attribute show "# forces replacement" in the plan: Terraform deletes and recreates
//     the scope instead of updating it in place.
func (r *scopeResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an IPv4 scope (`Add-DhcpServerv4Scope`, `Set-DhcpServerv4Scope`, `Remove-DhcpServerv4Scope`).\n\n" +
			"If the scope is part of a failover relationship, manage it on one partner only and let replication run (or run `Invoke-DhcpServerv4FailoverReplication` afterwards).",
		// Go note: `map[string]schema.Attribute{...}` is a map (a hashtable) from attribute
		// name to its definition.
		Attributes: map[string]schema.Attribute{
			// id is the conventional Terraform identifier; here it is just the scope ID.
			"id": schema.StringAttribute{
				MarkdownDescription: "Same as `scope_id`.",
				Computed:            true,
			},
			// scope_id is Computed only: the user never types it. ModifyPlan below derives it
			// from start_range and subnet_mask so that it is already known in the plan.
			"scope_id": schema.StringAttribute{
				MarkdownDescription: "Scope ID: the network address, derived from `start_range` and `subnet_mask`. Known at plan time.",
				Computed:            true,
			},
			// Go note: `[]validator.String{...}` below is a slice (list) holding one validator.
			"name": schema.StringAttribute{
				MarkdownDescription: "Scope name.",
				Required:            true,
				Validators:          []validator.String{stringvalidator.LengthBetween(1, 256)},
			},
			"description": schema.StringAttribute{
				MarkdownDescription: "Scope description. Defaults to an empty string.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(""),
			},
			// No RequiresReplace here: whether a start_range change needs a new scope depends
			// on whether the network address changes, which ModifyPlan decides.
			"start_range": schema.StringAttribute{
				MarkdownDescription: "First IPv4 address of the range the server leases from. Changing it within the same network updates the scope in place; moving it to another network replaces the scope.",
				Required:            true,
				Validators:          []validator.String{normalize.IPv4Validator()},
			},
			"end_range": schema.StringAttribute{
				MarkdownDescription: "Last IPv4 address of the range the server leases from.",
				Required:            true,
				Validators:          []validator.String{normalize.IPv4Validator()},
			},
			// Set-DhcpServerv4Scope has no -SubnetMask parameter, so any change must recreate.
			"subnet_mask": schema.StringAttribute{
				MarkdownDescription: "Subnet mask, for example `255.255.255.0`. Changing it replaces the scope (`Set-DhcpServerv4Scope` cannot change it).",
				Required:            true,
				Validators:          []validator.String{normalize.MaskValidator()},
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			// Go note: `scopeStates...` spreads the slice into separate arguments, like
			// passing each element of a PowerShell array as its own parameter value.
			"state": schema.StringAttribute{
				MarkdownDescription: "`Active` (default) or `InActive`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(stateActive),
				Validators:          []validator.String{stringvalidator.OneOf(scopeStates...)},
			},
			// CustomType swaps the plain string for normalize.DurationType, which gives the
			// "equivalent spellings never diff" behaviour described on scopeModel.
			"lease_duration": schema.StringAttribute{
				MarkdownDescription: "Lease duration as `d.hh:mm:ss` (for example `8.00:00:00`, the default) or a Go duration (for example `8h`, `90m`, `192h`). Equivalent spellings never show a diff.",
				Optional:            true,
				Computed:            true,
				CustomType:          normalize.DurationType{},
				Default:             stringdefault.StaticString(defaultLeaseDuration),
			},
			"type": schema.StringAttribute{
				MarkdownDescription: "Clients served: `Dhcp` (default), `Bootp` or `Both`.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(typeDhcp),
				Validators:          []validator.String{stringvalidator.OneOf(clientTypes...)},
			},
			// force_destroy is a Terraform-side safety switch, not a server property. Without
			// it, Remove-DhcpServerv4Scope refuses to delete a scope with active leases, which
			// protects production scopes from an accidental `terraform destroy`. Setting it
			// to true passes -Force. It must be applied (stored in state) before the destroy,
			// because Delete only sees the state, not the config.
			"force_destroy": schema.BoolAttribute{
				MarkdownDescription: "When `true`, destroying the resource removes the scope even if it has active leases (`-Force`). Defaults to `false`. Terraform only, not read from the server.",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
			},
		},
	}
}

// Configure receives the *dhcp.Client that the provider built from its provider block
// (host, credentials, transport) and stores it on the resource for the CRUD methods.
//
// Go note: `&resp.Diagnostics` passes a pointer to the diagnostics list so clientFrom can
// append errors to it directly.
func (r *scopeResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

// ModifyPlan derives scope_id from start_range and subnet_mask so it is
// known at plan time, and forces replacement when the network changes.
//
// Terraform calls ModifyPlan during `terraform plan`, after defaults and attribute plan
// modifiers have run. Two things happen here:
//
//  1. scope_id (and id) are computed locally from start_range and subnet_mask, for
//     example 10.1.2.50/255.255.255.0 -> 10.1.2.0. Without this they would show as
//     "(known after apply)", and other resources referencing scope_id (reservations,
//     option values) could not be planned precisely.
//  2. If the computed network differs from the one in state (start_range moved to another
//     subnet), the plan is marked "forces replacement" on start_range. A DHCP scope ID
//     cannot be changed by Set-DhcpServerv4Scope, so the scope has to be recreated. A
//     start_range change inside the same network stays an in-place update.
func (r *scopeResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	// A null plan means the resource is being destroyed: nothing to compute.
	if req.Plan.Raw.IsNull() {
		return
	}
	// Load the planned values into a scopeModel.
	//
	// Go note: `var plan scopeModel` declares an empty struct. `&plan` passes its address so
	// Get can fill it in place. Get returns diagnostics; `...` spreads that list into
	// Append's variadic (any number of arguments) parameter.
	var plan scopeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	// Stop if loading failed, or if the inputs are not known yet (for example they come
	// from another resource that will only be created during apply).
	if resp.Diagnostics.HasError() || plan.StartRange.IsUnknown() || plan.SubnetMask.IsUnknown() {
		return
	}
	// Go note: functions can return several values. Here NetworkAddress returns the result
	// and an error; `if err != nil` is Go's standard "did it fail?" check (no try/catch).
	// `:=` declares the new variables network and err, inferring their types.
	network, err := normalize.NetworkAddress(plan.StartRange.ValueString(), plan.SubnetMask.ValueString())
	if err != nil {
		return // attribute validators report it
	}
	// Write the derived network address into the plan for both scope_id and id.
	// path.Root("scope_id") addresses a top-level attribute by name.
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("scope_id"), network)...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("id"), network)...)

	// A null state means this is a create: there is no old network to compare with.
	if req.State.Raw.IsNull() {
		return
	}
	// Compare with the network address stored in state and force replacement if it moved.
	var state scopeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if !state.ScopeID.IsNull() && state.ScopeID.ValueString() != network {
		// Go note: append returns a new slice with the element added; the result must be
		// assigned back.
		resp.RequiresReplace = append(resp.RequiresReplace, path.Root("start_range"))
	}
}

// input converts the Terraform model into the dhcp package's ScopeInput, the plain-Go
// struct the client turns into the JSON parameters for Add/Set-DhcpServerv4Scope. It
// recomputes the network address (the scope ID) and parses lease_duration into a Go
// time.Duration, which the dhcp layer sends to PowerShell as whole seconds.
//
// Go note: the receiver `m *scopeModel` makes this a method on the model. It returns two
// values, the input and an error; on failure it returns an empty ScopeInput{} plus the
// error, and callers check the error first.
func (m *scopeModel) input() (dhcp.ScopeInput, error) {
	network, err := normalize.NetworkAddress(m.StartRange.ValueString(), m.SubnetMask.ValueString())
	if err != nil {
		return dhcp.ScopeInput{}, err
	}
	d, err := normalize.ParseDuration(m.LeaseDuration.ValueString())
	if err != nil {
		return dhcp.ScopeInput{}, err
	}
	return dhcp.ScopeInput{
		ScopeID:       network,
		Name:          m.Name.ValueString(),
		Description:   m.Description.ValueString(),
		StartRange:    m.StartRange.ValueString(),
		EndRange:      m.EndRange.ValueString(),
		SubnetMask:    m.SubnetMask.ValueString(),
		State:         m.State.ValueString(),
		Type:          m.Type.ValueString(),
		LeaseDuration: d,
	}, nil
}

// apply copies what the server returned (a *dhcp.Scope) into the model, so state reflects
// the real server values and Terraform can detect drift on the next plan.
//
// Two normalisations keep plans clean:
//   - State and Type go through normalize.EnumOf, which maps the server's spelling onto the
//     canonical enum value case-insensitively (the server may say "Inactive" while the
//     schema and the cmdlet use "InActive").
//   - force_destroy is not a server property, so it is left as the user set it; it is only
//     defaulted to false when it has no value yet (for example right after import).
//
// types.StringValue(...) wraps a plain Go string as a known (not null, not unknown)
// Terraform value.
func (m *scopeModel) apply(s *dhcp.Scope) {
	m.ID = types.StringValue(s.ScopeID)
	m.ScopeID = types.StringValue(s.ScopeID)
	m.Name = types.StringValue(s.Name)
	m.Description = types.StringValue(s.Description)
	m.StartRange = types.StringValue(s.StartRange)
	m.EndRange = types.StringValue(s.EndRange)
	m.SubnetMask = types.StringValue(s.SubnetMask)
	m.State = types.StringValue(normalize.EnumOf(s.State, scopeStates...))
	m.Type = types.StringValue(normalize.EnumOf(s.Type, clientTypes...))
	m.LeaseDuration = normalize.NewDurationValue(s.LeaseDuration)
	if m.ForceDestroy.IsNull() || m.ForceDestroy.IsUnknown() {
		m.ForceDestroy = types.BoolValue(false)
	}
}

// Create runs on `terraform apply` for a scope that is not in state yet. The pattern, shared
// by every resource in this package:
//
//  1. req.Plan.Get: read the planned values (config plus defaults) into the model.
//  2. Convert the model to a dhcp input struct and call the client (Add-DhcpServerv4Scope).
//  3. Copy the server's answer back into the model with apply.
//  4. resp.State.Set: save the model as the new Terraform state.
//
// Any error is reported with resp.Diagnostics.AddError(summary, detail): that is how
// failures reach the user (as "Error: Unable to create scope ..." in the CLI output). The
// detail carries the cmdlet name and PowerShell message from the dhcp package. If Create
// returns without setting state, Terraform considers the resource not created.
func (r *scopeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	// 1. Read the plan.
	var plan scopeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// 2. Build the cmdlet input and create the scope on the server.
	in, err := plan.input()
	if err != nil {
		resp.Diagnostics.AddError(errSummary("create", "scope"), err.Error())
		return
	}
	s, err := r.client.AddScope(ctx, in)
	if err != nil {
		resp.Diagnostics.AddError(errSummary("create", "scope "+in.ScopeID), err.Error())
		return
	}
	// 3 and 4. Record what the server actually holds as the new state.
	plan.apply(s)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read refreshes state from the server. Terraform calls it at the start of every plan
// (and after import), then compares the refreshed state with the config to compute a diff.
//
// If the scope no longer exists (someone deleted it in the DHCP console), Read calls
// resp.State.RemoveResource instead of failing. Terraform then reports the resource as
// deleted outside of Terraform and plans to create it again: this is drift handling.
func (r *scopeResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	// Load current state to learn which scope to look up.
	var state scopeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	s, err := r.client.GetScope(ctx, state.ScopeID.ValueString())
	// Gone on the server: drop it from state (drift), not an error.
	if dhcp.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError(errSummary("read", "scope "+state.ScopeID.ValueString()), err.Error())
		return
	}
	// Overwrite state with the server's current values.
	state.apply(s)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update runs on apply when the plan changes attributes that can be changed in place
// (anything except subnet_mask, or a start_range that moves the network: those replace).
// It follows the same steps as Create but calls Set-DhcpServerv4Scope. The whole planned
// object is sent, not only the changed attributes.
func (r *scopeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// Read the plan (the desired new values).
	var plan scopeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Build the input and push it to the server.
	in, err := plan.input()
	if err != nil {
		resp.Diagnostics.AddError(errSummary("update", "scope"), err.Error())
		return
	}
	s, err := r.client.SetScope(ctx, in)
	if err != nil {
		resp.Diagnostics.AddError(errSummary("update", "scope "+in.ScopeID), err.Error())
		return
	}
	// Save the server's answer as the new state.
	plan.apply(s)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete removes the scope on `terraform destroy` or as the first step of a replacement.
// It only has the state (not the config), which is why force_destroy must already be in
// state. A scope that is already gone counts as successfully deleted. The framework removes
// the resource from state automatically when Delete returns without errors.
func (r *scopeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state scopeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Remove-DhcpServerv4Scope, with -Force only when force_destroy is true.
	err := r.client.RemoveScope(ctx, state.ScopeID.ValueString(), state.ForceDestroy.ValueBool())
	if err != nil && !dhcp.IsNotFound(err) {
		// Add a hint for the most common failure: the scope still has leases.
		detail := err.Error()
		if !state.ForceDestroy.ValueBool() {
			detail += "\n\nIf the scope still has active leases, set force_destroy = true and apply before destroying."
		}
		resp.Diagnostics.AddError(errSummary("delete", "scope "+state.ScopeID.ValueString()), detail)
	}
}

// ImportState accepts the scope ID, for example 10.1.2.0.
//
// Used by `terraform import windowsddi_dhcp_scope.x 10.1.2.0` or an `import { id = "10.1.2.0" }`
// block. It only validates the ID and writes the key attributes into state; Terraform then
// calls Read, which fetches everything else from the server. force_destroy is set to false
// because it cannot be read from the server.
//
// Go note: `if _, err := ...; err != nil` runs the call and the check in one statement. The
// `_` discards the parsed IP, which is not needed here; %q prints the value in quotes.
func (r *scopeResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if _, err := normalize.ParseIPv4(req.ID); err != nil {
		resp.Diagnostics.AddError("Invalid Import ID", fmt.Sprintf("expected a scope ID such as 10.1.2.0, got %q", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("scope_id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("force_destroy"), false)...)
}
