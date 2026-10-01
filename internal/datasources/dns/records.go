// This file implements windowsddi_dns_records: the records of a zone, with
// optional name and type filters, each rendered as a generic string (the
// zone file form of its data). Useful to feed a source of truth such as
// NetBox. See internal/datasources/dhcp/leases.go for the framework basics
// of a list data source.
//
// Cmdlets used (through the dns package): Get-DnsServerZone,
// Get-DnsServerResourceRecord.

package dnsdatasources

import (
	"context"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/normalize"
)

// recordDataSources lists the record data sources.
func recordDataSources() []func() datasource.DataSource {
	return []func() datasource.DataSource{NewRecords}
}

var _ datasource.DataSourceWithConfigure = &recordsDataSource{}

// NewRecords returns the windowsddi_dns_records data source.
func NewRecords() datasource.DataSource { return &recordsDataSource{} }

// recordsDataSource implements windowsddi_dns_records.
type recordsDataSource struct{ client *dns.Client }

// recordItemModel is one element of the `records` list.
type recordItemModel struct {
	Name    types.String `tfsdk:"name"`
	Type    types.String `tfsdk:"type"`
	TTL     types.Int64  `tfsdk:"ttl"`
	Value   types.String `tfsdk:"value"`
	Dynamic types.Bool   `tfsdk:"dynamic"`
}

// recordsModel is the whole data source: inputs and the computed list.
type recordsModel struct {
	ZoneName types.String      `tfsdk:"zone_name"`
	Name     types.String      `tfsdk:"name"`
	Type     types.String      `tfsdk:"type"`
	Records  []recordItemModel `tfsdk:"records"`
}

// Metadata sets the name to "windowsddi_dns_records".
func (d *recordsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dns_records"
}

// Schema declares the inputs (zone_name, name, type) and the computed list.
func (d *recordsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists the records of a zone (`Get-DnsServerResourceRecord`), optionally filtered by name and type. " +
			"Every record is returned, including the apex SOA and NS records and dynamic records, with its data rendered as a string in zone file form.",
		Attributes: map[string]schema.Attribute{
			"zone_name": schema.StringAttribute{
				MarkdownDescription: "Zone to list, for example `lab.example.local`. Case and a trailing dot are ignored.",
				Required:            true,
				Validators:          []validator.String{normalize.ZoneNameValidator()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Only return records of this name, relative to the zone (`@` for the apex). Records of child names are not included. Case is ignored.",
				Optional:            true,
				Validators:          []validator.String{normalize.RecordNameValidator()},
			},
			"type": schema.StringAttribute{
				MarkdownDescription: "Only return records of this type, for example `A`, `MX` or `NS`. Case is ignored.",
				Optional:            true,
			},
			"records": schema.ListNestedAttribute{
				MarkdownDescription: "Matching records, sorted by name, type and value.",
				Computed:            true,
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"name": schema.StringAttribute{
						MarkdownDescription: "Record name relative to the zone, lowercase, `@` for the apex.",
						Computed:            true,
					},
					"type": schema.StringAttribute{
						MarkdownDescription: "Record type in upper case, for example `A`.",
						Computed:            true,
					},
					"ttl": schema.Int64Attribute{
						MarkdownDescription: "Time to live in seconds.",
						Computed:            true,
					},
					"value": schema.StringAttribute{
						MarkdownDescription: "Record data in zone file form, for example `10.1.20.80` (A), `www.lab.example.local.` (CNAME), `10 mail.lab.example.local.` (MX), `10 60 5060 sip1.lab.example.local.` (SRV) or the quoted text (TXT).",
						Computed:            true,
					},
					"dynamic": schema.BoolAttribute{
						MarkdownDescription: "`true` for a dynamic record (registered by a client or the DHCP server, has a timestamp), `false` for a static one.",
						Computed:            true,
					},
				}},
			},
		},
	}
}

// Configure receives the shared *dns.Client created by the provider.
func (d *recordsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

// Read lists the records and stores them sorted, so the output does not
// change order between runs.
func (d *recordsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg recordsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Validators already checked these, so the errors can be ignored.
	zone, _ := normalize.ZoneName(cfg.ZoneName.ValueString())
	name := ""
	if !cfg.Name.IsNull() {
		name, _ = normalize.RecordName(cfg.Name.ValueString())
	}
	rrType := strings.ToUpper(strings.TrimSpace(cfg.Type.ValueString()))

	recs, err := d.client.ListRecords(ctx, zone, name, rrType)
	if err != nil {
		summary := "Unable to list records in zone " + zone
		if dns.IsNotFound(err) {
			resp.Diagnostics.AddError(summary, "the zone does not exist on the DNS server: "+err.Error())
			return
		}
		resp.Diagnostics.AddError(summary, err.Error())
		return
	}

	cfg.Records = make([]recordItemModel, 0, len(recs))
	for _, r := range recs {
		cfg.Records = append(cfg.Records, recordItemModel{
			Name:    types.StringValue(strings.ToLower(r.Name)),
			Type:    types.StringValue(strings.ToUpper(r.Type)),
			TTL:     types.Int64Value(r.TTL),
			Value:   types.StringValue(r.Data),
			Dynamic: types.BoolValue(r.Dynamic),
		})
	}
	// Go note: sort.SliceStable sorts in place with the given "less" function.
	sort.SliceStable(cfg.Records, func(i, j int) bool {
		a, b := cfg.Records[i], cfg.Records[j]
		if a.Name.ValueString() != b.Name.ValueString() {
			return a.Name.ValueString() < b.Name.ValueString()
		}
		if a.Type.ValueString() != b.Type.ValueString() {
			return a.Type.ValueString() < b.Type.ValueString()
		}
		return a.Value.ValueString() < b.Value.ValueString()
	})
	resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
}
