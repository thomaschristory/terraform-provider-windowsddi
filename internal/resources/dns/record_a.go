// This file declares windowsddi_dns_a_record_set: every A record of one name.
// The implementation is shared by all record resources (record_base.go).
//
// Cmdlets used (through the dns package): Get-DnsServerResourceRecord,
// Add-DnsServerResourceRecordA, Set-DnsServerResourceRecord,
// Remove-DnsServerResourceRecord.

package dnsresources

import (
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/normalize"
)

// NewARecordSet returns the windowsddi_dns_a_record_set resource.
func NewARecordSet() resource.Resource {
	return newRecordResource(recordKind{
		typeSuffix: "_dns_a_record_set",
		rrType:     dns.TypeA,
		summary:    "Manages the A (IPv4 address) records of one name in a zone.",
		valueAttr:  "addresses",
		values: stringSetValues{
			description: "IPv4 addresses, one A record each.",
			elem:        []validator.String{normalize.IPv4Validator()},
			toRecord:    func(s string) dns.Record { return dns.Record{Address: s} },
			fromRecord:  func(r dns.Record) string { return r.Address },
		},
	})
}
