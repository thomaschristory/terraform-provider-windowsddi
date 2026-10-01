// This file implements windowsddi_dns_zone, a primary DNS zone (forward or reverse,
// AD-integrated or file-backed). The framework concepts (schema flags, plan modifiers, the
// CRUD pattern, drift handling, import) are explained in internal/resources/dhcp/scope.go;
// the comments here focus on what is specific to zones.
//
// Cmdlets used (through the dns package): Get-DnsServerZone, Add-DnsServerPrimaryZone,
// Set-DnsServerPrimaryZone, Get-DnsServerResourceRecord (force_destroy check),
// Remove-DnsServerZone.

package dnsresources

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/normalize"
)

// Compile-time interface checks (see scope.go). ResourceWithValidateConfig adds a
// cross-attribute check (Secure updates need an AD-integrated zone).
var (
	_ resource.ResourceWithConfigure      = &zoneResource{}
	_ resource.ResourceWithImportState    = &zoneResource{}
	_ resource.ResourceWithModifyPlan     = &zoneResource{}
	_ resource.ResourceWithValidateConfig = &zoneResource{}
)

// NewZone returns the windowsddi_dns_zone resource.
func NewZone() resource.Resource { return &zoneResource{} }

// zoneResource holds the shared DNS client; per-zone data lives in Terraform state.
type zoneResource struct{ client *dns.Client }

// zoneModel mirrors one windowsddi_dns_zone block.
//
// Name uses normalize.ZoneNameValue: "Lab.Example.local." and "lab.example.local" are
// semantically equal, so the server's spelling never causes a diff and state keeps the
// user's. ID is always the canonical (lowercase, no trailing dot) name and is what the
// resource uses to talk to the server. NetworkID cannot be read back from the server: it
// only exists in configuration and state.
type zoneModel struct {
	ID               types.String            `tfsdk:"id"`
	Name             normalize.ZoneNameValue `tfsdk:"name"`
	NetworkID        types.String            `tfsdk:"network_id"`
	ReplicationScope types.String            `tfsdk:"replication_scope"`
	ZoneFile         types.String            `tfsdk:"zone_file"`
	DynamicUpdate    types.String            `tfsdk:"dynamic_update"`
	ForceDestroy     types.Bool              `tfsdk:"force_destroy"`
	ADIntegrated     types.Bool              `tfsdk:"ad_integrated"`
	Reverse          types.Bool              `tfsdk:"reverse"`
}

// Metadata names the resource "windowsddi_dns_zone".
func (r *zoneResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dns_zone"
}

