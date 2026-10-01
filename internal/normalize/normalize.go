// Package normalize parses and canonicalises user input (durations, MAC
// addresses, IPv4 addresses) so plans never show cosmetic diffs.
//
// The problem it solves: users and the Windows DHCP server often spell the
// same value differently. A user writes lease_duration = "8h", the server
// reports "0.08:00:00". A user writes "AA:BB:CC:DD:EE:FF", the server
// reports "aa-bb-cc-dd-ee-ff". Without help, Terraform would compare the
// strings, see a difference, and plan a change on every run (a "perpetual
// diff"). This package defines one canonical spelling for each kind of value
// and Terraform custom types that compare values by that canonical form.
//
// Where it sits: it is a leaf helper package. Resources and data sources use
// its custom types and validators in their schemas; dhcpfake uses its
// parsing helpers to mimic the server. It never talks to the DHCP server.
//
// Files:
//   - normalize.go: pure parsing/formatting helpers (durations, MACs, IPs,
//     masks, network address).
//   - dns.go: DNS helpers (FQDN, zone and record names, IPv6, reverse zone
//     names, TTL limits); dns_test.go tests them.
//   - types.go: Terraform custom string types (DurationType, MACType) with
//     semantic equality, so "8h" and "0.08:00:00" count as the same value.
//   - validators.go: schema validators for IPv4 addresses and masks, and
//     EnumOf for case-insensitive enum matching.
//   - normalize_test.go: table-driven unit tests for all of the above.
package normalize

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// timespanRe matches the .NET TimeSpan text forms "d.hh:mm:ss" and
// "hh:mm:ss". Capture groups: 1 = days (optional), 2 = hours, 3 = minutes,
// 4 = seconds.
//
// Go note: regexp.MustCompile compiles the pattern once at program start and
// panics (crashes) if the pattern itself is invalid, which is fine for a
// constant pattern. The backtick string is a raw literal, so `\d` needs no
// double escaping.
var timespanRe = regexp.MustCompile(`^(?:(\d+)\.)?(\d{1,2}):(\d{2}):(\d{2})$`)

// ParseDuration accepts the .NET TimeSpan forms "d.hh:mm:ss" and "hh:mm:ss"
// as well as Go durations ("8h", "90m"). The result must be a positive whole
// number of seconds.
//
// Why both forms: d.hh:mm:ss is what Windows admins know and what the server
// returns; Go durations are shorter to type. Whole seconds only, because the
// value is sent to PowerShell as an integer number of seconds.
//
// Go note: names starting with a capital (ParseDuration) are exported, i.e.
// usable from other packages; lowercase names (timespanRe) are private.
// Go note: time.Duration is Go's length-of-time type (nanoseconds inside).
// Expressions like 24*time.Hour build durations from units.
// Go note: the function returns two values (the duration and an error); on
// failure the duration is 0 and the error says why.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	var d time.Duration
	// Go note: `if m := f(); m != nil` runs f, stores the result in m (only
	// visible in this if/else), and tests it. nil here means "no match".
	if m := timespanRe.FindStringSubmatch(s); m != nil {
		// TimeSpan form. m[0] is the whole match, m[1..4] the groups.
		var days int64
		if m[1] != "" {
			var err error
			// Cap days to avoid overflow on absurd input.
			if days, err = strconv.ParseInt(m[1], 10, 64); err != nil || days > 100000 {
				return 0, fmt.Errorf("invalid duration %q: too many days", s)
			}
		}
		// The regex guarantees digits, so conversion errors are impossible
		// and are discarded with `_`.
		h, _ := strconv.Atoi(m[2])
		mi, _ := strconv.Atoi(m[3])
		sec, _ := strconv.Atoi(m[4])
		if h > 23 || mi > 59 || sec > 59 {
			return 0, fmt.Errorf("invalid duration %q: hours must be < 24, minutes and seconds < 60", s)
		}
		d = time.Duration(days)*24*time.Hour + time.Duration(h)*time.Hour + time.Duration(mi)*time.Minute + time.Duration(sec)*time.Second
	} else {
		// Otherwise try Go's own duration syntax ("8h", "1h30m", "90m").
		var err error
		// Go note: plain `=` assigns to the existing d and err. Using `:=`
		// here would declare a new d local to this block and lose the result.
		d, err = time.ParseDuration(s)
		// Go note: `if err != nil { return ... }` is the standard error
		// check; Go has no exceptions, errors are ordinary return values.
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: use d.hh:mm:ss (e.g. 8.00:00:00) or a Go duration (e.g. 8h)", s)
		}
	}
	// Rules common to both forms.
	if d <= 0 {
		return 0, fmt.Errorf("invalid duration %q: must be positive", s)
	}
	if d%time.Second != 0 {
		return 0, fmt.Errorf("invalid duration %q: must be a whole number of seconds", s)
	}
	return d, nil
}

