#Requires -Version 5.1
<#
.SYNOPSIS
Publishes an agent release to the testbed's one Intune Win32 app, assigns it to the test device
group, and asks Intune to sync the VM.

.DESCRIPTION
Run by tools/testbed/deploy.mjs, which passes the values from refactor-endpoint/TESTBED.md.
Authenticates as the publishing app registration with its certificate and calls Microsoft Graph
only through Invoke-MgGraphRequest.

Without -AppId it creates the app and prints "app-id <id>" as soon as it exists, so deploy.mjs
records it. With -AppId it changes that app only, after checking it is the testbed's app. With
-IntuneWinFile it uploads the package as a new content version, sets the MSI product code and
version detection, and commits it. It then makes the one assignment (-Intent) to -GroupId and, with
-EntraDeviceId, syncs that managed device.

Nothing secret is printed: not the package's encryption info, not the upload URI.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$TenantId,
    [Parameter(Mandatory = $true)][string]$ClientId,
    [Parameter(Mandatory = $true)][string]$CertificateThumbprint,
    [Parameter(Mandatory = $true)][string]$GroupId,
    [ValidateSet('required', 'uninstall')][string]$Intent = 'required',
    [string]$AppId,
    [string]$IntuneWinFile,
    [string]$Version,
    [string]$ProductCode,
    [string]$EntraDeviceId
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
Set-StrictMode -Version 2.0

$Graph = 'https://graph.microsoft.com/beta'
$DisplayName = 'Shadow AI Capture (pre-prod test)'
$BlockBytes = 1MB

function Write-Step([string]$Message) {
    Write-Host "publish: $Message"
}

function Invoke-Graph([string]$Method, [string]$Path, $Body) {
    $uri = $Path
    if ($uri -notmatch '^https://') { $uri = "$Graph/$Path" }
    $request = @{ Method = $Method; Uri = $uri; OutputType = 'HashTable' }
    if ($null -ne $Body) {
        $request.Body = ConvertTo-Json -InputObject $Body -Depth 10 -Compress
        $request.ContentType = 'application/json'
    }
    Invoke-MgGraphRequest @request
}

function Assert-TestbedApp([string]$Id) {
    $app = Invoke-Graph 'GET' "deviceAppManagement/mobileApps/$Id" $null
    if ($app['@odata.type'] -ne '#microsoft.graph.win32LobApp' -or $app['displayName'] -ne $DisplayName) {
        throw "refusing Intune app ${Id}: it is not the Win32 app '$DisplayName'"
    }
}

