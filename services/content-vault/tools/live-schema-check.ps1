# Runs the classifier host's live-schema evidence? No — this is the content vault's.
#
# The live-PostgreSQL checks cannot run from inside `go test` on this build host: the DSH file
# sandbox denies a child process access to the Docker named pipe
# (npipe:////./pipe/dockerDesktopLinuxEngine), so the Go test that runs them skips with that exact
# reason and runs on a machine without that restriction. This harness is the same evidence produced
# from a shell that *can* reach the pipe, and it executes the statement text the service itself
# emits (`content-vault schema-sql`), not a copy of it.
#
# Usage:  pwsh -File tools/live-schema-check.ps1 [-Container shadowpg-invariants] [-Out evidence/live-schema.log]

param(
  [string]$Container = "shadowpg-invariants",
  [string]$Out = "",
  [string]$Docker = "C:\Program Files\Docker\Docker\resources\bin\docker.exe"
)

$ErrorActionPreference = "Stop"
# The script lives in services/content-vault/tools, so its parent is the module directory.
$module = Split-Path -Parent $PSScriptRoot
$repo = Split-Path -Parent (Split-Path -Parent $module)
if (-not $Out) { $Out = Join-Path $module "evidence\live-schema.log" }
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $Out) | Out-Null

function Invoke-Psql {
  param([string]$Script, [switch]$AllowFail)
  $tmp = New-TemporaryFile
  try {
    Set-Content -Path $tmp -Value $Script -Encoding utf8
    $previous = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    # PowerShell has no `<` redirection; piping into the process's stdin is the equivalent.
    $out = Get-Content -Raw -Path $tmp | & $Docker exec -i $Container psql -U postgres -d shadow -A -t -q -v ON_ERROR_STOP=1 -f - 2>&1
    $code = $LASTEXITCODE
    $ErrorActionPreference = $previous
    if ($code -ne 0 -and -not $AllowFail) {
      throw "psql failed (exit $code):`n$out"
    }
    return @{ Output = ($out -join "`n"); Code = $code }
  } finally {
    Remove-Item -Force $tmp -ErrorAction SilentlyContinue
  }
}

$log = New-Object System.Collections.Generic.List[string]
function Emit([string]$Line) { $log.Add($Line); Write-Output $Line }

Emit "== content-vault live-schema check $(Get-Date -Format o)"
Emit "container: $Container"
Emit "docker:    $Docker"

# 1. The server is the one the schema was applied to.
$probe = Invoke-Psql "SELECT count(*) FROM pg_tables WHERE schemaname='ops' AND tablename='content_object';"
Emit "ops.content_object present: $(($probe.Output).Trim())"

# 2. Every statement the service issues, prepared against the live schema.
$env:GOCACHE = Join-Path $repo ".tools\gocache"
$env:GOPROXY = "off"; $env:GOTOOLCHAIN = "local"; $env:GOFLAGS = "-mod=mod"
Push-Location $module
# The intermediate script goes inside the workspace: the file sandbox denies a child process a
# system-temp path, and a harness that fails for that reason reports "no evidence" for something
# that has nothing to do with the SQL.
$evidenceDir = Join-Path $module "evidence"
New-Item -ItemType Directory -Force -Path $evidenceDir | Out-Null
$sqlFile = Join-Path $evidenceDir "_schema.sql"
go run ./cmd/content-vault schema-sql --out $sqlFile
if ($LASTEXITCODE -ne 0) { throw "content-vault schema-sql failed" }
$statements = Get-Content -Raw $sqlFile
Pop-Location

$prepared = Invoke-Psql $statements
Emit "statement PREPARE/DEALLOCATE: OK ($(($statements -split "PREPARE ").Count - 1) statements)"

# 3. ADR 0014's mutually exclusive pair is refused by the database.
$impossible = @"
BEGIN;
INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, kek_id, ceiling_mode, content_search)
VALUES ('99999999-9999-4999-8999-999999999991','impossible','active','eastus','customer_held','kek-x','m3','full_text');
ROLLBACK;
"@
$r = Invoke-Psql $impossible -AllowFail
if ($r.Code -eq 0) { throw "the database ACCEPTED full_text with customer_held" }
if ($r.Output -notmatch "tenant_full_text_search_requires_vendor_readable_content") { throw "refused for the wrong reason: $($r.Output)" }
Emit "ADR 0014 pair refused by: tenant_full_text_search_requires_vendor_readable_content"

# 4. A search tier over a collection mode the tenant cannot reach is refused.
$tierMode = @"
BEGIN;
INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, ceiling_mode, content_search)
VALUES ('99999999-9999-4999-8999-999999999992','tier','active','eastus','vendor','m1','full_text');
ROLLBACK;
"@
$r = Invoke-Psql $tierMode -AllowFail
if ($r.Code -eq 0) { throw "the database ACCEPTED full_text below M3" }
if ($r.Output -notmatch "tenant_search_tier_requires_collection_mode") { throw "refused for the wrong reason: $($r.Output)" }
Emit "full_text below M3 refused by: tenant_search_tier_requires_collection_mode"

# 5. The content-object lifecycle: store, re-wrap, shred — inside a rolled-back transaction.
$lifecycle = @"
BEGIN;
INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, kek_id, ceiling_mode, content_search)
VALUES ('99999999-9999-4999-8999-999999999993','lifecycle','active','eastus','vendor','kek-lifecycle','m3','disabled');
INSERT INTO ops.content_object (tenant_id, object_id, submission_id, event_id, blob_path, ciphertext_sha256,
                                plaintext_size_bytes, wrapped_dek, kek_id, kek_version, retention_class, state, created_at, expires_at)
