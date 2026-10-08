# 31. Copilot config writer

Needs, in the device phase (task 60): as task 30.

## Problem

Copilot's OTel settings live in VS Code settings and in the CLI's environment. Both are per-user
and editable by the user unless an administrator enforces them. VS Code supports
machine-enforced policies on Windows (`HKLM\SOFTWARE\Policies\Microsoft\VSCode`) for settings that
declare a policy. GitHub announced enterprise-managed OTel export for Copilot in July 2026. Which
of these can carry the OTel endpoint, headers and content capture is unverified.

## Goal

With OTel on for Copilot in the dashboard, Copilot in VS Code and the Copilot CLI export to the
local receiver through the strongest machine-wide mechanism each one honours, with content
capture following the resolved mode. Where only user-level configuration works, the limitation
is recorded and visible in health.

## Scope

- **Verify first**, and record the versions in `DECISIONS.md`:
  - whether the `github.copilot.chat.otel.*` settings declare VS Code policies (check the
    extension's `package.json` `contributes.configuration` for `policy` entries), and if so their
    policy names under `HKLM\SOFTWARE\Policies\Microsoft\VSCode`;
  - whether the Copilot CLI reads its OTel settings from a machine-wide file or from machine
    environment variables only;
  - whether GitHub's enterprise-managed export can target a device-local endpoint. It most
    likely targets a cloud collector, which would put prompts off the device before
    classification. If so, record it as not used, and why.
- **`device/capture-core/toolconfig/copilot.go`** + `_windows.go`:
  - **VS Code**: if policies exist, write the policy registry values (endpoint, headers, content
    capture from the resolved mode for `app:github_copilot`, at `m1` or higher), owning only
    those values. Otherwise write nothing machine-wide for VS Code, and report it as below.
  - **CLI**: write machine environment variables (`HKLM\SYSTEM\CurrentControlSet\Control\Session Manager\Environment`,
    with `WM_SETTINGCHANGE` broadcast as the CLI shim does) for the verified `COPILOT_OTEL_*`
    names, with the endpoint, the token header, and content capture from the mode for
    `app:copilot_cli`.
  - The backup and `Remove` rule (§8) applies to each registry value and variable.
  - `Installed()` comes from task 17's and task 18's facts.
- **Provider**:
  - collector `tool_config_copilot` (constant and `ref.collector` row; the row goes in
    `services/database/schema.sql` and in the next numbered migration in
    `services/database/migrations/`);
  - `core.Toggled` on `endpoint.otel.enabled && endpoint.tools.copilot.otel`;
  - **health**:
    - `healthy` when every installed Copilot surface is configured machine-wide;
    - `degraded` with `tool_version_unsupported` when VS Code Copilot is installed but has no
      enforceable policy for these settings. The CLI may still be healthy; the row reports the
      worst state.
- **Tests**:
  - registry and environment writes through a fake seam;
  - backup and restore;
  - mode switching;
  - the degraded case.

## Done when

- `cd device/capture-core && go test -race ./toolconfig/` passes.
- `node tools/accept.mjs` passes.

## On the device

On the reference VM, running `main`'s release, with Copilot OTel on for the test tenant (ask the owner
to check the Settings page; wait), and task 30's temporary user settings removed:
1. `invm.ps1 -Command` shows the machine environment variables (and the VS Code policy values,
   if they exist) the agent wrote.
2. A `copilot` prompt run with `invm.ps1 -AsUser console -Command` (a new process, so it reads
   the machine environment) raises the `otel_receiver` row's `emitted` counter and drains
   (`invm.ps1 -AgentState`). The owner confirms on the dashboard a `tool.otel` event for
   `app:copilot_cli`.
3. VS Code Copilot, used at the VM's console (an owner step), either arrives as
   `app:github_copilot` through policy, or the `tool_config_copilot` row is `degraded` /
   `tool_version_unsupported`, consistent with `DECISIONS.md`. Show it in
   `invm.ps1 -AgentState`, and have the owner confirm the same state on the dashboard's device
   view.
4. Ask the owner to switch Copilot OTel off on the Settings page; wait. Within one policy poll the
   backups are restored, shown with `invm.ps1 -Command`.