# Reads Detection.xml and extracts the encrypted content of a .intunewin to a temporary file.
function Read-IntuneWin([string]$Path) {
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $zip = [System.IO.Compression.ZipFile]::OpenRead($Path)
    try {
        $entries = @{}
        foreach ($e in $zip.Entries) { $entries[$e.FullName.Replace('\', '/')] = $e }
        $detection = $entries['IntuneWinPackage/Metadata/Detection.xml']
        if ($null -eq $detection) { throw "$Path has no IntuneWinPackage/Metadata/Detection.xml" }
        $reader = New-Object System.IO.StreamReader($detection.Open())
        try { [xml]$xml = $reader.ReadToEnd() } finally { $reader.Dispose() }
        $info = $xml.ApplicationInfo
        $content = $entries["IntuneWinPackage/Contents/$($info.FileName)"]
        if ($null -eq $content) { throw "$Path has no IntuneWinPackage/Contents/$($info.FileName)" }
        $file = Join-Path ([System.IO.Path]::GetTempPath()) ('sac-testbed-' + [guid]::NewGuid().ToString('N') + '.bin')
        [System.IO.Compression.ZipFileExtensions]::ExtractToFile($content, $file, $true)
        return @{ Info = $info; File = $file }
    } finally {
        $zip.Dispose()
    }
}

# The app's properties: the MSI from Detection.xml, installed as SYSTEM, detected by its product
# code at this version or later, never restarting the VM (its users stay signed in).
function New-AppBody($Info) {
    $msi = $Info.MsiInfo
    $rule = @{
        '@odata.type'          = '#microsoft.graph.win32LobAppProductCodeRule'
        ruleType               = 'detection'
        productCode            = $msi.MsiProductCode
        productVersionOperator = 'greaterThanOrEqual'
        productVersion         = $msi.MsiProductVersion
    }
    $codes = @(
        @{ returnCode = 0; type = 'success' },
        @{ returnCode = 1707; type = 'success' },
        @{ returnCode = 3010; type = 'softReboot' },
        @{ returnCode = 1641; type = 'hardReboot' },
        @{ returnCode = 1618; type = 'retry' }
    )
    return @{
        '@odata.type'                   = '#microsoft.graph.win32LobApp'
        displayName                     = $DisplayName
        description                     = 'The Shadow AI Capture agent built by the deploy workflow, for the reference VM.'
        publisher                       = $msi.MsiPublisher
        displayVersion                  = $msi.MsiProductVersion
        fileName                        = $Info.FileName
        setupFilePath                   = $Info.SetupFile
        installCommandLine              = "msiexec /i `"$($Info.SetupFile)`" /qn"
        uninstallCommandLine            = "msiexec /x $($msi.MsiProductCode) /qn"
        applicableArchitectures         = 'x64'
        minimumSupportedOperatingSystem = @{ v10_21H1 = $true }
        installExperience               = @{ runAsAccount = 'system'; deviceRestartBehavior = 'suppress' }
        returnCodes                     = $codes
        rules                           = @($rule)
        msiInformation                  = @{
            '@odata.type'  = '#microsoft.graph.win32LobAppMsiInformation'
            productCode    = $msi.MsiProductCode
            productVersion = $msi.MsiProductVersion
            upgradeCode    = $msi.MsiUpgradeCode
            requiresReboot = $false
            packageType    = 'perMachine'
            productName    = $Info.Name
            publisher      = $msi.MsiPublisher
        }
    }
}

function Wait-FileState([string]$Uri, [string]$Stage) {
    $deadline = (Get-Date).AddMinutes(10)
    while ($true) {
        $file = Invoke-Graph 'GET' $Uri $null
        $state = [string]$file['uploadState']
        if ($state -eq "${Stage}Success") { return $file }
        if ($state -ne "${Stage}Pending") { throw "the content file is in upload state '$state'" }
        if ((Get-Date) -gt $deadline) { throw "the content file stayed in '$state' for 10 minutes" }
        Start-Sleep -Seconds 5
    }
}

# Azure Storage Put Block for each block, then Put Block List. The URI is a SAS: never printed.
function Send-Blob([string]$SasUri, [string]$Path) {
    $ids = New-Object System.Collections.Generic.List[string]
    $stream = [System.IO.File]::OpenRead($Path)
    try {
        $index = 0
        while ($stream.Position -lt $stream.Length) {
            $size = [int][Math]::Min($BlockBytes, $stream.Length - $stream.Position)
            $block = New-Object byte[] $size
            $read = 0
            while ($read -lt $size) { $read += $stream.Read($block, $read, $size - $read) }
            $id = [Convert]::ToBase64String([System.Text.Encoding]::ASCII.GetBytes($index.ToString('0000')))
            try {
                Invoke-WebRequest -UseBasicParsing -Method Put -Uri ($SasUri + '&comp=block&blockid=' + [uri]::EscapeDataString($id)) `
                    -Headers @{ 'x-ms-blob-type' = 'BlockBlob' } -ContentType 'application/octet-stream' -Body $block | Out-Null
            } catch {
                throw "uploading block $index to Azure Storage failed: $($_.Exception.Message)"
            }
            $ids.Add($id)
            $index++
        }
    } finally {
        $stream.Dispose()
    }
    $list = '<?xml version="1.0" encoding="utf-8"?><BlockList>' + (($ids | ForEach-Object { "<Latest>$_</Latest>" }) -join '') + '</BlockList>'
    try {
        Invoke-WebRequest -UseBasicParsing -Method Put -Uri ($SasUri + '&comp=blocklist') -ContentType 'text/plain' -Body $list | Out-Null
    } catch {
        throw "committing the block list to Azure Storage failed: $($_.Exception.Message)"
    }
    Write-Step "uploaded $($ids.Count) blocks"
}

# The Win32 LOB app upload: content version, file, blocks, commit. Returns the content version.
function Publish-Content([string]$Id, $Package) {
    $info = $Package.Info
    $base = "deviceAppManagement/mobileApps/$Id/microsoft.graph.win32LobApp/contentVersions"
    $content = Invoke-Graph 'POST' $base @{}
    $body = @{
        '@odata.type' = '#microsoft.graph.mobileAppContentFile'
        name          = [string]$info.FileName
        size          = [int64]$info.UnencryptedContentSize
        sizeEncrypted = [int64](Get-Item -LiteralPath $Package.File).Length
        manifest      = $null
        isDependency  = $false
    }
    $file = Invoke-Graph 'POST' "$base/$($content['id'])/files" $body
    $fileUri = "$base/$($content['id'])/files/$($file['id'])"
    $file = Wait-FileState $fileUri 'azureStorageUriRequest'
    Send-Blob $file['azureStorageUri'] $Package.File
    $enc = $info.EncryptionInfo
    $commit = @{
        fileEncryptionInfo = @{
            '@odata.type'        = '#microsoft.graph.fileEncryptionInfo'
            encryptionKey        = $enc.EncryptionKey
            macKey               = $enc.MacKey
            initializationVector = $enc.InitializationVector
            mac                  = $enc.Mac
            profileIdentifier    = $enc.ProfileIdentifier
            fileDigest           = $enc.FileDigest
            fileDigestAlgorithm  = $enc.FileDigestAlgorithm
        }
    }
    Invoke-Graph 'POST' "$fileUri/commit" $commit | Out-Null
    Wait-FileState $fileUri 'commitFile' | Out-Null
    Write-Step "committed the file of content version $($content['id'])"
    return [string]$content['id']
}

# Exactly one assignment: -Intent to the test device group.
function Set-Assignment([string]$Id) {
    $current = @((Invoke-Graph 'GET' "deviceAppManagement/mobileApps/$Id/assignments" $null)['value'] | Where-Object { $_ })
    if ($current.Count -eq 1 -and $current[0]['intent'] -eq $Intent -and
        $current[0]['target']['@odata.type'] -eq '#microsoft.graph.groupAssignmentTarget' -and $current[0]['target']['groupId'] -eq $GroupId) {
        Write-Step "already assigned as $Intent to the test device group"
        return
    }
    $assignment = @{
        '@odata.type' = '#microsoft.graph.mobileAppAssignment'
        intent        = $Intent
        target        = @{ '@odata.type' = '#microsoft.graph.groupAssignmentTarget'; groupId = $GroupId }
        settings      = @{
            '@odata.type'                = '#microsoft.graph.win32LobAppAssignmentSettings'
            notifications                = 'hideAll'
            deliveryOptimizationPriority = 'notConfigured'
        }
    }
    Invoke-Graph 'POST' "deviceAppManagement/mobileApps/$Id/assign" @{ mobileAppAssignments = @($assignment) } | Out-Null
    Write-Step "assigned as $Intent to the test device group"
}

# Finds the managed device by its Entra device id and asks Intune to sync it.
function Sync-Device([string]$DeviceId) {
    $uri = 'deviceManagement/managedDevices?$select=id,azureADDeviceId,deviceName'
    $device = $null
    while ($uri -and -not $device) {
        $page = Invoke-Graph 'GET' $uri $null
        $device = @($page['value']) | Where-Object { $_['azureADDeviceId'] -eq $DeviceId } | Select-Object -First 1
        $uri = $null
        if ($page.ContainsKey('@odata.nextLink')) { $uri = $page['@odata.nextLink'] }
    }
    if (-not $device) { throw "no Intune managed device has Entra device id $DeviceId" }
    Invoke-Graph 'POST' "deviceManagement/managedDevices/$($device['id'])/syncDevice" $null | Out-Null
    Write-Step "asked Intune to sync $($device['deviceName'])"
}

if (-not $AppId -and -not $IntuneWinFile) { throw 'no app is recorded in TESTBED.md and no package was given' }
if ($IntuneWinFile -and (-not $Version -or -not $ProductCode)) { throw '-IntuneWinFile needs -Version and -ProductCode' }

Import-Module Microsoft.Graph.Authentication
Connect-MgGraph -ClientId $ClientId -TenantId $TenantId -CertificateThumbprint $CertificateThumbprint -NoWelcome | Out-Null
$package = $null
try {
    if ($AppId) { Assert-TestbedApp $AppId }
    if ($IntuneWinFile) {
        $package = Read-IntuneWin $IntuneWinFile
        $msi = $package.Info.MsiInfo
        if (($msi.MsiProductCode -replace '[{}]', '') -ne ($ProductCode -replace '[{}]', '') -or $msi.MsiProductVersion -ne $Version) {
            throw "the package holds $($msi.MsiProductVersion) $($msi.MsiProductCode), not the release's $Version $ProductCode"
        }
        $body = New-AppBody $package.Info
        if (-not $AppId) {
            $created = Invoke-Graph 'POST' 'deviceAppManagement/mobileApps' $body
            $AppId = $created['id']
            Write-Host "app-id $AppId"
            Write-Step "created app $AppId"
        }
        # The detection rule and the command lines change with the content they describe, in one PATCH.
        $body.committedContentVersion = Publish-Content $AppId $package
        Invoke-Graph 'PATCH' "deviceAppManagement/mobileApps/$AppId" $body | Out-Null
        Write-Step "app $AppId now delivers $Version"
    }
    Set-Assignment $AppId
    if ($EntraDeviceId) { Sync-Device $EntraDeviceId }
    Write-Host "app-id $AppId"
} finally {
    if ($package) { Remove-Item -LiteralPath $package.File -Force -ErrorAction SilentlyContinue }
    Disconnect-MgGraph | Out-Null
}
