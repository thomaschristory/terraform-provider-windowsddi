// This file implements windowsddi_dhcp_reservation, an IPv4 reservation (a fixed IP for one
// client ID/MAC inside a scope).
//
// See scope.go for how the framework pieces (model struct, Schema, Configure, the CRUD
// methods, diagnostics, drift handling, ImportState) work; the comments here focus on what
// is different for reservations: a composite ID, MAC address normalisation, and a name the
// server can pick on its own.
//
// Cmdlets used (through the dhcp package): Get-, Add-, Set- and
// Remove-DhcpServerv4Reservation.

package dhcpresources

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dhcp"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/normalize"
)

// Compile-time checks that reservationResource supports Configure and import.
//
// Go note: assigning to `_` (the blank identifier) makes the compiler verify that the type
// has the methods of each interface, without keeping any value. No ModifyPlan here: unlike
// a scope, a reservation has nothing to derive at plan time.
var (
	_ resource.ResourceWithConfigure   = &reservationResource{}
	_ resource.ResourceWithImportState = &reservationResource{}
)

// NewReservation returns the windowsddi_dhcp_reservation resource.
//
// Go note: `&reservationResource{}` creates an empty struct and returns a pointer to it.
func NewReservation() resource.Resource { return &reservationResource{} }

// reservationResource holds the shared DHCP client, set by Configure.
type reservationResource struct{ client *dhcp.Client }

// reservationModel mirrors one windowsddi_dhcp_reservation block; each `tfsdk:"..."` tag
// names the HCL attribute the field maps to (see scopeModel in scope.go).
//
// ClientID uses the custom normalize.MACValue type. It treats aa:bb:cc:dd:ee:ff,
// AA-BB-CC-DD-EE-FF, aabbccddeeff and aabb.ccdd.eeff as equal ("semantic equality"). When
// the server returns a different spelling than the one in HCL, the framework sees the two
// as equal and keeps the user's spelling in state, so the plan shows no diff.
//
// Go note: the backquoted `tfsdk:"..."` text is a struct tag, metadata the framework reads.
type reservationModel struct {
	ID          types.String       `tfsdk:"id"`
	ScopeID     types.String       `tfsdk:"scope_id"`
	IPAddress   types.String       `tfsdk:"ip_address"`
	ClientID    normalize.MACValue `tfsdk:"client_id"`
	Name        types.String       `tfsdk:"name"`
	Description types.String       `tfsdk:"description"`
	Type        types.String       `tfsdk:"type"`
}

// Metadata sets the type name to "windowsddi_dhcp_reservation".
//
// Go note: `(r *reservationResource)` before the name makes this a method of that type.
func (r *reservationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dhcp_reservation"
}

// Schema defines the attributes of windowsddi_dhcp_reservation (see scope.go for what Required,
// Optional, Computed, Default, Validators and PlanModifiers mean).
//
// Differences from the scope worth knowing:
//   - scope_id and ip_address use RequiresReplace: Set-DhcpServerv4Reservation identifies
//     the reservation by its IP and cannot move it, so changing either one shows
//     "forces replacement" in the plan.
//   - client_id has no RequiresReplace: Set-DhcpServerv4Reservation -ClientId updates it in
//     place.
//   - UseStateForUnknown (on id and name): a Computed attribute is normally shown as
//     "(known after apply)" on every update. This modifier tells Terraform to reuse the
//     value already in state instead, which keeps plans quiet. It is safe because neither
//     value changes during an in-place update (id only changes on replacement).
//   - name is Optional+Computed without a Default: if the user omits it, the server picks a
//     name (Add-DhcpServerv4Reservation without -Name) and Terraform records that name.
//   - type defaults to Both here (the cmdlet's default for reservations), unlike Dhcp for
//     scopes.
func (r *reservationResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages an IPv4 reservation (`Add-DhcpServerv4Reservation`, `Set-DhcpServerv4Reservation`, `Remove-DhcpServerv4Reservation`).",
		// Go note: a map literal (attribute name -> definition), like a PowerShell hashtable.
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "`<scope_id>/<ip_address>`, for example `10.1.2.0/10.1.2.50`.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"scope_id": schema.StringAttribute{
				MarkdownDescription: "Scope the reservation belongs to. Changing it replaces the reservation.",
				Required:            true,
				Validators:          []validator.String{normalize.IPv4Validator()},
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"ip_address": schema.StringAttribute{
				MarkdownDescription: "Reserved IPv4 address, unique on the server. Changing it replaces the reservation.",
				Required:            true,
				Validators:          []validator.String{normalize.IPv4Validator()},
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			// MACType also validates the format during plan (an invalid MAC fails early).
			"client_id": schema.StringAttribute{
				MarkdownDescription: "Client identifier, usually the MAC address. Accepts `aa:bb:cc:dd:ee:ff`, `aa-bb-cc-dd-ee-ff`, `aabbccddeeff` and `aabb.ccdd.eeff` in any case; equivalent spellings never show a diff. Updated in place.",
				Required:            true,
				CustomType:          normalize.MACType{},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Reservation name. When unset, the server chooses one.",
				Optional:            true,
				Computed:            true,
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"description": schema.StringAttribute{
				MarkdownDescription: "Reservation description. Defaults to an empty string.",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(""),
			},
			// Go note: `clientTypes...` spreads the slice into separate arguments.
			"type": schema.StringAttribute{
				MarkdownDescription: "Clients allowed to use the reservation: `Dhcp`, `Bootp` or `Both` (default).",
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString(typeBoth),
				Validators:          []validator.String{stringvalidator.OneOf(clientTypes...)},
			},
		},
	}
}

