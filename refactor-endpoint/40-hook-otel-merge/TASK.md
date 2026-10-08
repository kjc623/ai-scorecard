# 40. Hook and OTel merge

Needs, in the device phase (task 60): Claude Code installed on the reference VM and signed in as the console user.

## Problem

Claude Code reports one prompt twice:
- the hook relay records it on route `tool.hook` (task 36) with the decision;
- its OpenTelemetry `user_prompt` event reaches the OTLP receiver (tasks 23 and 26) on route
  `tool.otel`, with metadata the hook doesn't have.

Two envelopes for one prompt double-count usage. The server merges only on an exact
`content_digest`, so at `m0` (no digest) nothing merges.

## Goal

One prompt produces one envelope on route `tool.hook`, carrying the hook's decision. A prompt
seen by only one path still produces exactly one envelope.

## Scope

- **Package `device/capture-core/merge`**: a hold-and-match buffer between the two providers and
  `core.Pipeline`.
  - The key:
    - at `m1`+: `(tool fingerprint, session id, sha256 of the prompt text)`;
    - at `m0`: `(tool fingerprint, session id, prompt length in bytes)`, matched only when the two
      `occurred_at` values are within 2 s.
  - A record arriving first is held for up to 10 s.
    - **If its partner arrives:** one observation goes to `Pipeline.Process`, with route
      `tool.hook`, the hook's decision and person, the earlier `occurred_at`, and the content
      reader of whichever side holds the text.
    - **If the hold expires:** the record goes on alone, on its own route.
  - At most 1,000 held records. Past that, the oldest is released unmerged and counts `dropped`
    on neither row. The release is not a loss.
  - Shutdown releases everything held, unmerged.
  - Held prompt text stays in memory only, never written, and is cleared when released.
- **Wiring**:
  - The hooks provider and the OTLP receiver's Claude Code normalizer (task 26) submit prompts to
    `merge` instead of directly to the pipeline. Only prompts merge; `agent_activity` records
    don't.
  - Session ids: the hook's `session_id` and the OTel event's `session.id` attribute. Confirm
    from the documentation and task 25's fixtures that they are the same value, record it in
    `DECISIONS.md`, and confirm it on the installed version in the device phase.
- Tests (`go test -race ./merge/`):
  - hook first, then OTel;
  - OTel first, then hook;
  - hook only and OTel only (released after 10 s on a fake clock);
  - two prompts with the same text in one session, which pair in arrival order;
  - the `m0` key;
  - the 1,000 cap.

## Done when

- `cd device/capture-core && go test -race ./merge/ ./hooks/ ./otlp/` passes.
- `node tools/accept.mjs` passes.

## On the device

On the reference VM, running `main`'s release, with Claude Code's hooks and OTel both on
for the test tenant (ask the owner to check the Settings page; wait):
1. Record `invm.ps1 -AgentState`'s `hook_relay` and `otel_receiver` counters.
2. Send five distinct prompts as the console user, each with
   `invm.ps1 -AsUser console -Command 'claude -p "<prompt n>"'` (each run is its own session).
   Then the owner sends two more in one interactive `claude` session at the VM's console.
3. `invm.ps1 -AgentState` shows `hook_relay` `emitted` up by exactly seven, and the spool
   drained with every batch acknowledged.
4. The agent log's `envelope spooled` lines for the test window (task 05), read with
   `invm.ps1 -Command` from `C:\ProgramData\ShadowAICapture\state\capture-core.log`, show
   exactly seven `kind=prompt` lines for `app:claude_code`, all `source=tool.hook` with a
   `policy_decision.action`, and none with `source=tool.otel`. Paste the filtered lines.
5. The owner confirms on the dashboard (the event list for `app:claude_code`, filtered to the
   VM's device and the time of the test): exactly seven prompt events, each on route `tool.hook`
   with a decision, and none on route `tool.otel`. The `agent_activity` events from the same
   sessions may appear; they are not prompts.
