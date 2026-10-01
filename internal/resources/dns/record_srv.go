// This file declares windowsddi_dns_srv_record_set: every SRV record of one
// name (for example `_sip._tcp`), as a set of {priority, weight, port,
// target} objects. The implementation is shared by all record resources
// (record_base.go).
//
// Cmdlets used (through the dns package): Get-DnsServerResourceRecord,
// Add-DnsServerResourceRecord -Srv, Set-DnsServerResourceRecord,
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

// srvValue is one element of the srv set.
type srvValue struct {
	Priority int64  `tfsdk:"priority"`
	Weight   int64  `tfsdk:"weight"`
	Port     int64  `tfsdk:"port"`
	Target   string `tfsdk:"target"`
}

// NewSRVRecordSet returns the windowsddi_dns_srv_record_set resource.
func NewSRVRecordSet() resource.Resource {
	// u16 builds a required 0..65535 number attribute.
	u16 := func(desc string) schema.Attribute {
		return schema.Int64Attribute{
			MarkdownDescription: desc,
			Required:            true,
			Validators:          []validator.Int64{int64validator.Between(0, 65535)},
		}
	}
	return newRecordResource(recordKind{
		typeSuffix: "_dns_srv_record_set",
		rrType:     dns.TypeSRV,
		summary:    "Manages the SRV (service locator) records of one name in a zone, for example `_sip._tcp`.",
		valueAttr:  "srv",
		values: objectSetValues[srvValue]{
			description: "Service targets, one SRV record each.",
			attrs: map[string]schema.Attribute{
				"priority": u16("Priority (0 to 65535); lower values are tried first."),
				"weight":   u16("Relative weight (0 to 65535) among targets of the same priority."),
				"port":     u16("TCP or UDP port of the service (0 to 65535)."),
				"target": schema.StringAttribute{
					MarkdownDescription: "Host name providing the service, for example `sip1.lab.example.local`. A trailing dot is optional and case is ignored.",
					Required:            true,
					Validators:          []validator.String{normalize.FQDNValidator()},
				},
			},
			attrTypes: map[string]attr.Type{
				"priority": types.Int64Type, "weight": types.Int64Type, "port": types.Int64Type, "target": types.StringType,
			},
			toRecord: func(m srvValue) dns.Record {
				return dns.Record{Priority: m.Priority, Weight: m.Weight, Port: m.Port, Target: m.Target}
			},
			fromRecord: func(r dns.Record) srvValue {
				return srvValue{Priority: r.Priority, Weight: r.Weight, Port: r.Port, Target: r.Target}
			},
		},
	})
}
