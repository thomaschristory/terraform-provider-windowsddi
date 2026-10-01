# Every reverse lookup zone on the server.
data "windowsddi_dns_zones" "reverse" {
  reverse = true
}

output "reverse_zones" {
  value = data.windowsddi_dns_zones.reverse.zones[*].name
}
