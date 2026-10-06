# 22. Discovery volume check

Needs: the reference host left running with the lab MSI from task 21 for 24 hours of ordinary use
(the owner's normal working day), with every endpoint collector on.

## Problem

Each discovery collector has been checked on its own. Together they must stay within the daily
budget (`DESIGN.md` §13, 200 records per device per UTC day), and every record must pass ingest's
validation. Two things can break this in real use:
- a per-day key that varies when it shouldn't (for example a version string that changes, or
  churning user hives) multiplies records;
- a record that fails the contract is quarantined, not stored.

## Goal

A measured day on the reference host shows the discovery volume per collector and per type,
under budget with headroom, with zero rejected records. Any defect found is fixed in the
collector at fault.

## Scope

- Measure from the lab database, for the lab tenant and the reference device, over one complete
  UTC day:
  - `ingest.observation` rows with `kind = 'discovery'`, grouped by `source` and
    `discovery_type`;
  - `ingest.rejected` rows for the device (must be zero);
  - the `dropped` counters on the `inventory_scanner`, `process_detector` and `flow_monitor`
    health rows (`ops.collector_state`).

  Put the SQL in the report.
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
  records. The report shows the query output.
- Any fix has its regression test, and `node tools/accept.mjs` passes.
