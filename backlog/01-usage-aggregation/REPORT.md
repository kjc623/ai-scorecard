# 01 — Usage aggregation · report

## What was built, and where

A new Go module, **`aggregation/aggregator`**, is the scheduled job that rolls
`ingest.submission`/`ingest.observation` up into the `mart` usage aggregates and writes
`ops.aggregate_watermark`. It is implemented exactly as docs/03 §5 and docs/04 §4 describe: every
bucket is replaced with `INSERT … ON CONFLICT DO UPDATE`, never incremented (brief C28).

| Path | What it is |
|---|---|
| `aggregation/aggregator/internal/rollup/rollup.go` | The five frozen upsert statements, the watermark upsert, the closed bucket set, and the window/last-complete-bucket arithmetic |
| `aggregation/aggregator/internal/rollup/runner.go` | One transaction per tenant; both bucket sizes; watermarks after each statement |
| `aggregation/aggregator/internal/rollup/*_test.go` | Unit tests (windows, upsert shape) and the live-PostgreSQL integration tests (shape, idempotency, late arrival, hour bucketing) |
| `aggregation/aggregator/cmd/aggregator/` | `main` (flags, loop, `--once`), the tagged pgx driver registration, and `/healthz`+`/readyz` |
| `aggregation/aggregator/Dockerfile`, `README.md` | The image and its documentation |
| `localdev/build.mjs` | Builds `sac/aggregator:lab-auth` in the `--auth` set |
| `localdev/authlab.compose.yaml` | Runs the aggregator as its own container, 30-second interval |
| `tools/verify-all.mjs` | Adds the module to the acceptance package list |
| `localdev/README.md` | Names the aggregator in the `--auth` build and the auth lab |

The `mart` tables written are `agg_tool_period`, `agg_tool_user_period`, `agg_class_period`,
`agg_org_period` and `agg_user_period`, plus one `ops.aggregate_watermark` row per
`(tenant, aggregate, bucket_size)`. `mart.agg_device_period` is deliberately **not** written here: it
rolls up `ops.collector_state`, not the event tables, and belongs to 02.

Design documents updated: docs/03 §5.4 and docs/04 §4.4 carry “As built” notes, and the closing note
in `database/schema.sql` now names the module. **No schema change** was needed — only a comment
changed — so an existing database is brought up to date by nothing at all; the job simply starts
writing rows into the tables and watermarks that were already defined.

## Why a new Go module

- It is a **job**, not a request-serving component: one transaction per tenant on a timer. A new
  module keeps that separate from the request paths and gives it its own image and lifecycle, which
  is what a scheduled container needs.
- It follows the established service pattern exactly: no third-party dependency in the default
  build, the PostgreSQL driver compiled only under `-tags sac_sql_driver` (pgx, already in the
  module cache and used by ingest-api, control-api and content-vault), `go test ./...` green
  offline. A Node implementation would have had to reuse or duplicate `query-api`'s private pg wire
  client, which is worse.
- The device simulator's own header already says “every `mart.*` aggregate come[s] from the
  aggregator, which is not built” — this is the component it was waiting for.

## What was verified, and how

**Unit tests** (`aggregation/aggregator`, `go test ./...`): the day/hour window aligns to the next
bucket and covers the lookback; `last_complete_bucket` excludes the open bucket; every aggregate is
an upsert that assigns `EXCLUDED.*` and never increments; the watermark is an upsert.

**Live PostgreSQL integration tests** (`internal/rollup`), run against the running auth lab server
(`sac-authlab-postgres-1`, discovered via `docker ps`; `SHADOWPG_CONTAINER` overrides). They drive
events through the real write path (`ingest.record_event`) and execute the frozen statements as
text with `PREPARE`/`EXECUTE`, all inside one transaction ending in `ROLLBACK`:

- **Shape.** three prompts count as three submissions with two distinct users and the right byte
  total; `usage_rollup` volume is excluded from `submissions` but counted in `rollup_events`; a
  detection-only tool appears with `submissions = 0` and `detections = 1`; class rows carry the
  right distinct-user count, `max_score`, and `degraded_events`; `ops.user_dim` being empty leaves
  `agg_org_period` empty.
- **Idempotent / re-runnable.** A second identical run produces byte-identical rows and the same
  row count. Tests `TestRollupAgainstPostgreSQL`, `TestRollupHourBucket`.
- **Late arrival.** A prompt flushed into an already-computed bucket is absorbed by the next
  recompute (submissions 3 → 4, users 2 → 3), and the bucket still has exactly one row.
- **Hour bucketing.** Two prompts in different hours land in two hour buckets, one each.

**On the device-auth lab, through the dashboard's own read path.** The aggregator container ran and
wrote 7,755 buckets and ten watermarks for tenant `1111…`. Querying through the dashboard's
forwarder (`http://host.docker.internal:8787/v1/query`, which adds the dev tenant header and calls
query-api) after seeding a multi-person fleet with the device simulator
(`node localdev/tools/simulate-devices.mjs --edge https://edge:8443 --tenant 1111… --devices 8 --events 1200`):

