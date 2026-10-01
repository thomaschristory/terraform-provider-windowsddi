// Tests for windowsddi_dns_zone. The test mechanics (TestStep fields, unit vs acceptance,
// shared steps) are explained in internal/resources/dhcp/scope_test.go.
//
// Unit tests run against dnsfake. Acceptance tests (TestAcc*) run the shared steps against a
// real DNS server with file-backed tfacc-<random>.test zones; AD-integrated tests also need
// WINDOWSDDI_AD=1 (the host must be a domain controller).

package dnsresources_test

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	tfacctest "github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/acctest"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns/dnsfake"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/psscript"
)

const zoneAddr = "windowsddi_dns_zone.test"

// zoneTestName returns a random lowercase zone name such as tfacc-x7k2p9qa.test.
func zoneTestName() string {
	return "tfacc-" + tfacctest.RandStringFromCharSet(8, tfacctest.CharSetAlphaNum) + ".test"
}

// zoneConfig renders a windowsddi_dns_zone named "test"; body is pasted inside the block.
func zoneConfig(body string) string {
	return fmt.Sprintf("resource \"windowsddi_dns_zone\" \"test\" {\n%s\n}\n", body)
}

// zoneExpectKnown is a plan check that attr is already known (and equal to want) at plan time.
func zoneExpectKnown(addr, attr, want string) plancheck.PlanCheck {
	return plancheck.ExpectKnownValue(addr, tfjsonpath.New(attr), knownvalue.StringExact(want))
}

// zoneAction is a plan check on the planned action for addr.
func zoneAction(addr string, a plancheck.ResourceActionType) resource.ConfigPlanChecks {
	return resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(addr, a)}}
}

// zoneSteps are shared by the unit and acceptance tests for a file-backed forward zone:
// create with an unusual spelling (state keeps it, id is canonical), change dynamic_update
// in place, change only the spelling (in place, no replacement), then import.
func zoneSteps(name string) []resource.TestStep {
	spelled := strings.ToUpper(name) + "."
	file := name + ".dns"
	return []resource.TestStep{
		{
			Config: zoneConfig(fmt.Sprintf("name = %q\nzone_file = %q", spelled, file)),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				zoneExpectKnown(zoneAddr, "id", name),
				zoneExpectKnown(zoneAddr, "dynamic_update", "None"),
			}},
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(zoneAddr, "id", name),
				resource.TestCheckResourceAttr(zoneAddr, "name", spelled),
				resource.TestCheckResourceAttr(zoneAddr, "zone_file", file),
				resource.TestCheckNoResourceAttr(zoneAddr, "replication_scope"),
				resource.TestCheckResourceAttr(zoneAddr, "dynamic_update", "None"),
				resource.TestCheckResourceAttr(zoneAddr, "ad_integrated", "false"),
				resource.TestCheckResourceAttr(zoneAddr, "reverse", "false"),
				resource.TestCheckResourceAttr(zoneAddr, "force_destroy", "false"),
			),
		},
		{
			Config:           zoneConfig(fmt.Sprintf("name = %q\nzone_file = %q\ndynamic_update = \"NonsecureAndSecure\"", spelled, file)),
			ConfigPlanChecks: zoneAction(zoneAddr, plancheck.ResourceActionUpdate),
			Check:            resource.TestCheckResourceAttr(zoneAddr, "dynamic_update", "NonsecureAndSecure"),
		},
		{
			Config:           zoneConfig(fmt.Sprintf("name = %q\nzone_file = %q\ndynamic_update = \"NonsecureAndSecure\"", name, file)),
			ConfigPlanChecks: zoneAction(zoneAddr, plancheck.ResourceActionUpdate),
			Check:            resource.TestCheckResourceAttr(zoneAddr, "name", name),
		},
		{
			ResourceName:            zoneAddr,
			ImportState:             true,
			ImportStateId:           name,
			ImportStateVerify:       true,
			ImportStateVerifyIgnore: []string{"force_destroy"},
		},
	}
}

