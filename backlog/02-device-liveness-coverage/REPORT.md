# 02. Device liveness and coverage — report

Branch: `backlog/02-device-liveness-coverage`, cut from `backlog/01-usage-aggregation`.

## What the task asked for

1. `ingestion/ingest-api` records device activity when it accepts a batch: `last_seen_at`, and
   per-collector state (which collector reported, healthy or degraded, spool depth and dropped counts
   where the envelope carries them).
2. The endpoint agent (`endpoint/capture-core`) sends a periodic heartbeat even when no AI traffic
   occurs, so an idle device is not mistaken for a dead one.
3. A job writes `ops.coverage_snapshot` per tenant: devices enrolled, devices reporting, and gap
   reasons, on the definition in the design.
4. `mart.v_device_liveness` then yields reporting, stale, never_reported and revoked correctly.
5. The device read (or a small companion read) returns fleet-wide counts by status, so the two cards
   on Devices agree.

## What was built, and where

### Device activity on the batch path — `ingestion/ingest-api`

- `internal/store/sql.go`: new `SQLTouchDevice`, issued inside the same transaction as
  `ingest.record_event`. It is monotonic
  (`WHERE last_seen_at IS NULL OR last_seen_at < $3`), so a spool flush or retry cannot move a live
  device's stamp backwards.
- `internal/store/memory.go`: the same rule for the test double, plus `LastSeenAt`.
- `database/schema.sql`: `GRANT UPDATE (last_seen_at) ON ops.device TO sac_ingest;` — column-level,
  so the write path cannot change a device's identity, OS or revocation.

### Per-collector state — `control/control-api` (health channel)

The design puts the health channel on `control-api` (docs/02 §5.4; the edge routes `/v1/health`
there; the schema comment and grants say "control-api: enrolment, policy, health, grants"). The task
named `ingest-api`; I followed the design/schema and split the two facts: ingest-api records
*activity* on the batch path, control-api records *per-collector state* from the heartbeat. See
"Decisions".

- `endpoint/protocol/health.go` (new): `HealthRequest`, `SpoolHealth`, `HealthResponse` and their
  validation. `HealthReport` gained an optional `permissions` map (the per-permission state docs/02
  §5.4 asks for) and its `device_id` is omitted when empty.
- `control-api/internal/health/service.go` (new): validates the report, builds the row, and carries
  the device-level fields (agent version, clock offset, policy version, signature state, kill-switch
  state, credential expiry, counters, spool bytes) in the row's `detail` jsonb, because the schema
  has no column for them.
- `control-api/internal/store/{store.go,sql.go,memory.go,health.go}`: `RecordHealth`, a single
  transaction that validates the collector vocabulary against `ref.collector`, upserts each collector
  row guarded by `last_report_at < EXCLUDED.last_report_at`, and stamps `ops.device.last_seen_at`
  monotonically. An unknown collector is refused with nothing written.
- `control-api/internal/httpapi/server.go`, `cmd/control-api/main.go`: `POST /v1/health`, wired to
  the health service. Authentication reuses the device-credential path (`resolveCurrent`), so the
  tenant and device come from the credential and never from the body.

### The endpoint heartbeat — `endpoint/capture-core`

- `drain/health.go` (new): `Drainer.ReportHealth`, which posts a `HealthRequest` to `/v1/health` over
  the same x509 or DPoP transport the event drain uses.
- `cmd/capture-core/health.go`: the health ticker now runs whenever there is a file **or** a
  control-plane endpoint; it writes the file and posts a heartbeat (once at startup, then on the
  interval). The request maps each route to its `ref.collector` code (`proxy.tls` → `egress_proxy`,
  …, browser routes → `capture_extension`) and adds a `classifier_host` row from the classifier
  status. The snapshot reports whether the heartbeat is being accepted.
- `cmd/capture-core/selftest_ingest.go`, `selftest.go`: the selftest's fake cloud now serves
  `/v1/health` and asserts the heartbeat arrived and validated.

### Coverage snapshots — `aggregation/aggregator`

