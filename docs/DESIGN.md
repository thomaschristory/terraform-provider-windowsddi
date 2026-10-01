# Design

## Goal

Manage Microsoft Windows Server DHCP and DNS configuration as Terraform state from one provider (`windowsddi`, for DNS, DHCP and IPAM):

- DHCP: scopes, reservations, exclusion ranges and option values.
- DNS: primary zones, conditional forwarders and resource record sets.

Every resource supports import so existing servers can be adopted. Typical upstream is a source of truth such as NetBox feeding Terraform.

Why one provider instead of `windowsdhcp` plus `windowsdns`: see [ADR 0001](adr/0001-single-ddi-provider.md).

## Non-goals (for now)

- DHCP: DHCPv6, failover relationships, policies, superscopes, MAC filters, server-level settings (audit log, conflict detection). Managing leases: leases are runtime data and are exposed read-only as a data source.
- DNS: DNSSEC, DNS policies, zone scopes, secondary and stub zones, root hints, cache and recursion settings, server forwarders, aging and scavenging.

## Positioning (DNS)

`hashicorp/dns` can already update records on Windows DNS through RFC 2136 dynamic updates with GSS-TSIG. This provider also manages zones and conditional forwarders, and does not need GSS-TSIG or secure dynamic updates to be enabled on the zone: it drives the `DnsServer` cmdlets like it drives the `DhcpServer` ones.

## Execution model

The provider connects to a Windows host and runs PowerShell that calls the `DhcpServer` and `DnsServer` module cmdlets. The host is either the server running the role (default, simplest) or a management host with the RSAT tools installed, in which case cmdlets get `-ComputerName <dhcp_server>` or `-ComputerName <dns_server>`.

One provider block builds one transport (`runner`) shared by a DHCP client and a DNS client, so they share the connection and the `max_concurrency` limit. When DHCP and DNS live on different servers, or need different accounts (DNS on a domain controller, DHCP on a member server), use two aliased provider blocks, each with `host` pointing at the right server.

### Module check

Every script carries a requirement for its module (`psscript.Requirement`). Before running its body, the wrapped script checks with `Get-Command` that a probe cmdlet of the module exists (`Get-DhcpServerv4Scope`, `Get-DnsServerZone`) and otherwise throws a clear error naming the role or RSAT feature to install. `Get-Command` is used rather than `Get-Module -ListAvailable` because it also sees modules imported from outside the module path (which is how the script tests load stub cmdlets). The error has a generic category, never `ObjectNotFound`, so it cannot be mistaken for "not found" and silently drop resources from state.

### Transports

Behind one interface:

```go
type Runner interface {
    // Run executes a PowerShell script with a JSON parameters object
    // and returns stdout. Implementations must be safe for concurrent use.
    Run(ctx context.Context, script string, params any) ([]byte, error)
}
```

| Transport | Library | Notes |
|---|---|---|
| `ssh` (default) | `golang.org/x/crypto/ssh` | OpenSSH Server on Windows Server 2019+. Password or key auth. Avoids WinRM auth issues. |
| `winrm` | `github.com/masterzen/winrm` | HTTPS (5986) by default. NTLM by default; Basic and Kerberos available. |

Both run a short fixed bootstrap: `powershell.exe -NoProfile -NonInteractive -EncodedCommand <base64 UTF-16LE bootstrap>`. The bootstrap reads the real script from stdin as base64-encoded UTF-8, runs it, and writes the script output to stdout as base64-encoded UTF-8. This:

- avoids every quoting problem (nothing user-controlled is on the command line),
- avoids the 8191 character `cmd.exe` command line limit (Windows OpenSSH and WinRM both start commands through `cmd.exe`),
- makes non-ASCII names survive regardless of the console code page.

`$ProgressPreference = 'SilentlyContinue'` is set so progress records never pollute stderr as CLIXML.

### SSH host key verification

SSH verifies the server host key. In order: `ssh_host_key` (a pinned public key in `authorized_keys` format), then the `known_hosts_file` (default `~/.ssh/known_hosts`). If neither matches the connection fails, unless `insecure = true`.

### WinRM authentication

