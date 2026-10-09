# 56. Linux: native telemetry and hooks

Needs: the Linux desktop from task 55 (for the build as well as the device phase), with Claude
Code, Codex CLI and Cursor installed and signed in.

## Problem

The tool config writers, the hooks and the drift watcher configure tools at their Windows
locations only. On Linux they report `absent` with `tool_version_unsupported`. Linux's machine-wide
files under `/etc` are admin-only by default, so the plan's "users can't override" holds more
naturally there than on Windows.

## Goal

On Linux, every tool writer configures its tool at the tool's Linux admin-managed location.
Prompts arrive as normalized events, hooks block in Claude Code (and Cursor if honoured), drift is
reverted within 5 s, and uninstall restores the machine.

## Scope

- **Verify each location** on the installed versions and record it in `DECISIONS.md`:
  - Claude Code `/etc/claude-code/managed-settings.json`;
  - Codex `/etc/codex/managed_config.toml`;
  - Copilot (VS Code's Linux policy file, if it supports one; otherwise record it as not
    enforceable);
  - Cursor's enterprise `hooks.json`.
- **`capture-core/toolconfig`**: a `_linux.go` location table per writer, with the same merge,
  backup and restore rules (`DESIGN.md` §8). Files are root-owned, mode 0644.
  - The service unit's `ReadWritePaths` must include each location's directory. Update
    `manifest.mjs` → `generated/linux/shadow-ai-capture.service`.
- **Hook command**: `/opt/shadow-ai-capture/bin/capture-core --hook <tool> <event>` over the Unix
  socket.
- **Drift watcher**: fsnotify (inotify). Confirm the 5 s revert.
- **Uninstall**: `device/installer/linux/uninstall.sh` runs `capture-core --uninstall-cleanup`
  before removing files. Add a Linux variant of task 50's snapshot comparison (a shell script),
  and prove a clean return.

## Done when

- `cd device/capture-core && go test ./toolconfig/ ./hooks/` passes on the Linux desktop and in
  CI's Linux job.
- `node tools/accept.mjs` passes.

## On the device

On the Linux desktop, with `main`'s `agent-release-linux` (task 55) installed and enrolled in the
test tenant, and the needed switches on for the test tenant (ask the owner to set them on the
Settings page; wait):
- a Claude Code prompt and a Codex prompt each appear as normalized events: the agent's
  `envelope spooled` log lines on the desktop, then the owner's confirmation on the dashboard;
- an AWS-key-shaped test string is blocked in Claude Code;
- a hand edit of `/etc/claude-code/managed-settings.json` is reverted within 5 s;
- `uninstall.sh` followed by the snapshot comparison shows no difference.
Paste the output for each.
