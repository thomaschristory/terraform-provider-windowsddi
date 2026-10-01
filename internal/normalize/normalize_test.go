package normalize

// Unit tests for the normalize package: duration/MAC/IP parsing, the custom
// Terraform types' semantic equality and validation, and the string
// validators. Pure Go, no Terraform binary and no DHCP server involved.
// Most tests are "table driven": a map or slice of inputs and expected
// outputs, looped over with one check each.
//
// Run with: go test ./internal/normalize/
//
// Go note: test files end in _test.go and only build under `go test`. Each
// func TestXxx(t *testing.T) is a test; t.Error records a failure and keeps
// going, t.Fatal records it and stops the test.

import (
	"context"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr/xattr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// TestParseDuration checks every accepted spelling (TimeSpan with and without
// days, Go durations, padding) and a list of inputs that must be rejected.
func TestParseDuration(t *testing.T) {
	// Go note: map[string]time.Duration{...} is a map literal: input -> want.
	ok := map[string]time.Duration{
		"8.00:00:00":     8 * 24 * time.Hour,
		"0.08:00:00":     8 * time.Hour,
		"08:00:00":       8 * time.Hour,
		"1:30:00":        90 * time.Minute,
		"8h":             8 * time.Hour,
		"192h":           8 * 24 * time.Hour,
		"90m":            90 * time.Minute,
		"1h30m15s":       time.Hour + 30*time.Minute + 15*time.Second,
		" 3.12:00:00 ":   3*24*time.Hour + 12*time.Hour,
		"49710.06:28:15": 49710*24*time.Hour + 6*time.Hour + 28*time.Minute + 15*time.Second,
	}
	// Go note: ranging over a map yields key and value on each iteration.
	for in, want := range ok {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	// Go note: `_` discards the slice index; []string{...} is a list literal.
	for _, in := range []string{"", "abc", "0s", "-1h", "1.5s", "8.24:00:00", "00:60:00", "1:2:3", "999999999.00:00:00", "8d"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("ParseDuration(%q) expected error", in)
		}
	}
}

// TestFormatDuration checks the d.hh:mm:ss output (day part always present)
// and that CanonicalDuration round-trips a Go duration.
func TestFormatDuration(t *testing.T) {
	for in, want := range map[time.Duration]string{
		8 * 24 * time.Hour:             "8.00:00:00",
		8 * time.Hour:                  "0.08:00:00",
		90*time.Minute + 5*time.Second: "0.01:30:05",
	} {
		if got := FormatDuration(in); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", in, got, want)
		}
	}
	if got, _ := CanonicalDuration("192h"); got != "8.00:00:00" {
		t.Errorf("CanonicalDuration(192h) = %q", got)
	}
}

// TestCanonicalMAC checks all separator styles map to aa-bb-..., that longer
// client identifiers are accepted, and that bad input is rejected.
func TestCanonicalMAC(t *testing.T) {
	for _, in := range []string{"AA:BB:CC:DD:EE:FF", "aa-bb-cc-dd-ee-ff", "aabbccddeeff", "aabb.ccdd.eeff", " AABBCCDDEEFF "} {
		got, err := CanonicalMAC(in)
		if err != nil || got != "aa-bb-cc-dd-ee-ff" {
			t.Errorf("CanonicalMAC(%q) = %q, %v", in, got, err)
		}
	}
	if got, err := CanonicalMAC("01-AA-BB-CC-DD-EE-FF"); err != nil || got != "01-aa-bb-cc-dd-ee-ff" {
		t.Errorf("client identifier: %q, %v", got, err)
	}
	for _, in := range []string{"", "abc", "zz:bb:cc:dd:ee:ff", "::"} {
		if _, err := CanonicalMAC(in); err == nil {
			t.Errorf("CanonicalMAC(%q) expected error", in)
		}
	}
}

