<#
=====================================================================================
database/tools/run-invariants.ps1 -- run the schema's invariants against a real server
=====================================================================================
Applies database/schema.sql then database/invariants.test.sql using exactly the command documented in
.cockpit/project.json:

    psql -v ON_ERROR_STOP=1 -f database/schema.sql && psql -v ON_ERROR_STOP=1 -f database/invariants.test.sql

and reports the assertion tally.

WHY THIS SCRIPT EXISTS AND WHY IT IS NOT JUST THAT ONE LINE
-----------------------------------------------------------
database/schema.sql creates its seven roles with bare `CREATE ROLE`. Roles are cluster-scoped, not
database-scoped, so dropping and recreating the database is NOT enough to re-run the file: the
second run fails at line 77 with `role "sac_owner" already exists`. The documented one-liner is
therefore correct for a fresh cluster and not re-runnable on a used one. This script performs
the documented command but resets that cluster-scoped state first, so a re-run is honest
rather than a false failure.

It also does not paper over anything: the point of the run is that the assertions execute as the
runtime roles. The *bootstrap* session is a superuser because ops.tenant's own row-level policy
forbids a runtime role from creating a tenant -- that is the migration path. The isolation
assertions themselves run under `SET ROLE sac_ingest`, which has neither SUPERUSER nor
BYPASSRLS. database/tools/probe-rls-nonsuperuser.sql independently proves that the isolation holds
even when the session_user is itself a non-superuser, and that a superuser sees different rows.

USAGE
-----
This host has Windows PowerShell 5.1 only (no pwsh), and its execution policy blocks script
files, so the working invocation is:

    powershell -NoProfile -ExecutionPolicy Bypass -File database/tools/run-invariants.ps1
    powershell -NoProfile -ExecutionPolicy Bypass -File database/tools/run-invariants.ps1 -Port 55434
    powershell -NoProfile -ExecutionPolicy Bypass -File database/tools/run-invariants.ps1 -Local -Port 5432

On a host with PowerShell 7 and a permissive policy, `pwsh database/tools/run-invariants.ps1` works
unchanged. On a host with no network, -Image is omitted and the highest cached postgres:* tag
is used, which is the path this repository was verified on.

Exit codes: 0 all assertions passed, 1 an assertion failed, 2 the harness could not run
(no server, no artifacts).
#>

[CmdletBinding()]
param(
    # Container image to use. Empty = pick the first locally cached postgres:* image, which
    # is the only option on a host with no network access.
    [string] $Image = '',
    [string] $Container = 'shadowpg-invariants',
    [int]    $Port = 55434,
    [string] $Database = 'shadow',
    [string] $SuperUser = 'postgres',
    [string] $Password = 'shadowpw',

    # Use a PostgreSQL server already reachable from this host instead of starting a container.
    [switch] $Local,
    [string] $HostName = '127.0.0.1',

    # Leave the container running after the run (useful for follow-up queries by hand).
    [switch] $KeepContainer,

    # Where to write the raw logs. Defaults to database/evidence.
    [string] $EvidenceDir = ''
)

# Windows PowerShell 5.1 raises a *terminating* error when a native command writes to stderr and
# $ErrorActionPreference is 'Stop' -- and psql writes NOTICEs to stderr, so a perfectly healthy
# run would abort here. Every native call below therefore has its exit code checked explicitly
# ($LASTEXITCODE / Die) instead of relying on the preference variable.
$ErrorActionPreference = 'Continue'

function Say  { param([string]$m) Write-Host $m }
function Head { param([string]$m) Write-Host ''; Write-Host "== $m" }
function Die  { param([string]$m) Write-Host "run-invariants: $m" -ForegroundColor Red; exit 2 }

# -------------------------------------------------------------------------------------
# 0. Artifacts
# -------------------------------------------------------------------------------------

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$schemaPath = Join-Path $repoRoot 'database\schema.sql'
$testsPath  = Join-Path $repoRoot 'database\invariants.test.sql'
if (-not $EvidenceDir) { $EvidenceDir = Join-Path $repoRoot 'database\evidence' }

foreach ($p in @($schemaPath, $testsPath)) {
    if (-not (Test-Path -LiteralPath $p)) { Die "missing artifact: $p" }
}

