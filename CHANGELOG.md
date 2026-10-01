## 0.2.0 (Unreleased)

FEATURES:

* **New Resource:** `windowsddi_dns_zone` (forward and reverse primary zones, file-backed or AD-integrated)
* **New Resource:** `windowsddi_dns_conditional_forwarder`
* **New Resource:** `windowsddi_dns_a_record_set`, `windowsddi_dns_aaaa_record_set`, `windowsddi_dns_cname_record`, `windowsddi_dns_ptr_record`, `windowsddi_dns_mx_record_set`, `windowsddi_dns_srv_record_set`, `windowsddi_dns_txt_record_set`
* **New Data Source:** `windowsddi_dns_zone`
* **New Data Source:** `windowsddi_dns_zones`
* **New Data Source:** `windowsddi_dns_records`
* Provider: `dns_server` to manage a DNS server through a management host.
* Provider: clear error when the `DhcpServer` or `DnsServer` PowerShell module is missing on the host.

## 0.1.0 (Unreleased)

FEATURES:

* **New Resource:** `windowsddi_dhcp_scope`
* **New Resource:** `windowsddi_dhcp_reservation`
* **New Resource:** `windowsddi_dhcp_exclusion_range`
* **New Resource:** `windowsddi_dhcp_option_value` (server, scope and reservation level, vendor and user classes)
* **New Data Source:** `windowsddi_dhcp_scope`
* **New Data Source:** `windowsddi_dhcp_scopes`
* **New Data Source:** `windowsddi_dhcp_reservation`
* **New Data Source:** `windowsddi_dhcp_leases`
* Provider: SSH transport (password or key, host key verification) and WinRM transport (NTLM, Basic, Kerberos).
* Provider: `dhcp_server` to manage a DHCP server through a management host, `max_concurrency` to limit parallel sessions.
* All resources support import.
