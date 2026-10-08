# 39. Other tools' hooks: spike

Needs, in the device phase (task 60): on the reference VM, installed and signed in as the console user: Codex CLI, Copilot CLI,
VS Code with GitHub Copilot, and Gemini CLI.

## Problem

Only Claude Code and Cursor are known to offer a pre-prompt hook that can block. Codex, Copilot
(CLI and VS Code) and Gemini CLI may have gained one. The bundle defaults keep their hook switches
off (`DESIGN.md` §5) until someone checks.

## Goal

For each of Codex CLI, Copilot CLI, Copilot in VS Code and Gemini CLI, there is either a working
adapter, or a recorded "not available" with the reason.

## Scope

- For each tool, answer from the current docs now, and in the device phase (On the device)
  confirm on the installed version by hand with a script hook on the reference VM (placed with
  `invm.ps1 -CopyTo`, the tool run with `invm.ps1 -AsUser console -Command` for CLIs or at the
  console for VS Code, and the result captured with `invm.ps1 -Screenshot`):
  1. Is there a hook that runs before a prompt is sent?
  2. Can it block, and does the tool show the hook's reason?
  3. Can the hook be declared in a machine-wide, admin-managed location that a user can't
     override or disable?
- An adapter is built only when all three answers are yes in the docs. It then follows task 36's interface
  and task 37's pattern:
  - `capture-core/hooks/<tool>.go`;
  - stdin fixtures under `testdata/<tool>/<version>/`;
  - a writer in `capture-core/toolconfig`, under the tool's existing `tool_config_<tool>`
    collector where task 29 or 31 created one; Gemini CLI gets `tool_config_gemini_cli` with a
    `ref.collector` row;
  - the tool's `hooks` switch is turned on in the server defaults (§5) and offered on the
    Settings page.

  A new tool key (`gemini_cli`) is added to the closed set in the database, control-api, the
  device bundle validation and the dashboard, as task 07 defined them. Every schema change
  (the tool key CHECK, the `ref.collector` row) goes in `services/database/schema.sql` and in the
  next numbered migration in `services/database/migrations/`.
- When the answers aren't all yes, nothing is built, and `DECISIONS.md` records which answer
  failed.
- `DECISIONS.md` gets one entry per tool: version checked, the three answers, and either "adapter
  built" or "not available: <reason>". The device phase confirms each entry on the installed
  version; an adapter the device contradicts is removed, and one the docs missed is built, on
  this task's fix branch.

## Done when

- `DECISIONS.md` has the four entries, from the documentation.
- For each adapter built, its `go test` passes.
- `node tools/accept.mjs` passes.

## On the device

On the reference VM, running `main`'s release:
- The hand test in Scope, per tool, confirms or corrects its `DECISIONS.md` entry.
- For each adapter built, with a test-tenant "block `credential`" rule (ask the owner to add it
  on the Settings page; wait), a prompt with an AWS-key-shaped test string is blocked in that tool
  with the rule's message, run as the console user (`invm.ps1 -AsUser console -Command` for a CLI,
  or at the console as an owner step with `invm.ps1 -Screenshot`).
