package dhcpdatasources

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dhcp"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/normalize"
)

// Compile-time check that *reservationDataSource satisfies the data source interface.
//
// Go note: `var _ Interface = &Type{}` makes the build fail if a required method is missing.
var _ datasource.DataSourceWithConfigure = &reservationDataSource{}

// NewReservation returns the windowsddi_dhcp_reservation data source.
// It is listed in All and called by the framework each time it needs a new instance.
func NewReservation() datasource.DataSource { return &reservationDataSource{} }

// reservationDataSource implements windowsddi_dhcp_reservation: find one reservation in a scope,
// either by reserved IP address or by client ID (MAC address).
type reservationDataSource struct{ client *dhcp.Client }

// reservationModel holds both the inputs (scope_id, plus ip_address or client_id) and the
// outputs (the remaining reservation properties).
//
// ClientID uses normalize.MACValue, a custom type that treats different spellings of the same
// MAC (aa-bb-cc-dd-ee-ff, AA:BB:CC:DD:EE:FF, aabb.ccdd.eeff, ...) as equal, so the plan does
// not show a diff just because of notation.
//
// Go note: the `tfsdk:"..."` struct tags map each Go field to its HCL attribute name.
type reservationModel struct {
	ScopeID     types.String       `tfsdk:"scope_id"`
	IPAddress   types.String       `tfsdk:"ip_address"`
	ClientID    normalize.MACValue `tfsdk:"client_id"`
	Name        types.String       `tfsdk:"name"`
	Description types.String       `tfsdk:"description"`
	Type        types.String       `tfsdk:"type"`
}

// Metadata sets the data source name to "windowsddi_dhcp_reservation".
//
// Go note: `(d *reservationDataSource)` is the method receiver, a pointer to the object.
func (d *reservationDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dhcp_reservation"
}

// Schema declares the attributes.
//
// ip_address and client_id are both Optional (the user may set one) and Computed (the
// provider fills the other one in from the server). The ExactlyOneOf validator on ip_address
// makes Terraform reject the config during validation unless exactly one of the two is set:
// neither, or both, is an error before any PowerShell runs.
func (d *reservationDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up one IPv4 reservation in a scope by IP address or client ID (`Get-DhcpServerv4Reservation`).",
		Attributes: map[string]schema.Attribute{
			"scope_id": schema.StringAttribute{
				MarkdownDescription: "Scope to search.",
				Required:            true,
				Validators:          []validator.String{normalize.IPv4Validator()},
			},
			"ip_address": schema.StringAttribute{
				MarkdownDescription: "Reserved IPv4 address to look up. Exactly one of `ip_address` and `client_id` must be set.",
				Optional:            true,
				Computed:            true,
				Validators: []validator.String{
					normalize.IPv4Validator(),
					stringvalidator.ExactlyOneOf(path.MatchRoot("ip_address"), path.MatchRoot("client_id")),
				},
			},
			"client_id": schema.StringAttribute{
				MarkdownDescription: "Client ID (MAC address) to look up, in any common notation. Exactly one of `ip_address` and `client_id` must be set.",
				Optional:            true,
				Computed:            true,
				CustomType:          normalize.MACType{},
			},
			"name":        schema.StringAttribute{MarkdownDescription: "Reservation name.", Computed: true},
			"description": schema.StringAttribute{MarkdownDescription: "Reservation description.", Computed: true},
			"type":        schema.StringAttribute{MarkdownDescription: "`Dhcp`, `Bootp` or `Both`.", Computed: true},
		},
	}
}

// Configure receives the shared *dhcp.Client created by the provider.
func (d *reservationDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

// Read finds the reservation and stores it in state.
//
// Why list then filter: rather than asking the server for one reservation by IP or by MAC
// (two different cmdlet parameter sets, with "not found" reported as a PowerShell error),
// it lists every reservation in the scope once and searches the list in Go. This gives one
// code path for both lookups, lets MACs be compared in canonical form whatever notation the
// server or the user used, and yields a clear "Reservation Not Found" message.
func (d *reservationDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	// Step 1: read the user's inputs.
	var cfg reservationModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Step 2: list every reservation in the scope (Get-DhcpServerv4Reservation).
	//
	// Go note: `:=` declares and assigns in one go; the two variables receive the function's
	// two return values (the list and an error).
	scopeID := cfg.ScopeID.ValueString()
	list, err := d.client.ListReservations(ctx, scopeID)
	if err != nil {
		resp.Diagnostics.AddError("Unable to list reservations in scope "+scopeID, err.Error())
		return
	}

	// Step 3: search the list. found stays nil (no match) until a reservation matches.
	// key describes what was searched for, for the error message.
	//
	// Go note: `*dhcp.Reservation` is a pointer type. `&list[i]` takes the address of the
	// element inside the slice, so found points at it without copying.
	// `for i := range list` loops over the indexes 0..len-1.
	var found *dhcp.Reservation
	// key describes what we searched for, for the "not found" message.
	key := "client ID " + cfg.ClientID.ValueString()
	if !cfg.IPAddress.IsNull() {
		key = "IP address " + cfg.IPAddress.ValueString()
	}
	for i := range list {
		r := &list[i]
		if !cfg.IPAddress.IsNull() {
			// Lookup by IP address: plain string comparison (the validator already ensured
			// the input is a valid IPv4 address).
			if r.IPAddress == cfg.IPAddress.ValueString() {
				found = r
				break
			}
		} else {
			// Lookup by client ID: compare canonical forms so any MAC notation matches.
			// Server entries that are not valid MACs (err != nil) are simply skipped.
			if c, err := normalize.CanonicalMAC(r.ClientID); err == nil && c == cfg.ClientID.Canonical() {
				found = r
				break
			}
		}
	}

	// Step 4: no match is an error that names what was searched for.
	if found == nil {
		resp.Diagnostics.AddError("Reservation Not Found", fmt.Sprintf("No reservation with %s in scope %s.", key, scopeID))
		return
	}

	// Step 5: copy the server's values into the model and save it.
	m := reservationModel{
		ScopeID:     types.StringValue(found.ScopeID),
		IPAddress:   types.StringValue(found.IPAddress),
		ClientID:    normalize.NewMACValue(found.ClientID),
		Name:        types.StringValue(found.Name),
		Description: types.StringValue(found.Description),
		Type:        types.StringValue(found.Type),
	}
	// If the user searched by client_id, store their spelling (for example AA:BB:...) rather
	// than the server's (aa-bb-...). Terraform expects a configured value to come back
	// unchanged; otherwise it would report an inconsistent result.
	if !cfg.ClientID.IsNull() {
		m.ClientID = cfg.ClientID // keep the configured spelling
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
