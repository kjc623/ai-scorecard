<#
.SYNOPSIS
  Build a development ShadowAICapture.msi from a staged Windows payload.

.DESCRIPTION
  The Windows half of the platform contract (docs/05-platform-delivery.md §6.1). It stages the
  enrolment profile (capture-core.env) and runs the WiX v4+ compiler over
  installer/generated/windows/ShadowAICapture.wxs, which installs it and registers the service that
  reads it via --config-file.

  NOT VERIFIED ON THIS HOST: there is no Windows, no WiX and no signtool on the machine this was
  written on. With no `wix` on PATH the script prints the exact commands it would run and exits
  non-zero, rather than pretending to have produced an artefact.

.EXAMPLE
  node installer/build.mjs --os windows --arch amd64
  pwsh installer/windows/Build-Msi.ps1 -ConfigFile installer/profiles/lab.env
#>
[CmdletBinding()]
param(
  [string]$StageDir = "installer/.stage/windows-amd64",
  [string]$OutDir = "installer/dist",
  [string]$ConfigFile = "",
  [string]$WixExe = "",
  [string]$SignTool = "",
  [string]$SignCert = "",
  # WiX v7 requires accepting the Open Source Maintenance Fee EULA before it will build (WIX7015).
  # Pass -EulaId wix7 for an automated build, or run 'wix eula accept wix7' once per user instead.
  # This script does not accept the EULA on your behalf.
  [string]$EulaId = ""
)

$ErrorActionPreference = "Stop"
$Root = (Resolve-Path (Join-Path $PSScriptRoot "../..")).Path
$Stage = (Resolve-Path (Join-Path $Root $StageDir)).Path

foreach ($f in @("bin\capture-core.exe", "bin\classifier-host.exe", "bin\capture-core-run.cmd", "etc\capture-core.env.example")) {
  if (-not (Test-Path (Join-Path $Stage $f))) {
    throw "Build-Msi.ps1: $Stage\$f is missing; run: node installer/build.mjs --os windows --arch amd64"
  }
}

# The endpoint configuration. The MSI installs etc\capture-core.env and the service reads it via
# --config-file, so the profile is a file, not a command line: a path or secret with spaces needs no
# quoting, and it is the same file the console wrapper (capture-core-run.cmd) reads. The file must be
# COMPLETE (capture-core resolves only what it contains), so render.mjs --env merges the profile over
# the windows defaults; with no -ConfigFile the defaults alone are written.
$envArgs = @('--env', '--os', 'windows')
if ($ConfigFile -ne "") {
  $envArgs = @('--env', (Resolve-Path (Join-Path $Root $ConfigFile)).Path, '--os', 'windows')
}
$envText = (& node (Join-Path $Root 'installer/render.mjs') @envArgs) | Out-String
Set-Content -Path (Join-Path $Stage 'etc\capture-core.env') -Value $envText -Encoding ascii
Write-Host "installed capture-core.env ($($envText.Trim().Split("`n").Count) keys; profile merged over windows defaults)"

# Find the WiX CLI three ways, because `dotnet tool install --global wix` puts wix.exe in
# %USERPROFILE%\.dotnet\tools, which is not always on the PATH of the shell that runs this script.
$wix = $null
if ($WixExe -ne "") { $wix = $WixExe }
elseif (Get-Command wix -ErrorAction SilentlyContinue) { $wix = 'wix' }
elseif (Test-Path (Join-Path $env:USERPROFILE '.dotnet\tools\wix.exe')) { $wix = Join-Path $env:USERPROFILE '.dotnet\tools\wix.exe' }
elseif (Test-Path (Join-Path $env:USERPROFILE '.dotnet\tools\wix')) { $wix = Join-Path $env:USERPROFILE '.dotnet\tools\wix' }

$out = Join-Path $Root $OutDir
New-Item -ItemType Directory -Force -Path $out | Out-Null
$msi = Join-Path $out "ShadowAICapture.msi"

if (-not $wix) {
  Write-Host ""
  Write-Host "Build-Msi.ps1: no WiX compiler found - this host cannot build an MSI." -ForegroundColor Yellow
  Write-Host "Install WiX one of two ways, then re-run this script:"
  Write-Host "  1) standalone CLI, no .NET SDK: install wix-cli-x64.msi from"
  Write-Host "     https://github.com/wixtoolset/wix/releases  (v5.0.2 asset: wix-cli-x64.msi)"
  Write-Host "  2) .NET tool: 'dotnet tool install --global wix' -- needs the .NET SDK 6+;"
  Write-Host "     check with 'dotnet --list-sdks' (an empty list means only a runtime is installed)."
  Write-Host "     The tool lands in %USERPROFILE%\.dotnet\tools; this script looks there when 'wix' is not on PATH."
  Write-Host "Then build:"
  Write-Host ""
  Write-Host "  node installer/build.mjs --os windows --arch amd64"
  Write-Host "  wix build installer/generated/windows/ShadowAICapture.wxs -arch x64 ``"
  Write-Host "      -d StageDir='$Stage' -o '$msi'"
  Write-Host ""
  exit 1
}

# An argument array (splatted) keeps values that contain spaces as one argument on every PowerShell.
$wixArgs = @(
  'build',
  (Join-Path $Root 'installer/generated/windows/ShadowAICapture.wxs'),
  '-arch', 'x64'
)
if ($EulaId -ne '') { $wixArgs += @('-acceptEula', $EulaId) }
$wixArgs += @('-d', "StageDir=$Stage", '-o', $msi)
$buildOut = & $wix @wixArgs 2>&1
$buildCode = $LASTEXITCODE
$buildOut | ForEach-Object { Write-Host $_ }
if ($buildCode -ne 0) {
  throw "wix build failed with exit code $buildCode`n$($buildOut | Out-String)"
}
Write-Host "built $msi"

if ($SignTool -ne "" -and $SignCert -ne "") {
  & $SignTool sign /fd SHA256 /f $SignCert $msi
  if ($LASTEXITCODE -ne 0) { throw "signtool failed with exit code $LASTEXITCODE" }
  Write-Host "signed $msi"
} else {
  Write-Warning "unsigned development MSI; pass -SignTool and -SignCert for a release (docs/05-platform-delivery.md §7)"
}
