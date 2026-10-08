# 26. Claude Code normalizer

## Problem

The OTLP receiver (tasks 23–24) passes Claude Code's log records to a normalizer by `service.name`,
but none exists. Claude Code's prompt, tool-result, API-request and API-error events need mapping
onto the envelope (`DESIGN.md` §3): prompts onto `prompt` and the rest onto `agent_activity`. A
vendor change that adds or renames an attribute must be noticed, not silently lost.

## Goal

A Claude Code normalizer converts every event in the task 25 fixtures to envelopes:
- each prompt goes through the pipeline's mode gate and classifier like any other prompt;
- each attribute is either mapped or explicitly listed as dropped.

## Scope

- **`device/capture-core/otlp/claudecode`**, implementing the `Normalizer` from task 23:
  - `Accepts`: `service.name` is Claude Code's, as observed in the fixtures (expected
    `claude-code`).
  - `tool_fingerprint` is `app:claude_code`. The person is the `Sender`'s (task 24); OTel's own
    `user.*` attributes are never used for identity.
  - **Prompt event** (`claude_code.user_prompt` or the observed name) → `core.Pipeline.Process`
    with:
    - route `tool.otel`;
    - `Kind` prompt;
    - `SizeBytes` from `prompt_length`;
    - `OccurredAt` from the record's timestamp;
    - `Content` a `core.ContentReader` returning the `prompt` attribute's text, or nothing when
      prompt logging is off. The pipeline never calls it at `m0` (§8);
    - `Enforce` from task 12 (route `tool.otel` can't enforce, so it records `logged`);
    - `ClientID` from the session id attribute.
  - **Tool result** → `Pipeline.Record` with:
    - `agent_activity` / `tool_call`;
    - `tool_name`, `duration_ms`;
    - `outcome`: `success`, `error`, or `denied` when the decision attribute says the user or
      a policy rejected it.
  - **API request** → `agent_activity` / `model_request` with `model`, `input_tokens`,
    `output_tokens` and `duration_ms`, outcome `success`.
  - **API error** → `agent_activity` / `model_request` with `model`, `duration_ms`, outcome
    `error`.
  - **`dedup_key` for activity**: as in §3, using the record's own event sequence or timestamp
    attribute.
- **No data loss, made testable**: the test walks every log record in every fixture file. For
  each attribute key it requires one of:
  - the key is in the normalizer's mapped-attribute table;
  - the key is in an explicit `dropped` table with a one-line reason (for example `cost_usd` "no
    envelope field", `user.email` "identity comes from the sender process").

  An unknown key fails the test with its name, so a new Claude Code release with a new attribute
  fails loudly.
- **Metrics** stay discarded (task 23). Say in the report which metric names exist, for the
  owner's information.
- Register the normalizer in `buildProviders`' OTLP provider.
- **Tests**:
  - every fixture event converts;
  - at `m0` the content reader is never called and the prompt envelope has no content fields;
  - at `m1` with the canary text, the envelope carries a digest and labels but no text;
  - the dropped-table test.

## Done when

- `cd device/capture-core && go test -race ./otlp/...` passes.
- `node tools/accept.mjs` passes.

## On the device

On the reference VM, running `main`'s release, with OTel on for the test tenant (the
default; ask the owner to confirm it on the Settings page):
- Run a headless Claude Code session as the console user:
  `invm.ps1 -AsUser console -Command '<set the task 25 variables for this process>; claude -p "..."'`,
  with endpoint `http://127.0.0.1:47318` and the token header (read with `invm.ps1 -Command 'Get-Content C:\ProgramData\ShadowAICapture\state\otlp.token'`, never printed in the report).
- It gives one `prompt` (route `tool.otel`) plus `agent_activity` records. Their `user_ref` is
  the one derived from the console user's UPN. Show them from the spool log (task 05's `envelope spooled` lines, read with `invm.ps1 -Command`), with the batch acknowledged in `invm.ps1 -AgentState`.
- The owner confirms on the dashboard's Search page that the prompt event appears for the
  console user, with tool Claude Code.
- Task 27 makes the configuration automatic.
