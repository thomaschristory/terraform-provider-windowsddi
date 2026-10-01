# Roadmap

Build in vertical slices: each milestone ends with passing unit and acceptance tests and generated docs.

The project started as `windowsdhcp` and was widened to DHCP plus DNS as `windowsddi` ([ADR 0001](adr/0001-single-ddi-provider.md)). The DHCP milestones were implemented under the old name and carried over.

Status: M0 to M7 are implemented and unit tested (fake servers plus PowerShell script tests under `pwsh`). Acceptance tests are written but have not yet run against a real server; running them is the remaining gate before tagging each release.

## M0: Skeleton

- [x] Provider with the configuration from DESIGN.md (`dhcp_server`, `dns_server`), env var fallback, `Configure()` building one `Runner` shared by the DHCP and DNS clients
- [x] Shared `runner` (SSH, WinRM), `psscript` (envelope, `ErrNotFound`, module check), `normalize`
- [x] `.golangci.yml`, `.goreleaser.yml`, `terraform-registry-manifest.json` (protocol 6)
- [x] GitHub Actions: lint + unit tests on PR; release on tag

## M1: DHCP scope

- [x] `windowsddi_dhcp_scope` resource with import
- [x] `windowsddi_dhcp_scope` / `windowsddi_dhcp_scopes` data sources
- [ ] Acceptance tests against a lab server (written, not yet run)

## M2: DHCP reservations and exclusions

- [x] MAC normalisation helper + tests
- [x] `windowsddi_dhcp_reservation` resource + data source
- [x] `windowsddi_dhcp_exclusion_range` resource

## M3: DHCP options and leases

- [x] `windowsddi_dhcp_option_value` at server, scope and reservation level
- [x] `windowsddi_dhcp_leases` data source

## v0.1.0: DHCP complete

- [x] README usage, Registry docs complete
- [ ] Run the DHCP acceptance suite over SSH and WinRM against a lab server
- [ ] Publish to the Terraform Registry as `thomaschristory/windowsddi`

## M4: DNS zones

- [x] `dns` client, `dnsfake`, DNS stubs for the script tests
- [x] `windowsddi_dns_zone` resource (forward and reverse, file-backed and AD-integrated)
- [x] `windowsddi_dns_zone` / `windowsddi_dns_zones` data sources
- [ ] Acceptance tests: file-backed `tfacc-*.test` zones, then AD-integrated (`WINDOWSDDI_AD=1`) (written, not yet run)

## M5: Record-set engine

- [x] `recordset` engine (authoritative sets, add before remove, keyed mutex, dynamic record guard)
- [x] `windowsddi_dns_a_record_set`, `windowsddi_dns_aaaa_record_set`, `windowsddi_dns_cname_record`

## M6: Remaining records

- [x] `windowsddi_dns_ptr_record`, `windowsddi_dns_mx_record_set`, `windowsddi_dns_srv_record_set`, `windowsddi_dns_txt_record_set`
- [x] `windowsddi_dns_records` data source

## M7: Conditional forwarders and WinRM

- [x] `windowsddi_dns_conditional_forwarder`
- [ ] Full acceptance suite run with `WINDOWSDDI_TRANSPORT=winrm` (not yet run)

## v0.2.0: DNS complete

- [ ] Run the DNS acceptance suite over SSH and WinRM against a lab domain controller
- [ ] Tag and publish

## Later

- DHCP failover relationships and `replicate_after_change`, policies, MAC filters
- DHCPv6 (`windowsddi_dhcp_v6_*`)
- DNS server forwarders singleton (destroy only removes from state)
- Zone aging and scavenging, secondary and stub zones
- Non-authoritative record-set mode for names shared with dynamic registrations
- WinRM Kerberos with keytab or credential cache
- Cross-service helpers (reservation with matching A and PTR records)
