// This file holds the code shared by the seven DNS record resources
// (windowsddi_dns_a_record_set, _aaaa_record_set, _cname_record, _ptr_record,
// _mx_record_set, _srv_record_set, _txt_record_set). For the framework
// basics (Schema, Configure, the CRUD methods, plan modifiers, state) read
// internal/resources/dhcp/scope.go first; this file only explains what is
// specific to record sets.
//
// # One generic resource, seven kinds
//
// All record resources behave the same way: they own every record of one
// (zone, name, type) tuple and differ only in the record type and in the
// shape of the attribute holding the values (a set of strings, a single
// hostname, or a set of objects). So there is a single resource
// implementation, recordResource, parameterised by a recordKind that names
// the type and supplies a recordValues "codec": the piece that converts the
// value attribute between Terraform values and dns.Record. Each record_*.go
// file only declares its kind.
//
// The CRUD logic itself lives in the recordset engine (internal/recordset):
// these methods translate between Terraform and dns.Record and call it.
//
// # Reading and writing attributes one by one
//
// The DHCP resources load the whole plan or state into a model struct with
// Get. That needs one struct per resource, because a struct's `tfsdk` tags
// must list every attribute. Here the value attribute has a different name
// and type per kind, so the common attributes are read individually with
// GetAttribute (recordCommon below) and the value attribute by the codec.

package dnsresources

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/normalize"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/recordset"
)

// Compile-time interface checks (see scope.go).
var (
	_ resource.ResourceWithConfigure   = &recordResource{}
	_ resource.ResourceWithImportState = &recordResource{}
)

// recordKind describes one record resource.
//
//   - typeSuffix: the resource type name after the provider prefix, for
//     example "_dns_a_record_set".
//   - rrType: the record type it manages (dns.TypeA, ...).
//   - summary: the first sentence of the resource description.
//   - valueAttr: the name of the attribute holding the values ("addresses").
//   - values: the codec for that attribute.
type recordKind struct {
	typeSuffix string
	rrType     string
	summary    string
	valueAttr  string
	values     recordValues
}

// recordValues converts the value attribute of a record resource between
// Terraform and dns.Record. The records it returns or receives only carry
// the data fields for the type (Address, HostName, ...); the engine fills in
// name, type and TTL.
//
// Go note: an interface lists methods; any type with these methods can be
// used where a recordValues is expected. The implementations are in
// record_values.go.
type recordValues interface {
	// attribute returns the schema of the value attribute.
	attribute() schema.Attribute
	// get reads the value attribute from a plan or state. A null value (for
	// example right after import) gives nil.
	get(ctx context.Context, src attrGetter, p path.Path) ([]dns.Record, diag.Diagnostics)
	// set writes records (already passed through recordset.PreserveSpelling)
	// into state. prior is the previous value, used by single-value kinds to
	// decide which record to show when the server holds several.
	set(ctx context.Context, dst *tfsdk.State, p path.Path, recs, prior []dns.Record) diag.Diagnostics
}

// attrGetter is what tfsdk.Plan and tfsdk.State have in common for reading
// one attribute, so the same helpers serve Create (plan) and Read (state).
type attrGetter interface {
	GetAttribute(ctx context.Context, p path.Path, target any) diag.Diagnostics
}

// recordResource is the single implementation behind every record resource.
type recordResource struct {
	kind   recordKind
	client *dns.Client
}

// newRecordResource returns a new resource of kind k. Each New*Record*
// function in the record_*.go files calls it with its kind.
func newRecordResource(k recordKind) resource.Resource { return &recordResource{kind: k} }

// recordCommon holds the attributes every record resource has.
type recordCommon struct {
	ZoneName              normalize.ZoneNameValue
	Name                  normalize.RecordNameValue
	TTL                   types.Int64
	AllowOverwriteDynamic types.Bool
}

// getRecordCommon reads the common attributes from a plan or state.
func getRecordCommon(ctx context.Context, src attrGetter) (recordCommon, diag.Diagnostics) {
	var c recordCommon
	var diags diag.Diagnostics
	diags.Append(src.GetAttribute(ctx, path.Root("zone_name"), &c.ZoneName)...)
	diags.Append(src.GetAttribute(ctx, path.Root("name"), &c.Name)...)
	diags.Append(src.GetAttribute(ctx, path.Root("ttl"), &c.TTL)...)
	diags.Append(src.GetAttribute(ctx, path.Root("allow_overwrite_dynamic"), &c.AllowOverwriteDynamic)...)
	return c, diags
}

// tuple returns the engine's key for these attributes, in canonical form
// (lowercase zone without trailing dot, lowercase name, "@" for the apex),
// whatever spelling the user wrote.
func (c recordCommon) tuple(rrType string) recordset.Tuple {
	return recordset.Tuple{Zone: c.ZoneName.Canonical(), Name: c.Name.Canonical(), Type: rrType}
}

