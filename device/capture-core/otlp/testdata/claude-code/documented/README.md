# Claude Code OTLP fixtures: documented, not captured

These files are written by hand from Claude Code's monitoring documentation. No Claude Code
process produced them. A capture from a real Claude Code install replaces this folder with one
named after the captured version.

- Source: https://code.claude.com/docs/en/monitoring-usage (the Markdown form,
  `monitoring-usage.md`), page `dateModified` 2026-10-08T19:00:28Z, read 2026-10-08.
  `docs.anthropic.com/en/docs/claude-code/monitoring-usage` was not reachable from the build
  machine; it is the same page under its earlier host.
- Claude Code version: the page describes the current release and names features up to v2.1.287.
  The npm `latest` of `@anthropic-ai/claude-code` on 2026-10-08 was 2.1.295, which the fixtures
  carry as `service.version` and `app.version`.

## Files

Each file is one OTLP JSON request body, as an OTLP/HTTP exporter with `http/json` sends it.

| File | Request | Assumed settings |
| - | - | - |
| `logs-prompts-on.json` | `ExportLogsServiceRequest` | `OTEL_LOG_USER_PROMPTS=1`, `OTEL_LOG_TOOL_DETAILS=1`, `OTEL_LOG_RAW_API_BODIES=1`, `OTEL_LOG_MANAGED_SETTINGS=1`, every `OTEL_METRICS_INCLUDE_*` on |
| `logs-prompts-off.json` | `ExportLogsServiceRequest` | defaults (prompts, responses and tool details redacted), with `OTEL_LOG_RAW_API_BODIES=file:<dir>` |
| `metrics.json` | `ExportMetricsServiceRequest` | every `OTEL_METRICS_INCLUDE_*` on |

`logs-prompts-on.json` holds every documented event and every documented attribute key.
`logs-prompts-off.json` holds the events of one short session (prompt, file read, denied shell
command, API request and responses, API error) with only the keys sent by default.

## Shapes

- Resource: `service.name` (`claude-code`), `service.version`, `os.type` (`windows`),
  `os.version`, `host.arch`. `metrics.json` has a second resource for a session under WSL
  (`os.type` `linux`, plus `wsl.version`).
- Scope: `com.anthropic.claude_code`, the documented meter name. The page names no logger, so the
  logs use the same name.
- Log record: `body` is the documented event name (`claude_code.<name>`), and the attributes hold
  `event.name` (`<name>`), `event.timestamp` (ISO 8601) and `event.sequence`. The page does not
  say where the record carries the event name; the OTLP `eventName` field is left unset.
- Metrics: monotonic delta sums (`aggregationTemporality` 1, the documented default) of
  `asDouble` points, with the documented units (`USD`, `tokens`, `s`, none for counts).
- Value types: integers are `intValue` and fractions `doubleValue`. Values the page quotes
  (`"true"`, `"false"`) are strings; values it gives unquoted or calls Boolean are `boolValue`;
  `managed_settings.sources` and `workspace.host_paths` are string arrays. `status_code` is an
  integer on `api_error` and `api_retries_exhausted` and a string on `auth`, as documented.
  `wsl.version` is a string (the page says "number" without a type).
- A redacted `prompt`, `prompt_text` or `response` is `<REDACTED>`. The page shows that value for
  `response` and for the span's prompt; for `prompt` it says only "redacted".

## Standard attributes

On every record in `logs-prompts-on.json` and every data point in `metrics.json`:
`session.id`, `ccr.session.id`, `app.version`, `app.entrypoint`, `organization.id`,
`user.account_uuid`, `user.account_id`, `user.id`, `user.email`, `terminal.type`,
`vcs.repository.url.full`, `vcs.owner.name`, `vcs.repository.name`, `vcs.provider.name`.
Log records add `prompt.id` and `workspace.host_paths`; `workflow.run_id` and `workflow.name`
are on one `api_request` and one `tool_result`.

In `logs-prompts-off.json`: `session.id`, `organization.id`, `user.account_uuid`,
`user.account_id`, `user.id`, `user.email`, `terminal.type`, `prompt.id`.

Not in the fixtures: keys from `OTEL_RESOURCE_ATTRIBUTES` (custom, no fixed names) and the
gateway-only `user.groups` and `identity.source`.

## Events

