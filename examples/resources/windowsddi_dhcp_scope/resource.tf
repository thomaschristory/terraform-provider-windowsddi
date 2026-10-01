resource "windowsddi_dhcp_scope" "users" {
  name           = "Users VLAN 120"
  description    = "Office users"
  start_range    = "10.1.20.10"
  end_range      = "10.1.20.250"
  subnet_mask    = "255.255.255.0"
  lease_duration = "8h" # or "0.08:00:00"
}
