# 18. IDE extension scanner

Needs, in the device phase (task 60): on the reference VM, VS Code with the GitHub Copilot (and Copilot Chat), Claude Code and
Continue extensions installed for the console user. Cursor (installed for task 16) also has at
least one AI extension.

## Problem

AI assistants also live inside IDEs as extensions, which neither the installed-app scanner (task
16) nor the CLI scanner (task 17) sees.

## Goal

The inventory provider lists catalog IDE extensions in VS Code, Cursor, Windsurf and JetBrains IDEs
for every user profile, with their versions and the IDE they belong to. It emits them as
`ide_extension` discovery records.

## Scope

- **`device/capture-core/inventory/ide.go`** (cross-platform parsing) plus `ide_windows.go`
  (locations), a scanner added to the inventory provider's list.
- For each user profile under `C:\Users`:

  | IDE (`host_app`) | Where | How |
  |---|---|---|
  | `app:vscode` | `%USERPROFILE%\.vscode\extensions\extensions.json` | `identifier.id`, `version` |
  | `app:cursor` | `%USERPROFILE%\.cursor\extensions\extensions.json` | as VS Code |
  | `app:windsurf` | `%USERPROFILE%\.windsurf\extensions\extensions.json` | as VS Code |
  | `app:jetbrains` | `%APPDATA%\JetBrains\<Product><Version>\plugins\<plugin>\lib\*.jar` → `META-INF/plugin.xml` | `<id>`, `<version>` |

  - Verify each location against the IDE's documentation and on the PC where the IDE is
    installed, and record the versions checked in `DECISIONS.md`; the device phase confirms them
    on the reference VM. If
    an `extensions.json` is absent, fall back to the folder names `<publisher>.<name>-<version>`.
  - Read JetBrains `plugin.xml` from inside the jar with `archive/zip`, with a 1 MiB cap per
    entry.
  - Match the extension id, case-insensitively, against `ide_extension_id`. Emit `ide_extension`
    with basis `extension_scan`, `app_key` from the match, `host_app` from the table, the version,
    and the profile owner's person.
- Never load or run an extension. Read files only, and cap each `extensions.json` at 4 MiB.
- **Tests**: fixture trees for each IDE, including the folder-name fallback and a JetBrains jar
  built in the test.

## Done when

- `cd device/capture-core && go test -race ./inventory/` passes.
- `node tools/accept.mjs` passes.

## On the device

On the reference VM, running `main`'s release:
- `app:github_copilot`, `app:claude_code_vscode` and `app:continue` arrive as
  `discovery` / `ide_extension`, with `host_app` `app:vscode` and the console user's `user_ref`.
  Show them from the spool log (the agent's `envelope spooled` lines, task 05: `invm.ps1 -Command 'Select-String "envelope spooled" C:\ProgramData\ShadowAICapture\state\capture-core.log | Select -Last 50'`), with the batch acknowledged in `invm.ps1 -AgentState`.
- Their versions match `invm.ps1 -AsUser console -Command 'code --list-extensions --show-versions'`.
  Show both side by side.
