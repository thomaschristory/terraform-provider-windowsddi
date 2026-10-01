# Stub DhcpServer cmdlets for script tests run under pwsh. Parameter names
# and types follow the documented cmdlet signatures. Like the real CDXML
# cmdlets, failures are written as non-terminating errors, so the scripts
# only stop on them because of the -ErrorAction default they set.
#
# Used by internal/dhcp/pwsh_test.go, which loads this file as an in-memory
# module (New-Module ... | Import-Module) before each script. Every stub:
#   1. records its call (cmdlet name + bound parameters) via Record, and
#   2. reads or updates the in-memory state below.
# State lives in globals and is lost when the pwsh process exits; tests
# rebuild it per run through a "seed" script.

# In-memory DHCP server state. Scopes and Reservations are keyed by IP string.
$global:Scopes = [ordered]@{}
$global:Reservations = [ordered]@{}
$global:Exclusions = [System.Collections.ArrayList]::new()
$global:Options = [System.Collections.ArrayList]::new()
$global:Leases = [System.Collections.ArrayList]::new()

# Common parameters (added by [CmdletBinding()]) that Record leaves out, so
# tests only see the DHCP-specific parameters a script passed.
$commonParams = 'ErrorAction', 'WarningAction', 'InformationAction', 'Verbose', 'Debug', 'ErrorVariable',
    'WarningVariable', 'InformationVariable', 'OutVariable', 'OutBuffer', 'PipelineVariable', 'ProgressAction'

# Record appends one JSON line { cmd = <name>; <param> = <value>; ... } to the
# file named by $env:WD_CALLS. Values are normalised so Go can compare them
# easily: TimeSpan -> total seconds, switch -> bool, IP -> string, array ->
# array of strings.
function Record($name, $bound) {
    $h = [ordered]@{ cmd = $name }
    foreach ($k in $bound.Keys) {
        if ($commonParams -contains $k) { continue }
        $v = $bound[$k]
        if ($v -is [TimeSpan]) { $v = $v.TotalSeconds }
        elseif ($v -is [System.Management.Automation.SwitchParameter]) { $v = $v.IsPresent }
        elseif ($v -is [ipaddress]) { $v = $v.ToString() }
        elseif ($v -is [array]) { $v = @($v | ForEach-Object { "$_" }) }
        $h[$k] = $v
    }
    Add-Content -LiteralPath $env:WD_CALLS -Value (ConvertTo-Json -InputObject $h -Compress -Depth 4) -Encoding utf8
}

# Write-DhcpError emits a non-terminating error shaped like the real cmdlets'
# ones: FullyQualifiedErrorId "DHCP <code>" and the given category (e.g.
# ObjectNotFound), so psscript's not-found detection is exercised.
function Write-DhcpError($cmdlet, $code, $category, $message) {
    $er = [System.Management.Automation.ErrorRecord]::new(
        [Exception]::new($message), "DHCP $code", $category, $null)
    $cmdlet.WriteError($er)
}

# New-Scope builds a scope object with the same property types as the real
# CIM object (IP addresses, TimeSpan). Like the real server, it reports the
# input state 'InActive' back as 'Inactive'.
function New-Scope($p) {
    [pscustomobject]@{
        ScopeId       = [ipaddress]$p.ScopeId
        SubnetMask    = [ipaddress]$p.SubnetMask
        Name          = $p.Name
        State         = if ($p.State -eq 'InActive') { 'Inactive' } else { $p.State }
        StartRange    = [ipaddress]$p.StartRange
        EndRange      = [ipaddress]$p.EndRange
        LeaseDuration = [TimeSpan]$p.LeaseDuration
        Description   = $p.Description
        Type          = $p.Type
    }
}

# ---- Scopes ----

