# 04. Device identity

Depends on: 02 (heartbeat transport).

## Problem

The Devices page and every search result identify a device only by UUID
(`35beae1b-e366-465a-8517-58df42c88bdc`). `ops.device` stores `hostname_hash`, not the hostname, and
has no primary user, agent version or collection mode. `managed_state` is "unknown" for both lab
devices.

## Goal

Enrolment and heartbeat carry, and the device read returns:

- hostname
- username at time of submission
- agent version
- the collection mode (M0 to M3) in force on the device
- managed state, resolved from MDM where available, otherwise reported by the agent

Then update the dashboard: Devices shows the hostname as the device name with the UUID on hover,
plus User, Agent version and Mode columns; search results show `user | hostname | tool`.

## Read first

- `database/schema.sql`: `ops.device` and the comment on `hostname_hash`. It was hashed deliberately.
- `docs/06-security-and-threat-model.md` on device identifiers.
- `endpoint/capture-core`: enrolment and how the device identity is resolved.
- `control/control-api`: the enrolment handler.
- `query/query-api/src/registry.js`: `mart.v_device_liveness` and the dimensions it serves.
- `query/dashboard/src/views.js` `devicesView`, and `src/explore-render.js` `exploreHitMeta`.

## Raise before building

Storing the hostname in the clear reverses a design choice. Write the options and your
recommendation into `DECISIONS.md` (the owner's starting suggestion: clear hostname as a
tenant-level setting, on by default), and record the outcome as an ADR under `docs/adr`.

## Done when

The lab device appears by hostname on Devices and in search results, with its version and mode. The
schema change is in `database/schema.sql` and the lab comes up clean. Tests cover enrolment and the
read. This needs a new agent build and a reinstall of the lab MSI (`installer/lab-msi.mjs`) on the
owner's machine: say in the report what the owner must run.
