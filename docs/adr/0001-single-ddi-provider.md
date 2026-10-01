# ADR 0001: one provider for Windows DNS and DHCP

Status: accepted, 2026-10-01.

This change has been applied: the repository was renamed and restarted as `terraform-provider-windowsddi`, and `CLAUDE.md`, `docs/DESIGN.md`, `docs/ROADMAP.md` and `README.md` now describe one provider. DESIGN.md is the living version of the DNS design below; this record keeps the decision and its rationale as accepted.

## Decision

Build a single Terraform provider that manages both Microsoft Windows Server DHCP and DNS, instead of two providers (`windowsdhcp` and `windowsdns`).

Rationale:
- Both services are driven the same way: PowerShell cmdlets (`DhcpServer`, `DnsServer` modules) run remotely over SSH or WinRM, with the same script contract, error handling, config and test harness. One codebase avoids a shared library or duplicated packages.
- One maintainer: one repo, CI pipeline, Registry entry, issue tracker and docs site.
- Common practice for DDI providers (Infoblox, BlueCat ship DNS and DHCP in one provider).
- Leaves room for cross-service features later (reservation plus A/PTR records).

Different hosts or credentials per service are handled with provider aliases or the per-service targets below.

## New identity

| Item | Old | New |
|---|---|---|
| Repo | `terraform-provider-windowsdhcp` | `terraform-provider-windowsddi` |
| Go module | `github.com/thomaschristory/terraform-provider-windowsdhcp` | `github.com/thomaschristory/terraform-provider-windowsddi` |
| Registry source | `thomaschristory/windowsdhcp` | `thomaschristory/windowsddi` |
| Provider type | `windowsdhcp` | `windowsddi` |
| Env var prefix | `WINDOWSDHCP_` | `WINDOWSDDI_` |

Resource and data source names carry the service after the provider prefix:

| Old | New |
|---|---|
| `windowsdhcp_scope` | `windowsddi_dhcp_scope` |
| `windowsdhcp_reservation` | `windowsddi_dhcp_reservation` |
| `windowsdhcp_exclusion_range` | `windowsddi_dhcp_exclusion_range` |
| `windowsdhcp_option_value` | `windowsddi_dhcp_option_value` |
| `windowsdhcp_scopes` (data) | `windowsddi_dhcp_scopes` |
| `windowsdhcp_leases` (data) | `windowsddi_dhcp_leases` |

New DNS names are listed under "DNS design".

## Provider configuration

```hcl
provider "windowsddi" {
  host            = "srv01.example.local"
  transport       = "ssh"        # ssh | winrm
  port            = 22           # default by transport: 22, 5986
  username        = "EXAMPLE\\svc-terraform"
  password        = var.password  # sensitive
  private_key     = null         # ssh only
  insecure        = false        # winrm: skip TLS verify
  dhcp_server     = null         # optional -ComputerName for DHCP cmdlets; default = host
  dns_server      = null         # optional -ComputerName for DNS cmdlets; default = host
  max_concurrency = 4
}
```

Recommended pattern when DNS (on a DC) and DHCP live on different servers or need different accounts: two aliased provider blocks, each pointing `host` at the right server. Document this in the README.

Each feature checks at first use that its PowerShell module is present (`Get-Module -ListAvailable DhcpServer` / `DnsServer`) and returns a clear error naming the missing role or RSAT feature.

## Layout

```
main.go
internal/
  provider/          provider config, schema, Configure(), registers both resource families
  runner/            Runner interface + ssh and winrm implementations (shared)
  psscript/          script templates, JSON envelope parsing, ErrNotFound (shared)
  normalize/         MAC, IP, duration, FQDN, TTL, TXT helpers (shared)
  dhcp/              typed DHCP client (GetScope, AddReservation, ...)
  dns/               typed DNS client (GetZone, AddRecord, ...)
  recordset/         generic DNS record-set engine
  resources/dhcp/    DHCP resources + tests
  resources/dns/     DNS resources + tests
  datasources/dhcp/
  datasources/dns/
```

Layering stays strict: resources call `dhcp` / `dns` (or `recordset`), which call `psscript` + `runner`. Resources never build PowerShell strings.

## DNS design

### Positioning

`hashicorp/dns` can update records on Windows DNS via RFC 2136 with GSS-TSIG. This provider also manages zones and conditional forwarders, and does not need GSS-TSIG or secure dynamic updates on the zone.

### Script contract additions

Same envelope as DHCP (`{ok, data, error}`, parameters only via `$p`), plus:
- Record queries always return an array (`@(...)`) so one record does not collapse to an object.
- Project `RecordData` CIM objects explicitly: `IPv4Address.IPAddressToString`, `IPv6Address.IPAddressToString`, `HostNameAlias`, `PtrDomainName`, `MailExchange` + `Preference`, `DomainName` + `Priority` + `Weight` + `Port`, `DescriptiveText`.
- `TimeToLive` emitted as total seconds.
- Emit `dynamic = $null -ne $r.Timestamp` so dynamic (client/DHCP-registered) records can be detected.

### AD-integrated zones

They replicate between domain controllers. Point the provider at one DC; a Read against another DC may briefly show drift. Document prominently.

### `windowsddi_dns_zone` (primary zones)

