# Copilot CLI OTLP fixtures: documented, not captured

These files are written by hand from the Copilot CLI's OpenTelemetry documentation. No Copilot CLI
process produced them. A capture from a real install replaces this folder with one named after the
captured CLI version.

- Source: the "OpenTelemetry monitoring" section of the CLI command reference
  (https://docs.github.com/copilot/reference/copilot-cli-reference/cli-command-reference#opentelemetry-monitoring),
  read 2026-10-08 as `content/copilot/reference/copilot-cli-reference/cli-command-reference.md`
  in `github/docs` at commit `9f651797` (2026-10-08). The `github/copilot-cli` repository holds
  only the changelog; the npm package is a loader for a native binary.
- Copilot CLI version: npm `latest` of `@github/copilot` on 2026-10-08 was 1.0.94, which the
  fixtures carry as `service.version` and the scope version.

## Settings assumed

The CLI reads these variables (not the `COPILOT_OTEL_CAPTURE_CONTENT` of VS Code):

- `COPILOT_OTEL_ENABLED=true` (or `OTEL_EXPORTER_OTLP_ENDPOINT` set, which enables it too);
- `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS`;
- `COPILOT_OTEL_EXPORTER_TYPE` `otlp-http` (the default), `OTEL_EXPORTER_OTLP_PROTOCOL`
  `http/json` (the default);
- content capture: `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT=true` for "on", unset for
  "off".

## Files

| File | Request | Content capture |
| - | - | - |
| `traces-content-on.json` | `ExportTraceServiceRequest` | on |
| `traces-content-off.json` | `ExportTraceServiceRequest` | off |

The page says the CLI exports traces and metrics; it names no log events, so there is no logs
file. No metrics file: the receiver discards metrics before any normalizer sees them.

One trace, one prompt with tool use: `invoke_agent Copilot` (root, with `server.address` and
`server.port`) holding a `chat` span that fails with a rate limit, the `chat` span that asks for a
tool, `execute_tool powershell`, a failing `execute_tool view`, `execute_tool task` whose child
`invoke_agent explore` (a subagent, without `server.*`) has its own `chat` span, and the final
`chat` span. Every documented span event is on the root span.

## Shapes

- Resource: `service.name` `github-copilot` and `service.version`, the documented pair. Scope:
  `github.copilot` (the `COPILOT_OTEL_SOURCE_NAME` default) with the CLI version.
- Spans: every documented attribute of `invoke_agent`, `chat` and `execute_tool`; kinds `INTERNAL`
  for agent and tool spans and `CLIENT` for chat, as documented. A failure has `error.type` and
  status `ERROR`.
- Content attributes are JSON strings in the semantic-convention message form. On the root
  `invoke_agent` the input messages are the conversation so far ending with the message the person
  sent; on a `chat` span they are what that request sent, so the last request of a turn ends with a
  tool result.
- Span events: the documented names with their key attributes; `github.copilot.message` on
  `compaction_complete` only with content capture on.
- Value types: integers are `intValue`, fractions (`github.copilot.cost`, durations in seconds)
  `doubleValue`, flags `boolValue`, `gen_ai.response.finish_reasons` a string array.

## The device phase must confirm

- That the CLI sends spans only (no log events), over `http/json` by default.
- That the root `invoke_agent` span carries the person's message as the last entry of
  `gen_ai.input.messages`, and that a subagent's agent span always has a parent.
- The service name in a CLI session started from VS Code, which forwards its OTel variables.

## Placeholders

Prompt text is the canary `SAC-CANARY-7f3a prompt text`. Other content and identifiers are
`SAC-PLACEHOLDER-<n>`; the numbers are those of the VS Code set, and the ones not listed are not
used here:

| n | Replaces |
| - | - |
| 1 | `enduser.pseudo.id` |
| 7 | the file path a tool read |
| 8 | the shell command line |
| 10 | the assistant's response text |
| 11 | the system prompt |
| 12 | a tool's description in `gen_ai.tool.definitions` |
| 13 | tool output |
| 14 | the hook's error message |
| 15 | the subagent's prompt, written by the model |
| 16 | an earlier turn of the conversation |
| 18 | `github.copilot.skill.path` |
| 19 | the compaction summary (`github.copilot.message`) |

Conversation, response, turn, interaction and tool-call identifiers are fixed made-up values.
