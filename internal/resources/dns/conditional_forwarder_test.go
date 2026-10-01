// Tests for windowsddi_dns_conditional_forwarder (see zone_test.go for the helpers and
// internal/resources/dhcp/scope_test.go for the test mechanics).

package dnsresources_test

import (
	"fmt"
	"os"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/acctest"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns/dnsfake"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/psscript"
)

const forwarderAddr = "windowsddi_dns_conditional_forwarder.test"

// forwarderConfig renders a forwarder named "test"; extra is pasted inside the block.
func forwarderConfig(name, masters, extra string) string {
	return fmt.Sprintf(`
resource "windowsddi_dns_conditional_forwarder" "test" {
  name           = %q
  master_servers = %s
  %s
}
`, name, masters, extra)
}

// forwarderSteps are shared by the unit and acceptance tests (not AD-integrated): create
// with an IPv6 address in a non-canonical spelling (kept in state) and the default timeout,
// change masters and timeout in place, then import.
func forwarderSteps(name string) []resource.TestStep {
	return []resource.TestStep{
		{
			Config: forwarderConfig(name, `["192.0.2.53", "2001:DB8::53"]`, ""),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(forwarderAddr, "id", name),
				resource.TestCheckResourceAttr(forwarderAddr, "master_servers.#", "2"),
				resource.TestCheckResourceAttr(forwarderAddr, "master_servers.0", "192.0.2.53"),
				resource.TestCheckResourceAttr(forwarderAddr, "master_servers.1", "2001:DB8::53"),
				resource.TestCheckResourceAttr(forwarderAddr, "forwarder_timeout", "5"),
				resource.TestCheckNoResourceAttr(forwarderAddr, "replication_scope"),
			),
		},
		{
			Config:           forwarderConfig(name, `["192.0.2.54", "192.0.2.53"]`, "forwarder_timeout = 3"),
			ConfigPlanChecks: zoneAction(forwarderAddr, plancheck.ResourceActionUpdate),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(forwarderAddr, "master_servers.0", "192.0.2.54"),
				resource.TestCheckResourceAttr(forwarderAddr, "master_servers.1", "192.0.2.53"),
				resource.TestCheckResourceAttr(forwarderAddr, "forwarder_timeout", "3"),
			),
		},
		{
			ResourceName:      forwarderAddr,
			ImportState:       true,
			ImportStateId:     name,
			ImportStateVerify: true,
		},
	}
}

// forwarderADSteps create an AD-integrated forwarder and move it to another scope in place.
func forwarderADSteps(name string) []resource.TestStep {
	return []resource.TestStep{
		{
			Config: forwarderConfig(name, `["192.0.2.53"]`, `replication_scope = "Domain"`),
			Check:  resource.TestCheckResourceAttr(forwarderAddr, "replication_scope", "Domain"),
		},
		{
			Config:           forwarderConfig(name, `["192.0.2.53"]`, `replication_scope = "Forest"`),
			ConfigPlanChecks: zoneAction(forwarderAddr, plancheck.ResourceActionUpdate),
			Check:            resource.TestCheckResourceAttr(forwarderAddr, "replication_scope", "Forest"),
		},
		{
			ResourceName:      forwarderAddr,
			ImportState:       true,
			ImportStateId:     name,
			ImportStateVerify: true,
		},
	}
}

