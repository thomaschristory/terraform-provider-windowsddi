// Tests for windowsddi_dhcp_option_value.
//
// Same structure as scope_test.go (read its header for how TestStep, unit tests against
// the dhcpfake in-memory server and TestAcc* acceptance tests work). The configs build a
// scope and a reservation, then set options at scope level (router, DNS) and at
// reservation level (host name).
//
// What is covered: create at scope, reservation and server level, value order being
// significant, vendor/user classes in the id, import for every level, the configured
// spelling of values being kept ("0x01" vs the server's "1"), drift (value changed or
// removed out of band) and validation (scope_id with reserved_ip, empty list, bad import).
//
// Run with: go test ./internal/resources/
//
// Go note: `package resources_test` is an external test package: only the exported API of
// package resources is visible, like for a real caller.

package dhcpresources_test

import (
	"fmt"
	"regexp"
	"strconv"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/acctest"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dhcp"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dhcp/dhcpfake"
)

// optionConfig renders a scope in 198.18.<third>.0/24, a reservation at .5, scope options
// 3 (router) and 6 (DNS servers) and reservation option 12 (host name). routers and dns
// are pasted inside the HCL list brackets, for example `"198.18.0.53", "198.18.1.53"`.
//
// Go note: in fmt.Sprintf, %[1]d, %[2]s and %[3]s pick arguments by position.
func optionConfig(third int, routers, dns string) string {
	return fmt.Sprintf(`
resource "windowsddi_dhcp_scope" "test" {
  name        = "tf-acc options"
  start_range = "198.18.%[1]d.10"
  end_range   = "198.18.%[1]d.200"
  subnet_mask = "255.255.255.0"
}

resource "windowsddi_dhcp_reservation" "test" {
  scope_id   = windowsddi_dhcp_scope.test.scope_id
  ip_address = "198.18.%[1]d.5"
  client_id  = "aa-bb-cc-dd-ee-40"
}

resource "windowsddi_dhcp_option_value" "router" {
  scope_id  = windowsddi_dhcp_scope.test.scope_id
  option_id = 3
  value     = [%[2]s]
}

resource "windowsddi_dhcp_option_value" "dns" {
  scope_id  = windowsddi_dhcp_scope.test.scope_id
  option_id = 6
  value     = [%[3]s]
}

resource "windowsddi_dhcp_option_value" "hostname" {
  reserved_ip = windowsddi_dhcp_reservation.test.ip_address
  option_id   = 12
  value       = ["printer"]
}
`, third, routers, dns)
}

// optionSteps are shared by the unit and acceptance tests.
//
// Step 1 creates the three options and checks ids and list elements ("value.#" is the
// list length, "value.0" the first element in Terraform's flattened state). Step 2 swaps
// the DNS servers and expects an in-place update of that option only. Steps 3 and 4 import
// a scope-level and a reservation-level option.
func optionSteps(third int) []resource.TestStep {
	// Go note: `:=` declares a new variable and infers its type (string here).
	scope := fmt.Sprintf("198.18.%d.0", third)
	res := fmt.Sprintf("198.18.%d.5", third)
	return []resource.TestStep{
		{
			Config: optionConfig(third, fmt.Sprintf(`"198.18.%d.1"`, third), `"198.18.0.53", "198.18.1.53"`),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr("windowsddi_dhcp_option_value.router", "id", "scope/"+scope+"/3"),
				resource.TestCheckResourceAttr("windowsddi_dhcp_option_value.dns", "value.#", "2"),
				resource.TestCheckResourceAttr("windowsddi_dhcp_option_value.dns", "value.0", "198.18.0.53"),
				resource.TestCheckResourceAttr("windowsddi_dhcp_option_value.dns", "value.1", "198.18.1.53"),
				resource.TestCheckResourceAttrSet("windowsddi_dhcp_option_value.dns", "name"),
				resource.TestCheckResourceAttr("windowsddi_dhcp_option_value.hostname", "id", "reservation/"+res+"/12"),
			),
		},
		{
			// Order matters: swapping DNS servers is an in-place update.
			Config: optionConfig(third, fmt.Sprintf(`"198.18.%d.1"`, third), `"198.18.1.53", "198.18.0.53"`),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction("windowsddi_dhcp_option_value.dns", plancheck.ResourceActionUpdate),
				plancheck.ExpectResourceAction("windowsddi_dhcp_option_value.router", plancheck.ResourceActionNoop),
			}},
			Check: resource.TestCheckResourceAttr("windowsddi_dhcp_option_value.dns", "value.0", "198.18.1.53"),
		},
		{
			ResourceName:      "windowsddi_dhcp_option_value.dns",
			ImportState:       true,
			ImportStateId:     "scope/" + scope + "/6",
			ImportStateVerify: true,
		},
		{
			ResourceName:      "windowsddi_dhcp_option_value.hostname",
			ImportState:       true,
			ImportStateId:     "reservation/" + res + "/12",
			ImportStateVerify: true,
		},
	}
}

