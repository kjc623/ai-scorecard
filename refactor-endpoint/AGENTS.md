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
    documentation before relying on it. A real install is checked in the device phase.
  - Record what you checked in `DECISIONS.md`: the documentation's version or date during the
    build, the installed version in the device phase.
  - If the brief's vendor detail turns out wrong, follow what you found and record it.
- **Unclear means narrowest.** Take the narrowest reading, state the assumption at the top of your
  report, and continue only if it is low-risk; otherwise stop and ask the owner.
- **Never:**
  - weaken or skip a test;
  - mark a task done without running its "Done when" check;
  - add telemetry of your own.
- **Repository rules hold:**
  - Work on a branch named `refactor-endpoint/<folder-name>`, cut from `main`.
  - Never commit to `main`.
  - Don't push or open a pull request unless asked. The owner merges.
  - Never commit `.claude/` or `skills-lock.json`.
  - Never touch the owner's tenant (`11111111-1111-1111-1111-111111111111`).
  - The local lab (`localdev/`) is not used for this work: don't run it, depend on it, or verify
    against it.
  - Every schema change goes in `services/database/schema.sql` **and** in the next numbered
    migration in `services/database/migrations/` (see the README there). The migration must also
    apply cleanly over the new `schema.sql`, because that is how pre-prod's database is built
    (task 04). Seed rows in `ref` tables (collectors, routes, catalog) are schema changes too.
  - An envelope change starts in `contracts/`.
  - `node tools/accept.mjs` passes before you report. A gate reported `SKIPPED` was not checked:
    say so.

## Two phases: build without infrastructure, then verify on the device

Nothing is deployed while the build tasks (01–57) run: there is no pre-prod and no device. A task
is built and verified on the PC, and the checks that need a device wait for the device phase.

### Build phase

- Implement the scope, and pass the unit and integration tests and `node tools/accept.mjs` on the
  PC. The brief's "Done when" is the whole finish line.
- A brief's "On the device" section is not run, not simulated and not ticked. Task 60 runs it.
- Vendor facts come from the vendor's current documentation (Rules). Where a brief wants a real
  tool's output (telemetry fixtures, hook input, a file an app writes), the build uses a set
  written from the documentation, marked `documented`, and the device phase replaces it with a
  capture.
- The PC is the owner's Windows machine. Read it only where a brief says so (a registry location,
  a provider manifest), read-only, and install nothing on it.
- Commit on the task branch, then stop and report **done**: the branch, what it changes, and the
  "On the device" section that stays pending.
- The owner merges when they choose. A merge deploys nothing until pre-prod exists.

### Device phase

Tasks 58–60. Task 58 brings up pre-prod, task 59 the reference VM tooling, and task 60 runs every
brief's "On the device" section in task order on the reference VM (`TESTBED.md`: a Windows 11
Hyper-V VM, Entra-joined and Intune-managed, enrolled into pre-prod's test tenant). Read
`TESTBED.md`, including its rules, before touching the VM, Intune or pre-prod (Fly.io and
Supabase).

How a change reaches the VM in this phase:

1. The owner merges to `main`. That deploys pre-prod (services, database migration, and the
   signed agent release).
2. The agent runs `node tools/testbed/deploy.mjs`, which takes that run's release to the VM
   through Intune and waits until it runs.
3. The agent runs the brief's "On the device" section.
4. If a check fails, or the section calls for code (a capture that differs from the documented
   fixtures, a spike that calls for a rule, a `Render` change), fix it on a new branch from `main`
   (`refactor-endpoint/<folder-name>-fix-N`), to the build phase's finish line, then stop and report
   **ready to merge**: the branch, what will deploy, and the checks you will run afterwards. The
   owner merges; repeat from step 2.

How each kind of check is done:

- **Device side**, with `powershell -File tools/testbed/invm.ps1`:
  - `-Command` runs as the VM administrator: services, files, HKLM, the machine trust store,
    `certutil`, the agent's log.
  - `-AsUser console` or `-AsUser second` runs in that Entra user's own session: HKCU, the user's
    environment, a tool or `curl.exe` run as that user.
  - `-AgentState` shows health rows, counters, spool and delivery, and the bundle version in force.
  - `-Screenshot` captures the console screen.
  - Quote the exact command lines in the report.
- **That an event arrived**: on the device, the emitting collector's `emitted` counter rises and the
  spool drains with the batch acknowledged (`-AgentState`). Then the owner confirms it on the
  dashboard. Name the page, the tool, and the field values they should see.
- **Dashboard settings and pages are the owner's.** Agents have no dashboard sign-in.
  1. When a brief says "switch X on" or "the dashboard shows Y", stop and tell the owner exactly
     what to change or look at, in the test tenant.
  2. Wait for them.
  3. Record their answer in the report.
- **Pre-prod is read-only** (`TESTBED.md` rules). `fly status` and the services' logs (`fly logs`)
  help to diagnose a failure; never change anything. The database, on Supabase, is the owner's.
- **Users.** "The console user" and "the second user" are the two Entra test users in
  `TESTBED.md`. Both are always signed in.
- **Tenant.** The test tenant is `TESTBED.md`'s "Test tenant id".
- **Console steps are the owner's.** A step "at the VM's console" means using a desktop app's
  window (Cursor, Claude Desktop, ChatGPT Desktop, VS Code chat, the browser, an interactive
  `claude` session). No tool can drive those windows, so the owner does it:
  1. Stop and tell the owner exactly what to do: which app, the exact text to paste (canaries and
     test secrets included), and what to look for.
  2. Wait for them to confirm.
  3. Capture the result with `invm.ps1 -Screenshot`, and check the device side yourself.
- **Needs.** A brief's "Needs" line lists what the owner must have done on the VM (a tool
  installed and signed in as the console user, a Mac). If it is missing, stop and ask; don't fake
  it.
- **Timing and resources.** One round is a merge, the deploy workflow, and Intune delivery, so
  finish the code completely before reporting ready to merge. The VM's resources are what the
  performance budgets are measured against.

## Report

Report in the session, not in a file:

1. What changed, file by file.
2. How "Done when" was checked, the commands, and their result.
3. Assumptions and deviations (also added to `DECISIONS.md`).
4. Anything undone or blocked, and what would unblock it.

When the task is done, tick its "Built" box in `README.md`; task 60 ticks "Device".
