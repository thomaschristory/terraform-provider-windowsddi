# Stub DnsServer cmdlets for script tests run under pwsh. Parameter names,
# types and parameter sets follow the documented cmdlet signatures (the ones
# the scripts use). Like the real CDXML cmdlets, failures are written as
# non-terminating errors with FullyQualifiedErrorId "WIN32 <code>,<cmdlet>",
# so the scripts only stop on them because of the -ErrorAction default they
# set.
#
# Used by internal/dns/pwsh_test.go, which loads this file as an in-memory
# module (New-Module ... | Import-Module) before each script. Every stub:
#   1. records its call (cmdlet name + bound parameters) via Record, and
#   2. reads or updates the in-memory state below.
# State lives in globals and is lost when the pwsh process exits; tests
# rebuild it per run through a "seed" script. Seeds set $global:WdQuiet so
# their own cmdlet calls are not recorded.
#
# Objects mimic the CIM shapes: zones have ZoneName/ZoneType/IsDsIntegrated/
# ..., records have HostName, RecordType, TimeToLive ([TimeSpan]), Timestamp
# ($null for static records) and a RecordData object whose properties have
# the real types (IPv4Address as [ipaddress], Preference as [uint16], ...).

$global:Zones = [ordered]@{}   # keyed by lowercase zone name
$global:Records = [System.Collections.ArrayList]::new()
$global:NextId = 0
$global:WdQuiet = $false

$commonParams = 'ErrorAction', 'WarningAction', 'InformationAction', 'Verbose', 'Debug', 'ErrorVariable',
    'WarningVariable', 'InformationVariable', 'OutVariable', 'OutBuffer', 'PipelineVariable', 'ProgressAction'

# Record appends one JSON line { cmd = <name>; <param> = <value>; ... } to the
# file named by $env:WD_CALLS. Values are normalised for Go: TimeSpan -> total
# seconds, switch -> bool, IP -> string, array -> array of strings, record
# object -> "<HostName>/<RecordType>/<Id>".
function Record($name, $bound) {
    if ($global:WdQuiet) { return }
    $h = [ordered]@{ cmd = $name }
    foreach ($k in $bound.Keys) {
        if ($commonParams -contains $k) { continue }
        $v = $bound[$k]
        if ($v -is [TimeSpan]) { $v = $v.TotalSeconds }
        elseif ($v -is [System.Management.Automation.SwitchParameter]) { $v = $v.IsPresent }
        elseif ($v -is [ipaddress]) { $v = $v.ToString() }
        elseif ($v -is [array]) { $v = @($v | ForEach-Object { "$_" }) }
        elseif ($v -isnot [string] -and $v.PSObject.Properties['RecordType']) { $v = "$($v.HostName)/$($v.RecordType)/$($v.Id)" }
        $h[$k] = $v
    }
    Add-Content -LiteralPath $env:WD_CALLS -Value (ConvertTo-Json -InputObject $h -Compress -Depth 4) -Encoding utf8
}

# Write-DnsError emits a non-terminating error shaped like the real cmdlets'
# ones: id "WIN32 <code>" (PowerShell appends ",<cmdlet>") and a category.
function Write-DnsError($cmdlet, $code, $category, $message) {
    $er = [System.Management.Automation.ErrorRecord]::new([Exception]::new($message), "WIN32 $code", $category, $null)
    $cmdlet.WriteError($er)
}

# Get-StubZone returns the zone object or $null.
function Get-StubZone($name) { $global:Zones["$name".TrimEnd('.').ToLowerInvariant()] }

# New-StubRecord stores a record and returns it. $data is a hashtable of
# RecordData properties. Each record gets an Id (to find it again after
# cloning) and a Clone() script method standing in for CimInstance.Clone().
function New-StubRecord($zone, $name, $type, [TimeSpan]$ttl, $data, $timestamp = $null) {
    $global:NextId++
    $r = [pscustomobject]@{
        Id = $global:NextId; ZoneName = $zone.ToLowerInvariant(); HostName = $name; RecordType = $type
        RecordClass = 'IN'; TimeToLive = $ttl; Timestamp = $timestamp; RecordData = [pscustomobject]$data
    }
    $r | Add-Member -MemberType ScriptMethod -Name Clone -Value { $this.PSObject.Copy() }
    [void]$global:Records.Add($r)
    $r
}

