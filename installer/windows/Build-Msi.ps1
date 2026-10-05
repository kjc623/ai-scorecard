<#
.SYNOPSIS
  Build ShadowAICapture.msi from a staged Windows payload.

.DESCRIPTION
  The Windows half of the platform contract (docs/05-platform-delivery.md section 6.1). It runs the WiX v5+
  compiler over installer/generated/windows/ShadowAICapture.wxs. Do not call it directly: the two
  entry points stage the payload it needs and pass it the right switches.

    node installer/release-msi.mjs   the generic, code-only MSI and release.json (installer/dist/release)
    node installer/lab-msi.mjs       the same MSI plus the lab's profile files, for this host

  The stage must hold the binaries, the signed classifier release (classifier\) and the generic,
  vendor-wide etc\capture-core.env (render.mjs --generic). The MSI carries no tenant data: a tenant's
  ShadowAICapture.tenant.env goes BESIDE it, and the MSI copies it at install. -TenantEnv puts one
  there for a lab build; without it, a stale one is removed so a release folder never holds one.

  Signing is OFF unless -Sign is passed (or SAC_SIGN=1). It then signs the two executables before
  they are packaged and the MSI after, with signtool and exactly one of:
    Trusted Signing   -SignDlib <Azure.CodeSigning.Dlib.dll> -SignMetadata <metadata.json>  (SAC_SIGN_DLIB, SAC_SIGN_METADATA)
    certificate store -SignThumbprint <SHA-1 thumbprint>                                    (SAC_SIGN_THUMBPRINT)
    PFX file          -SignPfx <file.pfx>, password in SAC_SIGN_PFX_PASSWORD                (SAC_SIGN_PFX)
  signtool is -SignTool, SAC_SIGNTOOL, PATH, or the newest Windows SDK's x64 signtool.exe. The
  timestamp server is -TimestampUrl or SAC_SIGN_TIMESTAMP_URL. No build host has signed yet.

  With no `wix` it prints the commands it would run and exits non-zero rather than pretending.
#>
[CmdletBinding()]
param(
  [string]$StageDir = "installer/.stage/windows-amd64",
  [string]$OutDir = "installer/dist",
  # The MSI ProductVersion (major.minor.build). Empty keeps the manifest's version.
  [string]$Version = "",
  # Lab only: the files a lab profile points at (signed policy bundle, device CA, pinned edge CA).
  # Installed, tree preserved, under C:\ProgramData\ShadowAICapture\profile.
  [string]$ProfileDir = "",
  # Lab only: a tenant file to place beside the MSI as ShadowAICapture.tenant.env. -ConfigFile is
  # the earlier name, kept so the instructions in profiles/lab-host.env still work.
  [Alias("ConfigFile")]
  [string]$TenantEnv = "",
  # The lab carries a new enrolment token for a server that does not know the credential an
  # earlier install sealed: clear the agent's state on install so the device enrols again.
  [switch]$FreshEnrolment,
  [string]$WixExe = "",
  # WiX v7 requires accepting the Open Source Maintenance Fee EULA before it will build (WIX7015).
  # Pass -EulaId wix7 for an automated build, or run 'wix eula accept wix7' once per user instead.
  # This script does not accept the EULA on your behalf.
  [string]$EulaId = "",
  [switch]$Sign,
  [string]$SignTool = "",
  [string]$SignDlib = "",
  [string]$SignMetadata = "",
  [string]$SignThumbprint = "",
  [string]$SignPfx = "",
  [string]$TimestampUrl = ""
)

$ErrorActionPreference = "Stop"
$Root = (Resolve-Path (Join-Path $PSScriptRoot "../..")).Path
$TenantFileName = "ShadowAICapture.tenant.env"
# Inputs may be given relative to the repository root (as the Node entry points pass them) or absolute.
function Resolve-Input([string]$p) { if ([IO.Path]::IsPathRooted($p)) { return (Resolve-Path $p).Path } ; return (Resolve-Path (Join-Path $Root $p)).Path }
$Stage = Resolve-Input $StageDir

