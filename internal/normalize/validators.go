package normalize

// Schema validators for plain string attributes. A validator runs during
// `terraform validate` and plan, before any call to the DHCP server, so a
// typo like "10.1.20.300" is reported with the attribute's path instead of
// as a PowerShell error at apply time.

import (
	"context"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

// IPv4Validator checks that a string is a dotted quad IPv4 address.
// Used in schemas as: Validators: []validator.String{normalize.IPv4Validator()}.
//
// Go note: the return type validator.String is an interface. stringCheck
// never declares that it implements it; it just has the required methods
// (Description, MarkdownDescription, ValidateString), which is enough.
// Go note: `func(s string) error { ... }` is an inline anonymous function
// stored in the struct and called later; `_` discards the parsed address.
func IPv4Validator() validator.String {
	return stringCheck{"must be an IPv4 address", func(s string) error { _, err := ParseIPv4(s); return err }}
}

// MaskValidator checks that a string is a contiguous IPv4 subnet mask.
func MaskValidator() validator.String {
	return stringCheck{"must be an IPv4 subnet mask", func(s string) error { _, err := ParseMask(s); return err }}
}

// FQDNValidator checks that a string is a valid hostname (a trailing dot is
// optional). See FQDN.
func FQDNValidator() validator.String {
	return stringCheck{"must be a DNS hostname", func(s string) error { _, err := FQDN(s); return err }}
}

// ZoneNameValidator checks that a string is a valid DNS zone name. See ZoneName.
func ZoneNameValidator() validator.String {
	return stringCheck{"must be a DNS zone name", func(s string) error { _, err := ZoneName(s); return err }}
}

// RecordNameValidator checks that a string is a record name relative to its
// zone ("@" for the apex, no trailing dot). See RecordName.
func RecordNameValidator() validator.String {
	return stringCheck{`must be a record name relative to the zone, or "@" for the apex`, func(s string) error { _, err := RecordName(s); return err }}
}

// IPv6Validator checks that a string is an IPv6 address.
func IPv6Validator() validator.String {
	return stringCheck{"must be an IPv6 address", func(s string) error { _, err := CanonicalIPv6(s); return err }}
}

// IPValidator checks that a string is an IPv4 or IPv6 address.
func IPValidator() validator.String {
	return stringCheck{"must be an IPv4 or IPv6 address", func(s string) error { _, err := CanonicalIP(s); return err }}
}

// ReverseNetworkValidator checks that a string is a network in CIDR form that
// a reverse lookup zone can be created for (IPv4 /8, /16 or /24; IPv6 on a
// nibble boundary). See ReverseZoneName.
func ReverseNetworkValidator() validator.String {
	return stringCheck{"must be a network in CIDR form on an octet (IPv4) or nibble (IPv6) boundary", func(s string) error { _, err := ReverseZoneName(s); return err }}
}

// stringCheck is a small generic string validator: a description for docs
// and error output, plus a check function that returns an error for a bad
// value. IPv4Validator and MaskValidator are both built from it.
//
// Go note: lowercase stringCheck is unexported (private to this package);
// callers only see it through the validator.String interface.
type stringCheck struct {
	desc  string
	check func(string) error
}

// Description and MarkdownDescription describe the rule (plain text and
// Markdown). The framework requires both; they are the same here.
func (v stringCheck) Description(context.Context) string         { return v.desc }
func (v stringCheck) MarkdownDescription(context.Context) string { return v.desc }

// ValidateString runs the check on the configured value. Null (not set) and
// unknown (computed from another resource, not known until apply) values are
// skipped; other validators or the framework handle those cases.
//
// Go note: resp is a pointer (*validator.StringResponse), so errors added to
// resp.Diagnostics are seen by the framework after the method returns.
func (v stringCheck) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	// Go note: `if err := f(); err != nil` calls f and checks its error in one
	// line; Go reports failures as returned error values, not exceptions.
	if err := v.check(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid Attribute Value", err.Error())
	}
}

// EnumOf returns the element of allowed equal to s ignoring case, or s
// unchanged. The DHCP server may return different casing (e.g. "Inactive"
// for "InActive"), so values are matched case-insensitively. Resources use it
// when reading from the server, so state keeps the spelling the schema
// allows and no perpetual diff appears.
//
// Go note: `allowed ...string` is a variadic parameter: callers pass any
// number of strings (or a slice followed by `...`), and inside the function
// it is a []string. `for _, a := range allowed` loops over it.
func EnumOf(s string, allowed ...string) string {
	for _, a := range allowed {
		if strings.EqualFold(a, s) {
			return a
		}
	}
	return s
}
