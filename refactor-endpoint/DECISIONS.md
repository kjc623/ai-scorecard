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
- **The catalog lookup.** `genai.Config.AppByExe(base)` takes the lower-case image base name.
  The service wires it to the bundle in force's `Bundle.AppByExe(<platform>, base)`, the first key
  when several apps match. The tests cover a catalog hit through the seam.
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
  image base name, as `genai.Config.AppByExe`). The service wires it to the bundle in force's
  `Bundle.AppByExe(<platform>, base)`, as the GenAI normalizer's. The tests cover the exclusion
  through the seam with a fake process resolver. The tool table is
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

## 2026-10-08, task 14, app catalog

- **Migration `0009-app-catalog.sql`** (renumbered at merge). Migration proof: the
  integration branch's `schema.sql` plus it, and `main`'s `schema.sql` plus `0002` to it, each dump
  (`pg_dump --schema-only`) identically to the new `schema.sql`, and so do the catalog rows
  (`ref.app`, `ref.app_signal`, `ref.tool_catalogue`); it alone, and `0002` to it, applied over the
  new `schema.sql` change nothing.
- **Where signals were checked.** learn.microsoft.com, the vendors' own sites (cursor.com,
  windsurf.com, lmstudio.ai, ollama.com, openai.com, jetbrains.com, code.visualstudio.com,
  docs.github.com, ai.google.dev, docs.aws.amazon.com) and formulae.brew.sh are blocked from the
  build machine. Reachable: code.claude.com, support.claude.com, registry.npmjs.org and GitHub,
  where several vendors publish their documentation source. Everything was read on 2026-10-08:
  - code.claude.com `setup`, `network-config`, `desktop`, `vs-code` (`.md` forms); support.claude.com
    "Deploy Claude Desktop for Windows" (updated 2026-07-13) and "Enterprise configuration for
    Claude Desktop".
  - npm `latest`: `@anthropic-ai/claude-code` 2.1.295 (`bin` `claude`), `@openai/codex` 0.162.0
    (`bin` `codex`; `bin/codex.js` runs `codex.exe` on Windows), `@google/gemini-cli` 0.63.0
    (`bin` `gemini`), `@github/copilot` 1.0.94 (`bin` `copilot`).
  - GitHub, default branch that day: `microsoft/vscode-docs` a884842 (`docs/setup/portable.md`,
    `docs/reference/variables-reference.md`, DateApproved 2026-10-07: `Code.exe`; marketplace
    item `GitHub.copilot-chat`); `microsoft/vscode` `build/win32/code.iss` (`AppPublisher=Microsoft
    Corporation`); `microsoft/vscode-copilot-chat` `package.json` 0.44.0 (publisher `GitHub`, name
    `copilot-chat`); `continuedev/continue` README and `extensions/vscode/package.json` 1.3.40
    (`Continue.continue`); `github/docs` 9f65179 `install-copilot-cli.md` (`npm install -g
    @github/copilot`); `openai/codex` 2c3156a README (`npm install -g @openai/codex`) and the
    `chatgpt.com/backend-api` base URL; `google-gemini/gemini-cli` README; `ollama/ollama`
    `docs/faq.mdx` (port 11434, model directories per platform), `docs/windows.mdx`,
    `app/ollama.iss` (`ollama app.exe`, `ollama.exe`); `lmstudio-ai/docs` 9b8bc20 (server on
    `localhost:1234`, models in `~/.lmstudio/models/`); `openai/openai-python` and
    `anthropics/anthropic-sdk-python` `_client.py` default base URLs; `googleapis/python-genai`
    `_api_client.py`; `MicrosoftDocs/azure-ai-docs` 6940f50 (`concepts-endpoints-2.md`, ms.date
    2026-07-31: `<resource-name>.openai.azure.com`; `<resource-name>.services.ai.azure.com` across
    the Foundry pages; `hub-configure-private-link.md`: `models.ai.azure.com` for serverless API
    deployments); `boto/botocore` `bedrock-runtime/2023-09-30/endpoint-rule-set-1.json` and
    `partitions.json`.
  - Package registries on GitHub: `microsoft/winget-pkgs` 6e0f54b (`Anthropic.Claude` 2.19675.1:
    MSIX family `Claude_pzs8sxrjxfjjc`, publisher `Anthropic, PBC`; `Anysphere.Cursor` 3.19.7:
    publisher `Anysphere`; `Codeium.Windsurf` 2.3.15: publisher `Codeium`;
    `Microsoft.VisualStudioCode` 1.140.0: publisher `Microsoft Corporation`; the JetBrains IDEs
    2026.2.x: publisher `JetBrains s.r.o.`, Apps & Features entries `<product> <version>`);
    `Homebrew/homebrew-cask` 5fa65f8 (`claude` 2.31226.0: `com.anthropic.claudefordesktop`;
    `chatgpt` 26.1002.52244: `com.openai.codex`; `chatgpt-classic`: `com.openai.chat`; `cursor`
    3.24.9: `com.todesktop.230313mzl4w4u92`; `visual-studio-code` 1.141.0: `com.microsoft.VSCode`).
- **Left out, not verifiable from the build machine** (the device phase can add them from a real
  install): Claude Desktop `windows_exe` and `windows_uninstall_name`; ChatGPT Desktop
  `windows_appx`, `windows_exe` and `publisher` (Store-only, no registry entry); Cursor
  `windows_exe`, `windows_uninstall_name` and `inference_domain`; Windsurf `windows_exe`,
  `windows_uninstall_name` and `macos_bundle_id` (no current cask); VS Code
  `windows_uninstall_name` (its name comes from the unpublished stable `product.json`); the
  `github.copilot` extension id (no current documentation names it; only `github.copilot-chat`);
  LM Studio `windows_exe` (`LM Studio.exe`, `lms.exe`).
- **Added beyond the brief's list, verified:** Claude Desktop `windows_appx`
  `Claude_pzs8sxrjxfjjc` (Anthropic deploys it as MSIX); ChatGPT Desktop's two bundle ids (the
  current app's `com.openai.codex` and the classic app's `com.openai.chat`); Codex's two release
  executables (`codex-x86_64-pc-windows-msvc.exe`, `codex-aarch64-pc-windows-msvc.exe`) beside
  `codex.exe`; Ollama's macOS and Linux model directories; Claude Desktop's `publisher`.
- **Brief values that differ from what was found:** `.aiplatform.googleapis.com` matches only
  the global Vertex host (`aiplatform.googleapis.com`):
  regional hosts are `<region>-aiplatform.googleapis.com`, which no host-suffix rule covers and no
  checked source lists by region, so they are left out. Bedrock has one row per region,
  `.bedrock-runtime.<region>.amazonaws.com`, for the 36 regions of the `aws` and `aws-us-gov`
  partitions (the rule set's `bedrock-runtime.{Region}.{dnsSuffix}`); FIPS and dual-stack hosts are
  left out. `lm_studio`'s model store is `~/.lmstudio/models` on `any` platform, as the docs write
  it; Ollama's Windows one is `%USERPROFILE%\.ollama\models` (`C:\Users\%username%\.ollama\models`
  in the FAQ).
- **Signal conventions:** `publisher` signals are `windows` (installer and Authenticode
  publisher); extension ids are stored lower case; the JetBrains `windows_uninstall_name` values
  are the product names that lead the entry name (`IntelliJ IDEA`, `JetBrains Rider`, ...), to be
  matched as a prefix. Vendors are lower-case slugs, as in the existing catalogue rows.
- **Lookups return every matching app key**, sorted (`[]string`; `api.anthropic.com` is both
  `anthropic_api` and `claude_code`). Exe, publisher and extension id compare case-insensitively;
  CLI binary, npm package and port exactly; domains by `hostMatches`. `Category` returns `""` for an
  unknown app. A rule category matches only a fingerprint `app:<app_key>` whose app is in the
  catalog.
- **The Settings read gains `app_categories`** (the distinct `ref.app.category` values, sorted), the
  only way the dashboard can know which categories the catalog holds; the picker offers those, in
  the fixed display order, and says "not reported" when the read lacks them. This touches
  control-api's `settings` handler and store `Settings`, outside the brief's named packages.
- **The catalog SQL orders with `COLLATE "C"`** and `compose` sorts again in Go byte order, so the
  served order never depends on the database's collation.

## 2026-10-08, task 15

- **`Record` gains `Route`.** The brief has the collector pass its route but gives `Emit` no
  parameter for it; the record carries it, and `Emit` refuses any route but `inv.scan`,
  `proc.detect` and `net.flow` (counting `errors`), as it refuses a record with no `AppKey` or no
  time.
- **`Stop` takes the collector's counter set**, `Stop(ctx, collector, Record)`: the brief's
  `Stop(ctx, Record)` has no set to count `observed` on. It changes no state; the start key alone
  decides whether a second start the same day is emitted.
- **The key's `user_ref` is `Record.UserRef`**, or `Person.UserRef` when `UserRef` is empty. The
  envelope is attributed to `Person` when set, else to `UserRef` alone (so `unattributed` is never
  replaced by the console user), else to the pipeline's identity.
- **The day boundary is the clock's UTC day; the key's day is `OccurredAt`'s.** The seen set is
  emptied when the clock's day changes, and the budget is the number of keys emitted that day, so
  a restart neither re-emits nor refills the budget.
- **A key is marked seen only after `Pipeline.Record` succeeds.** A failed record (`errors`) is
  retried on the next attempt; a record past the budget is not marked, so each later attempt that
  day counts `dropped` again. A failed write of the seen file counts `errors` and keeps the key in
  memory.
- **A seen file that cannot be read or decoded at construction** (bad JSON or an unparseable day)
  is replaced with an empty one for today, and counted as one `errors` on the first collector set
  the emitter next counts on, since construction has none.
- **No bundle in force means a budget of zero**: every record counts `dropped`.
- **`monotonic_offset_ms`** is milliseconds since the emitter was built, from its clock, as the
  OTLP normalizers do.
## 2026-10-08, task 14 fix 1

- **Shared category cases.** Task 14's eight category cases are in
  `device/integration/testdata/enforce/cases.json`, with a catalog of `claude_code`
  (`coding_agent`) and `cursor` (`ide`). The extension resolves `match.categories` as
  `Bundle.Category` does, and its policy cache now hands the evaluator the bundle's `catalog`
  beside the rules and sanctioned tools.
- **The `AppByExe` seams** (proxy.tls and the OTel normalizers) read the bundle in force at each
  call. The platform is the catalog's name for `runtime.GOOS`: `darwin` is `macos`. A signal for
  `any` platform matches too. Several matching apps give the first in catalog order.
- **The service tests check the seams the service builds**, not a proxied connection or an OTLP
  export: the connection-owner lookup that names the process is Windows-only. `buildProviders`
  takes the proxy configuration and the normalizer dependencies from two methods for this.

## 2026-10-08, task 45

- **`claudeai` and `chatgpt` wait for the device phase.** Their private backends' endpoints and
  body shapes are known only from captures on the reference VM, so this build has no parser for
  them: the registry reads requests to `claude.ai` and `chatgpt.com` with the generic parser (the
  moved extractor) until they are built on this task's fix branch. No capture step has run.
- **Streamed responses are not parsed** (a deviation from the plan's "and streamed responses").
  The agent reads requests only, so `Result` has no response text.
- **What a parser reads is the request's turn**: the trailing input after any trailing
  assistant/model message (Anthropic: the trailing user messages; Chat Completions: trailing user,
  tool and function messages; Responses: trailing user messages and tool call outputs, or a string
  `input`; Gemini: trailing `user` or role-less contents). Earlier messages were recorded with the
  request that first carried them. Tool results (`tool_result`, `mcp_tool_result`,
  `function_call_output`, `custom_tool_call_output`, a `tool` message, the string values of a
  Gemini `functionResponse`) are read as text, as are plain-text and content documents and search
  results. The generic extractor took the last user message, which re-read an old prompt when the
  turn was only tool results.
- **No attachment descriptors from the API parsers.** The envelope requires an attachment name,
  and the APIs' inline documents, files and images carry none or an optional one; `Result.Attachments`
  stays empty for them. Their bytes are not classified.
- **Two failures, two meanings.** `ErrUnknownShape` (the destination's parser matched but the body
  is in none of its documented shapes: an undocumented block, part, item type or role, or a missing
  required field) sets `content_unprocessable` on the `egress_proxy` row until a body is read
  again. `ErrNoText` (a known shape whose turn holds no text, or the generic parser finding
  nothing) degrades the record only. The loopback broker only uses the generic parser, so it never
  reports `content_unprocessable`. Unknown-shape errors name structure, never body values.
- **Isolation.** A parser's panic is recovered, the generic parser reads the request, and the route
  counts `errors`. The registry is assembled in `parsers/targets`, because each target package
  imports `parsers`.
- **Vendor facts** (read 2026-10-08; `docs.anthropic.com`, platform.openai.com,
  developers.openai.com and ai.google.dev are not reachable from the build machine):
  - Anthropic: the Messages API reference at platform.claude.com (`/docs/en/api/messages/create`
    and `/docs/en/api/beta/messages/create`, Markdown form), cross-checked with the generated types
    in `anthropics/anthropic-sdk-python` `main` at `50b78d17` (2026-10-08). `POST /v1/messages`;
    roles `user`, `assistant` and (beta) `system`.
  - OpenAI: `openapi.json` (spec 2.3.0) in `openai/openai-openapi` at `c7224137`.
    `POST /v1/chat/completions` and `POST /v1/responses`.
  - Gemini: `google/ai/generativelanguage/{v1,v1beta}` protos in `googleapis/googleapis` `master` at
    `6553725b`. `POST /{v1,v1beta}/{models,tunedModels,dynamic}/*:{generateContent,streamGenerateContent}`;
    roles `user` and `model`. Both proto3 JSON spellings of a field are accepted.
- **Depends on 43**, which is a device-phase spike not yet run; nothing here needed its result.

## 2026-10-08, task 36

- **The benchmark is `hooks/bench_test.go`, not `bench_windows_test.go`.** A `_windows` file name
  is a build constraint; the brief wants it build-tag-free and runnable on Linux as well.
- **How the benchmark's hook finds its service.** Hook mode dials the installed endpoint and trusts
  only the service's account, so a process the benchmark spawns could not reach an in-process
  service on a temporary endpoint run as the user. `cmd/capture-core` has a link-time variable,
  `main.hookBenchEndpoint`, empty in a release (`build.mjs` does not set it). The benchmark links
  it with `-ldflags "-X main.hookBenchEndpoint=..."`; hook mode then dials that endpoint and
  accepts it served by the hook's own account (or the service's). There is no runtime flag or
  environment variable. The endpoint is fixed per OS (`\\.\pipe\ShadowAICapture.native.hookbench`,
  `$TMPDIR/sac-hookbench/native.sock`) so binaries built on the PC work on the VM.
- **Prebuilt binaries for the VM.** Without the go tool the benchmark uses, beside the test
  binary: `capture-core(.exe)` linked as above, `classifier-host(.exe)`, the signed release in
  `classifier-release/` and its hex public key in `classifier-release.pub`. With the go tool it
  builds all of them, signing the shipped rules and model with a fresh key through
  `cmd/classifier-release`.
- **The test adapter's input is `hook_evaluate`'s own JSON**, whose `tool` names the tool it stands
  in for; it prints the decision as JSON and exits 0. `endpoint.tools` keys are a closed set without
  `test`, so a `test` tool would always be answered as a disabled tool.
- **`hook_evaluate` carries `prompt_bytes`**, the prompt's length, always: it is the envelope's
  `size_bytes`, and all an over-cap prompt carries. A prompt whose encoded frame would exceed the
  endpoint's 1 MiB frame is also sent as its length with `over_cap`.
- **What a hook prints when nothing was evaluated.** A stopped relay, hooks off and a tool whose
  hooks are off are answered `{action: allow}` with an empty `rule_id`; an evaluation with no
  matching rule says `policy.default`. A tool without an adapter, or a malformed `--hook` command
  line, prints nothing and exits 0. Hook mode exits with the adapter's exit code (the test
  adapter's is 0).
- **Fingerprint and correlation.** The tool key maps to `app:<key>` literally, as the brief says
  (so `copilot` is `app:copilot`); `session_id` becomes the record's client id.
- **Health.** `hook_relay` is healthy while started, absent while stopped; its counters are the
  pipeline's for route `tool.hook`, so a spool failure is its `dropped`.
- **The 30 ms classification can cascade under load.** `classifierlink` drops its connection (and
  so ends the classifier child) when a request outlives its budget, and the 30 ms request budget is
  also the host's own, so a host near its budget is dropped. In one of seven Linux benchmark runs,
  taken while other builds loaded the machine (load average above 5 on 4 cores), 51 of the 100
  AWS-key hooks were answered `allow` (labels unknown) and the p99 was 51 ms. Task 51 should
  measure this on the reference VM.
