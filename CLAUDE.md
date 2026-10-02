# terraform-provider-windowsddi

Terraform provider for managing Microsoft Windows Server DHCP (IPv4 first) and DNS by running the `DhcpServer` and `DnsServer` PowerShell cmdlets remotely over SSH or WinRM.

- Registry source: `thomaschristory/windowsddi`
- Go module: `github.com/thomaschristory/terraform-provider-windowsddi`
- Resource prefixes: `windowsddi_dhcp_` and `windowsddi_dns_`

Read `docs/DESIGN.md` before writing code and `docs/ROADMAP.md` to know what to build next. If a decision in those files turns out to be wrong, update the doc in the same change; do not silently diverge.

## Decisions

- [ADR 0001: one provider for Windows DNS and DHCP](docs/adr/0001-single-ddi-provider.md)

## Stack

- Go (current stable), `terraform-plugin-framework` only. Do NOT use `terraform-plugin-sdk/v2`.
- `terraform-plugin-testing` for acceptance tests.
- `terraform-plugin-docs` (`tfplugindocs`) to generate `docs/` for the Registry from schema descriptions and `examples/`.
- `goreleaser` for releases (GPG-signed `SHA256SUMS`, as the Registry requires).
- `golangci-lint` for linting.

## Commands

```sh
go build ./...
go test ./...                              # unit tests, no Windows host needed
TF_ACC=1 go test ./internal/... -run TestAcc -v -timeout 30m   # acceptance, needs a real DHCP and/or DNS server
golangci-lint run
go generate ./...                          # regenerates Registry docs via tfplugindocs
```

Acceptance tests read connection details from env vars: `WINDOWSDDI_HOST`, `WINDOWSDDI_USERNAME`, `WINDOWSDDI_PASSWORD`, `WINDOWSDDI_TRANSPORT` (`ssh` or `winrm`), plus optional `WINDOWSDDI_DHCP_HOST` / `WINDOWSDDI_DNS_HOST` when the roles live on different servers, and `WINDOWSDDI_AD=1` to run AD-integrated zone tests. DHCP and DNS tests skip independently when their variables are unset or their role is unavailable.

## Layout

```
main.go
internal/
  provider/          provider config, schema, Configure(), registers both resource families
  providerdata/      *Clients (DHCP + DNS client) handed to resources and data sources
  runner/            Runner interface + ssh and winrm implementations (shared)
  psscript/          script templates, module check, JSON envelope parsing, ErrNotFound (shared)
  normalize/         MAC, IP, duration, FQDN, TTL, TXT helpers and custom types (shared)
  dhcp/              typed DHCP client (GetScope, AddReservation, ...)
  dhcp/dhcpfake/     in-memory fake DHCP server implementing Runner (unit tests)
  dns/               typed DNS client (GetZone, AddRecord, ...)
  dns/dnsfake/       in-memory fake DNS server implementing Runner (unit tests)
  recordset/         generic DNS record-set engine
  resources/dhcp/    DHCP resources + tests (package dhcpresources)
  resources/dns/     DNS resources + tests (package dnsresources)
  datasources/dhcp/  DHCP data sources + tests (package dhcpdatasources)
  datasources/dns/   DNS data sources + tests (package dnsdatasources)
  acctest/           test helpers: provider factories (fake or real server), per-service PreChecks
examples/            HCL examples consumed by tfplugindocs
templates/           tfplugindocs templates (index page)
scripts/             lab-setup.ps1: prepares a Windows Server for the acceptance tests (run on the server, see docs/LAB_SETUP.md)
docs/                DESIGN.md, ROADMAP.md, adr/ (hand-written); generated Registry docs live under docs/resources, docs/data-sources
```

Layering is strict: resources call `dhcp` / `dns` (or `recordset`), which call `psscript` + `runner`. Resources never build PowerShell strings.

## Conventions

- Every resource implements Create, Read, Update, Delete and ImportState. Import is not optional.
- Read must handle "not found" by calling `resp.State.RemoveResource` (drift), not by erroring.
- Attributes that cannot be changed in place use `RequiresReplace` plan modifiers. Check the cmdlet's `Set-*` parameters to decide.
- Every attribute has a `MarkdownDescription`. It feeds the generated docs.
- Normalise user input (MAC, durations, FQDNs, IPv6) in plan modifiers, custom types or Read-side spelling preservation so the plan never shows a perpetual diff.
- Never interpolate user values into PowerShell source. Pass them as a JSON parameters object (see DESIGN.md, "Script contract").
- Never log secrets. Redact password fields in `tflog` output.
- Errors surfaced to the user include the cmdlet name and the PowerShell error message.
- Unit tests mock the `Runner` interface with recorded JSON fixtures in `testdata/`. Resource tests use `dhcpfake` / `dnsfake` through `resource.UnitTest`; PowerShell script tests run under a local `pwsh` against `internal/dhcp/testdata/stubs.ps1` and `internal/dns/testdata/stubs.ps1` (skipped when `pwsh` is missing). Keep the stubs' parameters in line with the documented cmdlet signatures.

## Do not

- Do not add DHCPv6, DHCP failover or policies, superscopes, DNSSEC, DNS policies, zone scopes or secondary/stub zones before the milestones in ROADMAP.md that schedule them.
- Do not shell out to a local `pwsh` binary in provider code; all execution goes through the `Runner`. (The script tests in `internal/dhcp/pwsh_test.go` and `internal/dns/pwsh_test.go` are the only exception.)
- Do not use em dashes in docs or descriptions; use colons, commas or parentheses.
