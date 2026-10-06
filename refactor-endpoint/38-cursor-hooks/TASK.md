# 38. Cursor hooks

Needs: Cursor installed on the reference host and signed in.

## Problem

Cursor has no OpenTelemetry, but it runs hooks from a `hooks.json`, including an enterprise-level
file that administrators manage. `beforeSubmitPrompt` runs before a prompt is sent, and
`beforeMCPExecution` before an MCP tool call. Whether Cursor honours a block from
`beforeSubmitPrompt` has changed between versions, so it must be verified, not assumed.

## Goal

When hooks are on for Cursor (`endpoint.hooks.enabled` and `endpoint.tools.cursor.hooks`), the
agent registers its hook in Cursor's enterprise-level hooks file. Prompts and MCP calls reach the
hook relay. The behaviour the current Cursor version actually supports (block, or monitor only)
works, and is recorded.

## Scope

- **Verify first** (`https://cursor.com/docs/hooks` and the installed version), and record in
  `DECISIONS.md` with the version:
  - the enterprise hooks file's path on Windows, and whether a user-level file can override or
    add to it;
  - the `hooks.json` schema (`version`, `hooks.beforeSubmitPrompt[].command`, and so on);
  - the stdin JSON of `beforeSubmitPrompt` and `beforeMCPExecution` (`conversation_id`, `prompt`,
    `tool_name`, `tool_input` or their current names);
  - the response shape (for example `{"continue": false, "user_message": ...}` or
    `{"permission": "deny", ...}`);
  - whether a block from each event stops the action in practice. Test it with a hand-written
    script before writing any agent code.
- **Adapter** (`capture-core/hooks/cursor.go`), using task 36's interface:
  - `beforeSubmitPrompt` sends `prompt` as `prompt_text`, with the conversation id as
    `session_id`.
  - `beforeMCPExecution` sends `tool_input` as compact JSON, with `tool_name`.
  - `Render` uses the verified response shape. If an event's block isn't honoured, `Render` for
    `block` still returns the block shape, and `DECISIONS.md` records the event as monitor-only
    (the record still says `blocked` only if Cursor stopped the action; otherwise
    `RecordedAction(d, canEnforce=false)`).
  - The adapter carries a per-event `canEnforce` from that finding, and the relay uses it.
- **Config writer** (`capture-core/toolconfig/cursor.go`):
  - Collector `tool_config_cursor`, with a `ref.collector` row.
  - Merge the agent's two hook entries into the enterprise `hooks.json` under `DESIGN.md` §8's
    backup and restore rules.
    - Command: `"<install dir>\bin\capture-core.exe" --hook cursor <event>`.
    - Keep any customer entries.
  - Health:
    - `healthy` when the file holds the entries;
    - `absent`/`tool_not_installed` when Cursor isn't installed (its catalog signals, task 14);
    - `degraded`/`config_write_failed` when the write fails.
  - It is `Toggled` on the two bundle switches and applies without a restart.
- Tests: the adapter against stdin fixtures captured from the installed version
  (`capture-core/hooks/testdata/cursor/<version>/`), and the merge with a customer hook present.

## Done when

- `cd device/capture-core && go test -race ./hooks/ ./toolconfig/` passes.
- On the reference host with a rebuilt lab MSI and a lab-tenant rule "block `credential`":
  1. In Cursor's chat, a prompt containing an AWS-key-shaped test string either is blocked with
     the rule's message, or (if verification found blocks not honoured) goes through and is
     recorded as `logged` with the rule id.
  2. Both outcomes appear on the dashboard as `tool.hook` events for `app:cursor`. Screenshot it.
- `DECISIONS.md` has the verified file path, formats and block behaviour for both events, with
  the Cursor version.
- `node tools/accept.mjs` passes.
