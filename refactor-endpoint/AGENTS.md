# Endpoint refactor: instructions for agents

Each numbered folder is one task; its `TASK.md` is the brief. Before you start, read the
repository's `AGENTS.md`, `README.md` and `docs/architecture.md`, then `DESIGN.md` in this
folder, then your brief. `PLAN.md` is the owner's source document. `DESIGN.md` maps it onto this
repository; where they differ, `DESIGN.md` wins, and your brief wins over both.

## Rules

- **Do your task only.**
  - The brief's Scope and "Done when" are the whole job; stop when "Done when" is met.
  - Add no feature, option, refactor or rename the brief doesn't ask for.
  - Change only the files and packages the brief names. If the task can't be done without
    changing something else, stop and report what and why.
- **Fit the product.**
  - The brief decides what to build; the code around it decides how.
  - Reuse what exists.
  - Add a dependency only if `DESIGN.md` §11 lists it.
  - Comments explain what is not obvious, briefly, with no references to tasks, plans or
    decisions.
- **Install once, control from the dashboard** (`DESIGN.md` §0).
  - Never add an install-time key, flag or MSI property to enable or tune a capability.
  - A capability is a tenant setting on the Settings page, delivered in the signed bundle and
    applied without a restart.
  - Device-internal values (tokens, hook commands, listen addresses) are written by the agent.
- **Fixed contracts.** The envelope, the policy bundle, the cloud API and the app catalog change
  only as `DESIGN.md` and your brief say. Privacy defaults never weaken:
  - nothing is read at `m0`;
  - prompt text leaves the device only at `m3` on a grant;
  - no prompt text appears in a log, a health report or an error message.
- **Vendor facts are checked.**
  - Verify every setting name, file path, event name and hook format against the vendor's current
    documentation or a real install before relying on it.
  - Record the version you checked in `DECISIONS.md`.
  - If the brief's vendor detail turns out wrong, follow what you observed and record it.
- **Unclear means narrowest.** Take the narrowest reading, state the assumption at the top of your
  report, and continue only if it is low-risk; otherwise stop and ask the owner.
- **Never:**
  - weaken or skip a test;
  - mark a task done without running its "Done when" check;
  - add telemetry of your own.
- **Repository rules hold:**
  - Work on a branch named `refactor-endpoint/<folder-name>`, cut from the previous task's branch
    unless the owner says otherwise.
  - Never commit to `main`; don't push or open a pull request unless asked.
  - Never commit `.claude/` or `skills-lock.json`.
  - Never run `node localdev/run.mjs`, and never touch the owner's tenant.
  - A schema change goes in `services/database/schema.sql` only (no environment exists yet).
  - An envelope change starts in `contracts/`.
  - `node tools/accept.mjs` passes before you report. A gate reported `SKIPPED` was not checked:
    say so.

## Verifying on the reference VM

Every on-device check runs on the reference VM described in `TESTBED.md`: a Windows 11 Hyper-V VM,
Entra-joined and Intune-managed. The owner's PC runs the lab and the browser, and is never the
test device. Read `TESTBED.md`'s rules before touching the VM or Intune.

- **Deploy** the agent to the VM through Intune, exactly as a customer would, with
  `node localdev/testbed/deploy.mjs`. It builds, publishes, nudges the VM and waits until the VM
  reports the new version.
  - Never install by copying or double-clicking an MSI in the VM.
  - The lab must be running; if it isn't, ask the owner to start it.
- **Check** inside the VM with `powershell -File localdev/testbed/invm.ps1`:
  - `-Command` runs as the VM administrator: services, files, HKLM, the machine trust store,
    `certutil`, the agent's state and log.
  - `-AsUser console` or `-AsUser second` runs in that Entra user's own session: HKCU, the user's
    environment, a tool or `curl.exe` run as that user, the tool's own UI.
  - `-Screenshot` captures the console screen.
  - Quote the exact `invm.ps1` command lines in the report.
- **Users.** "The console user" and "the second user" are the two Entra test users in
  `TESTBED.md`. Both are always signed in.
- **Tenant.** The lab tenant is `10ca1ab0-0000-4000-8000-000000000001`.
- **Dashboard checks.** A "Done when" clause about the dashboard is observed in the browser on the
  PC, at the lab's dashboard address.
- **Console steps are the owner's.** A step "at the VM's console" means using a desktop app's
  window (Cursor, Claude Desktop, ChatGPT Desktop, VS Code chat, the browser, an interactive
  `claude` session). No tool can drive those windows, so the owner does it:
  1. Stop and tell the owner exactly what to do: which app, the exact text to paste (canaries and
     test secrets included), and what to look for.
  2. Wait for them to confirm.
  3. Capture the result with `invm.ps1 -Screenshot`, and check the events and logs yourself.

  Everything that can run from a command line runs through `invm.ps1` instead.
- **Needs.** A brief's "Needs:" line lists what the owner must have done on the VM (a tool
  installed and signed in as the console user, a Mac). If it is missing, stop and ask; don't fake
  it.
- **Timing and resources.** Intune delivery takes minutes, sometimes longer. Batch your changes and
  deploy once they pass their unit tests, not after every edit. The VM's resources are what the
  performance budgets are measured against.

## Report

Report in the session, not in a file:

1. What changed, file by file.
2. How "Done when" was checked, the commands, and their result.
3. Assumptions and deviations (also added to `DECISIONS.md`).
4. Anything undone or blocked, and what would unblock it.

When the task is done, tick it in `README.md`.
