# 17. CLI and package scanner

Needs, in the device phase (task 60): on the reference VM, these installed for the console user: `claude` (Claude Code,
native installer or npm), `codex` (npm), `gemini` (npm, `@google/gemini-cli`) and `copilot`
(npm, `@github/copilot`).

## Problem

AI coding agents ship as command-line tools, installed in places the installed-app scanner (task
16) doesn't look:
- npm global packages;
- native installers under the user profile;
- pipx;
- directories on a user's PATH.

## Goal

The inventory provider also finds catalog CLIs for every user profile on the device, with their
versions, and emits `cli_installed` discovery records. It does this without running any
discovered program.

## Scope

- **`device/capture-core/inventory/cli_windows.go`**, a scanner added to the inventory provider's
  list (task 16). For each user profile under `C:\Users` that has a loaded hive or a
  `NTUSER.DAT`, it checks four locations:
  - **npm global**: `%APPDATA%\npm\node_modules\<pkg>\package.json`, which gives name and
    `version`. Match `npm_package` signals. Also check the npm prefix from the user's `.npmrc`
    `prefix=` line when present.
  - **Native installers**: the Claude Code native install. Verify its Windows location and where
    the installed version can be read without executing it (a versions directory or a metadata
    file), and record this in `DECISIONS.md`. Match `cli_binary` and `windows_exe`.
  - **pipx**: `%USERPROFILE%\.local\pipx\venvs\<pkg>`, with the version from the venv's
    `site-packages\<pkg>-<version>.dist-info`. Match `pipx_package`.
  - **The user's PATH**: from `HKU\<SID>\Environment\Path` plus the machine PATH. A file whose
    base name (without `.exe`, `.cmd` or `.ps1`) equals a `cli_binary` value is a match. The
    version comes from the PE file version resource (`GetFileVersionInfo` through
    `x/sys/windows` / `version.dll`) for an `.exe`; otherwise it is empty.

  Each match is one `cli_installed` record with basis `package_scan`, `app_key` from the matching
  signal, and the profile owner's person.
- **Security rule**: never execute, load or `--version` a discovered file. The service runs as
  SYSTEM, and the files are user-writable. Read files and metadata only, and cap each
  `package.json` read at 1 MiB.
- Record each location's outcome on the provider's counters: `observed` per file checked,
  `errors` per unreadable location.
- **Tests**: fixture directory trees for:
  - npm global (`claude`, `codex`, `gemini`, `copilot`);
  - a native Claude Code install;
  - a pipx venv;
  - a PATH hit with a PE version resource (use the test binary's own resource, or a checked-in
    tiny signed-free PE fixture);
  - a non-AI package (not emitted).

## Done when

- `cd device/capture-core && go test -race ./inventory/` passes.
- `node tools/accept.mjs` passes.

## On the device

On the reference VM, running `main`'s release:
- `app:claude_code`, `app:codex`, `app:gemini_cli` and `app:copilot_cli` arrive as
  `discovery` / `cli_installed`, each with the console user's `user_ref` and the version the
  tool reports for itself. Show them from the spool log (the agent's `envelope spooled` lines, task 05: `invm.ps1 -Command 'Select-String "envelope spooled" C:\ProgramData\ShadowAICapture\state\capture-core.log | Select -Last 50'`), with the batch acknowledged in `invm.ps1 -AgentState`.
- Compare against
  `invm.ps1 -AsUser console -Command 'claude --version; codex --version; gemini --version; copilot --version'`,
  run in the console user's session (the agent never runs them), and show both side by side.
