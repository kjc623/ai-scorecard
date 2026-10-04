<#
.SYNOPSIS
  Build a development ShadowAICapture.msi from a staged Windows payload.

.DESCRIPTION
  The Windows half of the platform contract (docs/05-platform-delivery.md §6.1). It composes the
  service argv from a config file using the same manifest as the Linux and macOS installers, then
  runs the WiX v4 compiler over installer/generated/windows/ShadowAICapture.wxs.

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
  [string]$SignCert = ""
)

$ErrorActionPreference = "Stop"
$Root = (Resolve-Path (Join-Path $PSScriptRoot "../..")).Path
$Stage = (Resolve-Path (Join-Path $Root $StageDir)).Path

foreach ($f in @("bin\capture-core.exe", "bin\classifier-host.exe", "bin\capture-core-run.cmd", "etc\capture-core.env.example")) {
  if (-not (Test-Path (Join-Path $Stage $f))) {
    throw "Build-Msi.ps1: $Stage\$f is missing; run: node installer/build.mjs --os windows --arch amd64"
  }
}

# The argv baked in as the SAC_ARGS default. A config file is optional: without one the MSI ships
# with an empty argv and the operator passes SAC_ARGS at install time.
$sacArgs = ""
if ($ConfigFile -ne "") {
  $cfg = (Resolve-Path (Join-Path $Root $ConfigFile)).Path
  $sacArgs = (& node (Join-Path $Root "installer/render.mjs") --args $cfg --os windows) | Out-String
  $sacArgs = $sacArgs.Trim()
  Write-Host "SAC_ARGS default (from $ConfigFile):"
  Write-Host "  $sacArgs"
}

$wix = $null
if ($WixExe -ne "") { $wix = $WixExe }
elseif (Get-Command wix -ErrorAction SilentlyContinue) { $wix = "wix" }

$out = Join-Path $Root $OutDir
New-Item -ItemType Directory -Force -Path $out | Out-Null
$msi = Join-Path $out "ShadowAICapture.msi"

if (-not $wix) {
  Write-Host ""
  Write-Host "Build-Msi.ps1: no WiX compiler found - this host cannot build an MSI." -ForegroundColor Yellow
  Write-Host "On a Windows host with the WiX v4/v5 .NET tool (dotnet tool install --global wix), run:"
  Write-Host ""
  Write-Host "  node installer/build.mjs --os windows --arch amd64"
  Write-Host "  $wix = 'wix'"
  Write-Host "  & $wix build installer/generated/windows/ShadowAICapture.wxs -arch x64 ``"
  Write-Host "      -d StageDir='$Stage' -d SacArgs='$sacArgs' -o '$msi'"
  Write-Host ""
  exit 1
}

& $wix build (Join-Path $Root "installer/generated/windows/ShadowAICapture.wxs") `
  -arch x64 `
  -d "StageDir=$Stage" `
  -d "SacArgs=$sacArgs" `
  -o $msi
if ($LASTEXITCODE -ne 0) { throw "wix build failed with exit code $LASTEXITCODE" }
Write-Host "built $msi"

if ($SignTool -ne "" -and $SignCert -ne "") {
  & $SignTool sign /fd SHA256 /f $SignCert $msi
  if ($LASTEXITCODE -ne 0) { throw "signtool failed with exit code $LASTEXITCODE" }
  Write-Host "signed $msi"
} else {
  Write-Warning "unsigned development MSI; pass -SignTool and -SignCert for a release (docs/05-platform-delivery.md §7)"
}
