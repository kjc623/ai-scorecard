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

## 2026-10-08, task 05

- **Protocol names follow Go initialisms and avoid the batch types**: `DiscoveryTypeIDEExtension`
  (the generated binding spells it `DiscoveryTypeIdeExtension`), and the `outcome` enum is
  `protocol.ActivityOutcome` with `ActivityOutcome*` constants, because `protocol.Outcome` is
  already the per-event batch result.
- **`core.Fact` carries no size and no population.** The brief's field list and `DESIGN.md` §3's
  table name neither; the mode is resolved from the tool fingerprint and the person.
  `BuildEnvelope` still accepts `size_bytes` on `agent_activity`, which the contract permits, but
  nothing sets it yet.
- **`BuildEnvelope` checks the metadata kinds' values, not only their fields**: the closed enums,
  the character caps, the `destination_host` pattern, the `model_names` count and non-negative
  integers, so a malformed record is refused on the device rather than rejected by ingest and
  dropped from the spool. On those kinds an empty but non-nil label or attachment list counts as
  set and is refused.
- **The spool record log** is `core.Pipeline.Log` (`log/slog`), set to the service's logger. The
  decision is the group `policy_decision` (`action`, `rule_id`), a nested object in the JSON log.
  `tool_name`, `outcome`, the token counts and `duration_ms` are not on the line: the brief's field
  list leaves them out. An envelope the spool refuses writes no line.
- **`Pipeline.Record` counts like `Process`**: `observed`, then `emitted`, `errors` or `dropped`
  on the route's counter set, and it marks the route's last success.
- **`check-vocab` has a device-only route list** (`tool.hook`, `tool.otel`, `inv.scan`,
  `net.flow`): the extension never emits them, so `messages.js` does not transcribe them. An entry
  that `device/protocol` no longer defines is a finding.
- **Not changed:** `localdev/tools/simulate-devices.mjs` still emits `model_detection`; the lab is
  not used for this work.

## 2026-10-08, task 06

- **Collector constants** are `CollectorEgressProxy`, `CollectorLoopbackBroker`, `CollectorCLIShim`,
  `CollectorProcessDetector`, `CollectorClassifierHost` and `CollectorCaptureExtension`, with
  `Valid()` beside the type in `device/protocol/envelope.go`, as task 05's enums.
- **The registry API is renamed with its key**: `Collectors()`, `StartCollector`, `StopCollector`,
  `SortedCollectors`, and a `Collector` field on `StartResult`, `StopResult` and `ApplyResult`. It
  adds `StartCollectors(ctx, skip...)` (the `start_collectors` step) and `StopExcept(ctx, skip...)`
  (`stop_remaining_providers`); `StopAll` is `StopExcept` with no skip. Both skip a provider the
  registry already stopped, so a provider switched off by policy is not stopped twice.
- **Policy toggles act only between `start_collectors` and `stop_remaining_providers`.** A bundle
  applied before (the supervisor's load step, the first-start fetch) only records the switch, so
  nothing starts before the spool opens; one applied during or after shutdown starts nothing. A
  `Toggled` provider's switch before any bundle is `Enabled(nil)`, read when it is added.
- **"Enabled by the bundle in force"** in `start_collectors` is the switch the registry recorded
  from the last `ApplyPolicy`; the load step applies the bundle in force, so the supervisor needs
  no bundle source of its own.
- **A toggle's Start and Stop run concurrently under `context.Background()`**: `ApplyPolicy` takes
  no context, and the provider outlives the poll that delivered the bundle.
- **The fixed providers keep their own steps** and are started there whether or not they implement
  `Toggled`; none of the three does yet.
- **Health**: a `Toggled` provider switched off and not running is `absent` with
  `disabled_by_policy`, whatever its own row says. A failed Stop is still `tampered` with
  `stop_failed`, also when policy asked for the stop.
- **`stop_remaining_providers` stops every provider except `egress_proxy` and `loopback_broker`**,
  concurrently; it stopped `cli_shim` only.
- **`disabled_by_policy` is transcribed in the extension's `DETAIL`** (`messages.js`), as every
  device-only detail already is: the extension's contract test requires each `Detail*` there. So
  `check-vocab` compares it like any other detail and needs no device-only entry; its file is
  unchanged.
- **The extension's row keeps its normalisation** (hyphens to underscores, for the native host's
  default `capture-extension`); provider rows carry `Name()` unchanged. `device/integration`'s
  fake `proxy.tls` provider changed with `Name()`.
