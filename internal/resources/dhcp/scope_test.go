// Tests for windowsddi_dhcp_scope.
//
// How these tests work: terraform-plugin-testing drives a real `terraform` binary through
// a list of TestSteps (each step writes an HCL config, runs plan/apply, then runs checks).
//   - Unit tests (resource.UnitTest) wire the provider to dhcpfake, an in-memory fake DHCP
//     server that answers the same PowerShell scripts the real server would. They need no
//     Windows host and always run.
//   - Acceptance tests (TestAcc*, resource.Test) run the same shared steps against a real
//     DHCP server. They skip unless TF_ACC=1 and WINDOWSDDI_* are set (acctest.AccPreCheckDHCP).
//
// TestStep fields used in this package:
//   - Config: the HCL to apply in this step.
//   - Check: assertions on the resulting state, run after apply.
//   - ConfigPlanChecks.PreApply: assertions on the plan before it is applied, for example
//     "this is an in-place update" or "this forces replacement".
//   - ImportState / ImportStateId / ImportStateVerify: run `terraform import` with the given
//     ID and verify the imported state equals the state from the previous step
//     (ImportStateVerifyIgnore lists attributes that cannot be read from the server).
//   - PreConfig: a Go function run before the step, used here to change the fake server
//     behind Terraform's back (simulating drift or adding leases).
//   - ExpectError: the step must fail with an error matching this regular expression.
//   - Destroy: run `terraform destroy` in this step instead of apply.
//   - TestCase.CheckDestroy: after the final destroy, verify the objects are really gone.
//
// Scope IDs use 198.18.0.0/15, a range reserved for benchmarking (RFC 2544), so acceptance
// tests do not collide with real networks.
//
// Run with: go test ./internal/resources/
//
// Go note: `package resources_test` is an external test package: these tests can only use
// the exported API, like real callers. Test functions are named TestXxx and receive
// t *testing.T from Go's test runner.
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
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/psscript"
)

// scopeAddr is the Terraform address of the scope under test (resource type plus name).
const scopeAddr = "windowsddi_dhcp_scope.test"

// scopeConfig renders an HCL scope in 198.18.<third>.0/24 with the given name; extra is
// pasted verbatim inside the block to add optional attributes.
//
// Go note: fmt.Sprintf fills the placeholders in order: %q inserts a quoted string, %d an
// integer, %s a plain string. Backquotes delimit a multi-line raw string.
func scopeConfig(third int, name, extra string) string {
	return fmt.Sprintf(`
resource "windowsddi_dhcp_scope" "test" {
  name        = %q
  start_range = "198.18.%d.10"
  end_range   = "198.18.%d.200"
  subnet_mask = "255.255.255.0"
  %s
}
`, name, third, third, extra)
}

// scopeSteps are shared by the unit and acceptance tests.
//
// Step 1 creates a scope and checks defaults and the computed id. Step 2 changes name,
// description, state and lease duration and expects an in-place update. Step 3 imports the
// scope and verifies the imported state matches.
//
// Go note: `[]resource.TestStep{ {...}, {...} }` is a slice literal; the inner `{...}` are
// TestStep structs whose type Go infers from the slice type.
func scopeSteps(third int) []resource.TestStep {
	id := fmt.Sprintf("198.18.%d.0", third)
	return []resource.TestStep{
		{
			Config: scopeConfig(third, "tf-acc scope", `lease_duration = "8h"`),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(scopeAddr, "id", id),
				resource.TestCheckResourceAttr(scopeAddr, "scope_id", id),
				resource.TestCheckResourceAttr(scopeAddr, "state", "Active"),
				resource.TestCheckResourceAttr(scopeAddr, "type", "Dhcp"),
				resource.TestCheckResourceAttr(scopeAddr, "description", ""),
				// The server answers 0.08:00:00; state keeps the configured spelling.
				resource.TestCheckResourceAttr(scopeAddr, "lease_duration", "8h"),
				resource.TestCheckResourceAttr(scopeAddr, "force_destroy", "false"),
			),
		},
		{
			Config: scopeConfig(third, "tf-acc scope renamed", `
  description    = "managed by terraform"
  state          = "InActive"
  lease_duration = "1.00:00:00"
`),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(scopeAddr, plancheck.ResourceActionUpdate),
			}},
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(scopeAddr, "name", "tf-acc scope renamed"),
				resource.TestCheckResourceAttr(scopeAddr, "description", "managed by terraform"),
				resource.TestCheckResourceAttr(scopeAddr, "state", "InActive"),
				resource.TestCheckResourceAttr(scopeAddr, "lease_duration", "1.00:00:00"),
			),
		},
		// force_destroy is ignored: it is Terraform-only and import sets it to false.
		{
			ResourceName:            scopeAddr,
			ImportState:             true,
			ImportStateId:           id,
			ImportStateVerify:       true,
			ImportStateVerifyIgnore: []string{"force_destroy"},
		},
	}
}