- **First benchmark numbers (Linux, not the reference VM):** Intel Xeon @ 2.80 GHz, 4 vCPU, 15 GiB,
  Go 1.27, `SAC_HOOK_BENCH=1 go test -run HookBench -v ./hooks/`, 1,100 runs each. Quiet runs: p50
  9.3–11.1 ms, p95 12.6–19.9 ms, p99 15.1–27.6 ms over all runs (worst single run 48 ms). The noisy
  run above: p50 11.1 ms, p95 21.9 ms, p99 51.3 ms.
- **No vendor facts.** This task ships only the test adapter; the Claude Code and Cursor formats are
  tasks 37 and 38.

## 2026-10-08, task 36 fix 1

- **A call that runs out of budget keeps the connection, and so the child.** Since task 48 the
  supervisor hands out the child's stdio once, so task 02's drop on a timeout ended
  classifier-host: it was restarted against the crash-loop budget and every call in between fell
  back to rules-only. Now each connection has one reader goroutine. Requests carry no sequence
  number: classifier-host answers every request frame exactly once, in the order it read them, or
  ends the connection (its doc comment and a pipelined-order test in `classify/server_test.go` pin
  this), so the n-th answer is the n-th request's. The link counts frames written and answers
  read, hands an answer only to the call registered for that ordinal, and discards answers for
  calls that gave up. Task 02's single-slot lock still admits one call at a time, and a request a
  caller gave up on before it was written is never written. No protocol or handshake change, so
  old and new hosts and cores interoperate.
- **A hung host is still dropped, after `stallLimit` (10 s).** When a call gives up and the host has
  answered nothing for 10 s while a request is outstanding, the connection is closed, which kills
  the child for the supervisor to restart. 10 s is far beyond any request budget (2 s at most).
- **Degraded clears on an answer.** A timed-out call still marks the link degraded
  (`host_unreachable`); a valid answer now clears it, since no reconnect follows a timeout any
  more. An answer that is not JSON is degraded `version_mismatch`, like an invalid answer, and keeps
  the connection (the framing is intact); before, it dropped the connection.
- **Benchmark** (`SAC_HOOK_BENCH=1 go test -run HookBench -v ./hooks/`, same Linux machine as task
  36; it now logs the supervisor's restarts). Load: 8 busy-loop shell processes on 4 vCPUs (the
  machine is shared, so load averages ran 4 to 14). Quiet, before: p50 7.8, p95 10.0, p99 11.9 ms,
  0 restarts; after: p50 8.2, p95 11.1, p99 14.6 ms, 0 restarts. Loaded, before (two runs): 100 and
  13 of 100 AWS-key hooks allowed, 5 restarts (crash loop) and 2 restarts, p99 594 and 112 ms.
  Loaded, after (two runs): 0 and 1 of 100 allowed, 0 restarts, p99 58.5 and 274 ms. The p99 under
  load is process scheduling, outside this fix; a hook allowed after the fix ran out of time (the
  30 ms classification or the hook's 400 ms deadline), which fails open by design.

## 2026-10-08, task 16

- **Migration `0011-inventory-scanner.sql`** (renumbered at merge if taken). Migration proof, on
  throwaway databases: the integration branch's `schema.sql` plus it, and `main`'s `schema.sql` plus
  `0002` to it, each dump (`pg_dump --schema-only`, and the rows of `ref.collector`, `ref.app`,
  `ref.app_signal` and `ref.tool_catalogue`) identically to the new `schema.sql`; it alone, and
  `0002` to it, applied over the new `schema.sql` change nothing.
- **Three catalog helpers are added to `policy.Bundle`**, which task 14 left without lookups for two
  of the four kinds the brief matches: `AppByUninstallName` (the signal's value alone, or followed by
  a space and more, case-insensitive, so the JetBrains product names match `IntelliJ IDEA 2026.2.1`),
  `AppByAppx` (the package family name, case-insensitive) and `HasSignal(app, platform, kind)`.
- **A publisher is the last resort.** An uninstall entry matches by its name, or by the executable
  its `DisplayIcon` or `InstallLocation` names; only when neither matches does its `Publisher`, and
  only for an app the catalog gives no `windows_exe` or `windows_uninstall_name`. Otherwise
  `Microsoft Corporation` would report every Microsoft product as `vscode`. With today's seed, Claude
  Desktop's and Cursor's per-user installs match by publisher (`Anthropic, PBC`, `Anysphere`); once
  the device phase adds their names or executables, the publisher stops counting for them.
- **An executable is taken only from a value that names an `.exe`** (quoted or not, with or without
  an icon index). The scan reads the registry only, so an `InstallLocation` folder yields none, nor
  does a Squirrel `DisplayIcon` (`app.ico`). Values are read unexpanded.
- **Machine-wide packages are `AppxAllUserStore\Applications`** (the provisioned packages, whose
  subkeys are package full names), attributed `unattributed`. The store's per-SID, `Staged`,
  `Deleted`, `EndOfLife`, `InboxApplications` and `Deprovisioned` subkeys are not read; each user's
  packages come from that user's repository. A subkey that is not a five-field package full name is
  skipped.
- **The machine's `Uninstall` key is read through both registry views** (`KEY_WOW64_64KEY` and
  `KEY_WOW64_32KEY`), not through the `Wow6432Node` path, which the documentation calls reserved.
- **Users** are the `HKEY_USERS` subkeys with `winproxy`'s SID prefixes (the rule is repeated in
  `inventory`, since `winproxy`'s is unexported), named by `LookupAccountSid`, and attributed through
  the service's `peerPerson`.
- **Version** is `DisplayVersion` or the full name's version segment; one longer than the
  envelope's 64 characters is left out. The record's `publisher` is not filled: the envelope's is a
  code-signing subject, and an uninstall entry's `Publisher` is not one.
- **Scheduling and health.** The first scan runs inside `Start`, then a loop rescans; an interval
  change applies from the next wait (the wait under way keeps its length). Health is `degraded` with
  `enumeration_partial` until a scan completes and after a partial one, `healthy` after a complete
  one, and `absent` with `tool_version_unsupported` where the platform has no scanner. An emit
  failure is counted by the emitter and does not change the state.
- **One discovery emitter per service**, built at its first use: it needs the device id, which
  exists once enrolled. Before that an emit counts `errors` and emits nothing.
- **Vendor facts (MicrosoftDocs on GitHub, read 2026-10-08; learn.microsoft.com is blocked):**
  `win32` `desktop-src/Msi/uninstall-registry-key.md` (ms.date 2018-05-31): the HKLM `Uninstall`
  key and `DisplayName`, `DisplayVersion`, `Publisher`, `InstallLocation` (`DisplayIcon` is not
  listed there); `desktop-src/WinProg64/registry-redirector.md` (ms.date 2018-05-31): `HKLM\Software`
  is redirected to `Wow6432Node`; `windows-dev-docs` `hub/apps/desktop/modernize/package-identity-overview.md`
  (ms.date 2023-01-10): full name `<Name>_<Version>_<Architecture>_<ResourceId>_<PublisherId>`,
  family name `<Name>_<PublisherId>`.
- **Not verified:** `AppxAllUserStore\Applications` and the per-user
  `Local Settings\...\AppModel\Repository\Packages`. No Microsoft documentation of either was
  reachable, and the PC's registry cannot be read from the build machine; they follow the brief.
  The device phase confirms them on the reference VM. The ChatGPT package family in the tests is a
  fixture value, not a catalog fact.
- **`device/protocol/envelope.go` is re-run through `gofmt`**: the integration branch's
  `CollectorHookRelay` line was misaligned.
## 2026-10-08, task 46

- **The body is held, classified and decided before the upstream is dialled.** At `m1`+ a body
  within the cap goes through the pipeline (parse, classify, rules, spool) first, so its event is
  recorded before the request is forwarded, and also when the upstream then fails. At `m0`, and
  for a body over the cap, nothing is classified, so the rules are evaluated without labels before
  forwarding (the decision the pipeline would record), and the event is recorded after the
  exchange as before. The upstream connection is opened only for a request that is forwarded, so
  a blocked request never opens one.
- **A blocked body that was not read is discarded, never held**: up to 256 KiB when its length is
  known, so the client finishes sending and reads the 403 rather than a reset, and to its end when
  it is chunked, which sizes it.
- **No decision taken by the pipeline** (no identity, a defect before the hook) is decided without
  labels, so a non-label block rule still applies; a label rule then cannot match and the request
  is carried.
- **`canEnforce` is per request**: the destination's own parser matched (not `generic`) and no
  `proxy.tls` kill switch is in force when the request is decided. Requests to `claude.ai` and
  `chatgpt.com` stay `logged` until their parsers exist; their block shapes follow their parsers,
  built from captures in the device phase (task 45's fix branch).
- **The displayed text** is the rule's message, then a space and its link. A rule with no message
  shows "Your organization's AI policy blocked this request." (block) or "... flagged this
  request." (warn), in the response and the notification, because the helper refuses an empty
  body.
- **Notification.** The session is named while the client is connected (the TCP owner's process),
  for a block or warning the proxy acts on only; the toast is shown in the background so the
  client never waits for the helper's 10 s answer. A failed lookup or notify counts `errors` on
  the `egress_proxy` row and is logged without content. With no `Session` or `Notify` (macOS,
  Linux) nothing is shown and nothing is counted.
- **The session seam**: `hostinfo.Process` gains `Session` (from `ProcessIdToSessionId`, 0 when it
  cannot be read; the helper never serves session 0). The service wires `tlsproxy.Config.Session`
  from the same connection owner as `Person`, and `Config.Notify` to `userhelper.Provider.Notify`.
- **Gemini's block shape** is added beside the brief's two: the error shape of Google's JSON APIs,
  `{"error":{"code":403,"message":...,"status":"PERMISSION_DENIED"}}`.
- **Vendor facts** (read 2026-10-08):
  - Anthropic: "Errors" at platform.claude.com (`/docs/en/api/errors.md`): a JSON body with
    top-level `type: "error"` and an `error` object with `type` and `message`; 403 is
    `permission_error`. Its `request_id` is left out: the proxy is not the API.
  - OpenAI: `openapi.yaml` (spec 2.3.0) in `openai/openai-openapi` at `c7224137`: `ErrorResponse`
    is `{error: Error}`, and `Error` requires `message`, `type`, `param` and `code` (the last two
    nullable), so both are sent as `null` beside the brief's `type: "policy_violation"`.
  - Gemini: AIP-193 (`aip-dev/google.aip.dev` `master` at `615875bf`), the HTTP/JSON error
    `{"error":{"code","message","status","details"}}` with the HTTP status as `code`;
    `google/rpc/code.proto` (`googleapis/googleapis` `master` at `6553725b`) maps
    `PERMISSION_DENIED` to 403. ai.google.dev is not reachable from the build machine.
## 2026-10-08, task 19

- **Dependency: `github.com/0xrawsec/golang-etw` v1.6.2** (the latest tag, 2022-09-22, commit
  `4b60579`), through the module proxy. Its `etw` package is Windows-only and pure Go; it brings
  `github.com/0xrawsec/golang-utils` v1.3.1 (its `log` and `datastructs`) as an indirect
  dependency. Only `etwsession` imports it.
- **Vendor fact, Microsoft-Windows-Kernel-Process** (`{22FB2CD6-0E7B-422B-A0C7-2FAD1FD0E716}`):
  keyword `WINEVENT_KEYWORD_PROCESS` is `0x10`; event 1 is ProcessStart (`ProcessID`,
  `CreateTime`, `ParentProcessID`, `SessionID`, `Flags`, `ImageName` as a kernel path such as
  `\Device\HarddiskVolume3\...\app.exe`, and in later versions more); event 2 is ProcessStop
  (`ProcessID` first, then times, counters and `ImageName`); both informational. Checked against
  the provider manifest as extracted from Windows 10 builds 17134 and 18990
  (`repnz/etw-providers-docs` on GitHub) and golang-etw v1.6.2's own `etwdump` (events 1 and 2,
  `ProcessID`, `ImageName`). Microsoft's documentation on GitHub (`MicrosoftDocs/sdk-api`,
  `MicrosoftDocs/win32`) covers the ETW API (StartTrace and EVENT_TRACE_PROPERTIES ms.date
  2018-12-05, ControlTrace 2022-08-04) but has no payload reference for this provider. The PC's
  manifest (`wevtutil gp`) was not read: this build ran on Linux. The device phase confirms the
  fields on the reference VM.
- **A stale session is stopped by name before the new one starts**, with properties sized for the
  name ControlTrace writes back. golang-etw's own fallback on `ERROR_ALREADY_EXISTS` passes
  ControlTrace a copy of the properties without that room. Close does not stop a session that has
  already ended, since its handle may by then name a newer session.
- **"Stops delivering" means the session ended** (its ProcessTrace returned), not a quiet period.
  The monitor is then `degraded` with `etw_session_failed` and opens the session again every
  minute; one that cannot be opened at start is degraded the same way. Where there is no ETW it is
  `absent` with `etw_session_failed` (`DESIGN.md` §12).
- **A running app is an instance, not a process.** A catalog process whose parent is a running
  process of the same app joins that app's instance; the instance is reported once and stops with
  its last process. Electron apps (Claude Desktop, Cursor, VS Code) run many processes of one
  executable, and a Squirrel launcher exits after starting the app; the brief's device check
  expects one log line for the start and one for the stop.
- **The start time is the event's timestamp** (when ProcessStart was written), not `CreateTime`;
  for an app found by the start-up process listing it is the time of the listing.
- **When several apps share the executable name**, the app whose catalog `publisher` is the
  observed signer wins, else the first in catalog order. The seed catalog lists `claude.exe` only
  for `claude_code`, so Claude Desktop's process is reported as `app:claude_code` until the catalog
  gives it a `windows_exe` signal; the brief's device step expects `app:claude_desktop`.
- **The version is the image's VS_FIXEDFILEINFO file version** (`major.minor.build.revision`),
  read with GetFileVersionInfo, which maps the file as data. It lives in `procmon` because task 17,
  which also needs it, is not built yet.
- **A process that cannot be opened** (it already exited, or is protected) is still recorded, with
  `user_ref` `unattributed` and no version or signer.
- **The running set is reconciled with the process list** (`CreateToolhelp32Snapshot`) at every
  session open and every 10 minutes, so a stop or start the session lost does not leave it wrong.
- **One discovery emitter per service.** `cmd/capture-core` builds it at the first record after
  enrolment (it needs the issued device id) and rebuilds it if the device id changes; before
  enrolment a record counts `errors`. Every discovery collector must use this one emitter: two
  would overwrite each other's seen file.
- **The service opens the session through a facility** (`processEvents`), which the service tests
  leave unset, so a test run, even elevated, never replaces an installed agent's
  `ShadowAICapture-process` session.
- **Counters and log.** A new instance counts `observed` (its stop counts `observed` in the
  emitter); an event with no readable `ProcessID` counts `errors`. The log has
  `procmon: app:<key> started (pid N)` and `... stopped (pid N)` at `info`: the app key and the PID
  only.
## 2026-10-08, task 37

