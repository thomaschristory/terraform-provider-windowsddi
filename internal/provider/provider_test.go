// Tests for the provider configuration: env var fallback and defaults (resolve), validation
// errors, the schema itself, and Configure building a real SSH or WinRM transport.
//
// Run them with:
//
//	go test ./internal/provider/
//
// They need no Windows host. Most call resolve directly with a fake environment. The last
// one (TestConfigureConnects) runs the real `terraform` binary (it must be on PATH) against
// the provider and expects connection errors, because it points at a closed local port.
// There are no TestAcc* tests in this package.
//
// Go note: a file ending in _test.go is compiled only by `go test`. It is in the same package
// as provider.go, so it can call unexported helpers such as resolve and lookupEnv.
package provider

import (
	"context"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// env turns a plain map into a fake environment with the same signature as os.LookupEnv, so
// tests control exactly which WINDOWSDDI_* variables "exist" without touching the real
// process environment.
//
// Go note: env returns a closure (an inline function that captures the map m). `v, ok :=
// m[k]` is the "comma ok" map lookup: ok is false when the key is missing.
func env(m map[string]string) lookupEnv {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

// TestResolveDefaults checks the defaults when only host, username and password are given:
// SSH on port 22, 4 concurrent commands, host verification on, WinRM HTTPS and NTLM.
//
// Go note: every function named TestXxx(t *testing.T) is run by `go test`. t.Fatal marks the
// test failed and stops it; t.Errorf marks it failed but keeps going.
func TestResolveDefaults(t *testing.T) {
	s, diags := resolve(Model{}, env(map[string]string{
		"WINDOWSDDI_HOST": "dhcp01", "WINDOWSDDI_USERNAME": "u", "WINDOWSDDI_PASSWORD": "p",
	}))
	if diags.HasError() {
		t.Fatal(diags)
	}
	if s.Transport != "ssh" || s.Port != 22 || s.MaxConcurrency != 4 || s.Insecure || !s.WinRMHTTPS || s.WinRMAuth != "ntlm" {
		t.Fatalf("unexpected defaults %+v", s)
	}
}

// TestResolvePrecedence checks that HCL values beat env vars, that a null HCL value falls
// back to the env var, and that the default port follows transport and winrm_https
// (5985 for WinRM over HTTP, 5986 over HTTPS).
func TestResolvePrecedence(t *testing.T) {
	m := Model{
		Host:      types.StringValue("from-config"),
		Transport: types.StringValue("winrm"),
		Username:  types.StringNull(),
		Password:  types.StringValue("p"),
	}
	s, diags := resolve(m, env(map[string]string{
		"WINDOWSDDI_HOST": "from-env", "WINDOWSDDI_USERNAME": "env-user", "WINDOWSDDI_WINRM_HTTPS": "false",
		"WINDOWSDDI_MAX_CONCURRENCY": "8", "WINDOWSDDI_INSECURE": "true", "WINDOWSDDI_DHCP_SERVER": "dhcp02",
	}))
	if diags.HasError() {
		t.Fatal(diags)
	}
	if s.Host != "from-config" || s.Username != "env-user" || s.Port != 5985 || s.WinRMHTTPS || s.MaxConcurrency != 8 || !s.Insecure || s.DHCPServer != "dhcp02" {
		t.Fatalf("unexpected settings %+v", s)
	}
	m.WinRMHTTPS = types.BoolValue(true)
	if s, _ = resolve(m, env(nil)); s.Port != 5986 {
		t.Fatalf("winrm https default port = %d", s.Port)
	}
}

// TestResolveErrors feeds broken environments to resolve and checks each produces the
// expected error summary (missing host, bad transport, non-numeric port, ...).
//
// Go note: this is a "table-driven test". The map holds one entry per case (name -> inputs
// and expected result), using an anonymous struct type declared inline. The loop runs each
// case as a named subtest with t.Run, so failures report which case broke.
func TestResolveErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"missing host":     {map[string]string{"WINDOWSDDI_USERNAME": "u", "WINDOWSDDI_PASSWORD": "p"}, "Missing Host"},
		"missing username": {map[string]string{"WINDOWSDDI_HOST": "h", "WINDOWSDDI_PASSWORD": "p"}, "Missing Username"},
		"missing password": {map[string]string{"WINDOWSDDI_HOST": "h", "WINDOWSDDI_USERNAME": "u"}, "Missing Credentials"},
		"winrm needs password": {map[string]string{"WINDOWSDDI_HOST": "h", "WINDOWSDDI_USERNAME": "u",
			"WINDOWSDDI_TRANSPORT": "winrm", "WINDOWSDDI_PRIVATE_KEY": "k"}, "Missing Credentials"},
		"bad transport": {map[string]string{"WINDOWSDDI_HOST": "h", "WINDOWSDDI_USERNAME": "u", "WINDOWSDDI_PASSWORD": "p",
			"WINDOWSDDI_TRANSPORT": "telnet"}, "Invalid Transport"},
		"bad port": {map[string]string{"WINDOWSDDI_HOST": "h", "WINDOWSDDI_USERNAME": "u", "WINDOWSDDI_PASSWORD": "p",
			"WINDOWSDDI_PORT": "ssh"}, "Invalid Environment Variable"},
		"bad bool": {map[string]string{"WINDOWSDDI_HOST": "h", "WINDOWSDDI_USERNAME": "u", "WINDOWSDDI_PASSWORD": "p",
			"WINDOWSDDI_INSECURE": "maybe"}, "Invalid Environment Variable"},
		"bad concurrency": {map[string]string{"WINDOWSDDI_HOST": "h", "WINDOWSDDI_USERNAME": "u", "WINDOWSDDI_PASSWORD": "p",
			"WINDOWSDDI_MAX_CONCURRENCY": "0"}, "Invalid Value"},
	} {
		t.Run(name, func(t *testing.T) {
			// resolve collects several errors at once, so search the list for the one we
			// expect rather than checking only the first. `_` discards the settings result.
			_, diags := resolve(Model{}, env(tc.env))
			found := false
			for _, d := range diags.Errors() {
				if d.Summary() == tc.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected %q, got %v", tc.want, diags)
			}
		})
	}
}

