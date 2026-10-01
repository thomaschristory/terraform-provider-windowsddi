// Package acctest holds helpers shared by resource and data source tests:
// provider factories for unit tests (backed by an in-memory fake server)
// and for acceptance tests (backed by a real DHCP or DNS server).
//
// How the tests use it: terraform-plugin-testing runs a real `terraform` binary (init,
// plan, apply, destroy) and needs to know how to start this provider in-process. A
// "provider factory" is a function that creates a provider server on demand; tests pass
// the map returned by UnitProviders or AccProviders as ProtoV6ProviderFactories.
//
//   - Unit tests: UnitProviders wires the provider to a fake runner (dhcpfake.Server or
//     dnsfake.Server), an in-memory imitation of the DhcpServer or DnsServer cmdlets. No
//     Windows host or network is involved, so these run on every `go test`.
//   - Acceptance tests (TestAcc*): AccPreCheckDHCP / AccPreCheckDNS skip them unless
//     TF_ACC=1, the WINDOWSDDI_* variables are set and the role answers on the target host;
//     AccDHCPProviders / AccDNSProviders then configure the provider from those variables,
//     so the very same test steps run against a real server.
//
// DHCP and DNS may live on different servers. WINDOWSDDI_DHCP_HOST and WINDOWSDDI_DNS_HOST,
// when set, replace WINDOWSDDI_HOST for that service's tests; the other connection variables
// are shared.
package acctest

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dhcp"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/provider"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/providerdata"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/runner"
)

// ProviderName is the provider's type name.
//
// It is the key in the factory maps, and the prefix Terraform expects on resource types
// (windowsddi_dhcp_scope, ...).
const ProviderName = "windowsddi"

// UnitProviders returns provider factories whose provider runs every script
// against r, normally a dhcpfake.Server or a dnsfake.Server.
//
// provider.NewWithRunner swaps the SSH/WinRM runner for r, so every PowerShell script the
// dhcp or dns package would send to Windows is answered by the fake instead. "test" is the
// provider version string. providerserver.NewProtocol6WithError wraps the framework
// provider in the gRPC server type (protocol version 6) that Terraform talks to.
//
// Go note: the return type is a map (hashtable) from provider name to a factory function
// that returns two values: a provider server and an error.
func UnitProviders(r runner.Runner) map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		ProviderName: providerserver.NewProtocol6WithError(provider.NewWithRunner("test", r)()),
	}
}

// Service names used to pick the per-service host variable.
const (
	serviceDHCP = "DHCP"
	serviceDNS  = "DNS"
)

// serviceEnv returns an environment lookup in which WINDOWSDDI_HOST is replaced by
// WINDOWSDDI_<service>_HOST when that variable is set. Everything else reads the real
// environment.
func serviceEnv(service string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		if k == "WINDOWSDDI_HOST" {
			if v := os.Getenv("WINDOWSDDI_" + service + "_HOST"); v != "" {
				return v, true
			}
		}
		return os.LookupEnv(k)
	}
}

// accProviders returns provider factories for acceptance tests of one service.
//
// This is the normal production provider, so acceptance tests exercise the real SSH or
// WinRM transport. Only the host lookup differs between services.
func accProviders(service string) map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		ProviderName: providerserver.NewProtocol6WithError(provider.NewWithEnv("test", serviceEnv(service))()),
	}
}

// AccDHCPProviders returns provider factories for DHCP acceptance tests.
func AccDHCPProviders() map[string]func() (tfprotov6.ProviderServer, error) {
	return accProviders(serviceDHCP)
}

// AccDNSProviders returns provider factories for DNS acceptance tests.
func AccDNSProviders() map[string]func() (tfprotov6.ProviderServer, error) {
	return accProviders(serviceDNS)
}

// requiredEnv lists the variables acceptance tests need. WINDOWSDDI_HOST may be replaced by
// the service-specific host variable. WINDOWSDDI_TRANSPORT is not listed because it is
// optional (the provider has a default).
var requiredEnv = []string{"WINDOWSDDI_USERNAME", "WINDOWSDDI_PASSWORD"}

// accPreCheck skips the test unless TF_ACC and the connection variables are set for the
// service.
//
// Skipping (not failing) keeps `go test ./...` green on machines without a lab.
//
// Go note: t *testing.T is the handle Go's test runner gives every test; t.Skip marks the
// test as skipped and stops it. t.Helper() makes failure messages point at the caller's
// line instead of this helper.
func accPreCheck(t *testing.T, service string) {
	t.Helper()
	if os.Getenv("TF_ACC") == "" {
		t.Skip("set TF_ACC=1 to run acceptance tests")
	}
	if v, _ := serviceEnv(service)("WINDOWSDDI_HOST"); v == "" {
		t.Skipf("WINDOWSDDI_HOST or WINDOWSDDI_%s_HOST must be set for acceptance tests", service)
	}
	for _, k := range requiredEnv {
		if os.Getenv(k) == "" {
			t.Skipf("%s must be set for acceptance tests", k)
		}
	}
}

// AccPreCheckDHCP skips a DHCP acceptance test unless the environment is configured and the
// DhcpServer module answers on the DHCP host. DNS-only labs therefore skip DHCP tests.
func AccPreCheckDHCP(t *testing.T) {
	t.Helper()
	accPreCheck(t, serviceDHCP)
	if err := AccDHCPClient(t).CheckAvailable(Ctx()); err != nil {
		t.Skipf("DHCP is not available on the acceptance host: %v", err)
	}
}

// AccPreCheckDNS skips a DNS acceptance test unless the environment is configured and the
// DnsServer module answers on the DNS host. DHCP-only labs therefore skip DNS tests.
func AccPreCheckDNS(t *testing.T) {
	t.Helper()
	accPreCheck(t, serviceDNS)
	if err := AccDNSClient(t).CheckAvailable(Ctx()); err != nil {
		t.Skipf("DNS is not available on the acceptance host: %v", err)
	}
}

// accClients caches one set of clients per service, built at most once per test run.
//
// Go note: sync.Mutex guards the map against tests running in parallel.
var (
	accMu      sync.Mutex
	accClients = map[string]*providerdata.Clients{}
)

// accClientsFor builds (once) and returns the clients for a service's acceptance host.
func accClientsFor(t *testing.T, service string) *providerdata.Clients {
	t.Helper()
	accMu.Lock()
	defer accMu.Unlock()
	if c, ok := accClients[service]; ok {
		return c
	}
	c, err := provider.NewClientsFromEnv(serviceEnv(service))
	if err != nil {
		t.Fatalf("building acceptance client: %v", err)
	}
	accClients[service] = c
	return c
}

// AccDHCPClient returns a client talking to the DHCP acceptance server.
//
// Acceptance tests use it outside Terraform: to check that an object really exists on the
// server (Check / CheckDestroy) and to simulate drift by changing the server behind
// Terraform's back (PreConfig).
func AccDHCPClient(t *testing.T) *dhcp.Client {
	t.Helper()
	return accClientsFor(t, serviceDHCP).DHCP
}

// AccDNSClient returns a client talking to the DNS acceptance server.
func AccDNSClient(t *testing.T) *dns.Client {
	t.Helper()
	return accClientsFor(t, serviceDNS).DNS
}

// Ctx is a background context for checks.
//
// Every client call takes a context.Context (Go's standard way to carry cancellation and
// deadlines). Test checks have no request to inherit one from, so they use this empty
// background context.
func Ctx() context.Context { return context.Background() }
