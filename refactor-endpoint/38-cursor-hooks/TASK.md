# 38. Cursor hooks

Needs: Cursor installed on the reference VM and signed in as the console user.

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
  - whether a block from each event stops the action in practice. Test it before writing any
    agent code, with a hand-written script hook placed in the VM with `invm.ps1 -CopyTo` and
    `invm.ps1 -Command`, and Cursor used at the VM's console (`invm.ps1 -Screenshot`).
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
  - Collector `tool_config_cursor`, with a `ref.collector` row in `services/database/schema.sql`
    and in the next numbered migration in `services/database/migrations/`.
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
- Ready to merge. After merge and deploy (`AGENTS.md`), with a test-tenant rule "block
  `credential`" (ask the owner to add it on the Settings page; wait for one policy poll):
  1. `invm.ps1 -Command 'Get-Content <enterprise hooks.json path>'` shows the agent's entries
     beside any customer entries.
  2. In Cursor's chat at the VM's console (an owner step), a prompt containing an AWS-key-shaped test string
     either is blocked with the rule's message, or (if verification found blocks not honoured)
     goes through and is recorded as `logged` with the rule id. Capture Cursor with
     `invm.ps1 -Screenshot`.
  3. The `hook_relay` row's `emitted` counter rises and the spool drains (`invm.ps1 -AgentState`).
     The owner confirms either outcome on the dashboard as a `tool.hook` event for `app:cursor`,
     attributed to the console user.
- `DECISIONS.md` has the verified file path, formats and block behaviour for both events, with
  the Cursor version.
- `node tools/accept.mjs` passes.
