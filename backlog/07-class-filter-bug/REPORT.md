# 07. Data class filter on events

## Verdict

| Clause ("Done when") | Verdict | Tenant | Evidence |
|---|---|---|---|
| `explore.html?transport=live#events?class=payment_card` lists the matching events from the lab's existing data. | met | owner (`11111111-…`) and sample (`5a3c0de0-…`) | observed in the browser: owner `2026-10-05T13-56-39-763Z-explore-…-payment-card.txt` ("50 events loaded, more available", rows carrying `payment_card`); sample `2026-10-05T13-56-31-172Z-….txt` ("26 events loaded", every row `payment_card`). Both under `.integration/observe/`. Console errors / failed requests / other-host requests: 0. |
| The class filter combined with another filter (`#events?class=payment_card&action=logged`) returns the events matching both. | met | owner and sample | observed: owner `…13-56-47-531Z-…-class-payment-card-action-logged.txt` ("39 events loaded"), sample `…13-56-35-645Z-….txt` ("3 events loaded"); every returned row shows `logged` and `payment_card`. 0 console errors. |
| A test pins the compiled predicate. | met | — | `query/query-api/test/compile.test.mjs` asserts the SQL contains `s.labels @> jsonb_build_array(jsonb_build_object('class', $…` and does **not** contain `labels @> jsonb_build_object('class'`. `test/db.test.mjs` additionally executes the compiled predicate against two fixture submissions (one `payment_card`/`logged`, one `source_code`/`blocked`) and asserts 1 row for class alone and 1 for class+action. Both pass (suite below). |
| The report gives the root cause in one paragraph. | met | — | "Root cause" below. |

The owner verifies: nothing (the brief says no clause needs the real device).

## Root cause

`ingest.submission.labels` is a jsonb **array** of `{class, score}` objects
(`contracts/event-envelope.schema.json` `$defs.label`), but the class predicate compiled to
`s.labels @> jsonb_build_object('class', $n)` — an **object** on the right-hand side. jsonb
containment requires the same top-level JSON type on both sides, so `array @> object` is false for
every row: the filter was valid, index-compatible, cost-guard-safe and returned nothing. The page
then showed "Not yet covered", because a zero-row result from `ingest.submission` (which has no
aggregate watermark) is reported `not_yet_covered` rather than `empty` — a second, pre-existing
read-path defect, not the cause. The fix wraps the operand in a one-element array:
`labels @> jsonb_build_array(jsonb_build_object('class', $n))`. `jsonb_path_ops` still serves it.

## For the owner to do

Nothing. The task leaves no line in `OWNER-TODO.md`; it has no clause the owner must verify.

## Decisions

- The predicate is fixed in `compile.js` as `jsonb_build_array(jsonb_build_object(...))`. It binds
  the class value and keeps the `jsonb_path_ops` GIN usable; an `unnest`/`EXISTS` would have needed
  a schema/index change and would not use the existing index. The SQL form is internal, so changing
  it later is cheap.
- To make the regression test actually run, `test/helpers.mjs` gained `findPsqlContainer()`, which
  prefers the device-auth lab (`sac-authlab-postgres-1`) for the `docker exec` tests. `findContainer`
  is unchanged: `pg-client.test.mjs` needs a reachable TCP host port, and the authlab publishes
  Postgres on 55435, which the harness container cannot open, so those integration tests must keep
  skipping. If the lab later publishes a reachable 5432, `pg-client` can use it too.
- A genuinely empty filtered list still returns `not_yet_covered`. I did not change the result-state
  logic: it is a wider, pre-existing issue across every watermarkless list source (findings,
  devices, coverage) and is outside the brief. Recorded in `FOLLOWUPS.md`.

## Not finished, not verified

- The empty-result state described above is not fixed. The brief's clauses only concern classes with
  matching rows, and the predicate was the reported bug.
- The `pg-client.test.mjs` live-TCP half still skips in this harness (8 skips); unchanged from the
  baseline and unrelated to this task.

## Downstream impact

- Read the remaining briefs (08–13): none is made wrong by this fix. Task 08 is about the
  classification input, not the class filter; task 09's prompt search does not use this predicate.
- `FOLLOWUPS.md`, Deferred work, one row added: a null `result_state` explanation — "any empty page
  from a watermarkless list source (`ingest.submission`, `mart.v_finding`, `mart.v_device_liveness`,
  `ops.coverage_snapshot`) reports `not_yet_covered`/`no_watermark_row`, so an honest 'no matching
  events' reads as 'the system cannot answer'; same root as the findings-freshness row above". Found
  by 07, left because it is a wider read-path change than this predicate fix.

## What was built

- Branch `backlog/07-class-filter-bug`, cut from `backlog/06-directory-sync` at `bc585f5`.
- Used the query read path as built through tasks 01–06 (`registry.js`/`compile.js`/`plan.js` were
  finalised with the tool-catalogue and finding work), and the lab's existing event data.
- `query/query-api/src/compile.js` — the `class` containment predicate now emits a one-element jsonb
  array operand.
- `query/query-api/src/registry.js` — the wrong-form comment on the `class` predicate corrected.
- `query/query-api/DSL.md` — §2.3's normative form corrected to the array operand, with why.
- `query/query-api/test/compile.test.mjs` — the pinning test updated to the array form and now also
  forbids the object form.
- `query/query-api/test/db.test.mjs` — a new live test proving the predicate matches a labelled row
  and composes with `action`.
- `query/query-api/test/helpers.mjs` — `findPsqlContainer()` added; a header note saying the db suite
  runs against the device-auth lab and `pg-client`'s TCP half still skips.

## Evidence

- No schema change; the lab needed no migration. `database/schema.sql` untouched.
- `query/query-api` (`node --test`): **before** this work 229 tests / 214 pass / 15 skip / 0 fail;
  **after** 230 tests / 222 pass / 8 skip / 0 fail. The +1 is the new db test; the +8 passing and −7
  skips are the db-backed suite now running against the device-auth lab. `BASELINE.md`'s 214/199/15
  is from task 00 and predates tasks 05–06.
- `query/dashboard` (`node --test`): 147 tests / 141 pass / 6 skip / 0 fail. Not touched; no failure.
- `node tools/accept.mjs`: gates 9, pass 5, fail 3 (`packages`, `seams`, `db`), skip 1 (`browser`) —
  identical to `BASELINE.md`. Inside `packages`, `query/query-api` is now `[PASS]` (its db suite runs);
  the only failures remain the baseline `endpoint/classifier-host` (2) and `ingestion/ingest-api` (5).
- Direct `POST /v1/query` (read-only) before the page observation: owner `class=payment_card` →
  `coverage_degraded`, 50 rows (limit); owner `class+action=logged` → 39; sample `class=payment_card`
  → 26; sample `class+action=logged` → 3.
- The db test SQL was also run directly through `docker exec sac-authlab-postgres-1 psql` wrapped in
  `BEGIN … ROLLBACK`, printing `class_only|1` and `class_and_action|1`.
- Edits to shared files beyond the task: `test/helpers.mjs` (listed above). No edit to `AGENTS.md` or
  `ENVIRONMENT.md`.
