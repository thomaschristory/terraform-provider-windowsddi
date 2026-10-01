// Package provider implements the windowsddi Terraform provider.
//
// The provider is the entry point Terraform talks to. Its jobs are:
//
//   - Describe the `provider "windowsddi" {}` block (Schema): host, credentials, transport...
//   - Turn that block, plus WINDOWSDDI_* environment variables, into a live connection once
//     per Terraform run (Configure).
//   - Hand the resulting clients (one for DHCP, one for DNS, sharing the connection) to every
//     resource and data source, so they never build their own connection.
//   - List which resources and data sources exist (Resources, DataSources).
//
// Overall flow of a call, from top to bottom:
//
//	Terraform CLI
//	  -> provider (this package: config, connection)
//	  -> resources / datasources (one Go type per `windowsddi_dhcp_*` or `windowsddi_dns_*` block)
//	  -> dhcp / dns (typed Go functions such as GetScope, AddReservation, GetZone)
//	  -> psscript (PowerShell templates + JSON envelope) and runner (SSH or WinRM transport)
//	  -> Windows host running the DhcpServer or DnsServer cmdlets
//
// The "provider.Provider" interface from terraform-plugin-framework defines the methods a
// provider must have (Metadata, Schema, Configure, Resources, DataSources). The framework
// calls them; this code never calls them itself.
package provider

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	dhcpdatasources "github.com/thomaschristory/terraform-provider-windowsddi/internal/datasources/dhcp"
	dnsdatasources "github.com/thomaschristory/terraform-provider-windowsddi/internal/datasources/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dhcp"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/providerdata"
	dhcpresources "github.com/thomaschristory/terraform-provider-windowsddi/internal/resources/dhcp"
	dnsresources "github.com/thomaschristory/terraform-provider-windowsddi/internal/resources/dns"
	"github.com/thomaschristory/terraform-provider-windowsddi/internal/runner"
)

// Fixed values used across this file: the two accepted transport names, the default number
// of PowerShell commands allowed in flight at once, and the prefix of every environment
// variable the provider reads (WINDOWSDDI_HOST, WINDOWSDDI_PASSWORD, ...).
//
// Go note: `const ( ... )` declares several compile-time constants in one block.
const (
	transportSSH   = "ssh"
	transportWinRM = "winrm"

	defaultMaxConcurrency = 4
	envPrefix             = "WINDOWSDDI_"
)

// This line makes the compiler verify that *Provider has every method of the
// provider.Provider interface. If one is missing or misspelled, the build fails here with a
// clear message instead of at runtime.
//
// Go note: `var _ Interface = &Type{}` is the standard compile-time check. `_` discards the
// value; only the type check matters. `&Type{}` creates a value and takes its pointer.
var _ provider.Provider = &Provider{}

// Provider is the windowsddi provider.
//
// It holds only what is fixed before Terraform sends any configuration: the version string,
// and optionally a pre-built runner for tests. The real connection is created in Configure.
//
// Go note: a `struct` is a record type with named fields, similar to a PowerShell
// [pscustomobject] whose properties are fixed at compile time.
type Provider struct {
	version string
	// runner, when set, replaces the transport built from configuration.
	// Unit tests use it to plug in a fake DHCP or DNS server.
	runner runner.Runner
	// env, when set, replaces os.LookupEnv for the WINDOWSDDI_* fallback.
	// Acceptance tests use it to point DHCP and DNS tests at different hosts.
	env lookupEnv
}

// New returns a provider factory.
//
// The plugin framework wants a function that builds a fresh provider on demand (it may build
// more than one, for example in tests), not a single shared instance. main.go passes the
// result straight to providerserver.Serve.
//
// Go note: the return type `func() provider.Provider` means "a function taking no arguments
// that returns a provider". The inner func is a closure: it remembers `version` from the
// outer call.
func New(version string) func() provider.Provider {
	return func() provider.Provider { return &Provider{version: version} }
}

// NewWithRunner returns a provider factory whose provider executes every
// script through r instead of connecting to a host.
//
// This is the test hook: unit tests pass an in-memory fake DHCP server (dhcpfake) as r, so
// they can run real Terraform plans and applies without any Windows machine. When r is set,
// Configure skips all connection settings and env var handling.
func NewWithRunner(version string, r runner.Runner) func() provider.Provider {
	return func() provider.Provider { return &Provider{version: version, runner: r} }
}

