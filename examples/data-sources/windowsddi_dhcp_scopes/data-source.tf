data "windowsddi_dhcp_scopes" "all" {}

output "active_scope_ids" {
  value = [for s in data.windowsddi_dhcp_scopes.all.scopes : s.scope_id if s.state == "Active"]
}
