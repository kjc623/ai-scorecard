# 28. Codex fixtures and normalizer

Needs, in the device phase (task 60): on the reference VM, the Codex CLI installed and signed in for the console user (and Codex
Desktop, if a Windows build is available). The agent may copy test-only helpers (`otelcol-contrib`)
into `C:\ProgramData\SacTestbed\` with `invm.ps1 -CopyTo`.

## Problem

Codex CLI and Codex Desktop export OTel events (`codex.user_prompt` and others, per
https://developers.openai.com/codex/security) when configured in their `[otel]` config section.
The receiver has no Codex normalizer, and the event and attribute names haven't been checked
against a real install.

## Goal

Real Codex OTel output is captured as fixtures, and a Codex normalizer maps it onto envelopes with
the same guarantees as the Claude Code normalizer (task 26).

## Scope

- **Fixtures**, as task 25: a `documented/` set written now from Codex's current documentation,
  replaced by a capture in the device phase (On the device). The capture, no product code:
  1. In the VM, use `otelcol-contrib` with a `file` exporter, run as the console user, as in
     task 25.
  2. Configure Codex for the capture session only, in the console user's
     `%USERPROFILE%\.codex\config.toml` (written with `invm.ps1 -AsUser console`):
     - an `[otel]` section with the OTLP HTTP exporter pointing at the collector;
     - `log_user_prompt = true`;
     - the documented exporter keys, verified against the current documentation.

     Restore the file afterwards.
  3. Run one headless session (`codex exec`, as the console user) with a prompt, a tool call
     (one allowed, one denied) and an API error.
  4. Run a second session with `log_user_prompt = false`.
  - Copy the exporter's files to the PC with `invm.ps1 -CopyFrom`, then remove the capture files
    and the collector from the VM.
  5. Save the captured fixtures to `device/capture-core/otlp/testdata/codex/<codex-version>/`,
     replacing the `documented/` set, with the
     same placeholder rules and README as task 25. If Codex Desktop exists on Windows and its
     events differ, add a `desktop-<version>/` folder.
  6. Record the version, the config keys used and the events observed in `DECISIONS.md`.
- **`device/capture-core/otlp/codex`**, a `Normalizer`:
  - `Accepts`: Codex's `service.name` values, as observed (CLI and Desktop may differ);
  - `tool_fingerprint` `app:codex`;
  - the prompt event goes to `Pipeline.Process` on route `tool.otel`, like task 26;
  - tool decisions and results go to `agent_activity` / `tool_call`;
  - API requests and responses go to `agent_activity` / `model_request`, with tokens and model
    when present;
  - the same mapped/dropped attribute test as task 26.
- Register it in the OTLP provider.

## Done when

- `cd device/capture-core && go test -race ./otlp/...` passes, with every fixture event converted
  and no unlisted attribute.
- `node tools/accept.mjs` passes.

## On the device

On the reference VM, running `main`'s release:
- First the capture in Scope (steps 1–6). Where the captured events differ from the
  `documented/` fixtures, fix the normalizer on this task's fix branch; the test's
  mapped/dropped table shows every difference.
- Point a Codex session, run as the console user, by hand at `http://127.0.0.1:47318` with the
  token header (read with `invm.ps1 -Command 'Get-Content C:\ProgramData\ShadowAICapture\state\otlp.token'`, never printed in the report). Use the same temporary user config as the capture; task 29
  makes it managed.
- It gives a `tool.otel` prompt event for `app:codex`, with the `user_ref` derived from the
  console user's UPN, shown from the spool log (task 05's `envelope spooled` lines, read with `invm.ps1 -Command`), with the batch acknowledged in `invm.ps1 -AgentState`.
- Remove the temporary config afterwards.