- **Coverage reads `ops.collector_state.error_code`**: control-api stores the report's detail
  there (`detail` is a separate JSON column). The table holds one row per device and collector,
  the latest report, so that row decides the day.
- **Not changed:** `ops.coverage_snapshot`'s `coverage_gap_requires_reason` check requires a gap
  reason on every unobserved row, so a not-expected `disabled_by_policy` row still carries
  `unknown` (or `tampered`). query-api's coverage `gap_reasons` counts every unobserved row
  whatever `expected` says, so such a row still appears there under `unknown`. Fixing it is a
  query-api change (`AND v.expected`) or a schema change, outside the brief.

## 2026-10-08, task 09

- **`OwnerOfLocalTCP` returns `uint32`** as the brief states (`DESIGN.md` §7 says `int`), the type
  `UserOfProcess` and the Win32 calls take. `Process` carries `Started` beside the §7 fields.
- **`Publisher` is a verified signer only.** WinVerifyTrust checks the embedded signature first;
  on `TRUST_E_NOSIGNATURE` the image is verified as a member of the system catalog that lists its
  hash (SHA-256, then SHA-1, under `DRIVER_ACTION_VERIFY`), because System32 binaries such as
  `curl.exe` are catalog-signed and carry no embedded signature. Any other trust failure (bad
  digest, untrusted root) gives `""`, as an unsigned image does. Revocation is not checked and URL
  retrieval is cache-only, so a lookup never goes to the network.
- **`ProcessInfo` fails only when the process cannot be opened, timed or its image read.** An
  unreadable token (a protected process) leaves `User` nil; the proxy treats that as an
  attribution failure.
