// Package providerdata defines the value the provider hands to every resource
// and data source: one typed client per service, sharing a single transport.
//
// The provider's Configure builds a *Clients and stores it as ResourceData and
// DataSourceData. Each resource family then picks the client it needs with
// DHCPFrom or DNSFrom in its own Configure method.
//
// It lives in its own package (rather than in provider) so the resource
// packages can import it without importing the provider, which imports them.
package providerdata

import (
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dhcp"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
)

// Clients holds the per-service clients built from one provider block.
type Clients struct {
	DHCP *dhcp.Client
	DNS  *dns.Client
}

// from turns the untyped provider data back into *Clients. It returns nil
// (without error) when data is nil, which happens when Terraform configures a
// resource before the provider itself is configured (early validation).
func from(data any, diags *diag.Diagnostics) *Clients {
	if data == nil {
		return nil
	}
	c, ok := data.(*Clients)
	if !ok {
		diags.AddError("Unexpected Provider Data", fmt.Sprintf("expected *providerdata.Clients, got %T. Please report this issue to the provider developers.", data))
		return nil
	}
	return c
}

// DHCPFrom extracts the DHCP client from provider data, or returns nil when
// the provider is not configured yet.
func DHCPFrom(data any, diags *diag.Diagnostics) *dhcp.Client {
	if c := from(data, diags); c != nil {
		return c.DHCP
	}
	return nil
}

// DNSFrom extracts the DNS client from provider data, or returns nil when the
// provider is not configured yet.
func DNSFrom(data any, diags *diag.Diagnostics) *dns.Client {
	if c := from(data, diags); c != nil {
		return c.DNS
	}
	return nil
}
