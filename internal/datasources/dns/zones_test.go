// Tests for the windowsddi_dns_zone and windowsddi_dns_zones data sources. The unit test runs
// against dnsfake; TestAccZoneDataSources runs the same configuration against a real DNS
// server (file-backed tfacc-<random>.test zones, a reverse zone in 198.18.0.0/15 and a
// conditional forwarder).

package dnsdatasources_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	tfacctest "github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/acctest"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns/dnsfake"
)

// zoneDSNames holds the random names used by one test run.
type zoneDSNames struct {
	forward, forwarder string
	third              int
}

func newZoneDSNames() zoneDSNames {
	r := tfacctest.RandStringFromCharSet(8, tfacctest.CharSetAlphaNum)
	return zoneDSNames{forward: "tfacc-" + r + ".test", forwarder: "tfacc-fwd-" + r + ".test", third: tfacctest.RandIntRange(100, 250)}
}

// zoneDSBase creates a forward zone, a reverse zone and a conditional forwarder.
func zoneDSBase(n zoneDSNames) string {
	return fmt.Sprintf(`
resource "windowsddi_dns_zone" "forward" {
  name      = %[1]q
  zone_file = "%[1]s.dns"
}

resource "windowsddi_dns_zone" "reverse" {
  network_id = "198.18.%[3]d.0/24"
  zone_file  = "tfacc-rev-%[3]d.dns"
}

resource "windowsddi_dns_conditional_forwarder" "fwd" {
  name           = %[2]q
  master_servers = ["192.0.2.53"]
}
`, n.forward, n.forwarder, n.third)
}

// zoneDSConfig adds the data sources. The forward zone is looked up in another spelling.
func zoneDSConfig(n zoneDSNames) string {
	return zoneDSBase(n) + fmt.Sprintf(`
data "windowsddi_dns_zone" "forward" {
  name       = %q
  depends_on = [windowsddi_dns_zone.forward]
}

data "windowsddi_dns_zone" "fwd" {
  name = windowsddi_dns_conditional_forwarder.fwd.name
}

data "windowsddi_dns_zones" "reverse" {
  reverse    = true
  depends_on = [windowsddi_dns_zone.reverse]
}

data "windowsddi_dns_zones" "not_ad" {
  ad_integrated = false
  depends_on    = [windowsddi_dns_zone.forward, windowsddi_dns_zone.reverse, windowsddi_dns_conditional_forwarder.fwd]
}
`, strings.ToUpper(n.forward)+".")
}

// zoneDSChecks are the assertions valid on any server.
func zoneDSChecks(n zoneDSNames) []resource.TestCheckFunc {
	rev := fmt.Sprintf("%d.18.198.in-addr.arpa", n.third)
	return []resource.TestCheckFunc{
		resource.TestCheckResourceAttr("data.windowsddi_dns_zone.forward", "name", strings.ToUpper(n.forward)+"."),
		resource.TestCheckResourceAttr("data.windowsddi_dns_zone.forward", "zone_type", "Primary"),
		resource.TestCheckResourceAttr("data.windowsddi_dns_zone.forward", "zone_file", n.forward+".dns"),
		resource.TestCheckResourceAttr("data.windowsddi_dns_zone.forward", "dynamic_update", "None"),
		resource.TestCheckResourceAttr("data.windowsddi_dns_zone.forward", "ad_integrated", "false"),
		resource.TestCheckResourceAttr("data.windowsddi_dns_zone.forward", "reverse", "false"),
		resource.TestCheckNoResourceAttr("data.windowsddi_dns_zone.forward", "replication_scope"),
		resource.TestCheckResourceAttr("data.windowsddi_dns_zone.fwd", "zone_type", "Forwarder"),
		resource.TestCheckResourceAttr("data.windowsddi_dns_zone.fwd", "master_servers.0", "192.0.2.53"),
		resource.TestCheckResourceAttr("data.windowsddi_dns_zone.fwd", "forwarder_timeout", "5"),
		resource.TestCheckTypeSetElemNestedAttrs("data.windowsddi_dns_zones.reverse", "zones.*", map[string]string{
			"name": rev, "reverse": "true", "zone_type": "Primary",
		}),
		resource.TestCheckTypeSetElemNestedAttrs("data.windowsddi_dns_zones.not_ad", "zones.*", map[string]string{
			"name": n.forward, "reverse": "false",
		}),
		resource.TestCheckTypeSetElemNestedAttrs("data.windowsddi_dns_zones.not_ad", "zones.*", map[string]string{
			"name": n.forwarder, "zone_type": "Forwarder",
		}),
	}
}

