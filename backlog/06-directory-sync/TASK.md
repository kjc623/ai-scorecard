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
pseudonymous by design. Write the question and the consequences of each answer into `DECISIONS.md`.

## Done when

With the lab file source loaded, Teams shows usage by department with the unmapped series, and the
Department filter in Search returns rows. Tests cover sync, retirement and the unmapped count.
