package dnsresources

import "github.com/hashicorp/terraform-plugin-framework/resource"

// zoneResources lists the zone-level DNS resources (zones, conditional forwarders).
func zoneResources() []func() resource.Resource {
	return []func() resource.Resource{
		NewZone,
		NewConditionalForwarder,
	}
}