VALUES ('99999999-9999-4999-8999-999999999993','99999999-9999-4999-8999-99999999999a',NULL,NULL,'tenants/x/objects/y',
        'sha256:$(('ab' * 32))',4096,'\x0102030405'::bytea,'kek-lifecycle','1','standard','uploaded',now(),now()+interval '90 days');
UPDATE ops.content_object SET wrapped_dek='\x0a0b0c'::bytea, kek_version='2'
 WHERE tenant_id='99999999-9999-4999-8999-999999999993' AND object_id='99999999-9999-4999-8999-99999999999a'
   AND kek_version='1' AND state='uploaded';
UPDATE ops.content_object SET state='shredded', shredded_reason='erasure', shredded_at=now(), wrapped_dek='\x00'::bytea
 WHERE tenant_id='99999999-9999-4999-8999-999999999993' AND object_id='99999999-9999-4999-8999-99999999999a' AND state='uploaded';
SELECT state||'/'||shredded_reason||'/'||kek_version||'/'||encode(wrapped_dek,'hex')||'/'||(expires_at>created_at)::text
  FROM ops.content_object WHERE tenant_id='99999999-9999-4999-8999-999999999993' AND object_id='99999999-9999-4999-8999-99999999999a';
ROLLBACK;
"@
$r = Invoke-Psql $lifecycle
$line = ($r.Output -split "`n" | Where-Object { $_ -match "^(uploaded|shredded)/" } | Select-Object -Last 1)
if ($line -ne "shredded/erasure/2/00/true") { throw "lifecycle row is '$line', want shredded/erasure/2/00/true" }
Emit "content_object lifecycle: $line"

# 6. ops.retrieval_grant (landed by db): put, read, claim, a second claim, and the trigger that
#    makes single use structural rather than caller-dependent. Rolled back.
$grants = @"
BEGIN;
INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, kek_id, ceiling_mode, content_search)
VALUES ('99999999-9999-4999-8999-999999999995','grants','active','eastus','vendor','kek-g','m3','disabled');
INSERT INTO ops.retrieval_grant (tenant_id, grant_id, event_id, object_id, submission_id, principal,
                                 case_reference, second_approver, issued_at, expires_at, raw_digest)
VALUES ('99999999-9999-4999-8999-999999999995','99999999-9999-4999-8999-99999999999d',
        '99999999-9999-4999-8999-99999999999e','99999999-9999-4999-8999-99999999999f',
        '99999999-9999-4999-8999-9999999999a0','analyst@example.com','CASE-1','approver@example.com',
        now(), now() + interval '5 minutes', 'sha256:$(('ab' * 32))');
-- The claim the service sends.
UPDATE ops.retrieval_grant SET used_at = now(), used_by = 'analyst@example.com'
 WHERE tenant_id='99999999-9999-4999-8999-999999999995'
   AND grant_id='99999999-9999-4999-8999-99999999999d' AND used_at IS NULL
RETURNING (used_at IS NOT NULL AND used_by IS NOT NULL)::text;
-- A second claim through the same guarded statement matches no row.
WITH claimed AS (
  UPDATE ops.retrieval_grant SET used_at = now(), used_by = 'someone.else@example.com'
   WHERE tenant_id='99999999-9999-4999-8999-999999999995'
     AND grant_id='99999999-9999-4999-8999-99999999999d' AND used_at IS NULL
  RETURNING 1)
SELECT count(*) FROM claimed;
ROLLBACK;
"@
$r = Invoke-Psql $grants
if ($r.Output -notmatch "true" -or $r.Output -notmatch "0") { throw "put/claim/claim-again evidence missing: $($r.Output)" }
Emit "ops.retrieval_grant: put + claim OK (both used halves set), second guarded claim matched 0 rows"

# 7. An unguarded UPDATE of a redeemed grant is refused by db's trigger.
$unguarded = @"
BEGIN;
INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, kek_id, ceiling_mode, content_search)
VALUES ('99999999-9999-4999-8999-999999999996','g2','active','eastus','vendor','kek-g2','m3','disabled');
INSERT INTO ops.retrieval_grant (tenant_id, grant_id, event_id, object_id, submission_id, principal,
                                 case_reference, second_approver, issued_at, expires_at, used_at, used_by, raw_digest)
VALUES ('99999999-9999-4999-8999-999999999996','99999999-9999-4999-8999-9999999999b1',
        '99999999-9999-4999-8999-99999999999e','99999999-9999-4999-8999-99999999999f',
        '99999999-9999-4999-8999-9999999999a0','analyst@example.com','CASE-1','approver@example.com',
        now(), now() + interval '5 minutes', now(), 'analyst@example.com', 'sha256:$(('ab' * 32))');
UPDATE ops.retrieval_grant SET used_by = 'attacker@example.com'
 WHERE tenant_id='99999999-9999-4999-8999-999999999996' AND grant_id='99999999-9999-4999-8999-9999999999b1';
ROLLBACK;
"@
$r = Invoke-Psql $unguarded -AllowFail
if ($r.Code -eq 0) { throw "an unguarded UPDATE of a redeemed grant was ACCEPTED" }
Emit "unguarded UPDATE of a redeemed grant refused by the retrieval_grant_single_use trigger"

Emit "== all live-schema checks passed"
Set-Content -Path $Out -Value ($log -join "`n") -Encoding utf8
Write-Output "written: $Out"
