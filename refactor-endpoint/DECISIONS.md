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
- **control-api sends `pac_listen` while TLS inspection is on.** `policyserve.Interception.PacListen` carries the server constant `policyserve.PACListen` (`127.0.0.1:8350`, the address the device used to fall back to) in every bundle with `interception.enabled` true, and omits it when the setting is off, so turning inspection on brings the desktop-app PAC up with the proxy, shim and root. It is not configurable. `TestComposeInterceptionEnabled` checks it present and equal to the constant, then absent again after the setting is switched off; `TestServedBundleVerifiesWithTheDevicesVerifier` compares the value the device decoded.
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

## 2026-10-08, task 24

- **`peerPerson` stays in `cmd/capture-core`.** It reads the service's console user, user-reference
  key and identity setting, so the receiver takes it as `otlp.Config.Person` (the way the proxy
  takes `Config.Person`) rather than moving that state into a shared package. Its behaviour and
  tests are unchanged.
- **The lookup runs on a connection's first authenticated request**, not inside `ConnContext` or
  `TagConn` themselves. Those only tag the connection (local and remote address) in its context;
  the lookup happens once, while the client waits for its answer. `http.Server` calls
  `ConnContext` on its accept loop, so a lookup there (the first `ProcessInfo` of an image hashes
  it) would hold up every other client; an unauthenticated client also causes no lookup. A
  metrics-only connection is looked up and logged too.
- **gRPC uses `TagConn` addresses**, not the raw `net.Conn`. grpc-go hands a stats handler the
  connection's addresses only, and they are all `OwnerOfLocalTCP` needs; the interceptor reads the
  tag from the call's context, which grpc-go derives from the connection's.
- **`Sender` drops `RemoteAddr`** and is the brief's `{PID, Image, Publisher, Person, Resolved}`.
  `Person` is never nil: an unresolved sender carries the `unattributed` user_ref, so a normalizer
  cannot fall back to the pipeline's identity (the console user).
- **Errors.** A failed lookup, or a process whose account cannot be read, counts one `errors` for
  the connection. `hostinfo.ErrUnsupported` (macOS, Linux) counts nothing, as for the proxy; the
  sender is still `unattributed`.
- **The connection line** is `otlp: new <http|grpc> connection: pid N, image <base>, user_ref <ref>`,
  with the lookup's error appended when unresolved.
- **Tests.** On Linux the dialling-client tests run through a stand-in for the TCP owner table that
  knows only the connections the test dialled. The Windows test
  (`TestSenderIsNamedByTheTCPOwnerTable`, real `OwnerOfLocalTCP` and `ProcessInfo`) compiles here
  but did not run; the device phase runs it.
## 2026-10-08, task 48

- **Where the row lives.** `component.Supervisor` is a `core.Provider`, but the service holds it
  rather than the registry. Registered there, `start_collectors` would start it again and
  `stop_remaining_providers` would stop it before the drain. The core supervisor keeps starting it
  at `start_classifier_host` and stopping it at `stop_classifier_host` through its `ClassifierHost`
  field, so `core` is unchanged. Its row follows the extension's, as the synthetic row did.
- **Health.** The row is `healthy` once `Ready` (the handshake) passes. It is `degraded`/
  `host_unreachable` while the host starts, waits for a restart or fails `Ready`, and `degraded`/
  `component_crash_loop` while restarts are suspended. It is `absent` when not started. So a device
  with no classifier release now reports `absent` instead of `degraded`/`classifier_unavailable`.
  A refused handshake shows as restarts, and then as a crash loop, instead of `version_mismatch`
  (the link still records the cause). The row's `version` is still the classifier version.
  `health.json`'s `classifier` is now that row.
- **Restart budget.** The first start is not a restart. The delay resets once a child has run for
  10 minutes. After the 10-minute suspension the host starts at once, and the delay keeps
  doubling. The crash-loop detail clears when a child next comes up.
- **Ready is bounded at 30 s.** An anonymous pipe has no deadlines, so a child that has not
  answered the handshake by then is killed.
- **Only `Ready` reaches the stdio.** `Dial` serves a child's stdio once, and only within its
  `Ready` check. The link's own reconnect fails fast to rules-only, and the supervisor reconnects a
  restarted host. Before, each `Classify` after a failure spawned a new host. A hung or refused
  host now counts against the restart budget.
