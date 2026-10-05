# Follow-ups

Two lists that outlive any one task. `AGENTS.md` says when to read and when to add to them.

Agents add entries and never remove them. The owner removes an entry by acting on it: editing the
brief, promoting the item to a task folder, or striking it as not wanted.

## Briefs that later work has made wrong

A `TASK.md` here says something that a finished task or a recorded decision has since changed. The
brief has not been edited yet. If your task is named, the entry is more recent than your brief:
follow the entry, and say so in your report.

| Brief | What it says | What is now true | Found by |
|---|---|---|---|
| 06 | People are "pseudonymous by design"; raise, do not decide, whether the dashboard may show a display name. | Task 04 put the clear account name (`subject_name`) on each submission and in search results, gated by `ops.tenant.device_identity` (ADR 0021). The question left for 06 is narrower: whether the directory's display name is shown as well as, or in place of, the account name the device reports, and whether it follows the same tenant setting. | 04 |
| 11 | Viewer: "aggregates only, no per-person data". | The Devices page now shows a user on each row, and the Devices read is subject-level and audited (04). The role reconciliation has to say whether a viewer sees Devices, and with or without the User column. | 04 |
| 11 | Its "Done when" names the dashboard and `/v1/query` as what an unauthenticated request must be refused by. | `POST /v1/finding-review` is a write on `query-api` that trusts the same development header (03). It needs the session and a role too. | 03 |
| 12 | A mode change "is delivered to devices through the signed policy bundle". | `ops.policy_bundle` has no writer (03, 04). No task builds one. Either 12 grows to include it or it becomes a task before 12. | 03, 04 |
| 12 | Lists the settings to build. | `ops.tenant.device_identity` (`clear` or `hashed`) is a tenant setting with no way to change it from the product (04). It reaches devices on the enrolment and health responses and is meant to move into the signed bundle. | 04 |
| 12 | "The mode each device has actually applied (task 04 supplies it)". | 04 supplies the device's effective base mode: the device override if the bundle names the device, else the tenant default. Per-tool and per-class modes are not reported. The owner had not decided this (04 `DECISIONS.md` D4). | 04 |
| 13 | Erasure removes a person's data. | The scheduled aggregation does not remove a subject who has disappeared from `ingest`; that needs a targeted bucket recompute, which 01 left to the erasure path. Findings are insert-only and are not removed on re-evaluation (03). | 01, 03 |

## Deferred work

Work a task found and did not do. One line each: enough to decide whether it becomes a task.

| Item | Why it was left | Found by |
|---|---|---|
| A writer for `ops.policy_bundle` (the signed bundle's server side). | Outside every task so far; 03 and 04 each worked around its absence. Blocks 12. | 03, 04 |
| `mart.agg_device_period` is not written; Devices is served from `mart.v_device_liveness`. | It rolls up collector state, not events, so 01 left it to 02, and 02 did not need it. | 01, 02 |
| The aggregator cannot list tenants as `sac_ops` under forced row-level security; production must pass tenants explicitly or use another role. | The lab runs it as `postgres`, so it did not arise. | 01 |
| Findings have no freshness watermark, so a findings read reports `not_yet_covered` seconds after the pass ran. | Kept 03 contained. | 03 |
| No confirm or dispute control on a finding in the page; the audited write (`POST /v1/finding-review`) exists. | Not in 03's "Done when"; it adds a fourth browser endpoint. | 03 |
| Findings are evaluated over the trailing day only: no backfill, and a submission upgraded later gains no finding. | Matches the aggregates' window. | 03 |
| Rules are a fixed seed in `ref.rule`; there are no tenant-specific rules. | Waits on the bundle writer. | 03 |
| The agent's route-to-collector mapping is a table in the agent, not read from `ref.collector`. | A conformance check would close it. | 02 |
| Coverage expects every collector on every device, so a device that rightly runs fewer shows a gap with reason `unknown`. | There is no per-device expected-collector data. | 02 |
| Managed state is whatever the agent reports; there is no MDM resolver. | Nothing in the repository to resolve from. | 04 |
| The account name is the machine's interactive user; a background service or a multi-user host may attribute a request to the wrong person, or to nobody. | Could not be tested without the real device. | 04 |
| The Devices read is audited on every load now that a row carries a user. | A consequence of 04's decision; the cost on a frequently read page was not measured. | 04 |
| The owner's tenant still holds eight simulated devices and their events from tasks 01, 02 and 04, and one device whose `last_seen_at` task 02 backdated by hand. | Removing them deletes data from the live tenant; the owner has not asked for it. | owner's review, 2026-10-04 |
| The Devices page lists 20 rows for 10 enrolled devices; each simulated device appears six times. | Seen on the first browser observation, after 04. Not investigated. | owner's review, 2026-10-04 |
| `vault/content-vault/Dockerfile` gained a `COPY` of `endpoint/protocol/` so the lab build works. | Outside 01's module; flagged for review, not reviewed. | 01 |
| The header of `localdev/tools/simulate-devices.mjs` says it needs `node localdev/run.mjs --auth`, which `AGENTS.md` forbids. | Instruction and script disagree; the script's header is the stale one. | owner's review, 2026-10-04 |
