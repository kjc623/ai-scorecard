# 16. Installed app scanner

Needs, in the device phase (task 60): on the reference VM, Claude Desktop, ChatGPT Desktop (Microsoft Store / MSIX) and Cursor
installed for the console user.

## Problem

The product can't tell which AI apps are installed on a device. Windows records installed
software in three places, and AI apps use all three:
- the machine and per-user `Uninstall` registry keys;
- AppX/MSIX packages;
- per-user installers. Claude Desktop installs under `%LOCALAPPDATA%` with a per-user uninstall
  key.

## Goal

A new `inventory` provider scans installed applications on a schedule, matches them against the
catalog (task 14), and emits `app_installed` discovery records through the emitter (task 15). An
admin can turn it on and off from the dashboard (task 07) without a restart.

## Scope

- **`device/capture-core/inventory`**, a `core.Provider`:
  - collector `inventory_scanner` (add the `protocol.Collector` constant and the `ref.collector`
    row, component `capture_core`, modes `m0`–`m3`,
    in `schema.sql` and the next free numbered migration);
  - emits on route `inv.scan`;
  - `core.Toggled` on `endpoint.inventory.enabled` (§5);
  - scans at start, then every `endpoint.inventory.interval_minutes`. An interval change takes
    effect at the next tick, through `ApplyPolicy`;
  - each scan is one pass of the provider's scanner list. This task adds the installed-app
    scanner; tasks 17, 18 and 20 add theirs to the same list.
- **Windows installed-app scanner** (`inventory/apps_windows.go`):
  - `HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall` and its `WOW6432Node` twin give
    machine-wide installs (`user_ref` `unattributed`).
  - `HKU\<SID>\Software\Microsoft\Windows\CurrentVersion\Uninstall` gives per-user installs, for
    every loaded user hive with the same SID prefixes `winproxy` uses (`signedInUsers`). Each
    record carries that user's person, resolved through the existing `hostinfo` and `person`
    logic.
  - AppX/MSIX packages: read the machine package repository
    (`HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Appx\AppxAllUserStore`) and each loaded
    user's
    `HKU\<SID>\Software\Classes\Local Settings\Software\Microsoft\Windows\CurrentVersion\AppModel\Repository\Packages`
    for package full names. Verify both against Microsoft's documentation and, read-only, on the
    PC's own registry, and record what was found in `DECISIONS.md`. The device phase confirms
    them on the reference VM (`invm.ps1 -Command` for HKLM; HKU of the console user's SID for
    the per-user store).
  - Match the uninstall `DisplayName` against `windows_uninstall_name`, the uninstall
    `DisplayIcon`/`InstallLocation` executable against `windows_exe`, the package family name
    against `windows_appx`, and the uninstall `Publisher` against `publisher`, all through the
    `policy.Bundle` helpers (task 14).
  - Version comes from `DisplayVersion`, or the package full name's version segment.
  - Basis `installed_scan`, type `app_installed`.
  - Read the registry only (`golang.org/x/sys/windows/registry`). Never start a discovered
    program.
- **Other platforms** (`apps_other.go`): no scanner. The provider reports `absent` with detail
  `tool_version_unsupported` on macOS and Linux until task 53 or 55 ports it (§12).
- **Health**: `healthy` after a complete scan. A scan that couldn't read a hive or key is
  `degraded` with detail `enumeration_partial` (existing), and counts `errors`.
- Wire the provider into `buildProviders` (`cmd/capture-core/service.go`). Task 06 starts it
  through `start_collectors`.
- **Tests**: a registry seam (an interface over the three reads) with fixtures for:
  - a machine install;
  - a per-user Squirrel install (Claude Desktop shape);
  - an MSIX package (ChatGPT shape);
  - a non-AI app (not emitted).

## Done when

- `cd device/capture-core && go test -race ./inventory/` passes.
- `node tools/accept.mjs` passes.

## On the device

On the reference VM, running `main`'s release:
1. Within one scan, `app:claude_desktop`, `app:chatgpt_desktop` and `app:cursor` are emitted as
   `discovery` / `app_installed` with versions, and with the console user's `user_ref` for the
   per-user installs.
   - Show the agent's spool record lines (task 05):
     `invm.ps1 -Command 'Select-String "envelope spooled" C:\ProgramData\ShadowAICapture\state\capture-core.log | Select -Last 50'`.
   - `invm.ps1 -AgentState` shows the batch acknowledged by pre-prod.
   - The owner confirms on the dashboard's Tools page that the three apps are listed for the
     test tenant. If no dashboard page shows discovery records, they say so, and the device-side
     evidence stands. A discovery view is not in this plan.
   - Show the versions the VM itself reports beside them:
     `invm.ps1 -AsUser console -Command 'Get-AppxPackage *ChatGPT*; Get-ItemProperty HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\* | Select DisplayName,DisplayVersion'`.
2. Ask the owner to switch "Inventory" off for the test tenant; wait. After the next policy poll,
   without a restart, the `inventory_scanner` health row is `absent` with
   `disabled_by_policy`. Show that the service's start time didn't change:
   `invm.ps1 -Command 'Get-Process capture-core | Select Id,StartTime'`, before and after.
   Read the row with `invm.ps1 -AgentState`.
3. Ask the owner to switch it back on, and it returns `healthy`. This is the plan's E03 finish
   line.
