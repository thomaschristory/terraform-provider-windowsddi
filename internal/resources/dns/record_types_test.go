// Tests for the record resources other than A: what differs per type is the
// value attribute's shape and its spelling rules, so each kind gets a short
// list of shared steps (create, a spelling that must not diff, update,
// import), run against the fake by TestRecordTypes and against a real server
// by TestAccRecordTypes. The generic behaviour is in record_a_test.go.

package dnsresources_test

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/acctest"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
)

// recKind is one record resource under test.
type recKind struct {
	resType string
	rrType  string
	reverse bool // needs a reverse lookup zone
	steps   func(zone string) []resource.TestStep
}

// recUnitReverseZone is the reverse zone seeded for PTR unit tests.
const recUnitReverseZone = "0.19.198.in-addr.arpa"

// recKinds lists the kinds tested here.
var recKinds = []recKind{
	{resType: "windowsddi_dns_aaaa_record_set", rrType: dns.TypeAAAA, steps: aaaaSteps},
	{resType: "windowsddi_dns_cname_record", rrType: dns.TypeCNAME, steps: cnameSteps},
	{resType: "windowsddi_dns_ptr_record", rrType: dns.TypePTR, reverse: true, steps: ptrSteps},
	{resType: "windowsddi_dns_mx_record_set", rrType: dns.TypeMX, steps: mxSteps},
	{resType: "windowsddi_dns_srv_record_set", rrType: dns.TypeSRV, steps: srvSteps},
	{resType: "windowsddi_dns_txt_record_set", rrType: dns.TypeTXT, steps: txtSteps},
}

// updateCheck expects an in-place update of the resource under test.
func updateCheck(addr string) resource.ConfigPlanChecks {
	return resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
		plancheck.ExpectResourceAction(addr, plancheck.ResourceActionUpdate),
	}}
}

// importCheck checks one attribute of the imported state.
func importCheck(attr, want string) resource.ImportStateCheckFunc {
	return func(s []*terraform.InstanceState) error {
		if got := s[0].Attributes[attr]; got != want {
			return fmt.Errorf("imported %s = %q, want %q", attr, got, want)
		}
		return nil
	}
}

// aaaaSteps: non-canonical IPv6 spellings are kept in state and never diff;
// import reports the compressed form.
func aaaaSteps(zone string) []resource.TestStep {
	const typ, addr = "windowsddi_dns_aaaa_record_set", "windowsddi_dns_aaaa_record_set.test"
	cfg := recConfig(typ, zone, "v6", `  addresses = ["2001:DB8:0:0::1", "2001:db8::2"]`)
	return []resource.TestStep{
		{
			Config: cfg,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckTypeSetElemAttr(addr, "addresses.*", "2001:DB8:0:0::1"),
				resource.TestCheckResourceAttr(addr, "addresses.#", "2"),
			),
		},
		{Config: cfg, PlanOnly: true},
		{
			Config:           recConfig(typ, zone, "v6", `  addresses = ["2001:DB8:0:0::1", "2001:db8::3"]`),
			ConfigPlanChecks: updateCheck(addr),
			Check:            resource.TestCheckTypeSetElemAttr(addr, "addresses.*", "2001:db8::3"),
		},
		{
			ResourceName:            addr,
			ImportState:             true,
			ImportStateId:           zone + "/v6",
			ImportStateVerify:       true,
			ImportStateVerifyIgnore: []string{"addresses"},
			ImportStateCheck: func(s []*terraform.InstanceState) error {
				a := s[0].Attributes
				if a["addresses.#"] != "2" || (a["addresses.0"] != "2001:db8::1" && a["addresses.1"] != "2001:db8::1") {
					return fmt.Errorf("imported addresses: %v", a)
				}
				return nil
			},
		},
	}
}

