// This file holds two tiny helpers shared by the resource tests in this directory. They
// shorten the plan checks (ConfigPlanChecks) that assert a planned attribute value, for
// example "scope_id is already known as 10.1.2.0 at plan time".
//
// Run the tests with: go test ./internal/resources/
//
// Go note: `package resources_test` (with the _test suffix) is an "external test package".
// The tests compile separately and can only use the exported API of package resources,
// exactly like the real provider does, so they test behaviour rather than internals.

package dhcpresources_test

import (
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// tfjsonPath returns a path to a top-level attribute in Terraform's JSON plan output, used
// by plancheck.ExpectKnownValue to point at the attribute to check.
func tfjsonPath(attr string) tfjsonpath.Path { return tfjsonpath.New(attr) }

// knownString returns a check that the value is known (not "(known after apply)") and
// equal to s.
func knownString(s string) knownvalue.Check { return knownvalue.StringExact(s) }