// NewWithEnv returns a provider factory whose provider reads its WINDOWSDDI_* fallback
// values through env instead of the process environment. Acceptance tests use it to
// substitute WINDOWSDDI_DHCP_HOST or WINDOWSDDI_DNS_HOST for WINDOWSDDI_HOST.
func NewWithEnv(version string, env func(string) (string, bool)) func() provider.Provider {
	return func() provider.Provider { return &Provider{version: version, env: env} }
}

// lookup returns the environment lookup function in effect: the injected one, or
// os.LookupEnv.
func (p *Provider) lookup() lookupEnv {
	if p.env != nil {
		return p.env
	}
	return os.LookupEnv
}

// Model is the provider configuration.
//
// It is the Go shape of the `provider "windowsddi" {}` block. The framework copies the HCL
// values into these fields, matching them by the `tfsdk:"..."` names.
//
// Fields use framework types (types.String, types.Int64, types.Bool) rather than plain Go
// string/int/bool because a Terraform value can also be null (not set) or unknown (only
// known after apply). Plain Go types cannot express those two states.
//
// Go note: the text in backquotes after each field is a "struct tag". It is metadata read by
// libraries at runtime; here `tfsdk:"host"` links the Go field Host to the HCL attribute host.
type Model struct {
	Host           types.String `tfsdk:"host"`
	Transport      types.String `tfsdk:"transport"`
	Port           types.Int64  `tfsdk:"port"`
	Username       types.String `tfsdk:"username"`
	Password       types.String `tfsdk:"password"`
	PrivateKey     types.String `tfsdk:"private_key"`
	SSHHostKey     types.String `tfsdk:"ssh_host_key"`
	KnownHostsFile types.String `tfsdk:"known_hosts_file"`
	Insecure       types.Bool   `tfsdk:"insecure"`
	WinRMHTTPS     types.Bool   `tfsdk:"winrm_https"`
	WinRMAuth      types.String `tfsdk:"winrm_auth"`
	KerberosRealm  types.String `tfsdk:"kerberos_realm"`
	KerberosConfig types.String `tfsdk:"kerberos_config"`
	KerberosSPN    types.String `tfsdk:"kerberos_spn"`
	DHCPServer     types.String `tfsdk:"dhcp_server"`
	DNSServer      types.String `tfsdk:"dns_server"`
	MaxConcurrency types.Int64  `tfsdk:"max_concurrency"`
}

// Metadata implements provider.Provider.
//
// It tells Terraform the provider's type name ("windowsddi"), which becomes the prefix of
// every resource and data source name (windowsddi_dhcp_scope, ...), and its version.
//
// Go note: `func (p *Provider) Metadata(...)` is a method: a function attached to the
// Provider type. `p` is the receiver (like `$this`), and `*Provider` means it receives a
// pointer, so it works on the original value rather than a copy. Parameters named `_` are
// required by the interface but unused here.
func (p *Provider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "windowsddi"
	resp.Version = p.version
}

// envNote returns the sentence appended to each attribute description that names its
// environment variable, for example "Can be set with the `WINDOWSDDI_HOST` environment
// variable." Keeping it in one helper guarantees the docs and the real variable names match.
func envNote(attr string) string {
	return fmt.Sprintf(" Can be set with the `%s%s` environment variable.", envPrefix, strings.ToUpper(attr))
}

