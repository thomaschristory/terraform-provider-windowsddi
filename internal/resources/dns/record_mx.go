// This file declares windowsddi_dns_mx_record_set: every MX record of one
// name, as a set of {preference, exchange} objects. The implementation is
// shared by all record resources (record_base.go).
//
// Cmdlets used (through the dns package): Get-DnsServerResourceRecord,
// Add-DnsServerResourceRecordMX, Set-DnsServerResourceRecord,
// Remove-DnsServerResourceRecord.

package dnsresources

import (
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/normalize"
)

// mxValue is one element of the mx set. Plain Go types are enough: both
// attributes are required, so an element is never null.
type mxValue struct {
	Preference int64  `tfsdk:"preference"`
	Exchange   string `tfsdk:"exchange"`
}

// NewMXRecordSet returns the windowsddi_dns_mx_record_set resource.
func NewMXRecordSet() resource.Resource {
	return newRecordResource(recordKind{
		typeSuffix: "_dns_mx_record_set",
		rrType:     dns.TypeMX,
		summary:    "Manages the MX (mail exchanger) records of one name in a zone, usually the apex (`@`).",
		valueAttr:  "mx",
		values: objectSetValues[mxValue]{
			description: "Mail exchangers, one MX record each.",
			attrs: map[string]schema.Attribute{
				"preference": schema.Int64Attribute{
					MarkdownDescription: "Preference (0 to 65535); lower values are tried first.",
					Required:            true,
					Validators:          []validator.Int64{int64validator.Between(0, 65535)},
				},
				"exchange": schema.StringAttribute{
					MarkdownDescription: "Mail server host name, for example `mail1.lab.example.local`. A trailing dot is optional and case is ignored.",
					Required:            true,
					Validators:          []validator.String{normalize.FQDNValidator()},
				},
			},
			attrTypes: map[string]attr.Type{"preference": types.Int64Type, "exchange": types.StringType},
			toRecord: func(m mxValue) dns.Record {
				return dns.Record{Preference: m.Preference, Exchange: m.Exchange}
			},
			fromRecord: func(r dns.Record) mxValue {
				return mxValue{Preference: r.Preference, Exchange: r.Exchange}
			},
		},
	})
}
