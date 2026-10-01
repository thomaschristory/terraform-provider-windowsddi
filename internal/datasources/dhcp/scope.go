package dhcpdatasources

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dhcp"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/normalize"
)

// Compile-time check that *scopeDataSource has all the methods the framework expects of a
// data source that also receives provider data (Metadata, Schema, Read, Configure).
//
// Go note: `var _ Interface = &Type{}` fails the build if Type is missing a method. Go
// interfaces are satisfied implicitly: there is no "implements" keyword.
var _ datasource.DataSourceWithConfigure = &scopeDataSource{}

// NewScope returns the windowsddi_dhcp_scope data source.
// It is listed in All and called by the framework each time it needs a new instance.
func NewScope() datasource.DataSource { return &scopeDataSource{} }

// scopeDataSource implements windowsddi_dhcp_scope: look up one IPv4 scope by its ID.
// Its only field is the shared DHCP client, filled in by Configure.
//
// Go note: the lowercase type name makes it private to this package; other packages only see
// it through the exported NewScope function and the datasource.DataSource interface.
type scopeDataSource struct{ client *dhcp.Client }

// Metadata sets the data source name: the provider prefix ("windowsddi") plus "_scope".
//
// Go note: `(d *scopeDataSource)` makes this a method on the type, with d as the receiver
// (like `$this`). The `*` means d is a pointer, so Configure can store the client in it.
func (d *scopeDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dhcp_scope"
}

// Schema declares the attributes: scope_id is the only input (Required, validated as an IPv4
// address before Read runs); all other scope attributes are computed outputs.
func (d *scopeDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	attrs := scopeAttributes()
	attrs["scope_id"] = schema.StringAttribute{
		MarkdownDescription: "Scope ID (network address) to look up, for example `10.1.2.0`.",
		Required:            true,
		Validators:          []validator.String{normalize.IPv4Validator()},
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Looks up one IPv4 scope by ID (`Get-DhcpServerv4Scope`).",
		Attributes:          attrs,
	}
}

// Configure receives the *dhcp.Client created once by the provider and keeps it for Read.
func (d *scopeDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

// Read fetches the scope from the server and writes every attribute into Terraform's state.
// A missing scope is an error (not an empty result): a data source that names a specific
// scope ID expects it to exist, and the error includes the cmdlet's own message.
func (d *scopeDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	// Step 1: read the user's HCL (only scope_id is set) into a model.
	//
	// Go note: Go has no exceptions. Failures come back as values (diagnostics here, or an
	// `error` below) that must be checked explicitly with `if ... { return }`.
	var cfg scopeModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Step 2: run Get-DhcpServerv4Scope through the dhcp client.
	//
	// Go note: GetScope returns two values, a pointer to the scope and an error. `:=` declares
	// both variables at once. When err is not nil, s should not be used.
	s, err := d.client.GetScope(ctx, cfg.ScopeID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to read scope "+cfg.ScopeID.ValueString(), err.Error())
		return
	}
	// Step 3: convert to the Terraform model and save it as the data source's result.
	// `*s` dereferences the pointer, passing a copy of the scope value.
	m := newScopeModel(*s)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
