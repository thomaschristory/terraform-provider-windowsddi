// Tests for the four data sources (windowsddi_dhcp_scope, _scopes, _reservation, _leases).
//
// Run the unit tests with:
//
//	go test ./internal/datasources/
//
// TestDataSources runs the real `terraform` binary (must be on PATH) against the provider,
// wired to dhcpfake, an in-memory fake DHCP server, so no Windows host is needed.
// TestAccDataSources runs the same configuration against a real DHCP server. It is skipped
// unless TF_ACC=1 and the WINDOWSDDI_HOST, _USERNAME, _PASSWORD and _TRANSPORT env vars are
// set:
//
//	TF_ACC=1 go test ./internal/datasources/ -run TestAcc -v
//
// Go note: the package name ends in _test, so these tests only see exported names, exactly
// like an outside user of the package would.
package dhcpdatasources_test

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/acctest"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dhcp"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dhcp/dhcpfake"
)

// baseConfig creates the objects the data sources will look up: a scope in the 198.18.0.0/15
// benchmarking range (unlikely to clash with real networks) and one reservation in it.
// force_destroy lets the test tear the scope down even if it still holds reservations.
//
// Go note: text between backquotes is a raw string literal: multi-line, no escaping.
const baseConfig = `
resource "windowsddi_dhcp_scope" "test" {
  name        = "tf-acc data sources"
  description = "lookup me"
  start_range = "198.18.50.10"
  end_range   = "198.18.50.200"
  subnet_mask = "255.255.255.0"

  force_destroy = true
}

resource "windowsddi_dhcp_reservation" "test" {
  scope_id   = windowsddi_dhcp_scope.test.scope_id
  ip_address = "198.18.50.5"
  client_id  = "aa-bb-cc-dd-ee-50"
  name       = "lookup-host"
}
`

// dataConfig adds one block per data source on top of baseConfig. The reservation is looked up
// twice: by IP, and by MAC written in a different notation (colons, uppercase) than the one
// it was created with, to prove MAC normalisation works.
const dataConfig = baseConfig + `
data "windowsddi_dhcp_scope" "test" {
  scope_id = windowsddi_dhcp_scope.test.scope_id
}

data "windowsddi_dhcp_scopes" "all" {
  depends_on = [windowsddi_dhcp_scope.test]
}

data "windowsddi_dhcp_reservation" "by_ip" {
  scope_id   = windowsddi_dhcp_reservation.test.scope_id
  ip_address = windowsddi_dhcp_reservation.test.ip_address
}

data "windowsddi_dhcp_reservation" "by_mac" {
  scope_id  = windowsddi_dhcp_reservation.test.scope_id
  client_id = "AA:BB:CC:DD:EE:50"
}

data "windowsddi_dhcp_leases" "test" {
  scope_id = windowsddi_dhcp_scope.test.scope_id
}
`

// dataChecks returns the assertions run against Terraform state after applying dataConfig.
// Lease checks are optional because only the fake server can be seeded with a known lease;
// a real test scope has no clients.
//
// Go note: `[]resource.TestCheckFunc{...}` is a slice (list) of check functions; append adds
// more, and `checks...` spreads the slice into the variadic ComposeAggregateTestCheckFunc,
// which runs them all and reports every failure, not just the first.
func dataChecks(withLeases bool) resource.TestCheckFunc {
	checks := []resource.TestCheckFunc{
		resource.TestCheckResourceAttr("data.windowsddi_dhcp_scope.test", "name", "tf-acc data sources"),
		resource.TestCheckResourceAttr("data.windowsddi_dhcp_scope.test", "description", "lookup me"),
		resource.TestCheckResourceAttr("data.windowsddi_dhcp_scope.test", "subnet_mask", "255.255.255.0"),
		resource.TestCheckResourceAttr("data.windowsddi_dhcp_scope.test", "lease_duration", "8.00:00:00"),
		resource.TestCheckTypeSetElemNestedAttrs("data.windowsddi_dhcp_scopes.all", "scopes.*", map[string]string{
			"scope_id": "198.18.50.0",
			"name":     "tf-acc data sources",
		}),
		resource.TestCheckResourceAttr("data.windowsddi_dhcp_reservation.by_ip", "client_id", "aa-bb-cc-dd-ee-50"),
		resource.TestCheckResourceAttr("data.windowsddi_dhcp_reservation.by_ip", "name", "lookup-host"),
		resource.TestCheckResourceAttr("data.windowsddi_dhcp_reservation.by_mac", "ip_address", "198.18.50.5"),
		resource.TestCheckResourceAttr("data.windowsddi_dhcp_reservation.by_mac", "client_id", "AA:BB:CC:DD:EE:50"),
	}
	if withLeases {
		checks = append(checks,
			resource.TestCheckResourceAttr("data.windowsddi_dhcp_leases.test", "leases.#", "1"),
			resource.TestCheckResourceAttr("data.windowsddi_dhcp_leases.test", "leases.0.host_name", "pc01.example.local"),
			resource.TestCheckResourceAttr("data.windowsddi_dhcp_leases.test", "leases.0.lease_expiry_time", "2026-10-09T08:00:00Z"),
		)
	}
	return resource.ComposeAggregateTestCheckFunc(checks...)
}