- **The host's stderr is discarded.** Before, it went to the service's stderr.
- **`classifierlink.New` and `ChildDialer` are gone.** `device/integration/attachment_path_test.go`
  used `New`; it now runs classifier-host under `component.Supervisor`, the way the service does.
  That file is outside the brief's list.
- **Windows.** Go's `os/exec` cannot start a process suspended, so the child joins the job just
  after it starts. Anything it starts before that is outside the job. The job code compiles and
  vets under `GOOS=windows`; none of it ran here.
- **Vendor facts (read 2026-10-08, from the `golang.org/x/sys` v0.48.0 and Go 1.27 `syscall`
  sources; learn.microsoft.com and man7.org are not reachable from the build machine):**
  - `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` kills every process in the job when its last handle
    closes. classifier-host's parser isolation already uses the same calls.
  - `SysProcAttr.Pdeathsig` (`PR_SET_PDEATHSIG`) exists on Linux only. Darwin gets the process
    group alone.
## 2026-10-08, task 11

- **Migration `0007-enforcement-rules.sql`** (renumbered at merge; numbers follow merge order).
  Migration proof: the integration branch's `schema.sql` plus
  it, and `main`'s `schema.sql` plus `0002` to it, each dump (`pg_dump --schema-only`) identically
  to the new `schema.sql`; it alone, and `0002` to it, applied over the new `schema.sql` change
  nothing.
- **The rule routes are `GET` and `PUT /admin/v1/settings/rules`, both `{"rules": [...]}`** in the
  bundle's spelling. A PUT without `rules` (or with `null`) is refused, so only `[]` clears the
  list. Refusals are 400 `invalid_request` naming the rule's index and field
  (`detail.index`, `detail.field`); an unknown member is `schema_violation`. The rules body cap is
  1 MiB (the other settings routes keep 16 KiB).
- **control-api refuses what the device would refuse**, so a saved list can never make devices
  reject the bundle: the `rule_id` pattern, a duplicate id, the action set, a message over 280
  characters, a link that is not `https://` with a host (lower-case scheme, as the CHECK), and a
  route outside `protocol.Route`. It also refuses a sanction value other than `sanctioned` or
  `unsanctioned`, an empty match value and more than 100 rules. Categories and tools are not
  checked against a set: the catalog does not exist yet, and a tool fingerprint is open.
- **A warn or block without a message is refused by the dashboard only**, as the brief places it;
  control-api, the database and the device accept it.
- **The labels CHECK is a trigger raising `check_violation` under the name
  `enforcement_rule_labels_known`**; it skips a `labels` value that is not an array and leaves it
  to the match shape CHECK, because a BEFORE trigger runs before the CHECKs. The shape CHECK uses
  strict jsonpath: lax mode unwraps arrays, so a lax filter would see the elements.
- **`message` is `NOT NULL DEFAULT ''`, `link` is nullable**; the bundle always carries `message`
  and omits an empty `link`, and every match list is sent, empty when it matches anything.
- **Replacing the list locks the tenant row** (`FOR NO KEY UPDATE`), then deletes and inserts in
  one transaction; `sac_control` holds `SELECT, INSERT, DELETE` and no `UPDATE`. The audit action
  is `tenant.enforcement_rules.set` with `previous` and `new` as rule lists.
- **`GET /admin/v1/settings` gains `data_classes`** (the `ref.data_class` codes, sorted): no
  existing route served them. The tools picker uses the Settings read's `tools` (the catalogue
  with the tenant's decisions); the category picker is the fixed set from DESIGN.md §4.
- **The route display names are the dashboard's** (`RULE_ROUTES` in `settings.js`, in fidelity
  order): no component held route display names before.
- **The dashboard reads the rules beside the settings**; when the rules read fails they are
  "not reported" and nothing can be saved over them. Edits build a draft that is sent whole with
  "Save rules"; "Discard changes" returns to the list read.
- **Not changed:** `ops.policy_bundle.feature_state` and the bundle's audit detail do not record
  the rules; nothing on the device evaluates them (task 12).

## 2026-10-08, task 25

- **Source.** `docs.anthropic.com` is blocked from the build machine (proxy 403), so the fixtures
  follow https://code.claude.com/docs/en/monitoring-usage (its `.md` form), page `dateModified`
  2026-10-08T19:00:28Z, read 2026-10-08. The page names features up to Claude Code v2.1.287; npm
  `latest` that day was 2.1.295, which the fixtures carry as `service.version`.