// Schema implements provider.Provider.
//
// It declares every attribute accepted in the `provider "windowsddi" {}` block, with its
// type, whether it is optional, whether it is sensitive (hidden in plan output and logs), and
// validators that reject bad values before Configure runs. The MarkdownDescription strings
// are also the source of the Registry documentation generated by tfplugindocs.
//
// Every attribute is Optional because each one can come from an environment variable
// instead; required-ness is enforced later in resolve.
func (p *Provider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	// Go note: `map[string]schema.Attribute{ "key": value, ... }` is a map literal (a hash
	// table, like a PowerShell @{} hashtable) from attribute name to its definition.
	// `[]validator.String{...}` is a slice literal: a growable list, like a PowerShell array.
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manage Microsoft Windows Server DHCP (IPv4) and DNS by running the `DhcpServer` and `DnsServer` PowerShell cmdlets on a Windows host over SSH or WinRM.\n\n" +
			"The recommended setup is to connect directly to the server that runs the role. When connecting to a management host with the RSAT tools installed, set `dhcp_server` and/or `dns_server`; " +
			"with WinRM and Kerberos, credentials may not be delegated to the target server (double hop). " +
			"When DHCP and DNS live on different servers or need different accounts, use two aliased provider blocks.",
		Attributes: map[string]schema.Attribute{
			"host": schema.StringAttribute{
				MarkdownDescription: "Hostname or IP address of the Windows host that runs the cmdlets (the DHCP or DNS server itself, or a management host)." + envNote("host"),
				Optional:            true,
			},
			"transport": schema.StringAttribute{
				MarkdownDescription: "How to reach `host`: `ssh` (default, OpenSSH Server on Windows) or `winrm`." + envNote("transport"),
				Optional:            true,
				Validators:          []validator.String{stringvalidator.OneOf(transportSSH, transportWinRM)},
			},
			"port": schema.Int64Attribute{
				MarkdownDescription: "Port to connect to. Defaults to `22` for SSH, `5986` for WinRM over HTTPS and `5985` for WinRM over HTTP." + envNote("port"),
				Optional:            true,
				Validators:          []validator.Int64{int64validator.Between(1, 65535)},
			},
			"username": schema.StringAttribute{
				MarkdownDescription: "Account used to connect, for example `EXAMPLE\\\\svc-terraform`. It must be a member of `DHCP Administrators` to manage DHCP and of `DnsAdmins` to manage DNS." + envNote("username"),
				Optional:            true,
			},
			"password": schema.StringAttribute{
				MarkdownDescription: "Password for `username`. With SSH and `private_key`, it is tried as the key passphrase first." + envNote("password"),
				Optional:            true,
				Sensitive:           true,
			},
			"private_key": schema.StringAttribute{
				MarkdownDescription: "PEM encoded private key for SSH public key authentication, an alternative to `password`. SSH only." + envNote("private_key"),
				Optional:            true,
				Sensitive:           true,
			},
			"ssh_host_key": schema.StringAttribute{
				MarkdownDescription: "Expected SSH host public key in `authorized_keys` format (for example `ssh-ed25519 AAAA...`). When unset, `known_hosts_file` is used. SSH only." + envNote("ssh_host_key"),
				Optional:            true,
			},
			"known_hosts_file": schema.StringAttribute{
				MarkdownDescription: "Path to an OpenSSH `known_hosts` file used to verify the host key when `ssh_host_key` is unset. Defaults to `~/.ssh/known_hosts`. SSH only." + envNote("known_hosts_file"),
				Optional:            true,
			},
			"insecure": schema.BoolAttribute{
				MarkdownDescription: "Skip host verification: the SSH host key check, or TLS certificate verification for WinRM over HTTPS. Defaults to `false`." + envNote("insecure"),
				Optional:            true,
			},
			"winrm_https": schema.BoolAttribute{
				MarkdownDescription: "Use HTTPS for WinRM. Defaults to `true`. Over HTTP with NTLM, messages are encrypted with SPNEGO. WinRM only." + envNote("winrm_https"),
				Optional:            true,
			},
			"winrm_auth": schema.StringAttribute{
				MarkdownDescription: "WinRM authentication: `ntlm` (default), `basic` or `kerberos`. WinRM only." + envNote("winrm_auth"),
				Optional:            true,
				Validators:          []validator.String{stringvalidator.OneOf(runner.WinRMAuthNTLM, runner.WinRMAuthBasic, runner.WinRMAuthKerberos)},
			},
			"kerberos_realm": schema.StringAttribute{
				MarkdownDescription: "Kerberos realm, for example `EXAMPLE.LOCAL`. Required when `winrm_auth = \"kerberos\"`." + envNote("kerberos_realm"),
				Optional:            true,
			},
			"kerberos_config": schema.StringAttribute{
				MarkdownDescription: "Path to the `krb5.conf` file. Defaults to `/etc/krb5.conf`. Kerberos only." + envNote("kerberos_config"),
				Optional:            true,
			},
			"kerberos_spn": schema.StringAttribute{
				MarkdownDescription: "Service principal name of the WinRM service. Defaults to `HTTP/<host>`. Kerberos only." + envNote("kerberos_spn"),
				Optional:            true,
			},
			"dhcp_server": schema.StringAttribute{
				MarkdownDescription: "DHCP server to manage, passed to every DHCP cmdlet as `-ComputerName`. Leave unset when `host` is the DHCP server." + envNote("dhcp_server"),
				Optional:            true,
			},
			"dns_server": schema.StringAttribute{
				MarkdownDescription: "DNS server to manage, passed to every DNS cmdlet as `-ComputerName`. Leave unset when `host` is the DNS server." + envNote("dns_server"),
				Optional:            true,
			},
			"max_concurrency": schema.Int64Attribute{
				MarkdownDescription: "Maximum number of PowerShell commands run at the same time. Defaults to `4`." + envNote("max_concurrency"),
				Optional:            true,
				Validators:          []validator.Int64{int64validator.AtLeast(1)},
			},
		},
	}
}