// TestIPv4Helpers checks IPv4 parsing (including rejecting IPv6 and leading
// zeros), mask prefix lengths, non-contiguous masks, and network address
// derivation for a /23.
func TestIPv4Helpers(t *testing.T) {
	if _, err := ParseIPv4("10.1.2.3"); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"", "10.1.2", "::1", "10.1.2.256", "10.1.2.03"} {
		if _, err := ParseIPv4(bad); err == nil {
			t.Errorf("ParseIPv4(%q) expected error", bad)
		}
	}
	for mask, want := range map[string]int{"255.255.255.0": 24, "255.255.254.0": 23, "255.255.255.252": 30, "0.0.0.0": 0, "255.255.255.255": 32} {
		if got, err := ParseMask(mask); err != nil || got != want {
			t.Errorf("ParseMask(%q) = %d, %v", mask, got, err)
		}
	}
	for _, bad := range []string{"255.0.255.0", "255.255.255.1", "foo"} {
		if _, err := ParseMask(bad); err == nil {
			t.Errorf("ParseMask(%q) expected error", bad)
		}
	}
	if got, err := NetworkAddress("10.1.2.10", "255.255.254.0"); err != nil || got != "10.1.2.0" {
		t.Errorf("NetworkAddress = %q, %v", got, err)
	}
	if got, err := NetworkAddress("10.1.3.10", "255.255.254.0"); err != nil || got != "10.1.2.0" {
		t.Errorf("NetworkAddress = %q, %v", got, err)
	}
	if _, err := NetworkAddress("10.1.3.10", "bad"); err == nil {
		t.Error("expected error")
	}
}

// TestEnumOf checks case-insensitive matching ("Inactive" -> "InActive") and
// that unknown values pass through unchanged.
func TestEnumOf(t *testing.T) {
	if got := EnumOf("Inactive", "Active", "InActive"); got != "InActive" {
		t.Errorf("EnumOf = %q", got)
	}
	if got := EnumOf("Other", "Active"); got != "Other" {
		t.Errorf("EnumOf = %q", got)
	}
}

// TestSemanticEquality checks the core anti-perpetual-diff behaviour: equal
// meanings compare equal, different meanings or types do not, null and
// unparsable values fall back to strict comparison, and Canonical().
func TestSemanticEquality(t *testing.T) {
	ctx := context.Background()
	// eq is a small local helper.
	// Go note: a function can be stored in a variable (a closure); it can
	// use ctx and t from the surrounding test. b.(basetypes.StringValuable)
	// is a type assertion converting b to the interface the method expects.
	eq := func(a, b basetypes.StringValuableWithSemanticEquals) bool {
		ok, diags := a.StringSemanticEquals(ctx, b.(basetypes.StringValuable))
		if diags.HasError() {
			t.Fatal(diags)
		}
		return ok
	}
	if !eq(NewDurationValue("8h"), NewDurationValue("0.08:00:00")) {
		t.Error("8h should equal 0.08:00:00")
	}
	if eq(NewDurationValue("8h"), NewDurationValue("8.00:00:00")) {
		t.Error("8h should not equal 8 days")
	}
	if !eq(NewMACValue("AA:BB:CC:DD:EE:FF"), NewMACValue("aa-bb-cc-dd-ee-ff")) {
		t.Error("MAC spellings should be equal")
	}
	if eq(NewMACValue("aa:bb:cc:dd:ee:ff"), NewMACValue("aa-bb-cc-dd-ee-00")) {
		t.Error("different MACs should not be equal")
	}
	if eq(NewMACValue("aa"), NewDurationValue("8h")) {
		t.Error("different types should not be equal")
	}
	null := MACValue{StringValue: basetypes.NewStringNull()}
	if eq(null, NewMACValue("aa")) || !eq(null, null) {
		t.Error("null handling")
	}
	if !eq(NewMACValue("not hex"), NewMACValue("not hex")) || eq(NewMACValue("x"), NewMACValue("y")) {
		t.Error("unparseable values compare as raw strings")
	}
	if NewDurationValue("192h").Canonical() != "8.00:00:00" || NewMACValue("AABBCCDDEEFF").Canonical() != "aa-bb-cc-dd-ee-ff" {
		t.Error("Canonical()")
	}
	if NewMACValue("junk").Canonical() != "junk" {
		t.Error("Canonical() of invalid value should be raw")
	}
}

