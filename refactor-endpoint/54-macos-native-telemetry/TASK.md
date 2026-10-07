# 54. macOS: native telemetry and hooks

Needs: the Mac from task 53, with Claude Code, Codex CLI and Cursor installed and signed in, and
VS Code with GitHub Copilot and a Copilot licence.

## Problem

The tool config writers (tasks 27, 29, 31, 37, 38) and the drift watcher (task 34) write each
tool's machine-wide managed configuration at its Windows location. On macOS they report `absent`
with `tool_version_unsupported`. macOS is where the plan expects stronger enforcement: Codex's
`/etc/codex/managed_config.toml` overrides user config there, unlike on Windows.

## Goal

On macOS, every tool writer configures its tool at the tool's macOS admin-managed location.
Claude Code's, Codex's and Copilot's prompts arrive as normalized events, hooks block in Claude
Code and Cursor, and drift is reverted within 5 s, as on Windows.

## Scope

- **Verify each location** on the installed versions and record it in `DECISIONS.md`:
  - Claude Code `/Library/Application Support/ClaudeCode/managed-settings.json`;
  - Codex `/etc/codex/managed_config.toml`;
  - Copilot's macOS managed settings (VS Code policy via a configuration profile, or the
    enterprise-managed export; whichever task 31 found, translated to macOS);
  - Cursor's enterprise `hooks.json`.
- **`capture-core/toolconfig`**: a `_darwin.go` location table per writer, with the same merge,
  backup and restore rules (`DESIGN.md` §8).
  - Files are root-owned, with mode 0644 for files the tool reads as the user.
  - Where macOS honours only a configuration profile (an MDM-only location), the writer reports
    `absent`/`tool_version_unsupported`, and the location is recorded as needing E38 (an MDM
    profile). Don't write a user-level file as a workaround.
- **Hook command**: `/usr/local/opt/shadow-ai-capture/bin/capture-core --hook <tool> <event>`
  over the Unix socket, through `localipc`.
- **Drift watcher**: fsnotify works on macOS (kqueue). Confirm the 5 s revert.
- **Uninstall**: add a macOS uninstall script to `device/installer/macos/` that runs
  `capture-core --uninstall-cleanup` and then removes the package's files. None exists today
  (backlog 27 notes it). Add a macOS variant of task 50's snapshot comparison (a shell script),
  and prove a clean return.

## Done when

- `cd device/capture-core && go test ./toolconfig/ ./hooks/` passes on the Mac.
- Ready to merge. After merge and deploy (`AGENTS.md`), with `main`'s `agent-release-macos`
  (task 53) installed on the Mac and enrolled in the test tenant, and the needed switches on for the
  test tenant (ask the owner to set them on the Settings page; wait):
  - a Claude Code prompt and a Codex prompt each appear as normalized `tool.otel` (or merged
    `tool.hook`) events: the agent log's `envelope spooled` lines on the Mac, then the owner's
    confirmation on the dashboard;
  - Copilot's do too, or are recorded as needing E38;
  - an AWS-key-shaped test string is blocked in Claude Code, and in Cursor if task 38 found it
    honoured;
  - a hand edit of the Claude Code managed settings is reverted within 5 s;
  - the uninstall snapshot comparison shows no difference.
  Paste the output for each.
- `node tools/accept.mjs` passes on Windows.
