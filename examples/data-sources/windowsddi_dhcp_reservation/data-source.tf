# By IP address.
data "windowsddi_dhcp_reservation" "printer" {
  scope_id   = "10.1.20.0"
  ip_address = "10.1.20.5"
}

# By client ID (MAC address), in any common notation.
data "windowsddi_dhcp_reservation" "by_mac" {
  scope_id  = "10.1.20.0"
  client_id = "aa:bb:cc:dd:ee:ff"
}
