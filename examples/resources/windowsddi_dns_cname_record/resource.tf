resource "windowsddi_dns_cname_record" "intranet" {
  zone_name = "lab.example.local"
  name      = "intranet"
  target    = "www.lab.example.local"
}