// Schema describes windowsddi_dns_zone.
//
// Replacement rules are split between attribute plan modifiers and ModifyPlan:
//   - zone_file: any change replaces (Set-DnsServerPrimaryZone cannot rename the file).
//   - replication_scope: Forest/Domain/Legacy changes are in place, switching to or from
//     file-backed replaces (zoneReplicationScopeReplace).
//   - name and network_id: ModifyPlan replaces only when the resulting zone name changes, so
//     switching from name to an equivalent network_id (or changing the name's casing) does
//     not recreate the zone.
func (r *zoneResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a primary DNS zone (`Add-DnsServerPrimaryZone`, `Set-DnsServerPrimaryZone`, `Remove-DnsServerZone`): " +
			"a forward zone by `name` or a reverse lookup zone by `network_id`, AD-integrated (`replication_scope`) or file-backed (`zone_file`).\n\n" +
			"AD-integrated zones replicate between domain controllers. Point the provider at one domain controller: " +
			"reads against another one may briefly show drift until replication catches up.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Zone name in canonical form (lowercase, no trailing dot). Known at plan time.",
				Computed:            true,
			},
			// Optional+Computed: set by the user for forward zones, derived from network_id at
			// plan time (ModifyPlan) for reverse zones.
			"name": schema.StringAttribute{
				MarkdownDescription: "Zone name, for example `lab.example.local`. Exactly one of `name` and `network_id` must be set. " +
					"Computed at plan time for reverse zones created with `network_id`. Casing and a trailing dot are ignored when comparing. " +
					"Renaming replaces the zone.",
				Optional:   true,
				Computed:   true,
				CustomType: normalize.ZoneNameType{},
				Validators: []validator.String{stringvalidator.ExactlyOneOf(path.MatchRoot("network_id"))},
			},
			"network_id": schema.StringAttribute{
				MarkdownDescription: "Network in CIDR form to create a reverse lookup zone for, for example `10.1.20.0/24` (zone `20.1.10.in-addr.arpa`). " +
					"IPv4 prefixes must be `/8`, `/16` or `/24`; IPv6 prefixes a multiple of 4. Changing it to another network replaces the zone. " +
					"Not read from the server (an imported reverse zone has only `name`).",
				Optional:   true,
				Validators: []validator.String{normalize.ReverseNetworkValidator()},
			},
			"replication_scope": schema.StringAttribute{
				MarkdownDescription: "Makes the zone AD-integrated and sets where Active Directory replicates it: `Forest`, `Domain` or `Legacy`. " +
					"Exactly one of `replication_scope` and `zone_file` must be set. Changing between the three values is done in place; " +
					"switching between AD-integrated and file-backed replaces the zone.",
				Optional: true,
				Validators: []validator.String{
					stringvalidator.OneOf(zoneReplicationScopes...),
					stringvalidator.ExactlyOneOf(path.MatchRoot("zone_file")),
				},
				PlanModifiers: []planmodifier.String{zoneReplicationScopeReplace()},
			},
			"zone_file": schema.StringAttribute{
				MarkdownDescription: "Makes the zone file-backed: file name under `%windir%\\System32\\dns`, for example `lab.example.test.dns`. " +
					"Changing it replaces the zone.",
				Optional:      true,
				Validators:    []validator.String{stringvalidator.LengthAtLeast(1)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"dynamic_update": schema.StringAttribute{
				MarkdownDescription: "Dynamic updates accepted by the zone: `None`, `Secure` or `NonsecureAndSecure`. " +
					"Defaults to `Secure` for AD-integrated zones and `None` for file-backed zones. `Secure` requires an AD-integrated zone.",
				Optional:   true,
				Computed:   true,
				Validators: []validator.String{stringvalidator.OneOf(zoneDynamicUpdates...)},
			},
			"force_destroy": schema.BoolAttribute{
				MarkdownDescription: "When `true`, destroying the resource removes the zone even if it holds records other than the apex SOA and NS records. " +
					"Defaults to `false`. Terraform only, not read from the server.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
			},
			"ad_integrated": schema.BoolAttribute{
				MarkdownDescription: "Whether the zone is stored in Active Directory.",
				Computed:            true,
			},
			"reverse": schema.BoolAttribute{
				MarkdownDescription: "Whether the zone is a reverse lookup zone (`in-addr.arpa` or `ip6.arpa`).",
				Computed:            true,
			},
		},
	}
}

// Configure stores the DNS client built by the provider.
func (r *zoneResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

// ValidateConfig rejects Secure dynamic updates on a file-backed zone before anything reaches
// the server (which would refuse them at apply time).
func (r *zoneResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg zoneModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if cfg.DynamicUpdate.ValueString() == dynamicUpdateSecure && !cfg.ZoneFile.IsNull() && !cfg.ZoneFile.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("dynamic_update"), "Invalid Attribute Combination",
			"dynamic_update = \"Secure\" requires an AD-integrated zone (replication_scope); file-backed zones support only \"None\" or \"NonsecureAndSecure\".")
	}
}

// zoneIsReverse reports whether a canonical zone name is a reverse lookup zone.
func zoneIsReverse(canonical string) bool {
	return strings.HasSuffix(canonical, ".in-addr.arpa") || strings.HasSuffix(canonical, ".ip6.arpa")
}