# New-StubZone stores a zone. Primary zones get apex SOA and NS records, as
# on a real server.
function New-StubZone($p) {
    $z = [pscustomobject]@{
        ZoneName = $p.ZoneName; ZoneType = $p.ZoneType; IsAutoCreated = $false
        IsDsIntegrated = [bool]$p.ReplicationScope; IsReverseLookupZone = $p.ZoneName -match '\.(in-addr|ip6)\.arpa$'
        IsSigned = $false; ReplicationScope = if ($p.ReplicationScope) { $p.ReplicationScope } else { 'None' }
        ZoneFile = $p.ZoneFile; DynamicUpdate = $p.DynamicUpdate
        MasterServers = $p.MasterServers; ForwarderTimeout = $p.ForwarderTimeout
    }
    $global:Zones[$p.ZoneName.ToLowerInvariant()] = $z
    if ($p.ZoneType -eq 'Primary') {
        $null = New-StubRecord $p.ZoneName '@' 'SOA' '01:00:00' @{ PrimaryServer = 'stub.'; ResponsiblePerson = 'hostmaster.'
            SerialNumber = [uint32]1; RefreshInterval = [TimeSpan]'00:15:00'; RetryDelay = [TimeSpan]'00:10:00'
            ExpireLimit = [TimeSpan]'1.00:00:00'; MinimumTimeToLive = [TimeSpan]'01:00:00' }
        $null = New-StubRecord $p.ZoneName '@' 'NS' '01:00:00' @{ NameServer = 'stub.' }
    }
    $z
}

# ConvertTo-ReverseName derives the reverse zone name from -NetworkId like
# the server does (IPv4 octets, IPv6 nibbles).
function ConvertTo-ReverseName($cidr) {
    $ip, $bits = $cidr -split '/'
    $b = ([ipaddress]$ip).GetAddressBytes()
    if ($b.Count -eq 4) {
        $o = @($b[0..([int]$bits / 8 - 1)]); [array]::Reverse($o)
        return ($o -join '.') + '.in-addr.arpa'
    }
    $n = @(foreach ($x in $b) { '{0:x}' -f ($x -shr 4); '{0:x}' -f ($x -band 15) })[0..([int]$bits / 4 - 1)]
    [array]::Reverse($n)
    ($n -join '.') + '.ip6.arpa'
}

# ---- Zones ----

# No -Name: list all. Unknown zone: WIN32 9601 ObjectNotFound.
function Get-DnsServerZone {
    [CmdletBinding()]
    param([Parameter(Position = 0)][string[]]$Name, [string]$ComputerName, [string]$VirtualizationInstance)
    Record 'Get-DnsServerZone' $PSBoundParameters
    if (-not $Name) { return $global:Zones.Values }
    foreach ($n in $Name) {
        $z = Get-StubZone $n
        if ($z) { $z } else { Write-DnsError $PSCmdlet 9601 ObjectNotFound "The zone $n was not found on server STUB." }
    }
}

# Four parameter sets, as documented: forward/reverse x AD/file.
function Add-DnsServerPrimaryZone {
    [CmdletBinding(SupportsShouldProcess, DefaultParameterSetName = 'ADForwardLookupZone')]
    param(
        [Parameter(Mandatory, Position = 0, ParameterSetName = 'ADForwardLookupZone')]
        [Parameter(Mandatory, Position = 0, ParameterSetName = 'FileForwardLookupZone')][string]$Name,
        [Parameter(Mandatory, ParameterSetName = 'ADReverseLookupZone')]
        [Parameter(Mandatory, ParameterSetName = 'FileReverseLookupZone')][string]$NetworkId,
        [Parameter(Mandatory, ParameterSetName = 'ADForwardLookupZone')]
        [Parameter(Mandatory, ParameterSetName = 'ADReverseLookupZone')]
        [ValidateSet('Forest', 'Domain', 'Legacy', 'Custom')][string]$ReplicationScope,
        [Parameter(Mandatory, ParameterSetName = 'FileForwardLookupZone')]
        [Parameter(Mandatory, ParameterSetName = 'FileReverseLookupZone')][string]$ZoneFile,
        [ValidateSet('None', 'Secure', 'NonsecureAndSecure')][string]$DynamicUpdate,
        [string]$ResponsiblePerson, [switch]$LoadExisting, [string]$ComputerName, [switch]$PassThru)
    Record 'Add-DnsServerPrimaryZone' $PSBoundParameters
    if ($NetworkId) { $Name = ConvertTo-ReverseName $NetworkId }
    if (Get-StubZone $Name) {
        Write-DnsError $PSCmdlet 9609 ResourceExists "Failed to create zone $Name on server STUB. The zone already exists."
        return
    }
    if (-not $DynamicUpdate) { $DynamicUpdate = if ($ReplicationScope) { 'Secure' } else { 'None' } }
    if ($DynamicUpdate -eq 'Secure' -and -not $ReplicationScope) {
        Write-DnsError $PSCmdlet 9611 InvalidArgument "Secure updates are only supported for Active Directory-integrated zones."
        return
    }
    $z = New-StubZone @{ ZoneName = $Name; ZoneType = 'Primary'; ReplicationScope = $ReplicationScope
        ZoneFile = $ZoneFile; DynamicUpdate = $DynamicUpdate }
    if ($PassThru) { $z }
}

