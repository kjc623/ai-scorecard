# Baseline

The harness and the test suites before any backlog task's work. `ENVIRONMENT.md` says what to run;
this file says what happened and what already fails. A later task reports only the difference from
this file.

## What was measured

- Date: 2026-10-05.
- Branch and commit: `backlog/00-environment-check`, cut from `main` at
  `78e85d8f2be360801b20b12bb4f46bc02fb9b559`.
- Harness: container `sac-harness-opencode-run-4e373535e5fc`, image
  `sha256:46459effb998ae5bc382399d00666da53274c2448b8b6db44418648a48cc922f`
  (`sac/harness-opencode`), Fedora Linux 44, Node v22.23.1, go1.27.1, PostgreSQL 18.6 client.
- Lab: the device-auth lab (`localdev/authlab.compose.yaml`) up and healthy. Every image was
  rebuilt with `node localdev/build.mjs --auth` and both dashboards from
  `query/dashboard/Dockerfile`, then the services and dashboards were recreated; the lab's CA
  volume was untouched. At measurement the owner's tenant (`11111111-…`) held 1373 submissions,
  1711 observations and 10 devices; the sample tenant (`5a3c0de0-…`) held 201 submissions, 240
  observations, 8 devices and 48 collector-state rows before the simulator ran, and 406 / 481 / 8 /
  48 after.

## Suites and gates that fail before any work

"Group" is the task's step-4 classification. Both failing Go suites were run a second time; the
second run was identical, so every failure below is deterministic.

| Suite / gate | Failing tests | Group | Cause |
|---|---|---|---|
| `endpoint/classifier-host` (`parser/isolation`) | `TestParseSendsOneDocumentAndGetsTypedTextBack`, `TestJobObjectCapBoundsAnAllocationBomb` | needs something the harness does not have | Both assert the child's peak memory is reported after the child has exited. On Windows that is the job object's peak; on Fedora `enforcer.peak()` re-reads `/proc/<pid>/statm`, which is gone once the child has been reaped, and returns 0. There is no job object on Linux, and the second test asks for `JOB_OBJECT_LIMIT_PROCESS_MEMORY` by name. |
| `ingestion/ingest-api` (`internal/store`) | `TestLiveRecordEventRunsTheLadder`, `TestLivePrincipalStatusRunsTheStatement`, `TestLiveDPoPReplayIsOneShot`, `TestLiveQuarantineInsertUsesTheLiveVocabulary`, `TestLiveM0BoundaryIsAlsoAConstraint` | collides with the running lab | Each seeds a fixture tenant with `ladder.TenantID` = `11111111-1111-1111-1111-111111111111`, which is the owner's tenant and already exists, so the seed fails `duplicate key value violates unique constraint "tenant_pkey"`. The suite discovers any running Postgres container; here that is the live lab's `sac-authlab-postgres-1` (no scratch `shadowpg` is running). |
| `node tools/accept.mjs`, gate `seams` | the whole gate | the code is wrong | `tools/check-seams.mjs` reports `endpoint/protocol/content.go:63` naming `policy_rule_id`, which is not in the event-envelope contract. The field is on `ContentGrantRequest` (`POST /v1/content/grant`), not on the envelope, so the checker is reading a different wire message; whether the field or the checker changes is the owner's call. |
| `node tools/accept.mjs`, gate `db` | the whole gate | needs something the harness does not have | The gate runs `database/tools/run-invariants.ps1` with `powershell`, which is not installed on Fedora. `spawnSync` fails, no TALLY line is found, and the gate reports FAIL rather than SKIP (unlike the `browser` gate, which skips when no Windows browser is present). |

## Suites that pass

Counts are top-level tests from the run of 2026-10-05.

Node packages, run as `ENVIRONMENT.md` says (`node --test` inside the directory, no argument):

| Package | Tests | Pass | Skip | Fail |
|---|---|---|---|---|
| `query/dashboard` | 145 | 139 | 6 | 0 |
| `query/query-api` | 214 | 199 | 15 | 0 |

The other Node packages run by `tools/verify-all.mjs` / `tools/accept.mjs`: `contracts/tools` 12
pass, `extension` 209 pass, `database/tools` 41 pass, `azure/tools` 41 pass.

Go modules, run as `go test ./...` (top-level PASS lines):

| Module | Pass | Fail |
|---|---|---|
| `endpoint/protocol` | 33 | 0 |
| `endpoint/canon` | 17 | 0 |
| `endpoint/integration` | 13 | 0 |
| `endpoint/capture-core` | 239 | 0 |
| `endpoint/capture-spool` | 34 | 0 |
| `endpoint/classifier-host` | 85 | 2 |
| `ingestion/ingest-api` | 105 | 5 |
| `control/control-api` | 32 | 0 |
| `vault/content-vault` | 48 | 0 |
| `aggregation/aggregator` | 16 | 0 |
| `vault/content-vault/vaultinvariants` | 6 | 0 |
| `contracts/generated/go` | build-only, no tests | — |

`tools/accept.mjs` gates: `packages` FAIL (the two suites above), `contract` PASS, `seams` FAIL,
`vocab` PASS, `installer` PASS, `invariants` PASS, `endpoint` PASS (26 assertions, 0 failures),
`browser` SKIP (it looks for a Windows Edge/Chrome path), `db` FAIL. Verdict: FAILED.

## Run-to-run differences

None observed. The first `tools/verify-all.mjs` run printed only two of the five `internal/store`
failures because it tails each package's output to 40 lines, but the package failed with five in
that run too, as the direct re-run shows. Nothing passed on one run and failed on another.

One count difference to be aware of: `node --test` with no argument (what `ENVIRONMENT.md` says)
loads every file under a `test/` directory, including `test/helpers.mjs`, so it counts one more
"test" per package than `tools/verify-all.mjs`, which passes the explicit `*.test.*` files. The
pass and skip counts agree.

## Other observations

- The `endpoint/classifier-host` suite rewrites the tracked files under
  `endpoint/classifier-host/reports/`. Task 00 restored them with `git checkout`; a later task that
  runs that suite will see the same drift.
- Re-running `simulate-devices.mjs --devices 8` into the sample tenant on 2026-10-05 added 241
  observations (to 406 submissions) but no device rows: the hardware-identity hash is fixed per
  index, so the same eight devices are re-enrolled. The owner's tenant counts and submission digest
  were unchanged.