$schemaHash = (Get-FileHash -LiteralPath $schemaPath -Algorithm SHA256).Hash
$testsHash  = (Get-FileHash -LiteralPath $testsPath  -Algorithm SHA256).Hash

$stamp = Get-Date -Format 'yyyy-MM-dd-HHmmss'
New-Item -ItemType Directory -Force -Path $EvidenceDir | Out-Null

Say "database/tools/run-invariants.ps1"
Say "  repo        : $repoRoot"
Say "  schema.sql  : sha256 $schemaHash"
Say "  tests.sql   : sha256 $testsHash"
Say "  evidence    : $EvidenceDir"

# -------------------------------------------------------------------------------------
# 1. Locate a real server
# -------------------------------------------------------------------------------------

$mode = $null

if ($Local) {
    if (-not (Get-Command psql -ErrorAction SilentlyContinue)) {
        Die "-Local was passed but psql is not on PATH."
    }
    $mode = 'local'
    Say "  server      : local PostgreSQL at ${HostName}:${Port} (psql on PATH)"

    $env:PGPASSWORD = $Password
    $probe = & psql -h $HostName -p $Port -U $SuperUser -d postgres -tAc "select version();" 2>&1
    if ($LASTEXITCODE -ne 0) { Die "cannot reach a PostgreSQL server at ${HostName}:${Port}: $probe" }
    $version = $probe
    Say "  version     : $($version -join ' ')"
}
else {
    if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
        Die "docker is not on PATH. Pass -Local to use an existing server, or install one."
    }

    & docker version --format '{{.Server.Version}}' *> $null
    if ($LASTEXITCODE -ne 0) { Die "the docker daemon is not responding. Start Docker, or pass -Local." }

    if (-not $Image) {
        # No network on this host: an already-cached image is the only way to get a server.
        $cached = @(& docker images --format '{{.Repository}}:{{.Tag}}' |
                    Where-Object { $_ -match '^postgres:' } |
                    Sort-Object -Property { if ($_ -match '(\d+)') { -[int]$Matches[1] } else { 0 } })
        if ($cached.Count -eq 0) {
            Die "no postgres:* image is cached locally and there is no network to pull one. Run 'docker images' to confirm, then either load an image tar or pass -Local."
        }
        $Image = $cached[0]
        Say "  image       : $Image (highest cached postgres tag; no network present)"
    }
    else {
        & docker image inspect $Image *> $null
        if ($LASTEXITCODE -ne 0) { Die "image '$Image' is not available locally and cannot be pulled without network." }
        Say "  image       : $Image"
    }

    $mode = 'docker'

    $exists = & docker ps -a --filter "name=^/$Container$" --format '{{.Names}}'
    if ($exists) {
        Say "  container   : $Container (reusing)"
        $running = & docker ps --filter "name=^/$Container$" --format '{{.Names}}'
        if (-not $running) { & docker start $Container *> $null }
    }
    else {
        Say "  container   : $Container (creating)"
        & docker run -d --name $Container -e "POSTGRES_PASSWORD=$Password" -e "POSTGRES_USER=$SuperUser" -p "${Port}:5432" $Image *> $null
        if ($LASTEXITCODE -ne 0) { Die "docker run failed for image $Image" }
    }

    # Wait for readiness rather than sleeping a fixed amount.
    $ready = $false
    for ($i = 0; $i -lt 60; $i++) {
        $out = & docker exec $Container pg_isready -U $SuperUser 2>&1
        if ($out -match 'accepting connections') { $ready = $true; break }
        Start-Sleep -Milliseconds 500
    }
    if (-not $ready) { Die "the server in $Container did not become ready within 30s" }

    $version = & docker exec -u $SuperUser $Container psql -U $SuperUser -tAc 'select version();'
    Say "  version     : $($version -join ' ')"
}

$kind = if ($mode -eq 'docker') { 'docker' } else { 'local' }

# -------------------------------------------------------------------------------------
# 2. Apply the documented command, capturing raw output
# -------------------------------------------------------------------------------------

Head "applying the documented test command"

