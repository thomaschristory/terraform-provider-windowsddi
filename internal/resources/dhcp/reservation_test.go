// Tests for windowsddi_dhcp_reservation.
//
// Same structure as scope_test.go (read its header for how TestStep, unit tests against
// the dhcpfake in-memory server and TestAcc* acceptance tests work). Each config also
// creates a windowsddi_dhcp_scope, because a reservation needs a scope to live in; referencing
// windowsddi_dhcp_scope.test.scope_id makes Terraform create the scope first.
//
// What is covered: create with server-chosen name, MAC spelling normalisation (state keeps
// the configured spelling), in-place update, import, replacement on IP change, drift
// (deleted and modified out of band), and validation of client_id and import IDs.
//
// Run with: go test ./internal/resources/
//
// Go note: `package resources_test` is an external test package: only the exported API of
// package resources is visible, like for a real caller.

package dhcpresources_test

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/acctest"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dhcp"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dhcp/dhcpfake"
)

// reservationAddr is the Terraform address of the reservation under test.
const reservationAddr = "windowsddi_dhcp_reservation.test"

// reservationConfig renders a scope in 198.18.<third>.0/24 plus a reservation in it; body
// is pasted inside the reservation block.
//
// Go note: in fmt.Sprintf, %[1]d and %[2]s pick arguments by position, so `third` can be
// used twice while being passed only once.
func reservationConfig(third int, body string) string {
	return fmt.Sprintf(`
resource "windowsddi_dhcp_scope" "test" {
  name        = "tf-acc reservations"
  start_range = "198.18.%[1]d.10"
  end_range   = "198.18.%[1]d.200"
  subnet_mask = "255.255.255.0"
}

resource "windowsddi_dhcp_reservation" "test" {
  scope_id = windowsddi_dhcp_scope.test.scope_id
  %[2]s
}
`, third, body)
}

// reservationSteps are shared by the unit and acceptance tests.
//
// Step 1 creates a reservation without a name (the server picks one) and checks defaults.
// Step 2 changes client_id (in another spelling), name, description and type in place.
// Step 3 imports it and verifies the imported state.
func reservationSteps(third int) []resource.TestStep {
	// Go note: `:=` declares a new variable and infers its type (string here).
	ip := fmt.Sprintf("198.18.%d.5", third)
	return []resource.TestStep{
		{
			Config: reservationConfig(third, fmt.Sprintf(`
  ip_address = %q
  client_id  = "AA:BB:CC:DD:EE:01"
`, ip)),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(reservationAddr, "id", fmt.Sprintf("198.18.%d.0/%s", third, ip)),
				// The server answers aa-bb-cc-dd-ee-01; state keeps the configured spelling.
				resource.TestCheckResourceAttr(reservationAddr, "client_id", "AA:BB:CC:DD:EE:01"),
				resource.TestCheckResourceAttrSet(reservationAddr, "name"),
				resource.TestCheckResourceAttr(reservationAddr, "type", "Both"),
				resource.TestCheckResourceAttr(reservationAddr, "description", ""),
			),
		},
		{
			Config: reservationConfig(third, fmt.Sprintf(`
  ip_address  = %q
  client_id   = "aabb.ccdd.ee02"
  name        = "printer-01"
  description = "2nd floor"
  type        = "Dhcp"
`, ip)),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(reservationAddr, plancheck.ResourceActionUpdate),
			}},
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(reservationAddr, "client_id", "aabb.ccdd.ee02"),
				resource.TestCheckResourceAttr(reservationAddr, "name", "printer-01"),
				resource.TestCheckResourceAttr(reservationAddr, "description", "2nd floor"),
				resource.TestCheckResourceAttr(reservationAddr, "type", "Dhcp"),
			),
		},
		// Import has no configured spelling to keep, so client_id comes back in the server's
		// form (aa-bb-cc-dd-ee-02). It is excluded from the generic comparison and checked
		// explicitly by ImportStateCheck instead.
		{
			ResourceName:      reservationAddr,
			ImportState:       true,
			ImportStateId:     fmt.Sprintf("198.18.%d.0/%s", third, ip),
			ImportStateVerify: true,
			// Import reads the server's canonical spelling.
			ImportStateVerifyIgnore: []string{"client_id"},
			ImportStateCheck: func(s []*terraform.InstanceState) error {
				if got := s[0].Attributes["client_id"]; got != "aa-bb-cc-dd-ee-02" {
					return fmt.Errorf("imported client_id = %q", got)
				}
				return nil
			},
		},
	}
}

