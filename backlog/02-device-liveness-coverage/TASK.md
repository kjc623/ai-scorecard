# 02. Device liveness and coverage

Depends on: nothing. Task 04 needs the heartbeat built here.

## Problem

The Devices page shows both lab devices as "Never checked in" with last seen "Never", although one
is actively streaming events. Every page carries "Coverage not yet measured (no snapshot)".
`ops.device.last_seen_at` is never updated after enrolment, and `ops.collector_state` and
`ops.coverage_snapshot` are empty. Nothing in the repository writes them.

## Goal

1. `ingestion/ingest-api` records device activity when it accepts a batch: `last_seen_at`, and
   per-collector state (which collector reported, healthy or degraded, spool depth and dropped
   counts where the envelope carries them).
2. The endpoint agent (`endpoint/capture-core`) sends a periodic heartbeat even when no AI traffic
   occurs, so an idle device is not mistaken for a dead one.
3. A job writes `ops.coverage_snapshot` per tenant: devices enrolled, devices reporting, and gap
   reasons, on the definition in the design.
4. `mart.v_device_liveness` then yields reporting, stale, never_reported and revoked correctly.
5. The device read (or a small companion read) returns fleet-wide counts by status. Today the
   dashboard's "Need attention" card counts only the devices in the loaded page while the fleet card
   uses the server's fleet figure, so the two cards describe different populations.

## Read first

- `docs/04-dashboard-and-query.md` section 11.3 and the `q7_devices` question.
- `docs/01-collectors.md` and `docs/02-ingest-and-transport.md`: the envelope and health signals.
- `database/schema.sql`: `ops.device`, `ops.collector_state`, `ops.coverage_snapshot`,
  `mart.v_device_liveness`.
- `query/dashboard/src/views.js`, `devicesView`: what the page reads and how it words each status.

## Done when

The streaming lab device shows "Reporting" with a recent last-seen time; the other shows its true
state; a device that stops for longer than the stale threshold turns "Quiet since <date>"; the
coverage banner shows real numbers; and the two cards on Devices agree. Tests cover the liveness
transitions.

The heartbeat needs a new agent build on the owner's Windows machine. Build it, verify with the
device simulator, and say in the report what the owner must run.

## Out of scope

Hostname and the other identity fields (04).