- **Vendor facts** (code.claude.com `hooks`, `settings-reference`, `managed-settings`,
  `tools-reference` and `changelog`, `.md` form, read 2026-10-08; the changelog's latest entry is
  2.1.295 of 2026-10-08; docs.anthropic.com is blocked from the build machine; no installed version
  could be checked):
  - The Windows managed file is still `C:\Program Files\ClaudeCode\managed-settings.json`.
  - Hooks are `hooks.<event>[]` matcher groups `{matcher, hooks: [{type: "command", command, args,
    timeout}]}`. `timeout` is in seconds (default 600, and 30 on `UserPromptSubmit`).
    `UserPromptSubmit` takes no matcher (one is ignored). A matcher with characters other than
    letters, digits, `_`, `-`, spaces, `,` and `|` is a JavaScript regular expression tested
    unanchored.
  - Stdin: the common fields `session_id`, `prompt_id`, `transcript_path`, `cwd`,
    `permission_mode`, `hook_event_name`; `UserPromptSubmit` adds `prompt` (pasted text expanded);
    `PreToolUse` adds `tool_name`, `tool_input`, `tool_use_id`, and `mcp_server` for MCP tools
    (v2.1.274 and later).
  - Blocking a prompt: exit code 2 with the reason on stderr, or JSON `{"decision": "block",
    "reason": ...}` with exit code 0; either way the reason is shown to the user and not added to
    Claude's context. The adapter uses the JSON with exit code 0, as `DESIGN.md` §9 has hooks exit 0.
    By default the block message ends with the submitted prompt and is written to the session
    transcript, so the adapter also sets `hookSpecificOutput.suppressOriginalPrompt: true`.
  - `PreToolUse` deny: `hookSpecificOutput {hookEventName: "PreToolUse", permissionDecision: "deny",
    permissionDecisionReason}`. The reason goes to Claude, not the user, so the adapter also sets
    `systemMessage`, the universal field "shown to the user".
  - Warn: `systemMessage` with no decision. Allow: no output and exit code 0 (on `UserPromptSubmit`
    plain stdout would be added to Claude's context).
  - `allowManagedHooksOnly` (managed scope only, boolean, default unset): with `true` only managed
    hooks, Agent SDK hooks and hooks of plugins the managed settings force-enable run. It also
    disables command-sourced plugins and marketplace `headersHelper` commands (unless
    `disableCommandPluginSources` is `false`), narrows `statusLine` and `fileSuggestion` to managed
    settings, and stops `/goal`.
  - Under the default `managedSourcesBehavior` (`first-wins`) the file's hooks apply only when no
    higher admin source (server-managed settings, or HKLM `SOFTWARE\Policies\ClaudeCode`) carries a
    policy key; `env` still merges per variable. A customer with such a policy gets no agent hooks,
    and the `tool_config_claude_code` row cannot see that. Not addressed here.
- **Deviation: the hook runs in exec form.** The brief's command is one shell string. Claude Code runs
  a shell-form command through Git Bash, or PowerShell where Git Bash is not installed, and in
  PowerShell a quoted path followed by arguments does not parse. The agent writes `command` as the
  executable's path and `args` as `["--hook", "claude_code", "<event>"]` (exec form, v2.1.139 and
  later), which Claude Code spawns without a shell; the documentation says an absolute path with
  spaces is valid there.
- **Deviation: the `PreToolUse` matcher is `^(Bash|PowerShell|WebFetch|mcp__.*)$`.** On Windows
  Claude Code routes shell commands through its PowerShell tool wherever that is enabled, and without
  Git Bash it does not register the Bash tool at all; the documentation says a hook matching `Bash`
  alone never fires there. The anchors keep the unanchored regular expression to these tools.
- **The toggle covers both switch sets.** `tool_config_claude_code` is on while either Claude Code's
  OTel (with the receiver on) or its hooks (with the relay on) is in effect, and writes only what is
  on: with OTel alone no hooks, with hooks alone no telemetry variables (the agent's variables go back
  to the backup's values). `managed_only` takes effect only while the agent's hooks are declared;
  with the hooks off, `allowManagedHooksOnly` is never set. With both off the registry stops the
  provider, which restores the file. Each bundle re-applies when anything in `Desired` changes.
- **What the agent owns.** Its telemetry variables, its own hook groups and `allowManagedHooksOnly`.
  A key the bundle does not ask for holds the backup's value, or is absent when the backup has none;
  a change someone makes to one of those keys while the agent keeps a backup is not kept, as for the
  telemetry variables before. An agent group is a matcher group whose handlers are all command hooks
  with `args` starting `--hook claude_code` and a `capture-core(.exe)` command, so one written from an
  earlier install path is replaced in place, and a customer's hook is never touched. A `hooks` that
  is not an object, or an event's list that is not an array, leaves the file untouched and reports
  `config_write_failed`, as a malformed `env` does.
- **The executable's path** is `toolconfig.Config.Executable`, `os.Executable` by default, so the
  service's wiring is unchanged. A path that cannot be read reports `config_write_failed`.
- **`Desired` gains `OTel`, `Hooks`, `HookCommand` and `ManagedOnly`**; the OTel fields are zero
  while OTel is off, so a mode change with only the hooks on rewrites nothing.
- **The stdin fixtures are documented, not captured** (`hooks/testdata/claude-code/documented/`,
  marked so in its README). The device phase replaces them with input captured from the installed
  version.
- **Not run here:** nothing new is Windows-only; the existing Windows tests of `toolconfig` compile
  and vet under `GOOS=windows`.

## 2026-10-08, task 17

- **Vendor facts: the Claude Code native install** (code.claude.com `setup` and `troubleshoot-install`,
  `.md` form, read 2026-10-08; docs.anthropic.com and learn.microsoft.com are blocked here). The
  Windows launcher is `%USERPROFILE%\.local\bin\claude.exe`; `~/.local/share/claude/` holds "each
  version it downloads" as `versions/<VERSION>` (the Windows uninstall removes
  `%USERPROFILE%\.local\share\claude`). On Windows an update renames the old `claude.exe` aside and
  moves the new one into place, so the launcher is a copy, not a symlink. The version is the highest
  release-named entry of `.local\share\claude\versions` (with or without `.exe`); failing that, the
  launcher's PE file version; failing that, empty. **Not verified:** how the entries are named on
  Windows (the page writes the folder for macOS and Linux). The device phase confirms it.
- **Vendor facts: npm** (`npm/cli` branch `latest`, `docs/lib/content/configuring-npm/folders.md`
  and `npmrc.md`, read 2026-10-08): the default Windows prefix is `%AppData%\npm`, global packages are
  in `{prefix}\node_modules` (no `lib`), and scoped ones are one folder deeper. `.npmrc` is ini
  (`;` and `#` comments) with `${VAR}` replaced. The scanner takes the last top-level `prefix`. It
  expands a leading `~` the way npm's path options do, and does not use a prefix that is relative or
  names an unset variable. `NPM_CONFIG_PREFIX` and `userconfig` are not read.
- **Deviation: pipx's home.** `pypa/pipx` `main` `src/pipx/paths.py` (read 2026-10-08) uses
  `~\.local\pipx` when it exists, else `~\pipx` on Windows when it exists, else
  `platformdirs.user_data_path("pipx")`, which is `%LOCALAPPDATA%\pipx\pipx` (`tox-dev/platformdirs`
  `windows.py`). The brief names only the first. The scanner chooses as pipx does (`PIPX_HOME`, a user
  variable, is not read). Venvs keep packages in `Lib\site-packages`. Names compare as Python
  normalizes them, through a new `policy.Bundle.AppByPipx`. The catalog has no `pipx_package` signal
  yet, so pipx finds nothing until one is added. The tests use a fixture app.
- **PATH.** A file `<name>.exe`, `.cmd` or `.ps1` (any case) whose name is a `cli_binary` value
  matches. npm's extensionless shell shim does not. A user's own PATH is read only from a loaded hive.
  Its `%VAR%`s expand with the profile's `USERPROFILE`, `APPDATA` and `LOCALAPPDATA` (the default
  `AppData` folders; folder redirection is not followed), then the service's environment. The machine
  PATH is `HKLM\...\Session Manager\Environment\Path` (documented: `ProcThread/environment-variables.md`,
  ms.date 2025-07-14). Relative entries and network paths are skipped, so the service never reads a
  share as the machine account. As the brief says, a hit on the machine PATH belongs to each profile's
  owner, not to `unattributed`.
- **One record per app and profile from the PATH.** A PATH hit for an app that the profile's npm,
  native or pipx location already found is not reported, and the first PATH folder that has an app
  wins. npm's shims and the native launcher sit on the PATH, so otherwise every such install would
  also appear with an empty or four-part version.
- **The PE version** is `VS_FIXEDFILEINFO`'s file version as `major.minor.build.revision` (for example
  `1.0.94.0`), not a `StringFileInfo` string. A file with no version resource gives an empty version,
  not an error. Documented in `MicrosoftDocs/sdk-api` `GetFileVersionInfoW` and `VS_FIXEDFILEINFO`
  (ms.date 2018-12-05). On Linux the read is a fake. The Windows tests use `cmd.exe` and `kernel32.dll`:
  a Go test binary has no version resource, and the tests check that it reads as empty.
- **Profiles** come from `HKLM\...\ProfileList\<SID>\ProfileImagePath`: people's SIDs only (task 16's
  rule), a folder directly under `FOLDERID_UserProfiles`, and a hive that opens under `HKEY_USERS` or an
  `NTUSER.DAT`. **Not verified:** no Microsoft documentation of `ProfileList` was reachable. The
  device phase confirms it.
- **Counters.** `inventory.go` gains an optional `CountedScanner`. The provider scans such a scanner
  through `ScanCounted` with its own counter set, so the CLI scanner counts `observed` for each file it
  checks: each `package.json` read, the native launcher, the matching `dist-info` and each matching
  PATH file. Other PATH entries are listed, not checked. A `package.json` or `.npmrc` over 1 MiB, or
  one that is not a regular file, is an error and is not read.
- **Claude Code's `Installed()`** is now a seam. `NewClaudeCodeWriter` takes `installed func() bool`,
  and the service wires it to `inventory.CLIInstalled(<bundle in force>, "claude_code")`, which runs
  the CLI scan on demand (also when `endpoint.inventory` is off) and is false off Windows. Task 27's
  direct checks are gone, including the WinGet `Packages` folders: a WinGet portable install is found
  through WinGet's `Links` folder on the PATH (assumed, as task 27 assumed it is portable). Task 27's
  Windows-only `TestClaudeCodeInstalledUnder` tested the removed function and is removed with it. Its
  native and npm cases are in the inventory tests, and `TestClaudeCodeInstalledIsTheSeam` covers the
  seam.
- **Not run here:** `cli_windows_test.go` (`TestFileVersion`, `TestCLIScannerPathHitWithAFileVersion`,
  `TestSystemCLIHostReads`) and the remaining Windows tests in `toolconfig`. They compile and vet under
  `GOOS=windows`.

## 2026-10-08, task 21

- **Migration `0012-flow-monitor.sql`** (renumbered at merge if taken) adds the `flow_monitor`
  collector row. Migration proof, on throwaway databases: the integration branch's `schema.sql` plus
  it, and `main`'s `schema.sql` plus `0002` to it, each dump (`pg_dump --schema-only`, and the rows
  of `ref.collector`, `ref.app`, `ref.app_signal`, `ref.tool_catalogue` and `ref.route_fidelity`)
  identically to the new `schema.sql`; it alone, and `0002` to it, applied over the new `schema.sql`
  change nothing.
