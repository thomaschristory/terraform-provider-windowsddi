// Tests for windowsddi_dns_records. The unit test seeds records in dnsfake
// (static, dynamic, every supported type plus the apex SOA and NS) and
// checks the filters and the zone file rendering. The acceptance test
// creates a file-backed tfacc-<random>.test zone and a few records with the
// dns client, then runs the same kind of checks against a real server.
//
// See internal/resources/dhcp/scope_test.go for how resource.UnitTest and
// TestAcc* tests work.

package dnsdatasources_test

import (
	"fmt"
	"regexp"
	"testing"

	sdkacctest "github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/acctest"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns/dnsfake"
)

// recDSConfig renders three windowsddi_dns_records blocks: the whole zone,
// and two name + type filters (in other casings for the first).
func recDSConfig(zone string) string {
	return fmt.Sprintf(`
data "windowsddi_dns_records" "all" {
  zone_name = %[1]q
}

data "windowsddi_dns_records" "a" {
  zone_name = upper(%[1]q)
  name      = "WWW"
  type      = "a"
}

data "windowsddi_dns_records" "apex_mx" {
  zone_name = %[1]q
  name      = "@"
  type      = "MX"
}
`, zone)
}

// recDSSeed is the records both tests create (besides SOA and NS).
var recDSSeed = []dns.Record{
	{Name: "www", Type: dns.TypeA, Address: "198.18.0.2", TTL: 300},
	{Name: "www", Type: dns.TypeA, Address: "198.18.0.1", TTL: 300},
	{Name: "@", Type: dns.TypeMX, Exchange: "mail.example.test.", Preference: 10},
	{Name: "_sip._tcp", Type: dns.TypeSRV, Target: "sip.example.test.", Priority: 1, Weight: 2, Port: 5060},
	{Name: "@", Type: dns.TypeTXT, Text: `v=spf1 "x"`},
	{Name: "alias", Type: dns.TypeCNAME, HostName: "www.example.test."},
}

// recDSChecks are the checks shared by the unit and acceptance tests.
func recDSChecks() resource.TestCheckFunc {
	return resource.ComposeAggregateTestCheckFunc(
		resource.TestCheckResourceAttr("data.windowsddi_dns_records.a", "records.#", "2"),
		resource.TestCheckResourceAttr("data.windowsddi_dns_records.a", "records.0.name", "www"),
		resource.TestCheckResourceAttr("data.windowsddi_dns_records.a", "records.0.type", "A"),
		resource.TestCheckResourceAttr("data.windowsddi_dns_records.a", "records.0.value", "198.18.0.1"),
		resource.TestCheckResourceAttr("data.windowsddi_dns_records.a", "records.0.ttl", "300"),
		resource.TestCheckResourceAttr("data.windowsddi_dns_records.a", "records.0.dynamic", "false"),
		resource.TestCheckResourceAttr("data.windowsddi_dns_records.apex_mx", "records.#", "1"),
		resource.TestCheckResourceAttr("data.windowsddi_dns_records.apex_mx", "records.0.name", "@"),
		resource.TestCheckResourceAttr("data.windowsddi_dns_records.apex_mx", "records.0.value", "10 mail.example.test."),
		resource.TestCheckTypeSetElemNestedAttrs("data.windowsddi_dns_records.all", "records.*",
			map[string]string{"type": "SRV", "name": "_sip._tcp", "value": "1 2 5060 sip.example.test."}),
		resource.TestCheckTypeSetElemNestedAttrs("data.windowsddi_dns_records.all", "records.*",
			map[string]string{"type": "TXT", "value": `"v=spf1 \"x\""`}),
		resource.TestCheckTypeSetElemNestedAttrs("data.windowsddi_dns_records.all", "records.*",
			map[string]string{"type": "CNAME", "name": "alias", "value": "www.example.test."}),
		resource.TestCheckTypeSetElemNestedAttrs("data.windowsddi_dns_records.all", "records.*",
			map[string]string{"type": "SOA", "name": "@"}),
	)
}

func TestRecordsDataSource(t *testing.T) {
	const zone = "tfacc-unit.test"
	srv := dnsfake.New()
	srv.SeedZone(dns.Zone{Name: zone, ZoneType: dns.ZoneTypePrimary, ZoneFile: zone + ".dns"})
	for _, r := range recDSSeed {
		srv.SeedRecord(zone, r)
	}
	// A dynamic record, upper-case name as a client might register it.
	srv.SeedRecord(zone, dns.Record{Name: "PC01", Type: dns.TypeA, Address: "198.18.0.50", TTL: 1200, Dynamic: true})
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.UnitProviders(srv),
		Steps: []resource.TestStep{
			// Error steps first: the post-test destroy uses the last config.
			{
				Config:      `data "windowsddi_dns_records" "x" { zone_name = "missing.test" }`,
				ExpectError: regexp.MustCompile(`zone does not exist`),
			},
			{
				Config:      `data "windowsddi_dns_records" "x" { zone_name = "a..b" }`,
				ExpectError: regexp.MustCompile(`invalid DNS name`),
			},
			{
				Config: recDSConfig(zone),
				Check: resource.ComposeAggregateTestCheckFunc(
					recDSChecks(),
					// 6 seeded + dynamic + SOA + NS.
					resource.TestCheckResourceAttr("data.windowsddi_dns_records.all", "records.#", "9"),
					resource.TestCheckTypeSetElemNestedAttrs("data.windowsddi_dns_records.all", "records.*",
						map[string]string{"name": "pc01", "type": "A", "dynamic": "true", "ttl": "1200"}),
				),
			},
		},
	})
}

func TestAccRecordsDataSource(t *testing.T) {
	acctest.AccPreCheckDNS(t)
	c := acctest.AccDNSClient(t)
	ctx := acctest.Ctx()
	zone := "tfacc-" + sdkacctest.RandString(8) + ".test"
	if _, err := c.AddPrimaryZone(ctx, dns.PrimaryZoneInput{Name: zone, ZoneFile: zone + ".dns"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.RemoveZone(ctx, zone) })
	for _, r := range recDSSeed {
		if err := c.AddRecord(ctx, zone, r); err != nil {
			t.Fatal(err)
		}
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.AccDNSProviders(),
		Steps:                    []resource.TestStep{{Config: recDSConfig(zone), Check: recDSChecks()}},
	})
}
