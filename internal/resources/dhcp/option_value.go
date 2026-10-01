// This file implements windowsddi_dhcp_option_value: one DHCP option (router, DNS servers,
// domain name, ...) set at one level: server-wide, on a scope, or on a reservation,
// optionally for a vendor class and/or user class.
//
// See scope.go for how the framework pieces (model struct, Schema, Configure, the CRUD
// methods, diagnostics, drift handling, ImportState) work. What is special here:
//   - The "level" is chosen by which optional attributes are set (scope_id, reserved_ip or
//     neither), and the identity is the tuple (level, option_id, vendor_class, user_class).
//   - value is a list of strings, not a single string.
//   - The server may echo values back in another spelling (hex vs decimal, letter case), so
//     apply keeps the configured spelling when the values are equivalent.
//   - Create and Update are the same operation (Set-DhcpServerv4OptionValue overwrites).
//
// Cmdlets used (through the dhcp package): Get-, Set- and Remove-DhcpServerv4OptionValue.

package dhcpresources

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dhcp"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/normalize"
)

// Compile-time checks that optionValueResource supports Configure and import.
//
// Go note: assigning a value to `_` (the blank identifier) only makes the compiler check
// that the type implements each interface; nothing is stored.
var (
	_ resource.ResourceWithConfigure   = &optionValueResource{}
	_ resource.ResourceWithImportState = &optionValueResource{}
)

// NewOptionValue returns the windowsddi_dhcp_option_value resource.
//
// Go note: `&optionValueResource{}` creates an empty struct and returns a pointer to it.
func NewOptionValue() resource.Resource { return &optionValueResource{} }

// optionValueResource holds the shared DHCP client, set by Configure.
type optionValueResource struct{ client *dhcp.Client }

// optionValueModel mirrors one windowsddi_dhcp_option_value block. Each `tfsdk:"..."` struct
// tag names the HCL attribute the field maps to (see scopeModel in scope.go).
//
// types.Int64 holds a Terraform number (option_id) and types.List holds a Terraform list;
// like types.String they can also be null or unknown. scope_id, reserved_ip, vendor_class
// and user_class are Optional without a default, so they are null when not set in HCL.
type optionValueModel struct {
	ID          types.String `tfsdk:"id"`
	OptionID    types.Int64  `tfsdk:"option_id"`
	Value       types.List   `tfsdk:"value"`
	ScopeID     types.String `tfsdk:"scope_id"`
	ReservedIP  types.String `tfsdk:"reserved_ip"`
	VendorClass types.String `tfsdk:"vendor_class"`
	UserClass   types.String `tfsdk:"user_class"`
	Name        types.String `tfsdk:"name"`
}

// Metadata sets the type name to "windowsddi_dhcp_option_value".
//
// Go note: `(r *optionValueResource)` before the name makes this a method of that type.
func (r *optionValueResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_dhcp_option_value"
}

