resource "windowsddi_dns_srv_record_set" "sip" {
  zone_name = "lab.example.local"
  name      = "_sip._tcp"

  srv = [
    { priority = 10, weight = 60, port = 5060, target = "sip1.lab.example.local" },
    { priority = 10, weight = 40, port = 5060, target = "sip2.lab.example.local" },
  ]
}
