# Endpoint agent refactor: design

The plan (`PLAN.md`) is written for a generic product. This file fixes how it maps onto this
repository: the names, wire shapes, settings, ports and dependencies every task uses. A task brief
refers to sections here by number. When a task and this file disagree, this file wins; stop and
report the conflict.

## 0. Install once, control from the dashboard

- A device is installed the standard way and never again for a feature. On Windows, Intune
  installs the generic MSI and the tenant file (`SAC_TENANT_ID`, `SAC_DEVICE_ENDPOINT`,
  `SAC_DEPLOYMENT_KEY`).
- No task adds an install-time configuration key, command-line flag, MSI property or second
  package to enable or tune a capability.
- Every collection capability and every policy is a tenant setting:
  - edited on the dashboard's Settings page;
  - carried in the signed policy bundle;
  - applied by the device on its next policy poll without a restart (task 06);
  - collectors, per-tool native telemetry and hooks, TLS inspection, the loopback broker, rules,
    collection modes and kill switches all follow this.
- The device's own wiring, such as the hook command line or the OTLP token, is written by the
  agent itself, never by an administrator.
- The one exception is what an operating system allows only through MDM (on macOS, system
  extension approval and Full Disk Access). It belongs to the deferred packaging work (E38) and is
  granted once at install, never per feature.

## 1. How the plan's terms map onto the repository

| Plan term | In this repository |
|---|---|
| Endpoint agent, agent core | `device/capture-core` (service `ShadowAICapture`) |
| Collector, `Start`/`Stop`/`Health` | `core.Provider` (`device/capture-core/core/health.go`), held by `core.Registry` (`core/registry.go`) |
| `aigov.event.v1` envelope | The event envelope in `contracts/event-envelope.schema.json` (`schema_version` `1.0`), extended in §3 |
| `app_key` | `tool_fingerprint` of the form `app:<app_key>` (§4) |
| Shared app catalog | `ref.app` and `ref.app_signal` in `services/database/schema.sql`, delivered to devices in the policy bundle (§4) |
| Signed policy bundle, policy cache | `policy.Bundle` (`device/capture-core/policy/bundle.go`) and `policyserve.Bundle` (`services/control-api/internal/policyserve/bundle.go`); verified and cached by `policy.Store` |
| "Content capture on/off", "metadata-only mode" | Collection modes. Metadata-only is `m0` (nothing read) and `m1` (read on the device, only labels and a digest leave it). Prompt text leaves the device only at `m3`, on a grant. There is no separate content switch. |
| Shared detectors | `device/classifier-host` (rules, validators, model), reached through `classifierlink` |
| Spool and uploader | `device/capture-spool` and `device/capture-core/drain` |
| Heartbeat and health report | `POST /v1/health` (`device/protocol/health.go`), one row per collector in `ops.collector_state` |
| Mock cloud | `fakeCloud` in `device/capture-core/cmd/capture-core/cloud_test.go` and `fakeEdge` in `device/capture-core/drain/edge_test.go` |
| Orchestrator | The owner, through this folder. A question this folder does not answer goes to the owner. |
| `DECISIONS.md` | `refactor-endpoint/DECISIONS.md` |
| `endpoint.config_tampered` event | A health row in state `tampered` with detail `config_tampered`, not an event (tampering is a health signal in this product) |
| MCP gateway | Does not exist. Task 48 makes child-process supervision generic so the gateway can plug in later |
| Email and OAuth discovery | Do not exist. Out of scope |

## 2. Collectors, routes and health rows

A route names how an observation was collected (envelope `source`); a collector names the
component whose health is reported. Task 06 re-keys the registry by collector. The full set after
this plan:

| Collector (`ref.collector.code`) | Route(s) it emits | Package | Task |
|---|---|---|---|
| `egress_proxy` | `proxy.tls` | `capture-core/proxy/tlsproxy` | existing |
| `loopback_broker` | `proxy.loopback` | `capture-core/proxy/loopback` | existing |
| `cli_shim` | none | `capture-core/cli` | existing |
| `desktop_proxy` | none (traffic it routes is observed as `proxy.tls`) | `capture-core/winproxy` | 08 |
| `inventory_scanner` | `inv.scan` | `capture-core/inventory` | 16 |
| `process_detector` | `proc.detect` | `capture-core/procmon` | 19 |
| `flow_monitor` | `net.flow` | `capture-core/flowmon` | 21 |
| `otel_receiver` | `tool.otel` | `capture-core/otlp` | 23 |
| `hook_relay` | `tool.hook` | `capture-core/hooks` | 36 |
| `tool_config_claude_code` | none | `capture-core/toolconfig` | 27 |
| `tool_config_codex` | none | `capture-core/toolconfig` | 29 |
| `tool_config_copilot` | none | `capture-core/toolconfig` | 31 |
| `tool_config_cursor` | none | `capture-core/toolconfig` | 38 |
| `user_helper` | none | `capture-core/userhelper` | 10 |

