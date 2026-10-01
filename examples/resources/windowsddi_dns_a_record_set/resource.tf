# Two A records for www.lab.example.local. Any other A record of that name is removed.
resource "windowsddi_dns_a_record_set" "www" {
  zone_name = "lab.example.local"
  name      = "www"
  addresses = ["10.1.20.80", "10.1.20.81"]
  ttl       = 300
}
