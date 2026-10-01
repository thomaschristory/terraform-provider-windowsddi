data "windowsddi_dhcp_leases" "users" {
  scope_id = "10.1.20.0"
}

output "leased_hosts" {
  value = { for l in data.windowsddi_dhcp_leases.users.leases : l.ip_address => l.host_name }
}