// Configure stores the provider's shared *dhcp.Client (see clientFrom in common.go).
func (r *reservationResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

// reservationID builds the Terraform id "<scope_id>/<ip_address>", the same format that
// ImportState accepts.
func reservationID(scopeID, ip string) string { return scopeID + "/" + ip }

// input converts the model into the dhcp package's ReservationInput.
//
// The MAC is sent in canonical form so the server always gets one consistent spelling. An
// unset (null) or not-yet-known name is sent as "" which the dhcp layer treats as "let the
// server choose" (no -Name).
func (m *reservationModel) input() dhcp.ReservationInput {
	// Go note: `:=` declares a new variable and infers its type (string here).
	name := ""
	if !m.Name.IsUnknown() && !m.Name.IsNull() {
		name = m.Name.ValueString()
	}
	return dhcp.ReservationInput{
		ScopeID:     m.ScopeID.ValueString(),
		IPAddress:   m.IPAddress.ValueString(),
		ClientID:    m.ClientID.Canonical(),
		Name:        name,
		Description: m.Description.ValueString(),
		Type:        m.Type.ValueString(),
	}
}

// apply copies the server's reservation into the model. client_id is stored as the server
// spelled it; because MACValue compares semantically, the framework keeps the user's
// spelling when the two are equivalent, so this never causes a diff. Type goes through
// EnumOf to get the canonical casing (see common.go).
func (m *reservationModel) apply(res *dhcp.Reservation) {
	m.ID = types.StringValue(reservationID(res.ScopeID, res.IPAddress))
	m.ScopeID = types.StringValue(res.ScopeID)
	m.IPAddress = types.StringValue(res.IPAddress)
	m.ClientID = normalize.NewMACValue(res.ClientID)
	m.Name = types.StringValue(res.Name)
	m.Description = types.StringValue(res.Description)
	m.Type = types.StringValue(normalize.EnumOf(res.Type, clientTypes...))
}

// Create adds the reservation (Add-DhcpServerv4Reservation) and saves the server's answer
// as state. Same steps as scopeResource.Create in scope.go.
func (r *reservationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	// Read the plan into the model.
	//
	// Go note: `&plan` passes a pointer so Get can fill the struct; `...` spreads the
	// returned diagnostics list into Append.
	var plan reservationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Create on the server.
	//
	// Go note: AddReservation returns two values (result, error); `if err != nil` checks
	// for failure, Go's replacement for try/catch.
	res, err := r.client.AddReservation(ctx, plan.input())
	if err != nil {
		resp.Diagnostics.AddError(errSummary("create", "reservation "+plan.IPAddress.ValueString()), err.Error())
		return
	}
	// Save what the server holds (including a server-chosen name) as state.
	plan.apply(res)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read refreshes the reservation from the server, looked up by scope and IP. A missing
// reservation is removed from state (drift) rather than reported as an error.
func (r *reservationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state reservationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	res, err := r.client.GetReservation(ctx, state.ScopeID.ValueString(), state.IPAddress.ValueString())
	// Deleted outside Terraform: forget it so the next plan recreates it.
	if dhcp.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError(errSummary("read", "reservation "+state.IPAddress.ValueString()), err.Error())
		return
	}
	state.apply(res)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update changes client_id, name, description or type in place with
// Set-DhcpServerv4Reservation (scope_id and ip_address changes are replacements instead).
func (r *reservationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan reservationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	res, err := r.client.SetReservation(ctx, plan.input())
	if err != nil {
		resp.Diagnostics.AddError(errSummary("update", "reservation "+plan.IPAddress.ValueString()), err.Error())
		return
	}
	plan.apply(res)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete removes the reservation. Only the IP is passed because reserved IPs are unique
// across the whole server. An already-missing reservation counts as deleted.
func (r *reservationResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state reservationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Go note: `if err := ...; err != nil` runs the call and tests the error in one line;
	// err only exists inside this if statement.
	if err := r.client.RemoveReservation(ctx, state.IPAddress.ValueString()); err != nil && !dhcp.IsNotFound(err) {
		resp.Diagnostics.AddError(errSummary("delete", "reservation "+state.IPAddress.ValueString()), err.Error())
	}
}

// ImportState accepts <scope_id>/<ip_address>, for example 10.1.2.0/10.1.2.50.
//
// It splits the ID, checks both halves are IPv4 addresses, and writes id, scope_id and
// ip_address into state. Terraform then calls Read to fill in client_id, name and the rest.
func (r *reservationResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Go note: strings.Cut splits "a/b" at the first "/" into "a" and "b"; ok is false
	// when there is no "/" at all. `_` discards the parsed IP values we do not need.
	scopeID, ip, ok := strings.Cut(req.ID, "/")
	_, errS := normalize.ParseIPv4(scopeID)
	_, errI := normalize.ParseIPv4(ip)
	if !ok || errS != nil || errI != nil {
		// Go note: fmt.Sprintf formats a string; %q prints the value in double quotes.
		resp.Diagnostics.AddError("Invalid Import ID", fmt.Sprintf("expected <scope_id>/<ip_address> such as 10.1.2.0/10.1.2.50, got %q", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("scope_id"), scopeID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("ip_address"), ip)...)
}
