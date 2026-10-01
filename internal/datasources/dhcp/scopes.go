package dhcpdatasources

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dhcp"
)

// Compile-time check that *scopesDataSource satisfies the data source interface.
//
// Go note: this line produces no runtime code; the build fails if a required method is
// missing, which catches typos in method names early.
var _ datasource.DataSourceWithConfigure = &scopesDataSource{}

// NewScopes returns the windowsddi_dhcp_scopes data source.
// It is listed in All and called by the framework each time it needs a new instance.
func NewScopes() datasource.DataSource { return &scopesDataSource{} }

// scopesDataSource implements windowsddi_dhcp_scopes: list every IPv4 scope on the server.
// It takes no input at all.
type scopesDataSource struct{ client *dhcp.Client }

// scopesModel is the whole result: a single `scopes` attribute holding a list of scopes.
//
// Go note: `[]scopeModel` is a slice, Go's growable list type. The framework maps it to a
// Terraform list, so in HCL you write data.windowsddi_dhcp_scopes.all.scopes[0].name.
type scopesModel struct {
	Scopes []scopeModel `tfsdk:"scopes"`
}

// Metadata sets the data source name to "windowsddi_dhcp_scopes".
//
// Go note: `(d *scopesDataSource)` is the method receiver: the object this method belongs to.
func (d *scopesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dhcp_scopes"
}

// Schema declares one computed attribute, `scopes`, as a nested list: a list whose elements
// are objects with their own attributes (scope_id, name, start_range, ...). This is the
// framework's ListNestedAttribute. Every nested attribute is Computed, including scope_id,
// because here the user does not supply anything.
func (d *scopesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	attrs := scopeAttributes()
	attrs["scope_id"] = schema.StringAttribute{MarkdownDescription: "Scope ID (network address).", Computed: true}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists every IPv4 scope on the server (`Get-DhcpServerv4Scope`).",
		Attributes: map[string]schema.Attribute{
			"scopes": schema.ListNestedAttribute{
				MarkdownDescription: "All IPv4 scopes, ordered as the server returns them.",
				Computed:            true,
				NestedObject:        schema.NestedAttributeObject{Attributes: attrs},
			},
		},
	}
}

// Configure receives the shared *dhcp.Client created by the provider.
func (d *scopesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

// Read lists all scopes and stores them in state. There is no config to read, so the request
// parameter is ignored (named `_`).
func (d *scopesDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	// Go note: `scopes, err := ...` receives two return values. Go reports failure with a
	// returned `error`, checked with `if err != nil`.
	scopes, err := d.client.ListScopes(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Unable to list scopes", err.Error())
		return
	}
	// Build the list. make(..., 0, len(scopes)) creates an empty slice with room for every
	// scope, so append does not need to grow it. Starting from an empty (not nil) slice
	// means "no scopes" is stored as an empty list rather than null.
	//
	// Go note: `for _, s := range scopes` loops over the slice; `_` drops the index, s is
	// each element in turn. append returns the extended slice, which is assigned back.
	m := scopesModel{Scopes: make([]scopeModel, 0, len(scopes))}
	for _, s := range scopes {
		m.Scopes = append(m.Scopes, newScopeModel(s))
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
