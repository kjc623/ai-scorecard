# 0011. Collector health is a keyed operational channel, not an event stream

Status: proposed
Date: 2026-10-02

## Context

Brief §4.1 requires that "every collection path must emit the same record". Brief §7 requires
per-collector health — state, version, last successful capture, permission state — and brief C24
requires tamper detection to be *reported* rather than inferred from an absence of events.

Taken together, the obvious reading is that health is just another kind of event.

The arithmetic says otherwise. Five thousand devices reporting health hourly is roughly **44M rows per
year** — ten times the ~4.4M prompt submissions the product exists to collect. Brief §3.1 warns about
exactly this shape of problem in its own words: a category that dominates everything else if it is
emitted raw rather than filtered and rolled up at the source. R7 applies the same reasoning to process
telemetry; health has the identical property and a different cause.

## Decision

Health is **not** an event kind. It is an upserted operational record:

- `POST /v1/health` writes `ops.collector_state`, keyed `(tenant_id, device_id, collector)`, replacing
  the previous report rather than appending to it. Steady state is 5,000 rows per collector, not
  44M rows per year.
- Only a **daily per-device rollup** enters the analytical store, as `mart.agg_device_period`, which is
  what brief §3.6 question 7 actually needs.
- `collector_health` is therefore absent from the closed `kind` registry in
  [ADR 0010](0010-the-envelope-is-a-discriminated-union-with-a-closed-kind-registry.md), which applies
  to *collection* records: prompts, rollups and detections.

Two consequences follow from treating health as state rather than history:

- Because the record is an upsert, **staleness is the signal**. A device that stops reporting does not
  produce a gap in a stream; it leaves a row whose `last_report_at` stops advancing. `mart.v_device_liveness`
  turns that into an explicit `stale` or `never_reported` state, so brief C24's "reported rather than
  inferred" is satisfied by a record that exists and is wrong, which is exactly what an operator needs.
- The four health states (`healthy`, `degraded`, `absent`, `tampered`) are separate values that are never
  merged, per brief §3.2. `absent` and `degraded` describe different facts; neither is `tampered`.

## Alternatives considered

- **Health as an event kind.** Rejected on the arithmetic above. It also has a subtler cost: an event
  stream cannot represent "this collector is still broken", only "this collector reported a problem at
  time T", so every reader would have to reconstruct current state from history.
- **Health only in the daily rollup.** Rejected: brief §7 requires degradation to "surface as data,
  promptly", and a day of latency is not promptly.
- **Health via a metrics pipeline rather than the product API.** Rejected: brief C25's rule — no path may
  fail into a state that reports success — applies to health itself. Health delivered through a
  different channel with different authentication would be the first thing to break silently during an
  incident.

## Consequences

Easier: health stays small and cheap forever, and current state is one indexed lookup rather than a
query over a time series. The unhealthy-device screen is fast because it reads a table with one row per
device per collector.

Harder: the health channel is a second device-facing endpoint with its own authentication and versioning.
Losing a single health report is invisible until the liveness job marks the device stale, so the
thresholds for `stale` and `absent` are product parameters that have to be chosen deliberately rather
than defaulted.

We now maintain: the health endpoint, the two liveness thresholds, and the daily rollup from
`ops.collector_state` into `mart.agg_device_period`.

Revisit if: per-collector health needs to be reconstructable historically at a finer grain than daily —
for example to answer "was this device covered on the day of the incident". The answer then is a longer
rollup retention, not a return to health-as-events.
