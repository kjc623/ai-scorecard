# 24. Alert when a scheduled job fails or stops running

## Problem

`azure/modules/monitoring.bicep` alerts on PostgreSQL, the two edges, container app restarts and
bursts of `ERROR` log lines. Nothing alerts when a job fails or never runs:

- `aggregate` (every 5 minutes): if it stops, every dashboard figure silently goes stale.
- `expire` (daily): if it stops, data outlives its retention, which customers are promised.
- `migrate` (each deployment): the deploy workflow fails on it, but a failed manual run is silent.

A job that is never scheduled writes no error line, so the log-burst alert does not cover it.

## Goal

An alert for each of: a failed execution of any job; no successful `aggregate` execution in the last
20 minutes; no successful `expire` execution in the last 26 hours.

## Scope

- `azure/modules/monitoring.bicep` and its call in `azure/main.bicep` (pass the job resource ids the
  way container app ids are passed today; none exist in a bootstrap deployment).
- Use signals Azure Container Apps really emits for jobs: job execution metrics, or
  `ContainerAppSystemLogs` in the environment's Log Analytics workspace. Cite the Microsoft
  documentation you relied on in your report (not in the code).
- Route to the existing action group.
- Out of scope: changing the jobs themselves.

## Done when

- `az bicep build --file azure/main.bicep` is clean (no warnings) and both parameter files build.
- `node azure/tools/check-infra.mjs` and `node tools/accept.mjs --only bicep,static` pass.
- After the first pre-prod deployment, the owner confirms the alerts exist in the portal and that
  stopping the `aggregate` schedule for 20 minutes fires the staleness alert.