// zoneReverseSteps create a file-backed reverse zone from network_id: the name is known at
// plan time, then the zone is imported by that name (network_id cannot be read back).
func zoneReverseSteps(third int, file string) []resource.TestStep {
	rev := fmt.Sprintf("%d.18.198.in-addr.arpa", third)
	return []resource.TestStep{
		{
			Config: zoneConfig(fmt.Sprintf("network_id = \"198.18.%d.0/24\"\nzone_file = %q", third, file)),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				zoneExpectKnown(zoneAddr, "name", rev),
				zoneExpectKnown(zoneAddr, "id", rev),
				plancheck.ExpectKnownValue(zoneAddr, tfjsonpath.New("reverse"), knownvalue.Bool(true)),
			}},
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(zoneAddr, "name", rev),
				resource.TestCheckResourceAttr(zoneAddr, "reverse", "true"),
			),
		},
		{
			ResourceName:            zoneAddr,
			ImportState:             true,
			ImportStateId:           rev,
			ImportStateVerify:       true,
			ImportStateVerifyIgnore: []string{"force_destroy", "network_id"},
		},
	}
}

// checkZonesGone returns a CheckDestroy that fails if any zone or conditional forwarder left
// in the final state still exists. get returns dns.ErrNotFound for a missing zone.
func checkZonesGone(get func(name string) error) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		for _, rs := range s.RootModule().Resources {
			if rs.Type != "windowsddi_dns_zone" && rs.Type != "windowsddi_dns_conditional_forwarder" {
				continue
			}
			err := get(rs.Primary.ID)
			if err == nil {
				return fmt.Errorf("zone %s still exists", rs.Primary.ID)
			}
			if !dns.IsNotFound(err) {
				return err
			}
		}
		return nil
	}
}

// zoneFakeGet looks a zone up in the fake server's memory.
func zoneFakeGet(srv *dnsfake.Server) func(string) error {
	return func(name string) error {
		err := dns.ErrNotFound
		srv.Lock(func(s *dnsfake.Server) {
			if _, ok := s.Zones[strings.ToLower(name)]; ok {
				err = nil
			}
		})
		return err
	}
}

// zoneAccGet looks a zone up on the acceptance server.
func zoneAccGet(t *testing.T) func(string) error {
	c := acctest.AccDNSClient(t)
	return func(name string) error {
		_, err := c.GetZone(acctest.Ctx(), name)
		return err
	}
}

// TestZoneResource runs the shared steps against the fake, then: rename (replace), zone file
// change (replace), drift, switch to AD-integrated (replace, Secure default), replication
// scope change (in place) and back to file-backed (replace).
func TestZoneResource(t *testing.T) {
	srv := dnsfake.New()
	name := zoneTestName()
	other := zoneTestName()
	steps := append(zoneSteps(name),
		resource.TestStep{
			Config:           zoneConfig(fmt.Sprintf("name = %q\nzone_file = \"x.dns\"", other)),
			ConfigPlanChecks: zoneAction(zoneAddr, plancheck.ResourceActionDestroyBeforeCreate),
			Check:            resource.TestCheckResourceAttr(zoneAddr, "id", other),
		},
		resource.TestStep{
			Config:           zoneConfig(fmt.Sprintf("name = %q\nzone_file = \"y.dns\"", other)),
			ConfigPlanChecks: zoneAction(zoneAddr, plancheck.ResourceActionDestroyBeforeCreate),
			Check:            resource.TestCheckResourceAttr(zoneAddr, "zone_file", "y.dns"),
		},
		resource.TestStep{
			PreConfig:        func() { srv.Lock(func(s *dnsfake.Server) { delete(s.Zones, other) }) },
			Config:           zoneConfig(fmt.Sprintf("name = %q\nzone_file = \"y.dns\"", other)),
			ConfigPlanChecks: zoneAction(zoneAddr, plancheck.ResourceActionCreate),
		},
		resource.TestStep{
			Config: zoneConfig(fmt.Sprintf("name = %q\nreplication_scope = \"Domain\"", other)),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(zoneAddr, plancheck.ResourceActionDestroyBeforeCreate),
				zoneExpectKnown(zoneAddr, "dynamic_update", "Secure"),
			}},
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(zoneAddr, "ad_integrated", "true"),
				resource.TestCheckResourceAttr(zoneAddr, "dynamic_update", "Secure"),
				resource.TestCheckNoResourceAttr(zoneAddr, "zone_file"),
			),
		},
		resource.TestStep{
			Config:           zoneConfig(fmt.Sprintf("name = %q\nreplication_scope = \"Forest\"", other)),
			ConfigPlanChecks: zoneAction(zoneAddr, plancheck.ResourceActionUpdate),
			Check:            resource.TestCheckResourceAttr(zoneAddr, "replication_scope", "Forest"),
		},
		resource.TestStep{
			Config:           zoneConfig(fmt.Sprintf("name = %q\nzone_file = \"z.dns\"", other)),
			ConfigPlanChecks: zoneAction(zoneAddr, plancheck.ResourceActionDestroyBeforeCreate),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(zoneAddr, "ad_integrated", "false"),
				resource.TestCheckResourceAttr(zoneAddr, "dynamic_update", "None"),
			),
		},
	)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.UnitProviders(srv),
		CheckDestroy:             checkZonesGone(zoneFakeGet(srv)),
		Steps:                    steps,
	})
}

