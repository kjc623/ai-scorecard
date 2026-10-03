param(
  # What to do: write and verify the registration, report it, or remove it.
  [Parameter(Mandatory = $true)]
  [ValidateSet('install', 'status', 'uninstall')]
  [string]$Action,

  # Overwrite a registry value that is already present. Without it, an existing value is reported
  # and left alone: the common case on a real device is a deployment's own registration, and this
  # script must never quietly replace it.
  [switch]$Force,

  # The endpoint binary. Defaults to the repository's own build output.
  [string]$HostExe,

  [switch]$Json
)

# native-host.ps1 - register the native-messaging host that connects the extension to
# `capture-core --native-host`.
#
# WHY THIS EXISTS. Every report on this project carried "a connected native channel is NOT VERIFIED
# - no host is registered on this host". That was never a limit of the extension or of capture-core:
# the binary has shipped a `--native-host` mode since round 6, and Chromium looks for the host
# manifest in a registry key an ordinary user can write. What was missing was (a) something that
# writes those keys, and (b) a stable extension id for `allowed_origins` to name, because an
# unpacked extension's id is derived from its path. (b) is the `key` field in manifest.json; this
# file is (a).
#
# WHY POWERSHELL AND NOT NODE. The first version of this tool shelled out to `reg.exe`. That works
# on a developer's machine and fails anywhere a process may not open a piped child - including the
# confined sandbox this repository is developed in, where `spawn` returns EPERM. `New-Item` and
# `Set-ItemProperty` are cmdlets: no child process, no pipe, and the same registry keys.
#
# WHAT IT WRITES, EXACTLY. Two values under HKCU, plus one file. HKCU, not HKLM: no elevation, and
# `-Action uninstall` removes exactly what `-Action install` created. Both values are the path to:
#
#   <repo>/.tools/tmp/capture-native-host/com.shadowaicapture.capture_core.json
#
# which the companion tool writes (node tools/native-host.mjs --write-manifest). The host manifest
# carries `allowed_origins` for the pinned extension id only - that is the extension's own security
# boundary, and no other extension id can open this host.
#
#   pwsh -File tools/native-host.ps1 -Action install
#   pwsh -File tools/native-host.ps1 -Action status
#   pwsh -File tools/native-host.ps1 -Action uninstall

$ErrorActionPreference = 'Stop'

$HostName = 'com.shadowaicapture.capture_core'
# $PSScriptRoot is extension/tools. Two levels up is the repository root.
$Pkg = Split-Path -Parent $PSScriptRoot   # extension
$Repo = Split-Path -Parent $Pkg           # repository root
$HostDir = Join-Path $Repo '.tools\tmp\capture-native-host'
$ManifestFile = Join-Path $HostDir "$HostName.json"
$EndpointExeDefault = Join-Path $Repo 'endpoint\capture-core\bin\capture-core.exe'

# Chromium reads a per-user native messaging host from these keys. Edge first, because Edge 154 is
# the only installed Chromium that will load an unpacked extension (Chrome 154 refuses
# --load-extension outright); Chrome is registered too so the same check works where it is allowed.
$RegistryKeys = @(
  "HKCU:\Software\Microsoft\Edge\NativeMessagingHosts\$HostName",
  "HKCU:\Software\Google\Chrome\NativeMessagingHosts\$HostName"
)

function Get-Registration {
  param([string]$Key)
  if (-not (Test-Path $Key)) { return $null }
  $item = Get-ItemProperty -Path $Key -ErrorAction SilentlyContinue
  if ($null -eq $item) { return $null }
  $value = $item.'(default)'
  if ($null -eq $value) { return '(present, no default value)' }
  return [string]$value
}

function Write-Registration {
  param([string]$Key, [string]$Value)
  if (-not (Test-Path $Key)) { New-Item -Path $Key -Force | Out-Null }
  Set-ItemProperty -Path $Key -Name '(default)' -Value $Value
}

$result = [ordered]@{ action = $Action }

