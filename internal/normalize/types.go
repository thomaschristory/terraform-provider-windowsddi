package normalize

// This file defines two Terraform "custom types": DurationType (lease
// durations) and MACType (MAC addresses / DHCP client IDs).
//
// Background for non-Go readers:
//
// Custom types. In terraform-plugin-framework every schema attribute has a
// type. Normally it is the built-in String type, and the framework compares
// values as plain strings. A custom type lets the provider attach its own
// behaviour to an attribute while it still looks like a string in HCL. A
// schema opts in with, for example:
//
//	"lease_duration": schema.StringAttribute{CustomType: normalize.DurationType{}, ...}
//
// Semantic equality. The framework asks a custom value "are you semantically
// equal to this other value?" (the StringSemanticEquals method below) in two
// situations: when the provider writes new state after Read/Create/Update,
// and when it compares the planned value with the prior state. If the answer
// is yes, the framework keeps the PRIOR spelling. So if the user wrote "8h"
// and Read gets "0.08:00:00" from the server, both canonicalise to
// "0.08:00:00", they are semantically equal, and state keeps "8h". The next
// plan compares "8h" (config) with "8h" (state): no diff. Without this, every
// plan would show `"8h" -> "0.08:00:00"` forever (a "perpetual diff").
// Likewise "AA:BB:CC:DD:EE:FF" vs the server's "aa-bb-cc-dd-ee-ff".
//
// Validation. The values also implement ValidateAttribute, so a bad duration
// or MAC is reported at `terraform validate`/plan time with the attribute's
// path, before anything is sent to the server.
//
// Why so many tiny methods: the framework talks to types through Go
// interfaces (basetypes.StringTypable, basetypes.StringValuableWithSemanticEquals,
// xattr.ValidateableAttribute). An interface is a list of required methods,
// and a type only qualifies if it has every one of them. Most of the methods
// below are the boilerplate those interfaces demand (String, Equal,
// ValueType, ValueFromString, ValueFromTerraform, Type). The interesting
// logic is in StringSemanticEquals, ValidateAttribute and Canonical, and
// that is shared through the helper functions semanticEqual, validate and
// canonicalOrRaw. Each method appears twice, once per custom type, because
// Go has no inheritance to share a method body across two named types.
//
// Two halves per custom type, as the framework requires:
//   - the Type (DurationType, MACType) describes the attribute's kind and
//     knows how to build values from raw Terraform data;
//   - the Value (DurationValue, MACValue) holds one actual value and carries
//     the comparison and validation behaviour.

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/attr/xattr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// canonicalFunc parses a string and returns its canonical form.
// CanonicalDuration and CanonicalMAC both have this shape, which lets the
// shared helpers below work for either type.
//
// Go note: this declares a named function type. Functions are values in Go
// and can be passed as arguments, like scriptblocks in PowerShell.
type canonicalFunc func(string) (string, error)

// The two custom string types below differ only in their canonicalisation
// function. Values compare semantically equal when their canonical forms
// match, so state keeps the user's spelling and no diff appears when the
// server returns a different spelling of the same value.

// DurationType is a string type for lease durations.
//
// Go note: `struct{ basetypes.StringType }` is an embedded struct. The field
// has a type but no name, and all of StringType's methods are "promoted":
// DurationType gets them for free, as if it inherited from StringType. The
// methods defined below in this file override (shadow) the promoted ones of
// the same name. This is Go's composition-based stand-in for inheritance.
type DurationType struct{ basetypes.StringType }

// MACType is a string type for MAC addresses and DHCP client identifiers.
// Same embedding trick as DurationType.
type MACType struct{ basetypes.StringType }

// Compile-time interface checks. Each line assigns a value to the blank
// identifier `_` with an interface type; it compiles only if the value's type
// has every method the interface requires. Nothing runs at runtime: this is
// purely a guard so a missing or misspelt method fails the build with a clear
// message instead of failing quietly inside Terraform.
//
// Go note: `var _ Interface = T{}` is the standard idiom for "assert T
// implements Interface". `_` discards the value.
var (
	_ basetypes.StringTypable                    = DurationType{}
	_ basetypes.StringTypable                    = MACType{}
	_ basetypes.StringValuableWithSemanticEquals = DurationValue{}
	_ basetypes.StringValuableWithSemanticEquals = MACValue{}
	_ xattr.ValidateableAttribute                = DurationValue{}
	_ xattr.ValidateableAttribute                = MACValue{}
)

