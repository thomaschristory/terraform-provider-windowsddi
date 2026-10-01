// This file implements windowsddi_dns_conditional_forwarder: a conditional forwarder zone
// that sends queries for one domain to fixed DNS servers. Framework concepts are explained
// in internal/resources/dhcp/scope.go.
//
// Cmdlets used (through the dns package): Get-DnsServerZone,
// Add-DnsServerConditionalForwarderZone, Set-DnsServerConditionalForwarderZone,
// Remove-DnsServerZone.

package dnsresources

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/normalize"
)

var (
	_ resource.ResourceWithConfigure   = &forwarderResource{}
	_ resource.ResourceWithImportState = &forwarderResource{}
)

// NewConditionalForwarder returns the windowsddi_dns_conditional_forwarder resource.
func NewConditionalForwarder() resource.Resource { return &forwarderResource{} }

// forwarderResource holds the shared DNS client.
type forwarderResource struct{ client *dns.Client }

// forwarderModel mirrors one windowsddi_dns_conditional_forwarder block. MasterServers is a
// types.List of strings (order matters: the server tries them in order).
type forwarderModel struct {
	ID               types.String            `tfsdk:"id"`
	Name             normalize.ZoneNameValue `tfsdk:"name"`
	MasterServers    types.List              `tfsdk:"master_servers"`
	ReplicationScope types.String            `tfsdk:"replication_scope"`
	ForwarderTimeout types.Int64             `tfsdk:"forwarder_timeout"`
}

// Metadata names the resource "windowsddi_dns_conditional_forwarder".
func (r *forwarderResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dns_conditional_forwarder"
}

// Schema describes windowsddi_dns_conditional_forwarder.
func (r *forwarderResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a conditional forwarder (`Add-DnsServerConditionalForwarderZone`, `Set-DnsServerConditionalForwarderZone`, `Remove-DnsServerZone`): " +
			"queries for the domain are forwarded to `master_servers`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Forwarded domain in canonical form (lowercase, no trailing dot).",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Forwarded domain, for example `partner.example.com`. Casing and a trailing dot are ignored when comparing. Changing it replaces the forwarder.",
				Required:            true,
				CustomType:          normalize.ZoneNameType{},
				PlanModifiers:       []planmodifier.String{zoneNameReplace()},
			},
			// Element validators run on each list item; SizeAtLeast(1) because the cmdlets
			// require at least one master server.
			"master_servers": schema.ListAttribute{
				MarkdownDescription: "IPv4 or IPv6 addresses of the DNS servers to forward to, in the order they are tried. " +
					"Equivalent IPv6 spellings (for example `2001:DB8::53` and `2001:db8::53`) never show a diff.",
				Required:    true,
				ElementType: types.StringType,
				Validators: []validator.List{
					listvalidator.SizeAtLeast(1),
					listvalidator.ValueStringsAre(normalize.IPValidator()),
				},
			},
			"replication_scope": schema.StringAttribute{
				MarkdownDescription: "Stores the forwarder in Active Directory and sets where it replicates: `Forest`, `Domain` or `Legacy`. " +
					"When unset the forwarder is stored on this server only. Changing between the three values is done in place; " +
					"setting or unsetting it replaces the forwarder.",
				Optional:      true,
				Validators:    []validator.String{stringvalidator.OneOf(zoneReplicationScopes...)},
				PlanModifiers: []planmodifier.String{zoneReplicationScopeReplace()},
			},
			// Optional+Computed without a Default: when omitted, the server's value (5 seconds
			// unless changed elsewhere) is kept and reported.
			"forwarder_timeout": schema.Int64Attribute{
				MarkdownDescription: "Seconds to wait for a master server before trying the next one. When unset, the server default (5) is used and reported.",
				Optional:            true,
				Computed:            true,
				Validators:          []validator.Int64{int64validator.AtLeast(1)},
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
		},
	}
}

