// Internal unit test for the option value comparison helper. It lives in
// package resources (not resources_test) so it can call the unexported
// function directly. Run with: go test ./internal/resources/
package dhcpresources

import "testing"

// TestOptionValuesEquivalent checks which server spellings count as "the
// same value" as the configuration (so no diff is shown).
func TestOptionValuesEquivalent(t *testing.T) {
	for _, tc := range []struct {
		a, b []string
		want bool
	}{
		{[]string{"10.0.0.1"}, []string{"10.0.0.1"}, true},
		{[]string{"Example.LOCAL"}, []string{"example.local"}, true},
		{[]string{"0x0A"}, []string{"10"}, true},
		{[]string{"0X0a"}, []string{"10"}, true},
		{[]string{"010"}, []string{"10"}, true}, // leading zero is decimal, not octal
		{[]string{"010"}, []string{"8"}, false},
		{[]string{"10.0.0.1", "10.0.0.2"}, []string{"10.0.0.2", "10.0.0.1"}, false}, // order matters
		{[]string{"1"}, []string{"1", "2"}, false},
		{[]string{"abc"}, []string{"abd"}, false},
	} {
		if got := optionValuesEquivalent(tc.a, tc.b); got != tc.want {
			t.Errorf("optionValuesEquivalent(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
