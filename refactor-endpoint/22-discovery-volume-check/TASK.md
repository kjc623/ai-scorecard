# 22. Discovery volume check

Needs:
- the reference VM running, with `main`'s agent (including task 21) deployed by Intune, and every
  endpoint collector on for the test tenant, for one full UTC day;
- during that day, the owner uses the console session for ordinary AI-tool work: opening the
  installed desktop apps, CLIs and VS Code several times;
- the second user stays signed in.

The VM has no other users, so this is the closest it gets to a working day.

## Problem

Each discovery collector has been checked on its own. Together they must stay within the daily
budget (`DESIGN.md` §13, 200 records per device per UTC day), and every record must pass ingest's
validation. Two things can break this in real use:
- a per-day key that varies when it shouldn't (for example a version string that changes, or
  churning user hives) multiplies records;
- a record that fails the contract is quarantined, not stored.

## Goal

A measured day on the reference VM shows the discovery volume per collector and per type,
under budget with headroom, with zero rejected records. Any defect found is fixed in the
collector at fault.

## Scope

- Measure on the device, over one complete UTC day:
  - the spool log's (task 05) `envelope spooled` lines with `kind` `discovery`, grouped by
    `source` and `discovery_type`, counted with an `invm.ps1 -Command` script over
    `C:\ProgramData\ShadowAICapture\state\capture-core.log` and its rotated files;
  - records the server refused: the spool's `rejected_total` in `health.json`
    (`invm.ps1 -AgentState`), which must not rise during the day;
  - the `dropped` counters on the `inventory_scanner`, `process_detector` and `flow_monitor`
    health rows (`invm.ps1 -AgentState`).

  Cross-check the rejections read-only in pre-prod, with
  `az monitor log-analytics query` over ingest-api's console logs for the device. Ask the owner
  for the device id from the dashboard's Devices page if the logs need it.

  Put the scripts and queries in the report.
- If the day exceeds 150 records (75 % of budget) or any record was rejected:
  1. Find the cause (duplicate keys that differ only in a field that shouldn't vary, an invalid
     field).
  2. Fix it in the collector at fault, with a regression test.
  3. Measure another day.

  Changing the budget is not a fix; the owner decides the budget.
- Write the measured numbers (the day, the totals per type, the peak collector) into
  `DECISIONS.md` as the discovery volume baseline.

## Done when

- One full UTC day has under 150 discovery records from the reference device and zero rejected
  records. The report shows the script and query output.
- Any fix has its regression test, and `node tools/accept.mjs` passes.
