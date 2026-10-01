# Terraform Provider: Windows DDI

Manage Microsoft Windows Server DHCP (IPv4) and DNS with Terraform:

- DHCP: scopes, reservations, exclusion ranges and option values.
- DNS: primary zones (forward and reverse, file-backed or AD-integrated), conditional forwarders and record sets (A, AAAA, CNAME, PTR, MX, SRV, TXT).

The provider runs the `DhcpServer` and `DnsServer` PowerShell cmdlets on the target host over SSH or WinRM; nothing is installed on the server.

Typical use: a source of truth such as NetBox feeds Terraform, which keeps the DHCP and DNS servers in line. Existing objects can be adopted with `terraform import`.

## Requirements

- Windows Server 2016+ with the DHCP Server and/or DNS Server role (or a management host with the RSAT tools, see `dhcp_server` and `dns_server`)
- OpenSSH Server (recommended) or WinRM enabled on the target
- An account in `DHCP Administrators` (DHCP) and/or `DnsAdmins` (DNS)
- Terraform 1.5+

## Usage

```hcl
terraform {
  required_providers {
    windowsddi = {
      source = "thomaschristory/windowsddi"
    }
  }
}

provider "windowsddi" {
  host      = "srv01.example.local"
  transport = "ssh"
  username  = "EXAMPLE\\svc-terraform"
  password  = var.password
}

resource "windowsddi_dhcp_scope" "users" {
  name           = "Users VLAN 120"
  start_range    = "10.1.20.10"
  end_range      = "10.1.20.250"
  subnet_mask    = "255.255.255.0"
  lease_duration = "8h"
}

resource "windowsddi_dhcp_reservation" "printer" {
  scope_id   = windowsddi_dhcp_scope.users.scope_id
  ip_address = "10.1.20.5"
  client_id  = "aa:bb:cc:dd:ee:ff"
  name       = "printer-01"
}

resource "windowsddi_dns_zone" "lab" {
  name              = "lab.example.local"
  replication_scope = "Domain"
}

resource "windowsddi_dns_zone" "reverse" {
  network_id        = "10.1.20.0/24"
  replication_scope = "Domain"
}

resource "windowsddi_dns_a_record_set" "printer" {
  zone_name = windowsddi_dns_zone.lab.name
  name      = "printer-01"
  addresses = [windowsddi_dhcp_reservation.printer.ip_address]
  ttl       = 3600
}

resource "windowsddi_dns_ptr_record" "printer" {
  zone_name = windowsddi_dns_zone.reverse.name
  name      = "5"
  target    = "printer-01.lab.example.local"
}

resource "windowsddi_dns_conditional_forwarder" "partner" {
  name           = "partner.example.com"
  master_servers = ["192.0.2.53", "192.0.2.54"]
}
```

SSH host keys are verified against `~/.ssh/known_hosts` (or a pinned `ssh_host_key`). See the [provider documentation](docs/index.md) for WinRM (NTLM, Basic, Kerberos), environment variables and the management host setup.

### DHCP and DNS on different servers

One provider block talks to one host. When DNS runs on a domain controller and DHCP on another server, or each needs a different account, use two aliased provider blocks:

```hcl
provider "windowsddi" {
  alias    = "dhcp"
  host     = "dhcp01.example.local"
  username = "EXAMPLE\\svc-dhcp"
  password = var.dhcp_password
}

provider "windowsddi" {
  alias    = "dns"
  host     = "dc01.example.local"
  username = "EXAMPLE\\svc-dns"
  password = var.dns_password
}

resource "windowsddi_dhcp_scope" "users" {
  provider = windowsddi.dhcp
  # ...
}

resource "windowsddi_dns_zone" "lab" {
  provider = windowsddi.dns
  # ...
}
```

Alternatively, connect to a management host with the RSAT tools and set `dhcp_server` / `dns_server`, which are passed as `-ComputerName` to the cmdlets of each service.