- **The proxy gains a `Config.Person` seam beside `Process`.** It is called once per intercepted
  connection, before the minted-leaf handshake, while the client is still connected (a closed
  connection's row names no process). A failure counts `errors` once, is logged without content,
  and leaves the observation to the pipeline's identity (the console user).
- **Attribution is wired on Windows only** (`facilities.connOwner`). On macOS and Linux, where
  `hostinfo` returns `ErrUnsupported`, the proxy keeps the console user and counts nothing, rather
  than an error on every request.
- **`ErrUnsupported`'s message names no lookup** ("the lookup is not supported on this
  platform"); it is returned by the new lookups too.
- **Cost on the CONNECT path:** the first `ProcessInfo` of a process verifies its signature, which
  hashes the image, once per process per 10 minutes. The device phase measures it (task 51).
- **Vendor facts (Win32 reference, MicrosoftDocs/sdk-api source, pages dated 2018-12-05, read
  2026-10-08; learn.microsoft.com is not reachable from the build machine):**
  `MIB_TCPROW_OWNER_PID` and `MIB_TCP6ROW_OWNER_PID` field order, addresses and ports in network
  byte order; `TCP_TABLE_OWNER_PID_ALL` is the sixth `TCP_TABLE_CLASS` value (5);
  `WINTRUST_CATALOG_INFO.hCatAdmin` is required for a non-SHA-1 hash;
  `CryptCATAdminCalcHashFromFileHandle2` sizes the hash with a NULL buffer; `CRYPT_PROVIDER_CERT`
  starts with `cbStruct` then `pCert`; `CertGetNameStringW` with `CERT_NAME_ATTR_TYPE` takes an
  ANSI OID (`2.5.4.3`). The `curl.exe` publisher `Microsoft Windows` is checked on Windows.
## 2026-10-08, task 07

- **`interval_minutes` is a server constant (360)**, beside the OTLP addresses and the discovery
  budget in `policyserve/service.go`: the brief gives `ops.endpoint_setting` boolean columns only,
  so nothing else could set it.
- **The device's `Endpoint` is a value, not a pointer.** A bundle without the section decodes with
  every endpoint collector off, which never widens collection. `Validate` checks the interval when
  the scanner is on or an interval is set, and the OTLP addresses when the receiver is on or an
  address is set, so a bundle without the section still validates.
- **An OTLP address must be a loopback IP literal** (`127.0.0.0/8` or `::1`) with a port;
  `localhost` is refused, because what it resolves to is the host's configuration.
- **Two store write methods**, `SetEndpointCollectors` and `SetEndpointTool`, one per route (the
  brief says "a write method"). The read is one statement, `endpoint_settings`, used by both
  `PolicyInputs` and `Settings`; it applies the §5 defaults (a `VALUES` list for the tools, whose
  defaults differ per tool, so `ops.endpoint_tool_setting` has no column defaults).
- **Both PUT bodies require every field** (the six collector switches; `otel` and `hooks`), 400
  otherwise, so a client that leaves one out never turns a collector off. An unknown tool key is
  404 `not_found`.
- **The server accepts a switch for a collector a tool lacks** (Cursor OTel, Codex and Copilot
  hooks): §5 names the keys, not which pairs are allowed, and task 39 may add hooks. The dashboard
  shows those cells as unavailable and never sends a change for them.
- **Audit actions** are `tenant.endpoint_collectors.set` (object `tenant`) and
  `tenant.endpoint_tool.set` (object `endpoint_tool`, id the tool key); `previous` and `new` are
  objects in the bundle's names.
- **The drift test also compares values**: the device re-encodes the endpoint section it decoded,
  and that must equal the served section; the test tenant's switches are off the defaults first.
- **Migration proof**: the integration branch's `schema.sql` plus `0003`, and `main`'s plus `0002`
  and `0003`, each dump identically to the new `schema.sql`; `0002` and `0003` applied over the new
  `schema.sql` change nothing.
- **Not changed:** `ops.policy_bundle.feature_state` still records only `cli_shim` and `proxy_tls`.
## 2026-10-08, task 08

- **A fixed provider that is `Toggled` waits at its step.** The supervisor's `start_cli.shim` and
  `start_proxy.tls` steps skip a provider the bundle in force switches off (`Registry.Enabled`); a
  policy toggle starts it once `start_collectors` has run. The step order is unchanged. Task 06's
  fixed providers started whatever their switch.
- **proxy.tls removes the root at every Stop**, a policy stop included, and also when it never
  started, so a root an earlier run left is removed at the next stop. The supervisor's removal at
  shutdown stays. A Start after a Stop binds and installs again; a Start that finds no kill switch
  in force clears an earlier `killed`, so the row never says killed while the proxy runs.
- **cli.shim and the PAC restart after a Stop**: their Start guard was "started", now "running".
- **The PAC is the `desktop_proxy` provider** (`winproxy.Server`), registered on Windows only
  (`facilities.desktopPAC`), so other platforms report no `desktop_proxy` row. Its switch is
  `interception.enabled`, a non-empty `pac_listen` and no proxy.tls kill switch in force; the kill
  switch keeps the suppression `buildPAC` had. `pac_listen` is read at each Start; empty means off,
  and the 8350 fallbacks (service and `winproxy`) are gone.
- **The server sends no `pac_listen`.** `policyserve.Interception` has no such field and the brief
  adds none, so every served bundle keeps the desktop-app PAC off, with TLS inspection on too. The
  PAC checks of the "On the device" section (the AutoConfigURL in steps 1 and 2) cannot pass until
  control-api serves a `pac_listen`; that is a bundle change for the owner to decide.
- **PAC health**: healthy while served with proxy.tls in the path; degraded `not_effective_proxy`
  when the proxy is out of it (the PAC then serves every user's original route). `observed` counts
  users the PAC was applied to, `not_cooperative` users left untouched, `errors` a failed user
  enumeration.
- **`interception.enabled` is never omitted** on either side, so the drift test sees the name. The
  drift test now turns TLS inspection on and compares the value the device decoded.
- **`ops.policy_bundle.feature_state`** records `cli_shim` and `proxy_tls` as the bundle's
  `interception.enabled`; it recorded `true` for every bundle.
- **`PUT /admin/v1/settings/tls-inspection`** requires `enabled` (400 `invalid_request` when it is
  missing or null). The audit action is `tenant.tls_inspection.set`, object `tenant`, with
  `previous` and `new` booleans.
- **`desktop_proxy`'s `ref.collector` row** supports `m0`–`m3`, as `cli_shim`'s does: it routes
  traffic and reads none.
- **`facilities.shimProfile`**: the service tests write the shim profile into their own temporary
  directory. On Linux its default is `/etc/profile.d`, which the service tests wrote before.
- **Migration proof** (migration `0004`, renumbered at merge): the integration branch's
  `schema.sql` plus the migration, and `main`'s plus `0002`, `0003` and it, each dump identically to
  the new `schema.sql`; `0002`, `0003` and it applied over the new `schema.sql` change nothing.
- **Not changed:** the proxy's listen address and the shim's proxy address are still read when the
  service builds them; only the PAC's is read at each start, as the brief says. A first start with
  no bundle therefore still binds the proxy on a random port once inspection is turned on.
  `Bundle.Intercepts` does not consult `enabled`: nothing calls it while the proxy is off.

## 2026-10-08, task 23

- **Migration `0005-otel-receiver.sql`** (renumbered at merge; the migration loader refuses gaps,
  so numbers follow merge order). The integration branch's `schema.sql` plus the migration dumps
  identically to the new `schema.sql`, and the migration over the new `schema.sql` changes nothing.
- **OTLP facts** checked against `opentelemetry-proto` `docs/specification.md` (main, read
  2026-10-08; `go.opentelemetry.io/proto/otlp` v1.11.1): OTLP/JSON carries `traceId` and `spanId`
  as hex, not protojson's base64, so the receiver converts them after `protojson.Unmarshal`;
  unknown JSON fields are ignored; the size limit applies after decompression too (413); a 4xx
  body is a `google.rpc.Status` (built with `grpc/status`, no new dependency); the response uses
  the request's `Content-Type`. The limit is the brief's 4 MiB, not the spec's suggested 64 MiB.
- **gRPC token check.** grpc-go decodes the message before a unary interceptor runs, so on gRPC an
  unauthenticated request is decoded but never routed; over HTTP the token is checked before the
  body is read (a test proves it). The gzip decompressor is registered for exporters that compress.
- **Counters.** `observed` per log record and span received, with or without a normalizer;
  `skipped_not_generative` per metrics request; `errors` per refused request. Refusals are logged
  with the endpoint, size and outcome; accepted requests are not logged.
- **Addresses come only from the bundle**, applied before the receiver starts; there is no
  device-side default. Any bind failure on a loopback address is `port_held_by_other`. A failed
  `Start` leaves the row `absent` with that detail; a rebind that cannot take one port leaves the
  other serving and the row `degraded`, and `ApplyPolicy` returns the error.
- **Normalizers** are `otlp.Config.Normalizers`, consulted in order (the first that accepts the
  `service.name` wins), and their methods return nothing. `Sender` is `{RemoteAddr string}`.
- **Versions added:** grpc v1.84.0, protobuf v1.36.12, otlp v1.11.1; for tests the OTel SDK and
  trace and metric exporters v1.47.0, the log exporters v0.23.0.
- **Not changed:** control-api's in-memory store (`storetest/memory.go`) still lists the six
  original collectors; nothing reports `otel_receiver` to it.

## 2026-10-08, task 10

- **Migration `0006-user-helper.sql`** (renumbered at merge; numbers follow merge order). The row is inserted
  `ON CONFLICT DO NOTHING`, so it applies over the new `schema.sql`. Its `modes_supported` is
  empty: the helper reads nothing.
- **The `localipc` move.** The framing tests moved with the framing into `localipc`; the relay test
  calls `localipc.WriteFrame`/`ReadFrame`. The trust seams `nativeServerOwnerTrusted` and
  `nativeServerUIDTrusted` stay in `cmd/capture-core` and are passed to `localipc.Dial`, so the
  test helpers that replace them are unchanged. The predicate's type stays per platform (owner SID
  on Windows, server uid elsewhere).
- **One endpoint, routed by the first frame.** A connection that opens with `helper_hello` is the
  provider's; any other is a browser relay, handled exactly as before.
- **The owner check compares SIDs** (the uid elsewhere): the user of `WTSQueryUserToken(session_id)`
  against the pipe client's token user. A refused hello is answered with a `malformed` refusal (no
  new refusal reason). A newer helper connection for a session replaces an older one.