# ReplicationScope (ADZone set) and DynamicUpdate (default set) cannot be
# combined in one call, as on the real cmdlet.
function Set-DnsServerPrimaryZone {
    [CmdletBinding(SupportsShouldProcess, DefaultParameterSetName = 'Parameters')]
    param([Parameter(Mandatory, Position = 0)][string]$Name, [string]$ComputerName, [switch]$PassThru,
        [Parameter(ParameterSetName = 'Parameters')][ValidateSet('None', 'Secure', 'NonsecureAndSecure')][string]$DynamicUpdate,
        [Parameter(Mandatory, ParameterSetName = 'ADZone')][ValidateSet('Forest', 'Domain', 'Legacy', 'Custom')][string]$ReplicationScope,
        [Parameter(ParameterSetName = 'ADZone')][string]$DirectoryPartitionName)
    Record 'Set-DnsServerPrimaryZone' $PSBoundParameters
    $z = Get-StubZone $Name
    if (-not $z) { Write-DnsError $PSCmdlet 9601 ObjectNotFound "The zone $Name was not found on server STUB."; return }
    if ($ReplicationScope) { $z.ReplicationScope = $ReplicationScope; $z.IsDsIntegrated = $true; $z.ZoneFile = $null }
    if ($DynamicUpdate) { $z.DynamicUpdate = $DynamicUpdate }
}

function Remove-DnsServerZone {
    [CmdletBinding(SupportsShouldProcess)]
    param([Parameter(Mandatory, Position = 0)][string]$Name, [string]$ComputerName, [switch]$PassThru, [switch]$Force)
    Record 'Remove-DnsServerZone' $PSBoundParameters
    $key = $Name.ToLowerInvariant()
    if (-not $global:Zones.Contains($key)) { Write-DnsError $PSCmdlet 9601 ObjectNotFound "The zone $Name was not found on server STUB."; return }
    $global:Zones.Remove($key)
    @($global:Records | Where-Object { $_.ZoneName -eq $key }) | ForEach-Object { $global:Records.Remove($_) }
}

# ---- Conditional forwarders ----

function Add-DnsServerConditionalForwarderZone {
    [CmdletBinding(SupportsShouldProcess, DefaultParameterSetName = 'FileForwardLookupZone')]
    param([Parameter(Mandatory, Position = 0)][ipaddress[]]$MasterServers, [Parameter(Position = 1)][uint32]$ForwarderTimeout,
        [Parameter(Mandatory, Position = 2)][string]$Name, [switch]$LoadExisting, [string]$ComputerName,
        [switch]$UseRecursion, [switch]$PassThru,
        [Parameter(Mandatory, ParameterSetName = 'ADForwardLookupZone')][ValidateSet('Forest', 'Domain', 'Legacy', 'Custom')][string]$ReplicationScope,
        [Parameter(ParameterSetName = 'ADForwardLookupZone')][string]$DirectoryPartitionName)
    Record 'Add-DnsServerConditionalForwarderZone' $PSBoundParameters
    if (Get-StubZone $Name) { Write-DnsError $PSCmdlet 9609 ResourceExists "Failed to create zone $Name on server STUB."; return }
    if (-not $PSBoundParameters.ContainsKey('ForwarderTimeout')) { $ForwarderTimeout = 5 }
    $z = New-StubZone @{ ZoneName = $Name; ZoneType = 'Forwarder'; ReplicationScope = $ReplicationScope
        MasterServers = $MasterServers; ForwarderTimeout = $ForwarderTimeout }
    if ($PassThru) { $z }
}

