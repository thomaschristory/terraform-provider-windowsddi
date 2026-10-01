// This file declares windowsddi_dns_txt_record_set: every TXT record of one
// name. The implementation is shared by all record resources
// (record_base.go).
//
// Cmdlets used (through the dns package): Get-DnsServerResourceRecord,
// Add-DnsServerResourceRecord -Txt, Set-DnsServerResourceRecord,
// Remove-DnsServerResourceRecord.

package dnsresources

import (
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
)

// NewTXTRecordSet returns the windowsddi_dns_txt_record_set resource.
func NewTXTRecordSet() resource.Resource {
	return newRecordResource(recordKind{
		typeSuffix: "_dns_txt_record_set",
		rrType:     dns.TypeTXT,
		summary:    "Manages the TXT records of one name in a zone (SPF, domain verification tokens, ...).",
		valueAttr:  "txt",
		values: stringSetValues{
			description: "Text values, one TXT record each. Compared exactly (case sensitive). Write the text without surrounding quotes.",
			elem:        []validator.String{stringvalidator.LengthAtLeast(1)},
			toRecord:    func(s string) dns.Record { return dns.Record{Text: s} },
			fromRecord:  func(r dns.Record) string { return r.Text },
		},
	})
}
