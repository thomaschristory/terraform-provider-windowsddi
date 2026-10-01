// Helpers shared by the record resource tests (record_*_test.go).
//
// How the tests work is explained in internal/resources/dhcp/scope_test.go:
// unit tests (resource.UnitTest) run real Terraform against dnsfake, an
// in-memory DNS server; acceptance tests (TestAcc*) run the same shared
// steps against a real DNS server and skip unless TF_ACC=1 and the
// WINDOWSDDI_* variables are set.
//
// Zones: unit tests seed a zone in the fake. Acceptance tests create a
// file-backed zone tfacc-<random>.test with the dns client before the test
// and remove it afterwards (t.Cleanup), so they do not depend on the zone
// resource.

package dnsresources_test

import (
	"fmt"
	"slices"
	"testing"

	sdkacctest "github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/acctest"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns/dnsfake"
)

// recUnitZone is the zone the unit tests seed in the fake.
const recUnitZone = "tfacc-unit.test"

// recFake returns a fake DNS server holding the given primary zones.
func recFake(zones ...string) *dnsfake.Server {
	srv := dnsfake.New()
	for _, z := range zones {
		srv.SeedZone(dns.Zone{Name: z, ZoneType: dns.ZoneTypePrimary, ZoneFile: z + ".dns"})
	}
	return srv
}

// recAccZone creates a file-backed zone on the acceptance server and
// registers its removal. name is the zone name; pass "" for a random
// tfacc-<random>.test forward zone.
func recAccZone(t *testing.T, name string) string {
	t.Helper()
	if name == "" {
		name = "tfacc-" + sdkacctest.RandString(8) + ".test"
	}
	c := acctest.AccDNSClient(t)
	if _, err := c.AddPrimaryZone(acctest.Ctx(), dns.PrimaryZoneInput{Name: name, ZoneFile: name + ".dns"}); err != nil {
		t.Fatalf("creating test zone %s: %v", name, err)
	}
	t.Cleanup(func() {
		if err := c.RemoveZone(acctest.Ctx(), name); err != nil && !dns.IsNotFound(err) {
			t.Errorf("removing test zone %s: %v", name, err)
		}
	})
	return name
}

// recAccReverseZone creates a random /24 reverse zone in 198.19.0.0/16 (a
// benchmarking range) on the acceptance server.
func recAccReverseZone(t *testing.T) string {
	t.Helper()
	return recAccZone(t, fmt.Sprintf("%d.19.198.in-addr.arpa", sdkacctest.RandIntRange(0, 256)))
}

// recConfig renders one record resource named "test"; body is pasted inside
// the block after zone_name and name.
func recConfig(resType, zone, name, body string) string {
	return fmt.Sprintf(`
resource %q "test" {
  zone_name = %q
  name      = %q
%s
}
`, resType, zone, name, body)
}

// recGetter looks up the records of a tuple on the fake or real server.
type recGetter func(zone, name, rrType string) ([]dns.Record, error)

// recFakeGetter reads the fake's state directly.
func recFakeGetter(srv *dnsfake.Server) recGetter {
	return func(zone, name, rrType string) ([]dns.Record, error) {
		return srv.RecordsAt(zone, name, rrType), nil
	}
}

// recAccGetter reads through the acceptance client.
func recAccGetter(t *testing.T) recGetter {
	c := acctest.AccDNSClient(t)
	return func(zone, name, rrType string) ([]dns.Record, error) {
		return c.GetRecords(acctest.Ctx(), zone, name, rrType)
	}
}

// recCheckGone returns a CheckDestroy that fails when a record resource left
// in the final state still has records on the server.
func recCheckGone(get recGetter, resType, rrType string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		for _, rs := range s.RootModule().Resources {
			if rs.Type != resType {
				continue
			}
			recs, err := get(rs.Primary.Attributes["zone_name"], rs.Primary.Attributes["name"], rrType)
			if dns.IsNotFound(err) {
				continue
			}
			if err != nil {
				return err
			}
			if len(recs) > 0 {
				return fmt.Errorf("%s %s still has %d records", resType, rs.Primary.ID, len(recs))
			}
		}
		return nil
	}
}

// recRemoveAll deletes every record of a tuple from the fake (drift).
func recRemoveAll(srv *dnsfake.Server, zone, name, rrType string) {
	srv.Lock(func(s *dnsfake.Server) {
		kept := s.Records[zone][:0]
		for _, r := range s.Records[zone] {
			if r.Name != name || r.Type != rrType {
				kept = append(kept, r)
			}
		}
		s.Records[zone] = kept
	})
}

// checkAddrs checks that the A records of name in the unit zone are exactly
// addrs (in any order), all with the given TTL.
func checkAddrs(srv *dnsfake.Server, name string, ttl int64, addrs ...string) error {
	got := srv.RecordsAt(recUnitZone, name, dns.TypeA)
	if len(got) != len(addrs) {
		return fmt.Errorf("%s: %d A records on the server, want %d: %+v", name, len(got), len(addrs), got)
	}
	for _, r := range got {
		if !slices.Contains(addrs, r.Address) || r.TTL != ttl {
			return fmt.Errorf("%s: unexpected record %+v (want %v, TTL %d)", name, r, addrs, ttl)
		}
	}
	return nil
}

// recPath points at a top-level attribute in the JSON plan.
func recPath(attr string) tfjsonpath.Path { return tfjsonpath.New(attr) }

// recInt checks a known number value.
func recInt(n int64) knownvalue.Check { return knownvalue.Int64Exact(n) }
