// This file holds the three recordValues codecs (see record_base.go): the
// value attribute shapes the record resources use.
//
//   - stringSetValues: set(string), for A and AAAA addresses and TXT strings.
//   - hostnameValue: a single hostname string, for CNAME and PTR targets.
//   - objectSetValues: a set of objects, for MX and SRV.
//
// Spelling: hostnames and IPv6 addresses have several spellings for one
// value. The codecs store whatever recordset.PreserveSpelling returns (the
// spelling from the previous state or plan when the server value is the
// same, canonical otherwise), so plans stay empty when the user writes
// "Mail.Example.com" and the server stores "mail.example.com.".

package dnsresources

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/normalize"
)

// stringSetValues is a required set(string) value attribute. toRecord and
// fromRecord map one element to and from the record's data field.
type stringSetValues struct {
	description string
	elem        []validator.String
	toRecord    func(string) dns.Record
	fromRecord  func(dns.Record) string
}

func (v stringSetValues) attribute() schema.Attribute {
	return schema.SetAttribute{
		MarkdownDescription: v.description,
		Required:            true,
		ElementType:         types.StringType,
		Validators: []validator.Set{
			setvalidator.SizeAtLeast(1),
			setvalidator.ValueStringsAre(v.elem...),
		},
	}
}

// get reads the set into records.
//
// Go note: ElementsAs copies the set's elements into a Go slice; the
// framework converts each Terraform string to a Go string.
func (v stringSetValues) get(ctx context.Context, src attrGetter, p path.Path) ([]dns.Record, diag.Diagnostics) {
	var set types.Set
	diags := src.GetAttribute(ctx, p, &set)
	if diags.HasError() || set.IsNull() || set.IsUnknown() {
		return nil, diags
	}
	var strs []string
	diags.Append(set.ElementsAs(ctx, &strs, false)...)
	recs := make([]dns.Record, len(strs))
	for i, s := range strs {
		recs[i] = v.toRecord(s)
	}
	return recs, diags
}

func (v stringSetValues) set(ctx context.Context, dst *tfsdk.State, p path.Path, recs, _ []dns.Record) diag.Diagnostics {
	strs := make([]string, len(recs))
	for i, r := range recs {
		strs[i] = v.fromRecord(r)
	}
	set, diags := types.SetValueFrom(ctx, types.StringType, strs)
	diags.Append(dst.SetAttribute(ctx, p, set)...)
	return diags
}

// hostnameValue is a required single hostname (normalize.HostnameType, so
// case and the trailing dot never cause a diff), stored in Record.HostName.
type hostnameValue struct{ description string }

func (v hostnameValue) attribute() schema.Attribute {
	return schema.StringAttribute{
		MarkdownDescription: v.description,
		Required:            true,
		CustomType:          normalize.HostnameType{},
	}
}

func (v hostnameValue) get(ctx context.Context, src attrGetter, p path.Path) ([]dns.Record, diag.Diagnostics) {
	var h normalize.HostnameValue
	diags := src.GetAttribute(ctx, p, &h)
	if diags.HasError() || h.IsNull() || h.IsUnknown() {
		return nil, diags
	}
	return []dns.Record{{HostName: h.ValueString()}}, diags
}

// set stores one record. The server normally holds one CNAME or PTR per
// name, but nothing stops a second PTR from being added out of band. Then
// the record that differs from prior is shown, so the plan has a diff and
// Update (which makes the server hold exactly the configured value) removes
// the extra one.
func (v hostnameValue) set(ctx context.Context, dst *tfsdk.State, p path.Path, recs, prior []dns.Record) diag.Diagnostics {
	if len(recs) == 0 {
		return nil
	}
	pick := recs[0]
	for _, r := range recs {
		if len(prior) == 0 || !r.SameValue(dns.Record{Type: r.Type, HostName: prior[0].HostName}) {
			pick = r
			break
		}
	}
	return dst.SetAttribute(ctx, p, normalize.NewHostnameValue(pick.HostName))
}

// objectSetValues is a required set of objects (SetNestedAttribute). M is a
// Go struct whose `tfsdk` tags match attrs, for example mxValue; toRecord
// and fromRecord map it to and from the record's data fields.
//
// Go note: [M any] makes this a generic type: objectSetValues[mxValue] and
// objectSetValues[srvValue] are two concrete types sharing this code.
type objectSetValues[M any] struct {
	description string
	attrs       map[string]schema.Attribute
	attrTypes   map[string]attr.Type
	toRecord    func(M) dns.Record
	fromRecord  func(dns.Record) M
}

func (v objectSetValues[M]) attribute() schema.Attribute {
	return schema.SetNestedAttribute{
		MarkdownDescription: v.description,
		Required:            true,
		NestedObject:        schema.NestedAttributeObject{Attributes: v.attrs},
		Validators:          []validator.Set{setvalidator.SizeAtLeast(1)},
	}
}

func (v objectSetValues[M]) get(ctx context.Context, src attrGetter, p path.Path) ([]dns.Record, diag.Diagnostics) {
	var set types.Set
	diags := src.GetAttribute(ctx, p, &set)
	if diags.HasError() || set.IsNull() || set.IsUnknown() {
		return nil, diags
	}
	var ms []M
	diags.Append(set.ElementsAs(ctx, &ms, false)...)
	recs := make([]dns.Record, len(ms))
	for i, m := range ms {
		recs[i] = v.toRecord(m)
	}
	return recs, diags
}

func (v objectSetValues[M]) set(ctx context.Context, dst *tfsdk.State, p path.Path, recs, _ []dns.Record) diag.Diagnostics {
	ms := make([]M, len(recs))
	for i, r := range recs {
		ms[i] = v.fromRecord(r)
	}
	set, diags := types.SetValueFrom(ctx, types.ObjectType{AttrTypes: v.attrTypes}, ms)
	diags.Append(dst.SetAttribute(ctx, p, set)...)
	return diags
}
