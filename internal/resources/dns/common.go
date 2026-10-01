// Package dnsresources implements the windowsddi DNS managed resources: the
// `resource "windowsddi_dns_..." "name" { ... }` blocks.
//
// It follows the same conventions as the DHCP resources (internal/resources/dhcp, whose
// scope.go explains the framework concepts in the most detail): every resource implements
// Create, Read, Update, Delete and ImportState, Read removes a vanished object from state,
// and resources never build PowerShell themselves. Zones and conditional forwarders call
// the dns client directly; record-set resources go through the recordset engine.
package dnsresources

import (
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/providerdata"
)

// All returns every DNS resource of the provider.
func All() []func() resource.Resource {
	return append(zoneResources(), recordResources()...)
}

// clientFrom extracts the DNS client from provider data. It returns nil (without error)
// before the provider is configured.
func clientFrom(data any, diags *diag.Diagnostics) *dns.Client {
	return providerdata.DNSFrom(data, diags)
}