// TestZoneDataSources is the unit test. The fake also holds an auto-created zone (left out
// of every list) and an AD-integrated zone (left out by ad_integrated = false), so exact
// counts can be checked.
func TestZoneDataSources(t *testing.T) {
	srv := dnsfake.New()
	srv.SeedZone(dns.Zone{Name: "TrustAnchors", ZoneType: dns.ZoneTypePrimary, IsAutoCreated: true, IsDsIntegrated: true, ReplicationScope: "Forest"})
	srv.SeedZone(dns.Zone{Name: "0.in-addr.arpa", ZoneType: dns.ZoneTypePrimary, IsAutoCreated: true, IsReverseLookupZone: true})
	srv.SeedZone(dns.Zone{Name: "AD.Example.local", ZoneType: dns.ZoneTypePrimary, IsDsIntegrated: true, ReplicationScope: "Domain", DynamicUpdate: "Secure"})
	n := newZoneDSNames()
	checks := append(zoneDSChecks(n),
		resource.TestCheckResourceAttr("data.windowsddi_dns_zones.reverse", "zones.#", "1"),
		resource.TestCheckResourceAttr("data.windowsddi_dns_zones.not_ad", "zones.#", "3"),
		resource.TestCheckResourceAttr("data.windowsddi_dns_zones.ad", "zones.#", "1"),
		resource.TestCheckResourceAttr("data.windowsddi_dns_zones.ad", "zones.0.name", "ad.example.local"),
		resource.TestCheckResourceAttr("data.windowsddi_dns_zones.ad", "zones.0.replication_scope", "Domain"),
		resource.TestCheckResourceAttr("data.windowsddi_dns_zones.all", "zones.#", "4"),
	)
	extra := `
data "windowsddi_dns_zones" "ad" {
  ad_integrated = true
}

data "windowsddi_dns_zones" "all" {
  depends_on = [windowsddi_dns_zone.forward, windowsddi_dns_zone.reverse, windowsddi_dns_conditional_forwarder.fwd]
}
`
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.UnitProviders(srv),
		Steps: []resource.TestStep{
			{Config: zoneDSBase(n)},
			{Config: zoneDSConfig(n) + extra, Check: resource.ComposeAggregateTestCheckFunc(checks...)},
			{
				Config: zoneDSBase(n) + `
data "windowsddi_dns_zone" "missing" {
  name = "missing.test"
}
`,
				ExpectError: regexp.MustCompile(`(?s)Unable to read DNS zone missing.test.*Get-DnsServerZone`),
			},
			{
				Config: `
data "windowsddi_dns_zone" "bad" {
  name = "bad..name"
}
`,
				ExpectError: regexp.MustCompile(`must be a DNS zone name|invalid DNS name`),
			},
			{Config: zoneDSBase(n)},
		},
	})
}

// TestAccZoneDataSources runs the same configuration against a real DNS server.
func TestAccZoneDataSources(t *testing.T) {
	acctest.AccPreCheckDNS(t)
	n := newZoneDSNames()
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.AccDNSProviders(),
		Steps: []resource.TestStep{
			{Config: zoneDSBase(n)},
			{Config: zoneDSConfig(n), Check: resource.ComposeAggregateTestCheckFunc(zoneDSChecks(n)...)},
		},
	})
}
