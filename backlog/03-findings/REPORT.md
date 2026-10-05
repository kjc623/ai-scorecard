# 03. Findings — report

## What was built, and where

**Rule catalogue.** `database/schema.sql` §11 now seeds `ref.rule` with the eight rule ids the
classifier actually publishes — `PCI_PAN_PATTERN`, `PAYMENT_CARD_PAN`, `SECRET_API_KEY`,
`GOV_ID_NUMBER`, `PII_CUSTOMER_RECORD`, `SRC_INTERNAL_REPO`, `PHI_CLINICAL_TERM`,
`LEGAL_CONTRACT_TERMS` — mapped to their `ref.data_class`, detector kind, severity and title. The
wording matches `query/dashboard/src/explore-stub.js`, so the preview and the seeded catalogue agree.
`ref.rule` had no rows before; `mart.finding.rule_id` is its foreign key, so nothing could reference
it.

**Finding severity/class are present-tense.** Per the owner's decision (recorded in `DECISIONS.md`
D3), `mart.finding` no longer stores `class_code` or `severity`. A finding is the match:
`(tenant_id, submission_id, rule_id, detected_at, decided_locally, collection_mode)`.
`mart.v_finding` reads `class_code`, `severity` and `title` from the current `ref.rule` row, so a rule
edit shows on every finding that names it. This is the shape `mart.v_tool_usage` already uses for
`ops.tool.sanctioned_state`. `docs/04` §3.5, §3.11 and §4.1 and the schema comments were updated to
match, rather than continuing to claim as-of-detection.

**The evaluation job.** `aggregation/aggregator/internal/rollup`:
- `FindingsSQL` (`rollup.go`) derives one finding per `(submission, rule)` from a prompt whose
  `labels` array names a rule that exists in `ref.rule`, over the half-open `received_at` window.
  `detected_at` is `ingest.submission.received_at`, the server clock (C26).
- `runner.go` runs it once per tenant per pass, in the same transaction as the aggregate buckets, over
  the trailing day window. It is insert-only: `ON CONFLICT (tenant_id, submission_id, rule_id) DO
  NOTHING`, so re-evaluating cannot duplicate a finding.
- A label naming a rule not in `ref.rule` raises nothing. Inventing a rule server-side would assert a
  detection the classifier never published; the class still appears in `mart.agg_class_period` via its
  `ref.data_class.default_severity` fallback.

**The review write path.** `query/query-api/src/review.js` plus `POST /v1/finding-review` in
`src/http/server.js`. It validates a closed body (`submission_id`, `rule_id`, `review_state` in
`confirmed`/`disputed`, optional `note`), refuses a body-supplied tenant, confirms the finding exists
for the session tenant, upserts `ops.finding_review` (actor from the session, `reviewed_at = now()`),
writes one `ops.audit` row (`finding.review`) in the same transaction, and returns the new state plus
the audit entry id. `open` is not settable — it is the absence of a review. `src/index.js` exports the
module. `sac_query` already held the grants (`database/schema.sql` §10).

## What was verified, and how

| Claim | Evidence |
|---|---|
| Rule matching and idempotence at the SQL level | `TestFindingsAgainstPostgreSQL` (aggregator integration test) against a live PostgreSQL with the schema applied: a published rule raises exactly one finding, an unpublished one raises none, a second evaluation inserts nothing, and editing `ref.rule` changes the view without touching the finding row |
| The frozen statements are insert-only and rule-bounded | `TestFindingsSQLIsIdempotentAndPresentTense` (unit) |
| The schema comes up clean from an empty database | `database/schema.sql` applied to a throwaway `postgres:17-alpine` with exit 0, then both aggregator integration tests run against it |
| Schema structure is intact | `node database/tools/check-schema.mjs` → 74 passed, 0 failed |
| The review endpoint's behaviour | five new tests in `query-api/test/http-server.test.mjs`: valid write stores + audits in one transaction with the session actor; unknown finding is 404 and writes nothing; a body tenant is refused; `open` is refused; no tenant is 403 and never reaches the database |
| The read path serves findings | `q5_findings` and `q9_event_detail` through the lab dashboard's own `/v1/query` forwarder |

**On the device-auth lab.** The migration was applied to `sac-authlab-postgres-1`
(`MIGRATION.sql`), the aggregator image was rebuilt and restarted
(`sac/aggregator:lab-auth`), and two passes ran. The first wrote 535 findings across the eight rules;
the second wrote zero new findings (the pass log dropped by exactly 535), which is idempotence in the
lab. The existing payment-card findings are present, so no device change was needed.