- **More events than the brief names.** The page documents 27 log events (from `user_prompt` to
  `managed_settings_resolved`, plus `system_prompt` under detailed beta tracing); all are in
  `logs-prompts-on.json`. `prompt_text` (a copy of `prompt`, same gate) is new and is a second
  attribute that carries prompt text.
- **Where the event name sits is undocumented.** The fixtures put `claude_code.<name>` in the log
  record body and `<name>` in `event.name`, and leave the OTLP `eventName` field unset; the
  logger's scope name is undocumented too (the fixtures use the meter name,
  `com.anthropic.claude_code`). The capture settles both.
- **Redacted prompt value.** The page says `prompt` is "redacted" by default and elsewhere that
  "only prompt length is recorded"; it shows `<REDACTED>` for `response` and the span prompt. The
  prompts-off fixture uses `<REDACTED>` for `prompt` and `prompt_text`.
- **Value types.** Quoted documented values (`"true"`) are strings, unquoted or Boolean ones are
  `boolValue`, integers `intValue`; `status_code` is an integer except on `auth`. Metrics are
  monotonic delta `asDouble` sums.
- **Left out:** custom `OTEL_RESOURCE_ATTRIBUTES` keys and the gateway-only `user.groups` and
  `identity.source`; spans (the brief asks for logs and metrics). Response text, the system
  prompt, MCP server names and hook commands are `SAC-PLACEHOLDER-<n>` as well as the paths,
  command lines and ids the brief names; the folder's README lists each one.

## 2026-10-08, task 12

- **Labels are known when the event's confidence is not degraded.** The pipeline passes
  `known = false` at M0 and whenever the classification ends degraded: over the body cap, the
  classifier unavailable, an attachment unclassified, or extraction failed so the whole body was
  classified. A client-generated request (nothing classified, confidence high) has known, empty
  labels. Only prompts call the hook.
- **Categories never match yet.** The bundle carries no catalog until task 14, so a rule with a
  non-empty `categories` list never matches (commented in `enforce`).
- **Sanction values** other than `sanctioned` and `unsanctioned` match nothing; the bundle
  validation does not refuse them.
- **`Observation.Decision` is gone**, replaced by `Enforce func(labels []string, known bool)
  protocol.Decision`. A prompt without a hook records no decision and the envelope refuses it, as a
  nil `Decision` did.
- **One hook builder, `enforce.Hook(bundles, route, tool, canEnforce)`**, used by every route. The
  TLS proxy uses its existing `Bundles` (the service passes the same function as `pipe.Bundles`);
  the loopback broker's `Decide` seam becomes `Bundles`, wired to `pipe.Bundles`; the native
  endpoint passes `pipe.Bundles`. The proxy's old default received the host; rules match the tool
  fingerprint.
- **The extension's decision is kept as sent.**
- **Outside the named packages:** `device/integration`'s frame conversion sets `Enforce` instead
  of `Decision`, and `device/README.md` lists the `enforce` package.
## 2026-10-08, task 26

- **Vendor fact: `tool_result` never says "denied".** https://code.claude.com/docs/en/monitoring-usage
  (the `.md` form, read 2026-10-08, the page task 25 used) says `tool_result` is "not emitted if the
  tool call was rejected" and its `decision_type` is always `accept`. A rejected call is a
  `tool_decision` with `decision` `reject`, so that event becomes the `tool_call` with outcome
  `denied` (tool name, no duration); an accepted `tool_decision` makes no record, its `tool_result`
  does. `tool_result` still reads `decision_type` `reject` as `denied`, as the brief asks.
- **Events.** `user_prompt`, `tool_result`, `tool_decision` (reject), `api_request` and `api_error`
  are converted; the other 23 events in the fixtures are listed in the test's `droppedEvents` with a
  reason, and an event in neither fails the test. The event name is read from `event.name`, then
  the OTLP `eventName`, then a body starting `claude_code.`, so the capture's placement works.
- **Attribute tables.** The mapped table is in the package (`attributes.go`) and records are read
  only through it, so a key missing from it is never read; the dropped table (157 keys, one reason
  each) is in the test, which walks resource and record attributes of every `logs-*.json` under
  `testdata/claude-code/`. A key dropped as "sent only on events that are not converted" fails the
  test if it appears on a converted event.