# No -ScopeId: list all. Unknown ID: DHCP 20005 ObjectNotFound error.
function Get-DhcpServerv4Scope {
    [CmdletBinding()]
    param([string]$ComputerName, [Parameter(Position = 0)][ipaddress[]]$ScopeId)
    Record 'Get-DhcpServerv4Scope' $PSBoundParameters
    if (-not $ScopeId) { return $global:Scopes.Values }
    foreach ($id in $ScopeId) {
        if ($global:Scopes.Contains("$id")) { $global:Scopes["$id"] }
        else { Write-DhcpError $PSCmdlet 20005 ObjectNotFound "Failed to get information for scope $id on DHCP server STUB." }
    }
}

# Derives the scope ID as StartRange AND SubnetMask, as the real server does.
# A duplicate subnet gives a DHCP 20052 error.
function Add-DhcpServerv4Scope {
    [CmdletBinding(SupportsShouldProcess)]
    param([string]$ComputerName,
        [Parameter(Mandatory)][ipaddress]$StartRange, [Parameter(Mandatory)][ipaddress]$EndRange,
        [Parameter(Mandatory)][string]$Name, [string]$Description,
        [ValidateSet('Active', 'InActive')][string]$State = 'Active',
        [TimeSpan]$LeaseDuration = '8.00:00:00', [Parameter(Mandatory)][ipaddress]$SubnetMask,
        [ValidateSet('Dhcp', 'Bootp', 'Both')][string]$Type = 'Dhcp', [switch]$PassThru)
    Record 'Add-DhcpServerv4Scope' $PSBoundParameters
    $mask = $SubnetMask.GetAddressBytes(); $start = $StartRange.GetAddressBytes()
    $net = [ipaddress][byte[]](0..3 | ForEach-Object { $start[$_] -band $mask[$_] })
    if ($global:Scopes.Contains("$net")) {
        Write-DhcpError $PSCmdlet 20052 InvalidArgument "Failed to add scope $net. The specified subnet already exists."
        return
    }
    $s = New-Scope @{ ScopeId = $net; SubnetMask = $SubnetMask; Name = $Name; State = $State; StartRange = $StartRange
        EndRange = $EndRange; LeaseDuration = $LeaseDuration; Description = $Description; Type = $Type }
    $global:Scopes["$net"] = $s
    if ($PassThru) { $s }
}

# Replaces the scope with the given values (keeps the subnet mask, which Set
# cannot change). Unknown ID: DHCP 20005 ObjectNotFound error.
function Set-DhcpServerv4Scope {
    [CmdletBinding(SupportsShouldProcess, DefaultParameterSetName = 'WithoutRange')]
    param([Parameter(Mandatory, Position = 0)][ipaddress]$ScopeId, [string]$ComputerName,
        [string]$Name, [string]$Description,
        [ValidateSet('Active', 'InActive')][string]$State, [TimeSpan]$LeaseDuration,
        [ValidateSet('Dhcp', 'Bootp', 'Both')][string]$Type,
        [Parameter(Mandatory, ParameterSetName = 'WithRange')][ipaddress]$StartRange,
        [Parameter(Mandatory, ParameterSetName = 'WithRange')][ipaddress]$EndRange, [switch]$PassThru)
    Record 'Set-DhcpServerv4Scope' $PSBoundParameters
    if (-not $global:Scopes.Contains("$ScopeId")) {
        Write-DhcpError $PSCmdlet 20005 ObjectNotFound "Failed to set scope $ScopeId."
        return
    }
    $old = $global:Scopes["$ScopeId"]
    $global:Scopes["$ScopeId"] = New-Scope @{ ScopeId = $ScopeId; SubnetMask = $old.SubnetMask; Name = $Name; State = $State
        StartRange = $StartRange; EndRange = $EndRange; LeaseDuration = $LeaseDuration; Description = $Description; Type = $Type }
}