// cnameSteps: the target keeps the user's spelling (case, no trailing dot).
func cnameSteps(zone string) []resource.TestStep {
	const typ, addr = "windowsddi_dns_cname_record", "windowsddi_dns_cname_record.test"
	cfg := recConfig(typ, zone, "alias", `  target = "WWW.Example.test"`)
	return []resource.TestStep{
		{
			Config: cfg,
			Check:  resource.TestCheckResourceAttr(addr, "target", "WWW.Example.test"),
		},
		{Config: cfg, PlanOnly: true},
		{
			Config:           recConfig(typ, zone, "alias", `  target = "other.example.test."`),
			ConfigPlanChecks: updateCheck(addr),
			Check:            resource.TestCheckResourceAttr(addr, "target", "other.example.test."),
		},
		{ResourceName: addr, ImportState: true, ImportStateId: zone + "/alias", ImportStateVerify: true},
	}
}

// ptrSteps: a PTR in a reverse zone.
func ptrSteps(zone string) []resource.TestStep {
	const typ, addr = "windowsddi_dns_ptr_record", "windowsddi_dns_ptr_record.test"
	return []resource.TestStep{
		{
			Config: recConfig(typ, zone, "80", `  target = "www.example.test"`),
			Check:  resource.TestCheckResourceAttr(addr, "target", "www.example.test"),
		},
		{
			Config:           recConfig(typ, zone, "80", `  target = "web.example.test"`),
			ConfigPlanChecks: updateCheck(addr),
		},
		{
			ResourceName:            addr,
			ImportState:             true,
			ImportStateId:           zone + "/80",
			ImportStateVerify:       true,
			ImportStateVerifyIgnore: []string{"target"},
			ImportStateCheck:        importCheck("target", "web.example.test."),
		},
	}
}

// mxSteps: a set of objects at the apex; exchange spelling is kept.
func mxSteps(zone string) []resource.TestStep {
	const typ, addr = "windowsddi_dns_mx_record_set", "windowsddi_dns_mx_record_set.test"
	cfg := recConfig(typ, zone, "@", `  mx = [
    { preference = 10, exchange = "Mail1.example.test" },
    { preference = 20, exchange = "mail2.example.test." },
  ]`)
	return []resource.TestStep{
		{
			Config: cfg,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(addr, "id", zone+"/@"),
				resource.TestCheckResourceAttr(addr, "mx.#", "2"),
				resource.TestCheckTypeSetElemNestedAttrs(addr, "mx.*", map[string]string{"preference": "10", "exchange": "Mail1.example.test"}),
			),
		},
		{Config: cfg, PlanOnly: true},
		// Same exchange, new preference: one record added, one removed.
		{
			Config: recConfig(typ, zone, "@", `  mx = [
    { preference = 5, exchange = "mail1.example.test." },
    { preference = 20, exchange = "mail2.example.test." },
  ]`),
			ConfigPlanChecks: updateCheck(addr),
			Check:            resource.TestCheckTypeSetElemNestedAttrs(addr, "mx.*", map[string]string{"preference": "5", "exchange": "mail1.example.test."}),
		},
		{ResourceName: addr, ImportState: true, ImportStateId: zone + "/@", ImportStateVerify: true},
	}
}

// srvSteps: a set of objects under an underscore name.
func srvSteps(zone string) []resource.TestStep {
	const typ, addr = "windowsddi_dns_srv_record_set", "windowsddi_dns_srv_record_set.test"
	return []resource.TestStep{
		{
			Config: recConfig(typ, zone, "_sip._tcp", `  srv = [
    { priority = 10, weight = 60, port = 5060, target = "SIP1.example.test" },
    { priority = 10, weight = 40, port = 5060, target = "sip2.example.test." },
  ]
  ttl = 120`),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(addr, "srv.#", "2"),
				resource.TestCheckTypeSetElemNestedAttrs(addr, "srv.*", map[string]string{"weight": "60", "target": "SIP1.example.test"}),
			),
		},
		{
			Config: recConfig(typ, zone, "_sip._tcp", `  srv = [
    { priority = 10, weight = 100, port = 5061, target = "sip1.example.test." },
  ]
  ttl = 120`),
			ConfigPlanChecks: updateCheck(addr),
			Check:            resource.TestCheckResourceAttr(addr, "srv.#", "1"),
		},
		{ResourceName: addr, ImportState: true, ImportStateId: zone + "/_sip._tcp", ImportStateVerify: true},
	}
}