- **Prompt text.** The reader returns `prompt`, else `prompt_text`, ignoring `<REDACTED>`. With no
  text (prompt logging off at `m1`+) the extractor fails, so the record is degraded with the Tier S
  key rather than digesting the redaction marker. Spans are not converted.
- **`dedup_key` for activity** is sha256 of `tenant|device|app:claude_code|activity_type|<event.timestamp>/<event.sequence>|duration_ms`
  (`sha256:` prefix, as every key). Both of the record's own fields are used: the docs say
  `event.sequence` restarts with each process and that the pair orders a session's events.
- **Decision.** The prompt's `Observation.Enforce` is `enforce.Hook` over the bundle in force
  (`Config.Bundles`, wired from the pipeline), with `canEnforce` false: route `tool.otel` reports a
  prompt after Claude Code has sent it, so the matching rule is recorded as `logged`. Without a
  bundle source (tests) it records `policy.default`.
- **Registration.** `buildProviders` passes `otlp.Config.Normalizers` with Claude Code's first; task
  33 appends the generic normalizer after it. Failed records are logged with the event name and the
  error only; records refused before enrolment are counted by the pipeline, not logged.
## 2026-10-08, task 33

- **Semantic conventions followed: v1.41.0** (schema URL `https://opentelemetry.io/schemas/1.41.0`),
  read 2026-10-08. opentelemetry.io is not reachable from the build machine, so the docs were read
  in the `open-telemetry/semantic-conventions` repository on GitHub. v1.42.0 moved the GenAI
  conventions to `open-telemetry/semantic-conventions-genai`, which has no release yet; its `main`
  keeps every name used here. v1.41.0 is the last release with them and matches
  `go.opentelemetry.io/otel/semconv/v1.41.0`, which the tests use for the attribute names.
  - Model request: `gen_ai.operation.name` `chat`, `text_completion` or `generate_content`;
    `gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.usage.input_tokens`,
    `gen_ai.usage.output_tokens` (int). Tool call: `execute_tool` with `gen_ai.tool.name`.
  - Messages: `gen_ai.input.messages` (opt-in) is a list of `{role, parts}`, where a text part is
    `{type: "text", content}`. It is structured on events and may be a JSON string on spans. The
    event that carries it is `gen_ai.client.inference.operation.details`.
  - Older forms: `gen_ai.system` was renamed `gen_ai.provider.name` in v1.37.0. `gen_ai.prompt`
    (a JSON string in the OpenAI messages format, on the `gen_ai.content.prompt` span event)
    was replaced in v1.28.0 by log events such as `gen_ai.user.message` (body `{content}`).
    Those events were dropped in v1.37.0.
- **Outcome comes from the span status alone.** `STATUS_CODE_ERROR` gives `error`. Anything else
  gives `success`. `error.type` is not read.
- **Which records give what.** A span gives `agent_activity` when it is a model request or a tool
  call. A model-request span also gives a prompt when it carries the latest user message.
  Other GenAI spans give nothing: embeddings, `invoke_agent` and the rest. An agent span repeats
  the messages its model spans send, so a prompt from it would be reported twice. A log record
  gives only a prompt, because the activity belongs to its span.
- **What a prompt is.** A prompt is reported only when the **last** message of the request is the
  user's, and it holds that message's text parts, joined by new lines. When the request ends with
  an assistant or tool message, it is a later step of an agent loop, and the user's message went
  with an earlier request. The older per-message events are judged within one export: a
  `gen_ai.user.message` event followed by another message event of the same span is an earlier
  turn. An exporter that sends each record on its own would have every turn reported.
- **Counting.** The outcomes are counted on the `otel_receiver` row, the counter set the receiver
  shares with the normalizer through `otlp.Config.Counters`. Each envelope counts `emitted`. A
  pipeline refusal counts `dropped`, or `errors` when the identity is unresolved. A messages
  attribute that is not JSON counts `errors`. Every other span or log record that gives nothing
  counts `skipped_not_generative`, including generative records with nothing to report.
- **`exe:` fingerprint.** It is used only when the sender is `Resolved` (process and owner named).
  Otherwise it is `exe:unknown`, even when the image is known, as the brief says. The contract and
  ingest put no pattern on `tool_fingerprint`. The normalizer's `exe:` envelopes pass ingest's
  contract validator at `m0`, `m1` and `m3`. `ref.tool_catalogue` has no `exe:` row, so
  `ops.tool_display_name` shows "Unrecognised tool".
