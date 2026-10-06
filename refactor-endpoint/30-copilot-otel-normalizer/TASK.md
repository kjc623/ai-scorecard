# 30. Copilot fixtures and normalizer

Needs: on the reference host, VS Code with GitHub Copilot Chat, and the Copilot CLI
(`@github/copilot`), both signed in with an account that has a Copilot licence.

## Problem

GitHub Copilot in VS Code and the Copilot CLI can export OTel, including prompt content when
content capture is on. VS Code uses the `github.copilot.chat.otel.*` settings; the CLI uses
`COPILOT_OTEL_*` variables
(https://code.visualstudio.com/docs/agents/guides/monitoring-agents). Their output may follow the
OpenTelemetry GenAI semantic conventions (spans) rather than log events. The receiver has no
Copilot normalizer, and the shapes haven't been checked.

## Goal

Real Copilot OTel output from VS Code and the CLI is captured as fixtures, and a Copilot
normalizer maps it onto envelopes with the same guarantees as task 26.

## Scope

- **Capture** (no product code), with `otelcol-contrib` and a file exporter:
  - **VS Code**: set the Copilot OTel settings in the owner's user `settings.json`, for the
    capture only: the endpoint pointing at the collector, and content capture on. Run a chat
    turn and an agent-mode turn that uses one tool.
  - **CLI**: set the documented `COPILOT_OTEL_*` variables in one shell, and run one prompt with
    a tool use.
  - Repeat both with content capture off.
  - Save the fixtures under `device/capture-core/otlp/testdata/copilot/vscode-<version>/` and
    `cli-<version>/`, with task 25's placeholder rules and README. Capture traces as well as
    logs if Copilot emits spans.
  - Record the extension and CLI versions, the setting and variable names, and the signal types
    observed in `DECISIONS.md`.
- **`device/capture-core/otlp/copilot`**, a `Normalizer`:
  - `Accepts`: the observed `service.name` values for VS Code Copilot and the CLI;
  - `tool_fingerprint`: `app:github_copilot` for VS Code and `app:copilot_cli` for the CLI;
  - the user prompt (a log event, or the `gen_ai.input.messages` / prompt attribute on the chat
    span) goes to `Pipeline.Process` on route `tool.otel`. Only the latest user message is the
    prompt; earlier conversation turns are not re-reported;
  - chat and inference spans go to `agent_activity` / `model_request`, with `gen_ai.request.model`
    and the token usage attributes;
  - tool execution spans go to `agent_activity` / `tool_call`;
  - the same mapped/dropped attribute test as task 26, applied to both logs and span attributes.
- Register it in the OTLP provider.

## Done when

- `cd device/capture-core && go test -race ./otlp/...` passes over all Copilot fixtures.
- On the reference host, a Copilot Chat turn in VS Code, pointed by hand at
  `http://127.0.0.1:47318` with the token header (temporary user settings; task 31 makes it
  managed), gives a `tool.otel` prompt event for `app:github_copilot`, attributed to the owner.
  The same is shown for the CLI with `app:copilot_cli`.
- `node tools/accept.mjs` passes.
