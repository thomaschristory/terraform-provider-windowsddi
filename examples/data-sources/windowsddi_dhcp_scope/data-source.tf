data "windowsddi_dhcp_scope" "users" {
  scope_id = "10.1.20.0"
}

output "users_range" {
  value = "${data.windowsddi_dhcp_scope.users.start_range}-${data.windowsddi_dhcp_scope.users.end_range}"
}
