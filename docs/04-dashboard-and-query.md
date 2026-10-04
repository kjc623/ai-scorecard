# Shadow AI Capture — Dashboard and Query Layer

**Status:** proposed · **Component:** `query-api` (plain JavaScript on Node, `node:http`, zero dependencies) + `content-vault` (Go, internal ingress — search and retrieval) + `dashboard` (plain ES modules, HTML and CSS; no framework) · **Data class:** regulated personal data (GDPR/CCPA)

The analyst-facing read path: how the ten questions in brief §3.6 become cheap queries, and the rules
that keep the read side honest and auditable. Master doc §5.3 cites **§3 of this document** as the
ten-question mapping, so that mapping is normative here.

Sources, cited throughout: the engineering brief (`§1.1`, `§3.1`, `§3.5`, `§3.6`, `§4.4`, `C14`,
`C16`, `C30`, `R10`), `docs/00-architecture.md` (`D6`, as revised by
[ADR 0014](adr/0014-content-search-is-a-per-tenant-capability.md), which supersedes ADR 0008),
`docs/06-security-and-threat-model.md` (`§6.3`, `§6.5`) for the index's security position,
`contracts/event-envelope.schema.json`, and `database/schema.sql`, whose object names are used verbatim.
Every parameter not fixed by one of those carries an explicit **ASSUMPTION:** label with a one-line
justification, indexed in §16.

---

## 1. Scope

The query layer answers brief §3.6's ten questions from **precomputed aggregates, bounded event reads
and a bounded content-search path**, and nothing else. It is not a SQL surface, not a warehouse and not
a separate search system: the one text index lives in the same PostgreSQL instance as the events, for
the same reason nothing else here needed a new datastore (master doc §1.4).

**The one sentence that governs it:** *structured filtering is always available; text search is
available to the extent the tenant's `content_search` tier permits, and never beyond it.*

That is master doc D6 as revised by
[ADR 0014](adr/0014-content-search-is-a-per-tenant-capability.md), which reverses on customer
requirement the earlier decision to build no content search at all (ADR 0008). Brief §3.5's mutual
exclusivity has not gone away; it has moved out of policy prose and into the schema — `full_text`
together with `key_custody = 'customer_held'` is unrepresentable (§15.1). Consequences that shape
everything below:

1. **No query path returns full content** (§8). A content search returns **bounded highlighted
   snippets** through `content-vault` (§15.3); full content is still reached only through brief §4.4's
   per-event approved retrieval.
2. **Exactly one text index exists** — `ingest.search_text`, over prompt bodies and attachment
   filenames — and exactly one component can read it. There is no index over attachment *contents*, over
   the M2 excerpt or over digests (§15.2), and `query-api` is not granted `SELECT` on the one that does
   exist (§15.4).
3. A customer who wants cross-content search **in their own environment** still runs it against the
   columnar export (§9). That path is now *complementary* — the customer's own analytics, which brief
   §3.6 asks for in its own right — rather than the product's only answer to R10.
4. The read path must say **where it could not see** as clearly as what it saw — coverage and freshness
   travel with every metric, and a search result carries the coverage of its own index (§15.3), not only
   of the fleet (master doc §1.2, R11).

Out of scope: ingest (`02-ingest-and-transport.md`), collection (`01-collectors.md`), aggregate
construction and retention (`03-data-platform.md`).

---

## 2. Query API shape

### 2.1 Runtime, identity, tenant binding

| Aspect | Decision | Source |
|---|---|---|
| Runtime | Plain JavaScript (ESM with JSDoc types) on Node 22, `node:http`, zero dependencies, no build step; Azure Container Apps | `query/query-api/README.md` |
| Database role | `sac_query`: `SELECT` on `ingest`/`mart`/most of `ops`, `INSERT`+`SELECT` on `ops.audit`, `SELECT`+`INSERT`+`UPDATE` on `ops.finding_review`, **no** `SELECT` on `ops.content_object` and **none** on `ingest.search_text`; `sac_vault` holds the only read of the index | `database/schema.sql` §10 |
| Analyst auth | Entra ID (OIDC, authorization-code + PKCE) against the customer's tenant; the dashboard is a static SPA behind it | master doc §4.1, brief §3.3 |
| Tenant binding | Session creation resolves the Entra tenant to exactly one `ops.tenant.tenant_id`; the connection sets `app.tenant_id` before any statement runs | `ops.current_tenant()`, RLS policies |
| Fail-closed default | A session with no tenant set reads **zero rows** — the RLS predicate compares against `NULL` and yields `NULL` | `database/schema.sql` §3, C32 |

**The tenant comes from the authenticated session and never from the request body.** A request
carrying a `tenant_id` anywhere — body, query string, filter — is **rejected as a validation error, not
ignored**: silently dropping it would turn an attempted cross-tenant read into an uneventful success,
while rejecting it makes the attempt an auditable failure. Enforcement is structural (forced row-level
security, tenant leading every primary key), so a bug in the query layer cannot cross tenants.

**Names are not resolved in v1.** The dashboard shows `user_ref` plus `department`, `population` and
`manager_ref` from `ops.user_dim`; `directory_object_id_enc` exists so that subject export and erasure
can resolve a person, and is deliberately not a naming facility. **ASSUMPTION:** the brief is silent on
how an analyst learns which person a `user_ref` is; keeping names out of the store and out of our logs
is the conservative reading, with name resolution a documented follow-on.

### 2.2 Authorisation roles

Entra app roles mapped to a closed set, enforced per endpoint. Database roles are per *component*, so
role checks are the application's job and tenant isolation is the database's.

| Role | Can read | Structurally cannot |
|---|---|---|
| `analyst` | Aggregates as §6 permits; event/finding lists; device and coverage state; **content search** over the tenant's enabled scopes, returning bounded snippets (§15) | Full content; attachment contents; `ops.audit`; export configuration |
| `investigator` | All of the above, plus case creation, retrieval **requests**, finding review | Approve their own retrieval request |
| `approver` | Retrieval requests awaiting a second approver, and their evidence | Initiate a retrieval request (C16 separation of duties) |
| `auditor` | `ops.audit`, `ops.erasure_receipt`, `ops.reconciliation_run`, `ops.coverage_snapshot` | Subject-level events; content; configuration changes |
| `privacy_officer` | Subject export (§10), erasure requests, holds, notices | Content unless the export includes it and a second approver concurs |
| `tenant_admin` | Modes, retention, holds, directory sync, export destination | Content; subject-level events by virtue of the role |
| Vendor operator | Infrastructure metadata | Content, under every `key_custody` mode (master doc §4.3) |

**Content search is an `analyst` capability, not a new role.** The tenant's tier decides *what* may be
searched and the role decides *who* may ask; nothing about a search is role-gated beyond that, because a
tier that needed a second approver would be a different tier (§15.6). The two roles whose remit already
includes reading subject-level data — `analyst` and `investigator` — are the two that can search;
`approver`, `auditor`, `privacy_officer` and `tenant_admin` cannot, for the reasons their rows give.
**ASSUMPTION:** A16 — the brief names no role mapping, and this document's tier is a tenant decision
about capability rather than a second approval gate on content (06 §6.3).

**ASSUMPTION:** the brief names only an "analyst" while requiring a second approver (C16) and an audit
read path (§3.6 Q10); these six roles are the minimum that keeps *reading*, *approving* and *proving
what was read* in different hands.

### 2.3 Response envelope and versioning

Every read is `POST /v1/query` (the filter is a structured body). A single record is the
`q9_event_detail` template over the same endpoint, not a `GET` resource route; the service's whole HTTP
surface is `/healthz`, `/readyz` and `POST /v1/query`. One envelope for everything:

```json
{ "api_version": "1", "query_version": "1", "result_state": "ok", "data": [ ... ],
  "page": { "returned": 50, "next_cursor": "…", "snapshot_upper_bound": "2026-10-02T11:05:00Z",
            "newer_events_exist": true },
  "freshness": { "aggregate": "mart.agg_tool_period", "last_run_at": "…", "lag_seconds": 168,
                 "state": "fresh" },
  "coverage": { "window": ["2026-09-03","2026-10-02"], "devices_reporting": 4180,
                "devices_enrolled": 4620, "gap_reasons": {"not_enrolled": 440}, "state": "partial" },
  "suppression": { "k": 5, "suppressed_cells": 2 },
  "audit": { "entry_id": "…", "written_at": "…" } }
```

Three properties matter more than the field names. **`freshness` and `coverage` are always present on
a data-bearing response**, so the UI cannot render a number and omit its state without visibly
discarding fields it was given. **`result_state` is a first-class answer, not an error channel** (§13).
**`audit.entry_id` is returned to the caller**, so a screenshot of a number traces to the audited read
that produced it — which is what makes Q10 answerable.

**A content search uses the same envelope, with the two honesty blocks carrying search facts.**
`freshness` names `ingest.search_text` rather than a `mart` aggregate; `coverage` is the
index-coverage block — how much of the narrowed scope is in the index, and what the rest is
(`not_captured`, `local_only`, `shredded`); `data` is hit references with bounded fragments, never a
unit; and `audit.entry_id` is the search's own audit row. The rule that no data-bearing response may
omit its state holds here exactly as elsewhere, and for the same reason: a hit count over a partly
indexed scope is a floor, and a floor rendered as a total is C25's failure mode in the one screen where
it would be least visible.

Versioning: the URL carries the API major (`/v1`), `query_version` carries the DSL version, both are in
every response, and changes within `v1` are **additive only** — a renamed or repurposed dimension is a
saved query that silently means something else. An unknown dimension, measure, operator or bucket is a
**400 `unsupported_query_shape`**, never ignored, because ignoring an unrecognised filter answers a
different question than the one asked. A client requesting an unserved `query_version` is rejected,
never silently downgraded. Cursors embed the DSL version and a hash of the normalised query, so a
cursor cannot be replayed against another question, and a deploy that changes an ordering key
invalidates in-flight pagination explicitly (`cursor_expired`).

### 2.4 The closed DSL

Compiled by `query-api` into parameterised SQL. The caller supplies **values**, never identifiers,
never fragments.

| Dimensions | Source |
|---|---|
| `bucket` (hour, day, week, month) | `mart.agg_*_period.bucket_start` / `bucket_size` |
| `tool`, `sanctioned_state`, `class`, `severity`, `classifier_version`, `rule`, `action`, `mode`, `route`, `content_state`, `detection_basis`, `merge_confidence`, `confidence`, `decided_locally`, `review_state` | columns of the same name in `mart.v_tool_usage`, `mart.agg_*_period`, `ingest.submission`, `mart.v_finding` |
| `subject` | `user_ref` |
| `department`, `population`, `manager` | `ops.user_dim` (requires the Q2 directory sync) |
| `device`, `device_os`, `managed_state`, `region`, `liveness`, `collector`, `collector_state` | `mart.v_device_liveness` joined to `ops.collector_state`; `mart.agg_device_period` |
| `snapshot_day`, `gap_reason`, `observed`, `expected` | `ops.coverage_snapshot` |
| `actor_type`, `actor`, `object_type`, `case` | `ops.audit` |

A dimension is offered only on the sources that carry its column; the per-source table is
`query/query-api/DSL.md` §2.2, compiled from the frozen registry in `query/query-api/src/registry.js`.

**Measures:** `submissions`, `users`, `bytes_total`, `blocked`, `warned`, `logged`, `detections`,
`rollup_events`, `degraded_events`, `max_score`, `tools_used`, `block_events`, `healthy_days`,
`degraded_days`, `absent_days`, `tampered_days`, `spool_dropped` — each a column that exists in §4's
tables. A measure that is not precomputed is not offered; `observation_count` is a filter-only column
on `ingest.submission`, not a measure.

**Filters:** `eq`, `ne`, `in`, `not_in`, `lt`, `lte`, `gt`, `gte`, `between`, `starts_with` (tool
fingerprints only, and only on aggregate sources), `is_null` (`department`'s absence is a fact worth filtering on). **Grouping:** ≤ 3
dimensions plus one bucket. **Ordering:** only on a listed measure or a grouped dimension, always with
a deterministic tie-break on the remaining grouping keys — an ordering that is not total is a
pagination bug waiting to happen. **Bucketing:** `hour` and `day` are native (the `bucket_size`
constraint admits exactly those); `week` (ISO, Monday) and `month` are **read-side reductions over day
rows**, still aggregate-only (C27), and reported in `meta.applied_bucket`.

**Prohibited, and enforced by shape:**

- **Arbitrary SQL.** No `sql`, `raw`, `where`, `expression`, `order_by` or `filter_sql` field exists; a
  request containing one fails validation. Requests parse into typed structs and compile from a
  hard-coded registry.
- **String interpolation into SQL.** Nowhere, including identifiers and `IN` lists: dimensions resolve
  through a frozen map to literal column references, filter values are bound parameters, and list
  filters become `= ANY($n::text[])`. A test asserts no compile path concatenates request-derived text
  into a statement.
- **Content predicates in this DSL — still none, for a new reason.** A text predicate is not a filter
  over a column; it is a different read, against a table this component cannot see. No `content_match`,
  `snippet` or `search` field exists on `/v1/query`, and nothing here compiles an expression over
  `body`, `tsv`, `content_excerpt` or a digest. Text search is `POST /v1/content-search` (§15.3),
  executed by `content-vault`; a request carrying a text predicate on `/v1/query` fails validation
  rather than being silently ignored.
- **Cross-tenant anything.** The tenant is a session setting; there is no parameter to abuse.

---

## 3. The ten questions → read paths

Costs use the brief's sizing (≤ 5,000 devices, ~4,000 AI-active users, ~12,000 submissions/day, §3.1)
against §8's **< 2 s p95 from precomputed aggregates**. T = distinct tool fingerprints (tens to low
hundreds); U = AI-active users (≤ 4,000); D = departments (tens).

