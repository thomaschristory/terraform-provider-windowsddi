// This file declares windowsddi_dns_aaaa_record_set: every AAAA record of one
// name. The implementation is shared by all record resources
// (record_base.go).
//
// Cmdlets used (through the dns package): Get-DnsServerResourceRecord,
// Add-DnsServerResourceRecordAAAA, Set-DnsServerResourceRecord,
// Remove-DnsServerResourceRecord.

package dnsresources

import (
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/normalize"
)

// NewAAAARecordSet returns the windowsddi_dns_aaaa_record_set resource.
func NewAAAARecordSet() resource.Resource {
	return newRecordResource(recordKind{
		typeSuffix: "_dns_aaaa_record_set",
		rrType:     dns.TypeAAAA,
		summary:    "Manages the AAAA (IPv6 address) records of one name in a zone.",
		valueAttr:  "addresses",
		values: stringSetValues{
			description: "IPv6 addresses, one AAAA record each. Compared by value, so `2001:DB8:0::1` and `2001:db8::1` are the same address and never show a diff.",
			elem:        []validator.String{normalize.IPv6Validator()},
			toRecord:    func(s string) dns.Record { return dns.Record{Address: s} },
			fromRecord:  func(r dns.Record) string { return r.Address },
		},
	})
}
