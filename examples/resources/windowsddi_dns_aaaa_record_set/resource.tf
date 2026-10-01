resource "windowsddi_dns_aaaa_record_set" "www" {
  zone_name = "lab.example.local"
  name      = "www"
  addresses = ["2001:db8::80"]
}
