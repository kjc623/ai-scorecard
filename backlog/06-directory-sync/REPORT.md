# 06 — Directory sync · report

## 1. Verdict

| Clause (task "Done when") | Verdict | Tenant | Evidence |
|---|---|---|---|
| "With a lab file source for the sample tenant's simulated people loaded, that tenant's Teams page shows usage by department with the unmapped series, and a department of fewer than five people stays suppressed." | **met** | sample (`5a3c…`) | `.integration/observe/2026-10-05T13-35-01-803Z-index-html-transport-live-teams.txt` (checks 2 held; 0 console errors, 0 failed requests, 0 other-host requests): rows `Engineering 184 / 5` and `Legal suppressed k=5`; tiles `MAPPED USER-DAYS 43 of 62` and `UNMAPPED USER-DAYS 19 of 62`. The Teams page was observed before and after the unmapped tile was added; this file is the after. |
| "The Department filter in Search returns rows there." | **met** | sample (`5a3c…`) | `.integration/observe/2026-10-05T13-36-10-555Z-explore-html-transport-live-events.txt` (checks 3 held; 0 console errors). Filled the department field to `Engineering`; the address became `#events?department=Engineering`, "Clear 1", and the rows are only Engineering people (`u_290f`, `u_3538`, `u_f6ed`); the Legal refs are absent. No query-api change was needed for this: the event source already joins `ops.user_dim`; the join matched nothing until this task filled the table. |
| "Tests cover sync, retirement and the unmapped count." | **met** | n/a | `control/control-api/internal/directory/directory_test.go` (11 tests): file and Graph sources; upsert, idempotent re-run, retirement keeps the row and its department, the unmapped NULL, skipped users with no mapping, the empty-read refusal, display-name gating, cipher round-trip and tenant isolation. `…/sql_live_test.go` (`-tags sac_sql_driver`, 2 tests) drives the same through the real store and schema. `query/query-api` keeps its q3 unmapped-residual test. |
| Owner: "Nothing on the device. The sync against a real Entra ID tenant through Microsoft Graph needs a tenant the harness does not have; say in the report what it needs to be pointed at one." | **owner** | n/a | No device path and nothing to observe there. What it needs is in §2. |

## 2. For the owner to do

Three lines in `backlog/OWNER-TODO.md`, under Run:

- Apply `backlog/06-directory-sync/MIGRATION.sql` to any database other than the lab's, after 05's.
- Point the sync at a real Entra ID tenant: register an application, grant `User.Read.All`
  (application), then run `control-api sync-directory --provider entra --entra-tenant <t>
  --entra-client-id <id> --entra-client-secret <secret> --directory-key "$SAC_DIRECTORY_KEY"` with
  `--store sql --dsn "$SAC_PG_DSN"`. The harness has no Entra tenant, so this is the one path not
  exercised here. The subcommand is in the tagged binary and in the rebuilt auth-lab image.

## 3. Decisions

- **The sync is `control-api sync-directory`, a subcommand of the control plane** — the component
  the Q2 record assigns directory work to. It is not an HTTP route and not a device path; it shares
  `sac_control`'s existing grant on `ops.user_dim`. Moving it elsewhere means a new role and grant.
- **The mapping is a configurable directory attribute whose value equals the device's `--user-ref`**
  (`onPremisesSamAccountName` by default). The owner chose this over bridging through the reported
  account name; it is stated in the package comment, the control-api README and `docs/04` §3.3, and
  tested at both sources.
- **A directory display name is stored (`ops.user_dim.display_name`) and shown beside the account
  name the device reports**, in the Devices list and on a search hit. The owner chose "both"; it is
  written only while `device_identity = 'clear'`, so the `hashed` opt-out still stores no clear name.
  Implementing it in more places (the event list) is a query/snapshot change this task did not make.
- **`directory_object_id_enc` is sealed with AES-256-GCM** under a per-tenant key derived from
  `SAC_DIRECTORY_KEY`. A lost key breaks only the mapping to a person (subject export/erasure), not
  the sync. Rotating the key would require re-sealing every row.
- **Retirement keeps the row and its attributes** (`status = 'inactive'`), so a departed person
  stays attributable and an already-computed aggregate is not rewritten. A read that returns no
  users is refused rather than acted on, so a misconfigured secret cannot retire a whole tenant.
- **`population` comes from an optional directory attribute, not group membership, and `manager_ref`
  is left NULL** — the legal workstream has not ruled on manager data (Q2 closing note 3).

## 4. Not finished, not verified

- **The Entra provider has never run against a real Entra tenant** — only against a fake Graph server
  that pages and carries a configured attribute. A real tenant may need different `$select`/paging.
- **Group membership is not resolved**: `population` is an attribute, so "provision users/groups to
  it" is satisfied for users only; a customer whose population is a group needs either a Graph
  `memberOf` read or a group attribute.