| # | Question (brief §3.6, verbatim) | Primary read path | Required key or index | Expected cost (p95) | Pagination / bucketing |
|---|---|---|---|---|---|
| 1 | Which AI tools are in use, ranked, over time? | `mart.v_tool_usage` over `mart.agg_tool_period` | PK `(tenant, bucket_start, bucket_size, tool)` | 1 day: T rows; 1 year ≈ 73k rows reduced in-query | Day native; week/month reduced; top-N per bucket (**as built:** not implemented — `limit` truncates whole buckets); ≤ 400 points |
| 2 | Which are unsanctioned, and who is using them? | `mart.v_tool_usage` → `mart.agg_tool_user_period` | PK + `(tenant, tool, bucket_start DESC, bucket_size)` | ≤ 7-day window unless scoped; ≤ few thousand rows | Cursor `(bucket_start DESC, tool, subject)`; 50/500 |
| 3 | How much is usage growing, per team? | `mart.agg_org_period` (**empty until Q2**) | PK + `(tenant, department, bucket_start DESC)` | D·T·buckets; capped at 2,000 cells | Day/week/month; `not_yet_covered` pre-sync |
| 4 | What classes of sensitive data are going into AI? | `mart.agg_class_period` | PK `(tenant, bucket_start, bucket_size, class, tool, severity, classifier_version)` | ≈ 7·T rows/day | Day/week/month; label fan-out reported, never summed as submissions |
| 5 | Which specific submissions hit a policy rule? | `mart.v_finding`; **+ content search (§15)** to narrow it | PK + `(tenant, detected_at DESC, submission_id)` | Cursor page ≤ 500; review join tiny | Cursor `(detected_at DESC, submission_id DESC, rule_id)` |
| 6 | Has a given person's usage changed, or spiked? | `mart.agg_user_period` + bounded `ingest.submission` check | `(tenant, user_ref, bucket_start DESC, bucket_size)` — **new** | ≤ 400 buckets; flush check ≤ low thousands of rows | Own baseline only; window-bounded; no cursor |
| 7 | What is the state of every device's collection? | `mart.v_device_liveness` ⋈ `ops.collector_state`; `ops.coverage_snapshot` as its own source | `ops.device (tenant, last_seen_at)`, `ops.collector_state (tenant, state)`, partial `ops.coverage_snapshot (tenant, snapshot_day) WHERE NOT observed` | ≤ 30k rows (5,000 × 6 collectors) | Cursor `(device_id, collector)`; 50/500 |
| 8 | What happened in this window, for this tool or person? | `ingest.submission`; **+ content search (§15)** to narrow it | `(tenant, received_at DESC, submission_id)`; `(tenant, user_ref, received_at DESC)`; `(tenant, tool, received_at DESC)`; GIN on `labels` | 1 day ≈ 12k rows; window capped at 31 days | Cursor only; 50/500; no unbounded reads (C29) |
| 9 | What exactly was sent? | **Search-then-retrieve**: candidates from `ingest.search_text` (§15), then `ingest.submission` + `ingest.observation` + `content_state`; full content via §8 | PK `(tenant, submission_id)`, `UNIQUE (tenant, dedup_key)`, `observation (tenant, dedup_key)`; `search_text_tsv_gin`, `search_text_name_trgm` | Search page ≤ 200 hits, then O(1) + O(routes) | Single record; search cursor `(rank DESC, received_at DESC, submission_id DESC)`; 50/200 |
| 10 | What has been accessed, and by whom? | `ops.audit` (append-only, hash-chained) | PK + `(tenant, occurred_at DESC, audit_seq DESC)`, `(tenant, object_type, object_id)` | ~36k rows/tenant/year; page ≤ 500 | Cursor `(occurred_at DESC, audit_seq DESC)` |

**Content search changes three rows of that mapping and adds no numbers.** **Q9 is now
search-then-retrieve**: a search narrows to candidate events and returns bounded snippets, and the full
content of one of them still goes through §8's approved path (§3.9). **Q5 and Q8 gain a text predicate
over the same bounded list they already served** — the search returns the matching events, and the
finding or activity view is read over that set — which is why both are marked above. **Q10 gains a new
action to display** (`content.search`) rather than a new read path. No aggregate row changes: search
never feeds a measure into Q1–Q4, Q7 or Q10, and it is not a measure itself. For a tenant at
`content_search = 'disabled'` the mapping is exactly what it was before the capability existed, which
is what a tier rather than a global switch means.

### 3.1 Q1 — Which AI tools are in use, ranked, over time?

- **Read path.** `mart.v_tool_usage` — `mart.agg_tool_period` LEFT JOIN `ops.tool`. The LEFT JOIN
  matters: a tool with no `ops.tool` row must still appear, `sanctioned_state` NULL rendered as
  `unknown`. Omitting it would be a third kind of wrong, and C8 forbids conflating `unknown` with
  `unsanctioned`.
- **Key and cost.** The primary key serves the range scan; ranking sorts T rows. One day is T rows; a
  year at 200 tools is ≈ 73k rows reduced to 12 monthly points — tens of ms against §8's 2 s budget.
- **Bucketing.** Day native; week/month reduced read-side and named in `meta.applied_bucket`; series
  ≤ 400 points — auto-coarsened beyond when the server chose the bucket, refused as `query_too_broad`
  naming the bucket that fits when the caller pinned one.
- **Ranking honesty.** Rank is by `submissions` with a deterministic tie-break and is never presented
  as a sanction signal; sanctioned state is present-tense configuration joined at read time, so the UI
  labels it "current policy state".
- **Gap.** Detection-only tools (mode I) and rollup-only evidence have no measure here, so such a tool
  is **absent from the tool inventory** — a silently incomplete answer. `detections` and `rollup_events`
  close this (§4.6); a deployment that predates them returns `not_yet_covered`.

### 3.2 Q2 — Which are unsanctioned, and who is using them?

- **Read path.** `mart.v_tool_usage` resolves the unsanctioned tool set; `mart.agg_tool_user_period`
  supplies the people. Both precomputed; nothing scans raw events.
- **Key and cost.** The PK serves the bucket range; add `(tenant, tool_fingerprint, bucket_start DESC,
  bucket_size)` for the tool-major view, where the bucket-first PK would read every tool's rows for
  every bucket. One day is users-per-tool rows (≤ a few thousand); thirty days unscoped would be
  ≈ 2.4M rows, so §12's guard refuses an unscoped window beyond **7 days** when `subject` is a grouping
  dimension, naming the narrowing that would make it servable.
- **Pagination.** Cursor `(bucket_start DESC, tool_fingerprint ASC, user_ref ASC)`; 50/500.
- **Subject-level.** Filters on and returns `user_ref`: audited as served (§5), and per-tool per-day
  cells below k subjects suppressed (§6).
- **Three states, not two.** `unsanctioned`, `unknown` and `sanctioned` are separate answers; `unknown`
  gets its own count and list. Merging it into either asserts something the tenant never decided
  (C8, brief §2).

### 3.3 Q3 — How much is usage growing, per team?

- **Read path, key, cost.** `mart.agg_org_period`, keyed `(tenant, bucket_start, bucket_size,
  department, tool_fingerprint, population)`; PK serves the bucket range, plus `(tenant, department,
  bucket_start DESC)` for a department-major trend. D·T·buckets: a year at 40 departments × 20 tools is
  292k rows, over the 2,000-cell cap, so the query collapses `tool` or coarsens — §12 says which rather
  than letting a browser wait.
- **The dependency, stated rather than assumed.** This question is answered **entirely** from
  `ops.user_dim`, synchronised from the customer's directory (Entra ID by default) with `department`,
  `population`, `manager_ref`, `status`, `synced_at`. That inbound flow is not described by the brief
  at all; it is **Q2** in master doc §7, and it is the only reason questions 2, 3 and 8 can name a team.
- **What the answer degrades to before it exists.** `mart.agg_org_period` is **empty** for that tenant.
  The API returns `not_yet_covered` with `reason: "directory_not_synced"` for the per-team axis, and the
  fallback is **by tool and by user** (`mart.agg_tool_period` + `mart.agg_user_period`) — what the
  schema's own comment on `agg_org_period` prescribes. A per-team chart with no data must never render
  as a flat zero line: zero usage by a team and no knowledge of which team a person is in are different
  facts (brief §3.2, C25).
- **Partial sync.** Users with `department IS NULL` are an explicit `unmapped` series, always present,
  beside `mapped_user_share` for the window; a per-team view silently covering 60% of usage is the
  failure R11 exists to prevent. Directory data older than the sync interval returns
  `coverage_degraded` with the sync age.
- **Suppression.** Department cells below k distinct users are suppressed (§6): a two-person team's
  daily count is de facto personal data.

### 3.4 Q4 — What classes of sensitive data are going into AI?

- **Read path and cost.** `mart.agg_class_period` keyed `(tenant, bucket_start, bucket_size,
  class_code, tool_fingerprint, severity, classifier_version)` with `submissions`, `users`,
  `max_score`, `degraded_events` — ≈ 7 classes × T
  tools per bucket, the smallest aggregate in the system.
- **Label fan-out is pinned in the API, not discovered by an analyst.** `submissions` counts
  submissions *carrying that class*, so one submission with three labels contributes to three rows;
  summing class rows and calling the result "submissions" overstates volume. The response carries the
  note and computes the non-additive total once for the same window from
  `mart.agg_tool_period.submissions` — two measures, two numbers, both labelled.
- **Suppression.** Each cell carries `users`, so §6 applies per cell: a class appearing in one person's
  submissions is that person's data.
- **Confidence must be visible.** C21 and brief §6 forbid reporting a failed classifier as "no
  sensitive data found", and this screen is where that failure would read as a fall in sensitive data.
  The same window's `degraded_events` count must sit beside the class mix (§4.6).
- **Version shifts.** Brief §6 wants a classifier change to appear as a version change, not a
  mysterious shift in the numbers. `classifier_version` is in `mart.agg_class_period`'s primary key
  (§4.6); because `mart` is droppable and rebuildable, adding it is a rebuild rather than a data
  migration.

### 3.5 Q5 — Which specific submissions hit a policy rule?

- **Read path.** `mart.v_finding` — `mart.finding` ⋈ `ref.rule` (title) ⋈ `ingest.submission` ⋈
  `ops.finding_review`, review state defaulting to `open`.
