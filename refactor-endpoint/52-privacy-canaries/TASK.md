# 52. Privacy canaries

## Problem

Prompt text now enters the agent through five paths:
- hooks;
- OpenTelemetry;
- the TLS proxy;
- the browser extension;
- the loopback broker.

The product's promise depends on where that text may go:
- at `m0`, nothing is read;
- at `m1`, only labels and a digest leave the device;
- prompt text never appears in a log, a health report or an error.

Each path was tested on its own. Nothing checks every path at once against every place text could
leak.

## Goal

A canary prompt, sent through every collector at `m0` and at `m1`, is found nowhere it must not
be:
- envelopes as uploaded;
- the spool;
- logs;
- `health.json`;
- health reports;
- the content store;
- the lab database.

## Scope

- **Canary**: a unique string per run (`SACCANARY-<uuid>`), embedded in an otherwise normal
  prompt. An AWS-key-shaped string travels beside it in the same prompt, so classification has
  something to find.
- **Automated test** (`device/integration/privacy_test.go`): in-process, with fakes for the tools,
  against a real service rig. Reuse the `fakeCloud` pattern from `cmd/capture-core/cloud_test.go`.
  If it can't be imported across modules, copy only what is needed into the integration module's
  test support.
  - For each mode (`m0`, `m1`), send the canary through:
    - the hook relay (a `test` adapter frame);
    - the OTLP receiver (an OTLP/HTTP log record with a `user_prompt`-shaped body);
    - the TLS proxy (an intercepted request to a fake upstream);
    - the native endpoint (an extension observation frame);
    - the loopback broker (a request to a fake local runtime).
  - Then search for the canary, case-sensitively and also base64- and JSON-escaped, in:
    - every envelope `fakeCloud` received;
    - the decrypted spool contents (through the spool's own reader);
    - every log line the service wrote;
    - `health.json`;
    - every health request body;
    - the content store directory.

    Expect zero hits at `m0` and at `m1`.
  - At `m1`, also assert that each prompt event carries labels including `credential`, and a
    digest.
- **On the reference host**: a script (`localdev/tools/privacy-canary.mjs`) that, against the lab
  tenant set to `m1`:
  1. Prints a fresh canary.
  2. Waits while the owner pastes it into Claude Code, Cursor, Claude Desktop and ChatGPT in the
     browser.
  3. Searches the lab database (all `ingest` and `ops` tables, as the lab's superuser through the
     lab's database tools, never the owner's tenant), the agent's log
     (`C:\ProgramData\ShadowAICapture\state\capture-core.log`) and `health.json`.
  4. Prints the hit count per location.
- Any hit is a defect. Report it with its path, but don't fix it in this task unless the fix is
  confined to removing the leak. Otherwise stop and report it.

## Done when

- `cd device/integration && go test -race -run Privacy ./...` passes, with zero hits.
- The reference-host script reports zero hits for a canary sent through every tool listed. Paste
  its output.
- `node tools/accept.mjs` passes.