// ttl returns the TTL to send: the planned value, or 0 ("zone default" on
// create, "leave alone" on update) when it is unknown or null.
func (c recordCommon) ttl() int64 {
	if c.TTL.IsNull() || c.TTL.IsUnknown() {
		return 0
	}
	return c.TTL.ValueInt64()
}

// Metadata names the resource, for example windowsddi_dns_a_record_set.
func (r *recordResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + r.kind.typeSuffix
}

// Schema returns the common attributes plus the kind's value attribute.
//
// Notes on the common attributes:
//   - zone_name and name use the normalize custom types, so "Example.COM."
//     and "example.com" are the same zone (no diff, no replacement). Both
//     force replacement when really changed: they are the tuple's identity.
//   - ttl is Optional + Computed: when the user leaves it out, the zone
//     default applies on create and the server's value is stored.
//     UseStateForUnknown keeps the stored TTL in later plans instead of
//     showing "(known after apply)" on every change.
func (r *recordResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: r.kind.summary + "\n\n" +
			"The resource is authoritative for every " + r.kind.rrType + " record of its name: records of that name and type that are not in the configuration are removed. " +
			"Creating it fails when such records already exist, so existing records are adopted with `terraform import` instead; " +
			"dynamic records (registered by clients or the DHCP server) can be replaced with `allow_overwrite_dynamic`.\n\n" +
			updateNote(r.kind.rrType),
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "`<zone_name>/<name>` in lowercase, the import ID.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"zone_name": schema.StringAttribute{
				MarkdownDescription: "Zone holding the records, for example `lab.example.local`. Case and a trailing dot are ignored. Changing it replaces the resource.",
				Required:            true,
				CustomType:          normalize.ZoneNameType{},
				PlanModifiers:       []planmodifier.String{recordReplaceIfRenamed(normalize.ZoneName)},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Record name relative to the zone, for example `www`, or `@` for the zone apex. Case is ignored. Changing it replaces the resource.",
				Required:            true,
				CustomType:          normalize.RecordNameType{},
				PlanModifiers:       []planmodifier.String{recordReplaceIfRenamed(normalize.RecordName)},
			},
			"ttl": schema.Int64Attribute{
				MarkdownDescription: fmt.Sprintf("Time to live in seconds, applied to every record of the set (1 to %d). When unset, the zone default is used on create and the server's value is reported.", normalize.MaxTTL),
				Optional:            true,
				Computed:            true,
				Validators:          []validator.Int64{int64validator.Between(1, normalize.MaxTTL)},
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"allow_overwrite_dynamic": schema.BoolAttribute{
				MarkdownDescription: "When `true`, creating the resource replaces existing dynamic records of the name (registered by clients or the DHCP server) instead of failing. Static records are never overwritten. Defaults to `false`. Terraform only, not read from the server.",
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
			},
			r.kind.valueAttr: r.kind.values.attribute(),
		},
	}
}

// recordReplaceIfRenamed forces replacement when zone_name or name really
// changes, but not when only the spelling changes ("WWW" to "www",
// "Example.com." to "example.com"): canon maps both values to their
// canonical form before comparing. A spelling change is then an in-place
// update that sends nothing to the server.
func recordReplaceIfRenamed(canon func(string) (string, error)) planmodifier.String {
	return stringplanmodifier.RequiresReplaceIf(
		func(_ context.Context, req planmodifier.StringRequest, resp *stringplanmodifier.RequiresReplaceIfFuncResponse) {
			if req.PlanValue.IsUnknown() || req.StateValue.IsNull() {
				resp.RequiresReplace = !req.StateValue.IsNull()
				return
			}
			a, errA := canon(req.PlanValue.ValueString())
			b, errB := canon(req.StateValue.ValueString())
			resp.RequiresReplace = errA != nil || errB != nil || a != b
		},
		"Changing the name replaces the resource; changes in casing (or a trailing dot on the zone) do not.",
		"Changing the name replaces the resource; changes in casing (or a trailing dot on the zone) do not.",
	)
}