// ModifyPlan fills in at plan time what can be derived from the configuration, so plans show
// real values instead of "(known after apply)":
//   - name (reverse zones: from network_id with normalize.ReverseZoneName, the same rule the
//     server applies), id, reverse and ad_integrated;
//   - dynamic_update when not configured: Secure for AD-integrated zones, None for
//     file-backed ones (like a Default, but the value depends on another attribute);
//   - replacement when the resulting zone name differs from the one in state.
func (r *zoneResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // destroy
	}
	var plan zoneModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	var cfgDynamicUpdate types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("dynamic_update"), &cfgDynamicUpdate)...)
	var state *zoneModel
	if !req.State.Raw.IsNull() {
		state = &zoneModel{}
		resp.Diagnostics.Append(req.State.Get(ctx, state)...)
	}
	if resp.Diagnostics.HasError() {
		return
	}

	// AD-integrated or file-backed, and the dynamic update default that follows from it.
	if !plan.ReplicationScope.IsUnknown() {
		ad := !plan.ReplicationScope.IsNull()
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("ad_integrated"), ad)...)
		if cfgDynamicUpdate.IsNull() {
			du := dynamicUpdateNone
			if ad {
				du = dynamicUpdateSecure
			}
			resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("dynamic_update"), du)...)
		}
	}

	// The effective zone name: configured, or derived from network_id.
	name, nameAttr := "", path.Root("name")
	switch {
	case !plan.NetworkID.IsNull():
		nameAttr = path.Root("network_id")
		if plan.NetworkID.IsUnknown() {
			return
		}
		rev, err := normalize.ReverseZoneName(plan.NetworkID.ValueString())
		if err != nil {
			return // the attribute validator reports it
		}
		name = rev
		// Keep the spelling already in state when it names the same zone (no diff).
		if state != nil && zoneCanonical(state.Name.ValueString()) == rev {
			name = state.Name.ValueString()
		}
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("name"), normalize.NewZoneNameValue(name))...)
	case plan.Name.IsUnknown() || plan.Name.IsNull():
		return
	default:
		name = plan.Name.ValueString()
	}
	canonical := zoneCanonical(name)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("id"), canonical)...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("reverse"), zoneIsReverse(canonical))...)

	if state != nil && zoneCanonical(state.ID.ValueString()) != canonical {
		resp.RequiresReplace = append(resp.RequiresReplace, nameAttr)
	}
}

// apply copies the zone the server returned into the model. Name is set to the server's
// spelling; the framework keeps the prior spelling when the two are semantically equal.
func (m *zoneModel) apply(z *dns.Zone) {
	m.ID = types.StringValue(zoneCanonical(z.Name))
	m.Name = normalize.NewZoneNameValue(z.Name)
	m.ReplicationScope = zoneOptString(normalize.EnumOf(z.ReplicationScope, zoneReplicationScopes...))
	// The server may report the file name in another casing than configured; Windows file
	// names are case-insensitive, so keep the configured spelling then.
	if !strings.EqualFold(m.ZoneFile.ValueString(), z.ZoneFile) || m.ZoneFile.IsUnknown() {
		m.ZoneFile = zoneOptString(z.ZoneFile)
	}
	m.DynamicUpdate = types.StringValue(normalize.EnumOf(z.DynamicUpdate, zoneDynamicUpdates...))
	m.ADIntegrated = types.BoolValue(z.IsDsIntegrated)
	m.Reverse = types.BoolValue(z.IsReverseLookupZone)
	if m.ForceDestroy.IsNull() || m.ForceDestroy.IsUnknown() {
		m.ForceDestroy = types.BoolValue(false)
	}
}