function Set-DnsServerConditionalForwarderZone {
    [CmdletBinding(SupportsShouldProcess, DefaultParameterSetName = 'Parameters')]
    param([Parameter(Mandatory, Position = 0)][string]$Name, [string]$ComputerName, [switch]$PassThru,
        [Parameter(Position = 1, ParameterSetName = 'Parameters')][bool]$UseRecursion,
        [Parameter(Position = 2, ParameterSetName = 'Parameters')][ipaddress[]]$MasterServers,
        [Parameter(Position = 3, ParameterSetName = 'Parameters')][uint32]$ForwarderTimeout,
        [Parameter(Mandatory, ParameterSetName = 'ADZone')][ValidateSet('Forest', 'Domain', 'Legacy', 'Custom')][string]$ReplicationScope,
        [Parameter(ParameterSetName = 'ADZone')][string]$DirectoryPartitionName)
    Record 'Set-DnsServerConditionalForwarderZone' $PSBoundParameters
    $z = Get-StubZone $Name
    if (-not $z) { Write-DnsError $PSCmdlet 9601 ObjectNotFound "The zone $Name was not found on server STUB."; return }
    if ($MasterServers) { $z.MasterServers = $MasterServers }
    if ($PSBoundParameters.ContainsKey('ForwarderTimeout')) { $z.ForwarderTimeout = $ForwarderTimeout }
    if ($ReplicationScope) { $z.ReplicationScope = $ReplicationScope; $z.IsDsIntegrated = $true }
}

# ---- Records ----

# Without -Name: the apex and every child (the whole zone). With -Name and
# -Node: that name only. With -Name without -Node: the name and its children.
# Unknown zone: WIN32 9601. Name without any record: WIN32 9714.
function Get-DnsServerResourceRecord {
    [CmdletBinding(DefaultParameterSetName = 'Name')]
    param([Parameter(Position = 0)][string]$Name, [Parameter(Mandatory, Position = 1)][string]$ZoneName,
        [string]$ComputerName, [switch]$Node, [string]$ZoneScope, [string]$VirtualizationInstance,
        [ValidateSet('HInfo', 'Afsdb', 'Atma', 'Isdn', 'Key', 'Mb', 'Md', 'Mf', 'Mg', 'MInfo', 'Mr', 'Mx', 'NsNxt', 'Rp',
            'Rt', 'Wks', 'X25', 'A', 'AAAA', 'CName', 'Ptr', 'Srv', 'Txt', 'Wins', 'WinsR', 'Ns', 'Soa', 'NasP', 'NasPtr',
            'DName', 'Gpos', 'Loc', 'DhcId', 'Naptr', 'RRSig', 'DnsKey', 'DS', 'NSec', 'NSec3', 'NSec3Param')][string]$RRType)
    Record 'Get-DnsServerResourceRecord' $PSBoundParameters
    $zone = Get-StubZone $ZoneName
    if (-not $zone) { Write-DnsError $PSCmdlet 9601 ObjectNotFound "The zone $ZoneName was not found on server STUB."; return }
    $key = $zone.ZoneName.ToLowerInvariant()
    $all = @($global:Records | Where-Object { $_.ZoneName -eq $key })
    if ($Name -and $Name -ne '@') {
        $n = $Name.ToLowerInvariant()
        $all = @($all | Where-Object { $_.HostName -eq $n -or (-not $Node -and $_.HostName.ToLowerInvariant().EndsWith(".$n")) })
        if ($all.Count -eq 0) { Write-DnsError $PSCmdlet 9714 ObjectNotFound "Failed to get $Name record in $ZoneName zone on server STUB."; return }
    } elseif ($Node) {
        $all = @($all | Where-Object { $_.HostName -eq '@' })
    }
    if ($RRType) { $all = @($all | Where-Object { $_.RecordType -eq $RRType }) }
    $all
}

