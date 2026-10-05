# 06. Directory sync

Depends on: 01.

## Problem

Teams says "The directory has not been synchronised" and is empty. People appear as raw references
(`lab-user`). The Department filter in Search matches nothing. `ops.user_dim` has no rows and no
sync exists.

## Goal

A directory sync that fills `ops.user_dim` per tenant with the pseudonymous user reference, the
department, and the encrypted directory identifier the schema already provides for. Start with
Microsoft Entra ID through Microsoft Graph, structured so a second provider can be added. For the
lab, provide a file-based source so it runs offline.

## Read first

- `database/schema.sql`: `ops.user_dim` and its column comments. The mapping from `user_ref` to a
  real person exists only for subject export and erasure.
- `docs/03` and `docs/06` on pseudonymity.
- `query/dashboard/src/views.js` `teamsView`: it expects an explicit unmapped series and
  `org_coverage`.
- `query/query-api/src/registry.js`: `mart.agg_org_period`.

## Requirements

- Incremental and idempotent. A user who leaves is retired, not deleted, so history stays
  attributable.
- People with no department are counted as an explicit "unmapped" series, never dropped.
- A team cell below k = 5 people stays suppressed.
- How the endpoint's user reference maps to a directory identity is stated and tested.

## Raise, do not decide

Whether the dashboard may show a display name for a user or must stay pseudonymous. Today it is
pseudonymous by design. Put the question and the consequences of each answer to the owner, and
build neither answer until it is given.

## Done when

The agent verifies, on the device-auth lab, with each page observed in the browser:

- With a lab file source for the sample tenant's simulated people loaded, that tenant's Teams page
  shows usage by department with the unmapped series, and a department of fewer than five people
  stays suppressed.
- The Department filter in Search returns rows there.
- Tests cover sync, retirement and the unmapped count.

The owner verifies:

- Nothing on the device. The sync against a real Entra ID tenant through Microsoft Graph needs a
  tenant the harness does not have; say in the report what it needs to be pointed at one.
