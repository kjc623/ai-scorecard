# 05 — Tool catalogue · report

## 1. Verdict

| Clause (task "Done when") | Verdict | Tenant | Evidence |
|---|---|---|---|
| "In the owner's tenant, the Claude Code traffic the lab device has already sent shows as 'Claude Code' on Tools, in Search rows and in search results." | **met** | owner (`1111…`) | Tools: `.integration/observe/2026-10-05T13-07-28-072Z-index-html-transport-live-tools.txt` (rows `Claude Code tls_b668…`, `tls_5a5a…`, `tls_69f6…`). Search rows: `…13-07-32-324Z-explore-html-transport-live-events.txt`. Search results (typed `device`, submit): `…13-07-34-956Z-explore-html-transport-live-events.txt`, hit meta `lab-user … Claude Code`. Each header: 0 console errors, 0 failed requests, 0 other-host requests. |
| "A fingerprint the catalogue does not know shows as 'Unrecognised tool', with the raw fingerprint available." | **met** | owner and sample | Owner Tools: `Unrecognised tool tls_2a942648fee3bbd5` in the same Tools file. Sample Unsanctioned: `Unrecognised tool acme_internal_llm` in `…13-09-53-316Z-index-html-transport-live-tools-view-unsanctioned.txt`. |
| "In the sample tenant, marking a tool unsanctioned through the API makes it appear in the Unsanctioned view with the people using it, and a tool used by fewer than five people stays suppressed." | **met** | sample (`5a3c…`) | `POST /v1/tool-sanction` for `claude_web` and `acme_internal_llm` (both `result_state: ok`, audited). `.integration/observe/2026-10-05T13-09-53-316Z-index-html-transport-live-tools-view-unsanctioned.txt` shows `Claude (web)` rows with their people and submissions, `acme_internal_llm` (2 people) as `suppressed k=5`, and the tiles `TOOLS INVOLVED 2`, `SUPPRESSED CELLS 2`. |
| "Tests cover resolution, the unknown case and the sanction write." | **met** | n/a | `query/query-api/test/tool-catalogue.test.mjs` (11 tests: registry/compile resolution, the unknown fallback, the sanction SQL and validation, plus two live tests against `sac-authlab-postgres-1` that resolve a catalogue name, an override and an unknown, and that exercise the upsert and the schema's attribution check, all rolled back); the three sanction HTTP tests in `test/http-server.test.mjs`. `query/dashboard/test/render.test.mjs` covers the name+fingerprint rendering. |
| Owner: "New Claude Code traffic from the Windows device resolves to the same name." | **owner** | owner | Needs the device; task 05 observed the four `tls_*` fingerprints already present resolving correctly. See `OWNER-TODO.md`. |

## 2. For the owner to do

- **Apply 05's migration to any non-lab database**, after 04's: `psql "$DSN" -v ON_ERROR_STOP=1 -f backlog/05-tool-catalogue/MIGRATION.sql`.
- **Verify on the real device** (after the pending reinstall): new Claude Code traffic shows as "Claude Code" on Tools, in Search rows and in prompt search results; an unknown destination shows "Unrecognised tool" with its fingerprint.
- Both are lines in `backlog/OWNER-TODO.md`.

## 3. Decisions

- **A shared catalogue, not per-tenant seeding.** New global `ref.tool_catalogue` (fingerprint → name, vendor, signal kind, evidence), resolved at read time by `ops.tool_display_name()`: tenant override in `ops.tool` → catalogue → the literal `Unrecognised tool`. `ops.tool` stays the per-tenant sanction decision. A name implies no sanction.
- **`tool` remains the raw fingerprint; `tool_name` is the display name.** Both are returned on Q1, Q2 and the event list, so the fingerprint stays the stable filter/group/index key and an unknown tool is visible as unknown. `tool_name` is selected only when `tool` is a grouped dimension (a name over an ungrouped fingerprint is a Postgres grouping error) — verified for Q4, which must not select it.
- **The sanction write lives on `query-api`.** `POST /v1/tool-sanction` upserts `ops.tool` and commits an `ops.audit` row in one transaction, beside the read that shows it (the finding-review precedent). Task 12's brief expects this API in `control-api`; changing it means moving the route and the grant. Recorded in `FOLLOWUPS.md`.
- **Q2's suppression cell is the `(bucket, tool)` group.** Compiled as `count(*) OVER (PARTITION BY bucket, tool)` over the grouped rows; a tool with < k people has its measures withheld while a tool with ≥ k publishes its per-person rows, per docs/04 §3.2. A suppressed row still names the tool and the person; the numbers are what is withheld.
- **Q2 orders by tool and then by person, never by volume.** The template previously ordered by `submissions` desc, which the dashboard's own table note ("not by volume") and the product rule forbid. It now uses the cursor docs/04 §3.2 names — `tool asc, subject asc` — so a working Unsanctioned view cannot become a leaderboard.
- **`tls_2a942648fee3bbd5` (owner tenant) is left unrecognised.** Three of the device's four TLS fingerprints reverse to `api.anthropic.com` paths (`/v1/messages`, `/v1/messages/count_tokens`, `/api/event_logging/v2/batch`); this one did not reverse in a 60M-candidate search over hosts and paths, so it is reported as unknown rather than guessed at. If the owner can say what destination it is, it is one catalogue row.
- **`observe.mjs` gained `--fill '<selector>=<value>'`** so a clause that needs text typed into the form (prompt search) is still observed in the browser. Documented in the script header.

## 4. Not finished, not verified

- Not identified: the `tls_2a942648fee3bbd5` fingerprint above.
- No page sets a sanction decision; the audited write exists and the pages show its result (deferred to 12, recorded in `FOLLOWUPS.md`).
- The `tf1:<base32>` extension fingerprints are shape-vector hashes the seed cannot enumerate; they render as "Unrecognised tool", which is honest but means a browser tool with no legacy name is unnamed.
- Not verified: the real Windows device (owner), and the `control-api` role/session path (11 is not built).

## 5. Downstream impact

Read the remaining briefs; two rows were added/changed in `FOLLOWUPS.md`, and one deferred item:

- **Brief 11.** "Its 'Done when' names the dashboard and `/v1/query` as what an unauthenticated request must be refused by." Now also: `POST /v1/finding-review` (03) **and `POST /v1/tool-sanction` (05)** are writes on `query-api` that trust the development principal header; they need the session and a role too.
- **Brief 12.** "An audited, admin-only API in `control-api` … The sanction decision per tool, using the catalogue from task 05." Now: 05 ships the audited sanction write on `query-api` (`POST /v1/tool-sanction`); 12 reuses it (and adds the page) or moves it to `control-api` and retires the route.
- **Deferred.** No dashboard control sets a sanction; it is task 12's Settings page.

## 6. What was built

Branch `backlog/05-tool-catalogue`, cut from `backlog/00-environment-check` (task 00's `REPORT.md` is in the tree; 01's is too). Used 01's aggregates and 04's device/identity fields.

- **`database/schema.sql`** — new `ref.tool_catalogue` (table + 43-row seed), `ops.tool_display_name()` (read-time resolution), the `ref.tool_catalogue`/`ops.tool`/function grants to `sac_query`, and the grant-preamble comment. **`backlog/05-tool-catalogue/MIGRATION.sql`** brings an existing database up (idempotent: `IF NOT EXISTS` + `ON CONFLICT DO NOTHING` + `CREATE OR REPLACE`).
- **`query-api`** — registry: `sanctioned_state` + the `ops.tool` join on `mart.agg_tool_user_period`, and the `tool_name` derived column on every aggregate/list source that groups a tool; `compile.js` emits the per-tool-cell suppression count and the conditional derived column; `templates.js` gives Q2 its default `unsanctioned` state, its tool-then-person order and its notes; `blocks.js` names the tool on the event detail; `sanction.js` + the `POST /v1/tool-sanction` route in `src/http/server.js`; `describeHits` names the tool on search hits.
- **`dashboard`** — `render.js` and `explore-render.js`/`explore-app.js` show the resolved name beside the raw fingerprint on Tools, Search rows, search hits and event detail; `questions.js` asks Q2 for `unsanctioned`; `vocab.js` mirrors the registry.
- **Docs** — `docs/04` §3.1/§3.2 "As built" notes and `query/query-api/DSL.md`.

## 7. Evidence

- **Tests.** `query/query-api`: 229 tests, 214 pass, 15 skip, 0 fail (baseline 214/199/15). `query/dashboard`: 147 tests, 141 pass, 6 skip, 0 fail (baseline 145/139/6). `node tools/accept.mjs`: 5 gates pass, 3 fail, 1 skip — the three failures are the baseline ones (`packages`: `classifier-host` and `ingest-api`; `seams`; `db`), and `invariants`/`vocab`/`contract`/`installer`/`endpoint` pass.
- **Schema from empty.** `database/schema.sql` applied cleanly to a throwaway `postgres:17-alpine` (`EXIT=0`), and `MIGRATION.sql` then applied to the same database re-ran clean; the resolver returned `Claude Code` and the catalogue held 43 rows. The throwaway container was removed.
- **Lab migration.** Applied to the running lab as `docker exec -i sac-authlab-postgres-1 psql -U postgres -d shadow -v ON_ERROR_STOP=1 < backlog/05-tool-catalogue/MIGRATION.sql` (`BEGIN…COMMIT`, `INSERT 0 43`). It adds reference data only; no tenant row changed. The owner's tenant still held 1373 submissions afterwards.
- **Sample-tenant seeding.** Three `ingest.record_event` calls for a new tool `acme_internal_llm` used by two people, written by `psql` into the sample tenant only; then `POST /v1/tool-sanction` for `claude_web` and `acme_internal_llm`, both `result_state: ok`.
- **Edits to shared instructions/scripts.** No `AGENTS.md`/`ENVIRONMENT.md` change. `query/dashboard/tools/observe.mjs` gained `--fill` (documented in its header); `query-api/README.md` names the two writes. The `db` and `seams` gate failures are unchanged from `BASELINE.md`.