# Add-StubRecordChecked applies the server rules shared by every Add cmdlet:
# zone must exist (9601), no identical record (9711), CNAME cannot coexist
# with other data (9709 / 9708). Then stores the record (TTL default 1h).
function Add-StubRecordChecked($cmdlet, $zoneName, $name, $type, $ttl, $data) {
    $zone = Get-StubZone $zoneName
    if (-not $zone) { Write-DnsError $cmdlet 9601 ObjectNotFound "The zone $zoneName was not found on server STUB."; return }
    $key = $zone.ZoneName.ToLowerInvariant()
    $at = @($global:Records | Where-Object { $_.ZoneName -eq $key -and $_.HostName -eq $name })
    if ($type -eq 'CNAME' -and @($at | Where-Object { $_.RecordType -ne 'CNAME' }).Count) {
        Write-DnsError $cmdlet 9709 InvalidOperation "Failed to create resource record $name in zone $zoneName. A CNAME cannot be added where other data exists."; return
    }
    if ($type -ne 'CNAME' -and @($at | Where-Object { $_.RecordType -eq 'CNAME' }).Count) {
        Write-DnsError $cmdlet 9708 InvalidOperation "Failed to create resource record $name in zone $zoneName. The node is a CNAME."; return
    }
    $json = ConvertTo-Json -InputObject ([pscustomobject]$data) -Compress
    foreach ($r in $at) {
        if ($r.RecordType -eq $type -and (ConvertTo-Json -InputObject $r.RecordData -Compress) -eq $json) {
            Write-DnsError $cmdlet 9711 ResourceExists "Failed to create resource record $name in zone $zoneName on server STUB. The resource record already exists."
            return
        }
    }
    if (-not $ttl) { $ttl = [TimeSpan]'01:00:00' }
    $null = New-StubRecord $zone.ZoneName $name $type $ttl $data
}

# Absolute(): the server stores hostnames fully qualified with a dot.
function Absolute($h) { if ($h.EndsWith('.')) { $h } else { "$h." } }

function Add-DnsServerResourceRecordA {
    [CmdletBinding(SupportsShouldProcess)]
    param([switch]$AllowUpdateAny, [switch]$CreatePtr, [Parameter(Mandatory, Position = 0)][string]$Name,
        [Parameter(Mandatory, Position = 1)][ipaddress[]]$IPv4Address, [string]$ComputerName, [TimeSpan]$TimeToLive,
        [Parameter(Mandatory, Position = 2)][string]$ZoneName, [switch]$AgeRecord, [switch]$PassThru)
    Record 'Add-DnsServerResourceRecordA' $PSBoundParameters
    foreach ($ip in $IPv4Address) { Add-StubRecordChecked $PSCmdlet $ZoneName $Name 'A' $TimeToLive @{ IPv4Address = $ip } }
}

function Add-DnsServerResourceRecordAAAA {
    [CmdletBinding(SupportsShouldProcess)]
    param([switch]$AllowUpdateAny, [switch]$CreatePtr, [Parameter(Mandatory, Position = 0)][string]$Name,
        [Parameter(Mandatory, Position = 1)][ipaddress[]]$IPv6Address, [string]$ComputerName, [TimeSpan]$TimeToLive,
        [Parameter(Mandatory, Position = 2)][string]$ZoneName, [switch]$AgeRecord, [switch]$PassThru)
    Record 'Add-DnsServerResourceRecordAAAA' $PSBoundParameters
    foreach ($ip in $IPv6Address) { Add-StubRecordChecked $PSCmdlet $ZoneName $Name 'AAAA' $TimeToLive @{ IPv6Address = $ip } }
}

function Add-DnsServerResourceRecordCName {
    [CmdletBinding(SupportsShouldProcess)]
    param([switch]$AllowUpdateAny, [Parameter(Mandatory, Position = 0)][string]$Name,
        [Parameter(Mandatory, Position = 1)][string]$HostNameAlias, [string]$ComputerName, [TimeSpan]$TimeToLive,
        [Parameter(Mandatory, Position = 2)][string]$ZoneName, [switch]$AgeRecord, [switch]$PassThru)
    Record 'Add-DnsServerResourceRecordCName' $PSBoundParameters
    Add-StubRecordChecked $PSCmdlet $ZoneName $Name 'CNAME' $TimeToLive @{ HostNameAlias = (Absolute $HostNameAlias) }
}

function Add-DnsServerResourceRecordPtr {
    [CmdletBinding(SupportsShouldProcess)]
    param([switch]$AllowUpdateAny, [Parameter(Mandatory, Position = 0)][string]$Name,
        [Parameter(Mandatory, Position = 1)][string]$PtrDomainName, [string]$ComputerName, [TimeSpan]$TimeToLive,
        [Parameter(Mandatory, Position = 2)][string]$ZoneName, [switch]$AgeRecord, [switch]$PassThru)
    Record 'Add-DnsServerResourceRecordPtr' $PSBoundParameters
    Add-StubRecordChecked $PSCmdlet $ZoneName $Name 'PTR' $TimeToLive @{ PtrDomainName = (Absolute $PtrDomainName) }
}