// settings is the resolved configuration after env var fallback.
//
// Unlike Model, it uses plain Go types: by the time a settings value exists, every field has
// a concrete value (from HCL, from the environment, or from a default), so null and unknown
// no longer need to be represented.
type settings struct {
	Host, Transport, Username, Password, PrivateKey string
	SSHHostKey, KnownHostsFile                      string
	WinRMAuth, KerberosRealm, KerberosConfig        string
	KerberosSPN, DHCPServer, DNSServer              string
	Port, MaxConcurrency                            int64
	Insecure, WinRMHTTPS                            bool
}

// lookupEnv is the signature of os.LookupEnv: given a variable name, return its value and
// whether it exists. Taking it as a parameter (instead of calling os.LookupEnv directly) lets
// unit tests feed a fake environment from a map.
//
// Go note: `type X func(...)` defines a named function type. Functions are values in Go and
// can be passed around like any other argument.
type lookupEnv func(string) (string, bool)

// resolveString picks the value of one string setting with this precedence:
// 1) the value written in HCL, 2) the WINDOWSDDI_<ATTR> environment variable if non-empty,
// 3) the default def. An empty env var counts as unset.
//
// The resolveInt and resolveBool functions below apply the same rule to numbers and booleans.
func resolveString(v types.String, attr string, env lookupEnv, def string) string {
	// Go note: `!` is "not", `&&` is "and". A value counts as "set in HCL" only when it is
	// neither null nor unknown.
	if !v.IsNull() && !v.IsUnknown() {
		return v.ValueString()
	}
	// Go note: `env(...)` returns two values. `s, ok := ...` receives both; this "comma ok"
	// form is how Go reports "found or not" without exceptions. `s` and `ok` exist only
	// inside this `if`.
	if s, ok := env(envPrefix + strings.ToUpper(attr)); ok && s != "" {
		return s
	}
	return def
}

// resolveInt is resolveString for integers (port, max_concurrency). An environment value
// that is not a valid integer is reported as an attribute error and the default is used so
// that resolution can continue and collect any other errors too.
//
// Go note: `diags *diag.Diagnostics` is a pointer, so errors added here land in the
// caller's list rather than in a throwaway copy.
func resolveInt(v types.Int64, attr string, env lookupEnv, def int64, diags *diag.Diagnostics) int64 {
	if !v.IsNull() && !v.IsUnknown() {
		return v.ValueInt64()
	}
	name := envPrefix + strings.ToUpper(attr)
	if s, ok := env(name); ok && s != "" {
		// Parse base 10 into a 64-bit integer.
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			diags.AddAttributeError(path.Root(attr), "Invalid Environment Variable", fmt.Sprintf("%s=%q is not an integer.", name, s))
			return def
		}
		return n
	}
	return def
}

// resolveBool is resolveString for booleans (insecure, winrm_https). strconv.ParseBool
// accepts 1/0, t/f, true/false and their capitalised forms; anything else is an error.
func resolveBool(v types.Bool, attr string, env lookupEnv, def bool, diags *diag.Diagnostics) bool {
	if !v.IsNull() && !v.IsUnknown() {
		return v.ValueBool()
	}
	name := envPrefix + strings.ToUpper(attr)
	if s, ok := env(name); ok && s != "" {
		b, err := strconv.ParseBool(s)
		if err != nil {
			diags.AddAttributeError(path.Root(attr), "Invalid Environment Variable", fmt.Sprintf("%s=%q is not a boolean.", name, s))
			return def
		}
		return b
	}
	return def
}

