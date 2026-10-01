# AD-integrated forward zone, replicated to all DNS servers of the domain.
resource "windowsddi_dns_zone" "lab" {
  name              = "lab.example.local"
  replication_scope = "Domain"
}

# Reverse zone for 10.1.20.0/24 (name computed: 20.1.10.in-addr.arpa).
resource "windowsddi_dns_zone" "reverse" {
  network_id        = "10.1.20.0/24"
  replication_scope = "Domain"
}

# File-backed zone with non-secure dynamic updates.
resource "windowsddi_dns_zone" "file" {
  name           = "files.example.test"
  zone_file      = "files.example.test.dns"
  dynamic_update = "NonsecureAndSecure"
}
