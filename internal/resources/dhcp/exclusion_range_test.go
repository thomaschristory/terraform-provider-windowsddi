// Tests for windowsddi_dhcp_exclusion_range.
//
// Same structure as scope_test.go (read its header for how TestStep, unit tests against
// the dhcpfake in-memory server and TestAcc* acceptance tests work). Each config also
// creates the scope the range lives in.
//
// What is covered: create, replacement on any change (there is no in-place update for
// exclusion ranges), import, drift after an out-of-band delete, and a malformed import ID.
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

// exclusionAddr is the Terraform address of the exclusion range under test.
const exclusionAddr = "windowsddi_dhcp_exclusion_range.test"

// exclusionConfig renders a scope in 198.18.<third>.0/24 and an exclusion range from .10
// to .<end> inside it.
//
// Go note: in fmt.Sprintf, %[1]d and %[2]d pick arguments by position, so each can be
// reused several times in the template.
func exclusionConfig(third, end int) string {
	return fmt.Sprintf(`
resource "windowsddi_dhcp_scope" "test" {
  name        = "tf-acc exclusions"
  start_range = "198.18.%[1]d.10"
  end_range   = "198.18.%[1]d.200"
  subnet_mask = "255.255.255.0"
}

resource "windowsddi_dhcp_exclusion_range" "test" {
  scope_id    = windowsddi_dhcp_scope.test.scope_id
  start_range = "198.18.%[1]d.10"
  end_range   = "198.18.%[1]d.%[2]d"
}
`, third, end)
}

// exclusionSteps are shared by the unit and acceptance tests.
//
// Step 1 creates .10-.20 and checks the id format. Step 2 widens it to .10-.30 and
// expects a replacement (destroy then create). Step 3 imports it and verifies the state.
func exclusionSteps(third int) []resource.TestStep {
	// Go note: `:=` declares a new variable and infers its type (string here).
	id := fmt.Sprintf("198.18.%[1]d.0/198.18.%[1]d.10-198.18.%[1]d.20", third)
	return []resource.TestStep{
		{
			Config: exclusionConfig(third, 20),
			Check:  resource.TestCheckResourceAttr(exclusionAddr, "id", id),
		},
		{
			Config: exclusionConfig(third, 30),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(exclusionAddr, plancheck.ResourceActionDestroyBeforeCreate),
			}},
			Check: resource.TestCheckResourceAttr(exclusionAddr, "end_range", fmt.Sprintf("198.18.%d.30", third)),
		},
		{
			ResourceName:      exclusionAddr,
			ImportState:       true,
			ImportStateId:     fmt.Sprintf("198.18.%[1]d.0/198.18.%[1]d.10-198.18.%[1]d.30", third),
			ImportStateVerify: true,
		},
	}
}

// checkExclusionsGone returns a CheckDestroy function that fails if any exclusion range
// left in the final state still exists. get does the lookup (fake or real server) and
// must return dhcp.ErrNotFound for a missing range. See checkScopeGone in scope_test.go.
//
// Go note: the returned value is a closure: a function that remembers get.
func checkExclusionsGone(get func(dhcp.ExclusionRange) error) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		// Go note: `range` over a map yields key and value; `_` discards the key.
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "windowsddi_dhcp_exclusion_range" {
				continue
			}
			a := rs.Primary.Attributes
			err := get(dhcp.ExclusionRange{ScopeID: a["scope_id"], StartRange: a["start_range"], EndRange: a["end_range"]})
			if err == nil {
				return fmt.Errorf("exclusion range %s still exists", rs.Primary.ID)
			}
			if !dhcp.IsNotFound(err) {
				return err
			}
		}
		return nil
	}
}

// TestExclusionRangeResource runs the shared steps against the fake, then adds unit-only
// steps: drift (all exclusions wiped out of band, so Terraform plans a create) and an
// import ID missing its "-<end_range>" part.
func TestExclusionRangeResource(t *testing.T) {
	srv := dhcpfake.New()
	// Go note: append returns the shared steps with the extra steps added at the end.
	steps := append(exclusionSteps(30),
		// Drift: the fake's exclusion list is cleared behind Terraform's back.
		resource.TestStep{
			PreConfig: func() { srv.Lock(func(s *dhcpfake.Server) { s.Exclusions = nil }) },
			Config:    exclusionConfig(30, 30),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(exclusionAddr, plancheck.ResourceActionCreate),
			}},
		},
		// Malformed import ID: no "-<end_range>".
		resource.TestStep{
			ResourceName:  exclusionAddr,
			Config:        exclusionConfig(30, 30),
			ImportState:   true,
			ImportStateId: "198.18.30.0/198.18.30.10",
			ExpectError:   regexp.MustCompile(`Invalid Import ID`),
		},
	)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.UnitProviders(srv),
		// The fake keeps exclusions in a plain list: "found" means an identical entry exists
		// (Go compares structs field by field with ==).
		CheckDestroy: checkExclusionsGone(func(e dhcp.ExclusionRange) error {
			err := dhcp.ErrNotFound
			srv.Lock(func(s *dhcpfake.Server) {
				for _, x := range s.Exclusions {
					if x == e {
						err = nil
					}
				}
			})
			return err
		}),
		Steps: steps,
	})
}

// TestAccExclusionRangeResource runs the shared steps against a real DHCP server (skipped
// unless TF_ACC=1 and WINDOWSDDI_* are set).
func TestAccExclusionRangeResource(t *testing.T) {
	acctest.AccPreCheckDHCP(t)
	c := acctest.AccDHCPClient(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.AccDHCPProviders(),
		CheckDestroy: checkExclusionsGone(func(e dhcp.ExclusionRange) error {
			_, err := c.GetExclusionRange(acctest.Ctx(), e)
			return err
		}),
		Steps: exclusionSteps(30),
	})
}
