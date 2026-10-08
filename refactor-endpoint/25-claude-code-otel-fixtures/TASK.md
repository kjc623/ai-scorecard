# 25. Claude Code telemetry fixtures

Needs, in the device phase (task 60): on the reference VM, Claude Code installed and signed in for the console user, able to run
a short headless session (`claude -p`) that reads a file and runs one shell command. The agent
may copy test-only helpers (`otelcol-contrib`) into `C:\ProgramData\SacTestbed\` with
`invm.ps1 -CopyTo`; the product itself is only ever installed through `deploy.mjs`.

## Problem

The normalizer (task 26) has to match what Claude Code actually emits, not what its documentation
says it emits:
- event names;
- attribute names;
- resource attributes;
- how prompt text appears when prompt logging is on.

Vendors change these between releases, so the normalizer is built and tested against captured
output.

## Goal

For the build: a fixture set written from Claude Code's current monitoring documentation, so the
normalizer (task 26) can be built and tested now. In the device phase: real Claude Code OTel
output, captured with prompt logging on, replaces it, with the version recorded. Both cover the
prompt, tool result, API request and API error events.

## Scope

- **No product code.**
- **For the build**: `device/capture-core/otlp/testdata/claude-code/documented/` holds
  `logs-prompts-on.json`, `logs-prompts-off.json` and `metrics.json`, written by hand from the
  current monitoring documentation
  (https://docs.anthropic.com/en/docs/claude-code/monitoring-usage): every documented event,
  with every documented attribute key and a value of the documented type, in the OTLP JSON
  shapes below. Its `README.md` names the page, the date and the Claude Code version the page
  describes. These fixtures carry the placeholders below from the start.
- **In the device phase** (On the device), capture with the official collector, not with
  capture-core:
  1. In the VM, run `otelcol-contrib` (the release from task 23, copied in with `invm.ps1 -CopyTo`)
     as the console user, with:
     - an `otlp` receiver on `127.0.0.1:4318` (HTTP);
     - a `file` exporter writing OTLP JSON (`format: json`) under
       `C:\ProgramData\SacTestbed\capture\`, one file per signal.
  2. In the same `invm.ps1 -AsUser console -Command` script, set the Claude Code telemetry
     variables for that process only (from the current monitoring documentation,
     https://docs.anthropic.com/en/docs/claude-code/monitoring-usage):
     - `CLAUDE_CODE_ENABLE_TELEMETRY=1`;
     - `OTEL_LOGS_EXPORTER=otlp`;
     - `OTEL_METRICS_EXPORTER=otlp`;
     - `OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf`;
     - `OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318`;
     - `OTEL_LOG_USER_PROMPTS=1`;
     - short export intervals.

     These variables are set for this capture only. Task 27 writes the managed configuration.
  3. Run headless sessions with `claude -p` as the console user that between them:
     - send a prompt;
     - read a file;
     - run a shell command, one tool use allowed (`--allowedTools`) and one denied;
     - trigger one API error (for example `--model` with an invalid model name).
  4. Run a second session with `OTEL_LOG_USER_PROMPTS=0`, to capture the redacted prompt event.
  5. Copy the exporter's files to the PC with `invm.ps1 -CopyFrom`. Then delete
     `C:\ProgramData\SacTestbed\capture\` and the collector from the VM.
- **Fixtures**:
  - `device/capture-core/otlp/testdata/claude-code/<claude-code-version>/` holds
    `logs-prompts-on.json`, `logs-prompts-off.json` and `metrics.json`, as OTLP JSON
    `ExportLogsServiceRequest` / `ExportMetricsServiceRequest` bodies. One request per file;
    merge the exporter's lines. The captured set replaces `documented/`.
  - Before committing, replace every piece of real content with stable placeholders:
    - prompt text with the canary `SAC-CANARY-7f3a prompt text`;
    - file paths, command lines and user, email or account ids with
      `SAC-PLACEHOLDER-<n>`.

    Keep every attribute key and the value types. Add a `README.md` in the version folder
    listing each event name found, its attribute keys, and which values were replaced.
- **`DECISIONS.md`**: for the build, the documentation version the fixtures follow; in the
  device phase, the Claude Code version, OS build, the date, the event names observed, and any
  difference from the documentation (for example a renamed attribute, or traces emitted or not).
  A difference is a fix to task 26's normalizer, on its fix branch.

## Done when

- For the build: the `documented/` fixture files and their README are in the repository. A
  reviewer can find in them a `claude_code.user_prompt` (or the documented name), a tool result,
  an API request and an API error event.
- `node tools/accept.mjs` passes.

## On the device

On the reference VM, with Claude Code installed and signed in for the console user:
- Run the capture in Scope (steps 1–5) and save the `<claude-code-version>/` fixtures in place
  of `documented/`.
- A reviewer can find in them a `claude_code.user_prompt` (or the name actually observed), a
  tool result, an API request and an API error event.
- `grep` over the fixtures finds no real user name, path, email or prompt text from the session.
  The report shows the check.
- Task 26's tests pass over the captured fixtures, or its normalizer is fixed on a fix branch.