- **Tools** returns six tools with submission and people counts (`chatgpt_web` 296/10,
  `copilot_chat` 223/10, …) and a per-bucket series; freshness is `fresh`, “complete to
  2026-10-03”, and the `no_watermark_row` reason is gone.
- **Data classes** returns all seven classes with values and `degraded_events`
  (`payment_card` 87/9, `source_code` 86/8, …).
- **Users** returns a day series for `lab-user` (and for a simulator person), with an audit entry
  id, which is the subject-level read being audited.

## What could not be verified

- **A real browser.** The harness has no Chromium (`accept.mjs` skips the browser gate for that
  reason). I verified the dashboard's data path by calling the dashboard server's forwarder, which
  is the same code path the page uses, but I did not render the pages.
- **Production tenant enumeration.** The lab runs the job as `postgres`, which can enumerate
  `ops.tenant`. A production job running as `sac_ops` **cannot** list tenants under forced
  row-level security, so it must be given the tenants explicitly (`--tenant`, repeatable) or run as a
  role that can see them. This is stated in the module README and docs/03 rather than papered over.
- **Coverage.** Reads still report `coverage_degraded`, because `ops.coverage_snapshot` is empty.
  That is 02's work, not 01's; the freshness block is `fresh`, which is what this task owned.
- **Multi-tenant scale.** Only one tenant exists in the lab; the per-tenant transaction and the
  watermark logic are exercised, but not cross-tenant load.

## Decisions the owner should know about

1. **Prompts are selected by `ingest.submission.kind = 'prompt'`,** not by a semi-join on
   `ingest.observation`. `kind` was denormalised onto the submission for exactly this purpose
   (docs/04 §4.6), and it keeps rollup/detection volume out of every submission count.
2. **`agg_class_period.severity` falls back to `ref.data_class.default_severity`** when a label
   names no `ref.rule` row. A classifier label need not carry a rule, and `ref.rule` is empty in the
   lab (and on a real first deployment), so joining only to `ref.rule` would have produced **no**
   class rows — reporting no sensitive data where the classifier found some, which brief C21
   forbids.
3. **`agg_org_period.population` is written as the empty string for a NULL `ops.user_dim.population`.**
   The aggregate's primary key makes the column NOT NULL, so a NULL cannot be stored. The table is
   empty anyway until 06 syncs the directory; the read side reports unmapped users separately.
4. **Both `hour` and `day` buckets are recomputed every pass** (7-day and 48-hour lookbacks by
   default), matching docs/03 §5.4/§4.2. `last_complete_bucket` is the last bucket that has fully
   elapsed, so the open bucket a run also writes is not claimed as complete.
5. **The bucket grid comes from the calendar and the known dimension members,** then LEFT JOINs the
   source, so an emptied bucket is written as zero. A dimension member that disappears entirely
   (e.g. a subject fully erased) is only removed by a *targeted* bucket recompute, which is the
   erasure path in 13; the scheduled run does not guess which vanished member to delete.
6. **k-suppression is untouched.** The aggregates store distinct-subject counts per cell, and the
   read path suppresses cells below k = 5. This means a lab with one or two people shows
   `suppressed` rather than counts — correct behaviour, and the reason the verification fleet was
   seeded with the device simulator. I did **not** weaken k to make the pages prettier.
7. **One pre-existing build bug was fixed to make the documented lab build work:**
   `vault/content-vault/Dockerfile` did not copy `endpoint/protocol/`, which its `go.mod` replace
   directive and `internal/store/sql.go` require, so `node localdev/build.mjs --auth` failed before
   reaching any aggregator image. The one missing `COPY` line is the fix. It is outside this task's
   module and is called out here for review.

## Anything the owner must run by hand

**Nothing on the endpoint.** The agent needs no new build and no MSI change; this task is
server-side only.

To build and run the job by hand in the lab (the harness does **not** run `run.mjs --auth`, because
that would regenerate the CA and orphan the installed agent):

```
node localdev/build.mjs --auth
docker compose -f localdev/authlab.compose.yaml up -d aggregator
docker logs -f sac-authlab-aggregator-1
```

The image also runs one pass and exits with `--once`; the tagged binary refuses `--store sql` in a
default (untagged) build with a message naming the tag, as the other services do.

## Baseline test failures, and what this change did

`node tools/accept.mjs` was run after the change. My module's package passes. The failures below are
**not** in packages this task touched and are present on the host independent of it:

- `endpoint/classifier-host` — `parser/isolation` (`TestParseSendsOneDocumentAndGetsTypedTextBack`,
  `TestJobObjectCapBoundsAnAllocationBomb`); the documented Windows-host baseline category.
- `ingestion/ingest-api` — its `TestLive*` tests seed the fixed tenant `1111…`, which already exists
  in the running device-auth lab database, so they fail with a primary-key collision. This is the
  lab database the tests discover, not a defect in the aggregator (which uses its own tenant and
  rolls back).
- `seams` — `endpoint/protocol/content.go` names `policy_rule_id`, which the seam checker does not
  recognise; that line predates this change.
- `db` — `database/tools/run-invariants.ps1` is a PowerShell runner and produced no `TALLY` line on
  this Linux host.
