package normalize

// DNS name, address and TTL helpers. Like the DHCP helpers in normalize.go,
// each one defines a single canonical spelling so that values the user and
// the DNS server spell differently ("WWW.Example.com" and "www.example.com.")
// compare equal and never cause a perpetual diff.
//
// Conventions used across the provider:
//   - Zone names are lowercase without a trailing dot ("example.com").
//   - Hostname values (CNAME and PTR targets, MX exchanges, SRV targets) are
//     lowercase fully qualified names with a trailing dot ("mail.example.com."),
//     which is how the DNS server reports them.
//   - Record names are relative to their zone and lowercase, "@" for the apex.
//   - IPv6 addresses use the compressed form ("2001:db8::1").

import (
	"fmt"
	"math"
	"net/netip"
	"strings"
)

// MaxTTL is the largest TTL the provider accepts, in seconds: 2^31 - 1, the
// RFC 2181 limit. The DNS server stores TTLs as unsigned 32-bit numbers but
// resolvers treat values above this as zero.
const MaxTTL int64 = math.MaxInt32

// checkLabels validates the dot separated labels of a name that has already
// been lowercased and stripped of its trailing dot: no empty label (".." or a
// leading dot), at most 63 characters per label, at most 253 in total, and no
// whitespace. Other characters (underscore for SRV owners, "*" for wildcards)
// are accepted because the DNS server accepts them.
func checkLabels(orig, s string) error {
	if s == "" {
		return fmt.Errorf("invalid DNS name %q: empty", orig)
	}
	if len(s) > 253 {
		return fmt.Errorf("invalid DNS name %q: longer than 253 characters", orig)
	}
	// Go note: strings.ContainsAny reports whether any of the listed
	// characters appear in s.
	if strings.ContainsAny(s, " \t\r\n") {
		return fmt.Errorf("invalid DNS name %q: contains whitespace", orig)
	}
	for _, l := range strings.Split(s, ".") {
		if l == "" {
			return fmt.Errorf("invalid DNS name %q: empty label", orig)
		}
		if len(l) > 63 {
			return fmt.Errorf("invalid DNS name %q: label %q longer than 63 characters", orig, l)
		}
	}
	return nil
}

// FQDN canonicalises a hostname value (CNAME or PTR target, MX exchange, SRV
// target): lowercase with exactly one trailing dot. Input without the dot is
// accepted, so "Mail.Example.com" and "mail.example.com." both give
// "mail.example.com.". The root name "." is rejected.
func FQDN(s string) (string, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	t = strings.TrimSuffix(t, ".")
	if err := checkLabels(s, t); err != nil {
		return "", err
	}
	return t + ".", nil
}

// ZoneName canonicalises a zone name: lowercase, without a trailing dot.
// "Example.COM." gives "example.com".
func ZoneName(s string) (string, error) {
	t := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
	if err := checkLabels(s, t); err != nil {
		return "", err
	}
	return t, nil
}

// RecordName canonicalises a record name relative to its zone: lowercase,
// "@" for the zone apex. A trailing dot is rejected because it would make the
// name absolute (users sometimes paste "www.example.com." where "www" is
// meant), and an empty name is rejected so the apex is always explicit.
func RecordName(s string) (string, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	if t == "@" {
		return t, nil
	}
	if strings.HasSuffix(t, ".") {
		return "", fmt.Errorf("invalid record name %q: must be relative to the zone (no trailing dot), use \"@\" for the zone apex", s)
	}
	if err := checkLabels(s, t); err != nil {
		return "", err
	}
	return t, nil
}

// CanonicalIPv6 parses an IPv6 address and returns its compressed lowercase
// form, the form the DNS server reports: "2001:DB8:0:0::1" gives
// "2001:db8::1". IPv4 addresses, IPv4-mapped IPv6 addresses and zoned
// addresses ("fe80::1%eth0") are rejected.
func CanonicalIPv6(s string) (string, error) {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil || !a.Is6() || a.Is4In6() || a.Zone() != "" {
		return "", fmt.Errorf("invalid IPv6 address %q", s)
	}
	return a.String(), nil
}

// CanonicalIP parses an IPv4 or IPv6 address and returns its canonical form
// (dotted quad, or compressed IPv6). Used for conditional forwarder master
// servers, which may be either family.
func CanonicalIP(s string) (string, error) {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil || a.Zone() != "" || a.Is4In6() {
		return "", fmt.Errorf("invalid IP address %q", s)
	}
	return a.String(), nil
}

// ReverseZoneName returns the reverse lookup zone name for a network in CIDR
// form, which is the name the DNS server gives a zone created with
// Add-DnsServerPrimaryZone -NetworkId:
//   - IPv4 on an octet boundary (/8, /16, /24): "10.1.2.0/24" gives
//     "2.1.10.in-addr.arpa".
//   - IPv6 on a nibble boundary (a multiple of 4 between /4 and /124):
//     "2001:db8::/32" gives "8.b.d.0.1.0.0.2.ip6.arpa".
//
// The address must be the network address (no host bits set), so a typo such
// as "10.1.2.5/24" is reported rather than silently truncated.
func ReverseZoneName(cidr string) (string, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(cidr))
	if err != nil {
		return "", fmt.Errorf("invalid network %q: use CIDR notation such as 10.1.2.0/24", cidr)
	}
	if p.Addr().Zone() != "" || p.Addr().Is4In6() {
		return "", fmt.Errorf("invalid network %q", cidr)
	}
	if p != p.Masked() {
		return "", fmt.Errorf("invalid network %q: host bits are set, use %s", cidr, p.Masked())
	}
	bits := p.Bits()
	// Go note: a slice of strings collects the labels; strings.Join glues
	// them together at the end.
	var labels []string
	if p.Addr().Is4() {
		if bits != 8 && bits != 16 && bits != 24 {
			return "", fmt.Errorf("invalid network %q: IPv4 reverse zones must use /8, /16 or /24", cidr)
		}
		b := p.Addr().As4()
		// Most significant octet last: walk the network octets backwards.
		for i := bits/8 - 1; i >= 0; i-- {
			labels = append(labels, fmt.Sprint(b[i]))
		}
		return strings.Join(labels, ".") + ".in-addr.arpa", nil
	}
	if bits%4 != 0 || bits < 4 || bits > 124 {
		return "", fmt.Errorf("invalid network %q: IPv6 reverse zones need a prefix length that is a multiple of 4 (nibble boundary)", cidr)
	}
	b := p.Addr().As16()
	// Each byte holds two nibbles (hex digits): high then low. Collect the
	// first bits/4 nibbles, then emit them in reverse order.
	nibbles := make([]string, 0, bits/4)
	for i := 0; i < bits/4; i++ {
		v := b[i/2]
		if i%2 == 0 {
			v >>= 4
		}
		nibbles = append(nibbles, fmt.Sprintf("%x", v&0x0f))
	}
	for i := len(nibbles) - 1; i >= 0; i-- {
		labels = append(labels, nibbles[i])
	}
	return strings.Join(labels, ".") + ".ip6.arpa", nil
}

// CheckTTL validates a TTL in seconds: 0 means "use the zone default" and is
// allowed; anything else must be between 1 and MaxTTL.
func CheckTTL(ttl int64) error {
	if ttl < 0 || ttl > MaxTTL {
		return fmt.Errorf("invalid TTL %d: must be between 0 and %d seconds", ttl, MaxTTL)
	}
	return nil
}