New collectors need a `ref.collector` row (component `capture_core`) before a device reports
them: one unknown collector makes control-api refuse the whole health report. The task that adds
a collector adds its row.

Route fidelity (`ref.route_fidelity`; a lower rank wins a merge): `tool.hook` 5, `tool.otel` 15,
then the existing ranks, then `inv.scan` 75, `net.flow` 80 (both `yields_content` false).
`proc.detect` keeps 70.

New health details (`device/protocol/envelope.go`, `Detail*`, plus `check-vocab`):
`disabled_by_policy` (06), `helper_unavailable` (10), `config_tampered`, `tool_not_installed`,
`tool_version_unsupported`, `config_write_failed`, `etw_session_failed`, `component_crash_loop`
(48) and `no_recent_events` (49). A task adds only the details it uses. A tool whose hooks a
spike task finds (task 39) gets a `tool_config_<tool>` collector in the task that builds its
adapter.

## 3. Envelope changes (contracts)

Task 03 changes `contracts/event-envelope.schema.json`; tasks 04 and 05 follow it on the server
and the device.

- `route` gains `tool.hook`, `tool.otel`, `inv.scan`, `net.flow`.
- `kind` drops `model_detection` (nothing emits it) and gains `discovery` and `agent_activity`.
  The final set is `prompt`, `usage_rollup`, `discovery` and `agent_activity`.
- `detection_basis` is replaced by `installed_scan`, `package_scan`, `extension_scan`,
  `process_event`, `model_store`, `port_listen` and `flow_metadata`.
- New optional fields in `$defs/envelopeCore`:

| Field | Type | Used by |
|---|---|---|
| `discovery_type` | enum `app_installed`, `app_running`, `cli_installed`, `ide_extension`, `local_model`, `inference_connection` | `discovery` (required) |
| `app_version` | string, 1–64 | `discovery` |
| `publisher` | string, 1–200 (code-signing subject or team id) | `discovery` |
| `host_app` | string, 1–128 (`app:<key>` of the IDE an extension lives in) | `discovery` with `ide_extension` |
| `destination_host` | string, 1–253, lower-case host name | `discovery` with `inference_connection` |
| `model_names` | array of string 1–200, at most 64 | `discovery` with `local_model` |
| `activity_type` | enum `model_request`, `tool_call` | `agent_activity` (required) |
| `model` | string, 1–128 | `agent_activity` with `model_request` |
| `input_tokens`, `output_tokens` | integer ≥ 0 | `agent_activity` with `model_request` |
| `duration_ms` | integer ≥ 0 | `agent_activity` |
| `tool_name` | string, 1–128 | `agent_activity` with `tool_call` |
| `outcome` | enum `success`, `error`, `denied` | `agent_activity` |

- Kind rules, in the style of the existing `allOf` branches:
  - `discovery` requires `discovery_type` and `detection_basis`. Its direction is `none`. It
    forbids every content-derived field (`content_digest`, `labels`, `classifier_version`,
    `content_excerpt`, `confidence`, `attachments`, `prompt_kind`), `policy_decision`,
    `size_bytes`, the rollup window fields and every `agent_activity` field.
  - `agent_activity` requires `activity_type`. Its direction is `none`. It forbids the same
    content-derived fields, `policy_decision`, the window fields and every `discovery` field.
  - `prompt` forbids every `discovery` and `agent_activity` field.
  - `usage_rollup` keeps its rules and also forbids the new fields.
- `user_ref` stays required. A machine-wide fact (an app installed for all users) carries
  `user_ref` `unattributed`, the value capture-core already uses when no person resolves.
- `tool_fingerprint` for every endpoint-collected record is `app:<app_key>` (§4) when the catalog
  matches. A sender the catalog doesn't know (generic GenAI telemetry, task 33) is
  `exe:<first 16 hex characters of sha256(lower-case image base name)>`, or `exe:unknown` when
  the sending process isn't resolved.
- `dedup_key` for `discovery` is the device's own per-day key (§6). For `agent_activity` it is the
  sha256 of `tenant|device|tool_fingerprint|activity_type|<tool's own event id or timestamp>|duration_ms`.

