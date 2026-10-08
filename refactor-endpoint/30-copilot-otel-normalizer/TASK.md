# 30. Copilot fixtures and normalizer

Needs, in the device phase (task 60): on the reference VM, installed and signed in as the console user: VS Code with GitHub
Copilot Chat, and the Copilot CLI (`@github/copilot`), with an account that has a Copilot licence.

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

- **Fixtures**, as task 25: a `documented/` set under `testdata/copilot/vscode-documented/` and
  `cli-documented/`, written now from the documentation above, replaced by a capture in the
  device phase (On the device). The capture, no product code, on the reference VM as the
  console user, with `otelcol-contrib` and a file exporter:
  - Copy the collector binary and its config in with `invm.ps1 -CopyTo`, and run it with
    `invm.ps1 -AsUser console -Command` (on `127.0.0.1:4318`, not the agent's ports).
  - **VS Code**: set the Copilot OTel settings in the console user's own `settings.json`, for the
    capture only: the endpoint pointing at the collector, and content capture on. Run a chat turn
    and an agent-mode turn that uses one tool, in VS Code at the VM's console.
  - **CLI**: run, with `invm.ps1 -AsUser console -Command`, one shell that sets the documented
    `COPILOT_OTEL_*` variables and runs one `copilot` prompt with a tool use.
  - Repeat both with content capture off.
  - Remove the temporary user settings afterwards, and copy the collector's output files back
    with `invm.ps1 -CopyFrom`.
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
- `node tools/accept.mjs` passes.

## On the device

On the reference VM, running `main`'s release, with OTel on for the test tenant (ask the owner to check
the Settings page; wait):
0. First the capture in Scope. Where the captured output differs from the `documented/`
   fixtures, fix the normalizer on this task's fix branch; the mapped/dropped table shows
   every difference.
1. Read the agent's token with `invm.ps1 -Command` (from `otlp.token` in the state directory;
   don't print it in the report).
2. Point VS Code Copilot by hand at `http://127.0.0.1:47318` with the token header, in the
   console user's settings. These are temporary user settings; task 31 makes them managed.
3. A Copilot Chat turn in VS Code at the VM's console (an owner step) raises the
   `otel_receiver` row's `emitted` counter, and the spool drains with the batch acknowledged
   (`invm.ps1 -AgentState`). The owner confirms on the dashboard: a `tool.otel` prompt event
   for `app:github_copilot` (Tools page), attributed to the console user. Its `user_ref` is the
   one derived from the console user's UPN, or its `subject_name` is that UPN if the tenant's
   device identity is clear.
4. The same is shown for the CLI, run with `invm.ps1 -AsUser console -Command 'copilot ...'`,
   with `app:copilot_cli`.
5. Remove the temporary settings afterwards.
