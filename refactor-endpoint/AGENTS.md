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

## Verifying on the reference host

The reference host is the owner's Windows 11 machine.

- Build the agent with `node localdev/lab-msi.mjs`. It needs the lab to be running; if it isn't,
  ask the owner to start it.
- Install by double-clicking `localdev\.msi\ShadowAICapture.msi`.
- The lab tenant is `10ca1ab0-0000-4000-8000-000000000001`.
- A "Done when" clause about the dashboard is observed in the browser, at
  `https://127.0.0.1:8787`.
- A brief that says "Needs:" lists what the owner must provide (an installed tool, a signed-in
  account, a Mac). If it is missing, stop and ask; don't fake it.

## Report

Report in the session, not in a file:

1. What changed, file by file.
2. How "Done when" was checked, the commands, and their result.
3. Assumptions and deviations (also added to `DECISIONS.md`).
4. Anything undone or blocked, and what would unblock it.

When the task is done, tick it in `README.md`.