// Create runs Add-DnsServerPrimaryZone with the canonical name (or the network ID) and stores
// the zone as read back.
func (r *zoneResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan zoneModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	in := dns.PrimaryZoneInput{
		ReplicationScope: plan.ReplicationScope.ValueString(),
		ZoneFile:         plan.ZoneFile.ValueString(),
		DynamicUpdate:    plan.DynamicUpdate.ValueString(),
	}
	if !plan.NetworkID.IsNull() {
		in.NetworkID = plan.NetworkID.ValueString()
	} else {
		in.Name = plan.Name.Canonical()
	}
	z, err := r.client.AddPrimaryZone(ctx, in)
	if err != nil {
		resp.Diagnostics.AddError(zoneErrSummary("create", "DNS zone "+plan.ID.ValueString()), err.Error())
		return
	}
	plan.apply(z)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// notPrimary reports, as an error, a zone that exists but is not a primary zone. Unlike a
// missing zone this is not drift: dropping it from state would make Terraform try to create
// a zone whose name is taken.
func notPrimary(name, zoneType string) (string, string) {
	return "DNS zone " + name + " is not a primary zone",
		fmt.Sprintf("Get-DnsServerZone reports zone type %q. windowsddi_dns_zone manages primary zones only; "+
			"use windowsddi_dns_conditional_forwarder for conditional forwarders.", zoneType)
}

// Read refreshes the zone; a missing zone is dropped from state (drift).
func (r *zoneResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state zoneModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	z, err := r.client.GetZone(ctx, state.ID.ValueString())
	if dns.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError(zoneErrSummary("read", "DNS zone "+state.ID.ValueString()), err.Error())
		return
	}
	if z.ZoneType != dns.ZoneTypePrimary {
		resp.Diagnostics.AddError(notPrimary(state.ID.ValueString(), z.ZoneType))
		return
	}
	state.apply(z)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update changes the replication scope and/or dynamic update mode in place. Only changed
// values are sent; other in-place changes (name spelling, network_id spelling,
// force_destroy) only touch state, so the zone is just read back.
func (r *zoneResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state zoneModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.ID.ValueString()
	var upd dns.PrimaryZoneUpdate
	if !plan.ReplicationScope.Equal(state.ReplicationScope) {
		upd.ReplicationScope = plan.ReplicationScope.ValueString()
	}
	if !plan.DynamicUpdate.Equal(state.DynamicUpdate) {
		upd.DynamicUpdate = plan.DynamicUpdate.ValueString()
	}
	var z *dns.Zone
	var err error
	if upd == (dns.PrimaryZoneUpdate{}) {
		z, err = r.client.GetZone(ctx, name)
	} else {
		z, err = r.client.SetPrimaryZone(ctx, name, upd)
	}
	if err != nil {
		resp.Diagnostics.AddError(zoneErrSummary("update", "DNS zone "+name), err.Error())
		return
	}
	plan.apply(z)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete removes the zone. Without force_destroy it first checks that the zone holds nothing
// but its apex SOA and NS records, so a `terraform destroy` cannot silently wipe records
// managed elsewhere. A zone that is already gone counts as deleted.
func (r *zoneResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state zoneModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	name := state.ID.ValueString()
	if !state.ForceDestroy.ValueBool() {
		has, err := r.client.ZoneHasUserRecords(ctx, name)
		if dns.IsNotFound(err) {
			return
		}
		if err != nil {
			resp.Diagnostics.AddError(zoneErrSummary("delete", "DNS zone "+name), err.Error())
			return
		}
		if has {
			resp.Diagnostics.AddError(zoneErrSummary("delete", "DNS zone "+name),
				"The zone holds records other than the apex SOA and NS records. Remove them first, "+
					"or set force_destroy = true and apply before destroying to delete the zone with all its records.")
			return
		}
	}
	if err := r.client.RemoveZone(ctx, name); err != nil && !dns.IsNotFound(err) {
		resp.Diagnostics.AddError(zoneErrSummary("delete", "DNS zone "+name), err.Error())
	}
}

// ImportState accepts the zone name, for example lab.example.local or 20.1.10.in-addr.arpa.
// Read then checks that the zone is a primary zone and fills in the rest.
func (r *zoneResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	canonical, err := normalize.ZoneName(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid Import ID", fmt.Sprintf("expected a zone name such as lab.example.local: %s", err))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), canonical)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), normalize.NewZoneNameValue(req.ID))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("force_destroy"), false)...)
}
