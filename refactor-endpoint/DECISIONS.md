# Decisions

Settled decisions for the endpoint refactor, newest last. A task that deviates from `DESIGN.md` or
`PLAN.md`, or settles a vendor fact, adds an entry here: the date, the task number, what was
decided and why, and for a vendor fact the product version checked.

## 2026-10-06, owner, before task 01

- **TLS inspection becomes opt-in.** A tenant setting, off by default, gates the TLS proxy, the
  CLI shim's proxy environment, the Windows desktop-app PAC and the device root's trust-store
  installation (`DESIGN.md` §10). With it on, tools covered by an enabled native collector are not
  decrypted.
- **Scope.** Phases 0–6 of the plan, including Phase 4 (network inspection, reworked against the
  existing proxy) and E37 (generic child-process supervision, as a seam for a future MCP gateway).
  E38 (signed and notarised packages, Jamf) and E41 (nightly vendor-tool matrix) are deferred.
- **Platforms.** Windows first: each collector is built and verified on Windows, compiles
  elsewhere and reports `absent` there. Tasks 53–56 port to macOS and Linux and need those
  machines.
- **Rules.** Block and warn rules are tenant data: an ordered rule list in the signed bundle,
  edited on the dashboard's Settings page and enforced by hooks, the extension and the proxy. The
  extension's broken policy hand-off is fixed as part of this.

- **Install once, control from the dashboard** (`DESIGN.md` §0):
  - devices are installed through Intune with the standard MSI and tenant file;
  - every collection capability and policy can be turned on and off from the dashboard at any
    time after enrolment, with no install-time flags;
  - this brings the loopback broker (task 57) and the kill switch (task 47) under dashboard
    control too.

## 2026-10-06, orchestrator, while writing the tasks

- **The existing envelope is extended, not replaced** (`DESIGN.md` §3). `aigov.event.v1` is the
  plan's name for the repository's envelope. `model_detection`, which nothing emits, becomes
  `discovery`; `agent_activity` is added for OTel model requests and tool calls.
  `endpoint.config_tampered` is a health state, not an event.
- **The hook binary is `capture-core --hook`**, a relay to the running service over the native
  endpoint, like the browser's native messaging host. The policy cache and the detectors live in
  the service. The state directory is not readable by users, so a standalone binary could not read
  the cached policy; the relay keeps everything local and makes no network call.
- **Redaction is not an action.** No hook can rewrite a prompt, and rewriting proxied bodies is
  outside the plan's tasks. Rules allow, warn or block.
- **OTLP ports are 47318 (HTTP) and 47317 (gRPC) on 127.0.0.1**, not 4318/4317, so a developer's own
  collector on the standard ports is not displaced.
- **No policy version gating.** The only device is the reference VM.
  - A merge deploys the server and the agent release together.
  - Until Intune updates the VM, its older agent refuses a bundle with new fields and keeps the
    bundle it has, which never widens collection. That transient is expected; tasks verify after
    the VM runs the new release.
- **Budgets** are as in `DESIGN.md` §13. The owner can change them before task 51.
- **Tool order.** Claude Code, then Codex, then Copilot, then Cursor.
- **Network inspection reads requests only** (task 45). The product records prompts; nothing
  stores model responses, so parsing streamed responses (plan E34) is left out.

## 2026-10-06, owner, after the first draft

- **Testing happens on a VM against pre-prod, never the local lab or the owner's PC.**
  - The VM is Hyper-V on the owner's PC, Entra-joined and Intune-managed, and enrolled into a
    dedicated "Endpoint Test" tenant in pre-prod (`TESTBED.md`). The owner's tenant stays
    untouched.
  - Pre-prod is deployed first (task 00a; superseded on 2026-10-08, below).
- **A change reaches the VM through `main`.**
  1. The owner merges each task branch.
  2. The push deploys pre-prod (services, migration, signed agent release).
  3. `tools/testbed/deploy.mjs` publishes that release to the VM through Microsoft Graph, to one
     Win32 app assigned only to a test device group (task 00b, now 59).
  - Agents never push, run workflows or change pre-prod.