- `internal/rollup/coverage.go` (new): `CoverageSnapshotSQL` and `CoverageWindowFor`, run inside each
  tenant's existing aggregate transaction. One row per enrolled (non-revoked) device per
  `ref.collector` per day for a trailing window (default 7 days). `expected` is always true;
  `observed` is true when that collector reported `healthy`/`degraded` that day; `gap_reason` is
  `tampered` when that is what the collector said and `unknown` otherwise. `observed` is monotonic
  within a day so a later run cannot erase a past day's answer.
- `internal/rollup/runner.go`, `cmd/aggregator/main.go`: the run writes coverage and a
  `-coverage-lookback` flag (env `SAC_COVERAGE_LOOKBACK`) controls the window.

### The read side — `query/query-api` and `query/dashboard`

- `src/blocks.js`: `deviceStatusStatement()`, fleet-wide counts by status from the enrolled
  denominator, with the same precedence the dashboard's row status uses.
- `src/templates.js`: `q7_devices` returns it as an extra; `src/plan.js` surfaces it as
  `meta.extras.device_status` and uses the **current UTC day** as the default coverage window when a
  query has no window, so the banner describes today.
- `src/views.js` (`devicesView`): the "Need attention" and fleet cards both use the server's
  fleet-wide counts, so they describe one population.
- `query/dashboard/index.html`, `explore.html`: regenerated with `node tools/build-index.mjs`.

### Schema

Three indexes the design names (docs/04 §3.11) and the grant above:
`ops.device (tenant_id, last_seen_at)`, `ops.collector_state (tenant_id, state)`, and
`ops.coverage_snapshot (tenant_id, snapshot_day) WHERE NOT observed`.

**Bringing an existing database up to it** (the lab uses a persistent volume, so the `schema`
container will not re-apply over existing objects; `localdev/schema/apply.sh` skips when the
`ingest` schema exists):

```sql
GRANT UPDATE (last_seen_at) ON ops.device TO sac_ingest;
CREATE INDEX IF NOT EXISTS device_by_last_seen ON ops.device (tenant_id, last_seen_at);
CREATE INDEX IF NOT EXISTS collector_state_by_state ON ops.collector_state (tenant_id, state);
CREATE INDEX IF NOT EXISTS coverage_snapshot_unobserved
  ON ops.coverage_snapshot (tenant_id, snapshot_day) WHERE NOT observed;
```

### Simulator

`localdev/tools/simulate-devices.mjs` now posts a `/v1/health` report per simulated device after its
events, so the lab has collector state without the owner's Windows agent.

## Tests added

- `endpoint/protocol/health_test.go`: health request validation and the closed counter set.
- `control/control-api/internal/health/service_test.go`: the write, the unknown-collector refusal,
  the stale-report guard and the shape refusal.
- `ingestion/ingest-api/internal/store/memory_test.go`: the batch path stamps `last_seen_at`
  monotonically.
- `aggregation/aggregator/internal/rollup/coverage_test.go`: the coverage window and the live
  coverage statement (idempotent, monotonic).
- `aggregation/aggregator/internal/rollup/liveness_test.go`: the four liveness transitions against
  `mart.v_device_liveness`.
- `query/query-api/test/templates.test.mjs`: `q7_devices` returns fleet-wide counts by status.
- `query/dashboard/test/render.test.mjs`: the two Devices cards agree when the read carries them.

## Verification

### Baseline (before any change)

Recorded at the start of the task on `backlog/01-usage-aggregation`:

- `endpoint/protocol`, `endpoint/capture-core`, `control/control-api`, `aggregation/aggregator`:
  pass.
- `query/query-api`: 193 pass / 0 fail; `query/dashboard`: 138 pass / 0 fail.
- `ingestion/ingest-api`: **5 pre-existing failures** in `internal/store`
  (`TestLiveRecordEventRunsTheLadder`, `TestLivePrincipalStatusRunsTheStatement`,
  `TestLiveDPoPReplayIsOneShot`, `TestLiveQuarantineInsertUsesTheLiveVocabulary`,
  `TestLiveM0BoundaryIsAlsoAConstraint`). Cause: those live tests `INSERT` tenant
  `11111111-…` into the *default* lab (which the harness's plain `psql` reaches and which already
  holds that tenant), so the insert violates the primary key. This is an environment collision, not
  a code defect, and it is unchanged by this task.