// Schema defines the attributes (see scope.go for the meaning of the flags).
//
// Everything that identifies the option value (option_id, scope_id, reserved_ip,
// vendor_class, user_class) uses RequiresReplace: changing any of them means a different
// option on the server, so Terraform removes the old one and sets the new one. Only value
// can change in place. ConflictsWith makes `terraform validate` reject a block that sets
// both scope_id and reserved_ip (declaring it on one side is enough to catch the pair).
func (r *optionValueResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	// Shared plan modifier list for the string identity attributes.
	//
	// Go note: `:=` declares a local variable and infers its type from the value.
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages one DHCP option value at server, scope or reservation level (`Set-DhcpServerv4OptionValue`, `Remove-DhcpServerv4OptionValue`).\n\n" +
			"Set `scope_id` for a scope option, `reserved_ip` for a reservation option, or neither for a server option. " +
			"Only the targeted (level, option, class) combination is managed: values set out of band at other levels, or by policies, are left alone. " +
			"Creating the resource overwrites a value that already exists at that level.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "Import ID: `server/<option_id>`, `scope/<scope_id>/<option_id>` or `reservation/<reserved_ip>/<option_id>`, optionally followed by `/<vendor_class>/<user_class>` (either may be empty).",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"option_id": schema.Int64Attribute{
				MarkdownDescription: "Option number, for example `3` (router), `6` (DNS servers) or `15` (DNS domain name). The option definition must exist on the server.",
				Required:            true,
				Validators:          []validator.Int64{int64validator.Between(1, 255)},
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			},
			// A list (ordered), not a set: the order of DNS servers or routers matters to
			// clients. ElementType says every element is a string.
			"value": schema.ListAttribute{
				MarkdownDescription: "Option values, as strings. Order matters (for example DNS servers). IP addresses as dotted quads, numbers as decimal or hex, binary data as hex.",
				Required:            true,
				ElementType:         types.StringType,
				Validators:          []validator.List{listvalidator.SizeAtLeast(1)},
			},
			// path.MatchRoot("reserved_ip") points the ConflictsWith validator at the
			// other top-level attribute.
			"scope_id": schema.StringAttribute{
				MarkdownDescription: "Set the option on this scope. Conflicts with `reserved_ip`.",
				Optional:            true,
				Validators: []validator.String{
					normalize.IPv4Validator(),
					stringvalidator.ConflictsWith(path.MatchRoot("reserved_ip")),
				},
				PlanModifiers: replace,
			},
			"reserved_ip": schema.StringAttribute{
				MarkdownDescription: "Set the option on the reservation for this IPv4 address. Conflicts with `scope_id`.",
				Optional:            true,
				Validators:          []validator.String{normalize.IPv4Validator()},
				PlanModifiers:       replace,
			},
			// LengthAtLeast(1) rejects "": an unset class must be written by omitting the
			// attribute (null), which keeps a single spelling for "no class".
			"vendor_class": schema.StringAttribute{
				MarkdownDescription: "Vendor class the option value applies to. Unset means the standard options.",
				Optional:            true,
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
				PlanModifiers:       replace,
			},
			"user_class": schema.StringAttribute{
				MarkdownDescription: "User class the option value applies to. Unset means the default user class.",
				Optional:            true,
				Validators:          []validator.String{stringvalidator.LengthAtLeast(1)},
				PlanModifiers:       replace,
			},
			// Read-only information from the server's option definition.
			"name": schema.StringAttribute{
				MarkdownDescription: "Option name as reported by the server, for example `DNS Servers`.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

// Configure stores the provider's shared *dhcp.Client (see clientFrom in common.go).
func (r *optionValueResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

// key extracts the identity of the option value as a dhcp.OptionKey. Null optional
// attributes become "" (ValueString returns "" for null), which the dhcp layer reads as
// "not set": no scope and no reservation means server level, no class means default class.
func (m *optionValueModel) key() dhcp.OptionKey {
	return dhcp.OptionKey{
		OptionID:    m.OptionID.ValueInt64(),
		ScopeID:     m.ScopeID.ValueString(),
		ReservedIP:  m.ReservedIP.ValueString(),
		VendorClass: m.VendorClass.ValueString(),
		UserClass:   m.UserClass.ValueString(),
	}
}

// optionValueID builds the Terraform id (and import ID) from a key, for example
// "server/15", "scope/10.1.2.0/6", "reservation/10.1.2.50/3" or, with classes,
// "scope/10.1.2.0/6/Vendor A/" (vendor class set, user class empty).
//
// Go note: strings.Builder efficiently assembles a string piece by piece. A `switch` with
// no value after it acts like an if/else-if chain: the first true `case` runs (and only
// that one; Go cases do not fall through).
func optionValueID(k dhcp.OptionKey) string {
	var b strings.Builder
	// Level prefix.
	switch {
	case k.ReservedIP != "":
		b.WriteString("reservation/" + k.ReservedIP + "/")
	case k.ScopeID != "":
		b.WriteString("scope/" + k.ScopeID + "/")
	default:
		b.WriteString("server/")
	}
	// Option number, in decimal.
	b.WriteString(strconv.FormatInt(k.OptionID, 10))
	// Classes are appended as a pair only when at least one is set.
	if k.VendorClass != "" || k.UserClass != "" {
		b.WriteString("/" + k.VendorClass + "/" + k.UserClass)
	}
	return b.String()
}

// parseOptionValueID is the reverse of optionValueID: it parses an import ID into a key,
// or returns an error describing the accepted formats.
//
// Go note: the function returns two values, the key and an error. A nil error means
// success. fmt.Errorf builds an error value from a format string.
func parseOptionValueID(id string) (dhcp.OptionKey, error) {
	// Split on "/" and prepare the error returned for any malformed input.
	//
	// Go note: strings.Split returns a slice; parts[1:] is the sub-slice from index 1 to
	// the end (everything after the level word). len(x) is the number of elements.
	var k dhcp.OptionKey
	parts := strings.Split(id, "/")
	bad := fmt.Errorf("expected server/<option_id>, scope/<scope_id>/<option_id> or reservation/<reserved_ip>/<option_id>, optionally followed by /<vendor_class>/<user_class>; got %q", id)
	if len(parts) < 2 {
		return k, bad
	}
	rest := parts[1:]
	// The first part picks the level. "server" needs nothing more; "scope" and
	// "reservation" are followed by an IPv4 address, which is consumed from rest.
	switch parts[0] {
	case "server":
	case "scope", "reservation":
		if len(rest) < 2 {
			return k, bad
		}
		if _, err := normalize.ParseIPv4(rest[0]); err != nil {
			return k, bad
		}
		if parts[0] == "scope" {
			k.ScopeID = rest[0]
		} else {
			k.ReservedIP = rest[0]
		}
		rest = rest[1:]
	default:
		return k, bad
	}
	// Next comes the option number (1-255, matching the schema validator).
	n, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil || n < 1 || n > 255 {
		return k, bad
	}
	k.OptionID = n
	// Then either nothing, or exactly two more parts: vendor class and user class (each
	// may be empty, as in "scope/10.1.2.0/6/Vendor A/").
	switch len(rest) {
	case 1:
	case 3:
		// Go note: Go can assign several variables at once: a, b = x, y.
		k.VendorClass, k.UserClass = rest[1], rest[2]
	default:
		return k, bad
	}
	return k, nil
}

// apply copies the server's view into the model. The server may spell a
// value differently from the configuration (hex as decimal, letter case), so
// the current value list is kept when it is equivalent.
//
// For example the user writes "0x0A" or "contoso.COM" and Get-DhcpServerv4OptionValue
// returns "10" or "contoso.com". Storing the server's spelling would make every plan show a
// change back to the configured spelling, so the configured list is kept unless the values
// really differ (then the server's list goes into state, and the plan shows real drift).
// After import there is no current list, so the server's values are always used.
func (m *optionValueModel) apply(ctx context.Context, v *dhcp.OptionValue) error {
	// Replace the list only when the server's values genuinely differ.
	current, err := m.values(ctx)
	if err != nil || !optionValuesEquivalent(current, v.Value) {
		// Convert the Go []string into a Terraform list value. Framework conversions return
		// diagnostics instead of a Go error, so they are wrapped into one here.
		list, diags := types.ListValueFrom(ctx, types.StringType, v.Value)
		if diags.HasError() {
			return fmt.Errorf("converting option value: %v", diags)
		}
		m.Value = list
	}
	// name comes from the server; id is rebuilt from the identity attributes.
	m.Name = types.StringValue(v.Name)
	m.ID = types.StringValue(optionValueID(m.key()))
	return nil
}

// optionValuesEquivalent compares option values element by element,
// ignoring letter case and treating numbers written in decimal or hex
// ("10", "0x0A") as equal.
//
// Order matters: the same values in a different order are not equivalent.
//
// Go note: `for i := range a` loops over the indexes of slice a. `continue` skips to the
// next element.
func optionValuesEquivalent(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		// Same text ignoring case (covers hex digits, domain names, IP addresses).
		if strings.EqualFold(a[i], b[i]) {
			continue
		}
		// Otherwise both must parse as the same unsigned number.
		x, errX := parseOptionNumber(a[i])
		y, errY := parseOptionNumber(b[i])
		if errX != nil || errY != nil || x != y {
			return false
		}
	}
	return true
}

// parseOptionNumber reads a number the way DHCP option values are written:
// "0x" or "0X" means hex, anything else is decimal. A leading zero does not
// mean octal ("010" is ten), unlike Go's own base-0 parsing.
func parseOptionNumber(s string) (uint64, error) {
	if hex, ok := strings.CutPrefix(strings.ToLower(s), "0x"); ok {
		return strconv.ParseUint(hex, 16, 64)
	}
	return strconv.ParseUint(s, 10, 64)
}

// values returns the value list as a plain Go []string, or nil when the list is null or
// unknown (for example in the state seeded by ImportState, before Read has run).
//
// Go note: `var out []string` declares an empty (nil) slice. ElementsAs fills it through
// the pointer &out. The final `false` means "do not allow unhandled null/unknown elements".
func (m *optionValueModel) values(ctx context.Context) ([]string, error) {
	var out []string
	if m.Value.IsNull() || m.Value.IsUnknown() {
		return nil, nil
	}
	if diags := m.Value.ElementsAs(ctx, &out, false); diags.HasError() {
		return nil, fmt.Errorf("reading value: %v", diags)
	}
	return out, nil
}

// set is the shared body of Create and Update: Set-DhcpServerv4OptionValue both creates
// and overwrites, so the two operations are identical. It sends the planned values, then
// applies the server's answer to the model (which the caller saves as state).
//
// Go note: m is a pointer (*optionValueModel), so the changes apply makes are visible to
// the caller's plan variable.
func (r *optionValueResource) set(ctx context.Context, m *optionValueModel) error {
	vals, err := m.values(ctx)
	if err != nil {
		return err
	}
	v, err := r.client.SetOptionValue(ctx, m.key(), vals)
	if err != nil {
		return err
	}
	return m.apply(ctx, v)
}

// Create sets the option value. If a value already exists at that level it is overwritten
// (no import needed first), as the resource description says.
func (r *optionValueResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	// Read the plan.
	//
	// Go note: `&plan` passes a pointer so Get can fill the struct; `...` spreads the
	// returned diagnostics list into Append's variadic parameter.
	var plan optionValueModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Push to the server and fold the answer into plan.
	//
	// Go note: `if err := ...; err != nil` runs the call and checks the error in one line.
	if err := r.set(ctx, &plan); err != nil {
		resp.Diagnostics.AddError(errSummary("set", "option value "+optionValueID(plan.key())), err.Error())
		return
	}
	// Save as state.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read refreshes the option value. If it no longer exists at that level, the resource is
// removed from state (drift). Values at other levels or from policies are never looked at.
func (r *optionValueResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state optionValueModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	v, err := r.client.GetOptionValue(ctx, state.key())
	// Gone on the server: drop from state.
	if dhcp.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	// On success, merge the server's view into state; apply can fail too, so both error
	// sources funnel into the single check below.
	if err == nil {
		err = state.apply(ctx, v)
	}
	if err != nil {
		resp.Diagnostics.AddError(errSummary("read", "option value "+optionValueID(state.key())), err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update changes the value list in place (every other attribute forces replacement). It is
// the same operation as Create.
func (r *optionValueResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan optionValueModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.set(ctx, &plan); err != nil {
		resp.Diagnostics.AddError(errSummary("set", "option value "+optionValueID(plan.key())), err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete removes the option value at its level only (Remove-DhcpServerv4OptionValue). An
// already-missing value counts as deleted.
func (r *optionValueResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state optionValueModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.RemoveOptionValue(ctx, state.key()); err != nil && !dhcp.IsNotFound(err) {
		resp.Diagnostics.AddError(errSummary("delete", "option value "+optionValueID(state.key())), err.Error())
	}
}

// ImportState accepts the formats documented on the id attribute, for example
// "server/15", "scope/10.1.2.0/6", "reservation/10.1.2.50/3" or "scope/10.1.2.0/6/Vendor A/".
// It writes the identity attributes into state; Terraform then calls Read to fetch value
// and name. id is rebuilt from the parsed key so it always has the canonical form.
func (r *optionValueResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Parse and validate the import ID.
	k, err := parseOptionValueID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid Import ID", err.Error())
		return
	}
	// optStr turns "" into a null Terraform string, matching what an omitted optional
	// attribute looks like in HCL, so the imported state does not diff against the config.
	//
	// Go note: this is a closure, a small function stored in a local variable.
	optStr := func(s string) types.String {
		if s == "" {
			return types.StringNull()
		}
		return types.StringValue(s)
	}
	// Seed state with the identity attributes.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), optionValueID(k))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("option_id"), k.OptionID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("scope_id"), optStr(k.ScopeID))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("reserved_ip"), optStr(k.ReservedIP))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("vendor_class"), optStr(k.VendorClass))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("user_class"), optStr(k.UserClass))...)
}