// Configure stores the DNS client built by the provider.
func (r *forwarderResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

// input converts the model into the client's input with the canonical name.
func (m *forwarderModel) input(ctx context.Context) (dns.ConditionalForwarderInput, diag.Diagnostics) {
	var masters []string
	diags := m.MasterServers.ElementsAs(ctx, &masters, false)
	return dns.ConditionalForwarderInput{
		Name:             m.Name.Canonical(),
		MasterServers:    masters,
		ReplicationScope: m.ReplicationScope.ValueString(),
		ForwarderTimeout: m.ForwarderTimeout.ValueInt64(),
	}, diags
}

// apply copies the zone the server returned into the model.
//
// Master servers come back in the server's canonical form. Element by element, the spelling
// already in the model is kept when it is the same address, so writing "2001:DB8::53" never
// causes a perpetual diff; otherwise the server value is stored.
func (m *forwarderModel) apply(ctx context.Context, z *dns.Zone) diag.Diagnostics {
	var prior []string
	if !m.MasterServers.IsNull() && !m.MasterServers.IsUnknown() {
		_ = m.MasterServers.ElementsAs(ctx, &prior, false)
	}
	masters := make([]string, len(z.MasterServers))
	for i, s := range z.MasterServers {
		masters[i] = s
		if i < len(prior) {
			a, errA := normalize.CanonicalIP(prior[i])
			b, errB := normalize.CanonicalIP(s)
			if errA == nil && errB == nil && a == b {
				masters[i] = prior[i]
			}
		}
	}
	list, diags := types.ListValueFrom(ctx, types.StringType, masters)
	m.MasterServers = list
	m.ID = types.StringValue(zoneCanonical(z.Name))
	m.Name = normalize.NewZoneNameValue(z.Name)
	m.ReplicationScope = zoneOptString(normalize.EnumOf(z.ReplicationScope, zoneReplicationScopes...))
	m.ForwarderTimeout = types.Int64Value(z.ForwarderTimeout)
	return diags
}

// Create runs Add-DnsServerConditionalForwarderZone.
func (r *forwarderResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan forwarderModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	in, diags := plan.input(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	z, err := r.client.AddConditionalForwarder(ctx, in)
	if err != nil {
		resp.Diagnostics.AddError(zoneErrSummary("create", "conditional forwarder "+in.Name), err.Error())
		return
	}
	resp.Diagnostics.Append(plan.apply(ctx, z)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read refreshes the forwarder. A missing zone is drift; a zone of another type under the
// same name is an error (not drift, recreating it would fail).
func (r *forwarderResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state forwarderModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.ID.ValueString()
	z, err := r.client.GetZone(ctx, name)
	if dns.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError(zoneErrSummary("read", "conditional forwarder "+name), err.Error())
		return
	}
	if z.ZoneType != dns.ZoneTypeForwarder {
		resp.Diagnostics.AddError("DNS zone "+name+" is not a conditional forwarder",
			fmt.Sprintf("Get-DnsServerZone reports zone type %q. windowsddi_dns_conditional_forwarder manages conditional forwarders only; "+
				"use windowsddi_dns_zone for primary zones.", z.ZoneType))
		return
	}
	resp.Diagnostics.Append(state.apply(ctx, z)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update runs Set-DnsServerConditionalForwarderZone with the planned master servers,
// timeout and replication scope (the client only moves the scope when it differs).
func (r *forwarderResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan forwarderModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	in, diags := plan.input(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	z, err := r.client.SetConditionalForwarder(ctx, in)
	if err != nil {
		resp.Diagnostics.AddError(zoneErrSummary("update", "conditional forwarder "+in.Name), err.Error())
		return
	}
	resp.Diagnostics.Append(plan.apply(ctx, z)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete runs Remove-DnsServerZone; a forwarder that is already gone counts as deleted.
func (r *forwarderResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state forwarderModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.ID.ValueString()
	if err := r.client.RemoveZone(ctx, name); err != nil && !dns.IsNotFound(err) {
		resp.Diagnostics.AddError(zoneErrSummary("delete", "conditional forwarder "+name), err.Error())
	}
}

// ImportState accepts the forwarded domain, for example partner.example.com.
func (r *forwarderResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	canonical, err := normalize.ZoneName(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid Import ID", fmt.Sprintf("expected a domain name such as partner.example.com: %s", err))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), canonical)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), normalize.NewZoneNameValue(req.ID))...)
}