// checkOptionsGone returns a CheckDestroy function that fails if any option value left in
// the final state still exists. It rebuilds the dhcp.OptionKey from the flat state
// attributes (all strings, hence the ParseInt for option_id). See checkScopeGone in
// scope_test.go.
//
// Go note: the returned value is a closure: a function that remembers get.
func checkOptionsGone(get func(dhcp.OptionKey) error) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		// Go note: `range` over a map yields key and value; `_` discards the key.
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "windowsddi_dhcp_option_value" {
				continue
			}
			a := rs.Primary.Attributes
			// Go note: `id, _ :=` keeps the number and ignores the parse error.
			id, _ := strconv.ParseInt(a["option_id"], 10, 64)
			err := get(dhcp.OptionKey{OptionID: id, ScopeID: a["scope_id"], ReservedIP: a["reserved_ip"], VendorClass: a["vendor_class"], UserClass: a["user_class"]})
			if err == nil {
				return fmt.Errorf("option value %s still exists", rs.Primary.ID)
			}
			if !dhcp.IsNotFound(err) {
				return err
			}
		}
		return nil
	}
}

// fakeOptionGet looks an option value up through a real dhcp.Client whose runner is the
// fake server, so the lookup uses the same Get-DhcpServerv4OptionValue logic as the
// provider. The "" argument means no dhcp_server (the cmdlets target the local server).
func fakeOptionGet(srv *dhcpfake.Server) func(dhcp.OptionKey) error {
	return func(k dhcp.OptionKey) error {
		c := dhcp.New(srv, "")
		_, err := c.GetOptionValue(acctest.Ctx(), k)
		return err
	}
}

// TestOptionValueResource runs the shared steps against the fake, then adds unit-only drift
// steps: the router changed out of band (in-place update back) and removed out of band
// (planned as a create).
func TestOptionValueResource(t *testing.T) {
	srv := dhcpfake.New()
	// Go note: append returns the shared steps with the extra steps added at the end.
	steps := append(optionSteps(40),
		// Drift: the value changed out of band.
		resource.TestStep{
			PreConfig: func() { srv.SetOption(dhcp.OptionKey{OptionID: 3, ScopeID: "198.18.40.0"}, "198.18.40.254") },
			Config:    optionConfig(40, `"198.18.40.1"`, `"198.18.1.53", "198.18.0.53"`),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction("windowsddi_dhcp_option_value.router", plancheck.ResourceActionUpdate),
			}},
		},
		// Drift: removed out of band.
		resource.TestStep{
			PreConfig: func() {
				// Go note: `_ =` deliberately ignores the returned error.
				_ = dhcp.New(srv, "").RemoveOptionValue(acctest.Ctx(), dhcp.OptionKey{OptionID: 3, ScopeID: "198.18.40.0"})
			},
			Config: optionConfig(40, `"198.18.40.1"`, `"198.18.1.53", "198.18.0.53"`),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction("windowsddi_dhcp_option_value.router", plancheck.ResourceActionCreate),
			}},
		},
	)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.UnitProviders(srv),
		CheckDestroy:             checkOptionsGone(fakeOptionGet(srv)),
		Steps:                    steps,
	})
}