Attribute keys besides the standard ones and `event.name`, `event.timestamp`, `event.sequence`.
"on" and "off" name the file; a key listed for "on" only is absent from "off".

| Event | Keys | In |
| - | - | - |
| `claude_code.user_prompt` | `prompt_length`, `prompt`, `prompt_text`, `message.uuid`, `command_name`, `command_source` | on (two records: a prompt and a command); off: `prompt_length`, `prompt`, `prompt_text`, `message.uuid` |
| `claude_code.assistant_response` | `response_length`, `response`, `model`, `request_id`, `message.uuid`, `query_source` | on, off |
| `claude_code.tool_result` | `tool_name`, `tool_use_id`, `success`, `duration_ms`, `error_type`, `error`, `decision_type`, `decision_source`, `tool_input_size_bytes`, `tool_result_size_bytes`, `mcp_server_scope`, `vcs.ref.head.revision`, `vcs.ref.head.name`, `vcs.ref.head.type`, `tool_parameters`, `tool_input` | on (Read, Bash `git commit`, failed Bash, MCP); off: Read without `tool_parameters`, `tool_input` |
| `claude_code.api_request` | `model`, `cost_usd`, `cost_usd_micros`, `duration_ms`, `input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_creation_tokens`, `request_id`, `client_request_id`, `speed`, `query_source`, `effort`, `agent.name`, `skill.name`, `plugin.name`, `marketplace.name`, `mcp_server.name`, `mcp_tool.name` | on (main and subagent); off: main, without the attribution keys |
| `claude_code.api_error` | `model`, `error`, `status_code`, `duration_ms`, `attempt`, `request_id`, `client_request_id`, `speed`, `query_source`, `effort`, attribution keys as `api_request` | on (invalid model 404; timeout without `status_code` or `request_id`); off: the 404 without attribution keys |
| `claude_code.api_refusal` | `model`, `request_id`, `query_source`, `speed`, `attempt`, `effort`, `server_fallback_hop`, `has_category`, `has_explanation`, `category`, attribution keys | on |
| `claude_code.api_request_body` | `body`, `body_ref`, `body_length`, `body_truncated`, `model`, `query_source`, `request_body_id` | on: inline `body`, truncated; off: `body_ref` |
| `claude_code.api_response_body` | `body`, `body_ref`, `body_length`, `model`, `query_source`, `request_id`, `request_body_id`, `message.id`, `message.uuid` | on: inline `body`; off: `body_ref` |
| `claude_code.tool_decision` | `tool_name`, `tool_use_id`, `decision`, `tool_source`, `source`, `tool_parameters` | on (Read accept, Bash reject, Bash accept, MCP accept); off: Read accept, Bash reject, without `tool_parameters` |
| `claude_code.permission_mode_changed` | `from_mode`, `to_mode`, `trigger` | on |
| `claude_code.auth` | `action`, `success`, `auth_method`, `error_category`, `status_code` | on |
| `claude_code.mcp_server_connection` | `status`, `transport_type`, `server_scope`, `duration_ms`, `error_code`, `is_plugin`, `plugin_id_hash`, `plugin.name`, `server_name`, `error` | on |
| `claude_code.internal_error` | `error_name`, `error_code` | on |
| `claude_code.plugin_installed` | `marketplace.is_official`, `install.trigger`, `plugin.name`, `plugin.version`, `marketplace.name` | on |
| `claude_code.plugin_loaded` | `plugin.name`, `marketplace.name`, `plugin.version`, `plugin.scope`, `enabled_via`, `plugin_id_hash`, `has_hooks`, `has_mcp`, `host_owned_mcp`, `skill_path_count`, `command_path_count`, `agent_path_count`, `safe_mode` | on |
| `claude_code.skill_activated` | `skill.name`, `invocation_trigger`, `skill.source`, `skill.kind`, `plugin.name`, `marketplace.name` | on |
| `claude_code.at_mention` | `mention_type`, `success` | on |
| `claude_code.api_retries_exhausted` | `model`, `error`, `status_code`, `total_attempts`, `total_retry_duration_ms`, `speed` | on |
| `claude_code.hook_registered` | `hook_event`, `hook_type`, `hook_source`, `safe_mode`, `hook_matcher`, `plugin.name`, `plugin_id_hash` | on |
| `claude_code.hook_execution_start` | `hook_event`, `hook_name`, `num_hooks`, `managed_only`, `hook_source`, `safe_mode`, `hook_definitions` | on |
| `claude_code.hook_execution_complete` | `hook_event`, `hook_name`, `num_hooks`, `num_success`, `num_blocking`, `num_non_blocking_error`, `num_cancelled`, `total_duration_ms`, `stdout_chars`, `additional_context_chars`, `system_message_chars`, `initial_user_message_chars`, `num_outputs_persisted`, `managed_only`, `hook_source`, `safe_mode`, `hook_definitions` | on |
| `claude_code.hook_plugin_metrics` | `plugin_id`, `hook_event`, plus plugin-chosen metric keys (here `findings`, integer, and `blocked`, Boolean) | on |
| `claude_code.compaction` | `trigger`, `success`, `duration_ms`, `pre_tokens`, `post_tokens`, `error`, `precompute_reuse` | on |
| `claude_code.subagent_completed` | `agent_type`, `agent.source`, `is_built_in`, `is_async`, `total_tokens`, `total_tool_uses`, `duration_ms`, `model`, `final_model`, `model_swapped`, `plugin_id_hash`, `plugin.name` | on |
| `claude_code.feedback_survey` | `event_type`, `appearance_id`, `survey_type`, `response`, `enabled_via_override` | on |
| `claude_code.retention_sweep` | `result`, `period_days`, `used_default`, `skip_reason`, `transcripts_deleted`, `transcripts_exempted_desktop`, `session_files_deleted`, `artifacts_deleted`, `files_retained_fresh`, `files_past_cutoff`, `error_count` | on (a complete and a skipped sweep) |
| `claude_code.managed_settings_resolved` | `managed_settings.trigger`, `error.type`, `managed_settings.sources`, `managed_settings.source_behavior`, `managed_settings.helper.state`, `managed_settings.helper.applied`, `managed_settings.helper.entry`, `managed_settings.helper.path`, `managed_settings.resolved_sha256`, `managed_settings.settings`, `managed_settings.settings_truncated` | on (a startup and a refused record) |
| `claude_code.system_prompt` | none documented: the page says the event carries the complete system prompt (detailed beta tracing with prompt logging) but names no attribute for it | on, standard keys only |