- **The catalog lookup waits for task 14.** `genai.Config.AppByExe(base)` takes the lower-case
  image base name. `buildProviders` leaves it nil, which matches nothing, because
  `Bundle.AppByExe` does not exist yet. Task 14 wires it to `Bundle.AppByExe(<platform>, base)`.
  The tests cover a catalog hit through the seam.
- **The decision.** A prompt's `Observation.Enforce` is `enforce.Hook` over the bundle in force
  (`Config.Bundles`, wired from the pipeline at merge with task 12), with `canEnforce` false:
  route `tool.otel` cannot enforce, so the matching rule is recorded as `logged`. Without a bundle
  source (tests) it records `policy.default`.
- **dedup_key for agent_activity.** It is `sha256:` plus the hex of
  sha256(`tenant|device|tool_fingerprint|activity_type|<trace id><span id>|duration_ms`), as in
  §3. The span's ids, in hex, are the tool's own event id. A span with no id uses its start time.
  An unknown duration is empty.
- **Tests.** Everything ran on Linux. With no person resolver, the end-to-end export through the
  real receiver gives `exe:unknown`. The resolved paths (catalog hit, `exe:` hash, POSIX and
  upper-case paths) replay the SDK's exported spans with a resolved `Sender`.

## 2026-10-08, task 44

- **Trust is installed by the agent, not by an MDM profile** (a deviation from the plan). The root
  is minted per device, so a tenant-wide Intune trusted-certificate profile cannot carry it.
  capture-core installs it in the machine Root store as before (`capture-core/trust`), and only
  while TLS inspection is on (`DESIGN.md` §10).
- **P-256.** certtostore v1.0.7 (tagged 2026-07-23) gives neither algorithm an explicit export
  policy. `WinCertStore.Generate` calls `NCryptCreatePersistedKey` with `NCRYPT_MACHINE_KEY_FLAG`
  and `NCRYPT_OVERWRITE_KEY_FLAG`, sets only `Length` (RSA) and `Key Usage`, then finalizes. So
  neither RSA 3072 nor P-256 is better placed. The choice is P-256, which the root and the leaves
  already use. certtostore packs an ECDSA signature for a digest as long as the curve's
  components, which holds for P-256 with SHA-256, the pair x509 uses.
- **Non-exportable is checked, not assumed.** The key is created without `NCRYPT_ALLOW_EXPORT_FLAG`.
  The store reads `NCRYPT_EXPORT_POLICY_PROPERTY` back after creating or opening the key and refuses
  a key with any export or archiving flag. An opened key that allows export is replaced.
- **Vendor facts** (read 2026-10-08 from the MicrosoftDocs `win32` and `sdk-api` sources on GitHub;
  learn.microsoft.com is not reachable from the build machine):
  - Key Storage Property Identifiers (ms.date 05/08/2025): `Export Policy` holds
    `NCRYPT_ALLOW_EXPORT_FLAG` 0x1, `..._PLAINTEXT_EXPORT_FLAG` 0x2, `..._ARCHIVING_FLAG` 0x4 and
    `..._PLAINTEXT_ARCHIVING_FLAG` 0x8.
  - `NCryptCreatePersistedKey` (05/29/2024): `NCRYPT_MACHINE_KEY_FLAG` makes a machine key;
    `NCRYPT_OVERWRITE_KEY_FLAG` replaces a key of the same name.
  - `NCryptExportKey` (08/21/2025) names `BCRYPT_PRIVATE_KEY_BLOB` and
    `NCRYPT_PKCS8_PRIVATE_KEY_BLOB`. It does not say which status a non-exportable key returns, and
    none of these pages states the provider's default export policy. The Windows test accepts
    `NTE_NOT_SUPPORTED`, `NTE_PERM` or `NTE_BAD_KEY_STATE` (certutil's error for a non-exportable
    key) and nothing else. It has not run yet.
- **The kept root moved into `tlsproxy`.** `ensureDeviceCA` (`cmd/capture-core/deviceca.go`) is now
  `tlsproxy.OpenDeviceCA`, and its tests moved with it, because replacing a file key needs the
  `caKeyStore` seam.
  - `tlsproxy.Config` takes the `*CA` in place of `CACertPEM`/`CAKeyPEM`, and `CA.KeyPEM` is gone:
    a CNG key has no PEM. The test for a half-configured PEM pair went with those fields.
  - The service tests replace the new `facilities.deviceCA` seam, so they touch no keystore.
