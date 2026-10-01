// This file declares windowsddi_dns_cname_record: the CNAME (alias) record of
// one name. A name with a CNAME cannot hold any other record. The
// implementation is shared by all record resources (record_base.go).
//
// Cmdlets used (through the dns package): Get-DnsServerResourceRecord,
// Add-DnsServerResourceRecordCName, Set-DnsServerResourceRecord,
// Remove-DnsServerResourceRecord.

package dnsresources

import (
	"github.com/hashicorp/terraform-plugin-framework/resource"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
)

// NewCNAMERecord returns the windowsddi_dns_cname_record resource.
func NewCNAMERecord() resource.Resource {
	return newRecordResource(recordKind{
		typeSuffix: "_dns_cname_record",
		rrType:     dns.TypeCNAME,
		summary:    "Manages the CNAME (alias) record of one name in a zone. The DNS server refuses a CNAME next to other records of the same name.",
		valueAttr:  "target",
		values: hostnameValue{
			description: "Canonical name the alias points to, for example `www.lab.example.local`. Fully qualified; a trailing dot is optional and case is ignored.",
		},
	})
}