// Configure stores the DNS client (see scope.go).
func (r *recordResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

// valuePath is the path of the kind's value attribute.
func (r *recordResource) valuePath() path.Path { return path.Root(r.kind.valueAttr) }

// updateNote describes how Update changes the server for a record type. CNAME
// differs because a name holds a single CNAME: the old target has to go first.
func updateNote(rrType string) string {
	if rrType == dns.TypeCNAME {
		return "Changing `target` removes the old record before adding the new one (the server allows a single CNAME per name), " +
			"so the name briefly does not resolve; if the add fails, the old record is restored."
	}
	return "Updates are not atomic: new values are added first, then TTLs are updated, then old values are removed, so the name never resolves empty. " +
		"A failure midway leaves a mix of old and new values, which the next plan shows and corrects."
}

// recordErrSummary builds the summary line of an error, for example
// `Unable to create A records "www" in zone example.com`.
func recordErrSummary(action string, t recordset.Tuple) string {
	return fmt.Sprintf("Unable to %s %s", action, t)
}

// refresh reads the tuple from the server into st: the value attribute
// (keeping the spelling of prior values, see recordset.PreserveSpelling),
// ttl and id. It returns false when the tuple has no records or the zone is
// gone. priorTTL drives the choice when the records disagree on TTL (see
// recordset.Read).
func (r *recordResource) refresh(ctx context.Context, t recordset.Tuple, prior []dns.Record, priorTTL int64, st *tfsdk.State, diags *diag.Diagnostics) bool {
	recs, ttl, err := recordset.Read(ctx, r.client, t, priorTTL)
	if dns.IsNotFound(err) {
		return false
	}
	if err != nil {
		diags.AddError(recordErrSummary("read", t), err.Error())
		return false
	}
	recs = recordset.PreserveSpelling(prior, recs)
	diags.Append(r.kind.values.set(ctx, st, r.valuePath(), recs, prior)...)
	diags.Append(st.SetAttribute(ctx, path.Root("ttl"), ttl)...)
	diags.Append(st.SetAttribute(ctx, path.Root("id"), t.ImportID())...)
	return true
}

// Create checks that the tuple is free (or only holds dynamic records and
// allow_overwrite_dynamic is set), adds every configured value, and reads
// the result back.
//
// State starts as a copy of the plan (resp.State.Raw = req.Plan.Raw), so
// attributes refresh does not touch (zone_name, name, the flag) keep the
// planned values; refresh then fills in the server's view.
func (r *recordResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	common, diags := getRecordCommon(ctx, req.Plan)
	resp.Diagnostics.Append(diags...)
	values, diags := r.kind.values.get(ctx, req.Plan, r.valuePath())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	t := common.tuple(r.kind.rrType)
	if err := recordset.Create(ctx, r.client, t, values, common.ttl(), common.AllowOverwriteDynamic.ValueBool()); err != nil {
		resp.Diagnostics.AddError(recordErrSummary("create", t), err.Error())
		return
	}
	resp.State.Raw = req.Plan.Raw.Copy()
	if !r.refresh(ctx, t, values, common.ttl(), &resp.State, &resp.Diagnostics) && !resp.Diagnostics.HasError() {
		resp.Diagnostics.AddError(recordErrSummary("create", t), "the records were added but are not on the server when read back")
	}
}

// Read refreshes the values and TTL. No records (or no zone) removes the
// resource from state, so the next plan proposes to create it again.
func (r *recordResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	common, diags := getRecordCommon(ctx, req.State)
	resp.Diagnostics.Append(diags...)
	prior, diags := r.kind.values.get(ctx, req.State, r.valuePath())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	found := r.refresh(ctx, common.tuple(r.kind.rrType), prior, common.ttl(), &resp.State, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	// After import the flag has no value yet; give it its default.
	if common.AllowOverwriteDynamic.IsNull() {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("allow_overwrite_dynamic"), false)...)
	}
}

// Update makes the server match the planned values and TTL (add, set TTL,
// remove: see recordset.Update) and reads the result back.
func (r *recordResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	common, diags := getRecordCommon(ctx, req.Plan)
	resp.Diagnostics.Append(diags...)
	values, diags := r.kind.values.get(ctx, req.Plan, r.valuePath())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	t := common.tuple(r.kind.rrType)
	if err := recordset.Update(ctx, r.client, t, values, common.ttl()); err != nil {
		resp.Diagnostics.AddError(recordErrSummary("update", t), err.Error())
		return
	}
	resp.State.Raw = req.Plan.Raw.Copy()
	if !r.refresh(ctx, t, values, common.ttl(), &resp.State, &resp.Diagnostics) && !resp.Diagnostics.HasError() {
		resp.Diagnostics.AddError(recordErrSummary("update", t), "the records are not on the server when read back")
	}
}

// Delete removes every record of the tuple. A missing zone counts as deleted.
func (r *recordResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	common, diags := getRecordCommon(ctx, req.State)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	t := common.tuple(r.kind.rrType)
	if err := recordset.Delete(ctx, r.client, t); err != nil {
		resp.Diagnostics.AddError(recordErrSummary("delete", t), err.Error())
	}
}

// ImportState accepts `<zone>/<name>`, for example `example.com/www` or
// `example.com/@`. It splits on the first "/" (zone names never contain
// one), stores the canonical zone and name, and lets Read fetch the values.
//
// Go note: strings.Cut splits around the first separator and reports
// whether it was found.
func (r *recordResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	zoneRaw, nameRaw, ok := strings.Cut(req.ID, "/")
	zone, errZ := normalize.ZoneName(zoneRaw)
	name, errN := normalize.RecordName(nameRaw)
	if !ok || errZ != nil || errN != nil {
		resp.Diagnostics.AddError("Invalid Import ID",
			fmt.Sprintf("expected <zone>/<name>, for example example.com/www or example.com/@ for the apex, got %q", req.ID))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), zone+"/"+name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("zone_name"), normalize.NewZoneNameValue(zone))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), normalize.NewRecordNameValue(name))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("allow_overwrite_dynamic"), false)...)
}
