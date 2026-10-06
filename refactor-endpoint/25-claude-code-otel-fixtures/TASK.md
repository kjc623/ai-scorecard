# 25. Claude Code telemetry fixtures

Needs: on the reference host, Claude Code installed and signed in for the owner's account, able
to run a short session that reads a file and runs one shell command.

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

Real Claude Code OTel output, captured with prompt logging on, is saved in the repository as
fixtures. It covers the prompt, tool result, API request and API error events, with the version
recorded.

## Scope

- **No product code.** Capture with the official collector, not with capture-core:
  1. Run `otelcol-contrib` (the release from task 23) with an `otlp` receiver on
     `127.0.0.1:4318` (HTTP) and a `file` exporter writing OTLP JSON (`format: json`), one file
     per signal.
  2. In one PowerShell session as the owner, set the Claude Code telemetry variables (from the
     current monitoring documentation, https://docs.anthropic.com/en/docs/claude-code/monitoring-usage):
     - `CLAUDE_CODE_ENABLE_TELEMETRY=1`;
     - `OTEL_LOGS_EXPORTER=otlp`;
     - `OTEL_METRICS_EXPORTER=otlp`;
     - `OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf`;
     - `OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318`;
     - `OTEL_LOG_USER_PROMPTS=1`;
     - short export intervals.

     These variables are set for this capture only. Task 27 writes the managed configuration.
  3. Run one session that:
     - sends a prompt;
     - reads a file;
     - runs a shell command (one tool use allowed, one denied);
     - triggers one API error (for example by setting an invalid model with `/model` and
       sending a prompt).
  4. Run a second session with `OTEL_LOG_USER_PROMPTS=0`, to capture the redacted prompt event.
- **Fixtures**:
  - `device/capture-core/otlp/testdata/claude-code/<claude-code-version>/` holds
    `logs-prompts-on.json`, `logs-prompts-off.json` and `metrics.json`, as OTLP JSON
    `ExportLogsServiceRequest` / `ExportMetricsServiceRequest` bodies. One request per file;
    merge the exporter's lines.
  - Before committing, replace every piece of real content with stable placeholders:
    - prompt text with the canary `SAC-CANARY-7f3a prompt text`;
    - file paths, command lines and user, email or account ids with
      `SAC-PLACEHOLDER-<n>`.

    Keep every attribute key and the value types. Add a `README.md` in the version folder
    listing each event name found, its attribute keys, and which values were replaced.
- **`DECISIONS.md`**: Claude Code version, OS build, the date, the event names observed, and any
  difference from the documentation (for example a renamed attribute, or traces emitted or not).

## Done when

- The fixture files and their README are in the repository. A reviewer can find in them a
  `claude_code.user_prompt` (or the name actually observed), a tool result, an API request and an
  API error event.
- `grep` over the fixtures finds no real user name, path, email or prompt text from the session.
  The report shows the check.
- `node tools/accept.mjs` passes.