### AD-integrated zones

AD-integrated zones replicate between domain controllers. Point the provider at one DC and keep it there: a read against another DC can briefly show drift until replication catches up.

### Record sets

Each record-set resource owns every record of its (zone, name, type) and is authoritative: records of that name and type that are not in the configuration are removed. Updates add new values before removing old ones so the name never resolves empty (except a CNAME target change, which removes the old target first), but the sequence is not atomic. Creating a record set over existing static records fails and asks you to import them; existing dynamic (DHCP or client registered) records are only replaced with `allow_overwrite_dynamic = true`.

### Import

```sh
terraform import windowsddi_dhcp_scope.users 10.1.20.0
terraform import windowsddi_dhcp_reservation.printer 10.1.20.0/10.1.20.5
terraform import windowsddi_dhcp_exclusion_range.static 10.1.20.0/10.1.20.10-10.1.20.20
terraform import windowsddi_dhcp_option_value.router scope/10.1.20.0/3
terraform import windowsddi_dns_zone.lab lab.example.local
terraform import windowsddi_dns_conditional_forwarder.partner partner.example.com
terraform import windowsddi_dns_a_record_set.printer lab.example.local/printer-01
terraform import windowsddi_dns_mx_record_set.apex lab.example.local/@
```

### Resources and data sources

| Resources | Data sources |
|---|---|
| `windowsddi_dhcp_scope` | `windowsddi_dhcp_scope`, `windowsddi_dhcp_scopes` |
| `windowsddi_dhcp_reservation` | `windowsddi_dhcp_reservation` |
| `windowsddi_dhcp_exclusion_range` | `windowsddi_dhcp_leases` |
| `windowsddi_dhcp_option_value` (server, scope, reservation level) | |
| `windowsddi_dns_zone` | `windowsddi_dns_zone`, `windowsddi_dns_zones` |
| `windowsddi_dns_conditional_forwarder` | `windowsddi_dns_records` |
| `windowsddi_dns_a_record_set`, `windowsddi_dns_aaaa_record_set` | |
| `windowsddi_dns_cname_record`, `windowsddi_dns_ptr_record` | |
| `windowsddi_dns_mx_record_set`, `windowsddi_dns_srv_record_set`, `windowsddi_dns_txt_record_set` | |

## Development

```sh
go build ./...
go test ./...            # unit tests: no Windows host needed
golangci-lint run
go generate ./...        # regenerates docs/ for the Registry
```

Unit tests drive the provider end to end against in-memory fake DHCP and DNS servers, and run the generated PowerShell under `pwsh` (when installed) against stub cmdlets. Acceptance tests run against a real server:

```sh
export WINDOWSDDI_HOST=lab.example.local WINDOWSDDI_USERNAME='LAB\admin' WINDOWSDDI_PASSWORD=...
export WINDOWSDDI_TRANSPORT=ssh   # or winrm
# optional: WINDOWSDDI_DHCP_HOST / WINDOWSDDI_DNS_HOST when the roles live on different servers
# optional: WINDOWSDDI_AD=1 to also test AD-integrated zones (host must be a domain controller)
TF_ACC=1 go test ./internal/... -run TestAcc -v -timeout 30m
```

They create and destroy DHCP scopes in `198.18.0.0/15` (benchmarking range) and DNS zones named `tfacc-*.test`; use a lab server. DHCP and DNS tests skip independently when their role is unavailable. [docs/LAB_SETUP.md](docs/LAB_SETUP.md) explains how to build a lab.

New to Go or to this codebase? Start with the [code tour](docs/CODE_TOUR.md). See [CLAUDE.md](CLAUDE.md) for conventions, [docs/DESIGN.md](docs/DESIGN.md) for the architecture, [docs/ROADMAP.md](docs/ROADMAP.md) for milestones and [docs/adr](docs/adr) for recorded decisions.

## License

MPL-2.0