`winrm_auth` selects `ntlm` (default), `basic` or `kerberos`. HTTPS (`winrm_https = true`) is the default. When HTTPS is disabled and NTLM is used, SPNEGO message encryption is used so the server does not need `AllowUnencrypted`. Kerberos is password based (no keytab or credential cache yet) and takes `kerberos_realm`, `kerberos_config` (path to `krb5.conf`, default `/etc/krb5.conf`) and an optional `kerberos_spn`.

Kerberos double hop: when connecting to a management host and targeting a different server with `-ComputerName`, credentials may not delegate. The recommended setup is connecting directly to the server that runs the role.

### Concurrency

Terraform runs resources in parallel (default 10). Limit concurrent sessions per runner with a semaphore (provider attribute `max_concurrency`, default 4). Reuse SSH connections; open a new session per command.

## Script contract

Every script follows the same shape so parsing is uniform:

```powershell
$ErrorActionPreference = 'Stop'
$PSDefaultParameterValues['*:ErrorAction'] = 'Stop'   # CDXML cmdlets ignore the preference alone
$p = '<PARAMS_JSON>' | ConvertFrom-Json               # injected as a single-quoted, escaped literal
try {
    $out = $null
    if (-not (Get-Command -Name 'Get-DhcpServerv4Scope' -ErrorAction SilentlyContinue)) { throw '...' }
    $null = . { <body, assigns $out> }
    ConvertTo-Json -InputObject @{ ok = $true; data = $out } -Depth 8 -Compress
} catch {
    ConvertTo-Json -InputObject @{ ok = $false; error = @{ message = ...; category = ...; id = ...; command = ... } } -Compress
}
```

Rules:
- User values reach PowerShell only through `$p` (JSON parameters). The JSON literal is embedded with single quotes doubled; nothing else is interpolated.
- Output is always one JSON envelope `{ok, data, error}`.
- "Not found" is detected from `category == "ObjectNotFound"` or a known API error code in the `FullyQualifiedErrorId` and mapped to the sentinel `psscript.ErrNotFound` (re-exported as `dhcp.ErrNotFound` and `dns.ErrNotFound`) so Read can drop the resource from state. Codes: `DHCP 20005` subnet not present, `DHCP 20010` option not present, `DHCP 20018` not a reserved client, `WIN32 9601` DNS zone does not exist, `WIN32 9701` DNS record does not exist, `WIN32 9714` DNS name does not exist (record scripts turn 9714 into an empty array so a missing name and a missing zone stay distinct). Where a cmdlet can list (reservations, exclusion ranges, option values, records), scripts list and filter instead of relying on error codes; an empty result is not found.
- Each script receives `$cn`, a hashtable splatted into every cmdlet (`@cn`), holding `ComputerName` when `dhcp_server` (DHCP scripts) or `dns_server` (DNS scripts) is set.
- Enums from `ConvertTo-Json` can serialise as integers; select explicit string properties (`State = "$($r.State)"`) in scripts to keep JSON stable.
- `TimeSpan` values (lease duration) are emitted as strings in `d.hh:mm:ss` form via `.ToString('d\.hh\:mm\:ss')` (plain `.ToString()` drops the day part below one day). Go sends durations to PowerShell as whole seconds.

### DNS script rules

On top of the rules above:
- Record queries always return an array (`$out = @(...)`) so a single record does not collapse to an object, and an empty name returns an empty array rather than an error.
- `RecordData` CIM objects are projected explicitly: `IPv4Address.IPAddressToString`, `IPv6Address.IPAddressToString`, `HostNameAlias`, `PtrDomainName`, `MailExchange` + `Preference`, `DomainName` + `Priority` + `Weight` + `Port`, `DescriptiveText`.
- `TimeToLive` is emitted as total seconds. Go sends TTLs as whole seconds.
- Each record carries `dynamic = $null -ne $r.Timestamp` so dynamic (client or DHCP registered) records can be detected. Static records have no timestamp.
- Records are removed by finding the matching object (all data fields compared in PowerShell) and passing it to `Remove-DnsServerResourceRecord -InputObject <obj> -Force`, because the `-RecordData` string format differs per type and is ambiguous for MX and SRV.
- `-ReplicationScope` sits in its own parameter set on `Set-DnsServerPrimaryZone` and `Set-DnsServerConditionalForwarderZone`, so scripts change it in a separate call, only when it differs.
- The client returns names in server casing; resources lowercase them. `ListZones` includes auto-created zones (`TrustAnchors`, `0.in-addr.arpa`, ...), flagged `IsAutoCreated`, which the zones data source filters out.

