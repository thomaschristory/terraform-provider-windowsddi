resource "windowsddi_dns_conditional_forwarder" "partner" {
  name              = "partner.example.com"
  master_servers    = ["192.0.2.53", "192.0.2.54"]
  replication_scope = "Forest"
  forwarder_timeout = 3
}
