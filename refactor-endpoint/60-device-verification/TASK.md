# 60. Device verification

Needs:
- tasks 58 and 59 finished, and `TESTBED.md` filled in;
- the build tasks to verify merged to `main` and deployed;
- for each brief in turn, its "Needs": the tools installed and signed in on the reference VM, and
  the owner available for the console steps and the dashboard.

## Problem

The build tasks were built and verified on the PC, with no pre-prod and no device. Each brief's
"On the device" section, the checks that need a managed device enrolled in a real environment, is
still pending. Nothing has yet shown an event from a device reaching PostgreSQL and the dashboard,
a setting on the Settings page reaching a device, or a real tool's output matching the fixtures the
normalizers and parsers were built against.

## Goal

Every "On the device" section in this folder has been run against pre-prod's test tenant, on the
reference VM, and passes. What it found wrong has been fixed and merged. The fixture sets written
from documentation have been replaced by captures.

## Scope

This task is a loop: one brief per session where a section is long. Take the briefs in task order;
`README.md`'s "Device" column shows the next one.

For each brief:

1. **Needs.** Check the brief's "Needs" line against the VM (`invm.ps1`), and ask the owner for
   anything missing; don't fake it.
2. **Settings.** Where the section says "ask the owner", stop, say exactly what to change or look
   at in the test tenant, and wait.
3. **Run the section** as written, with `tools/testbed/invm.ps1` (`AGENTS.md`, "How each kind of
   check is done"). Quote every command and its output in the report.
4. **Captures, spikes and measurements** come first within a section (tasks 25, 28, 30, 32, 36,
   39, 41, 43, 45 and 51): the capture replaces the `documented` fixtures, the spike's note goes in
   `DECISIONS.md`, and the measurement's numbers go in the report and in `DECISIONS.md`.
5. **A failed check, a capture that differs from the documented fixtures, or a spike that calls
   for code** is a fix on `refactor-endpoint/<folder-name>-fix-N` from `main`: the build phase's
   finish line, ready to merge, the owner merges, `deploy.mjs`, and the section again
   (`AGENTS.md`, the device phase). Tasks 45 (the desktop-backend parsers), 46 (their block
   shapes) and 47 (the QUIC rule, if task 43 calls for it) build their deferred pieces this way.
6. **Record.** Tick the brief's "Device" box in `README.md`. Anything the owner confirmed on the
   dashboard goes in the report with the page and the values they saw.

Order:
- Task 22 needs a full UTC day after task 21's section. Start it, and run later sections during
  that day only if they don't change the discovery collectors it measures.
- Task 43 runs before tasks 45 and 47.
- Task 50 restores the clean checkpoint and uninstalls: run it after task 49, and re-deploy at its
  end as its section says.

The rules of `TESTBED.md` hold throughout: pre-prod is read-only, the dashboard and the database
are the owner's, and the VM is reached only through `tools/testbed`.

## Done when

- Every brief with an "On the device" section is ticked in `README.md`'s "Device" column, and the
  report for each quotes its commands and results, with the owner's dashboard confirmations.
- No fixture set written from documentation (a `documented` folder) remains under
  `device/capture-core/otlp/testdata/` or `device/capture-core/parsers/`.
- Every fix branch is merged, and `node tools/accept.mjs` passes on `main`.