## Provider configuration

```hcl
provider "windowsddi" {
  host             = "srv01.example.local"
  transport        = "ssh"        # ssh | winrm
  port             = 22           # default by transport: 22, 5986 (5985 without HTTPS)
  username         = "EXAMPLE\\svc-terraform"
  password         = var.password # sensitive
  private_key      = null         # ssh only, alternative to password (PEM)
  ssh_host_key     = null         # ssh only, pinned host public key
  known_hosts_file = null         # ssh only, default ~/.ssh/known_hosts
  insecure         = false        # ssh: skip host key check; winrm: skip TLS verify
  winrm_https      = true         # winrm only
  winrm_auth       = "ntlm"       # winrm only: ntlm | basic | kerberos
  kerberos_realm   = null         # winrm kerberos only
  kerberos_config  = null         # winrm kerberos only, default /etc/krb5.conf
  kerberos_spn     = null         # winrm kerberos only, default HTTP/<host>
  dhcp_server      = null         # optional -ComputerName for DHCP cmdlets; default = host
  dns_server       = null         # optional -ComputerName for DNS cmdlets; default = host
  max_concurrency  = 4
}
```

All attributes can come from env vars `WINDOWSDDI_*` (upper-cased attribute name, e.g. `WINDOWSDDI_KERBEROS_REALM`). Explicit configuration wins over env vars.

The account needs `DHCP Administrators` membership to manage DHCP and `DnsAdmins` membership to manage DNS.

## Code layout

```
internal/
  provider/          provider config, schema, Configure(), registers both resource families
  providerdata/      the *Clients value (DHCP + DNS client) handed to resources
  runner/            Runner interface + ssh and winrm implementations (shared)
  psscript/          script templates, module check, JSON envelope parsing, ErrNotFound (shared)
  normalize/         MAC, IP, duration, FQDN, TTL, TXT helpers and custom types (shared)
  dhcp/              typed DHCP client; dhcp/dhcpfake: in-memory fake DHCP server
  dns/               typed DNS client; dns/dnsfake: in-memory fake DNS server
  recordset/         generic DNS record-set engine
  resources/dhcp/    DHCP resources (package dhcpresources)
  resources/dns/     DNS resources (package dnsresources)
  datasources/dhcp/  DHCP data sources (package dhcpdatasources)
  datasources/dns/   DNS data sources (package dnsdatasources)
  acctest/           provider factories (fake or real server), per-service PreChecks
```

The resource and data source packages are named `dhcpresources`, `dnsresources` and so on rather than after their directory, so they never shadow the `dhcp` and `dns` client packages they import.

Layering is strict: resources call `dhcp` / `dns` (or `recordset`), which call `psscript` + `runner`. Resources never build PowerShell strings.

## DHCP resources

### `windowsddi_dhcp_scope`

| Attribute | Type | Notes |
|---|---|---|
| `scope_id` | string, computed | Network address, derived from start range and mask. Resource ID. Changing `start_range` into another network forces replacement. |
| `name` | string, required | |
| `description` | string, optional | |
| `start_range` | string, required | |
| `end_range` | string, required | |
| `subnet_mask` | string, required | RequiresReplace (cannot be changed with `Set-`). |
| `state` | string, optional, default `Active` | `Active` / `InActive` |
| `lease_duration` | string, optional, default `8.00:00:00` | Accepts `d.hh:mm:ss`, `hh:mm:ss` or Go durations (`8h`, `90m`, `192h`). Custom type with semantic equality: equal durations never show a diff and state keeps the user's spelling. |
| `force_destroy` | bool, optional, default `false` | Terraform only. Pass `-Force` on delete so scopes with active leases are removed. |
| `type` | string, optional, default `Dhcp` | `Dhcp` / `Bootp` / `Both` |

