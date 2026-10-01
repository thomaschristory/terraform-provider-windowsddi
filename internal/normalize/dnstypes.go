package normalize

// Custom string types for DNS names. They follow exactly the same pattern as
// DurationType and MACType in types.go (read that file first): values compare
// semantically equal when their canonical forms match, so a user may write
// "Lab.Example.Local." while the server reports "lab.example.local" and the
// plan stays empty. State keeps the user's spelling.
//
//   - ZoneNameType: zone names (zone_name, zone and forwarder name), compared
//     with ZoneName (lowercase, no trailing dot).
//   - HostnameType: record targets (CNAME, PTR), compared with FQDN
//     (lowercase, trailing dot).
//   - RecordNameType: record owner names relative to the zone ("www", "@"),
//     compared with RecordName (lowercase).

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/attr/xattr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// ZoneNameType is a string type for DNS zone names.
type ZoneNameType struct{ basetypes.StringType }

// HostnameType is a string type for fully qualified host names.
type HostnameType struct{ basetypes.StringType }

// RecordNameType is a string type for record names relative to a zone.
type RecordNameType struct{ basetypes.StringType }

var (
	_ basetypes.StringTypable                    = ZoneNameType{}
	_ basetypes.StringTypable                    = HostnameType{}
	_ basetypes.StringTypable                    = RecordNameType{}
	_ basetypes.StringValuableWithSemanticEquals = ZoneNameValue{}
	_ basetypes.StringValuableWithSemanticEquals = HostnameValue{}
	_ basetypes.StringValuableWithSemanticEquals = RecordNameValue{}
	_ xattr.ValidateableAttribute                = ZoneNameValue{}
	_ xattr.ValidateableAttribute                = HostnameValue{}
	_ xattr.ValidateableAttribute                = RecordNameValue{}
)

func (t ZoneNameType) String() string   { return "normalize.ZoneNameType" }
func (t HostnameType) String() string   { return "normalize.HostnameType" }
func (t RecordNameType) String() string { return "normalize.RecordNameType" }

func (t ZoneNameType) Equal(o attr.Type) bool   { _, ok := o.(ZoneNameType); return ok }
func (t HostnameType) Equal(o attr.Type) bool   { _, ok := o.(HostnameType); return ok }
func (t RecordNameType) Equal(o attr.Type) bool { _, ok := o.(RecordNameType); return ok }

func (t ZoneNameType) ValueType(context.Context) attr.Value   { return ZoneNameValue{} }
func (t HostnameType) ValueType(context.Context) attr.Value   { return HostnameValue{} }
func (t RecordNameType) ValueType(context.Context) attr.Value { return RecordNameValue{} }

// ValueFromString wraps a framework string value in a ZoneNameValue.
func (t ZoneNameType) ValueFromString(_ context.Context, in basetypes.StringValue) (basetypes.StringValuable, diag.Diagnostics) {
	return ZoneNameValue{StringValue: in}, nil
}

// ValueFromString wraps a framework string value in a HostnameValue.
func (t HostnameType) ValueFromString(_ context.Context, in basetypes.StringValue) (basetypes.StringValuable, diag.Diagnostics) {
	return HostnameValue{StringValue: in}, nil
}

// ValueFromString wraps a framework string value in a RecordNameValue.
func (t RecordNameType) ValueFromString(_ context.Context, in basetypes.StringValue) (basetypes.StringValuable, diag.Diagnostics) {
	return RecordNameValue{StringValue: in}, nil
}

// ValueFromTerraform converts the wire value into a ZoneNameValue.
func (t ZoneNameType) ValueFromTerraform(ctx context.Context, in tftypes.Value) (attr.Value, error) {
	return stringFromTerraform(ctx, t.StringType, in, func(s basetypes.StringValue) attr.Value { return ZoneNameValue{StringValue: s} })
}

// ValueFromTerraform converts the wire value into a HostnameValue.
func (t HostnameType) ValueFromTerraform(ctx context.Context, in tftypes.Value) (attr.Value, error) {
	return stringFromTerraform(ctx, t.StringType, in, func(s basetypes.StringValue) attr.Value { return HostnameValue{StringValue: s} })
}

// ValueFromTerraform converts the wire value into a RecordNameValue.
func (t RecordNameType) ValueFromTerraform(ctx context.Context, in tftypes.Value) (attr.Value, error) {
	return stringFromTerraform(ctx, t.StringType, in, func(s basetypes.StringValue) attr.Value { return RecordNameValue{StringValue: s} })
}

