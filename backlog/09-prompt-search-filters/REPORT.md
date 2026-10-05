# 09 — Prompt search with filters

Branch `backlog/09-prompt-search-filters`, cut from `backlog/08-client-generated-requests` (the branch the session started on; every earlier task's `REPORT.md` is present in the tree).

## Verdict

| Clause (verbatim) | Verdict | Tenant | Evidence |
|---|---|---|---|
| "Searching "Australia" with the Person filter set returns only that person's prompts. Stored prompt content in the lab comes from one person, so also show that a different person returns none." | met | owner's | `.integration/observe/2026-10-05T15-16-53-386Z-…-window-d30.txt` (Person `lab-user`, query `Australia`, 4 hits, all `lab-user`; rail visible) and `…T15-17-00-855Z…txt` (Person `u_259d`, same query, "No uploaded prompt contains every one of those words.") |
| "The window switch applies to text results." | met | owner's | `…T15-17-08-004Z…txt`: after clicking the window switch, the address is `#events?window=h6` and the search is empty; the audit rows show the window sent (below). The indexed prompts all fall inside 24 h, so a 6-hour preset was added to make this observable. |
| "More than 20 matches can be paged." | met | owner's | `…T15-17-14-034Z…txt` ("Showing 20 matching prompts, more available", Load next page) then `…T15-17-20-399Z…txt` (39 after one page). 39 = the term `the`. |
| "If the lab holds fewer than 21 matches for any term, say so and show paging with a smaller page size." | not triggered | owner's | The lab holds 39 matches for `the`, so the condition does not arise. The page-size control (5 / 10 / 20) is present and covered by the dashboard test `a prompt-text search pages, and the page size can be smaller than the default`. |
| "The search is audited, with the filters recorded in the audit row." | met | owner's | `ops.audit` rows 909/912 (`action=content_search`, `detail.filters.subject` = `lab-user` / `u_259d`) and 916 (`filters.received_from` = six hours before, from the window run). Every observed search wrote one. |
| "Tests in the vault, `query-api` and the dashboard." | met | — | vault `TestFilteredSearchJoinsSubmissionAndPagesNewestFirst`; query-api 3 tests in `test/content-forward.test.mjs`; dashboard 4 tests in `test/explore-content.test.mjs`. Whole suites green (Evidence). |
| Owner: "nothing. No clause needs the real device." | owner | — | Nothing to check: every clause was observed on the lab, in the browser or the database. |

## For the owner to do

One line added to `OWNER-TODO.md` (Run): apply 09's grant to any database other than the lab's, after 08's,
`psql "$DSN" -v ON_ERROR_STOP=1 -f backlog/09-prompt-search-filters/MIGRATION.sql`. It is idempotent; the lab's database already has it.

## Decisions

- **The owner chose "B·join"** (asked before building, as the brief requires): the vault composes the whole search by joining `ingest.search_text` to `ingest.submission`, rather than `query-api` sending candidate ids or metadata being copied into the index row. It keeps the hard invariant (`query-api` still cannot read the index) and needs no write to the read-only owner's tenant. Hard to change later: the vault's column grant and the SQL join are the seam.
- **Ordering is newest-first**, as the brief says, where `docs/04` §15.3 had relevance first. The per-hit `rank` is still returned. `docs/04` §15.3 and §3.9 were updated.
- **A 6-hour window preset was added** to the events dataset (`src/vocab.js`, `src/explore-model.js`). Without it the window switch is not observable: every indexed prompt is inside the last 24 h. Removing it is a one-line revert; nothing in the API depends on it.
- **Rail filters the search cannot apply** (action, data class, request kind, department) are named on the page as list-only, rather than silently ignored. `docs/04` §15.3 records it.

## Not finished, not verified

- Pre-existing `docs/04` §15.3 design features are still not built and were left as they were: no index-coverage block, no request-time "at least one narrowing predicate" refusal (now marked an as-built gap; see FOLLOWUPS), and a hit carries one `ts_headline` snippet rather than 3 × 160-character fragments.
- The substring and fuzzy filename forms are still not forwarded by `query-api`, and no attachments are indexed; unchanged by this task.
- The page-size control was exercised by the dashboard test, not through the browser: `observe.mjs` runs all `--fill`s before `--click`s, so it cannot set a control that only appears after a search. The default-size paging was observed in the browser.

## Downstream impact

- Read the remaining briefs (10, 11, 12, 13). None is made wrong: 11's "content reader … may search prompt text" and 12's search-tier setting are unaffected. No FOLLOWUPS "brief now wrong" row.
- Added one **deferred work** row in `FOLLOWUPS.md`: the A15 request-time narrowing refusal is still not enforced; 09 composed filters but its "Done when" did not ask for the refusal.

## What was built

- **vault** (`internal/store`, `internal/vault`, `internal/httpapi`): `SearchQuery` gains `SearchFilters` and a keyset `SearchCursor`; the three search statements join `ingest.submission` and apply subject / tool / device / mode / received-at and the cursor, ordering `(received_at DESC, submission_id DESC, unit_kind DESC, unit_index DESC)`; the service mints/parses the cursor, records the filters in the audit detail, and returns `next_cursor`; `search_cursor_invalid` joins the closed refusal set.
- **query-api** (`src/http/content.js`): validates and forwards `subject`, `tool`, `device`, `mode`, `window` and `cursor`; relays `next_cursor`.
- **dashboard** (`src/explore-app.js`, `explore-render.js`, `explore-model.js`, `explore-stub.js`, `vocab.js`, `explore.css`): the filter rail and window stay visible during a text search; Person/Tool/Device/Mode and the window re-issue it; page-size control, "Load next page" and a shown-count; a note naming filters that apply only to the list.
- **database/schema.sql**: `sac_vault`'s column grant on `ingest.submission` extended with `tool_fingerprint`, `collection_mode`, `received_at`.
- **docs/04 §3.9, §15.3, docs/06 §4.4, the three service READMEs** updated.
- Earlier tasks used: 05 (`ops.tool_display_name` names a hit's tool), 06 (`ops.user_dim.display_name` enriches a hit), 08 (a `client_generated` submission is never indexed, so it never appears).

## Evidence

- Baselines this session: query-api 234 tests / 226 pass / 8 skip, dashboard 151 / 145 / 6, vault green. After: query-api 237 / 229 / 8, dashboard 155 / 149 / 6, vault `go test ./...` green (52 top-level pass, was 48 at `BASELINE.md`, the rest from tasks 05–08). No suite regressed; the only new failures are the `BASELINE.md` ones.
- `node tools/accept.mjs` → VERDICT FAILED, 5 pass / 3 fail / 1 skip: the same three gates as `BASELINE.md` (`packages`, failing on `endpoint/classifier-host` and `ingestion/ingest-api/internal/store`; `seams`; `db`), with vault, query-api and dashboard green inside `packages`.
- Live schema: `database/schema.sql` applied with `ON_ERROR_STOP=1` to a throwaway `postgres:17-alpine` container and exited 0; `database/tools/check-schema.mjs` 74 passed, 0 failed. All 20 vault statements `PREPARE` against the lab schema. The lab database had 09's grant applied with `psql "$DSN" -f backlog/09-prompt-search-filters/MIGRATION.sql` (privileges only).
- End-to-end through `dashboard`→`query-api`→`content-vault`: `australia` + `subject=lab-user` → 4 hits; `subject=u_259d` → 0; `the` + `limit=5` → 5 hits + cursor; `the` + a six-hour window → 0. Each browser run reported 0 console errors, 0 failed requests, 0 other-host requests.
- No edits to `AGENTS.md` or `ENVIRONMENT.md`. No new dependency.