# Removes silently; does not model the "scope has active leases" failure.
function Remove-DhcpServerv4Scope {
    [CmdletBinding(SupportsShouldProcess)]
    param([Parameter(Mandatory, Position = 0)][ipaddress[]]$ScopeId, [string]$ComputerName, [switch]$Force, [switch]$Passthru)
    Record 'Remove-DhcpServerv4Scope' $PSBoundParameters
    foreach ($id in $ScopeId) { $global:Scopes.Remove("$id") }
}

# ---- Reservations ----

# Lists a scope's reservations. Unknown scope: DHCP 20005 ObjectNotFound.
function Get-DhcpServerv4Reservation {
    [CmdletBinding()]
    param([string]$ComputerName, [Parameter(Position = 0)][ipaddress]$ScopeId)
    Record 'Get-DhcpServerv4Reservation' $PSBoundParameters
    if (-not $global:Scopes.Contains("$ScopeId")) {
        Write-DhcpError $PSCmdlet 20005 ObjectNotFound "Failed to enumerate reservations in scope $ScopeId."
        return
    }
    $global:Reservations.Values | Where-Object { "$($_.ScopeId)" -eq "$ScopeId" }
}

# Like the real server: an empty Name defaults to the IP address, and the MAC
# is stored lowercase with dashes.
function Add-DhcpServerv4Reservation {
    [CmdletBinding(SupportsShouldProcess)]
    param([string]$ComputerName, [Parameter(Mandatory, Position = 0)][ipaddress]$ScopeId,
        [Parameter(Mandatory, Position = 1)][ipaddress]$IPAddress, [Parameter(Mandatory, Position = 2)][string]$ClientId,
        [string]$Name, [string]$Description, [ValidateSet('Dhcp', 'Bootp', 'Both')][string]$Type = 'Both', [switch]$PassThru)
    Record 'Add-DhcpServerv4Reservation' $PSBoundParameters
    if (-not $Name) { $Name = "$IPAddress" }
    $global:Reservations["$IPAddress"] = [pscustomobject]@{ IPAddress = $IPAddress; ScopeId = $ScopeId
        ClientId = $ClientId.ToLower().Replace(':', '-'); Name = $Name; Type = $Type; Description = $Description }
}

# Keyed by IP only. Only parameters actually passed change; an explicitly
# passed empty Description clears it.
function Set-DhcpServerv4Reservation {
    [CmdletBinding(SupportsShouldProcess)]
    param([string]$ComputerName, [Parameter(Mandatory, Position = 0)][ipaddress]$IPAddress, [string]$ClientId,
        [string]$Name, [string]$Description, [ValidateSet('Dhcp', 'Bootp', 'Both')][string]$Type, [switch]$PassThru)
    Record 'Set-DhcpServerv4Reservation' $PSBoundParameters
    $r = $global:Reservations["$IPAddress"]
    if ($ClientId) { $r.ClientId = $ClientId }
    if ($Name) { $r.Name = $Name }
    if ($PSBoundParameters.ContainsKey('Description')) { $r.Description = $Description }
    if ($Type) { $r.Type = $Type }
}

function Remove-DhcpServerv4Reservation {
    [CmdletBinding(SupportsShouldProcess)]
    param([string]$ComputerName, [Parameter(Mandatory)][ipaddress[]]$IPAddress, [switch]$PassThru)
    Record 'Remove-DhcpServerv4Reservation' $PSBoundParameters
    foreach ($ip in $IPAddress) { $global:Reservations.Remove("$ip") }
}

# ---- Exclusion ranges ----

# Lists the exclusion ranges of the first scope given.
function Get-DhcpServerv4ExclusionRange {
    [CmdletBinding()]
    param([string]$ComputerName, [Parameter(Position = 0)][ipaddress[]]$ScopeId)
    Record 'Get-DhcpServerv4ExclusionRange' $PSBoundParameters
    $global:Exclusions | Where-Object { "$($_.ScopeId)" -eq "$($ScopeId[0])" }
}

