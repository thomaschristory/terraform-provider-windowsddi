// Helpers shared by the zone-level resources (zone.go, conditional_forwarder.go). Names are
// prefixed with "zone" so they cannot collide with the record-set helpers of this package.

package dnsresources

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/thomaschristory/terraform-provider-windowsddi/internal/normalize"
)

// Accepted values of replication_scope and dynamic_update, spelled like the cmdlet
// parameters. Values read from the server go through normalize.EnumOf so state keeps these
// spellings.
const (
	dynamicUpdateNone      = "None"
	dynamicUpdateSecure    = "Secure"
	dynamicUpdateNonsecure = "NonsecureAndSecure"
)

var (
	zoneReplicationScopes = []string{"Forest", "Domain", "Legacy"}
	zoneDynamicUpdates    = []string{dynamicUpdateNone, dynamicUpdateSecure, dynamicUpdateNonsecure}
)

// zoneErrSummary builds the first line of an error diagnostic, for example
// "Unable to create DNS zone lab.example.local". The detail (cmdlet and PowerShell message)
// comes from the dns client error.
func zoneErrSummary(action, what string) string {
	return fmt.Sprintf("Unable to %s %s", action, what)
}

// zoneOptString maps a server string to a Terraform value: "" (not applicable, for example
// the replication scope of a file-backed zone) becomes null.
func zoneOptString(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}

// zoneCanonical returns the canonical zone name (lowercase, no trailing dot) of s, or s
// lowercased when it is not a valid name (validators report that separately).
func zoneCanonical(s string) string {
	c, err := normalize.ZoneName(s)
	if err != nil {
		return strings.ToLower(s)
	}
	return c
}

// zoneReplicationScopeReplace forces replacement when replication_scope switches between
// set (AD-integrated) and unset (file-backed zone, or forwarder stored on this server only).
// Changing between Forest, Domain and Legacy stays in place: the scripts move the zone with
// Set-DnsServerPrimaryZone / Set-DnsServerConditionalForwarderZone -ReplicationScope.
//
// stringplanmodifier.RequiresReplaceIf only calls the function on updates where the value
// changed, so prior state exists here. An unknown planned value could turn out to be null,
// so it conservatively forces replacement too.
func zoneReplicationScopeReplace() planmodifier.String {
	return stringplanmodifier.RequiresReplaceIf(
		func(_ context.Context, req planmodifier.StringRequest, resp *stringplanmodifier.RequiresReplaceIfFuncResponse) {
			resp.RequiresReplace = req.PlanValue.IsUnknown() || req.PlanValue.IsNull() != req.StateValue.IsNull()
		},
		"Switching between AD-integrated (replication_scope set) and not AD-integrated replaces the zone.",
		"Switching between AD-integrated (`replication_scope` set) and not AD-integrated replaces the zone.",
	)
}

// zoneNameReplace forces replacement when a zone name changes other than in spelling:
// "Lab.Example.local." and "lab.example.local" name the same zone, so switching between them
// is an in-place update (state takes the new spelling, nothing is sent to the server).
func zoneNameReplace() planmodifier.String {
	return stringplanmodifier.RequiresReplaceIf(
		func(_ context.Context, req planmodifier.StringRequest, resp *stringplanmodifier.RequiresReplaceIfFuncResponse) {
			if req.PlanValue.IsUnknown() || req.StateValue.IsNull() {
				resp.RequiresReplace = !req.StateValue.IsNull()
				return
			}
			resp.RequiresReplace = zoneCanonical(req.PlanValue.ValueString()) != zoneCanonical(req.StateValue.ValueString())
		},
		"Renaming the zone replaces it; changes in casing or a trailing dot do not.",
		"Renaming the zone replaces it; changes in casing or a trailing dot do not.",
	)
}