## Metrics

Data point keys besides the standard ones.

| Metric | Unit | Keys |
| - | - | - |
| `claude_code.session.count` | | `start_type` |
| `claude_code.lines_of_code.count` | | `type`, `model` |
| `claude_code.pull_request.count` | | |
| `claude_code.commit.count` | | |
| `claude_code.cost.usage` | `USD` | `model`, `query_source`, `speed`, `effort`, `agent.name`, `skill.name`, `plugin.name`, `marketplace.name`, `mcp_server.name`, `mcp_tool.name` |
| `claude_code.token.usage` | `tokens` | `type`, and the keys of `claude_code.cost.usage` |
| `claude_code.code_edit_tool.decision` | | `tool_name`, `decision`, `source`, `language` |
| `claude_code.active_time.total` | `s` | `type` |

## Placeholders

Prompt text is the canary `SAC-CANARY-7f3a prompt text` (in `prompt`, `prompt_text` and the
inline request body). Other content and identifiers are `SAC-PLACEHOLDER-<n>`:

| n | Replaces |
| - | - |
| 1 | `user.email` |
| 2 | `user.account_uuid` |
| 3 | `user.account_id` |
| 4 | `user.id` |
| 5 | `organization.id` |
| 6 | the file path the Read tool read (`tool_input`) |
| 7 | the allowed shell command line (`git commit`), in `tool_parameters` and `tool_input` |
| 8 | the denied shell command line, in `tool_parameters` |
| 9 | the `workspace.host_paths` directory |
| 10 | `vcs.repository.url.full` |
| 11 | `vcs.owner.name` |
| 12 | `vcs.repository.name` |
| 13 | the request body file (`body_ref` on `api_request_body`) |
| 14 | the response body file (`body_ref` on `api_response_body`) |
| 15 | `managed_settings.helper.path` |
| 16 | the failed shell command line |
| 17 | the user-configured MCP server name (`mcp_server_name`, `server_name`) |
| 18 | the system prompt in the inline request body |
| 19 | the assistant response text (`response`, and the inline response body) |
| 20 | the MCP tool's input (`tool_input`) |
| 21 | the hook command in `hook_definitions` |

Session, prompt, message, request and tool-use identifiers are fixed made-up values.
