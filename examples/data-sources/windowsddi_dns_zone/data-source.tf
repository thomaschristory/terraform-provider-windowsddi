data "windowsddi_dns_zone" "lab" {
  name = "lab.example.local"
}

output "lab_is_ad_integrated" {
  value = data.windowsddi_dns_zone.lab.ad_integrated
}