// String returns a human readable type name, used by the framework in error
// messages and debug output. Required by attr.Type.
//
// Go note: `func (t DurationType) String() string` is a method with receiver
// t; it is called as someDurationType.String().
func (t DurationType) String() string { return "normalize.DurationType" }
func (t MACType) String() string      { return "normalize.MACType" }

// Equal reports whether o is the same custom type. Without this override the
// promoted StringType.Equal would say "not equal" to a DurationType, and the
// framework would reject values as having the wrong type.
//
// Go note: o.(DurationType) is a type assertion ("is o really a
// DurationType?"); the comma-ok form returns ok=false instead of crashing.
func (t DurationType) Equal(o attr.Type) bool { _, ok := o.(DurationType); return ok }
func (t MACType) Equal(o attr.Type) bool      { _, ok := o.(MACType); return ok }

// ValueType returns an empty value of the matching Value type, so the
// framework knows which Go type to use when reading this attribute into a
// model struct.
func (t DurationType) ValueType(context.Context) attr.Value { return DurationValue{} }
func (t MACType) ValueType(context.Context) attr.Value      { return MACValue{} }

// ValueFromString wraps an ordinary framework string value in our custom
// value type. Required by basetypes.StringTypable. Never fails, hence the
// nil diagnostics.
func (t DurationType) ValueFromString(_ context.Context, in basetypes.StringValue) (basetypes.StringValuable, diag.Diagnostics) {
	return DurationValue{StringValue: in}, nil
}

// ValueFromString wraps an ordinary framework string value in a MACValue.
func (t MACType) ValueFromString(_ context.Context, in basetypes.StringValue) (basetypes.StringValuable, diag.Diagnostics) {
	return MACValue{StringValue: in}, nil
}

// ValueFromTerraform converts the low-level wire value Terraform core sends
// (tftypes.Value) into a DurationValue. Required by attr.Type. The real work
// is shared in stringFromTerraform.
func (t DurationType) ValueFromTerraform(ctx context.Context, in tftypes.Value) (attr.Value, error) {
	return stringFromTerraform(ctx, t.StringType, in, func(s basetypes.StringValue) attr.Value { return DurationValue{StringValue: s} })
}

// ValueFromTerraform converts the wire value into a MACValue.
func (t MACType) ValueFromTerraform(ctx context.Context, in tftypes.Value) (attr.Value, error) {
	return stringFromTerraform(ctx, t.StringType, in, func(s basetypes.StringValue) attr.Value { return MACValue{StringValue: s} })
}

// stringFromTerraform lets the built-in StringType decode the wire value
// (handling null and unknown), then calls wrap to put the result in the
// caller's custom value type.
//
// Go note: wrap is a function passed as a parameter (a "func value"); the
// callers above pass small inline functions that build the right struct.
func stringFromTerraform(ctx context.Context, st basetypes.StringType, in tftypes.Value, wrap func(basetypes.StringValue) attr.Value) (attr.Value, error) {
	v, err := st.ValueFromTerraform(ctx, in)
	// Go note: Go returns errors as values; `if err != nil` checks for one
	// and returns it to the caller (there are no exceptions).
	if err != nil {
		return nil, err
	}
	// Defensive: StringType should always produce a StringValue.
	s, ok := v.(basetypes.StringValue)
	if !ok {
		return nil, fmt.Errorf("unexpected value type %T", v)
	}
	return wrap(s), nil
}

// DurationValue holds a lease duration in any accepted spelling ("8h",
// "08:00:00", "0.08:00:00"). It embeds StringValue, so it inherits methods
// such as ValueString(), IsNull() and IsUnknown().
type DurationValue struct{ basetypes.StringValue }

// MACValue holds a client identifier in any accepted spelling
// ("AA:BB:CC:DD:EE:FF", "aabb.ccdd.eeff", ...). Embeds StringValue too.
type MACValue struct{ basetypes.StringValue }

// Type returns the custom type this value belongs to. Required by
// attr.Value, and it ties each Value back to its Type.
func (v DurationValue) Type(context.Context) attr.Type { return DurationType{} }
func (v MACValue) Type(context.Context) attr.Type      { return MACType{} }

// Equal is STRICT equality: same custom type and exactly the same string
// (and null/unknown state). The framework uses it for exact comparisons;
// the lenient comparison is StringSemanticEquals below.
func (v DurationValue) Equal(o attr.Value) bool {
	ov, ok := o.(DurationValue)
	return ok && v.StringValue.Equal(ov.StringValue)
}

// Equal is strict equality for MACValue (see DurationValue.Equal).
func (v MACValue) Equal(o attr.Value) bool {
	ov, ok := o.(MACValue)
	return ok && v.StringValue.Equal(ov.StringValue)
}

