# 37. Claude Code hooks

Needs: Claude Code installed on the reference VM and signed in as the console user.

## Problem

The hook relay (task 36) can decide, but Claude Code doesn't call it. Claude Code runs hooks
declared in its settings. Its machine-wide `managed-settings.json` overrides user and project
settings, and `allowManagedHooksOnly` disables every hook not declared there. Task 27 already
writes that file for OpenTelemetry.

## Goal

When hooks are on for Claude Code (the Settings page; the bundle's `endpoint.hooks.enabled` and
`endpoint.tools.claude_code.hooks`), the agent declares its hook in Claude Code's managed
settings. A prompt containing a test secret is then blocked before it is sent, with the rule's
message. `allowManagedHooksOnly` is set only when the bundle's `endpoint.hooks.managed_only` is
true.

## Scope

- **Verify first**, against the current Claude Code docs (`https://code.claude.com/docs/en/hooks`)
  and the installed version, and record what you find in `DECISIONS.md` with the version:
  - the Windows path of `managed-settings.json`;
  - the hook declaration shape (`hooks.UserPromptSubmit[].hooks[].type/command/timeout`);
  - the stdin JSON for `UserPromptSubmit` and `PreToolUse`;
  - the block output: exit code 2 with a reason on stderr, or JSON `{"decision":"block","reason":...}`.
    Use whichever the current version documents for a user-visible reason;
  - the `PreToolUse` deny output;
  - the name and effect of `allowManagedHooksOnly`.
- **Adapter** (`capture-core/hooks/claudecode.go`), as in task 36:
  - `UserPromptSubmit` sends `prompt` as `prompt_text`, plus `session_id` and `cwd`.
  - `PreToolUse` sends `tool_input` serialised as compact JSON as `prompt_text`, with `tool_name`.
  - `Render`:
    - `block` maps to the verified block output, with the rule's message and link (link on its own
      line);
    - `warn` allows and shows the message, using the verified non-blocking user-visible field
      (for example `systemMessage`);
    - `allow` produces the empty allow output.
- **Config writer** (`capture-core/toolconfig`, the Claude Code writer from task 27): merge the
  `hooks` block and, when asked, `allowManagedHooksOnly` into `managed-settings.json`.
  - The rules of `DESIGN.md` §8 apply: back up the original once, touch only the agent's own keys,
    leave every other key as found, and remove exactly what was added when disabled.
  - The hook entries:
    - `UserPromptSubmit`: command `"<install dir>\bin\capture-core.exe" --hook claude_code UserPromptSubmit`;
    - `PreToolUse`: matcher `Bash|WebFetch|mcp__.*`, command `... --hook claude_code PreToolUse`;
    - timeout 1 s on both (Claude Code's unit, verified).
  - The install directory comes from the running executable's path, never from configuration.
  - Re-applied on every policy change (enable, disable, managed-only on or off) without a restart.
    The drift watcher (task 34) covers these keys once it exists.
- **Health**: the `tool_config_claude_code` row (task 27) reports `degraded`/`config_write_failed`
  when the hooks can't be written.
- **Tests**:
  - the adapter, against stdin fixtures captured from the installed version (saved under
    `capture-core/hooks/testdata/claude-code/<version>/`);
  - the merge, against a managed-settings file that holds an unrelated key and a customer hook:
    both survive enable and disable, and `allowManagedHooksOnly` appears only with `managed_only`.

## Done when

- `cd device/capture-core && go test -race ./hooks/ ./toolconfig/` passes.
- On the reference VM, deployed with `node localdev/testbed/deploy.mjs`, with a lab-tenant rule
  "block `credential`" (tenant at `m1` or higher):
  1. `invm.ps1 -Command 'Get-Content <managed-settings path>'` shows the agent's hook entries.
  2. In a new interactive `claude` session at the VM's console, a prompt containing an
     AWS-key-shaped test string is blocked, and Claude Code shows the rule's message. Capture it
     with `invm.ps1 -Screenshot`.
  3. A prompt without one goes through.
  4. As a non-interactive cross-check, run
     `invm.ps1 -AsUser console -Command 'claude -p "<prompt with the test string>"'` and show
     that it is refused, with the rule's message in the output.
  5. The blocked prompt's event shows `source = tool.hook` and `policy_decision.action = blocked`
     on the dashboard, attributed to the console user's UPN-derived `user_ref`.
- Turning "only managed hooks" on and then off in the dashboard adds and removes
  `allowManagedHooksOnly` within one policy poll. Show the file before and after, read with
  `invm.ps1 -Command`.
- `node tools/accept.mjs` passes.