- **The directory display name on a search hit is unit-tested, not observed in a browser**: the
  sample tenant has no uploaded content to search, and the owner's tenant holds no directory rows
  (and must not be written to). The Devices column was observed in the browser.
- **No dashboard control configures or reports the sync** (recorded as deferred for 12).
- **`Cipher.Open` has no caller yet**; subject export/erasure (13) is where it is needed.
- The event list and detail still show the pseudonymous `user_ref` under "Person" (unchanged); the
  directory name was added where names are already shown.

## 5. Downstream impact

Read the remaining briefs. Two rows were added to `FOLLOWUPS.md`:

- **Brief 11** — the Devices read now also returns `ops.user_dim.display_name`, and a search hit
  carries it; the viewer/role reconciliation must cover this second personal datum, not only the
  User column. (This is the same class as the row 04 already added.)
- **Deferred, for 13** — subject export and erasure must decrypt `directory_object_id_enc` with
  `SAC_DIRECTORY_KEY` and `directory.Cipher.Open`; no path calls them yet.
- **Deferred, for 12** — the sync is configured by CLI/environment, not from the product, though
  `docs/04` §11 lists "directory sync" among Settings.

I did not edit the existing 06 row: the owner has now answered the display-name question, so the
owner can strike it.

## 6. What was built

Branch `backlog/06-directory-sync`, cut from `backlog/05-tool-catalogue` (00, 01, 04 and 05
`REPORT.md` in the tree). It uses 01's `mart.agg_org_period` (the aggregator already joins
`ops.user_dim`), 04's `ops.tenant.device_identity` and subject name, and 05's catalogue context.

- **control-api** — new `internal/directory`: `Source`/`Store` interfaces, a file provider, a Graph
  provider, an AES-GCM `Cipher`, the `Syncer` (upsert + retire + unmapped count), and the SQL store
  with its statements. `cmd/control-api/sync_directory.go` adds the `sync-directory` subcommand,
  dispatched from `main.go` before the HTTP configuration. README updated.
- **database** — `ops.user_dim.display_name` (+ comment) in `schema.sql`; `MIGRATION.sql` for an
  existing database.
- **query-api** — the devices read selects `directory_name`; `describeHits` joins `ops.user_dim` and
  returns `directory_name` on a search hit. Snapshot regenerated.
- **dashboard** — the Teams view adds an explicit `Unmapped user-days` tile; the Devices list adds a
  `Directory name` column; search hits show the directory name after the account name.
  `index.html`/`explore.html` regenerated.
- **docs** — `docs/04` §2.1 and §3.3, `docs/06` B7 and A6, `docs/risks/Q2-organisational-dimension`,
  `query/dashboard/README.md`.
- **backlog** — this report, `MIGRATION.sql`, `sample-directory.json` (the sample tenant's 40
  deterministic refs), the OWNER-TODO and FOLLOWUPS lines.

## 7. Evidence

- **Tests.** `control/control-api`: 43 top-level tests pass, 0 fail (baseline 32; +11, all in
  `internal/directory`). With `-tags sac_sql_driver`, 13 pass including the 2 live tests against
  `sac-authlab-postgres-1` via the `PG*` environment. `query/query-api`: 229 tests, 214 pass, 15
  skip, 0 fail (matches 05). `query/dashboard`: 147 tests, 141 pass, 6 skip, 0 fail (matches 05).
- **`node tools/accept.mjs`.** 9 gates: 5 pass, 3 fail, 1 skip — the three failures are exactly the
  baseline ones (`packages`: classifier-host + ingest-api; `seams`; `db`), and the `packages` run
  failed only those two modules.
- **Schema from empty.** `database/schema.sql` applied to a throwaway `postgres:17-alpine` with
  `ON_ERROR_STOP=1` (`EXIT=0`) and `ops.user_dim.display_name` was present; the container was
  removed after.
- **Lab migration and sync.** Applied `MIGRATION.sql` to the running lab, then ran the file sync for
  the sample tenant only: `read 40, synced 40, skipped 0, unmapped 9, retired 0`. `ops.user_dim` for
  the sample tenant holds 40 active rows and the aggregator's next pass wrote `mart.agg_org_period`
  (Engineering 5 people, Legal 2). The owner's tenant was not written: it still holds 0 `user_dim`
  rows and 1373 submissions. The rebuilt auth-lab control-api image carries the subcommand.
- **Instruction/script edits.** `ENVIRONMENT.md`: the lab DSN and the note that the tagged
  control-api/ingest-api tests default to a non-lab `127.0.0.1:5432` and so skip here. No `AGENTS.md`
  change. `query/dashboard/tools/observe.mjs` was not changed (the `--fill` it already had drove the
  department filter).
