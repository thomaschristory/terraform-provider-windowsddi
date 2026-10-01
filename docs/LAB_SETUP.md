# Lab setup for acceptance tests

How to build a throwaway Windows DHCP and DNS server and run the provider's acceptance tests (`TestAcc*`) against it, over SSH and WinRM.

The tests only use the management APIs:

- DHCP: they create, change and delete scopes, reservations, exclusion ranges and option values in `198.18.0.0/15` (a range reserved for benchmarking).
- DNS: they create, change and delete file-backed zones named `tfacc-*.test`, their records and conditional forwarders. AD-integrated zones are only tested on a domain controller with `WINDOWSDDI_AD=1` (section 11).

One server can carry both roles. DHCP and DNS tests skip independently when their role is missing, and `WINDOWSDDI_DHCP_HOST` / `WINDOWSDDI_DNS_HOST` point each family at a different server if you prefer two VMs. The server never needs to hand out a lease or answer a real query, so it can sit on any network, including a cloud VM, as long as your machine can reach it on port 22 (SSH) and 5985/5986 (WinRM).

Budget about an hour, most of it Windows installing.

1. [Pick where the VM runs](#1-pick-where-the-vm-runs)
2. [Install Windows Server](#2-install-windows-server)
3. [Install and prepare the DHCP and DNS roles](#3-install-and-prepare-the-dhcp-and-dns-roles)
4. [Create the test account](#4-create-the-test-account)
5. [Enable SSH](#5-enable-ssh)
6. [Enable WinRM](#6-enable-winrm)
7. [Check from your machine](#7-check-from-your-machine)
8. [Run the acceptance tests](#8-run-the-acceptance-tests)
9. [Clean up](#9-clean-up)
10. [Troubleshooting](#10-troubleshooting)
11. [Optional: domain controller (Kerberos, AD-integrated zones)](#11-optional-domain-controller-kerberos-ad-integrated-zones)

---

## 1. Pick where the VM runs

Windows Server only ships for x64 (there is no generally available ARM64 build), so on an Apple Silicon Mac you cannot run it at usable speed locally.

| Option | Good for | Notes |
|---|---|---|
| **An x64 hypervisor you already have** (Proxmox, Hyper-V, ESXi, a spare PC) | Best overall | Free with the evaluation ISO. |
| **A cloud VM** (Azure, AWS, ...) | Apple Silicon Mac with no x64 box | Use a "Windows Server 2022 Datacenter" or "2025 Datacenter" image, 2 vCPU / 4 GB (e.g. Azure `B2s`). Restrict the inbound rules for 22 and 5986 to your own public IP. Stop the VM when done to avoid charges. Skip step 2. |
| UTM on Apple Silicon (x64 emulation) | Last resort | Works, but very slow. |

Sizing: 2 vCPU, 4 GB RAM, 40 GB disk. Give it a fixed IP or DNS name.

## 2. Install Windows Server

1. Download an evaluation ISO (180 days, free) from the Microsoft Evaluation Center: <https://www.microsoft.com/evalcenter>. Windows Server 2022 or 2025 both work; 2016 and 2019 should too.
2. Install. Either edition works. "Desktop Experience" is easier if you are not used to Server Core.
3. Set the local Administrator password, log in, and open **PowerShell as Administrator**. Every command below runs there unless stated otherwise.
4. Optional but handy:

   ```powershell
   Rename-Computer -NewName dhcp-lab -Restart
   ```

> Take a VM snapshot here. If anything goes wrong later, roll back instead of debugging.

## 3. Install and prepare the DHCP and DNS roles

```powershell
# Install the DHCP Server role and its PowerShell module (DhcpServer).
Install-WindowsFeature DHCP -IncludeManagementTools

# Create the "DHCP Administrators" and "DHCP Users" local groups, then
# restart the service so it picks them up.
netsh dhcp add securitygroups
Restart-Service DHCPServer

# Tell Server Manager the post-install configuration is done (removes the
# yellow warning flag; it has no effect on the service).
Set-ItemProperty -Path 'HKLM:\SOFTWARE\Microsoft\ServerManager\Roles\12' -Name ConfigurationState -Value 2

# Sanity check: the service runs and there are no scopes yet.
Get-Service DHCPServer
Get-DhcpServerv4Scope
```

The last command should print nothing (no scopes) and no error.

Then the DNS role (skip it if you only test DHCP):

```powershell
# Install the DNS Server role and its PowerShell module (DnsServer). This also
# creates the local "DnsAdmins" group.
Install-WindowsFeature DNS -IncludeManagementTools

# Sanity check: the service runs; only the built-in zones are listed.
Get-Service DNS
Get-DnsServerZone
```

You do **not** need to authorize the server in Active Directory: a workgroup (non-domain) DHCP server is enough for the tests.

## 4. Create the test account

For a lab, the simplest account that always works is a local administrator that is also in **DHCP Administrators** and **DnsAdmins**:

```powershell
$pw = Read-Host -AsSecureString 'Password for svc-terraform'
New-LocalUser -Name svc-terraform -Password $pw -PasswordNeverExpires -AccountNeverExpires
Add-LocalGroupMember -Group 'Administrators'       -Member svc-terraform
Add-LocalGroupMember -Group 'DHCP Administrators'  -Member svc-terraform
Add-LocalGroupMember -Group 'DnsAdmins'            -Member svc-terraform   # if the DNS role is installed
```

Local administrators other than the built-in `Administrator` get a filtered (non-admin) token over WinRM because of Remote UAC. Lift that for the lab:

```powershell
Set-ItemProperty -Path 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System' `
    -Name LocalAccountTokenFilterPolicy -Value 1 -Type DWord
```

(You can also just use the built-in `Administrator` account and skip this.)

> Least privilege: in production the README asks only for `DHCP Administrators` and `DnsAdmins`. Once everything works in the lab, you can try removing `svc-terraform` from `Administrators` and rerunning the tests to see whether your setup allows it.

## 5. Enable SSH

Windows Server 2025 ships OpenSSH Server already installed; on 2019 and 2022 add it first.

```powershell
# 2019/2022 only (harmless on 2025):
Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0

Set-Service -Name sshd -StartupType Automatic
Start-Service sshd

# The installer creates this firewall rule; make sure it is enabled.
Get-NetFirewallRule -Name OpenSSH-Server-In-TCP | Enable-NetFirewallRule
```

You do not need to change the default SSH shell: the provider always launches `powershell.exe` itself.

## 6. Enable WinRM

Needed only for the WinRM test run. Two listeners: HTTP 5985 (NTLM with message encryption) and HTTPS 5986 (the provider's default).

```powershell
# HTTP listener on 5985 plus its firewall rule.
Enable-PSRemoting -Force

# HTTPS listener on 5986 with a self-signed certificate.
$name = [System.Net.Dns]::GetHostEntry('localhost').HostName   # or the name/IP you connect with
$cert = New-SelfSignedCertificate -DnsName $name, $env:COMPUTERNAME -CertStoreLocation Cert:\LocalMachine\My
New-Item -Path WSMan:\localhost\Listener -Transport HTTPS -Address * -CertificateThumbprint $cert.Thumbprint -Force
New-NetFirewallRule -Name WinRM-HTTPS-In -DisplayName 'WinRM HTTPS' -Protocol TCP -LocalPort 5986 -Action Allow

# Check both listeners exist.
Get-ChildItem WSMan:\localhost\Listener
```

The certificate is self-signed, so the provider must skip TLS verification in the lab (`WINDOWSDDI_INSECURE=true`). Do not do that in production; use a certificate from your CA instead.

On a cloud VM, also open 5986 (and 5985 if you want to test plain HTTP) in the cloud firewall / security group, restricted to your IP.

## 7. Check from your machine

Replace `dhcp-lab.example.local` with the VM's name or IP.

**SSH host key.** The provider verifies it, so record it first:

```sh
ssh-keyscan -H dhcp-lab.example.local >> ~/.ssh/known_hosts
```

**SSH works and the DhcpServer module answers:**

```sh
ssh 'svc-terraform@dhcp-lab.example.local' 'powershell -NoProfile -Command "Get-Service DHCPServer; Get-DhcpServerv4Scope"'
```

You should see the service `Running` and no error.

**WinRM port is reachable:**

```sh
nc -vz dhcp-lab.example.local 5986
```

**The provider itself** (optional, using a local build): see "Try a local build with real Terraform" in [CODE_TOUR.md](CODE_TOUR.md#9-common-tasks), then run a `terraform plan` with a `data "windowsddi_dhcp_scopes" "all" {}` block.

> Snapshot the VM again now. Reverting to this snapshot is the fastest cleanup after a failed run.

## 8. Run the acceptance tests

From the repository root on your machine:

```sh
export WINDOWSDDI_HOST=dhcp-lab.example.local
export WINDOWSDDI_USERNAME=svc-terraform     # a local account; a domain one would be 'EXAMPLE\svc-terraform'
export WINDOWSDDI_PASSWORD='...'

# 1. SSH
WINDOWSDDI_TRANSPORT=ssh TF_ACC=1 go test ./internal/... -run TestAcc -v -timeout 30m

# 2. WinRM over HTTPS (NTLM), self-signed certificate
WINDOWSDDI_TRANSPORT=winrm WINDOWSDDI_INSECURE=true \
  TF_ACC=1 go test ./internal/... -run TestAcc -v -timeout 30m

# 3. Optional: WinRM over HTTP (NTLM with message encryption)
WINDOWSDDI_TRANSPORT=winrm WINDOWSDDI_WINRM_HTTPS=false \
  TF_ACC=1 go test ./internal/... -run TestAcc -v -timeout 30m
```

Tips:

- Run one test at a time while debugging: `-run TestAccScopeResource`.
- Roles on two servers: set `WINDOWSDDI_DHCP_HOST` and `WINDOWSDDI_DNS_HOST` (the other variables are shared).
- See every PowerShell call the provider makes: add `TF_LOG=DEBUG` (look for `running DHCP script`, `running DNS script` and `... script failed`). Passwords are never logged.
- If a test fails, copy the full output (including the `Error:` block with the cmdlet name) into an issue or a chat; that is everything needed to fix it.

What a pass proves that the unit tests could not: the real cmdlets accept the parameters, return the property names the scripts read, report "not found" with the expected error codes, and the stdin bootstrap works over each transport.

## 9. Clean up

Tests destroy what they create. After an interrupted run, remove leftovers on the server:

```powershell
# Removing a scope also removes its reservations, exclusion ranges and options.
Get-DhcpServerv4Scope | Where-Object { "$($_.ScopeId)" -like '198.18.*' } |
    Remove-DhcpServerv4Scope -Force
```

```powershell
# DNS: test zones and conditional forwarders are all named tfacc-*.test.
Get-DnsServerZone | Where-Object ZoneName -like 'tfacc-*' | Remove-DnsServerZone -Force
```

The acceptance tests only touch scopes in `198.18.0.0/15` and objects inside them, and zones named `tfacc-*.test`; they never change server-level settings.

Or revert to the snapshot from step 7.

When you are done for good: delete the VM (or stop it, for a cloud VM). The evaluation licence expires after 180 days anyway.

## 10. Troubleshooting

| Symptom | Likely cause and fix |
|---|---|
| `ssh: ... host ... is not in .../known_hosts` | Run the `ssh-keyscan` line from step 7, or pin the key with `WINDOWSDDI_SSH_HOST_KEY`. |
| `ssh: handshake failed: ssh: unable to authenticate` | Wrong user or password. Local accounts: plain `svc-terraform`. Test with the `ssh` command from step 7. |
| `The DhcpServer PowerShell module is not available on this host` | The DHCP management tools are missing: rerun `Install-WindowsFeature DHCP -IncludeManagementTools`. DHCP tests skip in that case. |
| `The DnsServer PowerShell module is not available on this host` | The DNS role or tools are missing: `Install-WindowsFeature DNS -IncludeManagementTools`. DNS tests skip in that case. |
| `...: Access is denied. (PermissionDenied)` | Account not in `DHCP Administrators` (or `DnsAdmins` for DNS), or added after the service started. Rerun `netsh dhcp add securitygroups`, `Restart-Service DHCPServer`, and log the account out and in again (group membership is read at logon). |
| `winrm: ... http response error: 401` | Wrong credentials, or Remote UAC filtering: set `LocalAccountTokenFilterPolicy` (step 4) or use the built-in `Administrator`. |
| `winrm: ... x509: certificate ...` | Self-signed certificate: set `WINDOWSDDI_INSECURE=true` for the lab. |
| `winrm: ... connection refused` / timeout | Listener or firewall: check `Get-ChildItem WSMan:\localhost\Listener` and the cloud security group. |
| A test fails with an unexpected cmdlet error | That is a real finding. Rerun the single test with `TF_LOG=DEBUG` and keep the output. |

## 11. Optional: domain controller (Kerberos, AD-integrated zones)

`winrm_auth = "kerberos"` and AD-integrated DNS zones need an Active Directory domain, so a workgroup lab cannot test them. To cover them, promote the lab server to a domain controller of a throwaway domain (DHCP can run on a DC in a lab):

```powershell
Install-WindowsFeature AD-Domain-Services -IncludeManagementTools
Install-ADDSForest -DomainName lab.example.local -InstallDns -Force   # asks for a DSRM password, then reboots
```

After the reboot:

```powershell
# A domain controller has no local groups: recreate the DHCP groups in the
# domain and restart the service.
netsh dhcp add securitygroups
Restart-Service DHCPServer
# Authorize the DHCP server in AD (required for DHCP in a domain).
Add-DhcpServerInDC -DnsName "$env:COMPUTERNAME.lab.example.local"
# Domain test account. Domain Admins can log on to the DC over WinRM; DHCP
# Administrators grants the DHCP rights explicitly.
New-ADUser -Name svc-terraform -AccountPassword (Read-Host -AsSecureString) -Enabled $true -PasswordNeverExpires $true
Add-ADGroupMember -Identity 'Domain Admins' -Members svc-terraform
Add-ADGroupMember -Identity 'DHCP Administrators' -Members svc-terraform
Add-ADGroupMember -Identity 'DnsAdmins' -Members svc-terraform
```

On your machine, `/etc/krb5.conf` (or a file passed with `WINDOWSDDI_KERBEROS_CONFIG`):

```ini
[libdefaults]
  default_realm = LAB.EXAMPLE.LOCAL
  dns_lookup_kdc = false

[realms]
  LAB.EXAMPLE.LOCAL = {
    kdc = dhcp-lab.lab.example.local
  }
```

Your machine must resolve the server's FQDN (add it to `/etc/hosts` if needed), and its clock must be within 5 minutes of the server's. Then:

```sh
WINDOWSDDI_HOST=dhcp-lab.lab.example.local WINDOWSDDI_USERNAME=svc-terraform \
WINDOWSDDI_TRANSPORT=winrm WINDOWSDDI_WINRM_AUTH=kerberos WINDOWSDDI_KERBEROS_REALM=LAB.EXAMPLE.LOCAL \
WINDOWSDDI_INSECURE=true TF_ACC=1 go test ./internal/... -run TestAcc -v -timeout 30m
```

To also run the AD-integrated zone tests, add `WINDOWSDDI_AD=1` to any of the commands above (over SSH or WinRM). They create zones replicated to the domain and remove them afterwards.
