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
- pre-prod: its service logs, and anything the dashboard can find.

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
- **On the reference VM**, in the device phase: a script (`tools/testbed/privacy-canary.mjs`,
  written then, because it builds on task 59's tooling, and run on the PC) that,
  against the test tenant set to `m1` (ask the owner to set it on the Settings page, with every
  endpoint collector, hooks, TLS inspection and Ollama capture on; wait for one policy poll):
  1. Prints a fresh canary, and starts capturing `fly logs` for every pre-prod app
     (`TESTBED.md`, service logs).
  2. Sends it where it can be scripted, through `tools/testbed/invm.ps1 -AsUser console`:
     - `claude -p` (hooks and OTel);
     - `curl.exe` to an intercepted API host (proxy);
     - `curl.exe` to Ollama on 11434 (loopback).
  3. Waits while the owner pastes it (`AGENTS.md`, console steps), at the VM's console as the console user,
     into Cursor, Claude Desktop and ChatGPT in the managed browser, capturing each with
     `invm.ps1 -Screenshot`.
  4. Searches for the canary in:
     - pre-prod's service logs, read-only: the captures started in step 1, for every app
       including the edge and the jobs, stopped once the device's spool has drained, searching
       for the canary;
     - the VM's agent log (`C:\ProgramData\ShadowAICapture\state\capture-core.log`) and
       `health.json`, copied back with `invm.ps1 -CopyFrom` and deleted from the PC after the
       search.
  5. Prints the hit count per location.
  6. Asks the owner to search the test tenant's dashboard Search page for the canary string, and
     records their answer (expected: no results).

  Pre-prod's database has no agent access. What reaches it at `m1` is exactly the envelopes,
  which the automated test searches, plus the server logs searched above.
- Any hit is a defect. Report it with its path, but don't fix it in this task unless the fix is
  confined to removing the leak. Otherwise stop and report it.

## Done when

- `cd device/integration && go test -race -run Privacy ./...` passes, with zero hits.
- `node tools/accept.mjs` passes.

## On the device

On the reference VM, running `main`'s release: write `tools/testbed/privacy-canary.mjs` as
Scope describes and run it. It reports zero hits for a canary sent through every tool listed,
and the owner reports no Search results. Paste its output.