// checkScopeGone returns a CheckDestroy function: for every windowsddi_dhcp_scope left in the
// final state it calls get and fails unless get reports "not found". get abstracts the
// lookup so the same check works against the fake (fakeGet) and a real server.
//
// Go note: this returns a closure, a function that remembers the get it was built with.
func checkScopeGone(get func(id string) error) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		// Go note: `range` over a map yields key and value; `_` discards the key.
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "windowsddi_dhcp_scope" {
				continue
			}
			err := get(rs.Primary.ID)
			if err == nil {
				return fmt.Errorf("scope %s still exists", rs.Primary.ID)
			}
			if !dhcp.IsNotFound(err) {
				return err
			}
		}
		return nil
	}
}

// fakeGet looks a scope up directly in the fake server's memory and returns
// dhcp.ErrNotFound when it is absent. srv.Lock runs the function while holding the fake's
// mutex, because the provider may be using the fake at the same time.
func fakeGet(srv *dhcpfake.Server) func(string) error {
	return func(id string) error {
		var err error
		srv.Lock(func(s *dhcpfake.Server) {
			// Go note: `_, ok := m[k]` tests whether key k exists in map m.
			if _, ok := s.Scopes[id]; ok {
				return
			}
			err = dhcp.ErrNotFound
		})
		return err
	}
}

// TestScopeResource runs the shared steps against the fake, then adds unit-only steps:
// moving to another network (replacement, scope_id known at plan time), drift after an
// out-of-band delete, and a subnet mask change (replacement).
func TestScopeResource(t *testing.T) {
	srv := dhcpfake.New()
	steps := scopeSteps(10)
	// Go note: append adds elements to a slice and returns the extended slice.
	steps = append(steps,
		// Moving start_range into another network replaces the scope.
		resource.TestStep{
			Config: scopeConfig(11, "tf-acc scope", ""),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(scopeAddr, plancheck.ResourceActionDestroyBeforeCreate),
				plancheck.ExpectKnownValue(scopeAddr, tfjsonPath("scope_id"), knownString("198.18.11.0")),
			}},
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(scopeAddr, "scope_id", "198.18.11.0"),
				resource.TestCheckResourceAttr(scopeAddr, "lease_duration", "8.00:00:00"),
			),
		},
		// Drift: the scope is deleted out of band, so Terraform plans to create it again.
		resource.TestStep{
			PreConfig: func() { srv.Lock(func(s *dhcpfake.Server) { delete(s.Scopes, "198.18.11.0") }) },
			Config:    scopeConfig(11, "tf-acc scope", ""),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(scopeAddr, plancheck.ResourceActionCreate),
			}},
		},
		// Changing the mask replaces the scope.
		resource.TestStep{
			Config: `
resource "windowsddi_dhcp_scope" "test" {
  name        = "tf-acc scope"
  start_range = "198.18.11.10"
  end_range   = "198.18.11.100"
  subnet_mask = "255.255.255.128"
}
`,
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(scopeAddr, plancheck.ResourceActionDestroyBeforeCreate),
			}},
		},
	)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.UnitProviders(srv),
		CheckDestroy:             checkScopeGone(fakeGet(srv)),
		Steps:                    steps,
	})
}