// TestZoneResourceReverse covers reverse zones: name computed from network_id at plan time,
// import by name, an equivalent name switch without replacement, and a network change
// (replace).
func TestZoneResourceReverse(t *testing.T) {
	srv := dnsfake.New()
	steps := append(zoneReverseSteps(77, "rev77.dns"),
		// Switching from network_id to the same zone spelled as a name is not a replacement.
		resource.TestStep{
			Config:           zoneConfig("name = \"77.18.198.IN-ADDR.ARPA\"\nzone_file = \"rev77.dns\""),
			ConfigPlanChecks: zoneAction(zoneAddr, plancheck.ResourceActionUpdate),
		},
		resource.TestStep{
			Config: zoneConfig("network_id = \"198.18.78.0/24\"\nzone_file = \"rev77.dns\""),
			ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
				plancheck.ExpectResourceAction(zoneAddr, plancheck.ResourceActionDestroyBeforeCreate),
				zoneExpectKnown(zoneAddr, "name", "78.18.198.in-addr.arpa"),
			}},
		},
		resource.TestStep{
			Config: zoneConfig("network_id = \"2001:db8::/32\"\nzone_file = \"rev6.dns\""),
			Check:  resource.TestCheckResourceAttr(zoneAddr, "name", "8.b.d.0.1.0.0.2.ip6.arpa"),
		},
	)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.UnitProviders(srv),
		CheckDestroy:             checkZonesGone(zoneFakeGet(srv)),
		Steps:                    steps,
	})
}

// TestZoneResourceForceDestroy checks that a zone holding records is not destroyed without
// force_destroy, and is once force_destroy = true has been applied.
func TestZoneResourceForceDestroy(t *testing.T) {
	srv := dnsfake.New()
	name := zoneTestName()
	cfg := zoneConfig(fmt.Sprintf("name = %q\nzone_file = \"f.dns\"", name))
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.UnitProviders(srv),
		CheckDestroy:             checkZonesGone(zoneFakeGet(srv)),
		Steps: []resource.TestStep{
			{Config: cfg},
			{
				PreConfig:   func() { srv.SeedRecord(name, dns.Record{Name: "www", Type: "A", Address: "192.0.2.10"}) },
				Config:      cfg,
				Destroy:     true,
				ExpectError: regexp.MustCompile(`(?s)holds records.*force_destroy = true`),
			},
			{Config: zoneConfig(fmt.Sprintf("name = %q\nzone_file = \"f.dns\"\nforce_destroy = true", name))},
		},
	})
}