- **Agents check the device side** (`tools/testbed/invm.ps1`, PowerShell Direct). The owner
  changes dashboard settings and confirms what the dashboard shows; agents have no dashboard
  sign-in and no database access.
- **Schema changes now ship as numbered migrations** as well as in `schema.sql`, because pre-prod
  exists.
- The two users of a check are Entra test users signed in on the VM. The performance budgets
  (`DESIGN.md` §13) are measured on the VM.

## 2026-10-07, owner, before task 00a (now 58)

- **Pre-prod runs on SaaS: the services on Fly.io, PostgreSQL on Supabase.**
  - One test tenant and one VM don't justify the cost of a self-built cloud environment; this
    setup costs about $40–45 a month.
  - Intune and Entra are unchanged: the VM, the Intune app, the Entra test users and the vendor
    Entra application stay as they are.
  - The repository's existing deployment (`azure/`) stays as it is and is not deployed by
    pre-prod.
- **The device edge becomes a product component** (`services/edge`, from the lab's edge). Fly.io
  passes the device's TLS connection through untouched, so the edge itself requests the client
  certificate and forwards it in `X-Client-Cert`, as the product expects.
- **Agents read pre-prod with a Fly.io read-only token** (`TESTBED.md`): app status, machines,
  addresses and logs. They still have no database access; Supabase is the owner's.
- **The environment's keys are kept outside Fly.io** by the owner: the device CA, the policy
  signing key, the content keys and the directory key. Devices pin the policy key and hold
  certificates from the device CA, and the content keys decrypt stored prompts, so pre-prod's
  devices and data survive a later move of the services only if these keys do.

## 2026-10-08, owner, before task 01

- **Build first, infrastructure last.** No pre-prod and no device exist while the build tasks
  (01–57) run. Each task is built and verified on the PC with its tests and `node tools/accept.mjs`;
  the checks that need a managed device enrolled in a real environment sit in each brief's "On the
  device" section and wait.
  - The environment tasks move to the end: 58 (pre-prod), 59 (the reference VM tooling) and 60
    (device verification), which runs every "On the device" section in task order and fixes what
    it finds on fix branches.
  - Vendor facts are checked in the vendor's current documentation during the build and on a real
    install in the device phase. Where a brief needs a real tool's output (telemetry fixtures, hook
    input, desktop-app request bodies), the build uses a set written from the documentation, marked
    `documented`, and the device phase replaces it with a capture. The desktop-backend parsers
    (task 45), their block shapes (46) and the QUIC firewall rule (47) have no documentation to
    build from and are built in the device phase, from the captures and task 43's note.
  - Three tasks have no build step and run only in the device phase: 22 (a measured day), 42 and
    43 (spikes on real machines).
  - Schema changes still ship as `schema.sql` plus a numbered migration. A migration must also
    apply cleanly over the current `schema.sql`, because the migrator builds an empty database from
    `schema.sql` and then applies every migration, and that is how pre-prod's database is built
    (task 04).

## 2026-10-08, task 01, bundle drift test

- **The drift test fails, not skips, when `device/` is missing.** `TestServedBundleVerifiesWithTheDevicesVerifier` resolves `<repo>/device`; the directory is part of this repository. The `-short` and no-Go-toolchain skips stay. The throwaway module needs no `replace` for `contracts/generated/go`: the build passes without it.
## 2026-10-08, task 02

- **A call that gives up while waiting for the classifier connection does not mark the link
  degraded.** It sent nothing and the host is not at fault; it returns the existing rules-only
  fallback, whose failed stage then names `host_unreachable`. Only a call that gave up after
  sending (budget, context or a read error) marks the link degraded and drops the connection.
- **The tests' fake host reads requests as they arrive**, as the OS pipe to a real child buffers
  them. A bare `net.Pipe` blocks each writer until the host is free, which serialises callers on
  its own and hides the defect.