// resolve turns the raw provider block (Model) plus the environment into final settings,
// applying defaults and checking the combination is usable. It returns every problem found
// at once (as diagnostics) rather than stopping at the first, so users fix them in one pass.
//
// It is a plain function with no side effects, which is why it is tested directly in
// provider_test.go without starting Terraform.
//
// Go note: `(settings, diag.Diagnostics)` is a multiple return value: the function returns
// both the result and the list of errors/warnings.
func resolve(m Model, env lookupEnv) (settings, diag.Diagnostics) {
	// Go note: `var diags diag.Diagnostics` declares an empty list (its "zero value").
	var diags diag.Diagnostics

	// Step 1: resolve each attribute independently (HCL, then env var, then default).
	s := settings{
		Host:           resolveString(m.Host, "host", env, ""),
		Transport:      resolveString(m.Transport, "transport", env, transportSSH),
		Username:       resolveString(m.Username, "username", env, ""),
		Password:       resolveString(m.Password, "password", env, ""),
		PrivateKey:     resolveString(m.PrivateKey, "private_key", env, ""),
		SSHHostKey:     resolveString(m.SSHHostKey, "ssh_host_key", env, ""),
		KnownHostsFile: resolveString(m.KnownHostsFile, "known_hosts_file", env, ""),
		WinRMAuth:      resolveString(m.WinRMAuth, "winrm_auth", env, runner.WinRMAuthNTLM),
		KerberosRealm:  resolveString(m.KerberosRealm, "kerberos_realm", env, ""),
		KerberosConfig: resolveString(m.KerberosConfig, "kerberos_config", env, ""),
		KerberosSPN:    resolveString(m.KerberosSPN, "kerberos_spn", env, ""),
		DHCPServer:     resolveString(m.DHCPServer, "dhcp_server", env, ""),
		DNSServer:      resolveString(m.DNSServer, "dns_server", env, ""),
		Insecure:       resolveBool(m.Insecure, "insecure", env, false, &diags),
		WinRMHTTPS:     resolveBool(m.WinRMHTTPS, "winrm_https", env, true, &diags),
		MaxConcurrency: resolveInt(m.MaxConcurrency, "max_concurrency", env, defaultMaxConcurrency, &diags),
	}

	// Step 2: the default port depends on other settings, so it is resolved last:
	// 22 for SSH, 5986 for WinRM over HTTPS, 5985 for WinRM over HTTP.
	defPort := int64(22)
	if s.Transport == transportWinRM {
		defPort = 5985
		if s.WinRMHTTPS {
			defPort = 5986
		}
	}
	s.Port = resolveInt(m.Port, "port", env, defPort, &diags)

	// Step 3: validate. The schema validator already checks `transport` when it is written in
	// HCL, but a value coming from WINDOWSDDI_TRANSPORT bypasses the schema, so check again.
	//
	// Go note: in a `switch`, cases do not fall through to the next one. The empty first case
	// means "these values are fine, do nothing"; `default` catches everything else.
	switch s.Transport {
	case transportSSH, transportWinRM:
	default:
		diags.AddAttributeError(path.Root("transport"), "Invalid Transport", fmt.Sprintf("transport must be %q or %q, got %q.", transportSSH, transportWinRM, s.Transport))
	}
	if s.Host == "" {
		diags.AddAttributeError(path.Root("host"), "Missing Host", "Set host in the provider configuration or the WINDOWSDDI_HOST environment variable.")
	}
	if s.Username == "" {
		diags.AddAttributeError(path.Root("username"), "Missing Username", "Set username in the provider configuration or the WINDOWSDDI_USERNAME environment variable.")
	}
	// A password is required unless SSH key authentication is available. WinRM has no key
	// auth, so with WinRM a private_key alone is not enough.
	if s.Password == "" && (s.PrivateKey == "" || s.Transport == transportWinRM) {
		diags.AddAttributeError(path.Root("password"), "Missing Credentials", "Set password (or private_key for SSH) in the provider configuration or the WINDOWSDDI_PASSWORD environment variable.")
	}
	// Same reasoning as transport: the schema's AtLeast(1) validator does not see env vars.
	if s.MaxConcurrency < 1 {
		diags.AddAttributeError(path.Root("max_concurrency"), "Invalid Value", "max_concurrency must be at least 1.")
	}
	return s, diags
}

