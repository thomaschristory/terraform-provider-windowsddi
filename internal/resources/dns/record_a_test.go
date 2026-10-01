// Tests for windowsddi_dns_a_record_set. Since every record resource shares
// one implementation (record_base.go), the generic behaviour is tested here
// in depth: create, add and remove values, TTL-only change, import, spelling
// changes that must not diff, drift, existing static and dynamic records,
// and validation. record_types_test.go covers what differs per type.

package dnsresources_test

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/acctest"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns/dnsfake"
)

const (
	aType = "windowsddi_dns_a_record_set"
	aAddr = aType + ".test"
)

// aSteps are shared by the unit and acceptance tests.
func aSteps(zone string) []resource.TestStep {
	return []resource.TestStep{
		// Create two records with a TTL.
		{
			Config: recConfig(aType, zone, "www", `  addresses = ["198.18.0.1", "198.18.0.2"]
  ttl       = 300`),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(aAddr, "id", zone+"/www"),
				resource.TestCheckResourceAttr(aAddr, "addresses.#", "2"),
				resource.TestCheckTypeSetElemAttr(aAddr, "addresses.*", "198.18.0.1"),
				resource.TestCheckResourceAttr(aAddr, "ttl", "300"),
				resource.TestCheckResourceAttr(aAddr, "allow_overwrite_dynamic", "false"),
			),
		},
		// Add .3, remove .1: in place.
		{
			Config: recConfig(aType, zone, "www", `  addresses = ["198.18.0.2", "198.18.0.3"]
  ttl       = 300`),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(aAddr, plancheck.ResourceActionUpdate),
			}},
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(aAddr, "addresses.#", "2"),
				resource.TestCheckTypeSetElemAttr(aAddr, "addresses.*", "198.18.0.3"),
			),
		},
		// TTL only.
		{
			Config: recConfig(aType, zone, "www", `  addresses = ["198.18.0.2", "198.18.0.3"]
  ttl       = 600`),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(aAddr, plancheck.ResourceActionUpdate),
			}},
			Check: resource.TestCheckResourceAttr(aAddr, "ttl", "600"),
		},
		// Import.
		{
			ResourceName:      aAddr,
			ImportState:       true,
			ImportStateId:     zone + "/www",
			ImportStateVerify: true,
		},
		// Other casing and a trailing dot on the zone: no replacement, nothing
		// sent to the server.
		{
			Config: recConfig(aType, "TFACC"+zone[5:]+".", "WWW", `  addresses = ["198.18.0.3", "198.18.0.2"]
  ttl       = 600`),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(aAddr, plancheck.ResourceActionUpdate),
			}},
			Check: resource.TestCheckResourceAttr(aAddr, "id", zone+"/www"),
		},
		{
			Config: recConfig(aType, "TFACC"+zone[5:]+".", "WWW", `  addresses = ["198.18.0.3", "198.18.0.2"]
  ttl       = 600`),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
		},
	}
}

// TestARecordSet runs the shared steps, then unit-only drift steps.
func TestARecordSet(t *testing.T) {
	srv := recFake(recUnitZone)
	cfg := recConfig(aType, recUnitZone, "www", `  addresses = ["198.18.0.2", "198.18.0.3"]
  ttl       = 600`)
	steps := append(aSteps(recUnitZone),
		// Records deleted out of band: the resource is recreated.
		resource.TestStep{
			PreConfig: func() { recRemoveAll(srv, recUnitZone, "www", dns.TypeA) },
			Config:    cfg,
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(aAddr, plancheck.ResourceActionCreate),
			}},
		},
		// One record's TTL changed and an extra value added out of band: both
		// corrected in place.
		resource.TestStep{
			PreConfig: func() {
				srv.Lock(func(s *dnsfake.Server) {
					s.Records[recUnitZone][len(s.Records[recUnitZone])-1].TTL = 60
				})
				srv.SeedRecord(recUnitZone, dns.Record{Name: "www", Type: dns.TypeA, Address: "198.18.0.99", TTL: 600})
			},
			Config: cfg,
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(aAddr, plancheck.ResourceActionUpdate),
			}},
			Check: func(*terraform.State) error { return checkAddrs(srv, "www", 600, "198.18.0.2", "198.18.0.3") },
		},
	)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.UnitProviders(srv),
		CheckDestroy:             recCheckGone(recFakeGetter(srv), aType, dns.TypeA),
		Steps:                    steps,
	})
}

