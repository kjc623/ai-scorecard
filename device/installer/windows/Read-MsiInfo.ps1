<#
.SYNOPSIS
  Read what a built MSI actually says about itself, as JSON.

.DESCRIPTION
  release-msi.mjs and verify.mjs ask the package, not the build inputs, for its identity: the
  ProductCode, UpgradeCode and version Intune's detection rule matches, the package code from the
  summary stream, and any table rows a check needs (File, MoveFile, ServiceInstall, LaunchCondition).
  It opens the database read-only through the Windows Installer COM object, which ships with
  Windows, so it needs no WiX, no msiinfo and no SDK.

.EXAMPLE
  powershell -NoProfile -File device/installer/windows/Read-MsiInfo.ps1 -Path device/installer/dist/release/ShadowAICapture.msi -Table File,MoveFile
#>
[CmdletBinding()]
param(
  [Parameter(Mandatory = $true)][string]$Path,
  [string[]]$Table = @()
)

$ErrorActionPreference = "Stop"
$full = (Resolve-Path $Path).Path

# Late binding: Windows PowerShell exposes the installer's automation objects without type info, so
# every member goes through InvokeMember.
function Invoke-Com($obj, [string]$member, [string]$kind, [object[]]$argv = @()) {
  $flags = [System.Reflection.BindingFlags]::$kind
  return $obj.GetType().InvokeMember($member, $flags, $null, $obj, $argv)
}

$installer = New-Object -ComObject WindowsInstaller.Installer
$db = Invoke-Com $installer 'OpenDatabase' 'InvokeMethod' @($full, 0)

function Read-Rows([string]$sql) {
  $view = Invoke-Com $db 'OpenView' 'InvokeMethod' @($sql)
  Invoke-Com $view 'Execute' 'InvokeMethod' | Out-Null
  # ColumnInfo(0) is a record of the column names.
  $names = Invoke-Com $view 'ColumnInfo' 'GetProperty' @(0)
  $count = Invoke-Com $names 'FieldCount' 'GetProperty'
  $cols = @(); for ($i = 1; $i -le $count; $i++) { $cols += (Invoke-Com $names 'StringData' 'GetProperty' @($i)) }
  $rows = @()
  while ($true) {
    $rec = Invoke-Com $view 'Fetch' 'InvokeMethod'
    if ($null -eq $rec) { break }
    $row = [ordered]@{}
    for ($i = 1; $i -le $count; $i++) { $row[$cols[$i - 1]] = (Invoke-Com $rec 'StringData' 'GetProperty' @($i)) }
    $rows += [pscustomobject]$row
  }
  Invoke-Com $view 'Close' 'InvokeMethod' | Out-Null
  return , $rows
}

function Test-Table([string]$name) {
  $rows = Read-Rows "SELECT ``Name`` FROM ``_Tables`` WHERE ``Name`` = '$name'"
  return $rows.Count -gt 0
}

$props = [ordered]@{}
foreach ($r in (Read-Rows 'SELECT `Property`, `Value` FROM `Property`')) { $props[$r.Property] = $r.Value }

# PID_REVNUMBER (9) is the package code; PID_TEMPLATE (7) the platform;
# PID_WORDCOUNT (15) bit 1 = compressed.
$summary = Invoke-Com $installer 'SummaryInformation' 'GetProperty' @($full, 0)
$packageCode = Invoke-Com $summary 'Property' 'GetProperty' @(9)
$template = Invoke-Com $summary 'Property' 'GetProperty' @(7)

$tables = [ordered]@{}
# `powershell -File` hands a comma list over as one string, so split it here.
foreach ($t in ($Table | ForEach-Object { $_ -split ',' } | Where-Object { $_ -ne '' })) {
  if (Test-Table $t) { $tables[$t] = Read-Rows "SELECT * FROM ``$t``" } else { $tables[$t] = @() }
}

[System.Runtime.InteropServices.Marshal]::ReleaseComObject($summary) | Out-Null
[System.Runtime.InteropServices.Marshal]::ReleaseComObject($db) | Out-Null
[System.Runtime.InteropServices.Marshal]::ReleaseComObject($installer) | Out-Null

[ordered]@{
  path         = $full
  product_code = $props['ProductCode']
  upgrade_code = $props['UpgradeCode']
  version      = $props['ProductVersion']
  product_name = $props['ProductName']
  manufacturer = $props['Manufacturer']
  package_code = $packageCode
  platform     = $template
  properties   = $props
  tables       = $tables
} | ConvertTo-Json -Depth 6