### Tests after the change

| Suite | Result |
|---|---|
| `endpoint/protocol` | pass (incl. new health tests) |
| `endpoint/capture-core` | pass (incl. new heartbeat mapping test; `--selftest` passes and now asserts the heartbeat POST) |
| `control/control-api` | pass (incl. new health service tests) |
| `aggregation/aggregator` | pass (incl. new coverage and liveness DB tests) |
| `ingestion/ingest-api` | pass except the same 5 pre-existing `internal/store` live failures |
| `query/query-api` | 193 pass / 0 fail (snapshot regenerated) |
| `query/dashboard` | 139 pass / 0 fail |
| `node tools/check-vocab.mjs` | no drift |
| `node tools/check-invariants.mjs` | 6 pass / 0 fail / 1 partial (INV-5 needs a live server; pre-existing) |
| `node installer/verify.mjs` | all checks passed |

`node tools/check-seams.mjs` reports one pre-existing finding (`endpoint/protocol/content.go:63`
names `policy_rule_id`), unrelated to this change.

### Schema from empty

Applied `database/schema.sql` to a fresh `postgres:17-alpine` container: applied clean; the three new
indexes exist; `has_column_privilege('sac_ingest','ops.device','last_seen_at','UPDATE')` is true.

### The device-auth lab (observed on the real dashboard)

1. Rebuilt the images (`node localdev/build.mjs --auth`, then the query-api image after a late fix)
   and brought the lab up (`docker compose -f localdev/authlab.compose.yaml up -d`). The `schema`
   container skips, so the migration above was applied by hand first.
2. Ran the simulator against the dashboard tenant:
   `node localdev/tools/simulate-devices.mjs --edge https://edge:8443 --tenant 11111111-1111-1111-1111-111111111111 --devices 2 --events 40`.
   It enrolled (idempotently, so the fleet stayed at 10), sent 40 events and 2 health reports.
3. `ops.device.last_seen_at` was stamped for the device(s) that sent a batch;
   `ops.collector_state` held 12 rows (2 devices × 6 collectors), with `egress_proxy` `degraded` on
   one device and `healthy` elsewhere; `control-api` logged
   `control: health report recorded … collectors:6`.
4. Within one aggregator interval, `ops.coverage_snapshot` for today read
   `expected = 60, observed = 12`, with `gap_reason = unknown` for the rest.
5. The dashboard's Devices read (`POST /v1/query q7_devices` via the dashboard's own origin,
   `http://host.docker.internal:8787/v1/query`) returned
   `coverage = {devices_reporting: 2, devices_enrolled: 10, state: "partial", gap_reasons: {unknown: 48}}`
   and `meta.extras.device_status = {reporting: 1, degraded: 1, stale: 0, never_reported: 8, revoked: 0}`.
6. Rendering the same envelope through the dashboard's own modules produced the Devices screen:
   fleet card **10** with split **Reporting 1 / Not reporting 9**, **Need attention 9**, and the banner
   *"Coverage is partial … 2 of 10 enrolled devices reporting · gaps in 1 category. Every figure here
   is a floor, not a total."* The two cards agree (9 = 9).
7. Stale transition: backdating one device's `last_seen_at` by 25 h turned its row `stale` and the
   rendered screen showed **"Quiet since 2026-10-03"**; `device_status` moved that device from
   `never_reported` to `stale` and the cards stayed in agreement.
8. The served `index.html` carries the new code (`device_status` present), i.e. the dashboard image
   really is the rebuilt one.

The two Devices cards agree by construction because both now use the same fleet-wide companion read.
Note the banner's "N of M enrolled devices reporting" is a *liveness* figure from
`ops.device.last_seen_at`, while the card's "Reporting" is "no attention flag" (a degraded device is
heard from but still needs attention). They are different measures, each labelled; the two *cards*
are the pair the task asked to agree.

### Whole-repository acceptance