- **Restart budget.** The first start in a session is not a restart; a session gets 5 restarts in
  a sliding 10 minutes, then none until the window allows one, and reports
  `degraded`/`helper_unavailable` meanwhile. Restarts happen on the 15 s tick. A session whose user
  changed is a new session with a fresh budget. Session 0 is never served.
- **Health.** With no signed-in session the row is healthy once the sessions were listed. A failed
  listing or start is `degraded`/`helper_unavailable` and counts `errors`.
- **`notify` limits.** Title 1-64 and body 1-280 characters, no control character except a line
  break in the body, link an absolute https URL of at most 2,048 bytes with no credentials. The
  helper validates again. `Notify` waits at most 10 s for `notify_result`.
- **The toast.** `ToastGeneric` with the title and body as two text lines; a link is an "Open"
  action with `activationType="protocol"` (the schema documents `activationType` on `action`, not
  on `toast`). The XML is written as ASCII with character references, because go-ole's
  `NewHString` passes the rune count where `WindowsCreateString` takes UTF-16 units.
- **The shortcut.** AppUserModelID `ShadowAICapture.Agent`; a "Shadow AI Capture" shortcut in the
  all-users Start menu, nested in capture-core.exe's `File` (its target), with arguments `--version`
  so opening it only prints the version. `verify.mjs` checks the Go constant equals the manifest's.
