# VS Code Copilot OTLP fixtures: documented, not captured

These files are written by hand from the Copilot Chat monitoring documentation. No VS Code process
produced them. A capture from a real VS Code install replaces this folder with one named after the
captured extension version.

- Source: "Monitor agent usage with OpenTelemetry"
  (https://code.visualstudio.com/docs/agents/guides/monitoring-agents), read 2026-10-08 as
  `docs/agents/guides/monitoring-agents.md` in `microsoft/vscode-docs` at commit `a8848427`
  (page `DateApproved` 10/7/2026). code.visualstudio.com was not used from the build machine.
- Where the page gives no shape (value types, where content sits, the log events' attributes), the
  fixtures follow the extension's source: `extensions/copilot` in `microsoft/vscode` at commit
  `f6f19d60` (2026-10-08), whose `package.json` is version 0.70.0. The fixtures carry that as
  `service.version` and the scope version. The extension moved there from
  `microsoft/vscode-copilot-chat`, which is archived.

## Settings assumed

- On: `github.copilot.chat.otel.enabled`, `github.copilot.chat.otel.captureContent` and
  `github.copilot.chat.otel.captureIdentity` true, `exporterType` `otlp-http`.
- Off: the same with `captureContent` and `captureIdentity` false.

## Files

Each file is one OTLP JSON request body, as an OTLP/HTTP exporter sends it.

| File | Request | Settings |
| - | - | - |
| `traces-content-on.json` | `ExportTraceServiceRequest` | on |
| `traces-content-off.json` | `ExportTraceServiceRequest` | off |
| `logs-content-on.json` | `ExportLogsServiceRequest` | on |
| `logs-content-off.json` | `ExportLogsServiceRequest` | off |

No metrics file: the receiver discards metrics before any normalizer sees them.

The traces hold four traces:

1. An agent-mode turn: `invoke_agent GitHub Copilot Chat` (root) with `chat` spans before and
   after its tools, `execute_tool` spans for `readFile`, `runCommand`, a failing MCP tool,
   `replaceString` (an edit) and `readSkill`, an `execute_hook` span for `PreToolUse`, and
   `runSubagent` whose child `invoke_agent Plan` (a subagent) has its own `chat` span.
2. A chat turn in Ask mode: a root `invoke_agent` and one `chat` span whose input holds an earlier
   turn of the conversation.
3. The title request: a root `chat` span with no agent span, which fails.
4. Inline chat: a root `invoke_agent Inline Chat` whose request is in `copilot_chat.user_request`
   only, and its `chat` span.

The logs hold every documented event, in the session of trace 1 where they belong to it; "off"
holds the first five (session start, the first inference event, an agent turn, two tool calls)
without content.

## Shapes

- Resource: `service.name` `copilot-chat`, `service.version`, `session.id`; with identity capture
  on, `process.user.name` and `host.name`. Scope: `copilot-chat` with the extension version.
- Spans: the documented attributes of `invoke_agent`, `chat`, `execute_tool` and `execute_hook`.
  Span kinds: `INTERNAL` for agent, tool and hook spans, `CLIENT` for chat. A failure has
  `error.type` and status `ERROR`. A subagent's `invoke_agent` is the child of the
  `execute_tool runSubagent` span, as the page says.
- Content attributes are JSON strings: `gen_ai.input.messages` and `gen_ai.output.messages` in the
  semantic-convention form (`[{"role":"user","parts":[{"type":"text","content":"..."}]}]`),
  `gen_ai.system_instructions`, `gen_ai.tool.definitions`, `gen_ai.tool.call.arguments`.
  On a top-level `invoke_agent` the input messages are the one message the person sent; on a
  `chat` span they are the conversation sent to the model, without the system prompt.
- From the source, not the page: `copilot_chat.user_request` (the typed request) on the agent and
  chat spans, the `user_message` span event (attribute `content`) on the agent span, the content
  attributes on `chat` spans, and `user.name` on agent spans with identity capture. With content
  capture off the exporter removes every content attribute, including `copilot_chat.hook_input`
  and `copilot_chat.hook_output` and the event's `content`.
- Log records: `body` is the source's string (`GenAI inference: <model>`, `copilot_chat.tool.call:
  <tool>`, ...), the event name is the `event.name` attribute, the OTLP `eventName` field is unset,
  and records inside a span carry its trace and span ids. Attribute keys and types are the
  source's; the page names the events only. `gen_ai.client.inference.operation.details` carries
  the content attributes only with content capture on.
- Value types: integers are `intValue`, fractions `doubleValue`, flags `boolValue`,
  `gen_ai.response.finish_reasons` a string array, `github.copilot.hook.tool_names` a JSON string.

## The device phase must confirm

- That VS Code exports both spans and log events to the OTLP endpoint (the page says "traces,
  metrics, and events"), and that the prompt is on the root `invoke_agent` span.
- Whether `gen_ai.input.messages` on the agent span holds only the latest message, and whether
  `copilot_chat.user_request` and the `user_message` event are sent.
- The service names: background Copilot agent sessions in VS Code send the Copilot runtime's spans
  as `github-copilot`, which the normalizer reads as the CLI.

## Placeholders

Prompt text is the canary `SAC-CANARY-7f3a prompt text` (the agent span's messages and
`copilot_chat.user_request`, the last user message of each chat request, the `user_message`
event). Other content and identifiers are `SAC-PLACEHOLDER-<n>`:

| n | Replaces |
| - | - |
| 1 | `user.name` (the GitHub account) |
| 2 | `process.user.name` |
| 3 | `host.name` |
| 4 | the repository remote URL |
| 5 | the branch name |
| 6 | the GitHub organisation |
| 7 | the file path a tool read or edited |
| 8 | the shell command line |
| 9 | the MCP server name |
| 10 | the assistant's response text |
| 11 | the system prompt |
| 12 | a tool's description in `gen_ai.tool.definitions` |
| 13 | tool arguments and results other than paths and commands |
| 14 | the hook's input and output |
| 15 | the subagent's prompt, written by the model |
| 16 | an earlier turn of the conversation, and the title request's input |
| 17 | `copilot_chat.file.relative_path` |

Session, conversation, request, response and tool-call identifiers and the commit hash are fixed
made-up values.