## 4. App catalog

Task 14 adds the catalog. These are the tables in `services/database/schema.sql`:

```
ref.app (
  app_key       text PRIMARY KEY CHECK (app_key ~ '^[a-z][a-z0-9_]{1,63}$'),
  display_name  text NOT NULL,
  vendor        text NOT NULL,
  category      text NOT NULL CHECK (category IN ('chat_assistant','coding_agent','ide_assistant',
                  'ide','local_runtime','inference_api','ai_feature')),
  source_url    text NOT NULL            -- where the signals were verified
)
ref.app_signal (
  app_key   text NOT NULL REFERENCES ref.app,
  platform  text NOT NULL CHECK (platform IN ('windows','macos','linux','any')),
  kind      text NOT NULL CHECK (kind IN ('windows_exe','windows_uninstall_name','windows_appx',
              'macos_bundle_id','linux_package','publisher','cli_binary','npm_package',
              'pipx_package','ide_extension_id','inference_domain','listen_port','model_store')),
  value     text NOT NULL,
  PRIMARY KEY (app_key, platform, kind, value)
)
```

- `ref.tool_catalogue` gains a nullable `app_key` column referencing `ref.app`, and
  `signal_kind` gains the value `endpoint`. Every `ref.app` row has a matching
  `ref.tool_catalogue` row `('app:<app_key>', display_name, vendor, 'endpoint', '{}', app_key)`,
  so `ops.tool_display_name()` and sanction work on endpoint fingerprints unchanged.
- The bundle carries the catalog as `catalog` (§5), composed by control-api from both tables.
- The seed `app_key` values are fixed, so later tasks can rely on them: `claude_desktop`,
  `chatgpt_desktop`, `cursor`, `windsurf`, `vscode`, `jetbrains`, `claude_code`, `codex`,
  `gemini_cli`, `copilot_cli`, `github_copilot`, `claude_code_vscode`, `continue`, `ollama`,
  `lm_studio`, `openai_api`, `anthropic_api`, `azure_ai_foundry`, `aws_bedrock`, `google_ai_api`.
  That is 20 rows.

## 5. Policy bundle additions

Each section is added to both `policy.Bundle` (device) and `policyserve.Bundle` (server) with
identical JSON names. The device decoder refuses unknown fields, so the device side lands in the
same task as the server side, and the drift test (task 01) must pass. No fleet is deployed, so
there is no version gating; old agents are reinstalled.

```jsonc
"interception": { "enabled": false, ... existing fields ... },          // task 08
"endpoint": {                                                           // task 07
  "inventory": { "enabled": true, "interval_minutes": 360 },
  "processes": { "enabled": true },
  "flows":     { "enabled": true },
  "otel":      { "enabled": true, "http_listen": "127.0.0.1:47318", "grpc_listen": "127.0.0.1:47317" },
  "hooks":     { "enabled": true, "managed_only": false },
  "tools": {
    "claude_code": { "otel": true,  "hooks": true },
    "codex":       { "otel": true,  "hooks": false },
    "copilot":     { "otel": true,  "hooks": false },
    "cursor":      { "otel": false, "hooks": true }
  },
  "discovery_daily_budget": 200
},
"rules": [                                                              // task 11
  { "rule_id": "block_credentials", "action": "block",
    "match": { "labels": ["credential"], "tools": [], "categories": [], "sanction": [], "routes": [] },
    "message": "Remove the credential and try again.", "link": "https://intranet.example/ai" }
],
"sanctioned_tools": ["app:claude_code"],                                // task 11
"catalog": [                                                            // task 14
  { "app_key": "cursor", "category": "ide", "signals": [ { "platform": "windows", "kind": "windows_exe", "value": "Cursor.exe" } ] }
]
```

- `tools` keys are the closed set `claude_code`, `codex`, `copilot` and `cursor`, plus `ollama`
  with the single switch `loopback` (task 57, which emits the existing `loopback` section rather
  than a field under `endpoint`). A tool switch is effective only when its collector (`otel` or
  `hooks`) is enabled.
