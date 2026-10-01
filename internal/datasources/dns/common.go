// Package dnsdatasources implements the windowsddi DNS data sources: the read-only
// `data "windowsddi_dns_..." {}` blocks. Like the DHCP data sources they only implement
// Metadata, Schema, Configure and Read, and only call the dns client.
package dnsdatasources

import (
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/providerdata"
)

// All returns every DNS data source of the provider.
func All() []func() datasource.DataSource {
	return append(zoneDataSources(), recordDataSources()...)
}

// clientFrom extracts the DNS client from provider data. It returns nil (without error)
// before the provider is configured.
func clientFrom(data any, diags *diag.Diagnostics) *dns.Client {
	return providerdata.DNSFrom(data, diags)
}