To exercise the task's exact scenario I drove `ingest.record_event` — the same function
`ingest-api` calls — to commit one prompt carrying
`{"class":"payment_card","score":0.9,"rule_id":"PAYMENT_CARD_PAN"}` for the lab tenant. Within one
aggregator cadence a finding appeared (`PAYMENT_CARD_PAN`, class `payment_card`, severity `critical`,
mode `m1`, `decided_locally` true); `q5_findings` returned it for `subject=u_03test`; `q9_event_detail`
returned the event behind it with its label, policy action and content state. A review
(`confirmed`) then wrote `ops.finding_review` and an `ops.audit` row (`finding.review`, actor
`analyst@lab.test`, detail naming the rule and state), and the next `q5_findings` read returned
`review_state: confirmed`.

Suites: aggregator `go test ./...` passes; `query/query-api` 198 pass / 0 fail / 15 skip (was 193/0/15;
five new); `query/dashboard` 139 pass / 0 fail / 6 skip (unchanged).

## What could not be verified

- **The owner's Windows device.** Findings are server-side; no new agent build is needed, and none was
  installed. The live test above used `ingest.record_event` rather than a real device submission.
- **The device simulator.** `localdev/tools/simulate-devices.mjs` cannot run from the harness: it
  speaks TLS to the edge at `https://127.0.0.1:8443`, but the dev certificate names `edge` and
  `localhost` and not `host.docker.internal` (the only name the harness can reach), and it shells out
  to a `docker` CLI it does not have inside the harness. This is why the real write function was used.
- **The dashboard page in a browser.** No Chromium is available on this host (`accept.mjs` reports the
  browser gate skipped), so the SPA itself was not rendered. The reads and the review write were
  exercised against the same `/v1/query` and `/v1/finding-review` the page calls.

## Decisions the owner should know about

- **D1** (job over `ingest.submission`) and **D2** (seed `ref.rule`) were built as recommended; **D3**
  was changed by the owner to current-severity-only and built that way. All three are recorded, with
  the research behind D3, in `DECISIONS.md`.
- **A rule edit now changes how old findings read.** That is the owner's accepted behaviour. It is the
  opposite of `mart.agg_tool_period`'s treatment of sanctioned state, and the schema comment says so
  deliberately.
- **No in-page confirm/dispute control was built.** The task's Done-when does not require one, and it
  would add a fourth browser endpoint plus a change to the section-14 endpoint allow-list. The write
  path exists and is tested; the dashboard already reads the resulting state. Say the word and it is a
  small follow-up.
- **Findings have no freshness watermark.** `SOURCE_WATERMARK['mart.v_finding']` is `null` (as is
  `ingest.submission`'s), so a findings response reports `freshness.state = not_yet_covered` even
  though the pass ran seconds ago. Wiring `mart.finding` to an `ops.aggregate_watermark` row is a
  follow-up; it was not done to keep this change contained.
- **The findings pass only evaluates the trailing day window.** A finding older than the window is
  never re-evaluated, and a submission upgraded after the window gains no new finding. This matches
  the aggregates' window and the "no backfill" decision, but it is a bound worth stating.
- **`POST /v1/finding-review` is a write on a service whose session is not built yet.** In the lab it
  uses the same development-principal header as the reads; a deployment refuses it until the Entra
  session exists.

## Bringing an existing database up to this schema

`mart` is derived and rebuildable, so the change is a rebuild rather than a data migration:
`backlog/03-findings/MIGRATION.sql` drops and recreates `mart.v_finding`, drops `mart.finding`'s
`class_code`/`severity` columns, reseeds `ref.rule` idempotently, and re-grants the view. It touches
no `ingest` row and no `ops.finding_review` row, so existing review decisions survive. Run it, then
let the aggregator run once (or run it with `--once`) to populate `mart.finding`.
`sql -v ON_ERROR_STOP=1 -f MIGRATION.sql`.

## Baseline / unrelated failures observed

- `device/classifier-host/parser/isolation` fails on this host ("the parent did not observe the
  child's memory use") — environment-specific and unrelated.
- `ingest-api/internal/store` fails against the populated authlab database with a duplicate
  `ops.tenant` key — the test inserts the lab tenant that already exists, unrelated to this change.
- `tools/check-seams.mjs` reports the pre-existing `endpoint/protocol/content.go` `policy_rule_id`
  finding.
- The `db` acceptance gate (PowerShell `run-invariants.ps1`) does not run on this host and was skipped.

None of the modules these touch were changed by this task.