- Rules:
  - `action` is one of `allow`, `warn`, `block`.
  - Every non-empty `match` list must match. Empty lists match anything. Matching uses OR inside a
    list and AND across lists.
  - `labels` are classifier classes (`credential`, `customer_pii`, `government_id`, `health`,
    `legal_commercial`, `payment_card`, `source_code`).
  - `tools` are fingerprints; `categories` are `ref.app.category` values.
  - `sanction` holds `sanctioned` or `unsanctioned`, decided against `sanctioned_tools`.
  - `routes` are envelope routes.
  - The first matching rule wins. No match records `logged` with rule id `policy.default`, as
    today. The envelope records `blocked`, `warned` or `logged`; `allow` records `logged`.
  - `message` is at most 280 characters; `link` is an optional https URL; `rule_id` matches
    `^[a-z][a-z0-9_.-]{0,127}$`.
- "Redact" from the plan is not an action: no hook can rewrite a prompt, and rewriting proxied
  bodies is out of scope.
- Server defaults for a tenant with no settings row: every `endpoint` collector on, every tool on
  for the collectors it supports (`cursor` has no OTel; `codex` and `copilot` hooks stay off until
  task 39 finds them), interception off, no rules.

## 6. Discovery emission

Task 15 builds `capture-core/discovery`, which every discovery collector uses:

- `Record{Type, Basis, AppKey, Version, Publisher, HostApp, DestinationHost, ModelNames, UserRef}`.
- Before a record is emitted it is de-duplicated per UTC day, on the key
  `sha256(device|user_ref|type|app_key|version|destination_host|day)`. The seen-set lives in
  the state directory (`discovery-seen.json`, today's keys only), so a restart does not re-emit.
- At most `discovery_daily_budget` records leave per UTC day. When the budget runs out, further
  records count as `dropped` on the emitting collector's health row and are not spooled.
- `app_running` start and stop are internal: the process monitor reports both to the emitter, and
  only the first start per app, user and day becomes an envelope.

## 7. Local IPC, identity and attribution

- Hooks and the user-session helper use the existing native endpoint (`\\.\pipe\ShadowAICapture.native`;
  `/var/run/shadow-ai-capture/native.sock`), its framing (4-byte length-prefixed JSON frames, up
  to 1 MiB), and its peer-credential check. Task 10 moves the transport out of `package main`
  into `capture-core/localipc` so the hook mode and the helper can share it. New message types are
  added to `device/protocol/native.go` and to `check-vocab` (the extension's `messages.js`
  mirrors only the types it uses; `check-vocab` compares the shared ones).
- Task 09 adds `hostinfo.OwnerOfLocalTCP(localAddr, remoteAddr) (pid int, err error)` and
  `hostinfo.ProcessInfo(pid) (Process{PID, Image, Publisher, User}, error)`. The OTLP receiver,
  the proxy and the flow monitor all attribute through these two calls. Task 20 adds
  `hostinfo.ListenersOn(port) ([]uint32, error)` (the PIDs listening on a local port) beside
  them.
- An observation's person is the owner of the process that produced it, never the console user,
  whenever that process is known.

## 8. Native telemetry

- The OTLP receiver listens on the `endpoint.otel` addresses (loopback only; a non-loopback
  address is a bundle validation error). It accepts OTLP/HTTP (protobuf and JSON) on `http_listen`
  and OTLP/gRPC on `grpc_listen`, for logs, traces and metrics.
- Every request must carry `Authorization: Bearer <token>`. The token is 32 random bytes in hex,
  generated once into `otlp.token` in the state directory, and written into each tool's managed
  configuration. A request without the token gets 401, and its body is not read.
- Metrics are accepted and discarded (counted `skipped_not_generative`). Logs and traces go to a
  per-tool normalizer chosen by `service.name`. Anything unknown goes to the generic GenAI
  normalizer (task 33).
- Prompt text from OTel is handed to `core.Pipeline.Process` as the observation's content reader,
  so the mode gate, classification, excerpting and the content store apply exactly as for the
  proxy. At `m0` the reader is never called and the text is dropped unread.
- Prompt logging in each tool is switched on only when the resolved mode for its fingerprint is
  `m1` or higher (the text is needed to classify locally). At `m0` it is switched off, and the
  prompt event still carries its length.
- Tool configuration is written to each tool's machine-wide, admin-managed location. The original
  file is backed up once to `toolconfig/<tool>/original` in the state directory, and on uninstall
  the agent's keys are removed and any keys it replaced are restored (task 50).

## 9. Hooks

- The hook command is `capture-core.exe --hook <tool> <event>` (installed path, quoted). It reads
  the tool's JSON from stdin and sends one `hook_evaluate` frame to the service over the native
  endpoint. It prints the tool's decision format to stdout and exits 0.
- Fail open:
  - Any error, a missing service, or no answer within 400 ms prints the tool's allow output.
  - The 400 ms is a hard ceiling; the target is task 36's p99.
  - The service never makes a network call on this path.
- The service decides:
  1. It resolves the mode for the tool's fingerprint.
  2. At `m1`+ it classifies with a 30 ms budget (rules and validators; the model stage is skipped
     when the budget does not allow it, and the confidence says so).
  3. It evaluates `rules` (§5).
  4. It records the observation on route `tool.hook` with the decision.
  5. It answers `{action, message, link, rule_id}`.
  The evaluation does not depend on the record being spooled first.
- At `m0` no text is classified, so label rules cannot match. Tool, category, sanction and route
  rules still apply.

## 10. Interception (TLS) is opt-in

`interception.enabled` (default `false`, a tenant setting) gates every interception component:
the TLS proxy, the CLI shim's proxy and CA environment, the Windows desktop-app PAC, and the
per-device root's trust-store installation. With it off, none of them run and nothing is in the
trust store. Toggling it takes effect on the next policy poll without a restart. With it on, the
proxy does not decrypt a connection whose owning process is a tool with an enabled native
collector (`hooks` or `otel` for that tool); that connection is blind-tunnelled and counted
`blind_tunnelled`. The process maps to an app through the catalog's `windows_exe` signals, and
the app maps to a tool key by a fixed table in `capture-core/toolconfig` (task 27):

| App | Tool key |
|---|---|
| `claude_code`, `claude_code_vscode` | `claude_code` |
| `codex` | `codex` |
| `copilot_cli`, `github_copilot` | `copilot` |
| `cursor` | `cursor` |

## 11. Dependencies approved for this plan

| Dependency | Module | For |
|---|---|---|
| `go.opentelemetry.io/proto/otlp` | capture-core | OTLP protobuf types |
| `google.golang.org/protobuf` | capture-core | protobuf and protojson decoding |
| `google.golang.org/grpc` | capture-core | OTLP/gRPC server |
| `go.opentelemetry.io/otel/sdk` and the OTLP log, trace and metric exporters (`otlploghttp`, `otlploggrpc`, `otlptracehttp`, `otlptracegrpc`, `otlpmetrichttp`, `otlpmetricgrpc`) | capture-core, tests only | the "official exporter" checks |
| `github.com/0xrawsec/golang-etw` | capture-core (Windows) | ETW real-time sessions: process start and stop, network connect, DNS client. Pure Go, no cgo |
| `github.com/fsnotify/fsnotify` | capture-core | the config drift watcher |
| `github.com/BurntSushi/toml` | capture-core | the Codex managed config |
| `github.com/go-ole/go-ole` | capture-core (Windows) | WinRT toast notifications from the user-session helper (task 10); the Windows Firewall COM API `INetFwPolicy2` (task 47) |
| `github.com/google/certtostore` | capture-core (Windows) | the non-exportable CNG key for the device CA (task 44) |

Any other dependency needs the owner's approval: stop and ask. Builds stay `CGO_ENABLED=0`
(`device/installer/build.mjs`).

## 12. Platforms

Every collector task is implemented on Windows: built and tested on the PC, then verified in the
device phase (task 60) on the reference VM in `TESTBED.md`: Hyper-V, Entra-joined and
Intune-managed, and enrolled into pre-prod's test tenant. The agent reaches it as CI's signed
release, through Intune, with `tools/testbed/deploy.mjs` (task 59). On macOS and Linux, the same
collector compiles and reports `absent` with detail `tool_version_unsupported` for the tool
configs, or `etw_session_failed` for ETW, until tasks 53–56 port it. Each new package keeps the
existing file-suffix convention
(`_windows.go`, `_darwin.go`, `_linux.go`, `_other.go`).

## 13. Budgets

| Budget | Value | Enforced by |
|---|---|---|
| Discovery records per device per UTC day | 200 (bundle `discovery_daily_budget`) | task 15; measured in task 22 |
| Hook decision, whole `--hook` process, 4 KB prompt, on the reference VM | p99 < 50 ms, hard ceiling 400 ms | task 36; gated in task 51 |
| OTLP receiver | 2,000 log records/s sustained, p99 request handling < 20 ms | task 51 |
| capture-core idle | < 1 % of one core averaged over 10 min, private working set < 150 MB | task 51 (measured on the reference VM in the device phase, not CI) |
| Policy toggle | collector started or stopped within one policy poll | tasks 06, 07, 08 |
| Config drift | reverted within 5 s | task 34 |