Cmdlets: `Add-`, `Get-`, `Set-`, `Remove-DhcpServerv4Scope`. Import ID: `10.1.2.0`.
Delete uses `Remove-DhcpServerv4Scope -Force` only if `force_destroy = true` (default false); otherwise fail when the scope has active leases.

### `windowsddi_dhcp_reservation`

| Attribute | Type | Notes |
|---|---|---|
| `scope_id` | string, required | RequiresReplace |
| `ip_address` | string, required | RequiresReplace. Unique on the server. |
| `client_id` | string, required | MAC; accept `aa:bb:..`, `aa-bb-..`, `aabb..`, `aabb.ccdd.eeff`. Custom type with semantic equality against the canonical `aa-bb-cc-dd-ee-ff` form the server returns. Updatable in place. |
| `name` | string, optional, computed | The server fills a name when none is given. |
| `description` | string, optional | |
| `type` | string, optional, default `Both` | `Dhcp` / `Bootp` / `Both` |

Cmdlets: `Add-`, `Get-`, `Set-` (by `-IPAddress`), `Remove-DhcpServerv4Reservation`. Import ID: `10.1.2.0/10.1.2.50`.

### `windowsddi_dhcp_exclusion_range`

| Attribute | Type | Notes |
|---|---|---|
| `scope_id` | string, required | RequiresReplace |
| `start_range` | string, required | RequiresReplace |
| `end_range` | string, required | RequiresReplace |

No update path: every change replaces. Import ID: `10.1.2.0/10.1.2.1-10.1.2.20`.

### `windowsddi_dhcp_option_value`

| Attribute | Type | Notes |
|---|---|---|
| `option_id` | number, required | RequiresReplace |
| `value` | list(string), required | Order matters (e.g. DNS servers). |
| `scope_id` | string, optional | Unset = server level. RequiresReplace |
| `reserved_ip` | string, optional | Reservation level; conflicts with being server level. RequiresReplace |
| `vendor_class` | string, optional | RequiresReplace |
| `user_class` | string, optional | RequiresReplace |

Cmdlets: `Set-`, `Get-`, `Remove-DhcpServerv4OptionValue`. Create and Update both call `Set-`. A computed `name` attribute reports the option name.
Import ID: `server/6`, `scope/10.1.2.0/6`, `reservation/10.1.2.50/6`, optionally followed by `/<vendor_class>/<user_class>` (either may be empty).

Caveat: options can also be set out of band or by policies; only the targeted (level, option) pair is managed.

### DHCP data sources

- `windowsddi_dhcp_scope`: lookup by `scope_id`.
- `windowsddi_dhcp_scopes`: list all scopes.
- `windowsddi_dhcp_reservation`: lookup by `ip_address` or `client_id` within `scope_id` (exactly one).
- `windowsddi_dhcp_leases`: leases in a scope (read-only, runtime). Active leases only unless `all_leases = true` (`-AllLeases`).

### DHCP failover pairs

If scopes are in a failover relationship, write to one partner only and let replication run, or call `Invoke-DhcpServerv4FailoverReplication` afterwards. For now the constraint is documented; a `replicate_after_change` provider flag is a later candidate.

## DNS resources

### AD-integrated zones

AD-integrated zones replicate between domain controllers. Point the provider at one DC; a Read against another DC may briefly show drift until replication catches up. This is documented prominently in the README and the zone resource docs.

### `windowsddi_dns_zone` (primary zones)

| Attribute | Type | Notes |
|---|---|---|
| `name` | string, optional + computed | Forward zone name. Computed for reverse zones (from `network_id`, at plan time). Exactly one of `name` / `network_id`. Compared case-insensitively, ignoring a trailing dot. RequiresReplace |
| `network_id` | string, optional | CIDR. Creates the matching `in-addr.arpa` / `ip6.arpa` zone. The prefix must sit on an octet boundary for IPv4 (`/8`, `/16`, `/24`) and a nibble boundary for IPv6 (multiple of 4). RequiresReplace |
| `replication_scope` | string, optional | `Forest` / `Domain` / `Legacy`: AD-integrated zone. Exactly one of `replication_scope` / `zone_file`. Changing between the three values is in place (`Set-DnsServerPrimaryZone -ReplicationScope`); switching to or from file-backed is a replace. |
| `zone_file` | string, optional | File-backed zone, file name under `%windir%\System32\dns`. The server has no default file name, so one of `replication_scope` / `zone_file` is required. RequiresReplace |
| `dynamic_update` | string, optional + computed | `None` / `Secure` / `NonsecureAndSecure`. Default `Secure` for AD-integrated, `None` for file-backed. `Secure` is rejected for file-backed zones (the server does not support it). |
| `force_destroy` | bool, optional, default `false` | Terraform only. Without it, Delete fails when the zone holds records other than the apex SOA and NS records. |
| `ad_integrated`, `reverse` | bool, computed | Reported by the server. |

