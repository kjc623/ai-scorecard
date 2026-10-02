# Device-side build/test entry point for capture-core.
#
# There is no dependency on any network: GOPROXY=off, GOTOOLCHAIN=local, and the only module
# requirement is the local replace of device/protocol. Dot-sourcing .tools\env.ps1 is blocked by
# execution policy on the build host, so the prefix is set here instead.
#
# Usage:
#   pwsh -File device\capture-core\run.ps1            # build, vet, test
#   pwsh -File device\capture-core\run.ps1 -Test core # one package
#   pwsh -File device\capture-core\run.ps1 -Race      # with the race detector (slower)

param(
    [string]$Test = "./...",
    [switch]$Race,
    [switch]$SkipVet
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
    Write-Output "=== ok ==="
}
finally {
    Pop-Location
}