| Attribute | Type | Notes |
|---|---|---|
| `name` | string, optional+computed | Forward zone name, computed for reverse zones. Exactly one of `name` / `network_id`. RequiresReplace |
| `network_id` | string, optional | CIDR, creates the matching `in-addr.arpa` / `ip6.arpa` zone. RequiresReplace |
| `replication_scope` | string, optional | `Forest` / `Domain` / `Legacy`. Conflicts with `zone_file`. |
| `zone_file` | string, optional | File-backed zone. RequiresReplace |
| `dynamic_update` | string, optional | `None` / `Secure` / `NonsecureAndSecure`. Default `Secure` for AD, `None` for file. |
| `force_destroy` | bool, default false | Required to delete a zone holding records beyond SOA/NS. |

Cmdlets: `Add-DnsServerPrimaryZone`, `Get-DnsServerZone`, `Set-DnsServerPrimaryZone`, `Remove-DnsServerZone -Force`. Import ID: zone name. Switching AD-integrated and file-backed is a replace in v0.x.

### `windowsddi_dns_conditional_forwarder`

| Attribute | Type | Notes |
|---|---|---|
| `name` | string, required | RequiresReplace |
| `master_servers` | list(string), required | Order matters. |
| `replication_scope` | string, optional | AD-integrated if set. RequiresReplace when switching mode. |
| `forwarder_timeout` | number, optional | Seconds. |

Cmdlets: `Add-` / `Set-DnsServerConditionalForwarderZone`, `Get-DnsServerZone`, `Remove-DnsServerZone`. Import ID: zone name.

### Record sets

One resource per record type. Each owns every record of its (zone, name, type) and is authoritative: values not in config are removed.

Common attributes: `zone_name` (RequiresReplace), `name` (relative, `@` for apex, stored lowercase, RequiresReplace), `ttl` (seconds, optional+computed).

| Resource | Value attribute | Shape |
|---|---|---|
| `windowsddi_dns_a_record_set` | `addresses` | set(string) IPv4 |
| `windowsddi_dns_aaaa_record_set` | `addresses` | set(string) IPv6, compressed canonical |
| `windowsddi_dns_cname_record` | `target` | string FQDN |
| `windowsddi_dns_ptr_record` | `target` | string FQDN |
| `windowsddi_dns_mx_record_set` | `mx` | set(object{preference, exchange}) |
| `windowsddi_dns_srv_record_set` | `srv` | set(object{priority, weight, port, target}) |
| `windowsddi_dns_txt_record_set` | `txt` | set(string) |

Hostname values are normalised to lowercase FQDN with trailing dot; input without the dot is accepted.

`recordset` engine:
- Read: `Get-DnsServerResourceRecord -ZoneName -Name -RRType`, decode values, return set + TTL.
- Create: fail if records already exist for the tuple (tell the user to import), then add each value.
- Update: diff; add new values (`Add-DnsServerResourceRecord`) before removing old ones (`Remove-DnsServerResourceRecord -RecordData -Force`) so the name never resolves empty. TTL-only changes use `Set-DnsServerResourceRecord -OldInputObject -NewInputObject`. Not atomic; document it.
- Delete: remove all values of the tuple.
- Keyed mutex per (zone, name, type) inside the provider.
- If an existing record in the tuple is dynamic, Create fails unless `allow_overwrite_dynamic = true`.

Import ID: `<zone>/<name>`, e.g. `example.local/www`, `example.local/@`.

### DNS data sources

- `windowsddi_dns_zone`: lookup by name.
- `windowsddi_dns_zones`: list, filters `reverse` / `ad_integrated`.
- `windowsddi_dns_records`: all records in a zone, optional `name` / `type` filters, generic string values (useful for feeding NetBox).

### DNS non-goals (for now)

DNSSEC, DNS policies, zone scopes, secondary and stub zones, root hints, cache and recursion settings.

## Roadmap (replaces the existing one)

- **M0 Skeleton:** provider (new identity and config above), shared `runner` / `psscript` / `normalize`, lint, goreleaser, registry manifest, CI.
- **M1 DHCP scope:** `windowsddi_dhcp_scope` + data sources, acceptance tests.
- **M2 DHCP reservations and exclusions.**
- **M3 DHCP options + leases data source.**
- **v0.1.0:** DHCP complete, published as `thomaschristory/windowsddi`.
- **M4 DNS zones:** `windowsddi_dns_zone` + data sources, file-backed `tfacc-*.test` zones first, then AD-integrated (`WINDOWSDDI_AD=1`).
- **M5 Record-set engine:** A, AAAA, CNAME.
- **M6 Remaining records:** PTR, MX, SRV, TXT + `windowsddi_dns_records`.
- **M7 Conditional forwarders + WinRM transport** (suite run with `WINDOWSDDI_TRANSPORT=winrm`).
- **v0.2.0:** DNS complete.
- **Later:** DHCP failover and policies, DHCPv6, DNS server forwarders singleton (destroy only removes from state), zone aging/scavenging, secondary/stub zones, non-authoritative record-set mode, WinRM Kerberos, cross-service helpers (reservation with A/PTR).

## Testing additions

Acceptance env vars become `WINDOWSDDI_HOST`, `WINDOWSDDI_USERNAME`, `WINDOWSDDI_PASSWORD`, `WINDOWSDDI_TRANSPORT`, plus optional `WINDOWSDDI_DNS_HOST` / `WINDOWSDDI_DHCP_HOST` when the roles live on different servers. DHCP and DNS acceptance tests skip independently when their host or role is unavailable.

## Open questions

- Name: `windowsddi` is the working choice. If changed, update this file first.
- Non-authoritative record-set mode for names shared with dynamic registrations.
- WinRM Kerberos in pure Go (`gokrb5`) or NTLM/HTTPS only.
