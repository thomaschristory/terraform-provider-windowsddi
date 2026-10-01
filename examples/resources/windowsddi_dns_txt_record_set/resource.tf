resource "windowsddi_dns_txt_record_set" "apex" {
  zone_name = "lab.example.local"
  name      = "@"
  txt = [
    "v=spf1 mx -all",
    "google-site-verification=abc123",
  ]
}