// TestSSHKeyOnlyIsEnough checks that with SSH, a private key without a password passes
// validation (key auth), unlike WinRM which always needs a password.
func TestSSHKeyOnlyIsEnough(t *testing.T) {
	_, diags := resolve(Model{}, env(map[string]string{"WINDOWSDDI_HOST": "h", "WINDOWSDDI_USERNAME": "u", "WINDOWSDDI_PRIVATE_KEY": "k"}))
	if diags.HasError() {
		t.Fatal(diags)
	}
}

// TestSchemaValid asks the framework to validate the provider schema and checks every
// attribute has a MarkdownDescription (the Registry docs are generated from them).
func TestSchemaValid(t *testing.T) {
	// New("test") returns a factory; the trailing () calls it to get the provider itself.
	p := New("test")()
	resp := &provider.SchemaResponse{}
	p.Schema(context.Background(), provider.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(context.Background()); diags.HasError() {
		t.Fatal(diags)
	}
	for name, a := range resp.Schema.Attributes {
		if a.GetMarkdownDescription() == "" {
			t.Errorf("attribute %s has no MarkdownDescription", name)
		}
	}
}

// TestConfigureConnects exercises Configure with a real transport: the
// configuration is valid, so the failure comes from dialing a closed port.
//
// Each step is a small Terraform configuration that terraform-plugin-testing plans with the
// real terraform binary. The three steps cover: SSH dial failure, Kerberos without a realm
// (rejected when building the WinRM runner), and an unparseable ssh_host_key.
func TestConfigureConnects(t *testing.T) {
	// Tell the test harness to serve this provider in-process under the name "windowsddi",
	// speaking plugin protocol v6, instead of downloading it from the Registry.
	factories := map[string]func() (tfprotov6.ProviderServer, error){
		"windowsddi": providerserver.NewProtocol6WithError(New("test")()),
	}
	resource.UnitTest(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{
			{
				Config: `
provider "windowsddi" {
  host     = "127.0.0.1"
  port     = 1
  username = "u"
  password = "p"
  insecure = true
}

data "windowsddi_dhcp_scopes" "all" {}
`,
				ExpectError: regexp.MustCompile(`ssh: running PowerShell: connecting to 127.0.0.1:1`),
			},
			{
				Config: `
provider "windowsddi" {
  host        = "127.0.0.1"
  username    = "u"
  password    = "p"
  transport   = "winrm"
  winrm_auth  = "kerberos"
}

data "windowsddi_dhcp_scopes" "all" {}
`,
				ExpectError: regexp.MustCompile(`kerberos_realm is required`),
			},
			{
				Config: `
provider "windowsddi" {
  host     = "127.0.0.1"
  username = "u"
  password = "p"
  ssh_host_key = "not a key"
}

data "windowsddi_dhcp_scopes" "all" {}
`,
				ExpectError: regexp.MustCompile(`parsing ssh_host_key`),
			},
		},
	})
}