// txtSteps: exact strings, including quotes and spaces.
func txtSteps(zone string) []resource.TestStep {
	const typ, addr = "windowsddi_dns_txt_record_set", "windowsddi_dns_txt_record_set.test"
	return []resource.TestStep{
		{
			Config: recConfig(typ, zone, "@", `  txt = ["v=spf1 mx -all", "say \"hi\""]`),
			Check:  resource.TestCheckTypeSetElemAttr(addr, "txt.*", `say "hi"`),
		},
		{
			Config:           recConfig(typ, zone, "@", `  txt = ["v=spf1 -all"]`),
			ConfigPlanChecks: updateCheck(addr),
			Check:            resource.TestCheckResourceAttr(addr, "txt.#", "1"),
		},
		{ResourceName: addr, ImportState: true, ImportStateId: zone + "/@", ImportStateVerify: true},
	}
}

// TestRecordTypes runs every kind's shared steps against the fake.
func TestRecordTypes(t *testing.T) {
	for _, k := range recKinds {
		t.Run(k.resType, func(t *testing.T) {
			zone := recUnitZone
			if k.reverse {
				zone = recUnitReverseZone
			}
			srv := recFake(zone)
			resource.UnitTest(t, resource.TestCase{
				ProtoV6ProviderFactories: acctest.UnitProviders(srv),
				CheckDestroy:             recCheckGone(recFakeGetter(srv), k.resType, k.rrType),
				Steps:                    k.steps(zone),
			})
		})
	}
}

// TestPTRRecordExtra checks that a second PTR added out of band shows as a
// diff and is removed by the update.
func TestPTRRecordExtra(t *testing.T) {
	const typ, addr = "windowsddi_dns_ptr_record", "windowsddi_dns_ptr_record.test"
	srv := recFake(recUnitReverseZone)
	cfg := recConfig(typ, recUnitReverseZone, "80", `  target = "www.example.test"`)
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.UnitProviders(srv),
		Steps: []resource.TestStep{
			{Config: cfg},
			{
				PreConfig: func() {
					srv.SeedRecord(recUnitReverseZone, dns.Record{Name: "80", Type: dns.TypePTR, HostName: "rogue.example.test."})
				},
				Config:           cfg,
				ConfigPlanChecks: updateCheck(addr),
				Check: func(*terraform.State) error {
					if got := srv.RecordsAt(recUnitReverseZone, "80", dns.TypePTR); len(got) != 1 || got[0].HostName != "www.example.test." {
						return fmt.Errorf("PTR records after update: %+v", got)
					}
					return nil
				},
			},
		},
	})
}

// TestCNAMEConflict checks that the server's refusal of a CNAME next to
// other data reaches the user with the cmdlet's message.
func TestCNAMEConflict(t *testing.T) {
	srv := recFake(recUnitZone)
	srv.SeedRecord(recUnitZone, dns.Record{Name: "host", Type: dns.TypeA, Address: "198.18.0.1"})
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: acctest.UnitProviders(srv),
		Steps: []resource.TestStep{{
			Config:      recConfig("windowsddi_dns_cname_record", recUnitZone, "host", `  target = "www.example.test"`),
			ExpectError: regexp.MustCompile(`(?s)Add-DnsServerResourceRecordCName.*CNAME record cannot be added`),
		}},
	})
}

// TestAccRecordTypes runs every kind's shared steps against a real server.
func TestAccRecordTypes(t *testing.T) {
	acctest.AccPreCheckDNS(t)
	for _, k := range recKinds {
		t.Run(k.resType, func(t *testing.T) {
			var zone string
			if k.reverse {
				zone = recAccReverseZone(t)
			} else {
				zone = recAccZone(t, "")
			}
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: acctest.AccDNSProviders(),
				CheckDestroy:             recCheckGone(recAccGetter(t), k.resType, k.rrType),
				Steps:                    k.steps(zone),
			})
		})
	}
}