foreach ($f in @("bin\capture-core.exe", "bin\classifier-host.exe", "bin\capture-core-run.cmd", "etc\capture-core.env")) {
  if (-not (Test-Path (Join-Path $Stage $f))) {
    throw "Build-Msi.ps1: $Stage\$f is missing; build through: node installer/release-msi.mjs (or lab-msi.mjs)"
  }
}
if (-not (Get-ChildItem -Recurse -File (Join-Path $Stage 'classifier') -ErrorAction SilentlyContinue)) {
  throw "Build-Msi.ps1: $Stage\classifier holds no signed classifier release; build through: node installer/release-msi.mjs"
}

# The lab's profile files are staged fresh every build, so a file dropped from the profile is dropped
# from the MSI, and a generic build never picks up what an earlier lab build left in the stage.
$stageProfile = Join-Path $Stage 'profile'
if (Test-Path $stageProfile) { Remove-Item -Recurse -Force $stageProfile }
if ($ProfileDir -ne "") {
  $src = Resolve-Input $ProfileDir
  foreach ($clash in @('capture-core.env', 'tenant.env')) {
    if (Test-Path (Join-Path $src $clash)) { throw "Build-Msi.ps1: -ProfileDir must not hold $clash; the MSI installs that name itself" }
  }
  New-Item -ItemType Directory -Force -Path $stageProfile | Out-Null
  Copy-Item -Recurse -Force (Join-Path $src '*') $stageProfile
  Write-Host "staged lab profile files ($((Get-ChildItem -Recurse -File $stageProfile).Count))"
}

# ---------------------------------------------------------------------------------------------
# Signing (off unless asked)

if (-not $Sign -and $env:SAC_SIGN -eq '1') { $Sign = $true }
function Get-Setting([string]$value, [string]$envName) { if ($value -ne '') { return $value } ; return [Environment]::GetEnvironmentVariable($envName) }
$signArgs = $null
if ($Sign) {
  $tool = Get-Setting $SignTool 'SAC_SIGNTOOL'
  if (-not $tool) { $cmd = Get-Command signtool.exe -ErrorAction SilentlyContinue; if ($cmd) { $tool = $cmd.Source } }
  if (-not $tool) {
    $tool = Get-ChildItem "${env:ProgramFiles(x86)}\Windows Kits\10\bin\*\x64\signtool.exe" -ErrorAction SilentlyContinue |
      Sort-Object FullName -Descending | Select-Object -First 1 -ExpandProperty FullName
  }
  if (-not $tool) { throw "Build-Msi.ps1 -Sign: signtool.exe not found; install the Windows SDK signing tools or pass -SignTool" }
  $dlib = Get-Setting $SignDlib 'SAC_SIGN_DLIB'
  $meta = Get-Setting $SignMetadata 'SAC_SIGN_METADATA'
  $thumb = Get-Setting $SignThumbprint 'SAC_SIGN_THUMBPRINT'
  $pfx = Get-Setting $SignPfx 'SAC_SIGN_PFX'
  $ts = Get-Setting $TimestampUrl 'SAC_SIGN_TIMESTAMP_URL'
  $modes = @($dlib, $thumb, $pfx) | Where-Object { $_ }
  if ($modes.Count -ne 1) { throw "Build-Msi.ps1 -Sign: configure exactly one of Trusted Signing (dlib + metadata), a certificate thumbprint, or a PFX" }
  if ($dlib) {
    if (-not $meta) { throw "Build-Msi.ps1 -Sign: Trusted Signing needs -SignMetadata (the account/profile metadata.json)" }
    if (-not $ts) { $ts = 'http://timestamp.acs.microsoft.com' }
    $signArgs = @('sign', '/v', '/fd', 'SHA256', '/tr', $ts, '/td', 'SHA256', '/dlib', $dlib, '/dmdf', $meta)
  } elseif ($thumb) {
    if (-not $ts) { $ts = 'http://timestamp.digicert.com' }
    $signArgs = @('sign', '/v', '/fd', 'SHA256', '/sha1', $thumb, '/tr', $ts, '/td', 'SHA256')
  } else {
    if (-not $ts) { $ts = 'http://timestamp.digicert.com' }
    $signArgs = @('sign', '/v', '/fd', 'SHA256', '/f', $pfx, '/tr', $ts, '/td', 'SHA256')
    if ($env:SAC_SIGN_PFX_PASSWORD) { $signArgs += @('/p', $env:SAC_SIGN_PFX_PASSWORD) }
  }
}
function Invoke-Sign([string]$file) {
  & $tool @signArgs $file
  if ($LASTEXITCODE -ne 0) { throw "signtool failed on $file with exit code $LASTEXITCODE" }
  Write-Host "signed $file"
}