- **Key and cost.** The natural key `(tenant, submission_id, rule_id)` is why rebuilding `mart` cannot
  orphan an analyst's judgement, but it is not a time index: add `(tenant, detected_at DESC,
  submission_id DESC)` and `(tenant, severity, detected_at DESC)`. The source feed is served by the
  partial index `ingest.submission (tenant, received_at DESC) WHERE policy_action <> 'logged'`. A
  cursor page is ≤ 500 rows; the review join is on a small table.
- **Pagination.** Cursor `(detected_at DESC, submission_id DESC, rule_id ASC)` — all three columns,
  because `(submission_id, rule_id)` is what is unique.
- **Severity is as-of-detection.** `mart.finding.severity` is materialised when the finding is raised,
  so reclassifying a rule in `ref.rule` does not retroactively relabel history: a customer asking why
  something was treated as critical last month gets the answer that was true last month.
- **Review state is never defaulted silently.** `open` means nobody has looked, not "reviewed and
  unremarkable"; the three values stay distinct (brief §3.2).

### 3.6 Q6 — Has a given person's usage changed, or spiked?

- **Read path and key.** `mart.agg_user_period` for the series plus a bounded read of
  `ingest.submission` for the same subject and window. The aggregate PK is bucket-first, so one
  subject's series over a long window would read every user's rows for every bucket; add
  `(tenant_id, user_ref, bucket_start DESC, bucket_size)` and it becomes one contiguous index range.
- **Cost.** ≤ 400 buckets; the flush check reads the subject's own submissions, hundreds to low
  thousands per year (D1). The window bounds the result, so there is no cursor.
- **Rule.** Flag a day above `median(trailing 28 daily buckets) + 4 × MAD` **and** ≥ 3× the median,
  requiring ≥ 14 observed buckets first. **ASSUMPTION:** the brief requires "changed or spiked" without
  defining either; median and MAD are used because a mean would absorb the very flush day it is
  compared against. Below 14 buckets the answer is `not_yet_covered`, not "no change".
- **Two ways the flag lies, both checked before it is shown.** *Coverage:* a device that was not
  reporting produces a dip, not a behaviour change — the response joins `mart.v_device_liveness` and
  `ops.coverage_snapshot` and returns `coverage_degraded` with the specific gap. *Late flush:* an
  offline device flushing spikes received time, not behaviour — both clocks are on
  `ingest.submission`, so rows where `received_at - first_occurred_at` exceeds an hour annotate the
  flag as a flush. This is brief §3.6's "two clocks, never one" doing real work.
- **No peer comparison, ever.** The subject is compared against their own baseline: no cohort
  percentile, no ranking, no people sorted by volume. Brief §1.2 excludes per-employee scoring and
  ranking, and a spike view is the easiest place for that non-goal to re-enter through the UI.
- **Always audited.** It filters on a subject and returns one; there is no anonymous form of it.

### 3.7 Q7 — What is the state of every device's collection?

- **Read path.** `mart.v_device_liveness` (liveness `reporting` · `stale` · `never_reported` ·
  `revoked`, stale at 24 h) ⋈ `ops.collector_state` (state `healthy` · `degraded` · `absent` ·
  `tampered`, permissions, `spool_depth`, `spool_dropped_total`, `last_success_at`), with
  `mart.agg_device_period` for history. `ops.coverage_snapshot` (expected, observed, `gap_reason`) is
  read as its own source rather than joined: it is one row per device per collector per **day**, so a
  join without a `snapshot_day` predicate would multiply every device row by the days in the window.
- **Key and cost.** PK for per-device lookup; add `(tenant, last_seen_at)` for silence ordering,
  `(tenant, state)` for current degradation, and the partial index on `coverage_snapshot` for the gap
  list. ≤ 5,000 devices × 6 collectors = 30k rows; the not-reporting list is a filtered fraction.
- **This is where the honesty requirement is load-bearing.** "Including devices not reporting" means a
  silent device must be a **row produced by something other than the device** — which is what the
  liveness view does (C24). `stale`, `never_reported`, `revoked` and `reporting` are four facts, and a
  deliberately revoked device is not a quiet one.
- **The denominator is stated, never implied.** Coverage is measured over the **enrolled** fleet; the
  unmanaged remainder is not in `ops.device` and is not measurable from inside the product (brief §5.5
  plans 70–85% management coverage; R11 makes the gap an output). Every figure renders as "of N
  enrolled devices", and no fleet-wide percentage is shown that the product cannot compute.
- **Pagination.** Cursor on `(device_id, collector)` — the row grain after the collector join, so the
  key is total; 50/500. Sorting by silence duration or dropped total is a
  bounded sort over the filtered set.

### 3.8 Q8 — What happened in this window, for this tool or person?

- **Read path.** `ingest.submission`, filtered — the one path that touches event rows, and a **bounded
  list**, which is what C29 contemplates when it forbids *unbounded* result sets. It is not how any
  time-bucketed number is produced (C27).
- **Key and cost.** `(tenant, received_at DESC, submission_id DESC)` for the unfiltered window,
  `(tenant, user_ref, received_at DESC)` for a person, `(tenant, tool_fingerprint, received_at DESC)`
  for a tool, `(tenant, device_id, received_at DESC)` for a device. A GIN index on `labels` serves the
  class filter: **label** search, which brief §3.5 states is unconstrained. It is not the text index:
  content search is a separate read against a table this path cannot see (§15). One tenant-wide day is
  ≈ 12,000 rows; the window is capped at **31 days** without a narrowing predicate, and a query with no
  indexed predicate is rejected as `unsupported_query_shape` rather than allowed to become a sequential
  scan (§12).
- **Pagination.** Cursor only (C29), 50/500, every page a frozen snapshot (§7).
- **Both clocks, one authority.** Rows are ordered and windowed by `received_at` (server-assigned),
  because brief §3.6 makes server time authoritative for display and device clocks drift. The
  device-reported `occurred_at` (`first_occurred_at`/`last_occurred_at` on a merged row) is displayed
  beside it, labelled as possibly skewed; per-device skew comes from the health data. Neither is
  normalised away (C26).
- **Merge confidence is displayed, not hidden.** Rows with `merge_confidence = 'low'` — M0 records,
  canvas UIs, WebSocket-only observations — are marked individually and counted in the header ("N of M
  rows are low-confidence merges"). The schema keeps them rather than discarding them precisely so a
  customer sees that number instead of a quietly wrong one (R9).

### 3.9 Q9 — What exactly was sent?

- **Read path and cost: search, then retrieve — the order is the design, not a UI convenience.**
  **Step one, find the candidates by content.** `POST /v1/content-search` (§15.3) matches a term,
  phrase, substring or fuzzy filename pattern within the tenant's enabled scopes, composed with the
  usual dimensions — tool, class, severity, rule, review state, subject, window — and returns **hit
  references with bounded highlighted snippets**: enough to recognise the event, never the event's
  content. **Step two, open one event.** The single `ingest.submission` row by
  `(tenant_id, submission_id)`, its `ingest.observation` rows — one per route in `observed_routes`, so
  an overlapping-route count can be *explained* rather than merely defended (R9) — its labels, its
  policy decision, its `content_state`. A bounded search page, then O(1) plus O(routes).
- **The search half is where the answer narrows; it is not the retrieval half.** A snippet is a fragment
  of content and is therefore subject-level: role-gated, scope-narrowed, bounded to ≤ 3 fragments × 160
  characters, and audited before it is served (§15.3, §15.4). It is not a substitute for step two, and
  it cannot be widened into one — no parameter returns the rest of the unit.
- **What step two can show depends on `content_state`, and each is a different answer:**

| `content_state` | What the analyst gets | Why |
|---|---|---|
| `not_captured` | Labels, digest, size, policy action; content was never read | M0/M1 is a permission boundary (brief §1.1, C2) — and there is no index entry either, because no text ever reached the server to index (§15.2) |
| `local_only` | The same, plus "content remains on the device". **Not retrievable in v1** | Grants are device-initiated (C14) and no device-facing pull endpoint exists (master doc §5.1); adding one would create the bulk path C5 forbids |
| `uploaded` | Approved retrieval through §8 | C14, C16 |
| `shredded` | `no_longer_available` with `shredded_reason` | C17, brief §4.4 |

- **A hit is a reference, not a reservation.** Between the search and the retrieval the record can be
  shredded by retention expiry or erased; the hit pre-authorises nothing, and §8.2's
  `no_longer_available` is the answer, with the receipt that destroyed it. The search's own audit row is
  how that sequence — searched at T1, gone at T2 — is reconstructed.
- **The M2 excerpt is part of the record, not a retrieval, and not an index unit.** At M2,
  `content_excerpt` is a minimised span (contract-capped at 2,048 characters, never the whole payload)
  that policy decided may travel by default (brief §1.1). It returns inline with the event detail and is
  **not** gated by the approval workflow — gating it would make §1.1 self-contradictory — but it *is*
  subject-level data, so the read is audited. It is not searchable: `ingest.search_text.unit_kind`
  admits only `prompt_body` and `attachment_name` (§15.2), so an M2 tenant has nothing to search rather
  than a search over redacted text. **ASSUMPTION:** the brief defines the excerpt as data that travels
  at M2 and defines approval only for content retrieval; this is the only reading that does not
  contradict §1.1.
- **Always audited, both halves.** Step two is the single most sensitive read in the product and writes
  an audit entry as it is served (§5); step one does the same, and a search that returns nothing is
  still a search (§15.4).

### 3.10 Q10 — What has been accessed, and by whom?

- **Read path, key, cost.** `ops.audit`, append-only and hash-chained, filtered by actor, action,
  object, subject reference, case reference or window. PK `(tenant, audit_seq)`; add `(tenant,
  occurred_at DESC, audit_seq DESC)` for time-ordered browsing and `(tenant, object_type, object_id)`
  so "who looked at this event" is answerable — the question an auditor actually asks. The log grows
  with *reads*, not submissions: ~100 subject-level reads/day is ~36k rows per tenant per year.
- **Integrity is visible.** Each row hashes its predecessor within the tenant and the chain head is
  periodically anchored to write-once storage. The read path verifies the links **within each returned
  page** and displays the last anchored head with its anchor time; a mismatch returns
  `audit_chain_broken` rather than a list that looks fine. Whole-chain verification per request is the
  reconciler's job.
- **Recursion is bounded, explicitly.** Reading the log is itself a subject-level read (it names
  subjects), so it writes **one** audit row per query describing the filter and row count, and that row
  is not re-audited; unbounded self-auditing would be a denial of service on the table that proves what
  happened. **ASSUMPTION:** C30 says "every read of subject-level data"; a one-level, per-query rule is
  the only terminating interpretation.
- **Audit rows are never deleted, including by erasure.** No runtime role holds `UPDATE`/`DELETE` and a
  trigger blocks both. A subject erasure removes events and content and records in
  `ops.erasure_receipt.remaining_counts` what deliberately survived; audit rows keep a pseudonymous
  `subject_ref`, and the column that resolved that reference to a person is addressed by the erasure
  path. "We removed everything except the proof that it was accessed" must be said out loud in the
  receipt, not discovered later.

### 3.11 Indexes this document requires

Beyond primary keys, `database/schema.sql` defines five indexes: two partial unique indexes on
`ingest.submission` (`submission_exact_key_uniq`, `submission_weak_key_uniq`) and the three search
indexes created with `ingest.search_text`. The read paths above additionally need the ones below, all
tenant-leading (C32). **As built:** none of the indexes listed below is created by
`database/schema.sql` yet; they are required by this design, and `query-api` names them as the indexes
its sources depend on (`query/query-api/DSL.md` §2.1). The one text index in the system is `ingest.search_text`'s, created
with its table (§15.2) and readable by one role this component does not hold — it is not one of these,
and no path in this list can reach it.

```sql
CREATE INDEX submission_by_user      ON ingest.submission (tenant_id, user_ref, received_at DESC);
CREATE INDEX submission_by_received  ON ingest.submission (tenant_id, received_at DESC, submission_id DESC);
CREATE INDEX submission_by_tool      ON ingest.submission (tenant_id, tool_fingerprint, received_at DESC);
CREATE INDEX submission_by_device    ON ingest.submission (tenant_id, device_id, received_at DESC);
CREATE INDEX submission_labels_gin   ON ingest.submission USING GIN (labels jsonb_path_ops);
CREATE INDEX submission_policy_hits  ON ingest.submission (tenant_id, received_at DESC)
                                     WHERE policy_action <> 'logged';
CREATE INDEX observation_by_dedup    ON ingest.observation (tenant_id, dedup_key);
CREATE INDEX agg_user_period_by_user ON mart.agg_user_period (tenant_id, user_ref, bucket_start DESC, bucket_size);
CREATE INDEX audit_by_time           ON ops.audit (tenant_id, occurred_at DESC, audit_seq DESC);
```

Plus `ops.device (tenant_id, last_seen_at)`, `ops.collector_state (tenant_id, state)`,
`ops.coverage_snapshot (tenant_id, snapshot_day) WHERE NOT observed`, `ops.user_dim (tenant_id,
department)`, `mart.finding (tenant_id, detected_at DESC, submission_id)`, `mart.finding (tenant_id,
severity, detected_at DESC)`, `mart.agg_tool_user_period (tenant_id, tool_fingerprint, bucket_start
DESC, bucket_size)`, `mart.agg_org_period (tenant_id, department, bucket_start DESC)`, and
`ops.audit (tenant_id, object_type, object_id)`.

The search indexes — `search_text_tsv_gin (tenant_id, tsv)`,
`search_text_name_trgm (tenant_id, body gin_trgm_ops) WHERE unit_kind = 'attachment_name'` and
`search_text_expiry (tenant_id, expires_at)` — are defined with their table in §15.2. No read path in
this section is served by them, and no role in §2.2 except the vault may use them.

---

## 4. Aggregates

### 4.1 The mart

| Table | Key (conflict target) | Dimensions | Answers |
|---|---|---|---|
| `mart.agg_tool_period` | `(tenant_id, bucket_start, bucket_size, tool_fingerprint)` | bucket, tool | Q1, part of Q2 |
| `mart.agg_tool_user_period` | `(…, tool_fingerprint, user_ref)` | bucket, tool, subject | Q2 |
| `mart.agg_class_period` | `(…, class_code, tool_fingerprint, severity, classifier_version)` | bucket, class, tool, severity, classifier version | Q4 |
| `mart.agg_org_period` | `(…, department, tool_fingerprint, population)` | bucket, department, population, tool | Q3 |
| `mart.agg_user_period` | `(…, user_ref)` | bucket, subject | Q6 |
| `mart.agg_device_period` | `(…, device_id, collector)` | bucket, device, collector | Q7 |
| `mart.finding` | `(tenant_id, submission_id, rule_id)` | finding, not an aggregate | Q5 |

Nothing in `mart` holds workflow state — review decisions live in `ops.finding_review` under the same
natural key, so a rebuild cannot destroy an analyst's judgement — and sanctioned state is joined at
read time rather than denormalised, so reclassifying a tool does not rewrite history.

### 4.2 Cadence and lookback

The aggregator runs as `sac_ops`, per tenant. **ASSUMPTION:** the brief fixes no cadence; five minutes
bounds a number's staleness to a coffee break while the open bucket is being written, at trivial cost.

| Run | Scope | Why |
|---|---|---|
| Every 5 min | Open hour and open day bucket | §8 requires an event visible in < 60 s; aggregates lag by at most one cadence and always say so |
| Every 5 min | Trailing **7 days** of day buckets, **48 hours** of hour buckets | Devices flush in bursts after outages, so the recent past is recomputed rather than patched (C28) |
| Nightly | Coverage snapshots, device rollups, reference tables | R11's coverage state is a daily fact |
| On erasure, hold release, reconciliation finding | Exactly the affected buckets | Erasure must decrease a count, and only a recompute can decrease |

The lookback does **not** grow with device offline duration, and that follows from brief §3.6's two
clocks: buckets are cut on `received_at`, the authoritative server time, so a device offline for a month
flushes into the *current* bucket rather than triggering a month of rewrites. The trailing window only
has to cover the aggregator's own execution gap and the bucket boundary — the same ground §9's 7-day
re-export covers from the customer's side.

### 4.3 Why every bucket is replaced, never incremented

Every write is `INSERT … ON CONFLICT DO UPDATE` that **replaces** the bucket (C28: "aggregates are
upserts, never increments"). Four independent reasons, any one sufficient:

1. **Late arrivals are normal.** An offline device flushes its spool, possibly days late. An increment
   cannot tell a new batch from a retry, and brief §4.3 makes retries free by construction.
2. **Jobs must be re-runnable.** Ingest rejects duplicates in the store, so a crashed aggregation run
   that is re-executed must produce the same result as one that completed — only a recompute is
   idempotent.
3. **Rows can be upgraded.** When a higher-fidelity route reports a submission a lower-fidelity route
   already reported, `ingest.submission` is updated in place: labels, confidence, mode and winning
   route can all change after the aggregate counted them. An increment cannot express "that earlier
   contribution is now different".
4. **Erasure must subtract.** Subject erasure removes rows (D1) and the receipt must be truthful, which
   means the numbers must fall. This is decisive: an increment-only mart is structurally incapable of
   honouring an erasure.

Because of (4), the erasure job collects the affected bucket keys **before** deleting, deletes, then
recomputes exactly those buckets with the statements below **inside the same transaction**, recording
the count in `ops.erasure_receipt.removed_counts`. Erasure and its effect on the numbers commit
together, so no analyst can see a deleted person still contributing to a total. The same transaction
also removes the subject's `ingest.search_text` rows — the index entry cascades with the submission row
it belongs to (`database/schema.sql`: the `search_text` foreign key is `ON DELETE CASCADE`), so there is no
second deletion path to forget — and the count goes in the receipt. An erased prompt that stayed
searchable would be an erasure that did not erase (§15.5).

### 4.4 The upsert statements

The bucket grid comes **from the calendar and the known dimension members, not from surviving rows**,
and is left-joined to the source; otherwise a bucket that becomes empty — after erasure or retention
expiry — keeps its last value forever. Every bucket in the window is written on every run, including
the zero ones.

```sql
-- mart.agg_tool_period, day buckets, trailing window [$2, $3).
WITH buckets AS (SELECT generate_series(date_trunc('day', $2::timestamptz),
                                       date_trunc('day', $3::timestamptz) - interval '1 day',
                                       interval '1 day') AS bucket_start),
