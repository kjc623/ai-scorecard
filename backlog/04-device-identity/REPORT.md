# 04. Device identity — report

Branch: `backlog/04-device-identity`, cut from `backlog/03-findings` (which already contains 01 and 02).

The task reverses a deliberate design choice — `ops.device` held a hostname hash and the wire was
pseudonymous end to end — so the options and the recommendation are in
[`DECISIONS.md`](DECISIONS.md), and the outcome is recorded in
[ADR 0021](../docs/adr/0021-device-identity-is-clear-by-default.md). Read those first; this report is
what was built and what was verified.

## What the task asked for

1. Enrolment and heartbeat carry, and the device read returns: hostname, the clear account name at
   submission, agent version, the collection mode in force, and managed state.
2. Devices shows the hostname as the device name (UUID on hover) plus User, Agent version and Mode.
3. Search results show `user | hostname | tool`.
4. The schema change is in `database/schema.sql`, the lab comes up clean, and tests cover enrolment
   and the read.

## What was built, and where

### The decision setting — `ops.tenant.device_identity`

A closed pair `clear` | `hashed`, `NOT NULL DEFAULT 'clear'` (ADR 0021). `clear` stores and shows the
clear hostname and account name; `hashed` stores a hostname hash and no name. The setting reaches the
device on `EnrolmentResponse.device_identity` and `HealthResponse.device_identity`, and the server is
authoritative: control-api drops a clear hostname for a `hashed` tenant, and `ingest.record_event`
drops a clear `subject_name` for one, so a device with stale configuration cannot make a `hashed`
tenant store what it forbade. The agent's local default is `clear` (the product default) with
`--device-identity` as a bootstrap override.

### Contract — `contracts/event-envelope.schema.json` and regenerated bindings

One optional field, `subject_name` (1–200 chars), beside `user_ref`, permitted at every kind and mode
(it is not content-derived). `user_ref` stays the key for policy scope, dedup, aggregates,
k-suppression and audit. Generated TypeScript and Go regenerated; `contracts/README.md` records the
reversal.

### Endpoint — `endpoint/protocol`, `endpoint/capture-core`

- `protocol.DeviceInfo` gains `hostname`, `hostname_hash`, `managed_state`;
  `EnrolmentResponse`/`HealthResponse` gain `device_identity`; `HealthRequest` gains `hostname`,
  `collection_mode`, `managed_state`, with closed-set validation for the last two.
- `core.Identity` and `EnvelopeInput` carry `SubjectName`; `envelopeWire` emits `subject_name`; the
  kind field policy declares it optional for all three kinds, and a name over 200 characters is
  refused at mint time rather than sent to be rejected by ingest.
- `cmd/capture-core` resolves the hostname from the OS (or `--hostname`), the account name from the
  OS user (or `--subject-name`), reports managed state (`--managed-state`, default `unknown`), and
  computes the effective base collection mode from the verified bundle (device override, else tenant
  default). The heartbeat and the enrolment request carry these; the health response is adopted so a
  tenant change takes effect without a reinstall.
- `credential.Credential` stores the enrolled `device_identity` so a restart keeps acting on it.

### control-api — enrolment and health

- `store.Tenant.DeviceIdentity`; `SQLTenant`, `SQLUpsertDevice`, `SQLFindDeviceByHardwareIdentity`,
  `SQLDevice` read/write the new device columns.
- `applyEnrolmentIdentity` writes hostname or hash by the tenant setting; `Enrol` returns the setting.
- `RecordHealth` now takes a `store.DeviceHealth` and applies `SQLSetDeviceHealth`, which updates
  `hostname`/`hostname_hash`/`agent_version`/`collection_mode`/`managed_state` under the same
  freshness guard as `last_seen_at`, reading the tenant setting in the same transaction.

### ingestion — `database/schema.sql` (and MIGRATION.sql)

`ingest.record_event` computes the gated `subject_name` once, stores it on the observation and the
merged submission, and maintains `ops.device.last_user_ref` / `last_subject_name` (the most recent
user) under the same out-of-order guard as `last_seen_at`. The device-activity grant extends to the
two new columns. This is the one change to the ingest write path, so no Go code changed there.

### query-api and dashboard

- `mart.v_device_liveness` exposes `hostname`, `agent_version`, `collection_mode`, `last_user_ref`,
  `last_subject_name`; the registry serves them and marks the source **subject-bearing**, so the
  Devices read is audited (`docs/04` §5.2). Snapshot regenerated.