// ZoneNameValue holds a zone name in any casing, with or without trailing dot.
type ZoneNameValue struct{ basetypes.StringValue }

// HostnameValue holds a host name in any casing, with or without trailing dot.
type HostnameValue struct{ basetypes.StringValue }

// RecordNameValue holds a relative record name in any casing.
type RecordNameValue struct{ basetypes.StringValue }

func (v ZoneNameValue) Type(context.Context) attr.Type   { return ZoneNameType{} }
func (v HostnameValue) Type(context.Context) attr.Type   { return HostnameType{} }
func (v RecordNameValue) Type(context.Context) attr.Type { return RecordNameType{} }

// Equal is strict equality (see DurationValue.Equal).
func (v ZoneNameValue) Equal(o attr.Value) bool {
	ov, ok := o.(ZoneNameValue)
	return ok && v.StringValue.Equal(ov.StringValue)
}

// Equal is strict equality (see DurationValue.Equal).
func (v HostnameValue) Equal(o attr.Value) bool {
	ov, ok := o.(HostnameValue)
	return ok && v.StringValue.Equal(ov.StringValue)
}

// Equal is strict equality (see DurationValue.Equal).
func (v RecordNameValue) Equal(o attr.Value) bool {
	ov, ok := o.(RecordNameValue)
	return ok && v.StringValue.Equal(ov.StringValue)
}

// StringSemanticEquals compares zone names case-insensitively, ignoring a trailing dot.
func (v ZoneNameValue) StringSemanticEquals(_ context.Context, o basetypes.StringValuable) (bool, diag.Diagnostics) {
	ov, ok := o.(ZoneNameValue)
	return ok && semanticEqual(v.StringValue, ov.StringValue, ZoneName), nil
}

// StringSemanticEquals compares host names case-insensitively, ignoring a trailing dot.
func (v HostnameValue) StringSemanticEquals(_ context.Context, o basetypes.StringValuable) (bool, diag.Diagnostics) {
	ov, ok := o.(HostnameValue)
	return ok && semanticEqual(v.StringValue, ov.StringValue, FQDN), nil
}

// StringSemanticEquals compares record names case-insensitively.
func (v RecordNameValue) StringSemanticEquals(_ context.Context, o basetypes.StringValuable) (bool, diag.Diagnostics) {
	ov, ok := o.(RecordNameValue)
	return ok && semanticEqual(v.StringValue, ov.StringValue, RecordName), nil
}

// ValidateAttribute reports an invalid zone name.
func (v ZoneNameValue) ValidateAttribute(_ context.Context, req xattr.ValidateAttributeRequest, resp *xattr.ValidateAttributeResponse) {
	validate(v.StringValue, req.Path, &resp.Diagnostics, ZoneName, "Invalid Zone Name")
}

// ValidateAttribute reports an invalid host name.
func (v HostnameValue) ValidateAttribute(_ context.Context, req xattr.ValidateAttributeRequest, resp *xattr.ValidateAttributeResponse) {
	validate(v.StringValue, req.Path, &resp.Diagnostics, FQDN, "Invalid Host Name")
}

// ValidateAttribute reports an invalid record name.
func (v RecordNameValue) ValidateAttribute(_ context.Context, req xattr.ValidateAttributeRequest, resp *xattr.ValidateAttributeResponse) {
	validate(v.StringValue, req.Path, &resp.Diagnostics, RecordName, "Invalid Record Name")
}

// Canonical returns ZoneName of the value, or the raw string when invalid.
func (v ZoneNameValue) Canonical() string { return canonicalOrRaw(v.StringValue, ZoneName) }

// Canonical returns FQDN of the value, or the raw string when invalid.
func (v HostnameValue) Canonical() string { return canonicalOrRaw(v.StringValue, FQDN) }

// Canonical returns RecordName of the value, or the raw string when invalid.
func (v RecordNameValue) Canonical() string { return canonicalOrRaw(v.StringValue, RecordName) }

// NewZoneNameValue returns a known ZoneNameValue.
func NewZoneNameValue(s string) ZoneNameValue {
	return ZoneNameValue{StringValue: basetypes.NewStringValue(s)}
}

// NewHostnameValue returns a known HostnameValue.
func NewHostnameValue(s string) HostnameValue {
	return HostnameValue{StringValue: basetypes.NewStringValue(s)}
}

// NewRecordNameValue returns a known RecordNameValue.
func NewRecordNameValue(s string) RecordNameValue {
	return RecordNameValue{StringValue: basetypes.NewStringValue(s)}
}
