// This file registers the zone data sources and implements windowsddi_dns_zones (list every
// zone, optionally filtered). windowsddi_dns_zone is in zone.go.

package dnsdatasources

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
)

// zoneDataSources lists the zone data sources.
func zoneDataSources() []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewZone,
		NewZones,
	}
}

var _ datasource.DataSourceWithConfigure = &zonesDataSource{}

// NewZones returns the windowsddi_dns_zones data source.
func NewZones() datasource.DataSource { return &zonesDataSource{} }

// zonesDataSource lists zones.
type zonesDataSource struct{ client *dns.Client }

// zonesModel holds the optional filters and the result. A null filter matches every zone.
type zonesModel struct {
	Reverse      types.Bool  `tfsdk:"reverse"`
	ADIntegrated types.Bool  `tfsdk:"ad_integrated"`
	Zones        []zoneModel `tfsdk:"zones"`
}

// Metadata names the data source "windowsddi_dns_zones".
func (d *zonesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dns_zones"
}

// Schema declares the two optional filters and the computed `zones` nested list.
func (d *zonesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the DNS zones on the server (`Get-DnsServerZone`), primary zones and conditional forwarders included. " +
			"Zones the server creates automatically (`TrustAnchors`, `0.in-addr.arpa`, ...) are left out.",
		Attributes: map[string]schema.Attribute{
			"reverse": schema.BoolAttribute{
				MarkdownDescription: "When set, only reverse lookup zones (`true`) or only forward zones (`false`) are returned.",
				Optional:            true,
			},
			"ad_integrated": schema.BoolAttribute{
				MarkdownDescription: "When set, only AD-integrated zones (`true`) or only zones not stored in Active Directory (`false`) are returned.",
				Optional:            true,
			},
			"zones": schema.ListNestedAttribute{
				MarkdownDescription: "Matching zones, in the order the server returns them.",
				Computed:            true,
				NestedObject:        schema.NestedAttributeObject{Attributes: zoneAttributes()},
			},
		},
	}
}

// Configure stores the DNS client built by the provider.
func (d *zonesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

// Read lists the zones, drops auto-created ones and applies the filters.
func (d *zonesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg zonesModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	zones, err := d.client.ListZones(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Unable to list DNS zones", err.Error())
		return
	}
	cfg.Zones = make([]zoneModel, 0, len(zones))
	for _, z := range zones {
		if z.IsAutoCreated ||
			(!cfg.Reverse.IsNull() && cfg.Reverse.ValueBool() != z.IsReverseLookupZone) ||
			(!cfg.ADIntegrated.IsNull() && cfg.ADIntegrated.ValueBool() != z.IsDsIntegrated) {
			continue
		}
		m, diags := newZoneModel(ctx, z)
		resp.Diagnostics.Append(diags...)
		cfg.Zones = append(cfg.Zones, m)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
}