- `describeHits` joins `ops.device` and returns `hostname` and `subject_name` (falling back to
  `user_ref`) on every search hit.
- `devicesView` names the device by hostname (UUID on hover, UUID as fallback) and adds User, Agent
  version and Mode columns; `exploreHitMeta` renders `user | hostname | tool` with the UUID as the
  device fallback. `index.html`/`explore.html` regenerated.

## Verification

### Baseline (before any change, on `backlog/03-findings`)

- Node: `query/query-api` 198 pass / 0 fail / 15 skipped; `query/dashboard` 139 / 0 / 6; `contracts/tools` 12 / 0.
- Go: `endpoint/protocol`, `endpoint/capture-core`, `control/control-api`, `aggregation/aggregator`
  pass. `ingestion/ingest-api` has **5 pre-existing failures** in `internal/store` (the live tests
  insert tenant `11111111-…` into the default lab, which already holds it — an environment collision,
  unchanged by this task).

### Tests after the change

| Suite | Result |
|---|---|
| `endpoint/protocol` | pass |
| `endpoint/capture-core` | pass (incl. new `TestEnvelope_SubjectNameIsOptionalAndGated`) |
| `control/control-api` | pass (incl. new `TestEnrolmentIdentitySettingGatesTheStoredHostname`, `TestReportGatesClearHostnameOnTenantSetting`) |
| `aggregation/aggregator` | pass (unchanged) |
| `ingestion/ingest-api` | pass except the same 5 pre-existing live failures |
| `query/query-api` | 199 pass / 0 fail / 15 skipped (new fallback test; audit test updated because the source is now subject-level) |
| `query/dashboard` | 139 pass / 0 fail / 6 skipped (fixtures carry hostname/user/version/mode; new hit test) |
| `contracts/tools` | 12 pass / 0 fail (regenerated) |
| `database/tools/check-schema.mjs` | 74 pass / 0 fail |
| `tools/check-vocab.mjs` | no drift |
| `tools/check-seams.mjs` | 1 finding, the same pre-existing `endpoint/protocol/content.go:63` (`policy_rule_id`) |
| `tools/check-invariants.mjs` | 6 pass / 0 fail / 1 partial (INV-5 needs a live server; pre-existing) |
| `installer/verify.mjs` | all checks passed (46 variables after adding 4) |

### Schema from empty and in-place migration

- `database/schema.sql` applied cleanly to a fresh `postgres:17-alpine`; the view, columns and
  function are as expected. `database/invariants.test.sql` ran on it: all 54 assertions passed.
- `backlog/04-device-identity/MIGRATION.sql` was applied over the **previous** schema
  (`git show HEAD:database/schema.sql` on the base branch) in a fresh container, with no error, and
  the resulting columns, view and `record_event` body match the new schema. It is additive plus a
  drop-and-recreate of the derived view and a `CREATE OR REPLACE` of the write function, so it is
  safe to re-run.
- Functional round-trip on the migrated database: a `clear` tenant's event stored `subject_name` and
  set `last_subject_name`; a `hashed` tenant's `subject_name` and `last_subject_name` were NULL while
  `last_user_ref` was still recorded. Evidence: the two rows differ exactly as the setting says.

### The device-auth lab (observed)

1. Applied `MIGRATION.sql` to `sac-authlab-postgres-1`, rebuilt the images
   (`node localdev/build.mjs --auth`, plus the query-api and dashboard images from their Dockerfiles),
   and brought the lab up. The schema container logged "already applied"; all services healthy.
2. Ran the simulator against tenant `11111111-…`:
   `node localdev/tools/simulate-devices.mjs --edge https://edge:8443 --tenant 11111111-1111-1111-1111-111111111111 --devices 2 --events 40`.
   It enrolled (idempotently, so the fleet stayed at 10), sent 40 events and 2 health reports; the
   server accepted all 40, rejected none.
3. `ops.device` now reads for two devices: `hostname` `SIM-MAC-01` / `SIM-WIN-02`,
   `agent_version` `simulate-devices/1`, `collection_mode` `m2`, `managed_state` `managed`,
   `last_user_ref`, and `last_subject_name` `sim.user.0.a@northwind.example` /
   `sim.user.1.a@northwind.example`. 31 submissions carry a `subject_name`.
