// This file implements the windowsddi_dns_zone data source (one zone by name) and holds the
// zone attributes and model shared with windowsddi_dns_zones (zones.go).

package dnsdatasources

import (
	"context"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/normalize"
)

var _ datasource.DataSourceWithConfigure = &zoneDataSource{}

// NewZone returns the windowsddi_dns_zone data source.
func NewZone() datasource.DataSource { return &zoneDataSource{} }

// zoneDataSource looks up one zone (any type) by name.
type zoneDataSource struct{ client *dns.Client }

// zoneModel is one zone as exposed by both zone data sources. Strings that do not apply to
// the zone type (zone_file of an AD-integrated zone, master_servers of a primary zone, ...)
// are null or empty.
type zoneModel struct {
	Name             types.String `tfsdk:"name"`
	ZoneType         types.String `tfsdk:"zone_type"`
	ADIntegrated     types.Bool   `tfsdk:"ad_integrated"`
	Reverse          types.Bool   `tfsdk:"reverse"`
	ReplicationScope types.String `tfsdk:"replication_scope"`
	ZoneFile         types.String `tfsdk:"zone_file"`
	DynamicUpdate    types.String `tfsdk:"dynamic_update"`
	MasterServers    types.List   `tfsdk:"master_servers"`
	ForwarderTimeout types.Int64  `tfsdk:"forwarder_timeout"`
}

// optString maps "" (not applicable) to null.
func optString(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}

// newZoneModel converts a client zone. Names are lowercased, like everywhere in the provider.
func newZoneModel(ctx context.Context, z dns.Zone) (zoneModel, diag.Diagnostics) {
	masters := z.MasterServers
	if masters == nil {
		masters = []string{}
	}
	list, diags := types.ListValueFrom(ctx, types.StringType, masters)
	m := zoneModel{
		Name:             types.StringValue(strings.ToLower(z.Name)),
		ZoneType:         types.StringValue(z.ZoneType),
		ADIntegrated:     types.BoolValue(z.IsDsIntegrated),
		Reverse:          types.BoolValue(z.IsReverseLookupZone),
		ReplicationScope: optString(z.ReplicationScope),
		ZoneFile:         optString(z.ZoneFile),
		DynamicUpdate:    optString(z.DynamicUpdate),
		MasterServers:    list,
		ForwarderTimeout: types.Int64Null(),
	}
	if z.ZoneType == dns.ZoneTypeForwarder {
		m.ForwarderTimeout = types.Int64Value(z.ForwarderTimeout)
	}
	return m, diags
}

// zoneAttributes returns the computed zone attributes. The caller adjusts name (input for
// windowsddi_dns_zone, computed in the zones list).
func zoneAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"name": schema.StringAttribute{
			MarkdownDescription: "Zone name, lowercase without trailing dot.",
			Computed:            true,
		},
		"zone_type": schema.StringAttribute{
			MarkdownDescription: "Zone type as reported by `Get-DnsServerZone`: `Primary`, `Secondary`, `Stub` or `Forwarder` (conditional forwarder).",
			Computed:            true,
		},
		"ad_integrated": schema.BoolAttribute{
			MarkdownDescription: "Whether the zone is stored in Active Directory.",
			Computed:            true,
		},
		"reverse": schema.BoolAttribute{
			MarkdownDescription: "Whether the zone is a reverse lookup zone.",
			Computed:            true,
		},
		"replication_scope": schema.StringAttribute{
			MarkdownDescription: "AD replication scope (`Forest`, `Domain`, `Legacy` or `Custom`). Null when the zone is not AD-integrated.",
			Computed:            true,
		},
		"zone_file": schema.StringAttribute{
			MarkdownDescription: "Zone file name for file-backed zones, null otherwise.",
			Computed:            true,
		},
		"dynamic_update": schema.StringAttribute{
			MarkdownDescription: "Dynamic update mode (`None`, `Secure` or `NonsecureAndSecure`). Null for zone types without one.",
			Computed:            true,
		},
		"master_servers": schema.ListAttribute{
			MarkdownDescription: "Master servers of a conditional forwarder, in order. Empty for other zone types.",
			Computed:            true,
			ElementType:         types.StringType,
		},
		"forwarder_timeout": schema.Int64Attribute{
			MarkdownDescription: "Forwarder timeout in seconds for a conditional forwarder, null otherwise.",
			Computed:            true,
		},
	}
}

// Metadata names the data source "windowsddi_dns_zone".
func (d *zoneDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dns_zone"
}

// Schema makes name the required input; everything else is computed.
func (d *zoneDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	attrs := zoneAttributes()
	attrs["name"] = schema.StringAttribute{
		MarkdownDescription: "Zone name to look up, for example `lab.example.local` or `20.1.10.in-addr.arpa`. Casing and a trailing dot are ignored. Kept as written.",
		Required:            true,
		Validators:          []validator.String{normalize.ZoneNameValidator()},
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up one DNS zone by name (`Get-DnsServerZone`): primary zones, conditional forwarders and any other zone type.",
		Attributes:          attrs,
	}
}

// Configure stores the DNS client built by the provider.
func (d *zoneDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

// Read looks the zone up. Unlike a resource, a missing zone is an error.
func (d *zoneDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg zoneModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name, err := normalize.ZoneName(cfg.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid zone name", err.Error())
		return
	}
	z, err := d.client.GetZone(ctx, name)
	if err != nil {
		resp.Diagnostics.AddError("Unable to read DNS zone "+name, err.Error())
		return
	}
	m, diags := newZoneModel(ctx, *z)
	resp.Diagnostics.Append(diags...)
	m.Name = cfg.Name
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
