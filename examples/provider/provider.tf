terraform {
  required_providers {
    windowsddi = {
      source = "thomaschristory/windowsddi"
    }
  }
}

variable "password" {
  type      = string
  sensitive = true
}

# Connect over SSH (OpenSSH Server on Windows) directly to the server running the DHCP and DNS roles.
# The host key is checked against ~/.ssh/known_hosts unless ssh_host_key is set.
provider "windowsddi" {
  host      = "srv01.example.local"
  transport = "ssh"
  username  = "EXAMPLE\\svc-terraform"
  password  = var.password
}

# Alternatively, connect with WinRM over HTTPS using Kerberos:
#
# provider "windowsddi" {
#   host           = "srv01.example.local"
#   transport      = "winrm"
#   winrm_auth     = "kerberos"
#   kerberos_realm = "EXAMPLE.LOCAL"
#   username       = "svc-terraform"
#   password       = var.password
# }
