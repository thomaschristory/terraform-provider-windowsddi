# PTR for 10.1.20.80 in the 20.1.10.in-addr.arpa zone.
resource "windowsddi_dns_ptr_record" "www" {
  zone_name = "20.1.10.in-addr.arpa"
  name      = "80"
  target    = "www.lab.example.local"
}