// TestTypes exercises the framework boilerplate methods on both custom types:
// building values from Terraform wire values and from strings, type
// equality, ValueType, and rejecting a non-string wire value.
func TestTypes(t *testing.T) {
	ctx := context.Background()
	for _, typ := range []basetypes.StringTypable{DurationType{}, MACType{}} {
		v, err := typ.ValueFromTerraform(ctx, tftypes.NewValue(tftypes.String, "aa"))
		if err != nil {
			t.Fatal(err)
		}
		if !v.Type(ctx).Equal(typ) || !typ.Equal(typ) || typ.Equal(types.StringType) || typ.String() == "" {
			t.Errorf("%T type round trip", typ)
		}
		sv, diags := typ.ValueFromString(ctx, types.StringValue("aa"))
		if diags.HasError() || !sv.Equal(v) {
			t.Errorf("%T ValueFromString", typ)
		}
		if typ.ValueType(ctx) == nil {
			t.Error("ValueType")
		}
		if _, err := typ.ValueFromTerraform(ctx, tftypes.NewValue(tftypes.Number, 1)); err == nil {
			t.Error("expected error for non-string")
		}
	}
	if NewMACValue("a").Equal(NewDurationValue("a")) {
		t.Error("cross-type Equal")
	}
}

// TestValidateAttribute checks the custom values report errors for bad input
// and skip unknown values.
func TestValidateAttribute(t *testing.T) {
	ctx := context.Background()
	// check returns true when validation produced no error.
	// Go note: &xattr.ValidateAttributeResponse{} creates a value and takes
	// its address (a pointer), so ValidateAttribute can fill it in.
	check := func(v xattr.ValidateableAttribute) bool {
		resp := &xattr.ValidateAttributeResponse{}
		v.ValidateAttribute(ctx, xattr.ValidateAttributeRequest{Path: path.Root("x")}, resp)
		return !resp.Diagnostics.HasError()
	}
	if !check(NewDurationValue("8h")) || check(NewDurationValue("eight hours")) {
		t.Error("duration validation")
	}
	if !check(NewMACValue("aa:bb:cc:dd:ee:ff")) || check(NewMACValue("zz")) {
		t.Error("MAC validation")
	}
	if !check(MACValue{StringValue: basetypes.NewStringUnknown()}) {
		t.Error("unknown values are not validated")
	}
}

// TestStringValidators checks IPv4Validator and MaskValidator accept good
// values, reject bad ones, skip null, and have descriptions.
func TestStringValidators(t *testing.T) {
	ctx := context.Background()
	// run returns true when the validator produced no error.
	run := func(v validator.String, s types.String) bool {
		resp := &validator.StringResponse{}
		v.ValidateString(ctx, validator.StringRequest{Path: path.Root("x"), ConfigValue: s}, resp)
		return !resp.Diagnostics.HasError()
	}
	if !run(IPv4Validator(), types.StringValue("10.0.0.1")) || run(IPv4Validator(), types.StringValue("10.0.0")) {
		t.Error("IPv4Validator")
	}
	if !run(MaskValidator(), types.StringValue("255.255.0.0")) || run(MaskValidator(), types.StringValue("255.0.255.0")) {
		t.Error("MaskValidator")
	}
	if !run(IPv4Validator(), types.StringNull()) {
		t.Error("null skipped")
	}
	if IPv4Validator().Description(ctx) == "" || MaskValidator().MarkdownDescription(ctx) == "" {
		t.Error("descriptions")
	}
}
