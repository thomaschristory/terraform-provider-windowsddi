resource "windowsddi_dhcp_reservation" "printer" {
  scope_id    = windowsddi_dhcp_scope.users.scope_id
  ip_address  = "10.1.20.5"
  client_id   = "AA:BB:CC:DD:EE:FF" # any common MAC notation
  name        = "printer-01"
  description = "2nd floor printer"
}