switch ($Action) {
  'status' {
    $result.manifestFile = $ManifestFile
    $result.manifestExists = Test-Path $ManifestFile
    $result.hostExe = if ($HostExe) { $HostExe } else { $EndpointExeDefault }
    $result.hostExeExists = Test-Path $result.hostExe
    $result.keys = @($RegistryKeys | ForEach-Object { [ordered]@{ key = $_; value = Get-Registration -Key $_ } })
  }

  'install' {
    if (-not $HostExe) { $HostExe = $EndpointExeDefault }
    if (-not (Test-Path $HostExe)) {
      $result.ok = $false
      $result.reason = 'no_endpoint_binary'
      $result.detail = "no endpoint binary at $HostExe; build it with: cd endpoint/capture-core; go build -o bin/capture-core.exe ./cmd/capture-core"
      break
    }
    if (-not (Test-Path $ManifestFile)) {
      $result.ok = $false
      $result.reason = 'no_host_manifest'
      $result.detail = "write it first: node tools/native-host.mjs --write-manifest (it computes the pinned extension id from manifest.json)"
      break
    }

    # The launcher, and why one is needed at all: Chromium launches a native messaging host with
    # **no arguments** — the host manifest has no field for them. `capture-core` refuses to start
    # without `--spool-dir` ("a provider with nowhere to write must not start", §3.5 step 2), so the
    # bare binary cannot be the host path. This shim supplies the flags, and the host manifest points
    # at the shim. Regenerated on every install, so it cannot drift from the flags the Node tool
    # prints. On a real deployment the MSI installs this same shim beside the binary — the shape is
    # the same, only the location differs.
    $launcher = Join-Path $HostDir "$HostName.cmd"
    $hostManifest = Get-Content -Raw $ManifestFile | ConvertFrom-Json
    if (-not $hostManifest._args -or $hostManifest._args.Count -eq 0) {
      $result.ok = $false
      $result.reason = 'manifest_has_no_args'
      $result.detail = 'the host manifest carries no _args, so the launcher would launch nothing useful'
      break
    }
    $lines = @('@echo off', ('"' + $HostExe + '" ' + ($hostManifest._args -join ' ')))
    Set-Content -Path $launcher -Value $lines -Encoding ASCII
    $result.launcher = $launcher

    $skipped = @()
    foreach ($key in $RegistryKeys) {
      $current = Get-Registration -Key $key
      if ($null -ne $current -and $current -ne $ManifestFile -and -not $Force) {
        $skipped += [ordered]@{ key = $key; existing = $current; reason = 'already registered to something else; pass -Force to replace' }
        continue
      }
      Write-Registration -Key $key -Value $ManifestFile
    }

    $verified = @($RegistryKeys | ForEach-Object { [ordered]@{ key = $_; value = Get-Registration -Key $_ } })
    $result.ok = @($verified | Where-Object { $_.value -eq $ManifestFile }).Count -gt 0
    $result.hostExe = $HostExe
    $result.manifestFile = $ManifestFile
    $result.skipped = $skipped
    $result.verified = $verified
    $result.manifest = (Get-Content -Raw $ManifestFile | ConvertFrom-Json)
  }

  'uninstall' {
    $removed = @()
    foreach ($key in $RegistryKeys) {
      $existed = Test-Path $key
      if ($existed) { Remove-Item -Path $key -Recurse -Force }
      $removed += [ordered]@{ key = $key; removed = $existed }
    }
    # The manifest file goes; the host's own state (spool, key, health log) stays, because it is the
    # evidence of what the host actually did during a run.
    $fileRemoved = $false
    if (Test-Path $ManifestFile) { Remove-Item $ManifestFile -Force; $fileRemoved = $true }
    $result.removed = $removed
    $result.manifestFileRemoved = $fileRemoved
  }
}

if ($Json) {
  $result | ConvertTo-Json -Depth 8
} else {
  foreach ($entry in $result.GetEnumerator()) {
    $v = $entry.Value
    if ($v -is [System.Array]) {
      Write-Output "$($entry.Key):"
      foreach ($item in $v) { Write-Output "  - $(($item | ConvertTo-Json -Compress -Depth 6))" }
    } elseif ($v -is [System.Management.Automation.PSCustomObject]) {
      Write-Output "$($entry.Key):"
      foreach ($p in $v.PSObject.Properties) { Write-Output "  $($p.Name): $($p.Value)" }
    } else {
      Write-Output "$($entry.Key): $v"
    }
  }
}

if ($Action -eq 'install' -and -not $result.ok) { exit 1 }
exit 0