// StringSemanticEquals reports whether two durations mean the same length of
// time, e.g. "8h" and "0.08:00:00". When true, the framework keeps the prior
// spelling instead of showing a diff. This is the method that prevents the
// perpetual diff described at the top of the file.
func (v DurationValue) StringSemanticEquals(_ context.Context, o basetypes.StringValuable) (bool, diag.Diagnostics) {
	ov, ok := o.(DurationValue)
	if !ok {
		return false, nil
	}
	return semanticEqual(v.StringValue, ov.StringValue, CanonicalDuration), nil
}

// StringSemanticEquals reports whether two client IDs are the same bytes,
// e.g. "AA:BB:CC:DD:EE:FF" and "aa-bb-cc-dd-ee-ff".
func (v MACValue) StringSemanticEquals(_ context.Context, o basetypes.StringValuable) (bool, diag.Diagnostics) {
	ov, ok := o.(MACValue)
	if !ok {
		return false, nil
	}
	return semanticEqual(v.StringValue, ov.StringValue, CanonicalMAC), nil
}

// semanticEqual compares two strings by their canonical forms.
//   - If either side is null or unknown (not yet known at plan time), fall
//     back to strict equality: there is nothing to canonicalise.
//   - If either side does not parse, fall back to exact string comparison;
//     the validator reports the bad value separately.
//   - Otherwise compare the canonical forms.
func semanticEqual(a, b basetypes.StringValue, canon canonicalFunc) bool {
	if a.IsNull() || a.IsUnknown() || b.IsNull() || b.IsUnknown() {
		return a.Equal(b)
	}
	ca, errA := canon(a.ValueString())
	cb, errB := canon(b.ValueString())
	if errA != nil || errB != nil {
		return a.ValueString() == b.ValueString()
	}
	return ca == cb
}

// ValidateAttribute reports an "Invalid Duration" error on the attribute
// when the configured value cannot be parsed. The framework calls it during
// validate and plan.
//
// Go note: resp is a pointer (*xattr.ValidateAttributeResponse), so changes
// made through it are visible to the framework after this returns. That is
// how errors are reported back: by adding to resp.Diagnostics.
func (v DurationValue) ValidateAttribute(_ context.Context, req xattr.ValidateAttributeRequest, resp *xattr.ValidateAttributeResponse) {
	validate(v.StringValue, req.Path, &resp.Diagnostics, CanonicalDuration, "Invalid Duration")
}

// ValidateAttribute reports an "Invalid Client ID" error for an unparsable
// MAC / client identifier.
func (v MACValue) ValidateAttribute(_ context.Context, req xattr.ValidateAttributeRequest, resp *xattr.ValidateAttributeResponse) {
	validate(v.StringValue, req.Path, &resp.Diagnostics, CanonicalMAC, "Invalid Client ID")
}

// validate is the shared body of both ValidateAttribute methods. Null and
// unknown values are skipped (unknown values are checked later, once known).
// Errors are attached to path p so Terraform points at the right attribute.
//
// Go note: &resp.Diagnostics passes the address of the diagnostics list so
// this function can append to it in place.
func validate(v basetypes.StringValue, p path.Path, diags *diag.Diagnostics, canon canonicalFunc, summary string) {
	if v.IsNull() || v.IsUnknown() {
		return
	}
	if _, err := canon(v.ValueString()); err != nil {
		diags.AddAttributeError(p, summary, err.Error())
	}
}

// Canonical returns the canonical form of the value, or the raw string when
// it does not parse (validation reports that separately). Resources and data
// sources call Canonical when they need one stable spelling, for example to
// send a client ID to the server in its own format or to compare values.
func (v DurationValue) Canonical() string { return canonicalOrRaw(v.StringValue, CanonicalDuration) }

// Canonical returns the canonical form of the value, or the raw string when
// it does not parse (validation reports that separately).
func (v MACValue) Canonical() string { return canonicalOrRaw(v.StringValue, CanonicalMAC) }

// canonicalOrRaw is the shared body of both Canonical methods.
func canonicalOrRaw(v basetypes.StringValue, canon canonicalFunc) string {
	c, err := canon(v.ValueString())
	if err != nil {
		return v.ValueString()
	}
	return c
}

// NewDurationValue returns a known (not null, not unknown) DurationValue.
// Resources use it to put a server-returned duration into state.
func NewDurationValue(s string) DurationValue {
	return DurationValue{StringValue: basetypes.NewStringValue(s)}
}

// NewMACValue returns a known MACValue, e.g. for a server-returned client ID.
func NewMACValue(s string) MACValue { return MACValue{StringValue: basetypes.NewStringValue(s)} }
