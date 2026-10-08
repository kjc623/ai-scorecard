# Codex OTLP fixtures: documented, not captured

These files are written by hand from Codex's OpenTelemetry exporter source. No Codex process
produced them. A capture from a real Codex install replaces this folder with one named after the
captured version.

- Source: the `openai/codex` repository on GitHub, read 2026-10-08: the `codex-otel` crate
  (`codex-rs/otel`: `README.md`, `src/events/session_telemetry.rs`, `src/events/shared.rs`,
  `src/tool_result.rs`, `src/skill_invocation.rs`, `src/agent_response.rs`,
  `src/guardian_assessment.rs`, `src/provider.rs`, `src/targets.rs`), the `[otel]` settings
  (`codex-rs/config/src/types.rs`), and where the service name comes from
  (`codex-rs/core/src/otel_init.rs`, `codex-rs/login/src/auth/default_client.rs`,
  `codex-rs/exec/src/lib.rs`, `codex-rs/app-server/src/lib.rs`). Tag `rust-v0.162.0`
  (`1f3f934`); `main` at `99aa053` (2026-10-08T22:42Z) has the same event code.
  https://developers.openai.com/codex (the security and advanced configuration pages) was not
  reachable from the build machine.
- Codex version: 0.162.0, the npm `latest` of `@openai/codex` on 2026-10-08, which the fixtures
  carry as `service.version` and `app.version`.

## Files

Each file is one OTLP JSON request body, as Codex's OTLP/HTTP log exporter with
`protocol = "json"` sends it.

| File | Request | Assumed settings |
| - | - | - |
| `logs-prompts-on.json` | `ExportLogsServiceRequest` | `[otel]` `exporter = { otlp-http = { ... } }`, `log_user_prompt = true`, `log_agent_responses = true`, `log_guardian_assessments = true`, signed in with ChatGPT |
| `logs-prompts-off.json` | `ExportLogsServiceRequest` | the same exporter with the defaults (`log_user_prompt = false`), signed in with an API key |

`logs-prompts-on.json` holds every log event the `codex-otel` crate exports, with every key it
can set. `logs-prompts-off.json` holds the events of one short session (prompt, a failed and a
retried request, a completed response, an allowed and a denied shell command, a second completed
response) with only the keys sent in that case.

Not in the fixtures: metrics and traces (Codex exports those through `otel.metrics_exporter` and
`otel.trace_exporter`, and the normalizer reads logs only); log events other crates send under a
`codex_otel` target (network proxy audit, file upload recovery, models endpoint, exec server,
agent communication, HTTP client fallbacks, app server shutdown); and the crate's own diagnostic
lines, which carry a message and no `event.name`.

## Shapes

- Resource: `service.name`, `service.version`, `env` (`otel.environment`, default `dev`),
  `host.name` (logs only), and the SDK's `telemetry.sdk.language` (`rust`),
  `telemetry.sdk.name` (`opentelemetry`), `telemetry.sdk.version` (`0.31.0`).
- `service.name` is the process's originator: `codex_exec` for `codex exec` (the fixtures),
  `codex_cli_rs` for the interactive CLI, and `codex-app-server` for the app server that Codex
  Desktop and the IDE extensions run.
- Scope: `codex_otel.log_only`, the tracing target, which the OTLP exporter uses as the scope
  name.
- Log record: `observedTimeUnixNano`, `severityNumber` INFO, `severityText` `INFO`, no body.
  The tracing bridge sets no record time, only the observed time. The event name is the
  `event.name` attribute (`codex.<name>`), with `event.timestamp` (RFC 3339, milliseconds); there
  is no event sequence. The bridge also sets the OTLP `eventName` to the tracing callsite name
  (`event <file>:<line>`, or `codex.skill_invocation`), which varies by build; the fixtures leave
  it out, and the normalizer reads only `event.name`.
- Value types: fields Codex formats with Display are strings, which includes `duration_ms`,
  `prompt_length`, `success` on `tool_result`, `input_token_count`, `output_token_count` and
  `tool_token_count`. Booleans are `boolValue`, other integers `intValue`.
- A redacted `prompt` is `[REDACTED]`. `prompt_length` counts characters, not bytes.

## Standard attributes

On every record except `codex.skill_invocation`: `event.timestamp`, `conversation.id`,
`app.version`, `auth_mode`, `originator`, `user.account_id`, `user.email`, `terminal.type`,
`model`, `slug`. `codex.skill_invocation` has `event.timestamp`, `conversation.id`, `turn.id`,
`user.id`, `user.account_id`, `model`, `app.version`, `originator`, `auth_mode`. In
`logs-prompts-off.json` (API-key sign-in) `user.account_id` and `user.email` are absent.

## Events

Attribute keys besides the standard ones and `event.name`. "on" and "off" name the file.