// runner builds the transport (SSH or WinRM) described by the resolved settings. It does not
// connect yet; the runner package opens connections lazily on the first command. Errors here
// mean the settings themselves are invalid (for example a malformed ssh_host_key, or Kerberos
// without a realm).
//
// Go note: the receiver `(s settings)` has no `*`, so the method gets a copy of the settings.
// That is fine because it only reads them.
func (s settings) runner() (runner.Runner, error) {
	// Go note: the constructors return concrete pointers (*runner.WinRM,
	// *runner.SSH). Returning one directly as a runner.Runner interface on
	// failure would give a "non-nil interface holding a nil pointer", so the
	// error case explicitly returns a plain nil.
	switch s.Transport {
	case transportWinRM:
		w, err := runner.NewWinRM(runner.WinRMConfig{
			Host: s.Host, Port: int(s.Port), Username: s.Username, Password: s.Password,
			HTTPS: s.WinRMHTTPS, Insecure: s.Insecure, Auth: s.WinRMAuth,
			KerberosRealm: s.KerberosRealm, KerberosConfig: s.KerberosConfig, KerberosSPN: s.KerberosSPN,
		})
		if err != nil {
			return nil, err
		}
		return w, nil
	default:
		// resolve has already rejected anything other than ssh or winrm, so default is SSH.
		r, err := runner.NewSSH(runner.SSHConfig{
			Host: s.Host, Port: int(s.Port), Username: s.Username, Password: s.Password,
			PrivateKey: s.PrivateKey, HostKey: s.SSHHostKey, KnownHostsFile: s.KnownHostsFile, Insecure: s.Insecure,
		})
		if err != nil {
			return nil, err
		}
		return r, nil
	}
}