// TestOptionValueResourceServerAndClasses covers validation (scope_id with reserved_ip,
// empty value list), server-level options, vendor/user classes in the id, import of both,
// the "0x01" vs "1" spelling case, and an import ID missing its option number.
func TestOptionValueResourceServerAndClasses(t *testing.T) {
	srv := dhcpfake.New()
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.UnitProviders(srv),
		CheckDestroy:             checkOptionsGone(fakeOptionGet(srv)),
		Steps: []resource.TestStep{
			// ConflictsWith: scope_id and reserved_ip together are rejected.
			{
				Config: `
resource "windowsddi_dhcp_option_value" "bad" {
  option_id   = 3
  value       = ["10.0.0.1"]
  scope_id    = "10.0.0.0"
  reserved_ip = "10.0.0.5"
}
`,
				ExpectError: regexp.MustCompile(`cannot be specified when`),
			},
			// SizeAtLeast(1): an empty value list is rejected.
			{
				Config: `
resource "windowsddi_dhcp_option_value" "bad" {
  option_id = 3
  value     = []
}
`,
				ExpectError: regexp.MustCompile(`at least 1`),
			},
			// Server-level options (no scope_id/reserved_ip), one with classes.
			{
				Config: `
resource "windowsddi_dhcp_option_value" "domain" {
  option_id = 15
  value     = ["example.local"]
}

resource "windowsddi_dhcp_option_value" "vendor" {
  option_id    = 43
  value        = ["0x01"]
  vendor_class = "Vendor A"
  user_class   = "Lab"
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("windowsddi_dhcp_option_value.domain", "id", "server/15"),
					resource.TestCheckNoResourceAttr("windowsddi_dhcp_option_value.domain", "scope_id"),
					resource.TestCheckResourceAttr("windowsddi_dhcp_option_value.vendor", "id", "server/43/Vendor A/Lab"),
				),
			},
			{
				ResourceName:      "windowsddi_dhcp_option_value.domain",
				ImportState:       true,
				ImportStateId:     "server/15",
				ImportStateVerify: true,
			},
			// After import there is no configured spelling to keep, so value.0 is the
			// server's "1"; it is checked explicitly instead of compared.
			{
				ResourceName:      "windowsddi_dhcp_option_value.vendor",
				ImportState:       true,
				ImportStateId:     "server/43/Vendor A/Lab",
				ImportStateVerify: true,
				// The server reports 0x01 as 1; apply kept the configured spelling.
				ImportStateVerifyIgnore: []string{"value.0"},
				ImportStateCheck: func(s []*terraform.InstanceState) error {
					if got := s[0].Attributes["value.0"]; got != "1" {
						return fmt.Errorf("imported value.0 = %q, want 1", got)
					}
					return nil
				},
			},
			// "scope/<ip>" without an option number is not a valid import ID.
			{
				ResourceName:  "windowsddi_dhcp_option_value.vendor",
				ImportState:   true,
				ImportStateId: "scope/10.0.0.0",
				ExpectError:   regexp.MustCompile(`Invalid Import ID`),
			},
		},
	})
}

// TestAccOptionValueResource runs the shared steps against a real DHCP server (skipped
// unless TF_ACC=1 and WINDOWSDDI_* are set).
func TestAccOptionValueResource(t *testing.T) {
	acctest.AccPreCheckDHCP(t)
	c := acctest.AccDHCPClient(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.AccDHCPProviders(),
		CheckDestroy: checkOptionsGone(func(k dhcp.OptionKey) error {
			_, err := c.GetOptionValue(acctest.Ctx(), k)
			return err
		}),
		Steps: optionSteps(40),
	})
}