tools AS (SELECT tool_fingerprint FROM ops.tool WHERE tenant_id = $1
          UNION SELECT DISTINCT tool_fingerprint FROM ingest.submission
                 WHERE tenant_id = $1 AND received_at >= $2 AND received_at < $3),
grid AS (SELECT b.bucket_start, t.tool_fingerprint FROM buckets b CROSS JOIN tools t),
src AS (SELECT date_trunc('day', s.received_at) AS bucket_start, s.tool_fingerprint,
               count(*) AS submissions, count(DISTINCT s.user_ref) AS users,
               coalesce(sum(s.size_bytes), 0) AS bytes_total,
               count(*) FILTER (WHERE s.policy_action = 'blocked') AS blocked,
               count(*) FILTER (WHERE s.policy_action = 'warned')  AS warned,
               count(*) FILTER (WHERE s.policy_action = 'logged')  AS logged
        FROM ingest.submission s
        WHERE s.tenant_id = $1 AND s.received_at >= $2 AND s.received_at < $3
          AND EXISTS (SELECT 1 FROM ingest.observation o    -- prompt logical submissions only
                       WHERE o.tenant_id = s.tenant_id AND o.dedup_key = s.dedup_key
                         AND o.kind = 'prompt')
        GROUP BY 1, 2)
INSERT INTO mart.agg_tool_period (tenant_id, bucket_start, bucket_size, tool_fingerprint,
                                  submissions, users, bytes_total, blocked, warned, logged)
SELECT $1, g.bucket_start, 'day', g.tool_fingerprint,
       coalesce(src.submissions, 0), coalesce(src.users, 0), coalesce(src.bytes_total, 0),
       coalesce(src.blocked, 0), coalesce(src.warned, 0), coalesce(src.logged, 0)
FROM grid g LEFT JOIN src ON src.bucket_start = g.bucket_start
                         AND src.tool_fingerprint = g.tool_fingerprint
ON CONFLICT (tenant_id, bucket_start, bucket_size, tool_fingerprint) DO UPDATE
  SET submissions = EXCLUDED.submissions, users = EXCLUDED.users,
      bytes_total = EXCLUDED.bytes_total, blocked = EXCLUDED.blocked,
      warned = EXCLUDED.warned, logged = EXCLUDED.logged;
```

The other five use the same skeleton with a different grouping; conflict targets are §4.1's keys.

| Target | Source | Notes |
|---|---|---|
| `agg_tool_user_period` | `ingest.submission` by (tool, user_ref) | Subject-bearing; suppressed below k (§6) |
| `agg_class_period` | `ingest.submission.labels` unnested | **Fan-out:** one row per (submission, label); `severity` from `ref.rule`, `max_score` from the label score |
| `agg_org_period` | `ingest.submission` ⋈ `ops.user_dim` | Empty when `department IS NULL`; unmapped users reported, never dropped |
| `agg_user_period` | `ingest.submission` by `user_ref` | Carries no score, rank or efficiency measure (brief §1.2) |
| `agg_device_period` | `ops.collector_state` daily snapshot | Four state counts as separate columns (brief §3.2); `spool_dropped` carries C22's undercount |

`usage_rollup` and `model_detection` envelopes also become submissions, so every aggregate selects
prompts explicitly. `ingest.submission.kind` — denormalised from the winning observation (§4.6) — is
that selector, `s.kind = 'prompt'`; the `EXISTS` above, through `ingest.observation` on
`(tenant_id, dedup_key)`, is the equivalent semi-join. Either keeps rollup volumes out of submission counts — adding a rollup's
`submission_count` into a count of submissions is exactly the inflated number R9 exists to prevent.

### 4.5 Freshness watermark, and how the dashboard shows it

Each completed run upserts `ops.aggregate_watermark` for `(tenant_id, aggregate_name, bucket_size)`
with `last_complete_bucket`, `last_run_at`, `last_run_rows`. That row is the honesty mechanism for the
whole read side: every data-bearing response carries the `freshness` block built from it, and the
dashboard renders the age **on the tile that shows the number** — `updated 3 min ago · complete to
10:00` — never in a tooltip and never only on a status page.

`state: "stale"` when `now() - last_run_at` exceeds **3× the cadence (15 minutes)**. **ASSUMPTION:** the
brief sets a < 60 s visibility target for an event and < 2 s for a query, but no aggregate staleness
bound; three missed runs is where a number stops being a measurement and becomes history, and the state
is displayed either way. A stale aggregate is **never** silently recomputed on the read path: reading
through to raw events to "fix" it would violate C27 and would hide the aggregator's failure — the exact
inversion of C25. Brief §8's 60-second target is met on the event and finding paths, which read
`ingest.submission` directly and see a row as soon as its ingest transaction commits; the aggregate
paths are bounded by the cadence and say so.

### 4.6 Changes this document requires to `database/schema.sql`

**Four gaps this document identified in an earlier revision have since landed in `database/schema.sql`.** They
are kept here, resolved, because the reasoning is what justified them — the first three are additive
columns on rebuildable `mart` or a denormalised copy in `ingest`, and the fourth is a one-line grant.
**None changed the event envelope** — `contracts/event-envelope.schema.json` gained only the attachment
descriptor that content search required (ADR 0014).

| Change | Landed as | Needed for | What breaks without it |
|---|---|---|---|
| `mart.agg_tool_period` measures | `detections`, `rollup_events`, `degraded_events` | Q1 inventory, Q4 confidence | Mode-I and rollup-only tools never appear in "which AI tools are in use"; a classifier outage renders as a fall in sensitive data, which C21 forbids |
| `mart.agg_class_period` key | `classifier_version` in the primary key, plus `degraded_events` | Q4 | A classifier change appears as a mysterious shift in the numbers instead of a version change (brief §6) |
| `ingest.submission` column | `kind`, denormalised from the winning observation | Q1, Q4, every aggregate run | The prompt/rollup/detection split needs a semi-join on every run: expressible today, but implicit where R9 wants it stated |
| `sac_query` grant | `SELECT` on `ops.reconciliation_run` | §11.4 drift panel | Without it `sac_query` cannot read the table, so the drift the reconciler records (C34) is invisible to the one screen whose job is to show degraded collection |

Where a measure is absent for a tenant — `degraded_events` on an older deployment, or `mart.agg_org_period`
with no directory sync (Q2) — the affected panel returns `not_yet_covered` with the reason, never a
partial number presented as a whole one.

---

## 5. Read auditing

Brief §3.6: **"Every read of subject-level data writes an audit entry as it is served."** *Every* read,
and *as it is served* — not afterwards, not in a batch, not best-effort.

### 5.1 Mechanism

The audit row is written **in the same transaction that serves the rows**, before the rows are read,
and the response is emitted only after that transaction commits. The one exception is §5.2's small-cell
trigger: the cells' distinct-subject counts exist only once the cells do, so that row is written after
the read — still in the same transaction, and still before anything is served.

```sql
SELECT set_config('app.tenant_id', $1, false);  -- from the authenticated session, never the body
BEGIN;
INSERT INTO ops.audit (tenant_id, actor_type, actor_id, action, object_type, object_id,
                       subject_ref, case_reference, detail)
VALUES (ops.current_tenant(), 'user', $2, $3, $4, $5, $6, $7, $8::jsonb);
-- … the read itself, in the same transaction …
COMMIT;                                 -- only now does the API write the response body
```

- **The tenant is bound, never interpolated, and cleared on release.** `SET LOCAL` is a utility
  statement and takes no bind parameter, so the setting is made with
  `set_config('app.tenant_id', $1, false)`. That is session-scoped, so the pool resets it before a
  connection is reused, and a connection whose reset fails is closed rather than returned
  (`query/query-api/src/http/pool.js`).
- **Fail closed.** If the audit insert fails for any reason — permission, chain trigger, disk — the
  transaction aborts, the read returns `503 audit_unavailable`, and **zero rows are served**. A read
  that cannot be proved to have happened does not happen.
- **Nothing is streamed.** A subject-level response is fully materialised and committed before its
  first byte reaches the socket; streaming would serve bytes before the audit commits — the ordering
  C16 forbids for content and C30 forbids for reads. It is also why the large paths (export, subject
  export) are jobs rather than streaming endpoints.
- **The chain serialises per tenant**, because `ops.audit_chain` takes a per-tenant advisory transaction
  lock. That is the right trade — a forked chain is worthless — and it bounds per-tenant read
  concurrency by one insert's hold time, which is why §12's limits are per tenant.

### 5.2 What counts as subject-level

| Trigger | Why |
|---|---|
| Any query that **filters on** `user_ref` | Asking about a person is the act being recorded, whatever comes back |
| Any query that **returns** `user_ref` | The response identifies a subject |
| Any single-submission or single-finding detail read | The row belongs to a person; the detail view is the most sensitive read in the product |
| **Any aggregate whose scope resolves to fewer than k distinct subjects** — **k = 5** | A small cell *is* the subjects in it: with the org chart in hand, a cell a handful of people wide is a per-person fact. Five is the conventional small-cell floor and is below typical team size at this customer scale, so it does not blunt the product. Full rationale in §6.2; the schema anticipates the rule for `mart.agg_class_period` |
| Any content retrieval (request, approval, reveal) | C16; the audit is written before content is returned |
| Any content search (§15.4) | A search reads prompt text or a filename as it serves the hit, so it is subject-level by construction — audited whatever it returns, including nothing. The terms and scopes are in the entry, and an audit log of who searched for whom is itself an investigation record |
| Any read of `ops.audit` | One row per query (§3.10), bounded so it terminates |
| Any export run | One row per run, not per row (§5.3) |

**Not** subject-level, and therefore not audited: tool- and class-level aggregates whose cells resolve
to k or more subjects, device and coverage state, retention and hold lists, reference data. An audit
log that records the ordinary dashboard is noise, and noise is how a real access goes unnoticed.

The k-check costs nothing extra: §6 already computes each cell's distinct-subject count to decide
suppression, so the same value decides the audit trigger. **The audit decision is made before
suppression** — a query answered with `suppressed` still writes an audit entry, because the attempt to
resolve a small group is the fact worth recording, and a sequence of such attempts is visible in Q10's
own log.

### 5.3 The columnar export path

The scheduled export (§9) is row-level and subject-bearing, one job over millions of rows. An audit
entry per row would create a second event stream an order of magnitude larger than the data being
audited — the failure D5 already rejected for health telemetry. Instead each run writes **one** audit
row naming the tables, the partition range, the row counts, the destination account and container, and
the credential identity used; the export's `_manifest.json` is the receipt and the audit row points at
it.

**The honest limit, stated plainly:** once the Parquet files are in the customer's storage, reads of
that copy are outside this product's audit boundary. The audit records what left, when and to where;
further access control is the customer's own, which is the point of brief §3.6's "the customer's own
storage". Content is not in the v1 export (§9.6) and the search index is not either (§9.4, 06 §6.5), so
this path never carries content and never interacts with C16's approval requirement.

---

## 6. k-suppression and small-cell rules

### 6.1 Why a small cell is personal data

A per-team, per-day count for a team of two identifies those two people — not statistically, but
directly: the analyst already has the org chart, so "3 submissions from Legal" in a two-person Legal
team is a per-person fact. The same holds for a class cell contributed by one person and a tool cell
used by one person. `mart.agg_class_period` carries `users` for exactly this reason, and the schema's
comment on it states the disposition: a small cell "is subject-level data for audit purposes and must
be suppressible".

### 6.2 The rule

- **k = 5** distinct subjects per cell. **ASSUMPTION:** the brief gives no threshold; 5 is the
  conventional small-cell floor, comfortably below typical team size at a 500–5,000-employee customer
  so it does not blunt the product, and above it a cell no longer resolves to individuals for a team an
  analyst can enumerate.
- k applies to **distinct subjects**, not rows: nine hundred submissions from two people is suppressed.
- A suppressed cell suppresses **every measure in it** — `submissions`, `bytes_total`, `max_score` —
  because the byte total of a two-person cell reveals nearly as much as the count.
- **Complementary suppression.** If a response contains exactly one suppressed cell and a published
  total over the same dimension, the total is suppressed too; otherwise the total minus the published
  cells recovers the hidden value. Applied per response, in `query-api`.
- Suppression is a **read-path** rule: nothing is removed from `mart`, so a suppressed cell can become
  publishable later while the aggregate stays correct.

### 6.3 How it is reported

```json
{ "bucket_start": "2026-10-01T00:00:00Z", "department": "Legal",
  "result_state": "suppressed", "reason": "fewer_than_k_subjects", "k": 5 }