| Event | Keys | In |
| - | - | - |
| `codex.conversation_starts` | `provider_name`, `auth.env_openai_api_key_present`, `auth.env_codex_api_key_present`, `auth.env_codex_api_key_enabled`, `auth.env_provider_key_name`, `auth.env_provider_key_present`, `auth.env_refresh_token_url_override_present`, `reasoning_effort`, `reasoning_summary`, `context_window`, `auto_compact_token_limit`, `approval_policy`, `sandbox_policy`, `mcp_servers` | on; off without the optional keys |
| `codex.user_prompt` | `prompt_length`, `prompt` | on (the canary), off (`[REDACTED]`) |
| `codex.api_request` | `duration_ms`, `http.response.status_code`, `error.message`, `attempt`, `auth.header_attached`, `auth.header_name`, `auth.retry_after_unauthorized`, `auth.recovery_mode`, `auth.recovery_phase`, `endpoint`, the `auth.env_*` keys, `auth.request_id`, `auth.cf_ray`, `auth.error`, `auth.error_code`, `auth.agent_id`, `auth.task_id` | on (200, and 400 invalid model); off (500, then 200 on attempt 2) |
| `codex.websocket_connect` | as `api_request` without `attempt`, plus `success`, `auth.connection_reused` | on |
| `codex.websocket_request` | `duration_ms`, `success`, `error.message`, the `auth.env_*` keys, `auth.connection_reused`, `auth.agent_id`, `auth.task_id` | on (failed) |
| `codex.auth_recovery` | `auth.mode`, `auth.step`, `auth.outcome`, `auth.request_id`, `auth.cf_ray`, `auth.error`, `auth.error_code`, `auth.recovery_reason`, `auth.state_changed` | on |
| `codex.sse_event` | `event.kind`, `duration_ms`, `error.message`; on `response.completed`: `input_token_count`, `output_token_count`, `cached_token_count`, `cache_write_token_count`, `reasoning_token_count`, `tool_token_count`, `ttft_ms`, `service_tier`, `model_reasoning_effort` | on (`response.created`, `response.output_item.done`, `response.completed` in the main and a subagent conversation, `response.failed` and the failed `response.completed`); off (two `response.completed`) |
| `codex.tool_decision` | `tool_name`, `tool_namespace`, `call_id`, `decision`, `source` | on (`approved`, `denied`, `denied_with_network_policy_deny`); off (`approved`, `denied`) |
| `codex.tool_result` | `tool_result_seq`, `tool_name`, `tool_namespace`, `call_id`, `product_sku`, `duration_ms`, `success`, `output_truncated`, `agent_name`, `arguments`, `output`, `mcp_server`, `mcp_server_origin` | on (allowed shell, two denied shells, failed MCP tool); off (allowed and denied shell, without `product_sku`) |
| `codex.sandbox_outcome` | `tool_name`, `call_id`, `outcome`, `initial_duration_ms`, `escalated_duration_ms` | on |
| `codex.startup_phase` | `startup.phase`, `startup.status`, `duration_ms` | on |
| `codex.turn_ttft` | `duration_ms` | on |
| `codex.turn_cost` | `turn.id`, `usage.estimated_usd`, `turn.interrupted`, `speed`, `reasoning_effort` | on |
| `codex.plugin_install_elicitation_sent` | `plugin_install.tool_type`, `plugin_install.tool_id`, `plugin_install.tool_name` | on |
| `codex.plugin_install_suggestion` | the three above, `plugin_install.response_action`, `plugin_install.user_confirmed`, `plugin_install.completed` | on |
| `codex.skill_invocation` | `skill.name`, `skill.scope`, `skill.plugin_id`, `skill.invocation_type` (with its own standard keys) | on |
| `codex.agent_response` | `agent.type`, `turn.id`, `item.id`, `parent.conversation.id`, `parent.turn.id`, `root.turn.id`, `initiating.agent.path`, `response`, `response_length`, `response_truncated` | on |
| `codex.guardian_assessment` | `review.id`, `turn.id`, `item.id`, `status`, `outcome`, `risk_level`, `user_authorization`, `started_at_ms`, `completed_at_ms`, `rationale`, `rationale_length`, `rationale_truncated` | on |

A denied call emits its `codex.tool_decision` and then a failed `codex.tool_result`; both are in
the fixtures for each denied call.

## Placeholders

Prompt text is the canary `SAC-CANARY-7f3a prompt text`. Other content and identifiers are
`SAC-PLACEHOLDER-<n>`:

| n | Replaces |
| - | - |
| 1 | `user.email` |
| 2 | `user.account_id` |
| 3 | `user.id` |
| 4 | `host.name` |
| 5 | the allowed shell command's `arguments` |
| 6 | the shell command the user denied (`arguments`) |
| 7 | the shell command the network policy denied (`arguments`) |
| 8 | the allowed command's `output` |
| 9 | a denied command's `output` |
| 10 | the MCP server name (`mcp_servers`, `mcp_server`, the tool namespace) |
| 11 | the MCP tool's `arguments` |
| 12 | the MCP tool's `output` |
| 13 | the guardian `rationale` |
| 14 | the skill name |
| 15 | `error.message` on `codex.websocket_connect` |
| 16 | `error.message` on `codex.websocket_request` |
| 17 | `error.message` on a failed `codex.api_request` |
| 18 | `error.message` on the failed stream (`codex.sse_event`) |
| 19 | the agent `response` |

Conversation, turn, call, review, request, agent and task identifiers are fixed made-up values.
