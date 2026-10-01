# Scope level: default gateway.
resource "windowsddi_dhcp_option_value" "router" {
  scope_id  = windowsddi_dhcp_scope.users.scope_id
  option_id = 3
  value     = ["10.1.20.1"]
}

# Server level: DNS servers (order matters) and domain name.
resource "windowsddi_dhcp_option_value" "dns" {
  option_id = 6
  value     = ["10.0.0.53", "10.0.1.53"]
}

resource "windowsddi_dhcp_option_value" "domain" {
  option_id = 15
  value     = ["example.local"]
}

# Reservation level: host name for one client.
resource "windowsddi_dhcp_option_value" "printer_hostname" {
  reserved_ip = windowsddi_dhcp_reservation.printer.ip_address
  option_id   = 12
  value       = ["printer-01"]
}