// checkReservationsGone returns a CheckDestroy function that fails if any reservation left
// in the final state still exists. get does the lookup (fake or real server) and must
// return dhcp.ErrNotFound for a missing reservation. See checkScopeGone in scope_test.go.
//
// Go note: the returned value is a closure: a function that remembers get.
func checkReservationsGone(get func(scopeID, ip string) error) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		// Go note: `range` over a map yields key and value; `_` discards the key.
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "windowsddi_dhcp_reservation" {
				continue
			}
			err := get(rs.Primary.Attributes["scope_id"], rs.Primary.Attributes["ip_address"])
			if err == nil {
				return fmt.Errorf("reservation %s still exists", rs.Primary.ID)
			}
			if !dhcp.IsNotFound(err) {
				return err
			}
		}
		return nil
	}
}

// TestReservationResource runs the shared steps against the fake, then adds unit-only
// steps: replacement on IP change, drift by out-of-band delete (recreate) and by an
// out-of-band client ID change (corrected with an in-place update).
func TestReservationResource(t *testing.T) {
	srv := dhcpfake.New()
	// Go note: append returns the shared steps with the extra steps added at the end.
	steps := append(reservationSteps(20),
		// Changing the IP replaces the reservation.
		resource.TestStep{
			Config: reservationConfig(20, `
  ip_address = "198.18.20.6"
  client_id  = "aa-bb-cc-dd-ee-02"
  name       = "printer-01"
`),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(reservationAddr, plancheck.ResourceActionDestroyBeforeCreate),
			}},
		},
		// Drift: removed out of band.
		resource.TestStep{
			PreConfig: func() { srv.Lock(func(s *dhcpfake.Server) { delete(s.Reservations, "198.18.20.6") }) },
			Config: reservationConfig(20, `
  ip_address = "198.18.20.6"
  client_id  = "aa-bb-cc-dd-ee-02"
  name       = "printer-01"
`),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(reservationAddr, plancheck.ResourceActionCreate),
			}},
		},
		// Drift: client ID changed out of band is corrected in place.
		resource.TestStep{
			PreConfig: func() {
				srv.Lock(func(s *dhcpfake.Server) {
					// Go note: map values are copies, so read the struct, change it, and
					// store it back.
					r := s.Reservations["198.18.20.6"]
					r.ClientID = "00-00-00-00-00-01"
					s.Reservations["198.18.20.6"] = r
				})
			},
			Config: reservationConfig(20, `
  ip_address = "198.18.20.6"
  client_id  = "aa-bb-cc-dd-ee-02"
  name       = "printer-01"
`),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(reservationAddr, plancheck.ResourceActionUpdate),
			}},
		},
	)
	// The fake stores reservations keyed by IP (unique per server), so the scope argument
	// is ignored in the lookup.
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.UnitProviders(srv),
		CheckDestroy: checkReservationsGone(func(_, ip string) error {
			var err error
			srv.Lock(func(s *dhcpfake.Server) {
				if _, ok := s.Reservations[ip]; !ok {
					err = dhcp.ErrNotFound
				}
			})
			return err
		}),
		Steps: steps,
	})
}

// TestReservationResourceValidation checks that a malformed MAC is rejected at plan time
// and that an import ID without the "<scope_id>/" prefix is refused.
func TestReservationResourceValidation(t *testing.T) {
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.UnitProviders(dhcpfake.New()),
		Steps: []resource.TestStep{
			{
				Config: reservationConfig(20, `
  ip_address = "198.18.20.7"
  client_id  = "not-a-mac"
`),
				ExpectError: regexp.MustCompile(`Invalid Client ID`),
			},
			{
				ResourceName: reservationAddr,
				Config: reservationConfig(20, `ip_address = "198.18.20.6"
  client_id = "aa-bb-cc-dd-ee-02"
  name = "printer-01"`),
				ImportState:   true,
				ImportStateId: "198.18.20.6",
				ExpectError:   regexp.MustCompile(`Invalid Import ID`),
			},
		},
	})
}

// TestAccReservationResource runs the shared steps against a real DHCP server (skipped
// unless TF_ACC=1 and WINDOWSDDI_* are set).
func TestAccReservationResource(t *testing.T) {
	acctest.AccPreCheckDHCP(t)
	c := acctest.AccDHCPClient(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.AccDHCPProviders(),
		CheckDestroy: checkReservationsGone(func(scopeID, ip string) error {
			_, err := c.GetReservation(acctest.Ctx(), scopeID, ip)
			return err
		}),
		Steps: reservationSteps(20),
	})
}