- **Replacing a file key.** It runs on a platform whose key is in a keystore (Windows) when
  `device-ca/ca.key` exists. The steps:
  1. Mint a new root in CNG.
  2. Remove the old root, if the trust store holds it. The store removes only the root it last
     installed, so the old root is installed again (idempotent) and then removed. A root the store
     does not hold is never added.
  3. Delete the key file.
  4. Write the new `ca.pem`.

  The proxy's Start installs the new root while TLS inspection is on. If step 2 fails, the start
  fails and leaves the key file and old certificate in place, so the next start tries again.
- **Lifecycle.** The service opens the root once per process; the proxy's Start and Stop only
  install and remove trust. Renewal near expiry overwrites the CNG key. Deleting the key is the
  uninstall's (task 50); this task adds no delete path.

## 2026-10-08, task 35

- **One registration list.** `otlp/normalizers.Registered(Deps)` returns the normalizers in the
  receiver's order. `buildProviders` calls it, and both canary tests iterate over it.
  - A normalizer's canary fixtures are `otlp/testdata/privacy/<Name()>/{logs,traces}-*.json`:
    OTLP/JSON export requests in which `{{CANARY}}` marks the prompt text and `{{DROPPED_CANARY}}` a
    value the normalizer drops.
  - A registered normalizer with no fixtures, or with none carrying a dropped value, fails the test.
    A later normalizer adds a line to the list and fixture files, not test code.
  - `Deps.AppByExe` passes through to the generic normalizer. It stays nil until the catalog lookup
    is wired.
- **The fixture prompt is a sentence around the canary** ("Check the medical records of
  SAC-CANARY-...") rather than the canary alone. The shipped rules then label it `health`, so m1 and
  m2 are checked with real labels.
  - Claude Code: the documented `user_prompt` record with its prompt replaced. Its dropped value is
    `user.email`.
  - GenAI: a chat span with `gen_ai.input.messages` as a JSON string, and an inference-details event
    whose dropped values are `gen_ai.system_instructions` and `gen_ai.output.messages`.
- **A second canary marks the dropped value**, so that value is told apart from the prompt even in
  the content store or an excerpt. It is allowed nowhere at any mode.
- **Two harnesses, the same matrix.** Neither module can import the other's test code, so the
  harness is written in both.
  - `otlp/privacy_test.go` uses a stand-in classifier whose m2 excerpt is the matched canary, so the
    "only inside `content_excerpt.text`" accounting sees a hit.
  - `device/integration/otel_privacy_test.go` uses classifier-host under its supervisor. Its m2
    excerpt is the rule's match ("medical records"), which holds no canary. Every drained envelope
    is also checked against the contract.
  - At m2 each uploaded excerpt must be one the classifier returned, or the empty redacted window.
- **What is searched.** The canary is matched as text, hex, and base64 at each byte alignment.
  - The spool: each entry through `Peek` (payload and metadata), and the files as stored.
  - The content store: each object through `Get`, and the files as stored.
  - The `/v1/events` bodies as the edge received them, and one `/v1/health` report.
  - Every log line: receiver, normalizers, pipeline, drain and the classifier-host supervisor.
  - Each run also sends the export cut short after the canary. The receiver refuses it with 400, and
    its log line must not quote the body.
- **The health report is assembled in the test** from the providers' rows (the receiver's, and the
  classifier host's in integration), because the service's health channel is in package `main`. It
  is sent through the real `drain.ReportHealth`.
- **Exports are OTLP/HTTP JSON only.** Protobuf and gRPC decode to the same messages before routing.
- **Leaks found: none.** No normalizer or receiver change was needed. Logging the `prompt`
  attribute in the Claude Code normalizer fails both tests at every mode (`logs 1`).
- **Outside `otlp`:** `buildProviders` calls the list; `device/integration` gains
  `startClassifierHost` (which `realClassifier` wraps), and `startTLSIngest` also serves
  `/v1/health` and keeps request bodies. The integration module's `go.mod` gains indirect
  requirements (otlp, grpc, protobuf, genproto, grpc-gateway, x/net, godbus) at capture-core's
  versions.

## 2026-10-08, task 13

- **The hand-off sends the stored envelope's `payload`.** `handlePolicySync` decodes the bytes the
  store holds (it holds only verified envelopes) as `policy.SignedBundle` and sends `Payload`, the
  exact bytes the signature covered; the `policy` package is unchanged.
- **The extension asks capture-core more often than the brief names.** It resolves the mode itself
  from `tenant_default_mode` and `tool_modes`, and sends `mode_query` when the bundle carries
  `population_modes`, `device_modes`, `class_priors` or `required_notice_version`: each needs the
  user, the device or a class ceiling the extension does not hold. An unanswered `mode_query` is
  `m0`. Every input but the tenant default can only lower the mode, so the body lane is open
  (`<all_urls>`) exactly while the tenant default reads content.
- **A tool's `tool_modes` key is its fingerprint derived under the tenant default**, so one tool
  keeps one fingerprint whatever its own mode is.
- **The extension classifies nothing, so it matches rules with unknown labels**: a rule that lists
  `labels` never matches in the browser, the extension records `policy.default`/`logged`, and
  capture-core keeps the extension's decision (task 12). The "On the device" check (block
  `credential` on `ext.web_request`) cannot pass until the browser path has labels at decision
  time; that is not in this brief.
