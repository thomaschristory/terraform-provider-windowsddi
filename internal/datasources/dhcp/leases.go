package dhcpdatasources

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dhcp"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/normalize"
)

// Compile-time check that *leasesDataSource satisfies the data source interface.
//
// Go note: `var _ Interface = &Type{}` makes the build fail if a required method is missing.
var _ datasource.DataSourceWithConfigure = &leasesDataSource{}

// NewLeases returns the windowsddi_dhcp_leases data source.
// It is listed in All and called by the framework each time it needs a new instance.
func NewLeases() datasource.DataSource { return &leasesDataSource{} }

// leasesDataSource implements windowsddi_dhcp_leases: list the leases of one scope. Leases are
// runtime data owned by the DHCP server, which is why they exist only as a data source and
// never as a resource.
type leasesDataSource struct{ client *dhcp.Client }

// leaseModel is one element of the `leases` list, copied from a Get-DhcpServerv4Lease result.
// All fields are strings; lease_expiry_time is already formatted as RFC 3339 by the dhcp
// package.
//
// Go note: the `tfsdk:"..."` struct tags map each Go field to its HCL attribute name.
type leaseModel struct {
	IPAddress       types.String `tfsdk:"ip_address"`
	ClientID        types.String `tfsdk:"client_id"`
	HostName        types.String `tfsdk:"host_name"`
	AddressState    types.String `tfsdk:"address_state"`
	LeaseExpiryTime types.String `tfsdk:"lease_expiry_time"`
	Description     types.String `tfsdk:"description"`
	ClientType      types.String `tfsdk:"client_type"`
}

// leasesModel is the whole data source: the inputs (scope_id, all_leases) and the computed
// list of leases.
//
// Go note: `[]leaseModel` is a slice (growable list); it maps to a Terraform list.
type leasesModel struct {
	ScopeID   types.String `tfsdk:"scope_id"`
	AllLeases types.Bool   `tfsdk:"all_leases"`
	Leases    []leaseModel `tfsdk:"leases"`
}

// Metadata sets the data source name to "windowsddi_dhcp_leases".
//
// Go note: `(d *leasesDataSource)` is the method receiver, a pointer to the object.
func (d *leasesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dhcp_leases"
}

// Schema declares scope_id (required input), all_leases (optional switch mapping to
// -AllLeases) and `leases`, a computed nested list (ListNestedAttribute) where every element
// is an object with the attributes of one lease.
func (d *leasesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	// c is a local helper (a function stored in a variable) that builds a computed string
	// attribute with the given description, to keep the nested attribute list short.
	c := func(desc string) schema.Attribute {
		return schema.StringAttribute{MarkdownDescription: desc, Computed: true}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the leases of a scope (`Get-DhcpServerv4Lease`). Leases are runtime data: the result changes as clients come and go.",
		Attributes: map[string]schema.Attribute{
			"scope_id": schema.StringAttribute{
				MarkdownDescription: "Scope to list leases for.",
				Required:            true,
				Validators:          []validator.String{normalize.IPv4Validator()},
			},
			"all_leases": schema.BoolAttribute{
				MarkdownDescription: "Also return offered, declined and expired leases (`-AllLeases`). Defaults to `false` (active leases only).",
				Optional:            true,
			},
			"leases": schema.ListNestedAttribute{
				MarkdownDescription: "Leases in the scope.",
				Computed:            true,
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"ip_address":        c("Leased IPv4 address."),
					"client_id":         c("Client ID (usually the MAC address), as `aa-bb-cc-dd-ee-ff`."),
					"host_name":         c("Host name reported by the client."),
					"address_state":     c("Lease state, for example `Active`, `ActiveReservation` or `Expired`."),
					"lease_expiry_time": c("Lease expiry time in RFC 3339 (UTC). Empty when the lease does not expire."),
					"description":       c("Lease description."),
					"client_type":       c("Client type, for example `Dhcp`."),
				}},
			},
		},
	}
}

// Configure receives the shared *dhcp.Client created by the provider.
func (d *leasesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

// Read lists the scope's leases and stores them in state. It reuses the config model as the
// result: the inputs are copied back unchanged and only Leases is filled in.
func (d *leasesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	// Step 1: read the inputs.
	var cfg leasesModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Step 2: run Get-DhcpServerv4Lease. ValueBool returns false when all_leases is not set,
	// which gives the documented default (active leases only).
	//
	// Go note: the call returns two values (the leases and an error); `if err != nil` is
	// Go's standard way to check for failure.
	leases, err := d.client.ListLeases(ctx, cfg.ScopeID.ValueString(), cfg.AllLeases.ValueBool())
	if err != nil {
		resp.Diagnostics.AddError("Unable to list leases in scope "+cfg.ScopeID.ValueString(), err.Error())
		return
	}

	// Step 3: convert each dhcp.Lease into a leaseModel. Starting from an empty (not nil)
	// slice means a scope without leases is stored as an empty list rather than null.
	//
	// Go note: `for _, l := range leases` visits each element; `_` discards the index.
	cfg.Leases = make([]leaseModel, 0, len(leases))
	for _, l := range leases {
		cfg.Leases = append(cfg.Leases, leaseModel{
			IPAddress:       types.StringValue(l.IPAddress),
			ClientID:        types.StringValue(l.ClientID),
			HostName:        types.StringValue(l.HostName),
			AddressState:    types.StringValue(l.AddressState),
			LeaseExpiryTime: types.StringValue(l.LeaseExpiryTime),
			Description:     types.StringValue(l.Description),
			ClientType:      types.StringValue(l.ClientType),
		})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
}
