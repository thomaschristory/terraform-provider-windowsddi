# All A records of a zone, for example to feed a source of truth.
data "windowsddi_dns_records" "a" {
  zone_name = "lab.example.local"
  type      = "A"
}

output "a_records" {
  value = {
    for r in data.windowsddi_dns_records.a.records : r.name => r.value...
  }
}
