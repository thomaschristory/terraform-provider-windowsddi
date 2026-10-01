# Server level: server/<option_id>
terraform import windowsddi_dhcp_option_value.dns server/6

# Scope level: scope/<scope_id>/<option_id>
terraform import windowsddi_dhcp_option_value.router scope/10.1.20.0/3

# Reservation level: reservation/<reserved_ip>/<option_id>
terraform import windowsddi_dhcp_option_value.printer_hostname reservation/10.1.20.5/12

# With a vendor class and/or user class, append /<vendor_class>/<user_class> (either may be empty).
terraform import windowsddi_dhcp_option_value.vendor "scope/10.1.20.0/43/Vendor A/"
