#Requires -RunAsAdministrator
<#
  Prepares a fresh Windows Server (2019/2022/2025, workgroup) for the
  terraform-provider-windowsddi acceptance tests. Automates sections 3 to 6
  of docs/LAB_SETUP.md: DHCP + DNS roles, svc-terraform account, SSH, WinRM.

  Safe to rerun. Run in an elevated Windows PowerShell 5.1 session:
    Set-ExecutionPolicy -Scope Process Bypass -Force; .\lab-setup.ps1
#>
param(
    [string]$UserName = 'svc-terraform',
    [switch]$SkipDns
)
$ErrorActionPreference = 'Stop'

function Step($m) { Write-Host "`n==> $m" -ForegroundColor Cyan }

# --- 3. Roles --------------------------------------------------------------
Step 'DHCP Server role'
Install-WindowsFeature DHCP -IncludeManagementTools | Out-Null
netsh dhcp add securitygroups | Out-Null
Restart-Service DHCPServer
Set-ItemProperty -Path 'HKLM:\SOFTWARE\Microsoft\ServerManager\Roles\12' -Name ConfigurationState -Value 2

if (-not $SkipDns) {
    Step 'DNS Server role'
    Install-WindowsFeature DNS -IncludeManagementTools | Out-Null
}

# --- 4. Test account -------------------------------------------------------
Step "Local account $UserName"
if (-not (Get-LocalUser -Name $UserName -ErrorAction SilentlyContinue)) {
    $pw = Read-Host -AsSecureString "Password for $UserName"
    New-LocalUser -Name $UserName -Password $pw -PasswordNeverExpires -AccountNeverExpires | Out-Null
} else {
    Write-Host "$UserName already exists, keeping its password"
}
$groups = @('Administrators', 'DHCP Administrators')
if (-not $SkipDns) { $groups += 'DnsAdmins' }
foreach ($g in $groups) {
    $member = Get-LocalGroupMember -Group $g -ErrorAction SilentlyContinue |
        Where-Object { $_.Name -like "*\$UserName" }
    if (-not $member) { Add-LocalGroupMember -Group $g -Member $UserName }
}
# Lift Remote UAC filtering so the local admin gets a full token over WinRM.
Set-ItemProperty -Path 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System' `
    -Name LocalAccountTokenFilterPolicy -Value 1 -Type DWord

# --- 5. SSH ----------------------------------------------------------------
Step 'OpenSSH Server'
$cap = Get-WindowsCapability -Online -Name 'OpenSSH.Server*'
if ($cap -and $cap.State -ne 'Installed') { Add-WindowsCapability -Online -Name $cap.Name | Out-Null }
Set-Service -Name sshd -StartupType Automatic
Start-Service sshd
Get-NetFirewallRule -Name OpenSSH-Server-In-TCP -ErrorAction SilentlyContinue | Enable-NetFirewallRule

# --- 6. WinRM --------------------------------------------------------------
Step 'WinRM HTTP (5985) and HTTPS (5986)'
Enable-PSRemoting -Force -SkipNetworkProfileCheck | Out-Null
$https = Get-ChildItem WSMan:\localhost\Listener | Where-Object { $_.Keys -contains 'Transport=HTTPS' }
if (-not $https) {
    $fqdn = [System.Net.Dns]::GetHostEntry('localhost').HostName
    $ips = Get-NetIPAddress -AddressFamily IPv4 | Where-Object { $_.IPAddress -notlike '127.*' -and $_.IPAddress -notlike '169.254.*' }
    $names = @($fqdn, $env:COMPUTERNAME) + @($ips.IPAddress) | Select-Object -Unique
    $cert = New-SelfSignedCertificate -DnsName $names -CertStoreLocation Cert:\LocalMachine\My
    New-Item -Path WSMan:\localhost\Listener -Transport HTTPS -Address * -CertificateThumbprint $cert.Thumbprint -Force | Out-Null
}
if (-not (Get-NetFirewallRule -Name WinRM-HTTPS-In -ErrorAction SilentlyContinue)) {
    New-NetFirewallRule -Name WinRM-HTTPS-In -DisplayName 'WinRM HTTPS' -Protocol TCP -LocalPort 5986 -Action Allow | Out-Null
}

# --- Summary ---------------------------------------------------------------
Step 'Checks'
Get-Service DHCPServer, sshd, WinRM | Format-Table Name, Status -AutoSize
if (-not $SkipDns) { Get-Service DNS | Format-Table Name, Status -AutoSize }
$scopes = @(Get-DhcpServerv4Scope)
Write-Host "DHCP scopes: $($scopes.Count) (expected 0 on a fresh server)"
Get-ChildItem WSMan:\localhost\Listener | ForEach-Object { ($_.Keys -join ', ') }

Write-Host "`nIPv4 addresses:"
Get-NetIPAddress -AddressFamily IPv4 | Where-Object { $_.IPAddress -notlike '127.*' } |
    ForEach-Object { "  $($_.IPAddress)  ($($_.InterfaceAlias))" }

Write-Host "`nSSH host key fingerprints (compare with ssh-keyscan on your Mac):"
Get-ChildItem "$env:ProgramData\ssh\ssh_host_*_key.pub" | ForEach-Object { & "$env:WINDIR\System32\OpenSSH\ssh-keygen.exe" -lf $_.FullName }

Write-Host "`nDone. Group membership is read at logon: if $UserName existed before, log it out and in again." -ForegroundColor Green
Write-Host 'Take a VM snapshot now.' -ForegroundColor Green