- **`check-vocab`.** `helper_hello`, `notify` and `notify_result` are device-only message types and
  `helper_unavailable` a device-only detail; `messages.js` is unchanged.
- **The Windows integration test** (`TestNotifyShowsAToastInTheConsoleSession`) runs only as
  LocalSystem with a signed-in console user and skips otherwise: `WTSQueryUserToken` needs
  `SE_TCB_NAME`, which an administrator does not hold. The test binary is its own helper
  (`--user-helper <pipe>`), started in the console session only. The device phase must run it as
  SYSTEM.
- **Vendor facts (read 2026-10-08; learn.microsoft.com is not reachable from the build machine):**
  - Win32 reference (MicrosoftDocs/sdk-api source, pages dated 2018-12-05): `WTSEnumerateSessionsW`
    (reserved 0, version 1, freed with `WTSFreeMemory`); `WTS_CONNECTSTATE_CLASS` (`WTSDisconnected`
    is a signed-in user who is not connected); `WTSQueryUserToken` (LocalSystem with
    `SE_TCB_NAME`; the token is closed by the caller); `CreateProcessAsUserW` (`winsta0\default`
    for an interactive process, `CREATE_UNICODE_ENVIRONMENT` with a `CreateEnvironmentBlock`
    block).
  - WinRT IIDs and method order from Microsoft's windows-rs bindings (master): `IXmlDocument`
    F7F3A506-..., `IXmlDocumentIO` 6CD0E74E-... (`LoadXml` slot 6), `IToastNotificationFactory`
    04124B20-... (`CreateToastNotification` 6), `IToastNotificationManagerStatics` 50AC103F-...
    (`CreateToastNotifierWithId` 7), `IToastNotifier` 75927B93-... (`Show` 6).
  - `ToastNotificationManager` (MicrosoftDocs/winrt-api): a desktop app's toast needs a Start
    shortcut with an AppUserModelID. Toast schema (MicrosoftDocs/winrt-related: `toast` 2017-04-05,
    `action` 2022-03-01, `binding` `ToastGeneric`). Application User Model IDs (MicrosoftDocs/win32,
    2018-05-31): an MSI sets it with the `MsiShortcutProperty` table.
  - WiX (wixtoolset/wix main, `Compiler_Package.cs`): `Shortcut` takes `Directory`, `Arguments`,
    `WorkingDirectory`, and targets its parent `File`; `ShortcutProperty` takes `Key` and `Value`.
  - None of it ran here; the device phase checks it on the reference VM.