```

A suppressed cell renders as a distinct hatched state with the reason available — never zero, never
null, never blank. **A suppressed cell and a genuine zero are different facts and must never be
merged**: `0` means "we looked and there was none", `suppressed` means "there was something and we are
not telling you the number". This is brief §3.2's "states that must never be merged" applied to a
number, and an analyst who cannot tell them apart will conclude a team is quiet when the system is
silent — C25's failure mode, in the UI.

### 6.4 Where it applies

- **Applies** to every aggregate cell and every grouped read, including department and class views.
- **Does not apply to explicitly subject-scoped reads** — a list filtered by `user_ref`, or a single
  detail. Those answer questions about a named person, which is the product's purpose (Q6, Q8, Q9); the
  controls there are authorisation and audit, not anonymity. Suppressing them would make three of the
  ten questions unanswerable while adding no protection.
- **Does not apply to the bulk export** (§9): the customer's own data, in their own storage, under
  their own credentials, role-gated and receipted. Different audience, different control.
- **Does not apply to content search** (§15): a search returns individual events rather than cells, and
  its result count is a fact about matched content, not a measure of a group. Suppressing it below k
  would make search unusable in exactly the case it exists for, which is the reasoning of the first
  bullet above. Its controls are the tier, the scope narrowing, the role, the snippet bound and the
  audit entry — not k.

---

## 7. Cursor pagination

Brief §3.6 requires **cursor pagination only** and forbids unbounded result sets over the event table
(C29).

### 7.1 Ordering keys

| Read path | Ordering key (total) |
|---|---|
| Event list (Q8) | `(received_at DESC, submission_id DESC)` |
| Finding list (Q5) | `(detected_at DESC, submission_id DESC, rule_id ASC)` |
| Audit list (Q10) | `(occurred_at DESC, audit_seq DESC)` |
| Device list (Q7) | `(device_id ASC, collector ASC)` |
| Subject-by-tool (Q2) | `(bucket_start DESC, tool_fingerprint ASC, user_ref ASC)` |
| Aggregate series (Q1–Q4, Q6) | `(bucket_start DESC, <caller's order terms>, <grouping keys> ASC)` |
| Content search (§15.3) | `(rank DESC, received_at DESC, submission_id DESC)` |

Every key ends in columns unique for the tenant, so the ordering is **total**: two rows can never
compare equal, which is what makes a keyset page exact rather than approximately right.

### 7.2 Encoding

Two forms, and the client cannot tell which it received:

- **Self-contained** (no subject data in the ordering key — event, finding, audit, device lists): a
  base64url token over canonical JSON signed with HMAC-SHA256 under a per-deployment key:
  `{ v, tenant, dsl_hash, order: [...], upper: <snapshot bound>, exp }`. The signature makes it
  unforgeable; `dsl_hash` binds it to the exact normalised query so it cannot be replayed against a
  different question; `tenant` binds it to the session, and a cursor presented under another tenant is
  rejected.
- **Server-side** (the ordering key contains `user_ref` — the Q2 and per-subject aggregate cursors): a
  random opaque id resolving to a stored resume position, never a client-held encoding of a subject
  reference. Cursors are treated as subject-level data: not logged in full, not echoed in errors,
  expired after **15 minutes**. **ASSUMPTION:** the brief fixes neither a lifetime nor a storage
  strategy; fifteen minutes is longer than any human pagination and short enough that a leaked cursor
  is not a durable grant.

An expired, unknown or version-mismatched cursor returns `cursor_expired` and the client restarts from
page one, told why. Silent restart at an offset is the failure this avoids.

### 7.3 Stability under concurrent inserts

- **The window is frozen at first page.** `snapshot_upper_bound` is the maximum `received_at` visible
  when the first page was served; every later page carries `WHERE received_at <= :upper`. Rows are
  appended at the head of a `DESC` ordering, so without the freeze an insert after page one would shift
  boundaries and a row would be seen twice or skipped. Frozen, it can do neither.
- **Newer data is announced, not hidden.** `newer_events_exist` (a bounded `EXISTS` on the same index)
  says rows have arrived since the snapshot, so a consistent-but-stale page is not mistaken for the
  whole truth.
- **Search results are frozen on the index's own clock.** A search's snapshot bound is
  `ingest.search_text.created_at <= :upper`, taken at the first page, so a unit indexed mid-iteration
  cannot shift a boundary; newer units are announced by the same `newer_events_exist` test. Units
  shredded mid-iteration disappear, which §7.4 already covers.
- **Content can change; membership cannot.** `ingest.submission` is immutable in its ordering columns,
  but a later higher-fidelity route upgrades labels, confidence, mode and winning route in place. The
  contract is therefore stability of *membership and order*, not of row content: a row may render
  differently on page 2 than it would have on page 1, and the upgrade is explainable because
  `winning_source`, `observed_routes` and `merge_confidence` travel with it (R9).

### 7.4 When rows change or disappear mid-pagination

Deletion is the only mid-pagination mutation — retention expiry and erasure delete whole rows, and
nothing updates an ordering column — so a deleted row makes the affected page **shorter**, possibly
empty. Therefore **a short page is not the end of the results**: only `next_cursor: null` ends an
iteration, and the client is explicitly specified not to stop on `returned < page_size`, because doing
so would truncate a result set whose rows were erased underneath it. An erasure mid-pagination also
recomputes the affected aggregate buckets (§4.3), so a chart and a list read seconds apart during an
erasure can differ — correct, because the aggregate has been told to forget, and `last_run_at` moves.

### 7.5 Page size bounds

| Path | Default | Maximum | Over the maximum |
|---|---|---|---|
| Event, finding, audit, device lists | 50 | 500 | `query_too_broad`, bound stated |
| Aggregate response | — | 2,000 cells | `query_too_broad`, naming the coarser bucket that fits |
| Time series | — | 400 points | Auto-coarsened when the server chose the bucket, applied bucket named in `meta.applied_bucket`; `query_too_broad` naming the bucket that fits when the caller pinned one |
| Response body | — | 8 MB | `query_too_broad`; never silent truncation |

---

## 8. Content retrieval through the approved path

Question 9 is the one question that touches content, through exactly one route.

### 8.1 The flow

1. **The analyst finds the event** (Q5, Q8) and sees `content_state = 'uploaded'`. Nothing in that read
   returns content.
2. **A retrieval request is raised** by an `investigator`: one submission, a mandatory `case_reference`,
   and a reason. There is no bulk request and no multi-select that becomes one — C14 makes a grant per
   event and single-use, and `ops.grant` has no bulk path, which is why the "upload everything" state
   (C5) is structurally unreachable rather than merely disallowed.
3. **A second approver decides**, and must not be the requester (C16). Master doc Q9 leaves open whether
   that approver is customer-side or vendor-side; the API carries `approved_by` and works either way.
4. **The audit entry is written before content is returned.** `query-api`, which has no `SELECT` on
   `ops.content_object` and so cannot see a wrapped key even in principle, calls `content-vault` with
   the case reference and approval evidence. `content-vault` writes the audit row
   (`action = content.reveal`, with object, `subject_ref`, `case_reference`) and commits it **before**
   unwrapping the data key; if that insert fails, the failure is returned and no content is released.
5. **Content is returned once**, as a single-object short-lived read; re-reading requires a new approved
   request, separately audited.
6. **Under a hold** (C35), content that would have expired is still retrievable and the record shows
   that the hold is why — a hold that silently extends retention is indistinguishable from a bug.

**As built:** steps 1, 2, 4 and 5 run in the local auth lab, from the dashboard's Explore page. Its event
panel offers the request when `content_state` is `uploaded`; `query-api` forwards
`POST /v1/content/retrieval` to `content-vault`, which records the request, commits the audit rows
(`content_retrieval_requested`, `_granted`, `_redeemed`, or `_refused`) before anything is read, issues a
single-use grant and redeems it. Four things differ from the flow above. **Step 3 is not a decision:** the
second approver is a name recorded on the request, which the vault requires to differ from the requester;
no second person approves, and there is no `approval_pending` state. The roles of §4 are not enforced,
because the requester is a development principal rather than an authenticated session. The content is
relayed in `query-api`'s response body, because the vault does not mint a retrieval URL yet. And holds
(step 6) are not consulted. The page shows what the user typed apart from the rest of the capture, and
drops retrieved content when the event is closed.

### 8.2 Result states

| State | When | What it says |
|---|---|---|
| `ok` | Approval recorded, object present and unwrappable | The content, plus the audit entry id |
| `no_longer_available` | `content_state = 'shredded'` | Explicitly gone **with the reason** — `retention_expired`, `erasure`, `hold_released`, `tenant_offboarded` — and the erasure receipt where one exists. Never an empty result (C17, brief §4.4) |
| `not_captured` | The mode never permitted reading content | There was never content to retrieve — a different fact from content destroyed (brief §3.2) |
| `local_only` | Content was taken and remains on the device | Not retrievable in v1: no device-facing pull endpoint exists (master doc §5.1) |
| `approval_pending` / `approval_denied` | Awaiting, or refused by, the second approver | Whose decision, and when |
| `key_unavailable` | Tenant key disabled, or Key Vault unreachable | Metadata and dashboards keep working; the outage is alerted separately from data loss (master doc §4.4) |
| `audit_unavailable` | The audit row could not be committed | Fail closed: no content, no exceptions |

Brief §8's "content retrieval once granted: < 30 s" is measured **from the moment the second approval is
recorded to the first content byte returned**; the request→approval interval is human latency, reported
separately as a case metric, because folding a person's response time into an engineering target hides
a real regression. **ASSUMPTION:** §8's "once granted" supports this reading, and no other reading is
measurable.

### 8.3 The only route to full content

Stated flatly, because it is the product's promise: **there is no query path that returns full content.**
No endpoint accepts a digest and returns bytes, no search returns a unit, no v1 export contains content.
Content search (§15) is not an exception to that sentence — it returns **bounded highlighted snippets**
of the units that match, through `content-vault`, under the tier and scope rules of §15 and with an
audit row written before it runs. Because snippets *are* content, the honest form of the old sentence
changes: what an analyst can see without this flow is classification labels with scores and rule ids,
the event's metadata, the bounded fragments a `full_text` search returns, and — at M2 only — the
minimised `content_excerpt` that policy decided may travel (brief §1.1). What has not changed is that a
whole prompt or an attachment body is reachable only here. `content-vault` has internal-only ingress and
no user-facing endpoint; neither devices nor browsers can reach it (D7).

---

## 9. Export

Brief §3.6: bulk export to the customer's own storage in a columnar format, on a schedule (C31).

### 9.1 Format and layout

**Parquet** (C31's columnar format; the export's shape is unchanged by ADR 0014 — 06 §6.5), Snappy, one
row group per ~128 MB, UTC timestamps, and `schema_version` as a column on every row so a reader knows
which contract version a file conforms to.

```
<container>/tenant=<tenant_id>/table=<name>/dt=<YYYY-MM-DD>/part-00000.parquet
<container>/tenant=<tenant_id>/table=<name>/dt=<YYYY-MM-DD>/_manifest.json
<container>/tenant=<tenant_id>/_export_state/<name>/dt=<YYYY-MM-DD>/_SUCCESS
```

Hive-style partitioning on `dt`, derived from `received_at` — the authoritative server clock — so a
partition means "what the system learned that day" and a customer's `WHERE dt = …` is stable. Partition
pruning is the point: their engine reads the columns and days it needs.

### 9.2 Destination and credential delegation

- The destination is **a storage account the customer controls**. The vendor has no default location; if
  it is unreachable or revoked the export fails and alerts, with **no fallback to vendor-controlled
  storage**.
- Credentials are delegated, never shared: the recommended shape is a federated workload identity into a
  multi-tenant Entra application the customer consents to, granted `Storage Blob Data Contributor`
  scoped to one container prefix. **No long-lived secret is stored** — nothing to leak or rotate on the
  vendor side — and revocation is a visible, alerted event rather than a silent gap.
- Where a customer will not consent to an application, the fallback is a short-lived user-delegation
  SAS, rotated by the job and held in Key Vault, scoped to the same prefix.
- **ASSUMPTION:** the brief says "the customer's own storage" without naming a cloud; Azure Blob is the
  v1 target because the platform is Azure (S1), behind a storage-writer interface so an S3-compatible
  target is a later implementation rather than a redesign.

### 9.3 Schedule, idempotency, re-runnability

- **Daily** at a fixed UTC hour for `dt = yesterday`, re-exporting the trailing **7 days**; **monthly**
  for the small reference tables; an on-demand run for a bounded range, role-gated and audited.
- **Deterministic file names**: a run writes `part-00000.parquet` for a given `(table, dt,
  export_version)`, so re-running overwrites the same object and never appends. Runs are idempotent by
  construction and a failed run is fixed by repeating it.
- **Atomic publication**: the object is written to a temporary prefix and committed as a single
  block-blob commit; `_manifest.json` is written **last** with row counts, the `[min, max]` of
  `received_at`, a row-set hash and the export code version; `_SUCCESS` marks the partition readable, and
  customer tooling globs on it so a half-written partition is never read.
- **The 7-day window is not padding.** Submission rows can be upgraded in place when a higher-fidelity
  route reports the same submission (R9), so yesterday's export can legitimately differ from today's
  view of yesterday; the re-export covers that window and the manifest's row-set hash makes the change
  visible rather than mysterious.

### 9.4 Contents

| Table | Rows | Notes |
|---|---|---|
| `submission` | The logical, deduplicated fact | Both clocks, mode, labels, policy action, `content_state`, `merge_confidence`, winning route, `observed_routes` |
| `observation` | Per route | So the customer can reconcile an overlapping-route count, not just accept it (R9) |
| `finding` | Rule hits | With severity and rule id |
| `device`, `collector_state` (daily), `coverage_snapshot` | Fleet and coverage state | Including `gap_reason`, so they see the gaps the product sees |
| `tool` | Fingerprints and sanctioned state | Present-tense state, labelled as such |
| `user_dim` | Department, population, manager ref, status | `directory_object_id_enc` is **never** exported |
| `audit` | The tenant's audit trail | Complete for the tenant; platform-level rows live in the platform log |
| `data_class`, `rule` (monthly), `aggregate_watermark` | Reference and freshness | So they can tell when a partition was computed |

Not exported: `ops.content_object` and blob ciphertext (no keys ever leave), `ingest.search_text.body`
and `.tsv` (the index is a plaintext-derived copy of content; shipping it would put content into
customer storage under a second name and a second lifecycle — 06 §6.5), `ingest.rejected`
(short-TTL, content-stripped), and the `mart` aggregates themselves — they are derived from
`submission`, which is the thing worth exporting.

### 9.5 The consequence, stated plainly

> **The export is the customer's own analytics, not the product's search.** A customer who wants their
> own SQL over the data, or a searchable copy in their own environment under keys they hold, runs it
> against storage they control, with their own tooling.

That is brief §3.6's requirement, and it is no longer carrying a second job. Under ADR 0008 the export
*was* the product's answer to R10 — the only way anyone could search content — and the export's value
does not depend on that: a supported path for "one customer in ten will run their own SQL" removes a
category of feature request whether or not the product also offers search. What changed is the framing,
and one consequence inside it:

- **For a `vendor` or `customer_managed` tenant**, content search is now a product capability (§15), so
  the export is complementary: their own copy answers questions the dashboard does not, and the two do
  not compete.
- **For a `customer_held` tenant**, the export is still the whole answer for content search, because
  `full_text` is unrepresentable alongside that custody mode (§15.1). That is not an oversight in the
  tier: those tenants keep `attachment_names`, structured filtering and per-event approved retrieval,
  and the only way anyone searches their prompt text is with keys the vendor never holds.

**What v1 does not include: content.** So the export is complete for metadata and label search and
**incomplete for cross-content search** — which for a `vendor` or `customer_managed` tenant is a gap in
the customer's own analytics rather than in the product, and for a `customer_held` tenant is the gap
§9.6 exists to close. A customer who bought on the export-as-search promise needs §9.6 resolved, and the
product must say so rather than imply otherwise.

### 9.6 Open question: content in the export (master doc Q11)

Master doc Q11 records content-in-the-export as undecided, and 06 §6.5 asserts metadata-and-labels only
for v1. This document adds the constraint that decides it:

- **Under `key_custody = 'customer_held'`, a content-inclusive export may not be optional.** If the
  vendor cannot unwrap, no vendor-side UI can ever render content, and the only way the customer can
  read content they granted (C14) is to receive the ciphertext with the wrapped data key and unwrap it
  themselves. Not shipping that makes customer-held keys a mode in which content is collected and never
  readable — which cannot be what brief §3.3 intends. **ADR 0014 sharpens this rather than softening
  it:** because `full_text` is unrepresentable alongside `customer_held` (§15.1), the customer's own
  copy is the only place that tenant's prompt content can ever be searched.
- **The mechanism does not weaken the key model.** At customer-held custody, exporting the blob plus
  `wrapped_dek` with its `kek_id`/`kek_version` hands over something the vendor cannot read, and there is
  no index on that tenant to leak alongside it (§15.1). The vendor's ciphertext copy is removed by the
  ordinary retention path, as it always was.
- **Under `vendor` or `customer_managed` custody the answer is no by default**: exporting plaintext
  content into a customer storage account under the vendor's identity expands the breach surface the
  architecture exists to keep small — and the argument that it substitutes for a missing capability is
  gone, because those tenants can search server-side at `full_text`.
- **The index is not a shortcut to any of this.** `body` and `tsv` are never exported (06 §6.5), so a
  customer-held tenant's searchable copy is built by them, inside their environment, from content they
  can decrypt.
- **This must be pinned before the export ships**, because it is a key-model question and R10 says the
  key model cannot be retrofitted. If `customer_held` in fact means "the customer may delegate a
  time-boxed unwrap capability to the vendor", the export does not need content and this becomes a
  simple no — but that reading must be written down, not assumed.

**This is the largest open item this document surfaces:** until it is answered, §9 and §8 are internally
consistent but the customer-held-key path has no way to deliver content to its owner — and therefore no
route to content search for the tenant whose keys we must not be able to use.

---

## 10. Subject export

Brief §4.5: a per-subject export covering that subject's events and any stored content, in a documented
format, within a bounded time (C18). It is a **job**, not a query: ad-hoc queries cannot produce a
complete, receipted artefact across `ingest`, `mart`, `ops` and possibly Blob storage.

**States:** `requested → authorised → running → sealed → delivered → expired`, plus `failed` and
`delivered_partial`. Every transition writes an audit row; the manifest is the receipt.

**Authorisation.** Requested by `privacy_officer` or `tenant_admin` with a recorded reason and a case or
lawful-basis reference — the reason is what makes it an accountable act rather than a data dump. **A
second approver is required when the export includes stored content. ASSUMPTION:** §4.5 does not require
one, but C16 does for single-object retrieval and a subject export is a bulk content read; applying the
weaker rule to the larger artefact would make C16 trivially avoidable. The job runs as a job identity,
never as the requester, so it cannot launder permissions.

**Contents and format** — a sealed directory documented by its own `README.md`:

| File | Contents |
|---|---|
| `events.jsonl` | Every `ingest.submission` for the subject, then its `ingest.observation` rows, one JSON object per line, each self-describing via `schema_version` |
| `findings.jsonl` | `mart.finding` rows with `ops.finding_review` state |
| `audit.jsonl` | `ops.audit` rows for that `subject_ref` |
| `content/` + `content_manifest.json` | Stored content, only when included: digest, plaintext size, mode, `content_state`, `shredded_reason` |
| `manifest.json` | Counts per table, filters applied, a hash over the contents, generator version and time, **and what was excluded and why** |

The exclusions are what make it usable: an export that silently omits shredded content, held rows, or
M0/M1 records where no content was ever read is indistinguishable from one that lost data. Each excluded
category is named with its reason — the same discipline as C34's receipt, because a claim of
completeness without an account of what was left out is not usable. The manifest also **resolves
`ingest.search_text`**: whether the subject has indexed units, and whether the tenant's tier means none
could exist, because "no indexed content for this subject" and "indexing is off for this tenant" are
different facts (06 §11.3). The index's `body` and `tsv` are never shipped as an artefact of their own
(§9.4, 06 §6.5) — the subject's content travels by the content path, and the manifest records which of
the two it came from. Erasure is the case where the index is deleted rather than reported (§15.5).

**Bounded time.** Target p95 ≤ 4 hours, hard bound 24 hours. **ASSUMPTION:** §4.5 requires "within a
bounded time" without a number; the bound is achievable because a subject's events number in the
hundreds to low thousands (D1) and content is capped by `ops.tenant.content_budget_bytes_per_day` — the
mechanism that bounds the only unbounded cost in the system. If the job cannot finish inside the hard
bound it fails loudly with the reason and never delivers a partial artefact marked complete.

**Delivery.** Written to the customer-controlled destination of §9.2 under the same delegation, never
emailed, never left at a public URL. Where an interactive download is genuinely needed it is a single
short-lived authenticated URL (≤ 15 minutes) issued to an authenticated session; the artefact expires on
the destination under the tenant's retention policy.

---

## 11. Dashboard information architecture

### 11.1 The landing page is Posture, not "Top tools"

The default screen is **Posture**: coverage, freshness, devices not reporting, degraded collection, and
the watermark for every aggregate the other screens read. The tool ranking and volume trend sit directly
beneath it, so the first thing an analyst sees is the health of the instrument. The rejected alternative
— landing on "top AI tools" — presents a number whose denominator is unknown; with 70–85% management
coverage as the planning assumption (brief §5.5) and R11 requiring gaps to be measured, it would be the
most confident-looking screen and the least trustworthy one. Master doc §1.2's honesty property is a UI
requirement: the caveat is not a page, it is the frame.

### 11.2 Screens

| Screen | Answers | Primary source | Subject-level | Roles |
|---|---|---|---|---|
| **Posture** (landing) | Coverage, freshness, gaps | `ops.coverage_snapshot`, `ops.aggregate_watermark`, `mart.v_device_liveness` | No | all |
| Tools | Q1 | `mart.v_tool_usage` | No | all |
| Unsanctioned | Q2 | `mart.v_tool_usage` → `mart.agg_tool_user_period` | Yes | analyst+ |
| Classes | Q4 | `mart.agg_class_period` | Cells below k | all |
| Teams | Q3 | `mart.agg_org_period` | Cells below k | all |
| Person | Q6 | `mart.agg_user_period` | Yes | investigator+ |
| Findings | Q5 | `mart.v_finding` | Yes | analyst+ |
| Activity | Q8 | `ingest.submission` | Yes | analyst+ |
| Event detail / retrieval | Q9 | `ingest.submission`, then §8 | Yes | investigator+ |
| Content search | Q9 first half, Q5, Q8 | `ingest.search_text` via `content-vault` (§15) | Yes — bounded snippets | analyst+ |
| Devices | Q7 | `mart.v_device_liveness`, `ops.collector_state` | Device, not person | all |
| Degraded collection | brief §7, R11 | `ops.collector_state`, `ingest.rejected`, `ops.reconciliation_run` | No | all |
| Audit | Q10 | `ops.audit` | Yes | auditor |
| Exports | §9, §10 | watermarks, manifests | Yes | tenant_admin, privacy_officer |
| Settings | Modes, retention, holds, directory sync, destination | `ops.*` | No | tenant_admin |

**Person is a lookup, not a list.** It is reached from a finding, a case, a content search (§15), or a
lookup for a known `user_ref`; there is no screen that enumerates people sorted by volume and no column
that ranks them, and a search ranks documents by relevance rather than people by volume (§15.3). Brief
§1.2's exclusion of per-employee scoring and ranking is a non-goal this screen is the most likely way to
violate, so it is designed as a lookup rather than as a leaderboard with the sort removed.

### 11.3 Coverage and freshness are never buried

Coverage renders as a persistent strip on every screen: devices reporting of devices enrolled, expected
vs observed collectors, and the `gap_reason` breakdown (`not_enrolled`, `not_managed`,
`client_bypassed_proxy`, `pinned_certificate`, `permission_denied`, `process_excluded`, `tampered`,
`unknown`). `unknown` shows as a reason, not a blank — blank and unknown look identical on a dashboard
and mean different things. Freshness renders **inside each metric tile** (`updated 3 min ago · complete
to 10:00`), never only in a tooltip; because the API always returns the `freshness` block, a tile cannot
render without it unless the UI discards data it was given. Where a window has no coverage the chart
renders `not_yet_covered` hatching rather than a zero line, and a trend spanning a coverage gap renders
the gap as a break in the series.

### 11.4 "Devices not reporting" and "degraded collection"

**Devices not reporting** is a first-class view over `mart.v_device_liveness` joined to
`ops.collector_state`, sorted by silence duration and `spool_dropped_total`. Its four liveness states
stay distinct, and its central claim is C24's: an absence of events is ambiguous, a health signal is
not. A device that has gone silent is a **row produced by the server**, not a gap an analyst must
notice.

**Degraded collection** collects everything the collection paths report about themselves: per-collector
state counts, spool depth and dropped totals (C22's visible undercount), the `degraded`-confidence share
(C21 — a classifier that did not finish must never read as "nothing sensitive found"), `ingest.rejected`
reason histogram (so an agent defect is diagnosable without the device), the low-merge-confidence share
(R9's "N observations we cannot merge"), per-device clock skew (C26), index expiry drift — index rows
surviving their `expires_at`, which is how the third copy of content is kept inside the same
reconciliation as the other two (§15.5) — and reconciliation drift
(`ops.reconciliation_run.drift_found`, recorded and alerted rather than auto-corrected). Every item is an
output of the collecting path, which is what R11 asks for.

---

## 12. Query safety

### 12.1 Limits

| Query class | Timeout | Row / cell cap | Notes |
|---|---|---|---|
| Aggregate read | 3 s | 2,000 cells | Brief §8 wants < 2 s p95; alert at p95 > 1.5 s |
| Event / finding list | 5 s | 500 rows | Cursor-paged, window ≤ 31 days |
| Single record | 3 s | 1 | |
| Content search | 5 s | 200 hits, ≤ 480 snippet characters per hit | Executes in `content-vault`; a narrowing predicate is mandatory; the coverage count runs over the same predicate |
| Audit read | 5 s | 500 rows | Window ≤ 366 days |
| Operational / device | 5 s | 500 rows | |
| Export planning | 10 s | — | Planning only; the export is a job |
| Subject export | 60 s per stage | — | A job with its own state machine (§10) |

**As built:** the per-class timeouts are carried on each compiled query as
`meta.statement_timeout_ms` but are not applied per statement. What bounds a statement today is one
session-level `statement_timeout`, set when the connection starts — 10 s by default
(`SAC_PG_STATEMENT_TIMEOUT_MS`).

### 12.2 The cost guard: rejection, not degradation

- **The DSL is closed, so cost is knowable before execution.** Each query shape declares a cost class;
  the guard multiplies requested cells by grouping cardinalities and refuses over-budget requests
  **before** the statement runs. No runtime `EXPLAIN`: planner estimates are unreliable for exactly the
  queries that need guarding, and an `EXPLAIN` of a pathological query still costs.
- **No shape may be served by a sequential scan of `ingest.submission`.** Every list shape declares the
  index it requires; if the submitted filter combination is not covered, the API returns
  `unsupported_query_shape` **naming the filters that would make it servable** rather than falling back
  to a scan. This is affordable only because the DSL is closed, and it is the strongest safety property
  in this document.
- Over-budget requests never degrade silently: `query_too_broad` names the coarser bucket, the narrower
  window, or the dimension to drop.
- **A content search is bounded by its narrowing predicate, not by a scan.** The text match is
  index-served — the `tsv` GIN for terms and phrases, the partial trigram GIN for substrings and
  filenames — and the mandatory scope predicate bounds the candidate set before anything is ranked. A
  shape with no covering index (a regular expression, a leading wildcard over an unindexed expression, a
  text match with no scope) returns `unsupported_query_shape` naming the fix; there is no fallback to a
  sequential scan of `ingest.search_text` or of `ingest.submission`.

### 12.3 Concurrency, shedding, cancellation

- **Per tenant 8 concurrent statements, per user 4.** The audit hash chain serialises per tenant (§5.1),
  so per-tenant concurrency is the limit that matters; the global pool is capped at ~40 connections
  pending verification of the instance ceiling (master doc Q12).
- **A bounded queue of 32**, and beyond it an immediate `busy` (429) — never unbounded queueing, which
  turns one tenant's pathological query into every tenant's latency. If a shape's p95 exceeds budget,
  that shape is shed for that tenant and returns `busy` with a retry hint.
- **Cancellation.** `statement_timeout` bounds every statement server-side;
  `idle_in_transaction_session_timeout` bounds a client that vanishes mid-transaction; a disconnect
  issues a driver-level cancel. Because subject-level responses are not streamed (§5.1), a cancelled
  read has served nothing — the property that makes cancellation safe here.
- **As built:** admission is one process-wide gate — 8 concurrent requests and a queue of 32
  (`SAC_MAX_CONCURRENCY`, `SAC_MAX_QUEUE`), `busy` (429) beyond it — keyed by neither tenant nor user,
  so the per-tenant and per-user limits above are not yet enforced separately. The pool is capped at
  40 connections (`SAC_MAX_CONNECTIONS`). `idle_in_transaction_session_timeout` is not set, and
  per-shape shedding is not implemented.
- **Pathological examples and their answers:** a four-year, tenant-wide trend with the caller
  pinning `day` → `query_too_broad`, suggests week or month (with no bucket supplied the server
  coarsens instead, §7.5); a `user_ref` prefix search across all users →
  `unsupported_query_shape` (`starts_with` is permitted only on `tool`); a filter combination with no
  covering index → `unsupported_query_shape` with the fix; a multi-select of 500 events for content
  retrieval → `unsupported_query_shape`, because retrieval is per event (C14); a text match with no
  scope predicate → `unsupported_query_shape`, naming the tool, class or population that would make it
  servable; a prompt-body search on an `attachment_names` tenant → `content_search_not_enabled`,
  naming the tier rather than returning an empty page (§13).

---

## 13. Error semantics

Every response carries `result_state`; HTTP status distinguishes "the request failed" from "the answer is
not a number", and the client must render the difference.

| `result_state` | HTTP | Meaning | UI |
|---|---|---|---|
| `ok` | 200 | A real answer | The data |
| `empty` | 200 | **We looked, coverage was adequate, there is nothing** | "No data" — an answer, not an error |
| `not_yet_covered` | 200 | Window predates collection, or the dimension has no source (§3.3) | Hatched, with reason (`directory_not_synced`, `before_enrolment`) |
| `stale_aggregate` | 200 | Watermark older than 3× cadence | Data **with** its age and a warning treatment |
| `coverage_degraded` | 200 | The value is a floor, not a total | Data with the gap share, link to devices; on a search, the index-coverage block (§15.3) |
| `suppressed` | 200 | Fewer than k subjects in the cell | Hatched cell, `k` and reason available (§6.3) |
| `no_longer_available` | **410** | The record or its content existed and was destroyed | Explicit, with `shredded_reason` and the receipt |
| `not_captured` | 200 | The mode never permitted reading content | "Never collected at this mode" |
| `not_retrievable` | 200 | `local_only` content, not centralised | "Content remains on the device" |
| `not_found` | 404 | No such record, **and** no purge covered its window | "No such record" — see below |
| `unsupported_query_shape` | 400 | Filter combination not servable | The fix, named |
| `query_too_broad` | 400 | Over budget or over a cap | The coarser bucket or narrower window that fits |
| `cursor_expired` | 400 | Cursor expired, mismatched, or from another query or tenant | Restart from page one, told why |
| `audit_unavailable` | 503 | The audit row could not be written | **No data at all** — "read not served" |
| `key_unavailable` | 503 | Tenant key disabled or Key Vault unreachable | Metadata unaffected; content temporarily unretrievable |
| `busy` | 429 | Shed by concurrency or circuit breaker | Retry hint |
| `unauthorised_role` | 403 | The role cannot make this read | Which role is needed |
| `content_search_not_enabled` | 403 | The tenant's tier, or the scope asked for, does not include this search — a prompt-body match at `attachment_names`, anything at `disabled`, a scope the signed bundle does not name | The tier that would enable it and who can change it — never an empty result |
| `audit_chain_broken` | 500 | A page's hash links do not verify | An integrity alert, not a list |

**Why `empty` and an unavailable state must never be the same response.** Brief §4.4 requires an expired
or erased record to return an explicit "no longer available", *not* an empty one, and the reason
generalises to every read path. `empty` asserts something true and useful: we were watching and nothing
happened. `not_yet_covered`, `coverage_degraded`, `stale_aggregate` and `no_longer_available` assert that
the system **cannot say**, and collapsing them into an empty result turns a blind spot into a fact. A
search is where that distinction has the shortest distance to travel: `empty` means the index covered
the scope and matched nothing, `coverage_degraded` means it matched inside a scope that is only partly
indexed, and `content_search_not_enabled` means we cannot search this at all — and an analyst shown the
first when the third is true has been told the tenant is clean. That is C25's failure mode — a path
failing into a state that reports success — expressed as an API contract, and a security product that
reports a clean bill of health while collecting nothing is worse than no product at all.

**The `not_found` rule, stated because it is the subtle one.** Event rows are deleted by retention expiry
(D1, C34), so a missing submission id is ambiguous between "never existed" and "existed and was purged".
The API resolves it: if no retention run and no erasure receipt covers the window in which the record
would have been received, the answer is `not_found`; if one does, the answer is `no_longer_available`
with the reason and the receipt. A bare 404 for a purged record would be the merge C17 forbids.
**As built:** only the erasure half is checkable — `ops.erasure_receipt` exists and is consulted, but
`database/schema.sql` has no retention-run ledger, so a record purged by retention expiry cannot be
told from one that never existed. That case returns `not_found` carrying
`detail.retention_evidence: "no_ledger_in_schema"`, and `purge_window_unknown: true` when the request
gave no `received_at_hint`, rather than guessing.

**No metric without its state.** A response carrying `data` must carry `freshness` and `coverage`; there
is no code path that returns one without the others. Encoding that in the envelope is deliberate — a UI
cannot forget a field it was never allowed to omit.

---

## 14. What the dashboard must never do

1. **No per-employee scoring, ranking or efficiency reporting** — no volume leaderboard, no productivity
   metric, no peer percentile, no "most active users" table (brief §1.2).
2. **No content search outside the tier** — no search at a `disabled` tenant, no prompt-body search at
   `attachment_names`, no search over attachment *contents* or over the M2 excerpt at any tier, no digest
   lookup that returns text, and never `full_text` on a `customer_held` tenant: that pair cannot be
   written to a row at all (§15.1). Search returns bounded snippets; it never returns a unit.
3. **No full content returned by any query path.** Bounded snippets are the deliberate and documented
   exception (§15.3), and they are audited; a whole prompt, an attachment body or an unwrapped key still
   requires the approved, case-referenced, second-approved, audit-first flow (§8; C14, C16).
4. **No cross-tenant aggregation** — not for benchmarks, not for "industry averages", not for anonymised
   statistics (brief §3.3, C32).
5. **No unauthenticated or non-expiring export links**, no export by email, no export to
   vendor-controlled storage (C31, §9.2, §10).
6. **No aggregate without its freshness and coverage state** (master doc §1.2, brief §8).
7. **No suppressed cell shown as a zero**, and no zero shown as suppressed (§6.3, brief §3.2).
8. **No merging of the state pairs** the brief lists — `local_only`/`uploaded`/`shredded`,
   `sanctioned`/`unsanctioned`/`unknown`, `blocked`/`warned`/`logged`,
   `healthy`/`degraded`/`absent`/`tampered`, `disputed`/`confirmed` (brief §3.2); nor `open` merged into
   either review state, nor `revoked` with `stale`.
9. **No inferred health** — "no events" must never render as "healthy" (C24, C25).
10. **No clock-skew normalisation** — both clocks shown, neither silently corrected (C26).
11. **No chart computed by scanning raw events** (C27).
12. **No response-side capture in v1** — no ingress view, no seam exposed as a setting (brief §1.2).
13. **No configuration change without an audit entry** — modes, retention, holds, destinations, role
    grants (brief §1.1, §3.2, C4).
14. **No silence presented as coverage** — the enrolled denominator is always on screen, and no
    fleet-wide percentage the product cannot compute is displayed (§3.7, R11).
15. **No unrecognised filter silently ignored** — an unknown dimension is an error, not a filter that
    quietly did nothing (§2.4).
16. **No content search without its audit entry** — written in the same transaction that serves the
    results, failing closed; a search that cannot be proved to have happened does not happen, and a
    search that returns nothing is still a search (§15.4, C30).
17. **No search result without its index coverage** — a hit count over a partly indexed scope is a floor
    and renders as one, with the reasons (`not_captured`, `local_only`, `shredded`), because the
    alternative is a clean-looking answer over content the product never had (C25, §15.3).

---

## 15. Content search

Structured filtering is always available; text search is available to the extent the tenant's
`content_search` tier permits, and never beyond it — because the two incompatible values cannot both be
written to a row. This section is the whole capability: what each tier enables, where the text lives,
what an analyst types, where the query runs, what it writes to the audit log, how it expires, and what a
customer has to be told before turning it on.

### 15.1 The three tiers, and the invariant that bounds them

`ops.tenant.content_search` is a per-tenant ceiling (master doc D6 as revised by
[ADR 0014](adr/0014-content-search-is-a-per-tenant-capability.md), which supersedes ADR 0008):

| Tier | Adds | Key custody permitted | Mode required |
|---|---|---|---|
| `disabled` | nothing; structured filtering only — tool, user, date, class, rule, severity, review state | any | any |
| `attachment_names` | substring and fuzzy search over **attachment filenames** | any, **including `customer_held`** | M1 or above |
| `full_text` | full-text search over **prompt text**, with highlighted snippets | `vendor` or `customer_managed` only — **forbidden with `customer_held`** | M3 for the scope |

**Brief §3.5's incompatible pair is a schema invariant, not a policy note.** The database refuses the
combination every other enforcement point would have to remember:

```sql
-- ops.tenant, database/schema.sql
content_search text NOT NULL DEFAULT 'disabled'
  CHECK (content_search IN ('disabled','attachment_names','full_text')),
CONSTRAINT tenant_full_text_search_requires_vendor_readable_content
  CHECK (content_search <> 'full_text' OR key_custody <> 'customer_held')
```

The consequence for a `customer_held` tenant is stronger than the old promise, not weaker: the sentence
is no longer "we have decided not to index your content" but "your tenant's row cannot carry the setting
that would". No configuration path, no migration and no operator can produce the pair, which is what
brief §3.5's "cannot be retrofitted" demands of a choice made once, at onboarding.

**The tier is a ceiling, and the signed bundle narrows it.** The policy bundle (C3) carries the effective
search tier per scope — per tool, per data class, per user population (C1) — and a scope the bundle does
not name carries `disabled`. Search is therefore never enabled globally, which is what keeps C5's "must
not be possible to configure the system into an upload-everything state" a property rather than a
slogan: `full_text` requires M3 in force for the named scope, the per-tenant content budget still bounds
what may be kept, and every content upload is still a per-event grant decision (C14).

**`attachment_names` is the tier most tenants can have, and the one that costs the key model nothing.**
A file upload reports a filename and never bytes (brief §5.1, E3), so a filename is metadata the M1
envelope already carries: substring and fuzzy search over filenames needs no key-model change, no M3, and
**no content leaving the device**. It is also frequently what an investigator is looking for — "find the
file called something like Q3-contract" is a real question, and it is answerable for a tenant that will
never accept server-side search over prompts.

### 15.2 Storage: `ingest.search_text`

One row per searchable unit — the prompt body, and one row per attachment filename:

```sql
-- ingest.search_text, database/schema.sql (abridged; the comments there carry the reasoning)
(tenant_id, submission_id, unit_kind, unit_index, body, tsv, created_at, expires_at)
  unit_kind  IN ('prompt_body','attachment_name')   -- unit_index 0 for the body, 0..n per filename
  body       text NOT NULL CHECK (length(body) BETWEEN 1 AND 65536)
  tsv        GENERATED ALWAYS AS (to_tsvector('simple'::regconfig, body)) STORED
  PRIMARY KEY (tenant_id, submission_id, unit_kind, unit_index)
  FOREIGN KEY (tenant_id, submission_id) REFERENCES ingest.submission ON DELETE CASCADE
CREATE INDEX search_text_tsv_gin   ON ingest.search_text USING gin (tenant_id, tsv);
CREATE INDEX search_text_name_trgm ON ingest.search_text USING gin (tenant_id, body gin_trgm_ops)
                                   WHERE unit_kind = 'attachment_name';
CREATE INDEX search_text_expiry    ON ingest.search_text (tenant_id, expires_at);
```

Four details are contractual rather than incidental:

- **`to_tsvector` takes two arguments.** The one-argument form depends on `default_text_search_config`
  and is only `STABLE`, so it is illegal in a generated column; `('simple', body)` is `IMMUTABLE` and the
  only form that may be used here. `simple` also means no stemming and no stopword list — the right
  default for multi-lingual, code-laden prompt text, where stemming the wrong language is worse than not
  stemming at all, and where an analyst's match should be predictable rather than dependent on a
  language guess.
- **The trigram index is partial, deliberately.** Full-text search is the right tool for prose; trigrams
  are the right tool for filenames. A trigram index over every prompt body would be large for no benefit,
  so substring and fuzzy matching are served for `attachment_name` units — the `attachment_names` tier —
  and term matching serves everything else. Both extensions (`pg_trgm` for the trigram operator class,
  `btree_gin` so a GIN index can lead with `tenant_id`) are declared by `database/schema.sql`, and their
  availability per target region is master doc Q12's platform check.
- **What is not in the table, and why that is a rule rather than an omission.** Attachment *contents* —
  the 50–500 GB/year tier of brief §3.1 — are not indexed in v1: different cost, different breach
  surface, separate decision. The M2 `content_excerpt` is not a unit either, because `unit_kind` admits
  only `prompt_body` and `attachment_name`, so an M2 tenant has nothing to search rather than a search
  over redacted text (§3.9). Content digests are not searchable: a digest lookup that returned text
  would be a confirmation oracle (06 §5.5), which is also why digests stay out of the export (§9.4).
- **Who writes it.** `content-vault`, and only `content-vault` — `sac_vault` holds the sole grant on the
  table, and `sac_query` holds none (`database/schema.sql` §10). A unit exists only for content that reached
  the server under C14's per-event grant path: the index adds no device egress and no collection mode of
  its own (06 §6.3). `prompt_body` units exist where M3 is in force for the scope; `attachment_name`
  units exist from M1.

**Volume, and why this needs no search cluster.** ~4.4M submissions/year × ~1 KB of prompt text ≈
**4.4 GB/year** per tenant, plus the `tsv` and the two GIN indexes — inside the instance that already
holds the events, and the same arithmetic that removed the warehouse and the message broker (master doc
§1.4). The line item that would change the answer, the attachment tier, is precisely the one this index
does not touch.

### 15.3 The query surface

`POST /v1/content-search`, a separate endpoint from `/v1/query`, because it is a different kind of read:
bounded text matching over a table this component cannot see, executed by the component that can.
**As built:** `query-api` serves this route as a forwarder (`src/http/content.js`): it adds the session's
principal and a configured scope and passes the request to `content-vault`, which executes and audits it.
It still rejects a text predicate on `/v1/query` with an error that names this endpoint. Only the
term-or-phrase form is forwarded; the substring and fuzzy filename forms are not, and the device uploads
no attachments to index. The response carries hits (`submission_id`, `snippet`, `rank`) and `truncated`,
and **no index-coverage block**. The dashboard's Explore page is the surface: a prompt-text search box
whose matches open the event they belong to.

**What an analyst types** is a match expression, in one of three forms:

| Form | Example | Served by |
|---|---|---|
| term or phrase | `"wire transfer" AND iban` | `tsv @@ to_tsquery('simple', …)` — `search_text_tsv_gin` |
| substring | `Q3-contract` | `body ILIKE '%…%'` — `search_text_name_trgm` |
| fuzzy | a misspelt filename | `body % …` / `similarity()` — `search_text_name_trgm` |

**What composes with it** is the same closed dimension set as §2.4 — tool, class, severity, rule, review
state, `content_state`, subject, device, department, population, and a `received_at` window — resolved
through the same frozen registry, with values bound and never interpolated. Three properties are
structural rather than conveniences:

- **At least one narrowing predicate is mandatory** — a tool, a data class or a user population drawn
  from the tenant's enabled scopes, plus a window of ≤ 31 days. A search naming none of them is rejected
  as `unsupported_query_shape`, with the predicates that would make it servable named in the response.
  This is §15.1's narrowing applied at request time: the tier is a ceiling, and a search that names no
  scope is asking for the whole tenant. **ASSUMPTION:** A15 — the narrowing requirement is fixed by C5
  and ADR 0014 but its request-time form is not; a named predicate is the form that can be refused
  before the query runs.
- **`unit_kind` is part of the request**, so the answer for a form the tenant cannot have is a capability
  answer, not an empty page: a prompt-body match at `attachment_names` returns
  `content_search_not_enabled` (§13).
- **Role is `analyst` or `investigator`** and nothing else (§2.2): the tier decides what may be searched,
  the role decides who may ask.

**What comes back is hit references with bounded fragments, never the unit:**

```json
{ "result_state": "ok",
  "search": { "tier": "full_text", "match": "\"wire transfer\"", "unit_kind": "prompt_body",
              "scopes": ["tool:claude_web", "population:finance"],
              "snippet_policy": { "fragments_per_hit": 3, "chars_per_fragment": 160 } },
  "coverage": { "submissions_in_scope": 412, "indexed": 399,
                "not_indexed": { "not_captured": 6, "local_only": 0, "shredded": 7 } },
  "freshness": { "source": "ingest.search_text", "last_unit_at": "…", "state": "fresh" },
  "data": [ { "submission_id": "…", "received_at": "…", "user_ref": "…", "tool": "claude_web",
              "unit_kind": "prompt_body", "rank": 0.0812,
              "fragments": [ { "offset": 412, "text": "…the wire transfer instruction…" } ] } ],
  "audit": { "entry_id": "…", "written_at": "…" } }
```

- **The snippet rule.** At most **3 fragments × 160 characters** per hit (≤ 480 characters), each a
  headline-style span around the match — never the unit, and never a contiguous read of it.
  **ASSUMPTION:** A14 — the brief fixes no bound; the cap is set deliberately below the 2,048-character
  M2 excerpt cap this document already pins (§3.9), so a search can never return, by volume, more content
  than the mode beneath it already allows to travel by default. The bound is configuration recorded per
  tenant (06 §14.3 Q-f).
- **Ordering is relevance, and relevance is not a person.** `(rank DESC, received_at DESC,
  submission_id DESC)` — total, so §7's keyset pagination is exact. A subject facet counts hits; no
  measure ranks people by volume, and no view of this endpoint is a leaderboard (§14 item 1, brief
  §1.2).
- **The coverage block is not optional, and a search that cannot state its coverage is not served.** A
  search over an index holding 399 of 412 in-scope submissions must say so, by reason: `not_captured`
  (never read at this mode), `local_only` (taken and left on the device), `shredded` (destroyed, with the
  reason on the retrieval path). Without it, three hits read as "only three prompts mention this" when
  most of the scope was never indexed — C25's failure mode, on the screen most likely to be believed.
  The counts come from `ingest.submission.content_state` over the same narrowed predicate and window, and
  they are part of the query's cost class (§12.1), not a decoration added afterwards.
- **Pagination is §7's.** Self-contained cursors — no subject reference in the ordering key — page 50 /
  maximum 200, frozen at the first page on `ingest.search_text.created_at <= :upper` so a unit indexed
  mid-iteration cannot shift a boundary. Units shredded mid-iteration disappear and make a page shorter,
  which §7.4 already defines as not-the-end.

### 15.4 Where it executes, why, and what it writes

**In `content-vault`, not `query-api`.** `query-api` is granted no `SELECT` on `ingest.search_text` —
the same structural exclusion that already covers `ops.content_object` (§2.1) — and calls the vault with
the narrowed predicate, receiving hit references and bounded snippets back. The invariant that exactly
one component can read content therefore survives the capability instead of being quietly retired: the
vault can now answer both "what is this event's content" and "which events mention this term", and no
second component acquires either path. That is a real expansion of the most sensitive component's job
and it is stated as one rather than as a routing detail.

**Every search is audited, in the transaction that serves it.** A search reads subject-level data — a
prompt, or a filename that names a person's document — so §5.2's rule applies with no k-test and no
exception: the audit row (`action = content.search`) is written **before** the query executes, in the
same transaction, and results are emitted only after that transaction commits. The entry carries the
terms, the scopes, the tier, the actor, the result count and the snippet count. If the insert fails the
search fails closed with `audit_unavailable` and **zero hits are served**.

**A search that returns nothing is still a search.** Zero hits is exactly the answer a confirmation-oracle
query is built to produce, so it is recorded like any other — the negative result is the signal, and it
must be as provable as the positive one.

**Search terms are themselves subject-level data.** An audit log showing that an analyst searched an
employee's name is a record of an investigation into that employee. The terms stay in the entry, which
inherits the append-only, hash-chained, `auditor`-readable controls of §3.10. Per-analyst search rate and
scope breadth therefore become queryable signals in their own right — Q10 is a first-class read path —
which is what makes bulk reading through search reviewable rather than merely recorded (06 §10.5), and
which is the whole of what this design offers against it: search has no second approver, so the record
and somebody reading it are the control.

### 15.5 Retention, erasure, and what the receipt must say

- **Expiry runs on the ordinary path.** Every row carries `expires_at` and is swept on it, indexed by
  `search_text_expiry`. The index is a third copy of content, so it is a third thing brief §3.4's two
  independent, periodically reconciled expiry mechanisms must cover (06 §6.3); a reconciler that finds
  index rows surviving their expiry records drift rather than correcting it silently (C34).
- **A hold suspends it for the held scope** (C35). A hold that made content retrievable but left its
  index rows expiring underneath would be a hold with a hole in it.
- **Subject erasure deletes the rows, and the index dies with the record it describes.** The submission's
  index entries cascade with the submission row (`ON DELETE CASCADE`, §15.2), inside the same transaction
  that deletes the events and destroys the content keys (D1, §4.3), so there is no second deletion path
  to forget and no window in which an erased prompt is still searchable. The count goes into
  `ops.erasure_receipt.removed_counts`, and the receipt names `ingest.search_text` explicitly: a receipt
  that does not say the index was covered is a claim of completeness with a gap in it.
- **Key destruction does not reach the index, and no receipt may imply that it does.** `body` is stored
  as text, not as ciphertext under the tenant KEK, so destroying the KEK leaves it exactly where it was;
  row deletion and expiry are the mechanisms, which is D1's preference anyway. This is the second reason
  the constraint in §15.1 keeps `full_text` away from `customer_held` tenants, for whom key destruction
  is the whole promise (06 §6.4).
- **Tenant offboarding deletes it explicitly**, for the same reason: there is no key whose destruction
  removes it, so the offboarding path must name the table rather than assume a cascade from content.

### 15.6 What a customer must be told before enabling `full_text`

All of this is knowable in advance, so none of it should be discovered afterwards:

1. **Prompt content leaves the device as a matter of course.** For a `full_text` tenant, brief §1.1's
   property — content is interpreted where it is observed, and by default the content itself does not
   leave the machine — survives as **the default and a per-tenant choice, not universally**. A
   `disabled` or `attachment_names` tenant keeps it whole; a `full_text` tenant trades it, for the named
   scopes, for the capability it asked for. Marketing material and the security questionnaire both have
   to say so in those words.
2. **The vendor can read prompt content, and the index is plaintext-derived.** `ingest.search_text` sits
   outside the key hierarchy. Under `customer_managed` custody a customer's key store records every
   unwrap, and a search performs none — so **a mode 2 tenant that enables `full_text` has a
   vendor-reading path its own key store does not see** (06 §6.2). That is the honest form of the
   sentence, and it is why the tier is not offered with `customer_held`.
3. **Search is not retrieval, and the second approver does not guard it.** C16's case reference and
   second approver still protect **full-content retrieval** (§8). A search returns bounded snippets to
   an authorised analyst with no approval step, because a two-person gate on every fragment would make
   the capability unusable. **A tenant that requires approval for any exposure of content should not
   enable `full_text`** — that is a limitation of the tier, not a configuration that can be tuned out of
   it (06 §6.3 states the same thing for the same reason).
4. **Attachment contents are not searchable.** A search finds a document by name, never by what is inside
   it; that is the 50–500 GB/year tier and its own decision (§15.2).
5. **`customer_held` and `full_text` are mutually exclusive by construction**, so a tenant that requires
   vendor-blind keys chooses between them rather than having both. It keeps `attachment_names`, all
   structured filtering, and per-event approved retrieval; the export (§9.5) is its route to search in
   its own environment.
6. **Everything here is deletable and receipted — by deletion, not by key destruction** (§15.5), and a
   hit is a reference rather than a reservation: if the record is shredded between the search and the
   retrieval, §8.2's `no_longer_available` is the answer, and the search's own audit row is how that
   sequence is reconstructed.

---

## 16. Assumptions

Labelled in place; this is the index. None contradicts a brief requirement or a master-doc decision —
they are the parameters the brief leaves open.

| # | Assumption | One-line justification |
|---|---|---|
| A1 | k = 5 for suppression and for the subject-level audit trigger | The brief fixes no threshold; 5 is the conventional small-cell floor and is below typical team size here |
| A2 | Cadence 5 min; lookback 7 days (day) / 48 h (hour); `stale_aggregate` at 15 min | The brief fixes no cadence; this bounds staleness to a coffee break at trivial recompute cost |
| A3 | Six analyst-facing roles | C16 and Q10 require reading, approving and proving to be different hands; the brief names none |
| A4 | Page 50/500, 2,000 cells, 400 points, 8 MB; concurrency 8 tenant / 4 user / queue 32 | The brief bounds the shape (C29) but not the numbers; these keep every response inside §8's p95 |
| A5 | Spike: ≥ 14 observed buckets, value > median + 4·MAD and ≥ 3× median | The brief says "changed or spiked" without defining it; median/MAD resists the flush day a mean would absorb |
| A6 | Subject export p95 ≤ 4 h, hard bound 24 h; second approver only when content is included | §4.5 requires a bounded time without a number; the bound holds because subjects have hundreds-to-thousands of rows (D1) |
| A7 | Export destination is Azure Blob in v1, behind a storage-writer interface | The brief says "the customer's own storage" without a cloud; the platform is Azure (S1) |
| A8 | The dashboard displays `user_ref`, not personal names | The brief is silent; the encrypted directory id exists for export/erasure resolution, not naming |
| A9 | Reading `ops.audit` writes one audit row per query and is not re-audited | C30 says "every read of subject-level data"; a one-level per-query rule is the only terminating reading |
| A10 | The M2 excerpt is part of the event record, not gated by the retrieval workflow | Brief §1.1 puts the excerpt on the wire at M2 by default; gating it would make M2 self-contradictory |
| A11 | The < 30 s retrieval target runs from second approval to first byte | §8's "once granted" is the only measurable reading; approval latency is reported separately |
| A12 | Cursor lifetime 15 min; subject-bearing cursors are server-side | The brief fixes neither; a cursor should not be a durable grant or a subject reference in a URL |
| A13 | Week buckets are ISO (Monday); all buckets UTC | The brief is silent, and a week boundary must be written down rather than inferred per chart |
| A14 | A search returns ≤ 3 fragments × 160 characters per hit (≤ 480) and 50/200 hits per page | The brief fixes no bound; the cap sits below the 2,048-character M2 excerpt cap (§3.9), so a search cannot return more content by volume than the mode beneath it already lets travel by default |
| A15 | Every search names at least one narrowing predicate (tool, class or population) and a ≤ 31-day window, and a scope must be named in the signed bundle before its tier applies | C5 forbids an upload-everything state and D6/ADR 0014 require search never to be enabled globally; a named predicate at request time and a named scope at signing time are the two mechanical forms of that |
| A16 | Content search is reachable by `analyst` and `investigator` only | The brief names no role mapping; the tier is for tenants who treat bounded search as an ordinary analyst capability (06 §6.3), and the other four roles are built around not reading subject-level data (§2.2) |

---

**Related:** [00-architecture](00-architecture.md) §4.5 (D6, D7), §5.3 (data model) ·
[ADR 0014](adr/0014-content-search-is-a-per-tenant-capability.md) ·
[06-security-and-threat-model](06-security-and-threat-model.md) §6.3, §10.5, §11.3 ·
[02-ingest-and-transport](02-ingest-and-transport.md) · [03-data-platform](03-data-platform.md) ·
[database/schema.sql](../database/schema.sql) · [contracts/event-envelope.schema.json](../contracts/event-envelope.schema.json)