4. The dashboard's own read (`POST http://host.docker.internal:8787/v1/query`, `q7_devices`) returned
   rows carrying `hostname` (`SIM-WIN-02`), `agent_version`, `collection_mode`, `managed_state`,
   `user_ref` and `subject_name`, with the expected `coverage_degraded` banner and
   `meta.extras.device_status` still present. The served `index.html` carries the new code
   (`device-name` and "Agent version").
5. Content search (`POST /v1/content-search`, query `capital`) returned 4 hits; each now carries
   `subject` and `hostname`. On the lab the indexed prompts predate this change and belong to devices
   that never reported a hostname, so `hostname` is `null` and the device UUID is shown; `subject`
   falls back to `user_ref` (`lab-user`). The enrichment itself is proven by the query-api test and by
   the device read above.

### Whole-repository acceptance

`node tools/accept.mjs`: **5 pass / 3 fail / 1 skip**, identical to the baseline in the 02 report:
`packages` (the two pre-existing failures: `endpoint/classifier-host` measurement and the ingest-api
live-store tenant collision), `seams` (the pre-existing `policy_rule_id` finding) and `db` (the
PowerShell runner `run-invariants.ps1` is not available on this host). Evidence:
`.integration/accept-2026-10-05T01-37-59-367Z.json`.

## What I could not verify

- **The real Windows agent.** The device half (resolving the submitting account name, sending the new
  enrolment and heartbeat fields, gating on the setting) is exercised by the protocol, core,
  capture-core, control-api and simulator paths, but the MSI is installed on the owner's Windows host
  and cannot be built or reinstalled here (see the hand-off). Specific unproven point: attributing a
  particular intercepted request to a particular interactive OS user. The implementation resolves the
  machine's account (config, else the OS user, else `USERNAME`/`USER`/`LOGNAME`) and the report says
  so; a background service or a multi-user host may attribute to the wrong person or omit the name.
- **MDM-resolved managed state.** There is no MDM resolver in the repository, so `managed_state` is
  the agent's report, as ADR 0021 says. The column already carries the vocabulary a resolved value
  would use.
- **A search hit with a non-null hostname on the lab.** The lab's indexed prompts are older than this
  change; the hit-shape and the join are tested, and the device read proves the hostname is stored.

## Decisions the owner should know

1. **The mode column is built on a recommendation, not a decision (DECISIONS.md D4).** The agent
   reports the *effective base mode for the device* — the device override if the signed bundle names
   it, else the tenant default — and Devices shows that one value, labelled "Mode". Per-tool,
   per-person and per-class modes can be narrower and are not shown. If the owner prefers the widest
   mode in force, or pure tenant configuration with no per-device raise, that is a one-line change
   (`healthChannel.baseMode` in `cmd/capture-core/health.go`) plus the column label.
2. **The Devices read is now audited.** Because the row returns a user, `docs/04` §5.2 makes it
   subject-level (`query.devices`, pre-read). This is a real cost on a frequently-read page and a
   change the original design did not have; the test that asserted the opposite was updated because
   the source's shape changed, not to make a test pass.
3. **The setting travels on the enrolment and health responses, not the signed policy bundle.**
   `ops.policy_bundle` still has no writer, so the bundle cannot deliver it yet; ADR 0021 records the
   move to the bundle as the follow-on once a writer exists.
4. **The hashed hostname is `sha256:<lowercased hostname>`.** The original design hashed the hostname
   but no code ever wrote it, so there was no format to match; a lowercased, prefixed digest is the
   convention every other digest in the repository uses.
5. **No new third-party dependency** was added to any module.

## Hand-off: what the owner must run by hand

The clear hostname, the account name, the agent version and the mode are reported by the endpoint
agent, which is installed on the owner's Windows host from an MSI. The harness cannot build or
install it. (The server, the read and the dashboard are already verified above with the simulator.)

On the Windows host, with the auth lab up and the migration applied to its database:

```
node installer/lab-msi.mjs        # writes installer/dist/ShadowAICapture.msi
msiexec /i installer\dist\ShadowAICapture.msi
```

The migration to apply to any existing database is
[`backlog/04-device-identity/MIGRATION.sql`](MIGRATION.sql) (the lab's own database has already had
it). Do **not** run `node localdev/run.mjs --auth`; it regenerates the lab CA and orphans the
installed agent. After the reinstall the agent sends `hostname`, `managed_state`, `agent_version`
and its effective mode on `/v1/enrol` and `/v1/health`, and a `subject_name` on each event, so the
device named `35beae1b…` in the task appears by hostname with its version and mode.
