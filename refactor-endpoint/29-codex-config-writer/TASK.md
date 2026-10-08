# 29. Codex config writer

Needs, in the device phase (task 60): on the reference VM, the Codex CLI installed and signed in for the console user.

## Problem

Codex reads an administrator-managed config layer. On macOS and Linux that is
`/etc/codex/managed_config.toml`, and it overrides user config. On Windows the machine-wide layer
is documented as overridable by users
(https://coralogix.com/docs/integrations/ai-observability/openai/codex-cli/). The agent has to
write Codex's OTel configuration where it can't be removed, or, where users can override it,
say so and detect it.

## Goal

With OTel on for Codex in the dashboard, Codex sessions on the device export to the local
receiver, with prompt logging following the resolved mode. Switching it off restores the
original. The Windows override gap is a recorded, visible limitation.

## Scope

- **Verify first**:
  - Codex's machine-wide config location and precedence on Windows, in the current Codex release:
    the documented location, any `CODEX_HOME` or `%ProgramData%` path, and whether a user's
    `config.toml` `[otel]` overrides it;
  - the `[otel]` keys (exporter, endpoint, headers, protocol, `log_user_prompt`).

  Record what was checked, with the version, in `DECISIONS.md`. This includes the plan's "Windows
  machine-wide file can be overridden by users", confirmed or corrected.
- **`device/capture-core/toolconfig/codex.go`** + `_windows.go`, using `github.com/BurntSushi/toml`
  (`DESIGN.md` §11):
  - reads and writes the machine-wide file, merging only the `[otel]` keys the agent owns;
  - follows the backup and `Remove` rule from task 27 (§8);
  - `Installed()` comes from task 17's facts (`app:codex`);
  - sets `log_user_prompt` true when the resolved mode for `app:codex` is `m1` or higher.
- **Provider**:
  - collector `tool_config_codex` (constant and `ref.collector` row,
    in `schema.sql` and the next free numbered migration);
  - `core.Toggled` on `endpoint.otel.enabled && endpoint.tools.codex.otel`;
  - health as in task 27.
  - On Windows, when the verified precedence lets a user override the file, check every user
    profile's `config.toml`. If one has an `[otel]` section that disables or redirects export,
    the row is `degraded` with `config_tampered` (the detail task 34 also uses; add it now if it
    doesn't exist). The provider never edits user files.
- **Tests**:
  - merge into a TOML file with unrelated tables;
  - backup and restore;
  - mode switching `log_user_prompt`;
  - user-override detection with a fixture user `config.toml`.

## Done when

- `cd device/capture-core && go test -race ./toolconfig/` passes.
- `node tools/accept.mjs` passes.

## On the device

On the reference VM, running `main`'s release, with Codex OTel on for the test tenant:
1. With the temporary user config from task 28 removed, a new Codex session's prompt
   (`invm.ps1 -AsUser console -Command 'codex exec "..."'`) is emitted as a `tool.otel` prompt
   event for `app:codex`, with the console user's `user_ref`, shown from the spool log (task 05's `envelope spooled` lines, read with `invm.ps1 -Command`), with the batch acknowledged in `invm.ps1 -AgentState`.
   - Show the machine-wide file with `invm.ps1 -Command`, at the verified path.
2. Add a user-level `[otel]` override that disables export, in the console user's
   `%USERPROFILE%\.codex\config.toml` via `invm.ps1 -AsUser console`. Remove it afterwards.
   - if the verified precedence says it wins, the `tool_config_codex` row goes `degraded` /
     `config_tampered` within one health interval;
   - if it doesn't win, the export continues.

   Show whichever happened, and that it matches `DECISIONS.md`.
3. Ask the owner to switch Codex OTel off on the Settings page; wait. The machine file is
   restored from its backup within one policy poll (compare with `invm.ps1 -Command`).