- **No match is `policy.default`** in the extension too (it was `NONE`).
- **Every extension lane uses the confirmation.** The metadata lane and WebSocket handshakes
  recorded `warned` without asking; they now ask as the body lane does.
- **`block` shows a notice**: a new content-script message `capture_block` renders "Request blocked"
  with the rule's message, its link (only an `https://` link is offered) and "Dismiss". A notice
  that could not be shown is counted as the error `block_notice_unavailable`; the request stays
  cancelled.
- **Removed with the fields:** the body-lane include list and `NEVER_BODY_BEARING` (applied only to
  that list), the bundle body cap (the extension's 1 MiB default applies), the release gating and
  the `hosts`, `paths`, `modes` and `min_size_bytes` matchers.
- **Golden frames.** The observation frames do not change. A golden `policy_bundle` frame is added
  in `device/integration/testdata/policy/`: capture-core's `native_test.go` checks that it answers a
  `policy_sync` with exactly that frame, and `golden-frames.test.mjs` applies it.
- **Shared evaluator cases** are `device/integration/testdata/enforce/cases.json`: task 12's 21 Go
  cases plus two extension-route cases. The category case covers only a bundle with no catalog;
  task 14 adds the catalog cases.
## 2026-10-08, task 27

- **Vendor facts** (code.claude.com `managed-settings`, `monitoring-usage`, `server-managed-settings`,
  `settings` and `setup`, `.md` form, read 2026-10-08; the pages name features up to v2.1.287, npm
  `latest` was 2.1.295; docs.anthropic.com is blocked from the build machine):
  - The Windows managed settings file is `C:\Program Files\ClaudeCode\managed-settings.json`;
    Claude Code no longer reads `C:\ProgramData\ClaudeCode\managed-settings.json`. The agent takes
    the folder from `FOLDERID_ProgramFiles`.
  - `env` merges per variable across admin sources (v2.1.223 and later), so the file's variables
    apply beside an HKLM or server-managed policy. The telemetry variables (`OTEL_EXPORTER_OTLP_*`,
    `OTEL_LOG_*`, `OTEL_LOGS_EXPORTER`) are one unit: a higher admin source that sets any of them
    wins them all. A customer's HKLM or claude.ai telemetry policy therefore overrides the agent's.
  - A managed file that is present but not a JSON object stops Claude Code from starting, so the
    writer leaves such a file (or one whose `env` is not an object) untouched and reports
    `config_write_failed`. An empty file is treated as `{}`.
  - Variable names are task 25's, unchanged: `CLAUDE_CODE_ENABLE_TELEMETRY`, `OTEL_LOGS_EXPORTER`,
    `OTEL_METRICS_EXPORTER`, `OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_EXPORTER_OTLP_ENDPOINT`,
    `OTEL_EXPORTER_OTLP_HEADERS` (`Authorization=Bearer <token>`, the documented form) and
    `OTEL_LOG_USER_PROMPTS`.
- **Deviation: the agent also owns `OTEL_LOG_ASSISTANT_RESPONSES=0`.** The monitoring page says
  that when it is unset it follows `OTEL_LOG_USER_PROMPTS`, so switching prompts on would also send
  assistant responses. The product records prompts only, so it is pinned off. It is backed up,
  restored and checked like the brief's keys.
- **`Installed()` checks the documented install locations directly**, because task 17's inventory
  facts do not exist yet; task 17 replaces it with its scanner functions. It looks in every folder
  under `FOLDERID_UserProfiles` for the native installer's `.local\bin\claude.exe` (setup page),
  the npm package under npm's default Windows prefix `AppData\Roaming\npm\node_modules\@anthropic-ai\claude-code`
  (npm `folders` docs), and a WinGet package folder `AppData\Local\Microsoft\WinGet\Packages\Anthropic.ClaudeCode_*`,
  and in `%ProgramFiles%\WinGet\Packages\Anthropic.ClaudeCode_*` (WinGet's portable-app spec; the
  Claude Code WinGet manifest could not be read from here, so that it is a portable package is
  assumed). A tool installed after the last apply is configured by the next bundle or restart.
- **The native-first exclusion's app lookup is a seam**, `tlsproxy.Config.AppByExe(base)` (lower-case
  image base name, as `genai.Config.AppByExe`). `buildProviders` leaves it nil, which matches
  nothing, because `Bundle.AppByExe` does not exist yet; task 14's merge wires it. The tests cover
  the exclusion through the seam with a fake process resolver. The tool table is
  `toolconfig.ToolForApp`; `toolconfig.NativelyCovered` applies §10's rule.