# ---------------------------------------------------------------------------------------------
# WiX

# Find the WiX CLI three ways, because `dotnet tool install --global wix` puts wix.exe in
# %USERPROFILE%\.dotnet\tools, which is not always on the PATH of the shell that runs this script.
$wix = $null
if ($WixExe -ne "") { $wix = $WixExe }
elseif (Get-Command wix -ErrorAction SilentlyContinue) { $wix = 'wix' }
elseif (Test-Path (Join-Path $env:USERPROFILE '.dotnet\tools\wix.exe')) { $wix = Join-Path $env:USERPROFILE '.dotnet\tools\wix.exe' }
elseif (Test-Path (Join-Path $env:USERPROFILE '.dotnet\tools\wix')) { $wix = Join-Path $env:USERPROFILE '.dotnet\tools\wix' }

$out = if ([IO.Path]::IsPathRooted($OutDir)) { $OutDir } else { Join-Path $Root $OutDir }
New-Item -ItemType Directory -Force -Path $out | Out-Null
$msi = Join-Path $out "ShadowAICapture.msi"

$wixArgs = @(
  'build',
  (Join-Path $Root 'installer/generated/windows/ShadowAICapture.wxs'),
  '-arch', 'x64'
)
if ($EulaId -ne '') { $wixArgs += @('-acceptEula', $EulaId) }
if ($Version -ne '') { $wixArgs += @('-d', "ProductVersion=$Version") }
if ($ProfileDir -ne '') { $wixArgs += @('-d', 'ProfileExtras=1') }
if ($FreshEnrolment) { $wixArgs += @('-d', 'FreshEnrolment=1') }
$wixArgs += @('-d', "StageDir=$Stage", '-o', $msi)

if (-not $wix) {
  Write-Host ""
  Write-Host "Build-Msi.ps1: no WiX compiler found - this host cannot build an MSI." -ForegroundColor Yellow
  Write-Host "Install WiX one of two ways, then re-run:"
  Write-Host "  1) standalone CLI, no .NET SDK: install wix-cli-x64.msi from"
  Write-Host "     https://github.com/wixtoolset/wix/releases"
  Write-Host "  2) .NET tool: 'dotnet tool install --global wix' -- needs the .NET SDK 6+;"
  Write-Host "     check with 'dotnet --list-sdks' (an empty list means only a runtime is installed)."
  Write-Host "     The tool lands in %USERPROFILE%\.dotnet\tools; this script looks there when 'wix' is not on PATH."
  Write-Host "The build it would run:"
  Write-Host "  wix $($wixArgs -join ' ')"
  exit 1
}

if ($Sign) {
  foreach ($exe in @('bin\capture-core.exe', 'bin\classifier-host.exe')) { Invoke-Sign (Join-Path $Stage $exe) }
}

# An argument array (splatted) keeps values that contain spaces as one argument on every PowerShell.
$buildOut = & $wix @wixArgs 2>&1
$buildCode = $LASTEXITCODE
$buildOut | ForEach-Object { Write-Host $_ }
if ($buildCode -ne 0) {
  throw "wix build failed with exit code $buildCode`n$($buildOut | Out-String)"
}
Write-Host "built $msi"

if ($Sign) {
  Invoke-Sign $msi
} else {
  Write-Host "unsigned MSI (signing is off; pass -Sign or set SAC_SIGN=1 for a signed release, docs/05-platform-delivery.md section 7)"
}

$beside = Join-Path $out $TenantFileName
if ($TenantEnv -ne "") {
  Copy-Item -Force (Resolve-Input $TenantEnv) $beside
  Write-Host "placed $TenantFileName beside the MSI"
} elseif (Test-Path $beside) {
  Remove-Item -Force $beside
  Write-Host "removed a stale $TenantFileName from $out"
}