Cmdlets: `Add-DnsServerPrimaryZone`, `Get-DnsServerZone`, `Set-DnsServerPrimaryZone`, `Remove-DnsServerZone -Force`. Import ID: zone name.

Notes:
- A computed `id` holds the canonical zone name (lowercase, no trailing dot); zones are created on the server in lowercase.
- Replacement is decided at plan time on the resulting zone name, not on a change of `name` or `network_id` as such: switching between `name = "20.1.10.in-addr.arpa"` and the equivalent `network_id` is a no-op. `network_id` cannot be read back from the server, so an imported reverse zone has only `name`.
- Read fails with a clear error (not drift) if the named zone exists but is not a primary zone.

### `windowsddi_dns_conditional_forwarder`

| Attribute | Type | Notes |
|---|---|---|
| `name` | string, required | Compared case-insensitively, ignoring a trailing dot. RequiresReplace |
| `master_servers` | list(string), required | IPv4 or IPv6 addresses. Order matters. |
| `replication_scope` | string, optional | `Forest` / `Domain` / `Legacy`: AD-integrated. Unset: stored on this server only. Switching between AD-integrated and not is a replace; changing between the three values is in place. |
| `forwarder_timeout` | number, optional + computed | Seconds, at least 1 (server default 5). |

Cmdlets: `Add-` / `Set-DnsServerConditionalForwarderZone`, `Get-DnsServerZone`, `Remove-DnsServerZone -Force`. Import ID: zone name. Read fails with a clear error (not drift) if the named zone exists but is not a conditional forwarder.

### Record sets

One resource per record type. Each owns every record of its (zone, name, type) tuple and is authoritative: values present on the server but not in the configuration are removed.

Common attributes:

| Attribute | Type | Notes |
|---|---|---|
| `zone_name` | string, required | RequiresReplace. Compared case-insensitively, ignoring a trailing dot. |
| `name` | string, required | Relative to the zone, `@` for the apex. Compared case-insensitively. RequiresReplace |
| `ttl` | number, optional + computed | Seconds. When unset, the server default for the zone is used and reported. |
| `allow_overwrite_dynamic` | bool, optional, default `false` | Terraform only. See Create below. |

| Resource | Value attribute | Shape |
|---|---|---|
| `windowsddi_dns_a_record_set` | `addresses` | set(string) IPv4 |
| `windowsddi_dns_aaaa_record_set` | `addresses` | set(string) IPv6, compared in compressed canonical form |
| `windowsddi_dns_cname_record` | `target` | string FQDN |
| `windowsddi_dns_ptr_record` | `target` | string FQDN |
| `windowsddi_dns_mx_record_set` | `mx` | set(object{preference, exchange}) |
| `windowsddi_dns_srv_record_set` | `srv` | set(object{priority, weight, port, target}) |
| `windowsddi_dns_txt_record_set` | `txt` | set(string) |

DNS names use custom string types with semantic equality (`normalize.ZoneNameType`, `RecordNameType`, `HostnameType`), like the MAC and duration types: state keeps the user's spelling and casing differences never show a diff. Hostname values (`target`, `exchange`) are compared as lowercase FQDN with trailing dot; input without the dot is accepted. IPv6 addresses are compared in canonical compressed form. To avoid perpetual diffs without forcing users to write canonical values, Read keeps the spelling already in state for every value that is semantically equal to a server value (same idea as the MAC and duration custom types, applied per set element), and stores the canonical form otherwise.