function Add-DhcpServerv4ExclusionRange {
    [CmdletBinding(SupportsShouldProcess)]
    param([string]$ComputerName, [Parameter(Mandatory, Position = 0)][ipaddress]$ScopeId,
        [Parameter(Mandatory, Position = 1)][ipaddress]$StartRange, [Parameter(Mandatory, Position = 2)][ipaddress]$EndRange, [switch]$PassThru)
    Record 'Add-DhcpServerv4ExclusionRange' $PSBoundParameters
    [void]$global:Exclusions.Add([pscustomobject]@{ ScopeId = $ScopeId; StartRange = $StartRange; EndRange = $EndRange })
}

# Only records the call; tests check the parameters it received.
function Remove-DhcpServerv4ExclusionRange {
    [CmdletBinding(SupportsShouldProcess)]
    param([string]$ComputerName, [Parameter(Mandatory, Position = 0)][ipaddress]$ScopeId,
        [Parameter(Position = 1)][ipaddress]$StartRange, [Parameter(Position = 2)][ipaddress]$EndRange, [switch]$Passthru)
    Record 'Remove-DhcpServerv4ExclusionRange' $PSBoundParameters
}

# ---- Option values ----
# Each stored option has a Level string: "" for server, the scope ID, or the
# reserved IP. Get returns every option at the requested level.
function Get-DhcpServerv4OptionValue {
    [CmdletBinding()]
    param([string]$VendorClass, [string]$ComputerName, [Parameter(Position = 0)][ipaddress]$ScopeId,
        [ipaddress]$ReservedIP, [Parameter(Position = 1)][uint32[]]$OptionId, [string]$UserClass, [switch]$All)
    Record 'Get-DhcpServerv4OptionValue' $PSBoundParameters
    $global:Options | Where-Object { "$($_.Level)" -eq "$ScopeId$ReservedIP" }
}

# Appends an option (does not replace an existing one; tests never set the
# same option twice). Name is always 'Router'. Like the real cmdlet, it
# rejects ScopeId and ReservedIP together.
function Set-DhcpServerv4OptionValue {
    [CmdletBinding(SupportsShouldProcess)]
    param([Parameter(Position = 0)][ipaddress]$ScopeId, [Parameter(Mandatory, Position = 2)][string[]]$Value,
        [Parameter(Mandatory, Position = 1)][uint32]$OptionId, [switch]$PassThru, [switch]$Force,
        [ipaddress]$ReservedIP, [string]$UserClass, [string]$ComputerName, [string]$VendorClass)
    Record 'Set-DhcpServerv4OptionValue' $PSBoundParameters
    if ($ScopeId -and $ReservedIP) { throw 'ScopeId and ReservedIP cannot both be specified' }
    [void]$global:Options.Add([pscustomobject]@{ Level = "$ScopeId$ReservedIP"; OptionId = $OptionId; Name = 'Router'
        Value = $Value; VendorClass = $VendorClass; UserClass = $UserClass })
}

# Only records the call; tests check the parameters it received.
function Remove-DhcpServerv4OptionValue {
    [CmdletBinding(SupportsShouldProcess)]
    param([string]$VendorClass, [string]$ComputerName, [Parameter(Mandatory, Position = 0)][uint32[]]$OptionId,
        [string]$UserClass, [Parameter(Position = 1)][ipaddress]$ScopeId, [ipaddress]$ReservedIP, [switch]$PassThru)
    Record 'Remove-DhcpServerv4OptionValue' $PSBoundParameters
}

# ---- Leases ----

# Returns the seeded leases of the scope; -AllLeases is only recorded.
function Get-DhcpServerv4Lease {
    [CmdletBinding()]
    param([Parameter(Position = 0)][ipaddress]$ScopeId, [string]$ComputerName, [switch]$AllLeases)
    Record 'Get-DhcpServerv4Lease' $PSBoundParameters
    $global:Leases | Where-Object { "$($_.ScopeId)" -eq "$ScopeId" }
}