// Configure implements provider.Provider.
//
// Terraform calls it once per run, after reading the provider block and before any resource
// or data source does work. It builds one *providerdata.Clients (a DHCP and a DNS client on
// top of the same runner) and stores it in resp.ResourceData and resp.DataSourceData; the
// framework then passes that same value to every resource's and data source's own Configure
// method. That way the SSH/WinRM connection and the concurrency
// limit are shared by the whole run.
//
// Errors are reported by adding diagnostics to resp (Terraform shows them to the user), not
// by returning an error value.
func (p *Provider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	// Step 1: copy the HCL provider block into a Model.
	//
	// Go note: `...` after a slice spreads its elements as separate arguments to a variadic
	// function (one accepting any number of arguments), here Append.
	var m Model
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Step 2: decide which runner to use. If a test runner was injected via NewWithRunner,
	// use it as is and skip the connection settings entirely.
	r := p.runner
	env := p.lookup()
	dhcpServer := resolveString(m.DHCPServer, "dhcp_server", env, "")
	dnsServer := resolveString(m.DNSServer, "dns_server", env, "")
	// Go note: `nil` is Go's "no value" for pointers, interfaces, maps and slices.
	if r == nil {
		// Step 3: reject unknown values. A provider attribute can be "unknown" when it refers
		// to something only known after apply (for example the IP of a VM created in the same
		// run). The provider needs real connection details already during plan, because
		// Read and plan-time lookups talk to the server, so this cannot be deferred.
		//
		// Go note: `map[string]interface{ IsUnknown() bool }` is a map whose values can be of
		// any type that has an IsUnknown() method (an inline, anonymous interface). That lets
		// one loop check String, Int64 and Bool fields alike. `for k, v := range m` iterates
		// over a map's keys and values (in random order).
		for attr, v := range map[string]interface{ IsUnknown() bool }{
			"host": m.Host, "transport": m.Transport, "port": m.Port, "username": m.Username, "password": m.Password,
			"private_key": m.PrivateKey, "ssh_host_key": m.SSHHostKey, "known_hosts_file": m.KnownHostsFile,
			"insecure": m.Insecure, "winrm_https": m.WinRMHTTPS, "winrm_auth": m.WinRMAuth,
			"kerberos_realm": m.KerberosRealm, "kerberos_config": m.KerberosConfig, "kerberos_spn": m.KerberosSPN,
			"dhcp_server": m.DHCPServer, "dns_server": m.DNSServer, "max_concurrency": m.MaxConcurrency,
		} {
			if v.IsUnknown() {
				resp.Diagnostics.AddAttributeError(path.Root(attr), "Unknown Provider Configuration",
					fmt.Sprintf("%s is not known until apply. The provider needs its connection settings during plan; configure them from values known in advance.", attr))
			}
		}
		if resp.Diagnostics.HasError() {
			return
		}

		// Step 4: merge HCL with WINDOWSDDI_* env vars and defaults, then validate.
		s, diags := resolve(m, env)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}

		// Step 5: build the SSH or WinRM runner, then wrap it with runner.Limit so no more
		// than max_concurrency PowerShell commands run at once, whatever Terraform's own
		// parallelism is.
		built, err := s.runner()
		if err != nil {
			resp.Diagnostics.AddError("Invalid Connection Settings", err.Error())
			return
		}
		r = runner.Limit(built, int(s.MaxConcurrency))
		dhcpServer = s.DHCPServer
		dnsServer = s.DNSServer

		// Log the effective settings (visible with TF_LOG=DEBUG). Password and private key are
		// deliberately left out so secrets never reach logs.
		//
		// Go note: `map[string]any{...}` is a map whose values can be of any type.
		tflog.Debug(ctx, "configured windowsddi provider", map[string]any{
			"host": s.Host, "transport": s.Transport, "port": s.Port, "username": s.Username,
			"dhcp_server": s.DHCPServer, "dns_server": s.DNSServer, "max_concurrency": s.MaxConcurrency,
		})
	}

	// Step 6: build the typed DHCP and DNS clients and share them with resources and data
	// sources. dhcpServer / dnsServer, when non-empty, are sent as -ComputerName on every
	// cmdlet of that service (management host setup); when empty, the cmdlets act on the host
	// they run on. Both clients use the same runner, so they share the connection and the
	// max_concurrency limit.
	clients := &providerdata.Clients{DHCP: dhcp.New(r, dhcpServer), DNS: dns.New(r, dnsServer)}
	resp.ResourceData = clients
	resp.DataSourceData = clients
}

// Resources implements provider.Provider.
//
// It returns one factory function per resource type (windowsddi_dhcp_scope,
// windowsddi_dns_zone, ...). The lists live in the per-service resource packages so adding a
// resource does not require editing here.
//
// Go note: `[]func() resource.Resource` is a slice of functions, each of which builds a new
// resource object when called.
func (p *Provider) Resources(context.Context) []func() resource.Resource {
	return append(dhcpresources.All(), dnsresources.All()...)
}

// DataSources implements provider.Provider.
//
// Same idea as Resources, for the read-only data sources (windowsddi_dhcp_scope,
// windowsddi_dns_zones, ...), listed in the per-service data source packages.
func (p *Provider) DataSources(context.Context) []func() datasource.DataSource {
	return append(dhcpdatasources.All(), dnsdatasources.All()...)
}

// NewClientsFromEnv builds DHCP and DNS clients from WINDOWSDDI_* variables read through
// env (pass os.LookupEnv for the real environment). Acceptance tests use it to check the
// server directly.
//
// For example, after Terraform destroys a scope, a test can call this client to confirm the
// scope is really gone on the Windows host, independently of what Terraform's state says.
// Model{} is an empty provider block, so every value comes from the environment or defaults.
//
// Go note: returning `nil, err` on failure and `clients, nil` on success is the usual Go
// pattern for "result or error".
func NewClientsFromEnv(env func(string) (string, bool)) (*providerdata.Clients, error) {
	s, diags := resolve(Model{}, env)
	if diags.HasError() {
		return nil, fmt.Errorf("resolving provider settings from environment: %v", diags)
	}
	r, err := s.runner()
	if err != nil {
		return nil, err
	}
	limited := runner.Limit(r, int(s.MaxConcurrency))
	return &providerdata.Clients{DHCP: dhcp.New(limited, s.DHCPServer), DNS: dns.New(limited, s.DNSServer)}, nil
}
