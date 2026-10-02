# Shadow AI Capture — offline build environment (this machine)
#
# The build host has NO outbound network (verified 2026-10-02: TLS egress fails,
# registry.npmjs.org and proxy.golang.org are unreachable from confined commands).
# The Go build cache also lives outside the writable workspace, so it must be moved
# inside it. Dot-source this file before any `go` or `node` command:
#
#   . .tools\env.ps1
#   go test ./...
#
# Nothing here changes what the code does; it only points the toolchain at paths that
# are writable inside this workspace.

$repoRoot = Split-Path -Parent $PSScriptRoot          # .tools\env.ps1 -> repo root
$env:GOCACHE   = Join-Path $repoRoot '.tools\gocache'
$env:GOPATH    = Join-Path $repoRoot '.tools\gopath'
$env:GOMODCACHE = Join-Path $env:GOPATH 'pkg\mod'
$env:GOTMPDIR  = Join-Path $repoRoot '.tools\tmp\go'
$env:GOPROXY   = 'off'          # no network: fail loudly instead of hanging
$env:GOFLAGS   = '-mod=mod'
$env:GOTOOLCHAIN = 'local'      # never try to download a toolchain

foreach ($d in @($env:GOCACHE, $env:GOPATH, $env:GOTMPDIR)) {
    if (-not (Test-Path $d)) { New-Item -ItemType Directory -Force -Path $d | Out-Null }
}

Write-Host "env: GOCACHE=$env:GOCACHE GOPROXY=off GOTOOLCHAIN=local (offline build host)" -ForegroundColor DarkGray