- **Vendor fact, Microsoft-Windows-DNS-Client** (`{1C95126E-7EEA-49A9-A3FE-A378B03DDB4D}`): event
  3008 ("DNS query is completed for the name %1, type %2, query options %3 with status %4 Results
  %5"), informational, no keyword, fields `QueryName`, `QueryType`, `QueryOptions`, `QueryStatus`,
  `QueryResults` (strings and integers; no process id in the payload). Checked against the provider
  manifests as extracted from Windows 10 builds 17134 and 18990 (`repnz/etw-providers-docs`) and
  Windows 11 22H2 build 22621.963 (`nasbench/EVTX-ETW-Resources`, the `wevtutil gp` content with
  in and out types). `QueryResults` is the answers separated by `;`, an IPv4 address as
  `::ffff:a.b.c.d`, any other record as `type:  <n> <data>`: no Microsoft source documents it; it
  is the format of Sysmon's DNS query event, which carries this field, as Elastic's winlogbeat
  Sysmon module (branch 7.17) parses it. The PC's manifests were not read: this build ran on Linux.
  learn.microsoft.com is blocked here. The device phase confirms the fields on the reference VM.
- **The asking process is the event header's process id.** The DNS client writes 3008 in the
  context of the process that called it, so `etwsession.Event` gains `PID`, the header's process id.
  If the device phase finds an answer written under another process (the DNS Client service), the
  any-process fallback still attributes the connect to the domain.
- **Vendor fact, Microsoft-Windows-Kernel-Network** (`{7DD42A49-5329-4832-8DFD-43D979153A88}`):
  keywords `KERNEL_NETWORK_KEYWORD_IPV4` `0x10` and `KERNEL_NETWORK_KEYWORD_IPV6` `0x20`; events 12
  (TCPv4) and 28 (TCPv6) "Connection attempted", informational, fields `PID`, `size`, `daddr`,
  `saddr` (`win:UInt32` as `win:IPv4`, or 16-byte `win:Binary` as `win:IPv6`), `dport`, `sport`
  (`win:UInt16` as `win:Port`), then `mss`, `sackopt`, `tsopt`, `wsopt`, `rcvwin`, `rcvwinscale`,
  `sndwinscale`, `seqnum`, `connid`. Same sources as DNS-Client (identical in 17134 and 18990;
  22621 adds the out types). golang-etw v1.6.2 formats each field with `TdhFormatProperty`, so
  addresses arrive as text, and reads a `win:Binary` field shown as `win:IPv6` as a 16-byte
  address. The connecting process is the payload's `PID`, not the header's: the kernel
  writes in whatever context it runs. The port is read by nothing: the record has no port.
- **A domain two catalog apps share is the first app's in catalog order** (`app_key` order), as
  the other `AppBy` lookups: `api.anthropic.com` is `app:anthropic_api`, not `app:claude_code`.
- **The latest answer for an address wins**, in the connecting process's map and then across
  processes. Expiry is measured on event times; expired answers are pruned at most once a minute.
  Only answers for catalog domains are kept, and a query whose `QueryStatus` is not 0 is ignored.
- **A process that cannot be opened** is still recorded, `unattributed` and with no signer, as in the
  process monitor.
- **Counters and log.** An attributed connect counts `observed`; a connect with no readable `PID` or
  `daddr` counts `errors`; DNS answers count nothing. The log has the session's state changes and a
  record that could not be emitted (app key and PID), never a line per connection.
- **The service opens the session through a facility** (`flowEvents`), which the service tests leave
  unset, as `processEvents`. The elevated Windows test resolves `api.openai.com` and connects to it
  on 443, sending nothing.
## 2026-10-08, task 40

- **Session ids (vendor fact).** code.claude.com `hooks` and `monitoring-usage` (`.md` form, read
  2026-10-08, describing versions up to 2.1.295): the hook's `session_id` is the "current session
  identifier" and the OTel `session.id` the "unique session identifier", on every event by default.
  Neither page says in so many words that they are the same value; both follow the same session
  (a new id on `/clear`, the same id on a resume). Task 25's OTel fixtures and task 37's hook
  fixtures were written independently and carry different ids, so they neither confirm nor refute
  it. The merge assumes they are equal; the device phase confirms it on the installed version.
  The documentation also says the hook's `prompt_id` matches the OTel `prompt.id`; the brief's
  key uses the session id, so `prompt_id` is not read.
- **Lengths (vendor fact).** `user_prompt`'s `prompt_length` is documented only as "Length of the
  prompt"; the span's `user_prompt_length` is "in characters". The hook's length is in bytes. If
  `prompt_length` counts characters, an m0 prompt with non-ASCII text does not merge and is
  recorded on both routes. The device phase checks it.
- **What is held.** The buffer holds a prompt on `tool.hook` or `tool.otel` that carries a session
  id (`ClientID`). Everything else passes straight through: `agent_activity` (`Record`), a prompt
  on another route, and a prompt without a session id, which is every prompt of the generic OTel
  normalizer. A Cursor hook prompt is held and released alone after 10 s.
- **Tool-call checks do not merge.** The relay records a `PreToolUse` check as a prompt with the
  tool input as its text. OTel never reports one as `user_prompt`, and at m0 one could pair with a
  prompt of the same length, so the relay sends only prompts without a `tool_name` to the merge
  (`hooks.Config.Prompts`); tool-call checks go to the pipeline as before.
- **Matching.** With both texts read (m1+), the key is the sha256 of the text. When either side has
  no text (m0, a hook prompt over the frame cap, or OTel with prompt logging off) the two match on
  the length in bytes with `occurred_at` within 2 s. The text is read only when the mode the
  pipeline resolves for the record reads content. A pair is the earliest held record of the other
  route with the same tool fingerprint and session id; two records of the same route never pair.
- **The merged record** is the hook's observation (route, decision, person, size, session) with
  the earlier `occurred_at`, and the OTel side's reader, extractor and over-cap flag when only it
  holds the text. It is counted once, on `tool.hook`; the OTel side's prompt is not counted on
  `tool.otel`. A record released alone keeps its own route, decision and person.
- **Held text.** At m1+ the buffer reads the text once into its own buffer, hands the pipeline a
  reader that returns a copy, and zeroes the buffer once the record is released, merged or not.
  Nothing in `merge` logs or returns text; a failed release logs the tool fingerprint, the route
  and the outcome's reason.
- **Privacy test coverage.** Task 35's `otlp/privacy_test.go` builds the registered normalizers
  over the pipeline directly, so the merge is not in its path; the buffer passes the same reader
  on and writes nothing itself. `merge`'s own test sends canary prompts through every release
  path (merged, expired, closed) and checks that the log never quotes them and that the text is
  cleared after each release.
- **Shutdown.** `buildProviders` closes the buffer when the background loops end, which `Stop`
  waits for before the shutdown sequence stops the providers and drains the spool. Close releases
  every held prompt unmerged, and from then on prompts pass straight through.
- **The hold is a real 10 s timer per record**; the tests run it on `testing/synctest`'s fake clock.
- **Outside `merge`, `hooks` and the wiring:** `cmd/capture-core/hook_test.go`'s
  `TestHookIsDecidedAndRecordedByTheService` waits up to `merge.HoldFor` plus its 10 s for the
  delivered hook prompt, which no OTel record joins and so goes on after its hold.

## 2026-10-08, task 18

- **Vendor facts: VS Code** (GitHub, default branch, read 2026-10-08; code.visualstudio.com is
  blocked here). `microsoft/vscode-docs` `docs/configure/extensions/extension-marketplace.md`
  (DateApproved 10/7/2026): extensions are in `%USERPROFILE%\.vscode\extensions` on Windows.
  `microsoft/vscode` source: `environmentService.ts` (`extensionsPath` is
  `<home>\<dataFolderName>\extensions`), `node/userDataProfile.ts` (the default profile's list is
  that folder's `extensions.json`), `extensionsProfileScannerService.ts` (the list is a JSON array of
  `{identifier:{id,uuid}, version, location, relativeLocation, metadata}`; an empty file is an empty
  list) and `extensionManagementUtil.ts` (`ExtensionKey`: a folder is named
  `<id>-<version>[-<targetPlatform>]`, parsed by `^([^.]+\..+)-(\d+\.\d+\.\d+)(-(.+))?$`, which the
  folder-name fallback uses, so a platform suffix such as `-win32-x64` is not part of the version).
  Only the default profile's list is read; VS Code's other profiles keep their own lists under
  `%APPDATA%\Code\User\profiles` and are not scanned.
- **Not verified: Cursor and Windsurf.** cursor.com, forum.cursor.com and docs.windsurf.com are
  blocked here and neither vendor publishes these paths on GitHub. `%USERPROFILE%\.cursor\extensions`
  is supported only by a web-search snippet of a Cursor forum staff reply;
  `%USERPROFILE%\.windsurf\extensions` is the brief's value with no source found. Both are VS Code
  forks with VS Code's `extensions.json` and folder naming, assumed. The device phase confirms Cursor
  on the reference VM; Windsurf is not in the VM's Needs, so it stays unverified.
- **Vendor facts: JetBrains** (GitHub, default branch, read 2026-10-08; jetbrains.com is blocked
  here). `JetBrains/intellij-community` `PathManager.java` (`getDefaultPluginPathFor`: on Windows
  `%APPDATA%\JetBrains\<selector>\plugins`, falling back to `<home>\AppData\Roaming` when `APPDATA`
  is unset) and `PluginDescriptorLoader.kt` (a plugin folder's descriptor is the
  `META-INF/plugin.xml` of the first jar in `lib` that has one, more likely jars first).
  `JetBrains/intellij-sdk-docs` `plugin_content.md` (a plugin with dependencies is
  `plugins\<plugin>\lib\*.jar`) and `plugin_configuration_file.md` (`<id>` defaults to `<name>`;
  `<version>` required). The scanner tries the jars whose name starts with the plugin folder's name
  first, then the rest by name, and identifies a descriptor without `<id>` by its `<name>`, as the IDE
  does. Not scanned: a plugin shipped as a single jar directly in `plugins` (the documented "plugin
  without dependencies" layout, which the brief's table leaves out), `.zip` archives in `lib`, and
  plugins bundled in the IDE's install folder.
- **Deviation: no `ide_windows.go`.** The per-profile locations are joins on the profile folder, so
  they live in `ide.go`, as task 17's npm and pipx locations live in `cli.go`, and the fixture trees
  exercise them on Linux. `%APPDATA%` is the profile's default `AppData\Roaming` (task 17's rule;
  folder redirection is not followed). The only machine read is the profile list, which the scanner
  takes from task 17's `SystemCLIHost()` through a new one-method `ProfileLister`, so the profiles are
  the CLI scanner's: people's SIDs, folders under `C:\Users`, with a loaded hive or an `NTUSER.DAT`.
- **One record per app, IDE, version and profile.** An `extensions.json` that exists is the IDE's own
  list and is used alone: an older version's folder awaiting cleanup is not reported. The folder
  names are read only when the list is absent; a list that cannot be read or decoded is an error,
  with no fallback.
- **Reads.** Each `extensions.json` over 4 MiB, each `plugin.xml` entry over 1 MiB (by its header and
  by what is read), a file that is not a regular file, a jar that is not a zip and XML whose root is
  not `<idea-plugin>` are errors, one each, and the rest of the scan goes on. Each list, fallback
  folder and descriptor read counts `observed`.
- **Fixture values.** The JetBrains plugin ids in the tests (`com.github.copilot`,
  `com.github.continuedev.continueintellijextension`) are not catalog facts: the catalog has no
  JetBrains plugin id, so on a device JetBrains plugins match nothing until one is added.
- **Not run here:** nothing Windows-only was added; the package compiles and vets under
  `GOOS=windows` and `GOOS=darwin`.

## 2026-10-08, task 38

- **What the vendor facts rest on (read 2026-10-08).** cursor.com and docs.cursor.com are blocked
  from the build machine, and the `cursor/docs` repository is not readable from here (its raw files
  answer 404 and git asks for credentials), so no first-party Cursor page was read. The facts below
  come from npm packages that implement Cursor hooks, which agree with each other except where
  noted: `cursor-hooks` 1.1.6 (2025-10-09, typings that mirror `cursor.com/docs/agent/hooks` of the
  Cursor 1.7 era), `@pmatrix/cursor-monitor` 0.6.1 (2026-05-21), `@blekline/cursor-hooks` 0.1.0
  (2026-07-03), `@cdot65/prisma-airs-cursor-hooks` 0.3.0 (2026-07-08), `@vaibot/cursor-circuitbreaker-plugin`
  0.2.2 (2026-09-28), `@unshadow/cursor-hook` 0.2.0 and `contexara` 1.2.13 (2026-10-07). No source
  names the Cursor version it describes; the catalog's newest is 3.24.9 (Homebrew cask, task 14).
  **Every fact here is for the device phase to confirm against the installed version.**
- **Enterprise hooks file on Windows: `C:\ProgramData\Cursor\hooks.json`** (`FOLDERID_ProgramData`).
  Not verified from a reachable source: it is the Windows location Cursor's hooks page gave before
  this build, beside `/Library/Application Support/Cursor/hooks.json` (macOS) and
  `/etc/cursor/hooks.json` (Linux), which the AIRS installer's notes repeat. Cursor runs the hooks of
  every file present (enterprise, project `.cursor/hooks.json`, user `~/.cursor/hooks.json`), so a
  user-level file adds hooks and cannot take the enterprise ones away.
- **`hooks.json`**: `{"version": 1, "hooks": {"<event>": [{"command": "..."}]}}`. An entry may also
  carry `timeout`, `failClosed` and `matcher`; the agent writes `command` only. AIRS says Cursor
  reads the file when it starts; whether a running Cursor picks up a change is for the device phase
  (the agent itself applies a bundle change without a restart).
- **stdin**: every event carries `conversation_id`, `generation_id`, `model`, `hook_event_name`,
  `cursor_version`, `workspace_roots`, `user_email` and `transcript_path`; `beforeSubmitPrompt` adds
  `prompt` and `attachments`; `beforeMCPExecution` adds `tool_name`, `tool_input` and the server's
  `command` or `url`. The packages disagree on `tool_input`: the newer ones read a string holding the
  JSON, older typings an object. The adapter takes both and sends compact JSON (a string that is not
  JSON as written). `conversation_id` is the session; input without one is refused, so the hook
  fails open.
- **Output**, exit code 0: `beforeSubmitPrompt` answers `{"continue": bool, "user_message": ...}`;
  `beforeMCPExecution` answers `{"permission": "allow"|"deny"|"ask", "user_message": ...,
  "agent_message": ...}`. The older packages spell the latter two `userMessage` and `agentMessage`;
  the newer ones, and the agent, use `user_message` and `agent_message`. A `warn` renders as the
  event's allow with `user_message`; whether Cursor shows it is task 41's.
- **Block behaviour**: every source documents both events as blocking (`continue: false` stops the
  prompt, `deny` stops the MCP call), so the adapter's `CanEnforce(event)` is true for both. The
  device phase tests each event by hand first and corrects `CanEnforce` and this entry if the
  installed version differs.
- **The relay reads `CanEnforce`.** `CanEnforce(event string) bool` is part of the `Adapter`
  interface (the test adapter and Claude Code answer true), and the relay records
  `RecordedAction(d, adapter.CanEnforce(event))`; a tool with no adapter records `logged`.
- **The agent's entry is the one whose command is** `"<exe>" --hook cursor <event>`, with `<exe>` a
  `capture-core(.exe)` from wherever the agent is or was installed, so one written from an earlier
  install path is replaced in place, as Claude Code's are. The running service's `os.Executable()`
  is written; the entry is added after the customer's entries for each event that lacks it.
  `"version": 1` is added to a file that has none and taken out again on Remove only when the rest
  of the file is as the backup had it. A file that is not a JSON object, or whose `hooks` is not an
  object of arrays, is left untouched and reported `config_write_failed`.
  Backup and restore are task 27's (`toolconfig/cursor/original`).
- **Installed** means the installed-app scanner (`inventory.Scanners`, task 16) finds app `cursor`
  under the bundle in force's catalog, for the machine or a loaded user hive (today by publisher
  `Anysphere`). The writer takes it as a seam, wired in `buildProviders`.
- **Access control**: a new `C:\ProgramData\Cursor` folder inherits ProgramData's access, which lets
  users create files in it; the hooks file itself gets the managed DACL, as Claude Code's does.
- **Fixtures** are written from the sources above under `hooks/testdata/cursor/documented/`, marked
  so in their README; the device phase replaces them with a capture named after the installed
  version.
- **Migration `0013-tool-config-cursor.sql`**, built as `0012` and renumbered at merge after task
  21's flow monitor. Migration proof, on the task's branch: the integration branch's `schema.sql`
  plus it, the new `schema.sql` plus `0002` to it, and `main`'s `schema.sql` plus `0002` to it each
  dump (`pg_dump --schema-only`) identically to the new `schema.sql`, with identical
  `ref.collector` rows.
- **Not run here:** `toolconfig/cursor_windows_test.go` (the ProgramData path). It compiles and vets
  under `GOOS=windows`.
- **Merged onto task 37's model.** Cursor's provider uses task 37's `Desired`: it is on while its
  hooks switch is in effect, and asks for `Hooks` and `HookCommand` (from `Config.Executable`) only.
  A tool's description says whether it has an OTel export (`otel`) and a managed-only setting
  (`managedOnly`); Cursor has neither, so its OTel switch does nothing and `managed_only` leaves its
  `Desired` unchanged. The writer takes the command from `Desired`, and `Apply` without `Hooks` takes
  the agent's entries out.

## 2026-10-08, task 20

- **Vendor facts: Ollama** (`ollama/ollama`, read 2026-10-08 at `main` c2b7368 and release tag
  `v0.40.2`; ollama.com is blocked here): `docs/faq.mdx` and `docs/windows.mdx` (Windows store
  `C:\Users\%username%\.ollama\models`, moved by `OLLAMA_MODELS` set "for your account");
  `envconfig/config.go` (`OLLAMA_MODELS` trimmed of spaces and quotes, else `~/.ollama/models`);
  `types/model/name.go` (name parts; `library` shown without its prefix); `manifest/paths.go` and
  `manifest/manifest.go`.
- **Deviation: Ollama's manifest trees.** The brief names only `manifests\registry.ollama.ai`.
  Current releases write each pulled model to `manifests-v2\ollama.com\<namespace>\<model>\<tag>`
  (on Windows as a copied file, not a link) and remove the legacy entry; they still list both
  trees, v2 first, and treat `registry.ollama.ai` and `ollama.com` as one registry. The scanner reads
  both trees and both host names, as Ollama does, and names a model as `ollama list` does
  (`model:tag`, or `namespace/model:tag` outside `library`). A name part Ollama would refuse
  (including its temporary `.manifest-*` files) and a folder with no tag file name nothing. Models
  from other registries (`hf.co`, a private host) are not listed: the brief limits the scan to the
  public registry.
- **Vendor facts: LM Studio** (`lmstudio-ai/docs` 9b8bc20, read 2026-10-08; lmstudio.ai is
  blocked): `0_app/5_advanced/import-model.md` gives the store as `~/.lmstudio/models/` with
  `<publisher>/<model>/<file>.gguf`, and the other Windows paths in those docs are under
  `%USERPROFILE%\.lmstudio`. `~/.cache/lm-studio/models` appears nowhere in them and is not read.
  **Needs device confirmation:** the default store on Windows (`%USERPROFILE%\.lmstudio\models`) is
  inferred from those docs, not seen on a real install; a store moved in the app's My Models tab is
  not followed. MLX weights are `model*.safetensors`, the files `ml-explore/mlx-lm`
  (`mlx_lm/utils.py`, `main`, read 2026-10-08) loads and saves.
- **LM Studio is found only by its store.** The catalog has no `windows_exe` for it, so no process
  of it is matched and its listener on 1234 is never attributed; its basis is always `model_store`.