Cmdlets: `Get-`, `Add-`, `Set-`, `Remove-DnsServerResourceRecord`. Import ID: `<zone>/<name>`, for example `example.local/www` or `example.local/@`; the record type comes from the resource type.

#### `recordset` engine

All record resources share one engine (`internal/recordset`), parameterised by record type:
- Read: `Get-DnsServerResourceRecord -ZoneName -Name -RRType`, decode values, return the set and the TTL. No records means not found (drift). When records of the tuple disagree on TTL, the engine reports the TTL that differs from the prior state so the next plan corrects it.
- Create: fail if static records already exist for the tuple and tell the user to import. If existing records are all dynamic, fail unless `allow_overwrite_dynamic = true`, in which case they are removed before the configured values are added. Then add each value.
- Update: diff the records currently on the server (not the prior state, so values added out of band are removed too) against the new values. Add new values (`Add-DnsServerResourceRecord`) before removing old ones (`Remove-DnsServerResourceRecord -InputObject -Force`) so the name never resolves empty. Kept values whose TTL changed are updated with `Set-DnsServerResourceRecord -OldInputObject -NewInputObject`. The sequence is not atomic: a failure midway leaves a mix of old and new values, which the next plan shows and fixes. CNAME is the exception: a name holds a single CNAME and the server refuses a second one, so the old target is removed first and restored (best effort) if adding the new one fails; the name is briefly empty during a CNAME target change.
- Create also rejects configured values that are the same record in two spellings, and removes the records it added when an add fails midway (best effort), so a retry does not hit the "already exists" check.
- Delete: remove all values of the tuple.
- CNAME and PTR resources hold one value. When the server has several PTR records for the name, Read reports the one that differs from state so the plan shows a diff and Update removes the extras.
- A change of `zone_name` or `name` that only differs in casing (or a trailing dot on the zone) is an in-place no-op update, not a replacement.
- A keyed mutex per (zone, name, type) inside the provider process serialises operations on the same tuple.

### DNS data sources

- `windowsddi_dns_zone`: lookup by name.
- `windowsddi_dns_zones`: list zones, optional filters `reverse` and `ad_integrated`.
- `windowsddi_dns_records`: all records in a zone, optional `name` and `type` filters, values rendered as generic strings (the zone file form of the RDATA, for example `10 mail.example.com.` for MX). Useful for feeding NetBox.

## Testing

- Unit: mock `Runner`, assert generated params and parsing of fixture JSON in `testdata/`. Cover envelope errors, not-found mapping, normalisation.
- Resource unit tests: `terraform-plugin-testing` `resource.UnitTest` against `dhcpfake` or `dnsfake`, in-memory servers implementing `Runner` (create, update, import, replace, drift, errors).
- Script tests: the real scripts and bootstrap run under a local `pwsh` against stub cmdlets loaded as a module (`internal/dhcp/testdata/stubs.ps1`, `internal/dns/testdata/stubs.ps1`). They catch syntax errors, wrong parameter names and the CDXML error-preference pitfall. Skipped without `pwsh`.
- Acceptance: Windows Server evaluation VM with OpenSSH Server and the DHCP and/or DNS role (see LAB_SETUP.md). Environment: `WINDOWSDDI_HOST`, `WINDOWSDDI_USERNAME`, `WINDOWSDDI_PASSWORD`, `WINDOWSDDI_TRANSPORT`, plus optional `WINDOWSDDI_DHCP_HOST` / `WINDOWSDDI_DNS_HOST` when the roles live on different servers. DHCP and DNS tests skip independently when their host or role is unavailable.
  - DHCP tests create scopes in the benchmark range `198.18.0.0/15` and destroy them.
  - DNS tests create file-backed `tfacc-*.test` zones. AD-integrated zone tests only run with `WINDOWSDDI_AD=1` (the host must be a domain controller).
  - `CheckDestroy` verifies via `Get-*`.

## Resolved questions

- WinRM Kerberos: supported through `masterzen/winrm` (`gokrb5`), password based only.
- `lease_duration` accepts Go-style durations in addition to `d.hh:mm:ss`.
- One provider for DHCP and DNS, named `windowsddi`: [ADR 0001](adr/0001-single-ddi-provider.md).

## Open questions

- Non-authoritative record-set mode for names shared with dynamic registrations.
