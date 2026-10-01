package normalize

// Table-driven tests for the DNS helpers in dns.go. Each case lists an input
// and the expected canonical output, or "" when the input must be rejected.

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// checkCanon runs fn on every input and compares with the expected output;
// want == "" means fn must return an error.
func checkCanon(t *testing.T, name string, fn func(string) (string, error), cases map[string]string) {
	t.Helper()
	for in, want := range cases {
		got, err := fn(in)
		if want == "" {
			if err == nil {
				t.Errorf("%s(%q) = %q, want error", name, in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%s(%q) = %q, %v; want %q", name, in, got, err, want)
		}
	}
}

func TestFQDN(t *testing.T) {
	checkCanon(t, "FQDN", FQDN, map[string]string{
		"mail.example.com":               "mail.example.com.",
		"Mail.Example.COM.":              "mail.example.com.",
		" host ":                         "host.",
		"_sip._tcp.example.":             "_sip._tcp.example.",
		"":                               "",
		".":                              "",
		"a..b":                           "",
		".a":                             "",
		"a b.com":                        "",
		"mail.example.com..":             "",
		strings.Repeat("a", 64) + ".com": "",
	})
}

func TestZoneName(t *testing.T) {
	checkCanon(t, "ZoneName", ZoneName, map[string]string{
		"Example.COM.":        "example.com",
		"example.com":         "example.com",
		"2.1.10.in-addr.arpa": "2.1.10.in-addr.arpa",
		"":                    "",
		".":                   "",
		"a..b":                "",
	})
}

func TestRecordName(t *testing.T) {
	checkCanon(t, "RecordName", RecordName, map[string]string{
		"@":         "@",
		"WWW":       "www",
		"_sip._tcp": "_sip._tcp",
		"*":         "*",
		"a.b":       "a.b",
		"www.":      "",
		"":          "",
		"a..b":      "",
	})
}

func TestCanonicalIPv6(t *testing.T) {
	checkCanon(t, "CanonicalIPv6", CanonicalIPv6, map[string]string{
		"2001:DB8:0:0:0:0:0:1": "2001:db8::1",
		"2001:db8::1":          "2001:db8::1",
		"::ffff:10.0.0.1":      "",
		"10.0.0.1":             "",
		"fe80::1%eth0":         "",
		"nope":                 "",
	})
}

func TestCanonicalIP(t *testing.T) {
	checkCanon(t, "CanonicalIP", CanonicalIP, map[string]string{
		"10.0.0.1":       "10.0.0.1",
		"2001:DB8::0:1":  "2001:db8::1",
		"::ffff:1.2.3.4": "",
		"x":              "",
	})
}

func TestReverseZoneName(t *testing.T) {
	checkCanon(t, "ReverseZoneName", ReverseZoneName, map[string]string{
		"10.0.0.0/8":       "10.in-addr.arpa",
		"172.16.0.0/16":    "16.172.in-addr.arpa",
		"10.1.2.0/24":      "2.1.10.in-addr.arpa",
		"2001:db8::/32":    "8.b.d.0.1.0.0.2.ip6.arpa",
		"fd00:1:2::/48":    "2.0.0.0.1.0.0.0.0.0.d.f.ip6.arpa",
		"2001:db8:a0::/44": "a.0.0.8.b.d.0.1.0.0.2.ip6.arpa",
		"10.1.2.0/23":      "",
		"10.1.2.5/24":      "",
		"10.1.2.0":         "",
		"2001:db8::/30":    "",
		"::/0":             "",
		"2001:db8::1/128":  "",
	})
}

func TestCheckTTL(t *testing.T) {
	for _, ok := range []int64{0, 1, 3600, MaxTTL} {
		if err := CheckTTL(ok); err != nil {
			t.Errorf("CheckTTL(%d) = %v", ok, err)
		}
	}
	for _, bad := range []int64{-1, MaxTTL + 1} {
		if err := CheckTTL(bad); err == nil {
			t.Errorf("CheckTTL(%d) should fail", bad)
		}
	}
}

// TestDNSValidators runs each schema validator on one good and one bad value.
func TestDNSValidators(t *testing.T) {
	cases := []struct {
		v         validator.String
		good, bad string
	}{
		{FQDNValidator(), "mail.example.com", "a..b"},
		{ZoneNameValidator(), "example.com.", ""},
		{RecordNameValidator(), "@", "www.example.com."},
		{IPv6Validator(), "2001:db8::1", "10.0.0.1"},
		{IPValidator(), "10.0.0.1", "10.0.0"},
		{ReverseNetworkValidator(), "10.1.0.0/16", "10.1.0.0/20"},
	}
	for _, c := range cases {
		for val, wantErr := range map[string]bool{c.good: false, c.bad: true} {
			req := validator.StringRequest{Path: path.Root("x"), ConfigValue: types.StringValue(val)}
			resp := &validator.StringResponse{}
			c.v.ValidateString(context.Background(), req, resp)
			if resp.Diagnostics.HasError() != wantErr {
				t.Errorf("%s on %q: error = %v, want %v", c.v.Description(context.Background()), val, resp.Diagnostics.HasError(), wantErr)
			}
		}
	}
}