- **Presence and attribution.** Processes come from a process snapshot; only one whose base name is
  a `local_runtime` app's `windows_exe` is described (`hostinfo.ProcessInfo`). One that cannot be
  described (it ended) or runs as no person's SID (task 16's rule; the system account) is skipped
  without an error. Records are per profile (task 17's list), so a runtime process of a user without
  such a profile is not reported. `hostinfo.ListenersOn` is asked only for a runtime with a running
  process, since a listener counts only when such a process owns it. With a process but no listener
  the basis is `model_store`, as the brief says, even when no store exists.
- **`OLLAMA_MODELS`** is read from the user's own `Environment` key, only when their hive is loaded,
  and expanded with the profile's folders, then the machine's variables; when set it replaces the
  catalog's store, as in Ollama. A machine-wide `OLLAMA_MODELS` is not read.
- **Version** is the PE file version of the listening process's executable, else of the other runtime
  processes' executables in path order; empty when the runtime is not running. Because the §6 key
  includes the version, a runtime first seen stopped and later running on the same day is emitted
  twice that day; a newly pulled model is not re-emitted until the next day (the key has no model
  list).
- **Model names** are sorted and de-duplicated; a name over 200 characters is left out. At most 64
  are kept, and a record that left any out counts one `dropped` (through `CountedScanner`).
- **`policy.Bundle` gains `AppsInCategory` and `SignalValues`**, outside the brief's files: the
  scanner needs the local runtimes and each one's ports and stores, and collectors read the catalog
  only through lookups.
- **`hostinfo.ListenersOn`** reads `GetExtendedTcpTable`'s `TCP_TABLE_OWNER_PID_LISTENER` class for
  IPv4 and IPv6 (`tcpTable` now takes the class) and returns each listening PID once, sorted.
  Elsewhere it returns `ErrUnsupported`.
- **Not run here:** `inventory/models_windows_test.go` and `TestListenersOnFindsThisProcess` in
  `hostinfo`. They compile and vet under `GOOS=windows`.

## 2026-10-08, task 32

Build phase: research only, no code and no fixtures. Every answer below is **from the documentation**,
read 2026-10-08 (https://claude.com/docs/cowork/monitoring.md; the Claude Desktop configuration, telemetry
and MDM pages under https://claude.com/docs/third-party/claude-desktop/, `.md` form; the support articles
"Monitor Cowork activity with OpenTelemetry" (14477985), "Enterprise configuration for Claude Desktop"
(12622667, dated 2026-09-02) and "Get started with Cowork" (13345190); https://code.claude.com/docs/en/managed-settings
and `model-config`). The newest Claude Desktop on the changelog is v2.26454.2 (2026-10-07, bundled Claude
Code 2.1.293). `docs.anthropic.com` was not tried; the Harmonic page (www.harmonic.security) is blocked from
the build machine, so only `PLAN.md`'s summary of it is used: OTel is the only way to see Cowork prompts,
and Anthropic's Compliance API, audit logs and exports do not cover Cowork. It says nothing the plan records
about what Harmonic configures; the vendor's own pages answer the questions below. The device phase confirms
or corrects each answer marked **[device]**.

- **Platforms.** Cowork runs in Claude Desktop on macOS and Windows (all paid plans; the support article
  names no minimum OS) and, as remote sessions, on web and mobile. OTel monitoring is **Team and Enterprise
  only**. Local sessions need Desktop 1.1.4173 or later; cloud sessions 1.22209.3 or later; the
  `assistant_response` event and the `otlpContentCapture` defaults below need 1.17377 or later. Local Cowork
  runs in a workspace VM on macOS and Windows. **[device]** the Desktop version on the reference VM, and that
  its account is Team or Enterprise (the setting may not exist on other plans).
- **Where the OTel settings live: two places, split by what they control.**
  - The collector (`OTLP endpoint`, `OTLP protocol` `http/json` or `http/protobuf`, `OTLP headers`) is set
    by an administrator in **Admin settings > Cowork** on claude.ai (the support article says
    "Organization settings > Cowork"). It is server-side, per organisation; the device agent cannot write it,
    and "events are only exported when an admin configures the OTLP endpoint". It applies to a new session,
    not a running one, and to every Cowork session on a user's computer in Claude Desktop, Dispatch tasks
    included, but **not** to Code-tab sessions (those arrive as `service.name` `claude-code-desktop`).
  - What events carry is a **device** key, `otlpContentCapture` (below).
- **An admin-managed device location exists.** Claude Desktop reads the same keys from MDM: on Windows
  `HKLM\SOFTWARE\Policies\Claude` (machine) or `HKCU\SOFTWARE\Policies\Claude`; on macOS the
  `com.anthropic.claudefordesktop` domain; on Linux `/etc/claude-desktop/managed-settings.json`. Values are
  `REG_SZ` directly under the key (`REG_DWORD` for booleans and integers; arrays and objects are JSON text;
  `REG_EXPAND_SZ`, `REG_MULTI_SZ`, `REG_QWORD` and `REG_BINARY` are unreadable). The keys are `otlpEndpoint`,
  `otlpProtocol` (`http/protobuf` default, `http/json`, `grpc`; Cowork falls back from `grpc` to
  `http/protobuf` on Windows), `otlpHeaders` (JSON object), `otlpHeadersHelper` (path of an executable that
  prints the headers as JSON), `otlpAuthMode`, `otlpResourceAttributes`, `otlpContentCapture` and
  `otlpTracesEnabled`. The reference lists them as "MDM + Bootstrap"; a managed source wins over locally
  written values, the app reads them at launch and, from 1.46388.1, re-checks every 10 minutes and asks the
  user to restart (required after 24 hours). Users cannot override an HKLM value. Two cautions for any
  writer:
  - when machine policy exists under `HKLM\SOFTWARE\Policies\Claude`, **the app ignores
    `HKCU\SOFTWARE\Policies\Claude` entirely**, so an agent that creates the HKLM key would silently switch
    off a customer's HKCU-only policy;
  - the key reference is written for third-party (3P) deployments. It says 3P reads "the same
    managed-configuration sources as standard Claude Desktop", and the Cowork page applies
    `otlpContentCapture` to first-party deployments, but no page says that a first-party install honours
    `otlpEndpoint` from HKLM, or which wins when the admin console and the registry both set it. **[device]**
- **Also documented:** Claude Code inside a Cowork session on the user's machine reads the device's MDM
  policy and `C:\Program Files\ClaudeCode\managed-settings.json` by default (task 27's file); with
  `requireCoworkFullVmSandbox` (deprecated) set it does not, and remote sessions never do. Server-managed
  settings are never delivered to Cowork. When `otlpEndpoint` is set, only `otlpTracesEnabled` decides trace
  export, whatever managed settings say. Whether task 27's `env` keys change a Cowork session's export when
  no `otlpEndpoint` is set is not documented. **[device]**
- **`service.name` and events.** Resource attributes: `service.name` = `cowork`, `service.version` (the
  app version), `host.arch`, `os.type`, `os.version`, `process.owner` (OS login name), and on 3P
  deployments `enduser.id`. The session types arrive as `cowork`, `claude-code-desktop` and `claude-desktop`
  (the app's own events, always `http/json`). Event names carry **no `claude_code.` prefix**:
  `user_prompt`, `assistant_response`, `tool_result`, `tool_decision`, `api_request`, `api_error`. Attributes:
  `session.id`, `prompt.id`, `organization.id`, `user.account_uuid`, `user.account_id`, `user.id`,
  `user.email`, `workspace.host_paths`, `terminal.type` (`non-interactive`), `event.timestamp`,
  `event.sequence`, and per event `prompt_length`, `prompt`; `model`, `request_id`, `response_length`,
  `response`; `tool_name`, `success`, `duration_ms`, `error`, `decision_type`, `decision_source`,
  `tool_result_size_bytes`, `mcp_server_scope`, `tool_parameters`, `tool_input`; `cost_usd`, `input_tokens`,
  `output_tokens`, `cache_read_tokens`, `cache_creation_tokens`, `speed`; `status_code`, `attempt`;
  `decision`, `source`. Logs and metrics are exported; traces only with `otlpTracesEnabled` (beta,
  1.22209.0 or later). Metrics export once a minute. **[device]** the names, where the event name sits
  (attribute or body, as task 25 found for Claude Code), the value types and any undocumented extras.
- **Prompt text and its switch.** `prompt` carries the text under `otlpContentCapture` (categories
  `userPrompts`, `assistantResponses`, `toolDetails`, `toolContent`, `rawApiBodies`; a JSON array string; `[]`
  means metadata only, and an empty string reads as unset). On Desktop 1.17377 or later `userPrompts`
  always adds model response text. **When the key is unset on a first-party deployment, events carry user
  prompts, responses and `toolDetails`** (on 3P, nothing); the support article agrees ("prompt content is
  included by default"). `workspace.host_paths` and, on first-party, `user.email` are always sent. This is
  the reverse of the product's default (nothing at `m0`; prompt text only on a grant), so a writer must set
  the key itself: `["userPrompts"]` at `m1` and above for the tool (responses then arrive too and are not
  used), `[]` otherwise. **[device]** the value of `prompt` when redacted (`<REDACTED>` is documented for
  `response` only) and that `[]` removes prompt text.
- **The exporter's location is the decisive open question.** The Cowork page says "the OTel exporter runs
  inside the Cowork VM, so it is subject to the session's egress rules" and adds the collector host to the
  egress allowlist itself; the 3P telemetry page says "each device opens its own connection to the collector"
  and that the collector must present a certificate the OS trusts. If the exporter is in the workspace VM,
  the agent's receiver on `127.0.0.1:47318` is not reachable: loopback in the VM is the VM, the receiver
  binds loopback only (`DESIGN.md` §5), and the sender-process attribution (task 24) would see a virtual-NIC
  peer, not a user process. Cowork would then not be collectable by the local receiver at all. **[device]**
  first: point the exporter at a listener on the host and read the source address and whether the
  connection arrives.
- **Whether the Claude Code normalizer (task 26) already accepts the events: no, as written.**
  - It `Accepts` only `service.name` `claude-code`; Cowork's is `cowork`, so the events fall through to the
    generic GenAI normalizer (task 33) or are not routed. Its `tool_fingerprint` is `app:claude_code`, which
    is wrong for Cowork.
  - The event name is read from `event.name`, then the OTLP `eventName`, then a `claude_code.` body, so the
    unprefixed names would match if the name sits in one of the first two. The converted events use
    attribute keys that Cowork documents under the same names (`prompt`, `prompt_length`, `tool_name`,
    `success`, `duration_ms`, `decision_type`, `decision`, `model`, `input_tokens`, `output_tokens`,
    `event.timestamp`, `event.sequence`, `session.id`). `assistant_response` is not a converted event and
    would need a dropped reason; the resource attributes `process.owner` and `enduser.id` are in no table.
    Read from `attributes.go` and the tests, not run.
- **Outcome (provisional, set in the device phase).** Most likely **Needs follow-up**; none is started, in
  this order:
  1. A Cowork normalizer for `service.name` `cowork` with fingerprint `app:claude_cowork` (the existing
     `claude_desktop` app key is the product; Cowork is a feature of it), sharing task 26's event handling,
     with its own mapped and dropped tables and fixtures under `testdata/cowork/<version>/`.
  2. A `cowork` tool key in `DESIGN.md` §5 and the policy bundle (`endpoint.tools.cowork.otel`, default on),
     with `toolconfig.ToolForApp` mapping `claude_desktop` to it; the native-first exclusion (§10) must not
     blind-tunnel Desktop's chat and Code traffic, which this OTel does not cover.
  3. A Windows writer, `tool_config_cowork`, that merges into `HKLM\SOFTWARE\Policies\Claude` as `REG_SZ`
     (`otlpEndpoint`, `otlpProtocol` `http/protobuf`, `otlpHeaders` or `otlpHeadersHelper` for the token,
     `otlpContentCapture` by mode), backs up and restores like task 27, and copies a customer's HKCU values
     into HKLM first. It is worth building only if the checks above pass. A collector endpoint that the
     customer's admin set in the admin console cannot be overridden from the device.

  If the exporter runs in the VM and cannot reach the host, or a first-party install ignores the registry
  keys, the outcome is **Not collectable**. What would change it: an endpoint reachable from the VM (a
  host-side listener on an address the VM can reach, which `DESIGN.md` §5 does not allow), or the customer's
  own collector forwarding to the product.
## 2026-10-08, task 28

- **Source (vendor fact).** developers.openai.com is blocked from the build machine (proxy 403),
  so the `documented/` fixtures follow Codex's OpenTelemetry exporter source in `openai/codex`,
  read 2026-10-08: tag `rust-v0.162.0` (`1f3f934`, npm `latest` of `@openai/codex` that day) and
  `main` at `99aa053` (2026-10-08T22:42Z), whose event code is the same. The folder's README names
  the files read.
- **Config keys (vendor fact, for the capture and task 29).** `[otel]` takes `exporter` (logs),
  `trace_exporter`, `metrics_exporter` (default `statsig`; set `"none"` to keep metrics local),
  `log_user_prompt` (default false), `log_agent_responses`, `log_guardian_assessments`,
  `environment` (default `dev`), `span_attributes`, `tracestate` and `tool_result`. An exporter is
  `{ otlp-http = { endpoint, protocol = "binary" | "json", headers, tls } }` or
  `{ otlp-grpc = { endpoint, headers, tls } }`; the HTTP endpoint is used as given, so it names
  the full `/v1/logs` path.
- **Service names (vendor fact).** `service.name` is the process's originator: `codex_cli_rs`
  (interactive CLI), `codex_exec` (`codex exec`), `codex-app-server` (the app server Codex Desktop
  and the IDE extensions run; a `Codex Desktop` client name does not change it).
  `CODEX_INTERNAL_ORIGINATOR_OVERRIDE` can change the first two; it is not accepted. The
  normalizer accepts exactly these three.
- **Record shape (vendor fact, from the bridge's source).** The event name is the `event.name`
  attribute (`codex.<name>`); the scope is the tracing target `codex_otel.log_only`; records have
  an observed time and no time; Display-formatted fields (`duration_ms`, `prompt_length`, token
  counts, `success` on `tool_result`) are strings. The redacted prompt is `[REDACTED]`, and
  `prompt_length` counts characters, which the prompt's `size_bytes` carries as is.
- **Events converted.** `codex.user_prompt` (prompt), `codex.tool_decision` when the call does not
  run (`denied`, `denied_with_network_policy_deny`, `abort`, `timed_out`: `tool_call` denied),
  `codex.tool_result` (`tool_call` success or error), and model requests: a failed
  `codex.api_request` or `codex.websocket_request` (`model_request` error, model, duration), and a
  `codex.sse_event` of kind `response.completed` (success with model and tokens, or error when it
  carries `error.message`). A successful request is recorded by its completed response, which
  alone carries the tokens, over HTTP and websocket alike, so one model call is one record. The
  12 other events in the fixtures are listed as dropped with a reason.
- **A denied call is one record.** Codex emits a denied call's `tool_decision` and then a failed
  `tool_result` with the same `call_id`. The decision becomes the denied record and the normalizer
  remembers the call (conversation and call id, at most 1024, oldest forgotten first) to skip its
  result, across exports.
- **Dedup event id.** Codex sends no event sequence: the activity key's event id is the event
  name, `conversation.id`, `event.timestamp` (milliseconds), `call_id` and `attempt`.
- **Spans are not converted** (as for Claude Code); taking Codex's `service.name` keeps its spans,
  which carry `gen_ai.usage.*`, from reaching the generic normalizer.
- **Left out of the fixtures:** metrics, traces, and the `codex_otel`-target log events of other
  crates (network proxy audit, file upload, models endpoint, exec server, agent communication,
  HTTP client fallbacks, app server shutdown). The capture shows which of them a session sends.
## 2026-10-08, task 47

- **Migration `0014-kill-switch.sql`** (renumbered at merge; numbers follow merge order).
  Migration proof, on throwaway databases: the integration branch's `schema.sql` plus it,
  `main`'s `schema.sql` plus `0002` to it, and the new `schema.sql` plus it alone and plus `0002` to
  it each dump (`pg_dump --schema-only`) identically to the new `schema.sql`; the `ref` rows are
  identical apart from `ref.data_class`'s load timestamp.
- **A tripped `proxy.tls` kill switch is a blind tunnel, not a stop** (a deviation from the earlier
  behaviour, which closed the listener and reported `absent`/`killed`). The proxy keeps listening,
  tunnels every new connection blind (counted `blind_tunnelled`) and reports `degraded`/`killed`. Its
  root leaves the trust store while the switch is in force, because a proxy that decrypts nothing
  needs no trusted authority; clearing the switch installs the root again and re-runs the end-to-end
  probe. A Start under a switch binds, installs nothing and skips the probe until it clears.
- **The desktop-app PAC stays on under a `proxy.tls` kill switch** (it used to switch off), so
  desktop apps keep reaching the proxy and are carried blind, which the device check counts. The CLI
  shim is unchanged: it still removes its files under a `proxy.tls` or `cli.shim` switch, so CLI
  tools go direct.
- **The switch is read per connection.** A request still to be decided on a connection that was
  already decrypted when the switch tripped is carried with enforcement off (`canEnforce`); a
  keep-alive tunnel stays a tunnel until it closes, so a cleared switch applies to new connections.
- **The loopback broker's own switch** (`proxy.loopback`): it keeps its ports and pipes each new
  connection to the upstream as bytes, unread and unrecorded, counted `blind_tunnelled`, and reports
  `degraded`/`killed`.
- **"Looks like pinning"** is, after the client's hello, a certificate alert from the client
  (`bad_certificate`, `unsupported_certificate`, `certificate_revoked`, `certificate_expired`,
  `certificate_unknown`, `unknown_ca`) or a hang-up (EOF or reset). A timeout, any other alert or a
  client that is not speaking TLS counts `errors` and excludes nothing. The key is the lower-case
  process image (as task 09's attribution names it: the image base name) and host; clients that
  cannot be attributed share the key `unknown`. An exclusion lasts 24 hours, then the client is
  probed again; an excluded connection counts `not_cooperative` and `blind_tunnelled`, as before.
- **Leaf expiry (task 44's note) is fixed here**, because it was in this path: an expired cached
  leaf would be refused with `certificate_expired` and taken for pinning. A cached leaf is presented
  only while the clock is inside its validity and more than an hour of it remains; otherwise a new
  one is minted.
- **Unparseable pass-through needed no change.** A test through the real pipeline shows a body of
  an unknown shape, one no parser reads and one over the cap reach the server byte for byte with
  their headers, and their events are `confidence: degraded`.
- **The reason is a code**, `^[a-z][a-z0-9_.-]{0,63}$` (`ops.kill_switch.reason_code`), required to
  trip and optional (audited) to clear. Tripping a tripped switch changes its reason and keeps its
  `effective_at`. `PUT /admin/v1/settings/kill-switch/{route}` takes `{on, reason_code}`; `GET
  /admin/v1/settings` lists `kill_switches` (`route`, `reason_code`, `effective_at`, `set_by`). The
  audit action is `tenant.kill_switch.set` on object `kill_switch`/route, with the route and the
  previous and new state.
- **Bundle**: `kill_switches` items are `{provider, mode: "disable", effective_at, reason_code}` in
  route order, `effective_at` in whole UTC seconds, omitted when none is tripped. The device already
  decoded the field; the drift test now carries both routes' switches.
- **QUIC**: nothing built; it waits for task 43's note in the device phase.
## 2026-10-08, task 30

- **Sources, read 2026-10-08.** code.visualstudio.com was not used; the VS Code page
  (`monitoring-agents`) was read in `microsoft/vscode-docs` at `a8848427` (`DateApproved`
  10/7/2026). Shapes the page leaves open (value types, where content sits, the log events'
  attributes) follow the extension's source, `extensions/copilot` in `microsoft/vscode` at
  `f6f19d60` (version 0.70.0; `microsoft/vscode-copilot-chat` is archived). The CLI's section is
  "OpenTelemetry monitoring" in the CLI command reference, read in `github/docs` at `9f651797`;
  npm `latest` of `@github/copilot` was 1.0.94 (a loader for a native binary, so its variables
  could not be read from the package).
- **Vendor facts.** VS Code exports spans, metrics and log events under `service.name`
  `copilot-chat` (settings `github.copilot.chat.otel.*`; content `captureContent`, identity
  `captureIdentity`). The CLI exports spans and metrics only, under `github-copilot`, enabled by
  `COPILOT_OTEL_ENABLED` or `OTEL_EXPORTER_OTLP_ENDPOINT`, default protocol `http/json`. **Its
  content switch is `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT`, not a `COPILOT_OTEL_*`
  variable** (`COPILOT_OTEL_CAPTURE_CONTENT` is VS Code's). Background Copilot agent sessions
  inside VS Code send the Copilot runtime's spans as `github-copilot` too.
- **The prompt is the root `invoke_agent` span, not the chat span** (a deviation from the brief).
  Both docs say the root agent span wraps the work for one message the person sent; a VS Code chat
  span's last user message is the request with its context added, and VS Code's helper requests
  (title, summaries) are root chat spans ending with a user message. So: a root agent span (no
  parent span) gives one prompt, its text the latest user message of `gen_ai.input.messages` by
  task 33's rule (`genai.LatestUserMessage`, newly exported, with `genai.ExtractText`), else
  `copilot_chat.user_request` (VS Code's inline chat and CLI wrapper spans carry only that). A
  subagent's agent span has a parent and its input is the model's, so it gives nothing.
- **With content capture off the prompt is still recorded**, with no text and `size_bytes` 0: at
  `m1` and above it is degraded, as Claude Code's redacted prompt is; at `m0` it is the prompt event
  §8 keeps.
- **Spans.** `chat` gives `model_request` (request model, else response model; the token counts),
  `execute_tool` gives `tool_call`. Outcome `error` from status `ERROR` or `error.type`.
  `execute_hook`, agent-span totals and every span event give nothing. The dedup key is task 33's
  (trace and span ids). Counted on the `otel_receiver` row as task 33 does.
- **Log events are not converted**: each repeats a span (inference details per chat span, tool
  call per execute_tool span, agent turn) or is an editor action with no envelope kind. They count
  `skipped_not_generative`.
- **Fingerprint by service name**: `copilot-chat` is `app:github_copilot`, `github-copilot`
  `app:copilot_cli`. `gen_ai.conversation.id` is not the prompt's client id, so the hook/OTel merge
  buffer does not hold Copilot prompts.
- **Fixtures.** `testdata/copilot/vscode-documented/` (traces and logs, content on and off) and
  `cli-documented/` (traces, on and off); no metrics files, since the receiver discards metrics.
  Each README lists sources, shapes, placeholders and what the device phase must confirm: spans
  and/or logs per tool, where the prompt sits, and the service name of VS Code's background agent.
  The mapped/dropped test walks resource, span, span-event and log attributes, span operations,
  span events and log events. Privacy fixtures: one agent-mode turn per tool, whose dropped values
  include output messages, system prompt, tool arguments, a subagent's input and `host.name`.

## 2026-10-08, task 39

Build phase: every answer is **from the documentation**, read 2026-10-08, and is for the device phase to
confirm on the installed version. The vendor sites (developers.openai.com, docs.github.com,
code.visualstudio.com, geminicli.com) are blocked from the build machine, so the sources are the vendors'
GitHub repositories: `openai/codex` `main` at `99aa053` (2026-10-08; the newest release tag is
`rust-v0.162.0`), `github/copilot-cli` at `a7ae5b0` (its changelog's newest entry is 1.0.94 of 2026-10-08),
`github/docs` at `9f65179` (2026-10-08; the source of docs.github.com), `microsoft/vscode-docs` at `a884842`
(2026-10-08; the hooks pages are approved 10/7/2026) and `google-gemini/gemini-cli` at `2ce1a69`
(2026-10-08; package 0.65.0-nightly.20261006). The three questions are (1) a hook before a prompt is sent,
(2) it can block and the tool shows the reason, (3) it can be declared in a machine-wide, admin-managed
location a user cannot override or disable.

- **Codex CLI: adapter built.** The prose hooks page is developers.openai.com's; the repository holds only a
  `docs/config.md` note (`allow_managed_hooks_only` works only in `requirements.toml`), so the answers rest on
  the hook schemas the repository publishes (`codex-rs/hooks/schema/generated/`) and its source.
  1. Yes: `UserPromptSubmit`, with stdin `session_id`, `turn_id`, `transcript_path` (or `null`), `cwd`,
     `hook_event_name`, `model`, `permission_mode`, `prompt`, and `agent_id`/`agent_type` for a subagent.
  2. Yes: `{"decision": "block", "reason": ...}` with exit code 0 (or exit code 2 with the reason on stderr)
     stops the turn, and the TUI shows "Blocked by hook" with the reason. A block whose reason is empty is
     ignored, so the adapter sends the rule's id when the rule has no message. `systemMessage` shows as a
     warning; plain stdout is added to the model's context, so allow is empty output.
  3. Yes: `[hooks]` in `%ProgramData%\OpenAI\Codex\requirements.toml` (no user override of that path; the
     loader's override is for tests). Managed hooks are always enabled and trusted, whatever a user's
     per-hook state says, and `allow_managed_hooks_only = true` drops user, project and session hooks. A
     user's `[features] hooks = false` would switch every hook off, managed ones included, unless the
     requirements pin `[features] hooks = true`; the writer pins it. Hook events append across requirements
     layers (system, cloud, MDM), so a customer's cloud requirements do not displace the agent's hook.
- **Copilot CLI: not available, answer 2 failed.** (1) Yes, `userPromptSubmitted` (`UserPromptSubmit` in the
  VS Code compatible format) with `prompt`. (2) No: command and HTTP config-file `userPromptSubmitted` hooks
  "have their output dropped" (only SDK programmatic hooks' `modifiedPrompt` is honoured), and exit code 2 is
  a warning whose stderr is shown while the run continues. (3) Yes: policy hooks in
  `C:\ProgramData\GitHub\Copilot\policy.d\*.json` or `HKLM\Software\Policies\GitHub\Copilot`, which
  `disableAllHooks` does not affect and users cannot modify. `preToolUse` can deny a tool call, but it is not
  a prompt hook. **[device]** a `userPromptSubmitted` policy hook that prints `{"decision":"block",...}` and
  one that exits 2: does the prompt still reach the model, and what is shown.
- **Copilot in VS Code: not available, answers 2 and 3 unclear in the docs.** The hook implementation is the
  session target's harness. *Local* (extension host): (1) yes, `UserPromptSubmit` with `prompt`; (2) unclear:
  `continue: false` with `stopReason` ("shown to the user") stops the agent execution and exit code 2 gives
  stderr to the model, but no page says the prompt is withheld from the model; (3) no file location: Local
  reads only workspace (`.github/hooks`), user (`~/.copilot/hooks`), custom-agent and plugin hooks, and the
  admin route is the `ChatAllowManagedHooksOnly` policy plus a plugin force-enabled by `ChatEnabledPlugins`
  from a marketplace in `ChatExtraMarketplaces` (GitHub shorthand or a Git URI), not a file the agent writes.
  *Copilot* (Agent Host): Copilot CLI's implementation, policy hooks included, so answer 2 fails as above.
  *Claude* and *Codex*: those tools' own hooks. **[device]** at the console, Local target, a workspace hook on
  `UserPromptSubmit` answering `{"continue": false, "stopReason": "..."}`: is the prompt sent, and is the
  reason shown; whether `ChatExtraMarketplaces` accepts a local `file://` Git repository, so a policy-forced
  plugin could carry the agent's hook; which session target a new chat uses on the VM; and whether the Claude
  target reads `C:\Program Files\ClaudeCode\managed-settings.json`.
- **Gemini CLI: not available, answer 3 failed.** (1) Yes, `BeforeAgent` ("after user submits prompt, before
  planning") with `prompt`. (2) Yes: `{"decision": "deny", "reason": ...}` (or exit code 2) blocks the turn and
  discards the prompt, and the UI shows "Agent execution blocked: <systemMessage or reason>". (3) No:
  `C:\ProgramData\gemini-cli\settings.json` is the system override file, but a user can point
  `GEMINI_CLI_SYSTEM_SETTINGS_PATH` elsewhere (the enterprise page says so and suggests a wrapper script), and
  `hooksConfig.disabled` is merged by union from every settings file and applies to system hooks, so a
  user's `~/.gemini/settings.json` (or `/hooks disable`) turns the agent's hook off by its command. The
  server-side Admin Controls have no hooks control. No `gemini_cli` tool key, collector or migration is
  added. **[device]** confirm both bypasses on the installed version.
- **`tool_config_codex` is created here.** Task 29 has not been built, so the Codex writer gets the
  collector `DESIGN.md` §2 names, its `ref.collector` row and its wiring in `buildProviders`. The tool is
  described with `otel: false` and `managedOnly: true`: task 29 adds the OTel export to the same provider
  (its own file, `config.toml`) and flips `otel`.
- **Only `UserPromptSubmit` is declared.** Codex also has `PreToolUse`; the brief asks for the pre-prompt hook.
- **The hook command is `cmd /c "<exe>" --hook codex UserPromptSubmit`.** Codex has no exec form: it runs a
  command hook through the user's shell, PowerShell (`-NoProfile -Command`) by default on Windows and cmd
  (`/c`) without one, and a quoted path followed by arguments parses in cmd but not in PowerShell. A nested
  cmd keeps the quotes in both, for an install path without `&<>()@^|` (the MSI's
  `C:\Program Files\ShadowAICapture` qualifies). The shell's start-up comes before the hook's 400 ms, so the
  timeout is 5 seconds; task 51 should measure the added latency. **[device]** under both shells.
- **What the writer owns** in `requirements.toml`: its `hooks.UserPromptSubmit` group (one whose handlers all
  run `capture-core(.exe)` that way, so an earlier install path is replaced in place), `features.hooks` and
  `allow_managed_hooks_only`, as task 37 owns its keys: a key the bundle does not ask for holds the backup's
  value. It never sets `hooks.windows_managed_dir`, since conflicting values across requirements layers fail
  closed. A managed hook Codex cannot load stops it starting sessions; the agent's entry is fixed and valid.
  A file that is not TOML, or whose `hooks`/`features` is not a table or whose `hooks.UserPromptSubmit` is not
  an array of tables, is left untouched and reported `config_write_failed`. The file is read and written with
  `github.com/BurntSushi/toml` v1.6.0 (`DESIGN.md` §11; it was already in the module graph at a pseudo-version
  through grpc-gateway), so a rewrite drops comments; Remove writes the backup's bytes back when the result
  equals it.
- **Access control:** the file gets the managed DACL; a new `C:\ProgramData\OpenAI\Codex` folder inherits
  ProgramData's, which Codex's own diagnostic probe reports as `StandardUserMutationAcl` (diagnostic only in
  the source). **[device]** whether Codex warns.
- **Installed** means the inventory's CLI scan finds `codex` in a user profile, as for Claude Code.
- **Server defaults and Settings:** `codex.hooks` defaults on and the Settings page offers it. `DESIGN.md` §5
  still says Codex hooks stay off "until task 39 finds them"; it is not edited.
- **Migration `0015-tool-config-codex.sql`**, built as `0014` and renumbered after rebasing onto the
  integration branch, which took `0014` for the kill switch. Proof: the integration branch's `schema.sql` plus
  it, and the new `schema.sql` plus it, each dump (`pg_dump --schema-only`) identically to the new
  `schema.sql`, with identical `ref.collector` rows.
- **Not run here:** `toolconfig/codex_windows_test.go` (the ProgramData path). It compiles and vets under
  `GOOS=windows`.
## 2026-10-08, task 57

- **Vendor facts: Ollama** (`ollama/ollama` at tag `v0.40.2`, commit b061384, read 2026-10-08;
  ollama.com is blocked here): `docs/faq.mdx` ("On Windows, Ollama inherits your user and system
  environment variables": quit the tray app, set the variable, start the app again),
  `docs/windows.mdx` ("If Ollama is already running, Quit the tray application and relaunch it"),
  `envconfig/config.go` (`Host()`: `OLLAMA_HOST` is `[scheme://]host[:port]`, default port 11434) and
  `app/server/server.go` (the Windows app starts `ollama serve` with a copy of its own process
  environment each time it starts the server).
  - **Machine or user:** the app reads the environment it was started with, which Windows builds from
    the machine's variables and then the user's. A user's own `OLLAMA_HOST` wins over the machine's,
    and the relocation then does not reach that user's Ollama (not handled; the broker stays
    released and reports `upstream_unreachable`).
  - **The tray app must restart.** A running Ollama keeps 11434 until it is quit and started again.
    Until then the broker's preflight to 21434 fails and it never takes 11434.
  - The app's "Expose Ollama to the network" setting replaces `OLLAMA_HOST` with `0.0.0.0` (port
    11434) whatever the environment says. With it on, Ollama keeps 11434 and the broker reports the
    port held by another process.
  - Ollama's own clients (`ollama run`, the app's chat window) also read `OLLAMA_HOST`, so after the
    move they reach 21434 directly and are not captured. Only clients that use the default address
    (other apps, `curl`, IDE extensions) pass through the broker.
- **Vendor facts: LM Studio** (`lmstudio-ai/docs` 4f8082f, read 2026-10-08; lmstudio.ai is blocked):
  the server port is set in the app's per-user Server Settings or by `lms server start --port` ("uses
  the last used port"); only the bind address has an environment variable (`LMS_SERVER_HOST`). No
  machine-wide setting moves the port, so **LM Studio is left out** of the setting.
- **The machine environment is written through the registry**
  (`HKLM\SYSTEM\CurrentControlSet\Control\Session Manager\Environment`), not the CLI shim's `setx`,
  because the original value has to be read back to be restored and `setx` only writes. A value
  containing `%` is written `REG_EXPAND_SZ`, as Windows' own editor does. Each write broadcasts
  `WM_SETTINGCHANGE` "Environment", as `setx` does.
- **Needs device confirmation:** the broadcast reaches only the service's own session, so the signed-in
  user's Explorer may keep its old environment, and an Ollama started again from the Start menu may
  still get the old `OLLAMA_HOST` until the user signs in again. If so, task 60 needs a sign-out and
  sign-in (or a reboot) between switching capture on and step 3, and broadcasting in each session
  (the user-session helper) is a follow-up.
- **Backup.** `toolconfig/ollama/original` uses Claude Code's format (`{"present", "content"}`), is
  taken once before the first write and deleted after a restore. The restore writes the recorded value
  back (or deletes the variable) whatever `OLLAMA_HOST` holds by then.
- **When the move happens.** A port runner moves its tool before its first preflight, and again when
  its entry's upstream port changes. It moves the tool back after its port is released: when the
  entry leaves the bundle or the broker stops (capture switched off). The shutdown sequence releases
  the port but does not stop the broker, so a service stop or restart leaves `OLLAMA_HOST` moved;
  restoring it at uninstall is task 50's. A failed move counts `errors` and reports
  `degraded`/`config_write_failed`; the runner still preflights.
- **The broker is a `core.Toggled` provider**, on while `loopback.ports` is non-empty. With no bundle
  it is off (`absent`/`disabled_by_policy`) where it used to start with no ports. A stopped broker
  starts again with new port runners (it used to stay stopped), and each runner keeps its own copy of
  the configuration. The bundle's timings take effect at the broker's next start; a running broker
  applies only the port diff. A timing the bundle leaves out keeps the broker's default. A runner
  whose entry left the bundle is dropped, so the entry coming back starts a new one.
- **Composition.** The `loopback` section is omitted while no runtime is on, so tenants that never
  switch capture on keep their bundle version. The held port is Ollama's catalog `listen_port` (the
  lowest valid one; no port, no entry). The mode is the tenant's override for `app:ollama`, else the
  tenant's collection mode, which is all the bundle carries to resolve from. Ollama is not an
  `endpoint.tools` key in the bundle.
- **Settings API.** `GET /admin/v1/settings` lists `endpoint.tools.ollama` as `{loopback}` and the
  other tools as `{otel, hooks}`. `PUT .../tools/ollama` requires `{loopback}` and nothing else; the
  other tools refuse `loopback` (unknown field, 400). An Ollama row stores `otel` and `hooks` false.
  The audit's `previous` and `new` are `{loopback}` for Ollama.
- **Dashboard.** The tools table gains a "Local model capture" column: a switch for Ollama,
  "unavailable" for the four native tools, whose OpenTelemetry and Hooks cells are unavailable for
  Ollama.
- **Migration proof** (`0016-ollama-loopback.sql`, renumbered at merge), on throwaway
  databases: the integration branch's `schema.sql` plus it, the new `schema.sql` plus it, the new
  `schema.sql` plus `0002`–`0016`, and `main`'s `schema.sql` plus `0002`–`0016` each dump
  (`pg_dump --schema-only`) identically to the new `schema.sql`.
- **Not run here:** nothing exercises `machineenv_windows.go` on Windows (a test would change the build
  machine's environment); it compiles and vets under `GOOS=windows`, and task 60's step 1 checks it.

## 2026-10-08, task 29

- **Built on task 39's provider.** `tool_config_codex`, its `ref.collector` row and migration came
  with task 39; this task adds no migration. The Codex tool is now described with `otel: true`, and
  its one `Writer` is `CodexFiles`: task 39's requirements writer for the hook fields of a `Desired`
  and a new `CodexConfig` writer for the OTel fields. A file whose part is switched off is restored
  as `Remove` restores it, so switching only the export off gives the customer's `config.toml` back
  byte for byte. Each file has its own backup: `toolconfig\codex\original` (requirements) and
  `toolconfig\codex\config\original` (config).
- **Source (vendor fact).** developers.openai.com is blocked from the build machine and the
  brief's coralogix page is unreachable, so the facts below come from Codex's config loader in
  `openai/codex`, read 2026-10-08: tag `rust-v0.162.0` (npm `latest` of `@openai/codex` that day)
  and `main` at `99aa053`, whose loader layering, merge and `[otel]` types are the same
  (`codex-rs/config/src/loader/mod.rs`, `layer_io.rs` and its README, `config_requirements.rs`,
  `types.rs`, `merge.rs`, `codex-rs/core/src/config/otel.rs`, `codex-rs/utils/home-dir`). **The
  device phase confirms the location and the precedence against the installed version.**
- **Windows machine-wide file (corrects the brief): `%ProgramData%\OpenAI\Codex\config.toml`**,
  Codex's "system" config layer. `managed_config.toml` is Unix only (`/etc/codex/managed_config.toml`,
  the top layer, being phased out); on Windows `CODEX_HOME\managed_config.toml` is ignored with a
  startup warning. `requirements.toml`, which users cannot override, has no `otel` key, so Codex's
  OTel export cannot be enforced on Windows.
- **The plan's "Windows machine-wide file can be overridden by users" is confirmed.** Codex's
  layers, lowest first: packaged defaults, the system `config.toml`, the cloud-managed bundle, the
  user's `${CODEX_HOME}\config.toml` (`CODEX_HOME` unset: `%USERPROFILE%\.codex`), a selected
  profile's `${CODEX_HOME}\<name>.config.toml`, project `.codex\config.toml` (which cannot set
  `otel`: it is on Codex's project denylist) and `-c` flags. Tables merge key by key and any other
  value replaces, so a user's `[otel]` key, or a key under `otel.exporter`, wins over the system
  file.
- **Keys the agent owns** in `config.toml`: `otel.exporter = { otlp-http = { endpoint =
  "http://<http_listen>/v1/logs", protocol = "binary", headers = { Authorization = "Bearer <token>" } } }`
  and `otel.log_user_prompt` (true at `m1` and higher for `app:codex`). The trace and metrics
  exporters, `log_agent_responses` (default false, its own opt-in) and every other key stay the
  customer's. The file is read and written with `BurntSushi/toml` as task 39's is: a rewrite drops
  comments, which a restore puts back; a byte-order mark is kept.
- **Tampering is a user's `[otel]`, laid over the system file as Codex merges, that changes the
  log exporter** (disables it, or another exporter, endpoint, header or protocol). A user's
  `log_user_prompt = false` alone is not: the export continues and the prompt event keeps its
  length. A missing file, or one Codex could not parse (it would not start), overrides nothing. The
  check reads `<profile>\.codex\config.toml` of every profile the inventory lists, on each health
  report, never writes it, and runs only while the export is on. The row is
  `degraded`/`config_tampered`, as the brief says (`DESIGN.md` §1 names the state `tampered`).
- **Not detected** (narrowest reading of "every user profile's `config.toml`"): a user whose
  `CODEX_HOME` points elsewhere, a selected profile file, and `-c otel...` flags. The row can be
  healthy while such a user's sessions export elsewhere.
- **`config_tampered`** is added to the detail vocabulary (device only in `check-vocab`); task 34
  reports the same detail. A `Writer` may implement `Overridden(Desired) bool`, which the
  provider's health reads after `Holds`.
- **Task 39's provider tests** that pinned the OTel switch as doing nothing for Codex now expect it
  to switch the provider on and to add the OTel fields to the desired state; their hook assertions
  are unchanged.
- **Not run here:** `TestCodexConfigPath` and `TestCodexUserConfigsAreInProfiles` in
  `toolconfig/codex_windows_test.go`. They compile and vet under `GOOS=windows`.
## 2026-10-08, task 31

- **Sources, read 2026-10-08.** `microsoft/vscode` at `f6f19d60`: the policy catalog
  (`build/lib/policies/policyData.jsonc`), the OTel policies in
  `src/vs/platform/agentHost/common/agentHostStarter.config.contribution.ts`, the policy services in
  `src/vs/platform/policy` and `src/vs/code/electron-main/main.ts`, and the Copilot extension
  `extensions/copilot` 0.70.0 (`package.json`, `otelConfig.ts`, `otelConfigResolution.ts`).
  `microsoft/vscode-policy-watcher` at `42c46404` (how VS Code reads registry policies).
  `microsoft/vscode-docs` at `a8848427`, `docs/enterprise/policies.md` (`DateApproved` 10/7/2026).
  `github/docs` at `9f651797`: the CLI command reference's "OpenTelemetry monitoring", the
  enterprise managed settings reference and "Deploy managed settings". `github/copilot-cli`
  `changelog.md` at `a7ae5b0c` (latest 1.0.94).
- **VS Code policies exist.** The extension's `github.copilot.chat.otel.*` settings declare no
  `policy` of their own but a `policyReference` to policies VS Code owns, read from VS Code 1.127 at
  `HKLM\SOFTWARE\Policies\Microsoft\VSCode` (HKCU only when HKLM has none):
  `CopilotOtelEnabled` (`REG_DWORD`), `CopilotOtelEndpoint` (`REG_SZ`), `CopilotOtelHeaders`
  (`REG_SZ`, a JSON object) and `CopilotOtelCaptureContent` (`REG_DWORD`), which the agent writes,
  and protocol, wire protocol, outfile, service name, resource attributes and identity, which it does
  not. With any of them set the extension takes its whole OTel configuration from the policies and
  ignores the user's settings. Limitation: the extension still lets environment variables
  (`COPILOT_OTEL_ENDPOINT`, `OTEL_EXPORTER_OTLP_ENDPOINT`, `COPILOT_OTEL_CAPTURE_CONTENT` and
  others) override those settings, policy values included.
- **Degraded case.** `degraded`/`tool_version_unsupported` when the IDE extension scan finds the
  Copilot extension in Cursor or Windsurf (other products, which do not read VS Code's policy key) or
  the installed-app scan finds a VS Code older than 1.127. A VS Code whose version is unknown counts
  as reading the policies. The VS Code values are written whenever the extension is found, so a later
  VS Code update takes them up.
- **The CLI** reads its OTel settings from environment variables, or from GitHub's enterprise
  managed settings (below); it has no other machine-wide file. Its endpoint, header and content
  switches have no `COPILOT_OTEL_*` names (the brief assumed they had): the agent writes
  `COPILOT_OTEL_ENABLED=true`, `OTEL_EXPORTER_OTLP_ENDPOINT`,
  `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer <token>` and
  `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT` (`true` or `false` from the mode for
  `app:copilot_cli`) into the machine environment, and only while the CLI scan finds the CLI. For
  the owner: these standard names reach every OTel-instrumented program on the device, which then
  exports to the receiver (the generic normalizer takes what it sends); a user variable of the same
  name overrides the machine one, so a user can turn the CLI's export off or redirect it.
- **GitHub's enterprise-managed export does not target a cloud collector.** It is the `telemetry`
  managed setting (CLI 1.0.66 and later, and VS Code): client-side configuration whose endpoint,
  headers and content capture the administrator chooses, so it can target the device-local receiver.
  It is delivered server-managed (the enterprise's `.github-private` repository), by native MDM
  (`REG_SZ` values such as `telemetry.endpoint` under `HKLM\SOFTWARE\Policies\GitHubCopilot`) or
  as a file (`%ProgramFiles%\GitHubCopilot\managed-settings.json`); MDM wins over server, server
  over file, file over user settings, and users cannot override it. **Not used**: one setting read by
  VS Code, the CLI and every other Copilot client cannot carry separate content modes for
  `app:github_copilot` and `app:copilot_cli`, and VS Code would apply it over its own policies. It is
  the stronger mechanism for the CLI (not overridable, no effect on other programs); adopting it is
  the owner's call. A customer's own `telemetry` managed setting overrides what the agent writes,
  and the health row cannot see that.
- **Environment writes go through the registry, not `setx`** as the CLI shim's do: restoring a
  replaced variable needs its kind (`REG_EXPAND_SZ` included) and full length, and removing an added
  one needs a delete, neither of which `setx` does. The `WM_SETTINGCHANGE` broadcast `setx` makes is
  sent with `SendMessageTimeoutW` after the writes. From the service it reaches session 0 only, as
  the shim's does: a process started with a fresh environment block sees the change at once,
  Explorer in a user's session at the next sign-in.
- **Backup per value**, each taken before the agent first writes it (a part found later is backed up
  then), together in `toolconfig\copilot\original`. `Remove` restores or deletes every backed-up
  value and then drops the backup. A part not installed is not written; one uninstalled after a
  write keeps the values until the export is switched off.
- **Provider.** `Desired.LogCLIPrompts` beside `LogPrompts`, resolved for the CLI's fingerprint; a
  tool flag makes the hooks switches do nothing for Copilot, whose collector follows
  `endpoint.otel.enabled && endpoint.tools.copilot.otel` only. The migration is the next free number
  on this branch.
- **Device phase to confirm**: that the IDE extension scan finds Copilot Chat in VS Code (the
  extension's source now lives in `microsoft/vscode` and may ship built in rather than in the user's
  extensions folder), and that the installed VS Code and CLI honour the values above.

## 2026-10-08, task 41

Build phase: the golden tests (`hooks/testdata/<tool>/render/`) render a warn and a block with a
280-character message and an https link for every declared event. Everything below is **from the
documentation**, read 2026-10-08, for the device phase to confirm on the installed version. No
source contradicts the current output, so `Render` and `canEnforce` are unchanged.

- **Claude Code** (code.claude.com `hooks`, `hooks-guide`, `interactive-mode`, `.md` form; changelog
  latest 2.1.295 of 2026-10-08). `systemMessage` is a "warning message shown to the user", and a
  `UserPromptSubmit` block's `reason` is "shown to the user"; `permissionDecisionReason` on a deny
  goes to Claude. A warn is therefore shown. `systemMessage` is capped at 10,000 characters, so 280
  plus a link is not truncated. No page says whether these messages are Markdown or plain text, how
  they wrap, or whether a URL in them is a hyperlink (the documented hyperlinks are Claude Code's own
  PR badge and issue references). **[device]** Markdown, wrapping, link clickable.
- **Cursor** (cursor.com still blocked; the npm sources task 38 lists). `user_message` is "shown to
  the user" / "in Cursor's UI"; `@vaibot/cursor-circuitbreaker-plugin` shortens a derived one to a
  160-character line, its own choice, not a documented limit. No source says whether Cursor shows a
  `user_message` with `continue: true` or `permission: "allow"` (a warn), and `cursor-hooks` 1.1.6's
  `beforeSubmitPrompt` response has `continue` only. Nothing on Markdown or links. **[device]**
  whether a warn is shown on each event; if not, warn there is recorded `logged`.
- **Codex CLI** (`openai/codex` `main` at `780d7ab`, 2026-10-08, `codex-rs/tui/src/history_cell/hook_cell.rs`
  and `codex-rs/hooks/src/events/user_prompt_submit.rs`; the brief predates task 39, so Codex is
  covered too). A block's `reason` becomes a feedback entry shown as "Blocked by hook" with the
  reason's lines below it, plain text split at `\n`; a `systemMessage` becomes a warning shown as
  "↳ Hook · " and its lines, with ANSI styles interpreted and no Markdown. Neither is truncated (only
  additional context is). The hook cell writes no hyperlink, so a link is clickable only where the
  terminal detects URLs. **[device]** both, and link clickability in Windows Terminal.
## 2026-10-08, task 51

- **`TestHookDecisionBudget` (`hooks/budget_test.go`) gates 40 % of the whole-process budget**:
  p99 under 20 ms against §13's 50 ms. It times the service's side only (connect, a
  `hook_evaluate` frame with a 4 KB prompt in, classification by a real classifier-host under its
  component supervisor, rule evaluation, the decision frame out) over a loopback TCP socket, 2,000
  decisions, every tenth with an AWS key that must be blocked, recorded to a real spool. The other
  60 % is what the CI test cannot see and a runner cannot time stably: process creation, the Go
  runtime's start, reading stdin, the endpoint's peer check and the console write, which task 36's
  whole-process numbers put at several milliseconds on Linux and which is larger on Windows. On
  this machine the service's side is p50 3.5 ms and p99 6.5–8.2 ms (5 runs), 9.1 ms under `-race`,
  so 20 ms leaves a loaded runner about twice the headroom and still fails a 30 ms regression.
- **Loopback TCP, not the native endpoint.** `localipc.Dial` takes a different trust callback per
  OS, so a build-tag-free test cannot dial it on both; the relay is served exactly as the service's
  `serveLocal` hands it a `hook_evaluate` connection. The peer-credential check is not timed.
- **A decision made with the labels unknown fails the test**, as in task 36's benchmark: a
  classification that ran out of its 30 ms measured a timeout, not a decision.
- **`TestOTLPBudget` (`otlp/budget_test.go`)**: `otlploghttp` (retries off, so a failed request
  is a dropped batch) exports 20 records every 10 ms for 10 s, 20,000 records in 1,000 requests,
  each record a Claude Code style `user_prompt` with a 1 KB prompt attribute. Request handling time
  is timed in the exporter's HTTP transport, from sending the request to the response's headers,
  so it includes loopback transfer and is an upper bound on the receiver's own time. The receiver
  runs with its real sender lookup; a counting normalizer stands in for the normalizers and
  pipeline. None dropped means every export succeeded, the normalizer was handed 20,000 records,
  and the receiver counted 20,000 `observed` and no `errors`. Here: p50 0.6 ms, p99 1.4–3.2 ms
  (5 runs), 4.1 ms under `-race`.
- **No single-core scaling.** Neither test failed in five consecutive runs of `go test -count=5`
  on this shared 4-core Linux machine (load average 0.6–1.8), and both passed in `node
  tools/accept.mjs`, so the raw thresholds stand and nothing is scaled. **Risk under contention:**
  in the whole module's `go test -count=1 ./...` (packages run concurrently) while other builds
  loaded the machine (load average 12–18), `TestHookDecisionBudget` failed 2 of 5 runs (p99 21.0
  and 64.8 ms, with 1 and 4 secret prompts allowed after the 30 ms classification ran out) and
  `TestOTLPBudget` passed all 5 (p99 6.6–8.6 ms). Under `-race ./...`, as CI runs the go gate, at
  load 1–5 both passed twice (decision p99 15.4 and 19.7 ms, request p99 14.2 and 11.8 ms); in a
  third run, next to another session's `-race` tests, both failed (35.9 ms, 29.5 ms). A hosted
  runner has no foreign load, but the race detector's margin is thin; if CI flakes, the brief's
  scaling rule (or a CI-side decision) is the owner's call, since the budgets stay as they are.
- **Both fail on a 30 ms sleep** in the measured path (in `Relay.Serve` before the answer, in the
  OTLP/HTTP handler before routing; reverted): decision p99 39.8 ms, request handling p99 34.9 ms.
- **`device/installer/windows/measure-idle.ps1`** samples the ShadowAICapture service's process (or
  `-ProcessId`, for a capture-core started by hand) and its descendants every 2 s for 10 minutes
  with `Get-Counter` (`ID Process`, `% Processor Time`, `Working Set - Private`), re-reading the
  process tree each sample so a restarted classifier-host or a new session's helper counts. It sums
  the tree: average CPU as a share of one core, and the peak of the tree's total private working
  set (MB = 1,048,576 bytes). It exits 1 at or above 1 % or 150 MB. The counter paths are the
  English names, as on the reference VM. **Not run and not syntax-checked in the build:** this
  machine is Linux with no `pwsh`; the "runs on the PC" check is pending on a Windows host.
## 2026-10-08, task 52

- **The rig is assembled from the agent's packages.** The service (`cmd/capture-core`) is a `main`
  package, which the integration module cannot import, and running the built service would touch
  the machine (trust store, managed settings, the fixed native endpoint). `privacy_test.go` builds
  the service's graph from the same packages: proxy.tls, the loopback broker, the OTLP receiver and
  the hook relay in a `core.Registry` started by `core.Supervisor` under one bundle (which passes
  `Bundle.Validate`), the merge buffer in front of the pipeline for hook and OTel prompts, the real
  classifier-host, spool, content store and drain, and the native endpoint on `localipc`. The
  service's native frame dispatch (`serveLocal`, the observation handling of `native.go`) and its
  `health.json` document are repeated in the test over the same packages; a leak confined to those
  two `main`-package functions would not be seen by this test. Both were read for one: neither
  quotes content (a refusal quotes the pipeline's error, whose messages carry no text).
- **The fake edge** is the module's existing `startTLSIngest` (`/v1/events` and `/v1/health` over
  mutual TLS), which is the `fakeCloud` pattern without enrolment and policy: the rig saves an
  issued credential and puts the bundle in force directly.
- **The hook path is a real hook process**: `capture-core --hook test UserPromptSubmit`, built
  with `-X main.hookBenchEndpoint=<the rig's endpoint>` as the hook benchmark does, standing in
  for Claude Code. Its stderr is searched with the logs.
- **One canary per path** (`SACCANARY-<uuid>`), so a hit names its path. Each prompt carries
  `AKIAIOSFODNN7EXAMPLE` beside the canary.
- **Encodings searched:** the text (case-sensitive), hex, standard and URL base64 at each byte
  alignment, and JSON `\u` escapes in either case. A canary is letters, digits and hyphens, so
  ordinary JSON escaping leaves it as text.
- **m0 sends text anyway.** At m0 the extension sends no content and the tool's prompt logging is
  off; the rig sends the canary through every path at m0 regardless, so the agent's own mode gate
  is what keeps it out.
- **The merge hold** (`merge.HoldFor`, a real 10 s timer) is not shortened: the rig closes the
  buffer before it searches, as the service's stop does, which releases every held prompt to the
  pipeline. The search covers what was released.
- **Result:** zero hits at m0 and at m1 for all five paths in every location; at m1 each path's
  prompt envelope carries a `sha256:` digest and the `credential` label.

## 2026-10-09, task 34

- **The watcher is one `toolconfig.Watcher` for every tool config provider**, built in
  `buildProviders` and run on the service's background loops (it stops before the providers do). A
  provider registers with it when it starts (after its first apply) and leaves it when it stops, so
  only running writers are watched. Registering schedules a comparison, which is the brief's "on
  start" comparison; every running provider is also compared every 60 seconds.
- **What is watched.** A file writer's `Path()`; Codex's `CodexFiles` names both its files. Each
  file's folder is watched with `github.com/fsnotify/fsnotify` v1.10.1 (2026-05-04, the latest tag;
  `DESIGN.md` §11), and only write, create, rename and remove events for the file itself (or the
  folder's own deletion) schedule a comparison, 250 ms after the last one. A folder that does not
  exist yet, or was deleted, is watched again after the next comparison. If file notifications cannot
  start, the backstop alone compares.
- **Registry.** `machineRegistry` gains `watch(key, stop, changed)`; Copilot watches both of its keys
  (`HKLM\SOFTWARE\Policies\Microsoft\VSCode` and the machine environment). On Windows it is
  `RegNotifyChangeKeyValue` (`REG_NOTIFY_CHANGE_NAME | REG_NOTIFY_CHANGE_LAST_SET |
  REG_NOTIFY_THREAD_AGNOSTIC`, asynchronous, re-armed after each signal); a key that does not exist is
  looked for every second, and its deletion and creation count as changes. Any change in the machine
  environment key schedules a comparison; the comparison decides.
- **Compare-then-apply** runs under the provider's lifecycle lock, against what the provider last
  applied, so a bundle's apply and the watcher never interleave and the agent's own write (its echo
  included) compares clean. Only a provider whose last apply succeeded is compared: a failed apply
  that no outside change caused stays `degraded`/`config_write_failed` and waits for the next bundle,
  as before. A re-apply after an outside change that fails counts `errors`, keeps the row tampered,
  and is tried again at each later comparison.
- **"For the rest of the health interval"**: the provider does not know the health interval, so the
  tamper holds until `Health` has returned it once (a health report carried it) and a later comparison
  is clean. It has precedence over `config_write_failed`; a stop clears it. Task 29's user override
  stays `degraded`/`config_tampered`.
- **One log line per re-apply**, naming the tool key and `Path()` (both Codex files, or both registry
  keys for Copilot); a failed re-apply also logs the existing write-failure line. No content is logged.
- **Fix outside the brief's files:** on the integration branch `toolconfig` did not compile for
  Windows: `machineenv_windows.go` (local model relocation) and task 31's `copilot.go` and
  `copilot_windows.go` both declared `machineEnvKey` and `procSendMessageTimeoutW`. The duplicates in
  `machineenv_windows.go` are removed; the values are the same.
- **Not run here:** `TestWinRegistryWatch` in `toolconfig/copilot_windows_test.go` (the real
  `RegNotifyChangeKeyValue` watch under HKCU) and the existing Windows `toolconfig` tests. They compile
  and vet under `GOOS=windows`. The fsnotify tests ran on Linux (inotify) only; Windows'
  `ReadDirectoryChangesW` backend is for the device phase.

## 2026-10-09, task 50

- **The cleanup is `capture-core --uninstall-cleanup`**, read before any other mode, with the
  service's two configuration files. Only the state directory is used; a missing or incomplete
  tenant file does not stop it (a step that needs the state directory reports `failed` when there is
  none). Steps, in order, one log line each: `tool_config_claude_code`, `tool_config_codex`,
  `tool_config_copilot`, `tool_config_cursor` (each writer's `Remove`, from its backup),
  `ollama_host` (task 57's `Restore`), `desktop_proxy_pac`, `trust_root`, `device_root_key`,
  `quic_firewall_rules`, `cli_shim_environment` (the shim's own `Stop`). The log line is
  `<UTC time> <step>: ok|failed: <error>|skipped, <reason>`, appended to
  `%WINDIR%\Temp\ShadowAICapture-uninstall.log` and printed; the exit code is always 0. The whole
  run is bounded at two minutes for the steps that take a context.
- **The PAC originals are now kept on disk** (`pac-originals.json` in the state directory; a
  deviation the brief's "winproxy's existing restore" did not foresee). That restore kept each
  user's original in memory only, so nothing outside the running service could put it back: a
  user whose hive was unloaded before the service restored it, or a crash, left the agent's PAC
  URL behind. Each user's previous `AutoConfigURL` and the URL applied are recorded before the PAC
  replaces it and forgotten when it is restored. The cleanup restores a recorded user whose value
  is still the applied URL, leaves one who changed it since, and keeps (and reports) one whose hive
  is not loaded. A run that finds a record applies over it with the recorded original, so the PAC
  delegates to the user's own PAC rather than to the agent's stale URL.
- **The device root is retired as task 44 retires a file-key root**: installed again (idempotent)
  and removed, and only when the store holds it. `tlsproxy.RetireDeviceRoot` and
  `tlsproxy.DeleteDeviceKey` are the uninstall's; the CNG delete (`NCryptOpenKey` with
  `NCRYPT_MACHINE_KEY_FLAG`, then `NCryptDeleteKey`) moved from task 44's Windows test into
  `ca_windows.go`, and a key that is not there (`NTE_BAD_KEYSET`) is already deleted.
- **QUIC firewall rules**: none exist (task 47 built none). The step removes every rule whose name
  starts `ShadowAICapture QUIC ` through `INetFwPolicy2` (ProgID `HNetCfg.FwPolicy2`, go-ole's
  IDispatch), calling `Remove` once for each rule that carries such a name, and reports the count.
- **The MSI action** `UninstallCleanup`: `FileRef` capture-core.exe, deferred, no impersonation,
  `Return="ignore"` (type 3154), `After="StopServices"` (so before `RemoveFiles`), condition
  `REMOVE="ALL" AND NOT UPGRADINGPRODUCTCODE`. `manifest.mjs` gains `UNINSTALL_CLEANUP`; `verify.mjs`
  checks the generated action and that the argument and log name equal capture-core's constants;
  `releaseChecks` reads `CustomAction` and `InstallExecuteSequence` from a built MSI.
- **`snapshot.ps1`** records one flat map: the managed files' and the shortcut's SHA-256 (or
  `absent`), each loaded user hive's Internet Settings values, the machine environment, VS Code's
  machine policies (Copilot's managed values), the Root store (thumbprint and subject), the machine
  keys of the Microsoft Software KSP (`NCryptEnumKeys`, P/Invoke) and the firewall rule names with
  their counts. It records every entry of each location, not only the agent's, so a root Windows
  fetches on demand between the two snapshots shows as a difference; the device phase judges such
  a line by its subject.
- **Integration fix**: `toolconfig/machineenv_windows.go` (task 57) and `copilot*.go` (task 31) both
  declared `machineEnvKey` and `procSendMessageTimeoutW`, so the integration branch did not build
  for Windows. The machine environment now uses Copilot's declarations and broadcast.
- **Left as found, not changed here** (each outside the brief's steps): a folder the agent created
  for a tool's file (`C:\Program Files\ClaudeCode`, `C:\ProgramData\Cursor`,
  `C:\ProgramData\OpenAI\Codex`) stays, empty, when the file is deleted, and so does an empty
  `HKLM\SOFTWARE\Policies\Microsoft\VSCode` key Copilot's writer created; the snapshot compares files
  and values, so it does not show them. The CLI shim deletes its variable names from the machine
  environment without a backup, so a customer's own machine `HTTP_PROXY` (or another of its names)
  overwritten while TLS inspection was on is not restored. A user not signed in at uninstall keeps
  an unrestored PAC in their unloaded hive.
- **Vendor facts** (MicrosoftDocs `sdk-api` sources, read 2026-10-09; learn.microsoft.com is not
  reachable from the build machine): `NCryptOpenKey` (ms.date 12/05/2018: `NCRYPT_MACHINE_KEY_FLAG`
  opens the machine key, `NTE_BAD_KEYSET` when no key has the name), `NCryptDeleteKey` (12/05/2018:
  frees the handle on success only), `NCryptEnumKeys` (12/05/2018: `NCRYPT_MACHINE_KEY_FLAG`,
  `NTE_NO_MORE_ITEMS`, both buffers freed with `NCryptFreeBuffer`), `INetFwRules::Remove`
  (12/05/2018: removes by name, no effect for an unknown name). The `HNetCfg.FwPolicy2` ProgID is
  not on a page read here; the device phase confirms it.
- **Not run here:** `TestCNGKeyDeleteRemovesTheKey` (`proxy/tlsproxy/ca_windows_test.go`, needs a
  Windows administrator), the COM firewall removal, the registry and certutil paths of the cleanup,
  the MSI's action and `releaseChecks`, and `snapshot.ps1` (no PowerShell here). They compile and
  vet under `GOOS=windows`; the device phase runs the brief's "On the device" section.

## 2026-10-09, task 49

- **"The device detail view" is the Search page's device row detail** (`explore-render.js`), the
  only per-device view the dashboard has (narrowest reading). Opening a device row reads that
  device's collector rows and shows the "Collectors" table: collector, state, cause in words
  (`COLLECTOR_DETAILS` in `vocab.js`, one entry per device detail, held equal to
  `device/protocol` by `test/parity.test.mjs`) and last report time. A `disabled_by_policy` row
  shows "Off in policy", untinted, with no cause.
- **The closed query is a registry source, `ops.collector_state`**, read as a DSL list document
  (`dsl.js` `buildListDocument`), not an eleventh template: the templates are the ten questions.
  It selects device, collector, state, `error_code`, `last_report_at` and `last_success_at`, and
  needs the `device` capability (viewer and up), as `mart.v_device_liveness` does.
- **Not person-resolving, so not audited (checked against `audit.js` `auditDecision`):** a read is
  audited when it filters on `subject`, its source is `subjectBearing`, it is a single-record read,
  it reads `ops.audit`, or a k-suppressed aggregate resolves below k. The source names no person
  (no `user_ref`, name or hostname) and is `subjectBearing: false`, so none applies;
  `audit-plan.test.mjs` asserts the plan writes no audit row.
- **`inventory.Installed` is a method of the inventory provider**, `(*Provider).Installed(appKey)`:
  the last scan belongs to the running provider. It gives the lowest release when an app is
  installed more than once, and nothing before the first scan or after the scanner stops, so with
  the inventory switched off no version is checked.
- **Each tool's minimum is in its description in `toolconfig`** (`versionApp`, `minVersion`). Below
  it the provider writes nothing and the row is `degraded`/`tool_version_unsupported`; a version
  the scan could not read does not hold the write back. As with `Installed`, an update is picked up
  by the next bundle or restart. The drift watcher (task 34) does not compare a provider held back
  this way, since nothing was written.
- **Minimum versions (vendor facts, read 2026-10-09):**
  - Claude Code **2.1.49**, from `anthropics/claude-code` `CHANGELOG.md` (newest 2.1.295): 2.0.58
    first reads `C:\Program Files\ClaudeCode\managed-settings.json`, and 2.1.49 stops non-managed
    settings disabling managed hooks. `allowManagedHooksOnly` has no introduction entry (it is named
    from 2.1.101); the 2.1.223 per-key `env` merge matters only beside server-managed or MDM
    settings and is not required. **[device]** confirm on the installed version.
  - Codex **0.131.0**, from `openai/codex` release tags (raw files at each tag):
    `codex-rs/config/src/config_requirements.rs` has managed hooks from 0.124.0 and
    `allow_managed_hooks_only` from 0.131.0; at 0.131.0 the loader reads the ProgramData layer and
    `[otel]` has `otlp-http`.
  - Copilot CLI **1.0.4**, from `github/copilot-cli` `changelog.md` (1.0.4 of 2026-03-11 "Enables
    OpenTelemetry instrumentation"; newest 1.0.94). Whether 1.0.4 already reads the standard OTLP
    variable names is not in the changelog. The IDE side keeps task 31's VS Code 1.127 check. The
    check is per tool, so a CLI below 1.0.4 also holds back the VS Code policy values.
  - Cursor **1.7.0**: hooks arrived in Cursor 1.7 per task 38's npm sources (`cursor-hooks` 1.1.6
    mirrors the 1.7-era hooks page); no first-party page is reachable. **[device]** confirm.
- **`no_recent_events`**: the row is degraded when a process of one of the tool's catalog apps
  (`toolByApp`) was seen running in the last 24 hours, the agent's configuration had been in place
  the whole 24 hours (a restart, a failed or skipped apply, or a drift re-apply starts it again),
  and neither the OTLP receiver nor the hook relay had anything from the tool in that time. The
  seams are `otlp.Receiver.LastReceived(normalizer)`, `hooks.Relay.LastServed(tool)` and
  `procmon.Provider.LastRunning(app)`, all in memory since the service started; the service maps
  each tool to its normalizer (`claude-code`, `codex`, `copilot`). It is checked after every other
  cause, so it never hides one.
- **Cursor and the Copilot CLI have no `windows_exe` in the catalog** (Cursor is matched by
  publisher, the CLI by npm package), so the process monitor never sees them run and their rows do
  not report `no_recent_events` until the catalog gains one. Adding signals is a schema change
  outside this brief.
- **Server**: control-api validates a report with `protocol.HealthRequest.Validate`, which checks
  each detail against `AllDetails`, so `no_recent_events` passes once it is in the vocabulary
  (`TestReportStoresToolCollectorCauses`). `ops.collector_state.error_code` has no CHECK, so there
  is no schema change and no migration. `check-vocab` lists the new detail as device-only.
- **Task 06's open item is not closed here.** `gap_reasons` is computed by `coverageStatement` in
  `blocks.js`, not by the new query, so the new query is not the natural place; the fix stays
  `AND v.expected` there (or the schema change task 06 names).
- **Rebased onto the integration branch with task 34 merged.** The brief's "On the device" step 3
  expects `degraded`/`config_tampered`; since task 34 a drift is reported `tampered` with
  `config_tampered`.
- **Not run here:** the existing Windows tests of the touched packages. The new device tests are
  platform-neutral and ran on Linux; the packages compile and vet under `GOOS=windows`.