// TestScopeResourceForceDestroy checks the force_destroy safety switch: with an active
// lease, destroy fails with a hint mentioning force_destroy; after applying
// force_destroy = true, the final destroy succeeds (verified by CheckDestroy).
func TestScopeResourceForceDestroy(t *testing.T) {
	srv := dhcpfake.New()
	// addLease puts one active lease into the fake scope (used as a PreConfig hook).
	addLease := func() {
		srv.Lock(func(s *dhcpfake.Server) {
			s.Leases["198.18.12.0"] = []dhcp.Lease{{IPAddress: "198.18.12.11", ScopeID: "198.18.12.0", AddressState: "Active"}}
		})
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.UnitProviders(srv),
		CheckDestroy:             checkScopeGone(fakeGet(srv)),
		Steps: []resource.TestStep{
			{Config: scopeConfig(12, "leased", "")},
			{
				PreConfig:   addLease,
				Config:      scopeConfig(12, "leased", ""),
				Destroy:     true,
				ExpectError: regexp.MustCompile(`(?s)Remove-DhcpServerv4Scope.*force_destroy = true`),
			},
			{Config: scopeConfig(12, "leased", "force_destroy = true")},
		},
	})
}

// TestScopeResourceErrors checks that errors reach the user properly: a PowerShell error
// carries the cmdlet name and message, and invalid durations, enum values, IPs, masks and
// import IDs are rejected before anything is sent to the server.
func TestScopeResourceErrors(t *testing.T) {
	srv := dhcpfake.New()
	// srv.Fail lets a test inject a PowerShell error for a given fake operation; here
	// every scope creation is answered with "Access is denied.".
	srv.Fail = func(op string, _ map[string]any) *psscript.Error {
		if op == "scope.add" {
			return &psscript.Error{Message: "Access is denied.", Category: "PermissionDenied", Command: "Add-DhcpServerv4Scope"}
		}
		return nil
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.UnitProviders(srv),
		Steps: []resource.TestStep{
			{
				Config:      scopeConfig(13, "denied", ""),
				ExpectError: regexp.MustCompile(`Add-DhcpServerv4Scope: Access is denied\. \(PermissionDenied\)`),
			},
			{
				Config:      scopeConfig(13, "bad", `lease_duration = "forever"`),
				ExpectError: regexp.MustCompile(`Invalid Duration`),
			},
			{
				Config:      scopeConfig(13, "bad", `state = "Paused"`),
				ExpectError: regexp.MustCompile(`value must be one of`),
			},
			{
				Config: `
resource "windowsddi_dhcp_scope" "test" {
  name        = "bad"
  start_range = "198.18.13.300"
  end_range   = "198.18.13.200"
  subnet_mask = "255.0.255.0"
}
`,
				ExpectError: regexp.MustCompile(`(?s)invalid IPv4 address.*not contiguous`),
			},
			{
				ResourceName:  scopeAddr,
				Config:        scopeConfig(13, "x", ""),
				ImportState:   true,
				ImportStateId: "not-an-ip",
				ExpectError:   regexp.MustCompile(`Invalid Import ID`),
			},
		},
	})
}

// TestAccScopeResource runs the shared steps against a real DHCP server (acceptance test;
// skipped unless TF_ACC=1 and WINDOWSDDI_* are set). CheckDestroy asks the real server.
func TestAccScopeResource(t *testing.T) {
	acctest.AccPreCheckDHCP(t)
	c := acctest.AccDHCPClient(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.AccDHCPProviders(),
		CheckDestroy: checkScopeGone(func(id string) error {
			_, err := c.GetScope(acctest.Ctx(), id)
			return err
		}),
		Steps: scopeSteps(10),
	})
}
