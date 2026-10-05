# 01. Usage aggregation

Depends on: nothing. Do this first; tasks 05 and 06 need it.

## Problem

On live data the Overview, Tools & data classes, Teams and Users pages show dashes and "Not yet
covered (no_watermark_row)". `ingest.submission` has rows, but `mart.agg_tool_period`,
`mart.agg_tool_user_period`, `mart.agg_class_period`, `mart.agg_org_period`, `mart.agg_user_period`
and `ops.aggregate_watermark` are all empty. No component in the repository writes them.

## Goal

A service or scheduled job that rolls `ingest.submission` and `ingest.observation` up into those
`mart` tables per tenant and bucket, and writes `ops.aggregate_watermark` so `query-api` can state how
fresh each aggregate is.

## Read first

- `docs/03-data-platform.md`: aggregates, watermarks, late-arriving events.
- `docs/04-dashboard-and-query.md` section 3: the ten questions and the measures each needs.
- `database/schema.sql`: the `mart.*` and `ops.aggregate_watermark` definitions and constraints.
- `query/query-api/src/registry.js`: the columns and measures each source must carry.

## Requirements

- Idempotent and re-runnable: a bucket recomputed twice gives the same rows.
- Events received late (a device spool flush) cause the buckets they fall in to be recomputed.
- `users` measures are distinct-subject counts. Keep whatever the schema needs for k-suppression.
- Runs in `localdev/authlab.compose.yaml` as its own container, on a short interval.
- Decide where it lives (a new Go module is fine) and say why in the report.

## Done when

After prompts are sent from the lab device: Tools shows tools with submission and people counts and
a time chart; Users shows a series for `lab-user`; Data classes shows the classes seen; and the
"no_watermark_row" banner is gone. Tests cover the rollup SQL against PostgreSQL and the
late-arrival case.

## Out of scope

Findings (03), coverage snapshots (02), directory data for Teams (06).