// TestARecordSetDefaultTTL checks that an unset ttl takes the zone default
// and stays stable across updates.
func TestARecordSetDefaultTTL(t *testing.T) {
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.UnitProviders(recFake(recUnitZone)),
		Steps: []resource.TestStep{
			{
				Config: recConfig(aType, recUnitZone, "@", `  addresses = ["198.18.0.1"]`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(aAddr, "ttl", "3600"),
					resource.TestCheckResourceAttr(aAddr, "id", recUnitZone+"/@"),
				),
			},
			{
				Config: recConfig(aType, recUnitZone, "@", `  addresses = ["198.18.0.2"]`),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectKnownValue(aAddr, recPath("ttl"), recInt(3600)),
				}},
			},
		},
	})
}

// TestARecordSetExisting checks the create-time protection: existing static
// records must be imported; dynamic ones need allow_overwrite_dynamic.
func TestARecordSetExisting(t *testing.T) {
	srv := recFake(recUnitZone)
	srv.SeedRecord(recUnitZone, dns.Record{Name: "static", Type: dns.TypeA, Address: "198.18.1.1"})
	srv.SeedRecord(recUnitZone, dns.Record{Name: "pc01", Type: dns.TypeA, Address: "198.18.1.50", Dynamic: true, TTL: 1200})
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.UnitProviders(srv),
		Steps: []resource.TestStep{
			{
				Config:      recConfig(aType, recUnitZone, "static", `  addresses = ["198.18.1.2"]`),
				ExpectError: regexp.MustCompile(`(?s)already exist on the server.*terraform import`),
			},
			{
				Config:      recConfig(aType, recUnitZone, "pc01", `  addresses = ["198.18.1.2"]`),
				ExpectError: regexp.MustCompile(`(?s)dynamic records.*allow_overwrite_dynamic`),
			},
			{
				Config: recConfig(aType, recUnitZone, "pc01", `  addresses = ["198.18.1.2"]
  allow_overwrite_dynamic = true`),
				Check: func(*terraform.State) error { return checkAddrs(srv, "pc01", dnsfake.DefaultTTL, "198.18.1.2") },
			},
		},
	})
	if got := srv.RecordsAt(recUnitZone, "static", dns.TypeA); len(got) != 1 || got[0].Address != "198.18.1.1" {
		t.Fatalf("static record touched: %+v", got)
	}
}

// TestARecordSetValidation checks plan-time validation and import IDs.
func TestARecordSetValidation(t *testing.T) {
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.UnitProviders(recFake(recUnitZone)),
		Steps: []resource.TestStep{
			{
				Config:      recConfig(aType, recUnitZone, "www", `  addresses = ["2001:db8::1"]`),
				ExpectError: regexp.MustCompile(`IPv4`),
			},
			{
				Config:      recConfig(aType, recUnitZone, "www.tfacc-unit.test.", `  addresses = ["198.18.0.1"]`),
				ExpectError: regexp.MustCompile(`Invalid Record Name`),
			},
			{
				Config: recConfig(aType, recUnitZone, "www", `  addresses = ["198.18.0.1"]
  ttl = 0`),
				ExpectError: regexp.MustCompile(`ttl`),
			},
			{
				Config:      recConfig(aType, recUnitZone, "www", `  addresses = []`),
				ExpectError: regexp.MustCompile(`at least 1`),
			},
			{
				ResourceName:  aAddr,
				Config:        recConfig(aType, recUnitZone, "www", `  addresses = ["198.18.0.1"]`),
				ImportState:   true,
				ImportStateId: "www",
				ExpectError:   regexp.MustCompile(`Invalid Import ID`),
			},
			// Importing a name without records fails (Read removes it).
			{
				ResourceName:  aAddr,
				Config:        recConfig(aType, recUnitZone, "www", `  addresses = ["198.18.0.1"]`),
				ImportState:   true,
				ImportStateId: recUnitZone + "/nothing",
				ExpectError:   regexp.MustCompile(`Cannot import non-existent remote object`),
			},
		},
	})
}

// TestAccARecordSet runs the shared steps against a real DNS server.
func TestAccARecordSet(t *testing.T) {
	acctest.AccPreCheckDNS(t)
	zone := recAccZone(t, "")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.AccDNSProviders(),
		CheckDestroy:             recCheckGone(recAccGetter(t), aType, dns.TypeA),
		Steps:                    aSteps(zone),
	})
}
