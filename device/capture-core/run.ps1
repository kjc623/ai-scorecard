# Device-side build/test entry point for capture-core.
#
# There is no dependency on any network: GOPROXY=off, GOTOOLCHAIN=local, and the module requirements
# are local replaces only (device/protocol, device/capture-spool, device/canon). Dot-sourcing
# .tools\env.ps1 is blocked by execution policy on the build host, so the prefix is set here instead.
#
# Usage (from the repository root, because .ps1 execution needs the bypass on this host):
#   powershell -NoProfile -ExecutionPolicy Bypass -File device\capture-core\run.ps1
#   ... -Selftest                 # also build the binary and run the end-to-end self test
#   ... -Test core                # one package
#   ... -Race                     # with the race detector (slower)

param(
    [string]$Test = "./...",
    [switch]$Race,
    [switch]$SkipVet,
    # Run cmd/capture-core --selftest after the unit tests. It is the only end-to-end check of the
    # assembled endpoint (service + native host + spool + classifier link + shutdown ordering), so it
    # is worth the extra seconds whenever the device tier is touched.
    [switch]$Selftest,
    [switch]$SkipBuildBinary
)

$ErrorActionPreference = "Stop"

# Resolve the repository root from this script's location, so the script works from anywhere.
$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$moduleDir = $PSScriptRoot

$env:GOCACHE = Join-Path $root ".tools\gocache"
$env:GOPROXY = "off"
$env:GOTOOLCHAIN = "local"
$env:GOFLAGS = "-mod=mod"

Push-Location $moduleDir
try {
    Write-Output "=== gofmt -l . ==="
    $unformatted = & gofmt -l .
    if ($unformatted) {
        Write-Output $unformatted
        throw "gofmt reports unformatted files"
    }
    Write-Output "(clean)"

    Write-Output "=== go build ./... ==="
    & go build ./...
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }

    if (-not $SkipVet) {
        Write-Output "=== go vet ./... ==="
        & go vet ./...
        if ($LASTEXITCODE -ne 0) { throw "go vet failed" }
    }

    Write-Output "=== go test $Test ==="
    if ($Race) {
        & go test -race -count=1 -timeout 300s $Test
    } else {
        & go test -count=1 -timeout 300s $Test
    }
    if ($LASTEXITCODE -ne 0) { throw "go test failed" }

    $binary = Join-Path $moduleDir "bin\capture-core.exe"
    if (-not $SkipBuildBinary) {
        Write-Output "=== go build -o bin\capture-core.exe ./cmd/capture-core ==="
        New-Item -ItemType Directory -Force -Path (Join-Path $moduleDir "bin") | Out-Null
        & go build -o $binary ./cmd/capture-core
        if ($LASTEXITCODE -ne 0) { throw "building the capture-core binary failed" }

        Write-Output "=== capture-core --version ==="
        & $binary --version
        if ($LASTEXITCODE -ne 0) { throw "capture-core --version failed" }
    }

    if ($Selftest) {
        Write-Output "=== capture-core --selftest (end-to-end: service, native host, spool, shutdown) ==="
        # The selftest creates its own bundle, spool, classifier peer and ports, and removes its work
        # directory on exit — success or failure. Non-zero means an assertion failed.
        & $binary --selftest
        if ($LASTEXITCODE -ne 0) { throw "capture-core --selftest failed ($LASTEXITCODE)" }
    }

    Write-Output "=== ok ==="
}
finally {
    Pop-Location
}