$logSchema = Join-Path $EvidenceDir "$stamp-schema-apply.log"
$logTests  = Join-Path $EvidenceDir "$stamp-invariants.log"

if ($mode -eq 'docker') {
    # Ship the artifacts and a runner script into the container, so the command that runs is
    # byte-identical to the one in .cockpit/project.json and no host quoting can alter it.
    & docker exec $Container mkdir -p /database *> $null
    & docker cp (Join-Path $repoRoot 'database\.') "${Container}:/database" *> $null
    if ($LASTEXITCODE -ne 0) { Die "docker cp of database/ into $Container failed" }

    # In-container hashes: proves the file that ran is the file that was reviewed.
    $inHashes = & docker exec $Container sha256sum /database/schema.sql /database/invariants.test.sql
    Say "  in-container: $($inHashes -join ' | ')"

    $runner = @'
set -u
DB="$DBNAME"
LOG1=/tmp/01-schema-apply.log
LOG2=/tmp/02-invariants-run.log

echo "### reset: database plus the cluster-scoped sac_* roles"
psql -U postgres -d postgres -q -v ON_ERROR_STOP=1 \
  -c "DROP DATABASE IF EXISTS $DB WITH (FORCE);" \
  -c "DROP ROLE IF EXISTS sac_owner, sac_migrator, sac_ingest, sac_control, sac_vault, sac_query, sac_ops;" \
  -c "CREATE DATABASE $DB;" > /tmp/reset.log 2>&1 || { cat /tmp/reset.log; exit 9; }

echo "### psql -v ON_ERROR_STOP=1 -f database/schema.sql"
PGDATABASE=$DB psql -U postgres -v ON_ERROR_STOP=1 -f /database/schema.sql > $LOG1 2>&1
SCHEMA_EXIT=$?
echo "schema_exit=$SCHEMA_EXIT"

echo "### psql -v ON_ERROR_STOP=1 -f database/invariants.test.sql"
PGDATABASE=$DB psql -U postgres -v ON_ERROR_STOP=1 -f /database/invariants.test.sql > $LOG2 2>&1
TESTS_EXIT=$?

# Make each log self-describing: a raw psql log with no record of which revision produced it
# is not evidence, it is an anecdote.
SCHEMA_SHA=$(sha256sum /database/schema.sql | cut -d' ' -f1)
TESTS_SHA=$(sha256sum /database/invariants.test.sql | cut -d' ' -f1)
SERVER=$(psql -U postgres -tAc 'select version();')

header() {
  echo "# command : psql -v ON_ERROR_STOP=1 -f database/schema.sql && psql -v ON_ERROR_STOP=1 -f database/invariants.test.sql"
  echo "# schema  : database/schema.sql sha256 $SCHEMA_SHA"
  echo "# tests   : database/invariants.test.sql sha256 $TESTS_SHA"
  echo "# server  : $SERVER"
  echo "# database: $DB   captured: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "#"
}
header > /tmp/01-headed.log && cat $LOG1 >> /tmp/01-headed.log && mv /tmp/01-headed.log $LOG1
header > /tmp/02-headed.log && cat $LOG2 >> /tmp/02-headed.log && mv /tmp/02-headed.log $LOG2

# The tally is deliberately NOT computed here. It lives in database/tools/tally-log.mjs so it can be
# unit-tested against captured logs: the false positive that made a healthy run exit 1 was an
# inline grep that no test could reach, and it survived until a second person ran the script.
# This script runs psql and reports exit codes; the host side does the judging.
echo "EXITS schema=$SCHEMA_EXIT invariants=$TESTS_EXIT"
'@

    $tmp = Join-Path ([System.IO.Path]::GetTempPath()) "run-invariants-$stamp.sh"
    Set-Content -LiteralPath $tmp -Value $runner -Encoding ascii -NoNewline
    & docker cp $tmp "${Container}:/tmp/run-invariants.sh" *> $null
    Remove-Item -LiteralPath $tmp -Force

    $raw = & docker exec -u $SuperUser -e "DBNAME=$Database" $Container sh /tmp/run-invariants.sh 2>&1
    $raw | ForEach-Object { Say "  $_" }

    & docker cp "${Container}:/tmp/01-schema-apply.log" $logSchema *> $null
    & docker cp "${Container}:/tmp/02-invariants-run.log" $logTests  *> $null

    $schemaExit = if ("$raw" -match 'EXITS schema=(\d+)') { [int]$Matches[1] } else { 9 }
    $testsExit  = if ("$raw" -match 'invariants=(\d+)')     { [int]$Matches[1] } else { 9 }
}
else {
    $env:PGPASSWORD = $Password
    # Same reset, for the same reason: CREATE ROLE is cluster-scoped.
    & psql -h $HostName -p $Port -U $SuperUser -d postgres -q -v ON_ERROR_STOP=1 `
        -c "DROP DATABASE IF EXISTS $Database WITH (FORCE);" `
        -c "DROP ROLE IF EXISTS sac_owner, sac_migrator, sac_ingest, sac_control, sac_vault, sac_query, sac_ops;" `
        -c "CREATE DATABASE $Database;" *> $null
    if ($LASTEXITCODE -ne 0) { Die "could not reset the database/roles on ${HostName}:${Port}" }

    & psql -h $HostName -p $Port -U $SuperUser -d $Database -v ON_ERROR_STOP=1 -f $schemaPath *> $logSchema
    $schemaExit = $LASTEXITCODE
    & psql -h $HostName -p $Port -U $SuperUser -d $Database -v ON_ERROR_STOP=1 -f $testsPath  *> $logTests
    $testsExit = $LASTEXITCODE
}

# -------------------------------------------------------------------------------------
# 3. Tally
# -------------------------------------------------------------------------------------

Head "tally"

if (-not (Test-Path -LiteralPath $logTests)) { Die "the invariants log was not produced; the run did not complete" }

$testLog = Get-Content -LiteralPath $logTests -Raw

# Judge through the tested module rather than inline pattern matching. Its rules are covered by
# database/tools/tally-log.test.mjs against captured logs, including the exact log that the old inline
# `grep -ci ERROR` mis-counted because the provenance header quotes ON_ERROR_STOP.
$tallyScript = Join-Path $PSScriptRoot 'tally-log.mjs'
if (-not (Get-Command node -ErrorAction SilentlyContinue)) {
    Die "node is not on PATH. database/tools/tally-log.mjs computes the tally and is the tested implementation; install Node rather than trusting an inline fallback."
}
$tallyRaw = & node $tallyScript $logTests $logSchema 2>&1
if ($LASTEXITCODE -ne 0) { Die "tally-log.mjs failed: $($tallyRaw -join ' ')" }
$t = ($tallyRaw | Select-Object -First 1) | ConvertFrom-Json
$tallyLine = ($tallyRaw | Select-Object -Last 1)

$passCount = [int]$t.pass
$distinct  = [int]$t.distinct
$failCount = [int]$t.fail
$errCount  = [int]$t.errorLines

# tools/accept.mjs parses exactly this line, so its shape is a contract. It is printed unwrapped.
Write-Host $tallyLine

Say "  schema exit code     : $schemaExit"
Say "  invariants exit code : $testsExit"
Say "  PASS notices         : $passCount"
Say "  distinct assertions  : $distinct"
Say "  FAIL markers         : $failCount"
Say "  ERROR lines          : $errCount"
Say "  logs                 : $logSchema"
Say "                         $logTests"

$verdict = ($schemaExit -eq 0) -and ($testsExit -eq 0) -and ($failCount -eq 0) -and ($errCount -eq 0)

Say ''
if ($verdict) {
    Say "  RESULT: PASS -- $distinct assertions (T1..T$distinct), $passCount PASS notices, 0 failures." -ForegroundColor Green
    Say "          Applied to: $($version -join '')"
    Say "          Target from ADR 0002 is PostgreSQL 16; check the version line above and record any"
    Say "          deviation. Nothing in this harness substitutes for the real server run."
}
else {
    Say "  RESULT: FAIL -- see $logTests" -ForegroundColor Red
}

if ($mode -eq 'docker' -and -not $KeepContainer) {
    Say ''
    Say "  (container '$Container' left running for inspection; remove with: docker rm -f $Container)"
}

if (-not $verdict) { exit 1 }
exit 0