// TestZoneResourceErrors checks validation and server errors.
func TestZoneResourceErrors(t *testing.T) {
	srv := dnsfake.New()
	srv.SeedZone(dns.Zone{Name: "fwd.test", ZoneType: dns.ZoneTypeForwarder, MasterServers: []string{"192.0.2.53"}, ForwarderTimeout: 5})
	srv.Fail = func(op string, p map[string]any) *psscript.Error {
		if op == "dns.zone.add_primary" && p["name"] == "denied.test" {
			return &psscript.Error{Message: "Access is denied.", Category: "PermissionDenied", Command: "Add-DnsServerPrimaryZone"}
		}
		return nil
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.UnitProviders(srv),
		Steps: []resource.TestStep{
			{
				Config:      zoneConfig("name = \"Denied.test\"\nzone_file = \"d.dns\""),
				ExpectError: regexp.MustCompile(`Add-DnsServerPrimaryZone: Access is denied\. \(PermissionDenied\)`),
			},
			{
				Config:      zoneConfig("name = \"a.test\"\nnetwork_id = \"10.0.0.0/8\"\nzone_file = \"d.dns\""),
				ExpectError: regexp.MustCompile(`(?s)one \(and only one\)`),
			},
			{
				Config:      zoneConfig("name = \"a.test\""),
				ExpectError: regexp.MustCompile(`(?s)one \(and only one\)`),
			},
			{
				Config:      zoneConfig("name = \"a.test\"\nzone_file = \"a.dns\"\nreplication_scope = \"Domain\""),
				ExpectError: regexp.MustCompile(`(?s)one \(and only one\)`),
			},
			{
				Config:      zoneConfig("name = \"a.test\"\nzone_file = \"a.dns\"\ndynamic_update = \"Secure\""),
				ExpectError: regexp.MustCompile(`requires an AD-integrated zone`),
			},
			{
				Config:      zoneConfig("network_id = \"10.1.0.0/20\"\nzone_file = \"a.dns\""),
				ExpectError: regexp.MustCompile(`/8, /16 or /24`),
			},
			{
				Config:      zoneConfig("name = \"a..test\"\nzone_file = \"a.dns\""),
				ExpectError: regexp.MustCompile(`Invalid Zone Name`),
			},
			{
				ResourceName:  zoneAddr,
				Config:        zoneConfig("name = \"fwd.test\"\nzone_file = \"a.dns\""),
				ImportState:   true,
				ImportStateId: "fwd.test",
				ExpectError:   regexp.MustCompile(`(?s)is not a primary zone.*"Forwarder"`),
			},
			{
				ResourceName:  zoneAddr,
				Config:        zoneConfig("name = \"fwd.test\"\nzone_file = \"a.dns\""),
				ImportState:   true,
				ImportStateId: "bad..name",
				ExpectError:   regexp.MustCompile(`Invalid Import ID`),
			},
		},
	})
}

// TestAccZoneResource runs the shared forward zone steps against a real DNS server.
func TestAccZoneResource(t *testing.T) {
	acctest.AccPreCheckDNS(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.AccDNSProviders(),
		CheckDestroy:             checkZonesGone(zoneAccGet(t)),
		Steps:                    zoneSteps(zoneTestName()),
	})
}

// TestAccZoneResourceReverse creates a file-backed reverse zone in 198.18.0.0/15 on a real
// DNS server.
func TestAccZoneResourceReverse(t *testing.T) {
	acctest.AccPreCheckDNS(t)
	third := tfacctest.RandIntRange(100, 250)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.AccDNSProviders(),
		CheckDestroy:             checkZonesGone(zoneAccGet(t)),
		Steps:                    zoneReverseSteps(third, fmt.Sprintf("tfacc-rev-%d.dns", third)),
	})
}

// zoneADSteps create an AD-integrated zone (Secure by default), move it to another
// replication scope in place, then import it.
func zoneADSteps(name string) []resource.TestStep {
	return []resource.TestStep{
		{
			Config: zoneConfig(fmt.Sprintf("name = %q\nreplication_scope = \"Domain\"", name)),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(zoneAddr, "ad_integrated", "true"),
				resource.TestCheckResourceAttr(zoneAddr, "dynamic_update", "Secure"),
			),
		},
		{
			Config:           zoneConfig(fmt.Sprintf("name = %q\nreplication_scope = \"Forest\"", name)),
			ConfigPlanChecks: zoneAction(zoneAddr, plancheck.ResourceActionUpdate),
			Check:            resource.TestCheckResourceAttr(zoneAddr, "replication_scope", "Forest"),
		},
		{
			ResourceName:            zoneAddr,
			ImportState:             true,
			ImportStateId:           name,
			ImportStateVerify:       true,
			ImportStateVerifyIgnore: []string{"force_destroy"},
		},
	}
}

// TestZoneResourceAD runs the AD steps against the fake (always).
func TestZoneResourceAD(t *testing.T) {
	srv := dnsfake.New()
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.UnitProviders(srv),
		CheckDestroy:             checkZonesGone(zoneFakeGet(srv)),
		Steps:                    zoneADSteps(zoneTestName()),
	})
}

// TestAccZoneResourceAD runs the AD steps on a domain controller (WINDOWSDDI_AD=1).
func TestAccZoneResourceAD(t *testing.T) {
	acctest.AccPreCheckDNS(t)
	if os.Getenv("WINDOWSDDI_AD") != "1" {
		t.Skip("set WINDOWSDDI_AD=1 to run AD-integrated zone tests (the host must be a domain controller)")
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.AccDNSProviders(),
		CheckDestroy:             checkZonesGone(zoneAccGet(t)),
		Steps:                    zoneADSteps(zoneTestName()),
	})
}
