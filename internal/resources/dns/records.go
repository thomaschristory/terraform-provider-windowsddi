package dnsresources

import "github.com/hashicorp/terraform-plugin-framework/resource"

// recordResources lists the DNS record-set resources. They all share the
// implementation in record_base.go; each record_*.go file declares one.
func recordResources() []func() resource.Resource {
	return []func() resource.Resource{
		NewARecordSet,
		NewAAAARecordSet,
		NewCNAMERecord,
		NewPTRRecord,
		NewMXRecordSet,
		NewSRVRecordSet,
		NewTXTRecordSet,
	}
}