// FormatDuration renders d as "d.hh:mm:ss", the form the provider reads back
// from the server. The day part is always present, e.g. 8h -> "0.08:00:00".
// Sub-second parts are dropped.
func FormatDuration(d time.Duration) string {
	total := int64(d / time.Second)
	days := total / 86400
	total %= 86400
	return fmt.Sprintf("%d.%02d:%02d:%02d", days, total/3600, (total%3600)/60, total%60)
}

// CanonicalDuration parses s and formats it as "d.hh:mm:ss". This is the
// single "canonical spelling" used to compare durations: two inputs are the
// same lease duration exactly when their canonical forms are equal.
func CanonicalDuration(s string) (string, error) {
	d, err := ParseDuration(s)
	if err != nil {
		return "", err
	}
	return FormatDuration(d), nil
}

// CanonicalMAC normalises a hardware address or client identifier to
// lowercase hex octets joined by "-" (the form the DHCP server returns).
// Separators ":", "-", "." and spaces are accepted, as are bare hex digits.
// Any even number of hex digits is accepted, since DHCP client IDs are not
// always 6-byte MACs. Examples: "AA:BB:CC:DD:EE:FF", "aabb.ccdd.eeff" and
// "AABBCCDDEEFF" all become "aa-bb-cc-dd-ee-ff".
func CanonicalMAC(s string) (string, error) {
	// Strip separators. strings.Map calls the function for each character;
	// returning -1 drops that character.
	//
	// Go note: `func(r rune) rune {...}` is an inline anonymous function.
	// A rune is one Unicode character; '...' in single quotes is a rune.
	clean := strings.Map(func(r rune) rune {
		switch r {
		case ':', '-', '.', ' ':
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if clean == "" {
		return "", fmt.Errorf("invalid client ID %q: empty", s)
	}
	if len(clean)%2 != 0 {
		return "", fmt.Errorf("invalid client ID %q: odd number of hex digits", s)
	}
	// Decode to bytes (also rejects non-hex characters), then re-encode
	// each byte as two lowercase hex digits joined by dashes.
	b, err := hex.DecodeString(clean)
	if err != nil {
		return "", fmt.Errorf("invalid client ID %q: not hexadecimal", s)
	}
	// Go note: make([]string, n) creates a slice (list) of n empty strings;
	// `for i, x := range b` loops with index i and element x.
	parts := make([]string, len(b))
	for i, x := range b {
		parts[i] = fmt.Sprintf("%02x", x)
	}
	return strings.Join(parts, "-"), nil
}

// ParseIPv4 parses a dotted quad IPv4 address. IPv6 is rejected, since this
// provider is IPv4 only for now.
//
// Go note: netip.Addr{} is the empty ("zero") address, returned alongside
// the error when parsing fails.
func ParseIPv4(s string) (netip.Addr, error) {
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() {
		return netip.Addr{}, fmt.Errorf("invalid IPv4 address %q", s)
	}
	return a, nil
}

// ParseMask parses a contiguous IPv4 subnet mask and returns its prefix
// length, e.g. "255.255.255.0" -> 24. Non-contiguous masks such as
// "255.0.255.0" are rejected.
func ParseMask(s string) (int, error) {
	a, err := ParseIPv4(s)
	if err != nil {
		return 0, fmt.Errorf("invalid subnet mask %q", s)
	}
	// Turn the 4 bytes into one 32-bit number.
	// Go note: b[:] turns the fixed-size array into a slice view of it.
	b := a.As4()
	m := binary.BigEndian.Uint32(b[:])
	// Count leading 1 bits by shifting left until the top bit is 0.
	ones := 0
	for m&0x80000000 != 0 {
		ones++
		m <<= 1
	}
	// Any 1 bit left after the first 0 means the mask had a gap.
	if m != 0 {
		return 0, fmt.Errorf("invalid subnet mask %q: not contiguous", s)
	}
	return ones, nil
}

// NetworkAddress returns the network address of ip under mask, which is
// what the DHCP server uses as the scope ID. For example ("10.1.20.10",
// "255.255.255.0") -> "10.1.20.0". The scope resource uses it to know the
// scope ID at plan time, before the scope exists.
func NetworkAddress(ip, mask string) (string, error) {
	a, err := ParseIPv4(ip)
	if err != nil {
		return "", err
	}
	bits, err := ParseMask(mask)
	if err != nil {
		return "", err
	}
	// Prefix(bits) zeroes the host bits, giving the network prefix.
	p, err := a.Prefix(bits)
	if err != nil {
		return "", err
	}
	return p.Addr().String(), nil
}
