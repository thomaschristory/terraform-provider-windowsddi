// This file declares windowsddi_dns_ptr_record: the PTR (reverse lookup)
// record of one name, usually in an in-addr.arpa or ip6.arpa zone. The
// implementation is shared by all record resources (record_base.go).
//
// Cmdlets used (through the dns package): Get-DnsServerResourceRecord,
// Add-DnsServerResourceRecordPtr, Set-DnsServerResourceRecord,
// Remove-DnsServerResourceRecord.

package dnsresources

import (
	"github.com/hashicorp/terraform-plugin-framework/resource"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
)

// NewPTRRecord returns the windowsddi_dns_ptr_record resource.
func NewPTRRecord() resource.Resource {
	return newRecordResource(recordKind{
		typeSuffix: "_dns_ptr_record",
		rrType:     dns.TypePTR,
		summary:    "Manages the PTR (reverse lookup) record of one name in a zone, for example name `80` in zone `20.1.10.in-addr.arpa` for 10.1.20.80. Extra PTR records of the name are removed.",
		valueAttr:  "target",
		values: hostnameValue{
			description: "Host name the address resolves to, for example `www.lab.example.local`. Fully qualified; a trailing dot is optional and case is ignored.",
		},
	})
}
