# MX records at the zone apex.
resource "windowsddi_dns_mx_record_set" "apex" {
  zone_name = "lab.example.local"
  name      = "@"

  mx = [
    { preference = 10, exchange = "mail1.lab.example.local" },
    { preference = 20, exchange = "mail2.lab.example.local" },
  ]
}