// TestConditionalForwarderResource runs the shared steps against the fake, then: name
// spelling change (in place), drift, AD switches (replace) and scope change (in place),
// rename (replace).
func TestConditionalForwarderResource(t *testing.T) {
	srv := dnsfake.New()
	name := zoneTestName()
	other := zoneTestName()
	steps := append(forwarderSteps(name),
		resource.TestStep{
			Config:           forwarderConfig(name+".", `["192.0.2.54", "192.0.2.53"]`, "forwarder_timeout = 3"),
			ConfigPlanChecks: zoneAction(forwarderAddr, plancheck.ResourceActionUpdate),
		},
		resource.TestStep{
			PreConfig:        func() { srv.Lock(func(s *dnsfake.Server) { delete(s.Zones, name) }) },
			Config:           forwarderConfig(name, `["192.0.2.54"]`, ""),
			ConfigPlanChecks: zoneAction(forwarderAddr, plancheck.ResourceActionCreate),
		},
		resource.TestStep{
			Config:           forwarderConfig(name, `["192.0.2.54"]`, `replication_scope = "Forest"`),
			ConfigPlanChecks: zoneAction(forwarderAddr, plancheck.ResourceActionDestroyBeforeCreate),
		},
		resource.TestStep{
			Config:           forwarderConfig(name, `["192.0.2.54"]`, `replication_scope = "Domain"`),
			ConfigPlanChecks: zoneAction(forwarderAddr, plancheck.ResourceActionUpdate),
			Check:            resource.TestCheckResourceAttr(forwarderAddr, "replication_scope", "Domain"),
		},
		resource.TestStep{
			Config:           forwarderConfig(name, `["192.0.2.54"]`, ""),
			ConfigPlanChecks: zoneAction(forwarderAddr, plancheck.ResourceActionDestroyBeforeCreate),
		},
		resource.TestStep{
			Config:           forwarderConfig(other, `["192.0.2.54"]`, ""),
			ConfigPlanChecks: zoneAction(forwarderAddr, plancheck.ResourceActionDestroyBeforeCreate),
			Check:            resource.TestCheckResourceAttr(forwarderAddr, "id", other),
		},
	)
	steps = append(steps, forwarderADSteps(zoneTestName())...)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.UnitProviders(srv),
		CheckDestroy:             checkZonesGone(zoneFakeGet(srv)),
		Steps:                    steps,
	})
}

// TestConditionalForwarderResourceErrors checks validation and server errors.
func TestConditionalForwarderResourceErrors(t *testing.T) {
	srv := dnsfake.New()
	srv.SeedZone(dns.Zone{Name: "primary.test", ZoneType: dns.ZoneTypePrimary, ZoneFile: "primary.test.dns", DynamicUpdate: "None"})
	srv.Fail = func(op string, _ map[string]any) *psscript.Error {
		if op == "dns.forwarder.add" {
			return &psscript.Error{Message: "Access is denied.", Category: "PermissionDenied", Command: "Add-DnsServerConditionalForwarderZone"}
		}
		return nil
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.UnitProviders(srv),
		Steps: []resource.TestStep{
			{
				Config:      forwarderConfig("denied.test", `["192.0.2.53"]`, ""),
				ExpectError: regexp.MustCompile(`Add-DnsServerConditionalForwarderZone: Access is denied\. \(PermissionDenied\)`),
			},
			{
				Config:      forwarderConfig("a.test", `["192.0.2.300"]`, ""),
				ExpectError: regexp.MustCompile(`invalid IP address "192.0.2.300"`),
			},
			{
				Config:      forwarderConfig("a.test", `[]`, ""),
				ExpectError: regexp.MustCompile(`at least 1`),
			},
			{
				Config:      forwarderConfig("a.test", `["192.0.2.53"]`, `replication_scope = "Everywhere"`),
				ExpectError: regexp.MustCompile(`value must be one of`),
			},
			{
				ResourceName:  forwarderAddr,
				Config:        forwarderConfig("primary.test", `["192.0.2.53"]`, ""),
				ImportState:   true,
				ImportStateId: "primary.test",
				ExpectError:   regexp.MustCompile(`(?s)is not a conditional forwarder.*"Primary"`),
			},
		},
	})
}

// TestAccConditionalForwarderResource runs the shared steps against a real DNS server.
func TestAccConditionalForwarderResource(t *testing.T) {
	acctest.AccPreCheckDNS(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.AccDNSProviders(),
		CheckDestroy:             checkZonesGone(zoneAccGet(t)),
		Steps:                    forwarderSteps(zoneTestName()),
	})
}

// TestAccConditionalForwarderResourceAD runs the AD steps on a domain controller
// (WINDOWSDDI_AD=1).
func TestAccConditionalForwarderResourceAD(t *testing.T) {
	acctest.AccPreCheckDNS(t)
	if os.Getenv("WINDOWSDDI_AD") != "1" {
		t.Skip("set WINDOWSDDI_AD=1 to run AD-integrated forwarder tests (the host must be a domain controller)")
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.AccDNSProviders(),
		CheckDestroy:             checkZonesGone(zoneAccGet(t)),
		Steps:                    forwarderADSteps(zoneTestName()),
	})
}
