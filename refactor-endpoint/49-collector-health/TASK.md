# 49. Per-tool health in the cloud

## Problem

Each tool config writer (`tool_config_claude_code`, `tool_config_codex`, `tool_config_copilot`,
`tool_config_cursor`) reports a health row. But "the tool's config is in place" is not the same
as "the tool is sending what we expect":
- a tool version too old for the settings we write still looks healthy;
- a user who broke the config between drift checks looks healthy.

The dashboard's device view shows coverage counts, but not which per-tool collectors are
degraded and why.

## Goal

Every tool collector reports one of three things:
- `healthy` (config in place and the tool's events seen recently);
- `degraded` with a cause (`config_tampered`, `config_write_failed`, `tool_version_unsupported`,
  or `no_recent_events`);
- `absent` (`tool_not_installed` or `disabled_by_policy`).

The dashboard's device view lists every collector row with its state and cause. A hand-broken
tool config shows as degraded there.

## Scope

- **Device** (`capture-core/toolconfig`):
  - Each writer knows the minimum tool version its settings need. Record it per tool in
    `DECISIONS.md` from tasks 27, 29, 31 and 38.
  - Each writer reads the installed version from the inventory provider's last scan (task 16),
    through a read-only `inventory.Installed(appKey) (version string, ok bool)` added to that
    package.
  - Below the minimum: `degraded`/`tool_version_unsupported`, and the writer doesn't write.
  - A tool whose native collector is on but whose OTel or hook events haven't been seen for 24 h,
    while the tool's process was seen running (task 19), is `degraded`/`no_recent_events`. Add the
    detail to the vocabulary and `check-vocab`.
- **Server**: `ops.collector_state` already stores the state and detail. Confirm the new details
  pass control-api's validation, which uses the shared vocabulary.
- **Dashboard** (`services/dashboard/src`):
  - The device detail view gains a "Collectors" table: one row per collector reported by that
    device, with state, cause (human wording for each detail, kept in `vocab.js`) and last report
    time.
  - `disabled_by_policy` rows are shown as "Off in policy", not as a fault.
  - The data comes from query-api: add a closed query for a device's collector rows to
    `services/query-api/src/registry.js` / `blocks.js`. It is not person-resolving, so it needs no
    audit entry. Confirm this against query-api's rules and say so.
- Tests: device tests for the version and no-recent-events states, plus query-api and dashboard
  tests for the new table.

## Done when

- The device, query-api and dashboard tests pass.
- Ready to merge. After merge and deploy (`AGENTS.md`), with Claude Code installed and its OTel on
  for the test tenant (ask the owner to check the Settings page; wait):
  1. `invm.ps1 -AgentState` shows `tool_config_claude_code` healthy, and the owner confirms the new
     "Collectors" table on the VM's device view shows it healthy, with every other collector row
     the VM reports.
  2. In one `invm.ps1 -Command` script: stop the `ShadowAICapture` service, remove the OTel `env`
     block from `managed-settings.json`, and start the service again. Stopping first keeps the
     drift watcher (task 34) from reverting the edit before the service sees it.
  3. The first health report after the restart shows the row `degraded` with `config_tampered`,
     before the revert (`invm.ps1 -AgentState`). Ask the owner to confirm that the device view's
     Collectors table shows the same row and cause; wait.
  4. Then let it revert, and confirm the file with `invm.ps1 -Command 'Get-Content <managed-settings path>'`.
- `node tools/accept.mjs` passes.
