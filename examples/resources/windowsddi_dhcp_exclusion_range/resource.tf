# Keep the first addresses of the scope for static assignments.
resource "windowsddi_dhcp_exclusion_range" "static" {
  scope_id    = windowsddi_dhcp_scope.users.scope_id
  start_range = "10.1.20.10"
  end_range   = "10.1.20.20"
}