- **The `Writer` has a fifth method, `Holds(Desired)`**, which health uses to read the file back.
  `Apply` takes `Desired{HTTPListen, Token, LogPrompts}`.
- **Backup.** `toolconfig/claude_code/original` is JSON `{"present": bool, "content": <base64>}`,
  written with the state directory's protection before the first write and never replaced while it
  exists. A complete `Remove` deletes it, so the next switch-on backs up the file as it is then (a
  customer edit made while the agent's keys were out is not lost). `Remove` writes the backup's
  bytes back when the result holds the same JSON values; otherwise it writes the merged file.
- **Merging** keeps the order and the values of every other key and of the customer's `env`
  entries, and a UTF-8 byte order mark. The rewritten file is indented by two spaces, so another
  key's whitespace can change; a file that already holds the agent's values is not rewritten.
- **Access control.** The new file is written beside the old one and renamed over it. It gets the
  old file's DACL (and its protection) unless an allow entry gives a write right to anyone but
  SYSTEM, Administrators or TrustedInstaller; then, and for a new file, it gets
  `D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;BU)`. A new `ClaudeCode` folder inherits Program Files'
  read-only access for users.
- **Mode.** Prompt logging is on when `core.Resolve` gives `m1` or higher for `app:claude_code`
  under the bundle being applied, for the issued identity's device and user
  and no population, as the pipeline resolves.
- **Lifecycle and health.** `Start` applies, and a failed write leaves the provider running and
  `degraded`/`config_write_failed` (counted in `errors`) rather than failing the start; the next
  bundle retries. `ApplyPolicy` re-applies when the mode, address or token changes, or the last
  apply did not complete. `Stop` removes, also for a provider that never started (keys a crashed
  run left are removed), and the registry stops it at every service stop, as proxy.tls removes its
  root. Health reads the file: `healthy` when it holds the agent's keys, `degraded`/
  `config_write_failed` when it does not (task 34 adds tampering), `absent`/`tool_not_installed`
  without Claude Code. `tool_version_unsupported` is added to the vocabulary too: macOS and Linux
  report it.
- **`tool_config_claude_code`'s `ref.collector` row** supports no modes, as `user_helper`'s: it
  reads nothing. The new details are device-only in `check-vocab`.
- **Migration proof** (`0008-tool-config-claude-code.sql`, the next free number on this branch):
  the integration branch's `schema.sql` plus it, the new `schema.sql` plus it, the new `schema.sql`
  plus `0002`–`0008`, and `main`'s `schema.sql` plus `0002`–`0008` each dump
  (`pg_dump --schema-only`) identically to the new `schema.sql`, with identical `ref.collector`
  rows.
- **Not run here:** the Windows tests (`managedfile_windows_test.go`: the DACL of a new file, a kept
  DACL and a replaced one; `claudecode_windows_test.go`: the managed path and the install
  locations). They compile and vet under `GOOS=windows`.