function Add-DnsServerResourceRecordMX {
    [CmdletBinding(SupportsShouldProcess)]
    param([Parameter(Mandatory, Position = 0)][string]$Name, [Parameter(Mandatory, Position = 1)][string]$MailExchange,
        [Parameter(Mandatory, Position = 2)][uint16]$Preference, [string]$ComputerName, [TimeSpan]$TimeToLive,
        [Parameter(Mandatory, Position = 3)][string]$ZoneName, [switch]$AgeRecord, [switch]$AllowUpdateAny, [switch]$PassThru)
    Record 'Add-DnsServerResourceRecordMX' $PSBoundParameters
    Add-StubRecordChecked $PSCmdlet $ZoneName $Name 'MX' $TimeToLive @{ MailExchange = (Absolute $MailExchange); Preference = $Preference }
}

# Generic Add with the SRV and TXT parameter sets only.
function Add-DnsServerResourceRecord {
    [CmdletBinding(SupportsShouldProcess)]
    param([Parameter(Mandatory, Position = 0)][string]$ZoneName, [Parameter(Mandatory, Position = 1)][string]$Name,
        [string]$ComputerName, [TimeSpan]$TimeToLive, [switch]$AgeRecord, [switch]$AllowUpdateAny, [switch]$PassThru,
        [Parameter(Mandatory, ParameterSetName = 'SRV')][string]$DomainName,
        [Parameter(Mandatory, ParameterSetName = 'SRV')][uint16]$Priority,
        [Parameter(Mandatory, ParameterSetName = 'SRV')][uint16]$Weight,
        [Parameter(Mandatory, ParameterSetName = 'SRV')][uint16]$Port,
        [Parameter(ParameterSetName = 'SRV')][switch]$Srv,
        [Parameter(Mandatory, ParameterSetName = 'TXT')][string]$DescriptiveText,
        [Parameter(ParameterSetName = 'TXT')][switch]$Txt)
    Record 'Add-DnsServerResourceRecord' $PSBoundParameters
    if ($PSCmdlet.ParameterSetName -eq 'SRV') {
        Add-StubRecordChecked $PSCmdlet $ZoneName $Name 'SRV' $TimeToLive @{ DomainName = (Absolute $DomainName)
            Priority = $Priority; Weight = $Weight; Port = $Port }
    } else {
        Add-StubRecordChecked $PSCmdlet $ZoneName $Name 'TXT' $TimeToLive @{ DescriptiveText = $DescriptiveText }
    }
}

# InputObject set (used by the scripts) and RRType set. The real
# -InputObject is a CimInstance; the stub records are pscustomobjects, so it
# is typed [psobject] here.
function Remove-DnsServerResourceRecord {
    [CmdletBinding(SupportsShouldProcess, DefaultParameterSetName = 'InputObject')]
    param([Parameter(Mandatory, Position = 0)][string]$ZoneName,
        [Parameter(Mandatory, ValueFromPipeline, ParameterSetName = 'InputObject')][psobject]$InputObject,
        [Parameter(Mandatory, Position = 1, ParameterSetName = 'RRName')][string]$Name,
        [Parameter(Mandatory, ParameterSetName = 'RRName')][string]$RRType,
        [Parameter(ParameterSetName = 'RRName')][string[]]$RecordData,
        [switch]$PassThru, [string]$ComputerName, [switch]$Force, [string]$ZoneScope)
    Record 'Remove-DnsServerResourceRecord' $PSBoundParameters
    $r = $global:Records | Where-Object { $_.Id -eq $InputObject.Id } | Select-Object -First 1
    if (-not $r) { Write-DnsError $PSCmdlet 9701 ObjectNotFound "Failed to remove the resource record on STUB server."; return }
    $global:Records.Remove($r)
}

# Copies the new TTL onto the stored record matching the old object.
function Set-DnsServerResourceRecord {
    [CmdletBinding(SupportsShouldProcess)]
    param([Parameter(Mandatory, Position = 0)][psobject]$NewInputObject, [Parameter(Mandatory, Position = 1)][psobject]$OldInputObject,
        [string]$ComputerName, [Parameter(Mandatory, Position = 2)][string]$ZoneName, [switch]$PassThru, [string]$ZoneScope)
    Record 'Set-DnsServerResourceRecord' $PSBoundParameters
    $r = $global:Records | Where-Object { $_.Id -eq $OldInputObject.Id } | Select-Object -First 1
    if (-not $r) { Write-DnsError $PSCmdlet 9701 ObjectNotFound "Failed to update the resource record on STUB server."; return }
    $r.TimeToLive = $NewInputObject.TimeToLive
}