`node tools/accept.mjs`: **5 pass / 3 fail / 1 skip**. Evidence:
`.integration/accept-2026-10-04T22-46-47-438Z.json`. All three failures are pre-existing or
environmental, and the baseline `verify-all` evidence from before this task
(`.integration/verify-all-2026-10-04T21-33-23-512Z.json`) shows the same two failing packages:

| Gate | Result | Why |
|---|---|---|
| `packages` | FAIL | 16 of 18 pass. The two failures are `endpoint/classifier-host` (the parent does not observe the child's memory use — a platform measurement) and `ingestion/ingest-api` (the 5 live-store tenant collision above). Both also fail in the 21:33 baseline evidence, so this change added none. |
| `seams` | FAIL | The pre-existing `endpoint/protocol/content.go:63` finding (`policy_rule_id`); present before this change. |
| `db` | FAIL | `database/tools/run-invariants.ps1` is PowerShell and this host has no `pwsh`, so the runner emits no `TALLY` line. |
| `contract`, `vocab`, `installer`, `invariants`, `endpoint` | PASS | The `endpoint` gate runs `capture-core --selftest`: **26 assertions, 0 failures**, including the new heartbeat POST. |
| `browser` | SKIP | No Chromium-family browser on the harness. |

## Decisions the owner should know

1. **The health channel is on `control-api`, not `ingest-api`.** The task item 1 names ingest-api,
   but docs/02 §5.4, the edge route table, the schema comment on the grants, and the
   simulate-devices note all put `/v1/health` on control-api, and `sac_ingest` held only `SELECT` on
   `ops.device`. I followed the design and split the two facts (activity on the batch path,
   collector state on the health channel), and updated docs/02 §9 with the as-built note.
   `ingest-api` still records device activity when it accepts a batch, the first clause of the item.
2. **Collector names are canonicalised in the agent.** A provider reports a route name; the coverage
   tables key on `ref.collector`. The agent maps before sending and the server refuses an unknown
   name, so docs/01 §4.3's "the collector name must come from `ref.collector`" holds on the wire. The
   mapping is a table in the agent, not read from `ref.collector`; O4's conformance check would close
   that.
3. **`expected` is every collector for every device.** There is no per-device expected-collector
   policy data, and docs/04 §3.7 sizes coverage as devices × 6 collectors. A device that legitimately
   runs fewer collectors therefore shows as a gap with reason `unknown`; that is the honest answer
   with the data available.
4. **The coverage read's default window is the current UTC day.** Previously a window-less read
   covered "the last 24 h", which by date arithmetic meant yesterday; the coverage fact is written
   for today, so the banner would have lagged a day. The window is now the current day and the
   envelope still carries it.
5. **No new third-party dependency was added** to any module.

## What is not finished

- The owner's Windows agent still runs the old build: it does not send the heartbeat or the canonical
  collector names. Until the new MSI is installed the dashboard tenant's per-device collector state
  is only what the batch path and the simulator produce (the batch path alone is enough for
  `last_seen_at` and therefore for `reporting` / `stale` / `never_reported`).
- `mart.agg_device_period` is still not written (as in task 01); the Devices answer is served from
  `mart.v_device_liveness`.

## Hand-off: what the owner must run by hand

The heartbeat is an agent change, and the agent is installed on the owner's Windows host from an MSI.
The harness cannot build that MSI (`installer/lab-msi.mjs` refuses a non-Windows host) and cannot
reinstall it. I cross-compiled and verified the Windows payload
(`node installer/build.mjs --os windows --arch amd64`, plus `node installer/verify.mjs`, both pass),
so the code is ready; the packaging step is the host's.

On the Windows host, with the auth lab already up:

```
docker compose -f localdev/authlab.compose.yaml up -d     # the lab must already be up
node installer/lab-msi.mjs                                # writes installer/dist/ShadowAICapture.msi
msiexec /i installer\dist\ShadowAICapture.msi             # or double-click
```

Do **not** run `node localdev/run.mjs --auth`: it regenerates the lab certificate authority and
orphans the agent already installed. After installing, the agent will send `POST /v1/health` every
15 minutes (and once at startup), `ops.collector_state` will fill for the device, and the coverage
banner will move from `unknown` gaps to `observed`.
