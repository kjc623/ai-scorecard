# 27. Claude Code config writer

Needs: on the reference host, Claude Code installed and signed in for the owner's account.

## Problem

Claude Code exports telemetry only when it is configured to. If that configuration lives in a
user's own settings, the user can switch it off. Claude Code reads a machine-wide managed
settings file whose `env` users can't override. The agent has to write that file:
- merging with whatever the customer already deployed there;
- following policy for prompt logging;
- keeping a backup so uninstall can restore it.

Separately, with TLS inspection on (task 08), Claude Code's API traffic would be decrypted as well
as reported over OTel, recording one prompt twice. `DESIGN.md` §10 says a tool covered by an
enabled native collector is not decrypted.

## Goal

With OTel on for Claude Code in the dashboard, a new Claude Code session on the device exports to
the local receiver, and a user can't switch that off. Prompt logging follows the resolved mode
(§8). Switching it off in the dashboard removes the agent's keys and restores what was there.
With interception on, Claude Code's connections are blind-tunnelled.

## Scope

- **`device/capture-core/toolconfig`** (shared by tasks 29, 31, 37, 38 and 57; create it, or
  extend it if task 57 created it):
  - A `Writer` per tool, with `Installed() bool`, `Apply(desired) error`, `Remove() error` and
    `Path() string`.
  - The backup rule (§8): before the first write, copy the existing file (or record "absent") to
    `toolconfig/<tool>/original` in the state directory, once.
  - `Remove` deletes only the keys the agent owns, and restores any key the agent replaced from
    the backup. A file that was absent before is deleted if nothing else is left in it.
  - Writes are atomic (temp file and rename), keep the file's original ACL, and leave the file
    with an ACL users can read but not write.
- **Claude Code writer** (`toolconfig/claudecode.go` + `_windows.go`):
  - Path: the Windows managed settings location. Verify it in the current documentation
    (https://docs.anthropic.com/en/docs/claude-code/settings, believed to be
    `C:\Program Files\ClaudeCode\managed-settings.json`) and on the install. Record it and the
    version in `DECISIONS.md`.
  - `Installed()` uses the inventory facts: `app:claude_code` found by task 17's locations.
    Reuse the scanner functions; don't re-scan in a second way.
  - It merges into the JSON's `env` object and touches no other key. Keys:
    - `CLAUDE_CODE_ENABLE_TELEMETRY=1`;
    - `OTEL_LOGS_EXPORTER=otlp`;
    - `OTEL_METRICS_EXPORTER=otlp`;
    - `OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf`;
    - `OTEL_EXPORTER_OTLP_ENDPOINT=http://<endpoint.otel.http_listen>`;
    - `OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer <otlp token>`;
    - `OTEL_LOG_USER_PROMPTS` = `1` when the resolved mode for `app:claude_code` is `m1` or
      higher, else `0`.

    Use the names verified in task 25. A customer value for one of these keys is backed up and
    restored on `Remove`.
- **Provider** (`toolconfig` exposes one `core.Provider` per tool):
  - collector `tool_config_claude_code` (new `protocol.Collector` constant and `ref.collector`
    row, component `capture_core`);
  - `core.Toggled` on `endpoint.otel.enabled && endpoint.tools.claude_code.otel`;
  - `Start` applies, `Stop` removes;
  - `ApplyPolicy` re-applies when the mode, address or token changes;
  - **health**:
    - `healthy` when the file on disk holds the desired keys;
    - `absent` with a new detail `tool_not_installed` when Claude Code isn't installed;
    - `degraded` with `config_write_failed` when the write fails;
    - add both details to `device/protocol` and `check-vocab`.
  - Other platforms report `absent` with `tool_version_unsupported` until task 54 or 56.
- **Native-first exclusion** (§10), in `proxy/tlsproxy`:
  - before decrypting a CONNECT, resolve the client process (task 09) and look its image up with
    `Bundle.AppByExe`;
  - if that app's `endpoint.tools.<tool>` has an enabled collector (`otel` or `hooks`, with the
    collector itself enabled), blind-tunnel the connection and count `blind_tunnelled`;
  - the tool key for an app key is a fixed map in `toolconfig` (`DESIGN.md` §10: `claude_code`
    and `claude_code_vscode` → `claude_code`, `codex` → `codex`, `copilot_cli` and `github_copilot` → `copilot`, `cursor` → `cursor`).
- **Tests**:
  - merge into an existing file with unrelated keys and a customer `env` key;
  - backup taken once;
  - `Remove` restores byte-for-byte when nothing else changed;
  - a mode change flips `OTEL_LOG_USER_PROMPTS`;
  - not installed gives `absent`;
  - the proxy exclusion with a fake process resolver.

## Done when

- `cd device/capture-core && go test -race ./toolconfig/ ./proxy/tlsproxy/` passes.
- On the reference host with the lab MSI rebuilt and OTel on for the lab tenant and Claude Code:
  1. The managed settings file contains the agent's `env` keys, and any keys it had before.
  2. A new `claude` session's prompt arrives as a `tool.otel` prompt event, with no environment
     variables set by hand.
  3. Setting `CLAUDE_CODE_ENABLE_TELEMETRY=0` in the owner's user settings
     (`%USERPROFILE%\.claude\settings.json` `env`) and in the shell doesn't stop the export.
     Show the next prompt arriving.
  4. Switching Claude Code's OTel off in the dashboard removes the keys within one policy poll,
     and the file matches its backup.
  5. With TLS inspection on, a Claude Code prompt produces no `proxy.tls` event, and the proxy's
     `blind_tunnelled` counter rises.
- `node tools/accept.mjs` passes.