- **Before the fix a timed-out call already closed the connection.** The late answer reached
  another call through the request goroutine re-reading the shared connection after a reconnect,
  and a call that timed out also broke the requests of every concurrent caller on that connection.
  Both are gone: a call uses only the connection it holds the request lock for.
- **Not changed:** the public `Connect` is not serialised with `Classify`. capture-core's service
  hands the client to the pipeline before its first `Connect`, so a `Classify` arriving then can
  dial a second child and one of the two connections is replaced without being closed. No answer
  goes to the wrong caller; the brief keeps the API and scope narrow, so it is left for the owner.
## 2026-10-08, task 03

- **Branch comments use the schema's existing `comment` key**, not `$comment`: every existing
  `if`/`then` branch uses `comment`, and the generator reads that key. The new and changed branches
  follow them.
- **`destination_host` has a pattern**: lower-case RFC 1123 labels separated by dots
  (`DESIGN.md` §3 says "lower-case host name"), so an upper-case name, a URL or a path is refused.
  An IPv4 literal matches; an IPv6 literal does not.
- **`detection_basis` counts as a discovery field**: `agent_activity` forbids it, as `prompt` and
  `usage_rollup` already did. `agent_activity` permits `size_bytes`, which §3 does not forbid it.
- **The rules are per kind only.** The pairings in §3's "Used by" column (`host_app` with
  `ide_extension`, `model` with `model_request`, and so on) are not enforced by the schema: §3's
  rules are per kind, and the generator's variants are per kind and collection mode.

## 2026-10-08, task 04

- **The new `ingest.observation` columns come last** in `schema.sql`, after `expires_at`:
  `ALTER TABLE ... ADD COLUMN` appends, so a migrated table and a new one only have the same column
  order this way. The migration proof's `pg_dump --schema-only` diff depends on it.
- **The `model_detection` guard lifts `FORCE ROW LEVEL SECURITY`** on `ingest.observation` and
  `ingest.submission` for its check and restores it in the same transaction. Under forced row-level
  security the owner's query sees no rows, so without this the guard could never fail. The migrator
  owns the tables because it created them.
- **The migration is idempotent by construction**: columns are added `IF NOT EXISTS`, each changed
  CHECK is dropped `IF EXISTS` and added again, the route rows are `ON CONFLICT DO NOTHING`, and
  `ingest.record_event` is dropped and recreated with its `EXECUTE` grant to `sac_ingest` (the drop
  removes the grant). Applied over the new `schema.sql`, the dump is unchanged.
- **Column CHECKs for the new fields are enums and non-negative integers only.** `discovery_type`,
  `activity_type` and `outcome` take the contract's enums; `input_tokens`, `output_tokens` and
  `duration_ms` are `bigint >= 0`. String lengths and the `destination_host` pattern are left to
  ingest's contract validation, as for the existing text columns.
- **The shape CHECKs also require what the contract requires**: `discovery_type` and
  `detection_basis` for `discovery`, `activity_type` for `agent_activity`, besides the forbid-lists
  `check-schema.mjs` compares.
- **`dedup_tier` is part of the batch response**, so `EventResult.DedupTier`'s comment in
  `device/protocol/batch.go` now lists `T, S, R, V or A`.
- **Not changed:** `discovery` and `agent_activity` fold into a submission by the server's weak key
  (tenant, device, tool, kind, a 300-second bucket and size), as rollups do; the device's
  `dedup_key` is stored on the observation only. Several `agent_activity` records of one tool in one
  bucket with the same size (or none) therefore become one submission with a rising
  `observation_count`; every observation is kept. Changing this is outside the brief.
- **Not changed:** `ops.tool_display_name` (reads `ops.tool` and `ref.tool_catalogue` only) and the
  grants (table-level for `sac_ingest`, `sac_query` and `sac_ops`; the column lists of `sac_control`
  and `sac_vault` name no new column). `localdev/tools/simulate-devices.mjs` still emits
  `model_detection`, and `device/protocol` still declares it (task 05).