// TestDataSources is the unit test: each step applies a config against the fake server and
// checks the result or the expected error.
//
// Go note: every function named TestXxx(t *testing.T) is run by `go test`.
func TestDataSources(t *testing.T) {
	// Fresh fake server for this test; UnitProviders wires the provider to it (via
	// provider.NewWithRunner) instead of a real SSH/WinRM connection.
	srv := dhcpfake.New()
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.UnitProviders(srv),
		Steps: []resource.TestStep{
			// Step 1: neither ip_address nor client_id set, so ExactlyOneOf must reject it.
			{
				Config: `
data "windowsddi_dhcp_reservation" "bad" {
  scope_id = "198.18.50.0"
}
`,
				ExpectError: regexp.MustCompile(`(?s)No attribute specified when one \(and only one\) of`),
			},
			// Step 2: create the scope and reservation.
			{Config: baseConfig},
			// Step 3: seed one lease directly into the fake server (PreConfig runs before
			// Terraform), then read all four data sources and check their values.
			{
				PreConfig: func() {
					srv.Lock(func(s *dhcpfake.Server) {
						s.Leases["198.18.50.0"] = []dhcp.Lease{{
							IPAddress: "198.18.50.11", ScopeID: "198.18.50.0", ClientID: "00-11-22-33-44-55",
							HostName: "pc01.example.local", AddressState: "Active", LeaseExpiryTime: "2026-10-09T08:00:00Z", ClientType: "Dhcp",
						}}
					})
				},
				Config: dataConfig,
				Check:  dataChecks(true),
			},
			// Step 4: an IP with no reservation must give a "not found" error.
			{
				Config: baseConfig + `
data "windowsddi_dhcp_reservation" "missing" {
  scope_id   = windowsddi_dhcp_scope.test.scope_id
  ip_address = "198.18.50.99"
}
`,
				ExpectError: regexp.MustCompile(`No reservation with IP address 198.18.50.99`),
			},
			{
				Config: baseConfig + `
data "windowsddi_dhcp_reservation" "missing_mac" {
  scope_id  = windowsddi_dhcp_scope.test.scope_id
  client_id = "00:00:00:00:00:99"
}
`,
				ExpectError: regexp.MustCompile(`No reservation with client ID 00:00:00:00:00:99`),
			},
			// Step 5: an unknown scope must fail with the cmdlet name in the error.
			{
				Config: baseConfig + `
data "windowsddi_dhcp_scope" "missing" {
  scope_id = "198.18.99.0"
}
`,
				ExpectError: regexp.MustCompile(`Get-DhcpServerv4Scope`),
			},
			// Step 6: back to a clean, valid config before the harness destroys everything.
			{Config: baseConfig},
		},
	})
}

// TestAccDataSources is the acceptance test: same configs against a real DHCP server.
// AccPreCheck skips it unless TF_ACC and the WINDOWSDDI_* variables are set.
func TestAccDataSources(t *testing.T) {
	acctest.AccPreCheckDHCP(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.AccDHCPProviders(),
		Steps: []resource.TestStep{
			{Config: baseConfig},
			{Config: dataConfig, Check: dataChecks(false)},
		},
	})
}
