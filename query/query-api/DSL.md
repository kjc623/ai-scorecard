# query-api DSL v1 — the frozen grammar

**Status:** frozen for `query_version: "1"`. · **Owner:** `query/query-api` · **Consumers:** `query/dashboard`, the integration verifier.
**Normative sources:** [ADR 0003](../../docs/adr/0003-the-dashboard-reads-through-a-query-api-with-a-closed-query-dsl.md), [`docs/04-dashboard-and-query.md`](../../docs/04-dashboard-and-query.md) §2.3, §2.4, §3, §5, §6, §7, §12, §13. This file is the *implemented* grammar; where the two disagree the disagreement is listed in §9 rather than hidden.

> **INV-3.** The browser never speaks SQL. No part of a client-supplied value is ever concatenated
> into SQL text; a value is *bound*, and a dimension name that is not in the enumerated vocabulary
> is *rejected, not escaped*. The DSL is closed rather than escaped, so there is no escaping code
> to get wrong.

If you are the dashboard author, read §1 (two ways to ask), §2 (the closed vocabularies), §3 (the
ten templates), §4 (the response), §5 (result states) and §7 (pagination). §9 lists every decision
this document had to make that `docs/04` left open — read it before filing a bug against the API.

---

## 1. Two ways to ask

Both are `POST /v1/query`. Both go through the same validate → guard → compile pipeline, so a
template cannot express anything a document cannot.

**1a. A named template** — the ten questions of §3, pre-shaped server-side:

```json
{ "query_version": "1",
  "template": "q1_tools_ranked",
  "params": { "window": { "from": "2026-09-01T00:00:00Z", "to": "2026-10-01T00:00:00Z" },
              "bucket": "day", "limit": 200 } }
```

Unknown template names, unknown template parameters and out-of-range parameter values are
rejected. A parameter the template does not declare is an error, not a no-op.

**1b. A closed query document:**

```json
{ "query_version": "1",
  "source": "mart.v_tool_usage",
  "bucket": "day",
  "dimensions": ["tool"],
  "measures": ["submissions", "users"],
  "filters": [ { "field": "sanctioned_state", "op": "eq", "value": "unsanctioned" } ],
  "window": { "from": "2026-09-01T00:00:00Z", "to": "2026-10-01T00:00:00Z" },
  "order": [ { "by": "submissions", "dir": "desc" } ],
  "limit": 200,
  "rollup": false,
  "cursor": null }
```

### Top-level keys — the complete set

| Key | Type | Notes |
|---|---|---|
| `query_version` | `"1"` | **required**; any other value is `unsupported_query_version`. There is no silent downgrade. |
| `template` | string | one of the ten names in §3 |
| `params` | object | only with `template` |
| `source` | string | one of §2.1 |
| `bucket` | `"hour"｜"day"｜"week"｜"month"` or absent | see §2.4 |
| `dimensions` | string[] | ≤ 3, plus the bucket. Aggregates only. |
| `measures` | string[] | ≥ 1 on an aggregate; must exist on the chosen source (§2.2) |
| `filters` | object[] | ≤ 16; `{field, op, value}` |
| `window` | `{from,to}` | ISO-8601 UTC; half-open `[from, to)`. Required except on `mart.v_device_liveness`. |
| `order` | `{by,dir}[]` | ≤ 3; only a returned measure or a grouped dimension; aggregates only |
| `limit` | integer | page size / row cap; ≤ 500 on a list, ≤ 2000 on an aggregate |
| `cursor` | string | opaque; echo it back verbatim (§7) |
| `rollup` | boolean | adds a published total row; mutually exclusive with `cursor` |

Anything else is `unsupported_query_shape / unknown_key`. Ten keys are rejected **by name** because
their presence is an attempt rather than a typo:

* `sql`, `raw`, `where`, `expression`, `order_by`, `filter_sql`, `having`, `select`, `from`, `join`
  → `prohibited_field`
* `tenant_id`, `tenant` → `tenant_in_request`. **The tenant comes from the authenticated session
  and never from the request.** A request carrying one is rejected, not ignored: silently dropping
  it would turn an attempted cross-tenant read into an uneventful success.
* `content_match`, `snippet`, `search`, `match`, `tsv`, `body` → `text_predicate_in_request`.
  There is no text predicate on `/v1/query`; text search is `POST /v1/content-search` §15.3.
* `__proto__`, `prototype`, `constructor` → `not_closed`.

---

## 2. The closed vocabularies

### 2.1 Sources

| `source` | Kind | Answers | §3.11 indexes it needs |
|---|---|---|---|
| `mart.v_tool_usage` | aggregate | Q1, Q2 tool set | `mart.agg_tool_period` PK |
| `mart.agg_tool_period` | aggregate | Q1 including `detections`/`rollup_events`/`degraded_events` | PK |
| `mart.agg_tool_user_period` | aggregate | Q2 people | PK + `(tenant, tool_fingerprint, bucket_start DESC, bucket_size)` |
| `mart.agg_org_period` | aggregate | Q3 teams | PK + `(tenant, department, bucket_start DESC)` |
| `mart.agg_class_period` | aggregate | Q4 classes | PK |
| `mart.agg_user_period` | aggregate | Q6 one subject | PK + `(tenant, user_ref, bucket_start DESC, bucket_size)` |
| `mart.agg_device_period` | aggregate | Q7 history | PK |
| `mart.v_device_liveness` | list | Q7 current state | `ops.device (tenant, last_seen_at)`, `ops.collector_state (tenant, state)` |
| `ops.coverage_snapshot` | list | coverage gaps | PK + partial `(tenant, snapshot_day) WHERE NOT observed` |
| `ingest.submission` | list | Q8, Q9 | `submission_by_received`/`_by_user`/`_by_tool`/`_by_device`, `submission_labels_gin` |
| `mart.v_finding` | list | Q5 | `mart.finding (tenant, detected_at DESC, submission_id)` |
| `ops.audit` | list | Q10 | `audit_by_time`, `(tenant, object_type, object_id)` |

### 2.2 Dimensions and measures, per source

A dimension or measure exists only where the source carries the column. Asking for one that is not
offered is `unknown_dimension` / `unknown_measure`, and the error lists what is offered.

| Source | Dimensions | Measures |
|---|---|---|
| `mart.v_tool_usage` | `bucket`, `tool`, `sanctioned_state` | `submissions`, `users`, `bytes_total`, `blocked`, `warned`, `logged` |
| `mart.agg_tool_period` | `bucket`, `tool`, `sanctioned_state` | the six above plus `detections`, `rollup_events`, `degraded_events` |
| `mart.agg_tool_user_period` | `bucket`, `tool`, `subject` | `submissions`, `bytes_total` |
| `mart.agg_org_period` | `bucket`, `department`, `population`, `tool` | `submissions`, `users` |
| `mart.agg_class_period` | `bucket`, `class`, `tool`, `severity`, `classifier_version` | `submissions`, `users`, `max_score`, `degraded_events` |
| `mart.agg_user_period` | `bucket`, `subject` | `submissions`, `bytes_total`, `tools_used`, `block_events` |
| `mart.agg_device_period` | `bucket`, `device`, `collector` | `healthy_days`, `degraded_days`, `absent_days`, `tampered_days`, `spool_dropped` |
| `mart.v_device_liveness` | `device`, `device_os`, `managed_state`, `region`, `liveness`, `collector`, `collector_state` | — (a list) |
| `ops.coverage_snapshot` | `snapshot_day`, `device`, `collector`, `gap_reason`, `observed`, `expected` | — |
| `ingest.submission` | `subject`, `tool`, `device`, `mode`, `action`, `content_state`, `route`, `detection_basis`, `merge_confidence`, `confidence`, `department`, `population`, `manager` | — |
| `mart.v_finding` | `severity`, `rule`, `class`, `subject`, `tool`, `review_state`, `mode`, `decided_locally` | — |
| `ops.audit` | `actor_type`, `actor`, `action`, `object_type`, `subject`, `case` | — |

Extra filter-only columns (not groupable): on `ingest.submission` — `submission_id`, `received_at`,
`first_occurred_at`, `last_occurred_at`, `expires_at`, `size_bytes`, `observation_count`, and the
predicate `class`; on `mart.v_finding` — `detected_at`, `submission_id`, `rule_id`; on
`ops.audit` — `audit_seq`, `occurred_at`, `object_id`; on `mart.v_device_liveness` — `enrolled_at`,
`last_seen_at`, `revoked_at`, `spool_depth`, `spool_dropped_total`.

**`mart.agg_user_period` requires a subject filter** (`subject eq` or `subject in`). Its row grain
*is* a person, so a read without one would be a list of people — which §11.2 ("Person is a lookup,
not a list") and §14 item 1 (no volume leaderboard) forbid. The refusal is
`unsupported_query_shape / subject_scope_required` and the fix is in `error.detail.fix`.

The window column is filterable under its own name (`received_at`, `detected_at`, `occurred_at`,
`snapshot_day`) with the comparison operators below.

### 2.3 Operators

`eq`, `ne`, `in`, `not_in`, `lt`, `lte`, `gt`, `gte`, `between`, `starts_with`, `is_null`.

* `in` / `not_in` take a non-empty array of ≤ **200** values.
* `between` takes exactly `[low, high]`.
* `is_null` takes no value. `eq: null` is rejected — use `is_null` (a `= NULL` predicate that
  silently matches nothing is exactly the class of bug this DSL exists to prevent).
* `starts_with` is permitted **only** on `tool` fingerprints, and only on aggregate sources; on
  `ingest.submission` and `mart.v_finding` it is refused as `unsupported_query_shape` because no
  index in §3.11 serves a prefix predicate there (the error names
  `fix.alternative_source: mart.agg_tool_period`).
* Type rules: text/uuid → `eq ne in not_in is_null`; timestamp/date/number → those plus
  `lt lte gt gte between`; boolean → `eq ne is_null`.
* Values are typed: timestamps must be ISO-8601 UTC (`2026-09-01T00:00:00Z`), dates `YYYY-MM-DD`,
  identifiers canonical uuids, strings ≤ 256 characters. Nested objects and arrays are not values.
* `class` on `ingest.submission` is a **predicate**, not a dimension: `eq` only, compiled as
  `labels @> jsonb_build_object('class', $n)` and served by the `labels` jsonb_path_ops GIN.

### 2.4 Buckets, grouping and ordering

* `bucket`: `hour` and `day` are native (`mart.agg_*_period.bucket_size` admits exactly those);
  `week` (ISO, Monday) and `month` are read-side reductions over day rows. Whichever is applied,
  exactly one `bucket_size` is pinned — without that, an hour row would be added to a day total.
  The applied bucket is returned in `meta.applied_bucket`.
* Omitting `bucket` on an aggregate means a single window total, computed from **day** rows
  (`meta.applied_bucket: "day"`).
* `dimensions` ≤ 3, and `bucket` is not a dimension name (pass it as `bucket`).
* A list source does not group: grouping an event list is `unsupported_query_shape`. Any
  time-bucketed number must come from `mart` (C27).
* `order` is ≤ 3 terms, each naming a returned measure or a grouped dimension. The server then
  **appends the deterministic tie-break** — bucket first (DESC) when bucketed, then every grouped
  key ASC — so the ordering is total and a keyset page is exact. Caller terms come after the
  bucket: `ORDER BY bucket DESC, <your terms>, <tie-break>`.
* List sources have one fixed total ordering each; supplying `order` for one is
  `cursor_requires_total_order`.

### 2.5 Measures have semantics, and the response says which

| `meta.measure_semantics[name]` | Meaning |
|---|---|
| `additive` | a counter; summed over the cell's rows |
| `exact` | a distinct-subject count that is exact for this cell |
| `distinct_lower_bound` | a distinct-subject count that may under-count when the cell combines rows |

`users` is `exact` only when the cell is a single aggregate row (the grouping includes the source's
full grain and the bucket is native); otherwise it is `distinct_lower_bound` and is served as
`max(...)`, which **never overstates**. The same value is the k input of §6.

`max_score` is a `MAX`, not a sum. `submissions` on `mart.agg_class_period` counts submissions
*carrying that class*: one submission with three labels appears in three rows, so summing class
rows and calling the result "submissions" overstates volume. The Q4 template returns the
non-additive total separately, from `mart.agg_tool_period`, as `meta.extras.class_total`.

---

## 3. The ten templates

Every template declares exactly these parameters; anything else is an error.

| Template | Source | Parameters | Notes |
|---|---|---|---|
| `q1_tools_ranked` | `mart.v_tool_usage` | `window`, `bucket`, `limit`, `tool`, `sanctioned_state` | rank by `submissions`, tie-break `tool`; `sanctioned_state` NULL renders as `unknown`, never as `unsanctioned` |
| `q2_unsanctioned_users` | `mart.agg_tool_user_period` | `window`, `bucket`, `limit`, `tool`, `subject` | subject-bearing → audited, server-side cursor; > 7 days unscoped is refused |
| `q3_team_growth` | `mart.agg_org_period` | `window`, `bucket`, `limit`, `department`, `population` | carries the `unmapped` residual in `meta.extras.org_coverage`; `not_yet_covered` before the directory sync |
| `q4_class_mix` | `mart.agg_class_period` | `window`, `bucket`, `limit`, `dimensions`, `class`, `severity` | `dimensions` ⊆ `{class, tool, severity, classifier_version}`, ≤ 3; default `[class, severity]` |
| `q5_findings` | `mart.v_finding` | `window`, `limit`, `cursor`, `severity`, `rule`, `review_state`, `subject`, `tool`, `class` | review state `open` means nobody has looked |
| `q6_subject_series` | `mart.agg_user_period` | `subject` (**required**), `window`, `bucket`, `limit` | k-suppression exempt (§6.4); carries `meta.extras.flush_check` |
| `q7_devices` | `mart.v_device_liveness` | `limit`, `cursor`, `liveness`, `collector_state`, `collector`, `device_os`, `managed_state`, `region` | **no window**; four liveness values stay distinct |
| `q8_activity` | `ingest.submission` | `window`, `limit`, `cursor`, `subject`, `tool`, `device`, `class`, `content_state`, `action`, `mode`, `department` | window ≤ 31 days; both clocks returned |
| `q9_event_detail` | `ingest.submission` | `submission_id` (**required**), `received_at_hint` | single record; response is `data: [row, …observations]` |
| `q10_audit_trail` | `ops.audit` | `window`, `limit`, `cursor`, `actor`, `action`, `object_type`, `subject`, `case` | hash links verified in SQL before the page is returned |

`window` is `{from, to}` in every row above.

---

## 4. The response envelope

```json
{ "api_version": "1",
  "query_version": "1",
  "result_state": "ok",
  "data": [ /* cells or rows */ ],
  "page": { "returned": 50, "next_cursor": "…", "snapshot_upper_bound": "2026-10-02T11:05:00Z",
            "newer_events_exist": true },
  "freshness": { "aggregate": "mart.agg_tool_period", "bucket_size": "day", "last_run_at": "…",
                 "last_complete_bucket": "…", "lag_seconds": 168, "state": "fresh" },
  "coverage": { "window": ["2026-09-03","2026-10-02"], "devices_reporting": 4180,
                "devices_enrolled": 4620, "gap_reasons": {"not_enrolled": 440}, "state": "partial" },
  "suppression": { "k": 5, "suppressed_cells": 2, "subject_count_basis": "lower_bound" },
  "audit": { "entry_id": "8123", "written_at": "2026-10-02T11:05:01Z" },
  "meta": { "…": "see below" } }
```

Three properties are load-bearing and are enforced in code, not by convention:

1. **`freshness` and `coverage` are always present on a data-bearing response.** The envelope
   builder refuses to construct one without them, so a UI cannot omit a state it was given.
   `page` is present on a list read. `audit` is present exactly when an audit row was written —
   `audit.entry_id` is what makes a screenshot traceable (Q10).
2. **`result_state` is a first-class answer, not an error channel** (§5).
3. **`data` never contains a bare number for a suppressed cell, and never a hidden column.**
   Internal columns (`__k_subjects`, `__ord_*`, `__recomputed_hash`) never reach the wire.

`meta` carries, at least: `source`, `kind`, `applied_bucket`, `native_bucket_size`, `reduced_from`,
`dimensions`, `measures`, `measure_semantics`, `order`, `limit`, `probe_row`, `rollup`,
`has_cursor`, `k`, `subject_count_basis`, `query_class`, `statement_timeout_ms`, `required_indexes`,
`warnings`, `joins_used`, `dsl_hash`, `snapshot_upper_bound`, `cursor_mode`, `coarsened`,
`guard{estimated_cells,bounded_cells,paged,estimated_bytes}`, `notes`, and `extras` when the
template produced a side read.

A suppressed cell looks like this and carries **no measure at all** — not `0`, not `null`:

```json
{ "bucket": "2026-10-01T00:00:00Z", "department": "Legal",
  "result_state": "suppressed", "reason": "fewer_than_k_subjects", "k": 5 }
```

---

## 5. Result states

Every response carries `result_state`. HTTP distinguishes "the request failed" from "the answer is
not a number", and the client must render the difference.

| `result_state` | HTTP | Meaning | UI |
|---|---|---|---|
| `ok` | 200 | a real answer | the data |
| `empty` | 200 | we looked, coverage was adequate, there is nothing | "No data" — an answer, not an error |
| `not_yet_covered` | 200 | window predates collection, or the dimension has no source | hatched, with `freshness.reason` / `coverage.reason` |
| `stale_aggregate` | 200 | watermark older than 3× the cadence (15 min) | data **with** its age |
| `coverage_degraded` | 200 | the value is a floor, not a total | data with the gap share |
| `suppressed` | 200 | every cell in the response was suppressed | hatched cells |
| `no_longer_available` | **410** | the record existed and was destroyed | with `shredded_reason` and the receipt |
| `not_found` | 404 | no such record, and no purge covers its window | "No such record" |
| `unsupported_query_shape` | 400 | filter combination not servable | the fix, named |
| `query_too_broad` | 400 | over budget or over a cap | the coarser bucket / narrower window that fits |
| `cursor_expired` | 400 | expired, mismatched, or from another query or tenant | restart from page one, told why |
| `audit_unavailable` | 503 | the audit row could not be committed | **no data at all** |
| `busy` | 429 | shed by concurrency | retry hint |
| `unauthorised_role` | 403 | the role cannot make this read | which role is needed |
| `audit_chain_broken` | 500 | a page's hash links do not verify | an integrity alert, not a list |
| `not_captured` / `not_retrievable` / `key_unavailable` | 200 / 200 / 503 | content states of §8.2 | as §13 |

Error body:

```json
{ "api_version": "1", "query_version": "1", "result_state": "query_too_broad",
  "error": { "code": "cost_estimate_exceeded", "message": "…", "fixable": true,
             "detail": { "max_cells": 2000, "estimated_cells": 33600,
                         "fix": { "coarser_bucket": "week" } } } }
```

A rejection never carries `data`, `freshness` or `coverage` — there is no number to qualify.

---

## 6. Suppression (k = 5)

* k applies to **distinct subjects per cell**, not to rows: 900 submissions from 2 people is
  suppressed.
* A suppressed cell suppresses **every measure in it**.
* **A genuine zero is not suppressed.** `0` means "we looked and there was none"; `suppressed`
  means "there was something and we are not telling you the number". They are never merged.
* **Complementary suppression.** With `rollup: true`, if exactly one cell in the response is
  suppressed the published total is suppressed too (`reason: "complementary_suppression"`),
  because otherwise total − published cells recovers the hidden value.
* Does **not** apply to explicitly subject-scoped reads (`q6`, a subject-filtered list) — §6.4.
* `meta.subject_count_basis` is `exact` or `lower_bound`; a `lower_bound` can only over-suppress.
* The audit decision is made **before** suppression: a query answered with `suppressed` still writes
  an audit entry.

---

## 7. Cursor pagination

* **Only `page.next_cursor` ends an iteration.** A short page is not the end: rows deleted
  mid-pagination (retention expiry, erasure) make a page shorter, and stopping there would truncate
  a result set whose rows were erased underneath it (§7.4).
* Pass the cursor back verbatim as `cursor`. Treat it as opaque; do not parse, decode or construct
  one.
* Two encodings, and the client cannot tell which it received: a signed self-contained token (no
  subject reference in the ordering key) or an opaque server-side id (ordering key contains
  `subject`). Both expire after **15 minutes**.
* Every failure — expired, unknown, tampered, another tenant, another query, a changed ordering key
  after a deploy — is `cursor_expired` (400). Restart from page one; do not retry with an offset.
* `page.snapshot_upper_bound` freezes the window at first page: everything after page one carries
  `received_at <= upper`, so an insert cannot shift a boundary and a row can be neither seen twice
  nor skipped. `page.newer_events_exist` announces that rows arrived since the snapshot rather than
  hiding them.
* Cursors are subject-level data: never log one in full, never echo it in an error.

---

## 8. Cost guard and audit

### 8.1 What is refused, before anything runs

| Bound | Value | Refusal |
|---|---|---|
| Aggregate cells | 2,000 | `query_too_broad` naming the coarser bucket that fits |
| Aggregate page | ≤ 2,000 | a paged aggregate is bounded by its page; the estimate is disclosed in `meta.guard` |
| List rows | 50 default, 500 max | `query_too_broad` with the bound |
| Event/finding window | 31 days | `query_too_broad` |
| Audit window | 366 days | `query_too_broad` |
| Coverage window | 366 days | `query_too_broad` |
| Subject-grouped window | 7 days unless narrowed by a tool or subject filter | `query_too_broad` naming the narrowing |
| Time series | 400 points | auto-coarsened when the server chose the bucket; refused, naming the bucket that fits, when the caller pinned one |
| Response body | 8 MB | `query_too_broad` |
| Statement timeout | aggregate 3 s, list/audit/operational 5 s, single 3 s | server-side `statement_timeout` |

Statement timeouts, row caps and `SET LOCAL` are applied by the executor from
`meta.statement_timeout_ms`; the compiled statement itself carries only `SELECT`.

### 8.2 When a read is audited (§5)

Audited **before** the rows are served: any query that filters on `subject`; any query that returns
a subject reference; a single-record detail; any read of `ops.audit` (one row per query, not
re-audited).

Audited **after** the read and before anything is served: an aggregate whose cells resolve to fewer
than k distinct subjects — the same value §6 computes for suppression decides it.

Not audited: tool-, class- and team-level aggregates whose cells are k or wider; device and
coverage state; reference data.

Fail closed: if the audit row cannot be committed the transaction is rolled back, the response is
`503 audit_unavailable`, and **zero rows** are served. Nothing is streamed; a subject-level
response is materialised and committed before its first byte.

---

## 9. Decisions this document makes, where `docs/04` left a gap

Each of these is a place the specification is silent, contradictory, or unrepresentable, and the
choice this package made instead. They are listed so the dashboard author and the verifier can
disagree with the choice rather than discover it.

1. **`query_version` is a string, and a mismatch is a hard rejection.** §2.3 requires rejection
   "never silently downgraded"; the version is carried on every request and response.
2. **`sanctioned_state` is a dimension.** §2.4's dimension table omits it, but §3.2 requires
   "which are unsanctioned" and the three states to stay separate. It is groupable and filterable
   on the two tool sources, and is nullable (NULL = a tool with no `ops.tool` row = `unknown`).
3. **`classifier_version` is a dimension.** §3.4 requires a classifier change to appear as a
   version change; it is in `mart.agg_class_period`'s primary key, so it is offered. With the
   three-dimension cap, `class` + `tool` + `severity` + `classifier_version` cannot all be shown at
   once; the q4 template takes `dimensions` to choose.
4. **Measures beyond §2.4's list are offered where the column exists**: `detections`,
   `rollup_events`, `degraded_events` (on `mart.agg_tool_period`), `block_events` (on
   `mart.agg_user_period`). §4.6 says these landed in the schema for exactly these questions.
5. **Q1 cannot show `detections`/`rollup_events`/`degraded_events`.** `mart.v_tool_usage` — the view
   §3.1 names — does not expose them. The response says so in `meta.warnings`; use source
   `mart.agg_tool_period` for those measures.
6. **Q7 is two sources, not one three-way join.** §3.7 names
   `mart.v_device_liveness ⋈ ops.collector_state ⋈ ops.coverage_snapshot`, but
   `ops.coverage_snapshot` is one row per device per collector per **day**: joining it without a
   `snapshot_day` predicate multiplies every device row by the number of days in the window. So
   `mart.v_device_liveness` joins `ops.collector_state` (≤ 6 rows per device, which is what §3.7's
   "5,000 × 6 collectors = 30k rows" describes), and coverage is its own source.
7. **The Q7 cursor is `(device_id, collector)`, not `(device_id)`.** §7.1 gives a device-only key,
   but the row grain after the collector join is `(device, collector)`; a device-only key is not
   total and the keyset page would be inexact.
8. **The Q2 cursor and the 2,000-cell cap.** §12.1 caps an aggregate read at 2,000 cells; §3.2
   requires Q2 to be cursor-paged at 50/500. Both cannot hold unless the cap applies to the
   *response*. So: an aggregate with a page size is bounded by its page, an aggregate without one
   must fit 2,000 cells, and the estimate is disclosed in `meta.guard.estimated_cells`.
9. **Auto-coarsening versus refusal.** §7.5 and §3.1 say a series beyond 400 points is
   auto-coarsened and the applied bucket named in `freshness`; §12.3 says a four-year day-bucketed
   trend is `query_too_broad` suggesting week or month. Both are honoured by asking who chose the
   resolution: a **server-chosen** bucket (the caller sent none) is auto-coarsened and reported; a
   **caller-pinned** bucket that cannot fit is refused with `fix.coarser_bucket`.
10. **Ordering precedence.** §2.4 says ordering is on a measure or a grouped dimension "always with
    a deterministic tie-break on the remaining grouping keys"; §7.1 gives the aggregate-series key
    as `(bucket_start DESC, <grouping keys> ASC)`. Implemented as: bucket DESC first, then the
    caller's terms, then the grouping keys ASC. That is what makes "top N within each bucket" the
    natural reading, and it is the key the cursor binds to.
11. **`limit` on an aggregate truncates whole buckets rather than returning a top-N per bucket.**
    §3.1 mentions "top-N per bucket"; that needs a window function whose keyset page is not the
    same ordering, so it is not implemented. A caller that needs it should page the series and
    rank client-side, or ask for one bucket.
12. **Windows are mandatory except on `mart.v_device_liveness`.** §3.6/C29 forbid unbounded result
    sets; the device list is current state rather than a stream and is bounded by its page.
13. **A findings list window is capped at 31 days** because §12.1's "Event / finding list" row says
    so. A year-long findings review therefore needs either a window walk or an API change; §3.5's
    cost note assumes a shorter horizon. Flagged as a document ambiguity, not a design choice.
14. **The audit window cap is 366 days.** §12.1 gives no window for an audit read; a year is what
    the audit trail's own sizing (§3.10, ~36k rows/tenant/year) makes servable at 500 rows a page.
15. **`not_found` versus `no_longer_available` is only half-implementable.** §13 resolves a missing
    record by asking whether "no retention run and no erasure receipt covers the window". Erasure
    receipts exist (`ops.erasure_receipt`), and are checked; **there is no retention-run ledger in
    `database/schema.sql`**, and without the row there is no way to recover the expiry it would have
    carried. The response therefore returns `not_found` with
    `detail.retention_evidence: "no_ledger_in_schema"` rather than guessing, and without a
    `received_at_hint` it says `purge_window_unknown: true` — a submission id is a uuid, not a
    timestamp, so the window is not derivable from it.
16. **The k-triggered audit row is written after the read, before the response.** §5.1 says the row
    is written "before the rows are read"; §5.2's small-cell trigger needs the cells' distinct
    subject counts, which §6 computes only once the cells exist. Both are satisfied by writing the
    row in the same transaction and serving nothing before it commits.
17. **`users` is a distinct-subject count served as `max(...)` when the cell may combine rows** — a
    lower bound that never overstates, labelled `distinct_lower_bound` in
    `meta.measure_semantics`. Summing the column would be an upper bound and would publish cells
    that are really smaller than k.
18. **A cell of genuine zeros is published as zero even when its subject count is below k.** §6.3's
    rule that a zero and a suppressed cell are different facts decides it.
19. **Audit action names are this package's own closed vocabulary** (`query.aggregate`,
    `query.events`, `query.findings`, `query.devices`, `query.coverage`, `audit.read`,
    `query.record`, `content.search`, `content.reveal`, `export.run`). §3.10 names only
    `content_search` as a new action; the rest are not fixed by the document.
20. **`meta` is additive.** New keys may appear in `meta`, `freshness`, `coverage` and `suppression`
    within `query_version: "1"`; `data`, `page`, `result_state` and the error codes are the stable
    surface. §2.3's "additive only" is applied to the envelope the same way it is applied to the
    DSL. Should this change, `query_version` changes.
21. **A per-subject source must name its subject.** §2.4 describes `subject` as a dimension without
    saying that some sources may not be grouped by it freely. `mart.agg_user_period`'s row grain
    *is* a person, so a read of it without a `subject eq`/`in` filter is refused as
    `subject_scope_required`. Without that rule, §14 item 1's "no per-employee ranking" would be
    one query document away, and the Q6 template would be the only thing standing in the way.
    `mart.agg_tool_user_period` (Q2, "who is using them") is deliberately *not* subject-scoped:
    the document asks that question per tool, and the 7-day and page-size rules bound it instead.

---

## 10. Contract for the dashboard author

* Call `POST /v1/query` with `query_version: "1"`. Never send `tenant_id`; the session owns it.
* Prefer a template; use a document only when the template's parameters do not cover the screen.
* Render `freshness` and `coverage` on the tile that shows a number, not in a tooltip.
* Treat a cell with `result_state: "suppressed"` as a distinct hatched state, with `k` and
  `reason` available. Never coerce it to `0`, and never coerce a `0` to suppressed.
* Page only while `next_cursor` is non-null.
* On `cursor_expired`, restart from page one and tell the user why.
* On `query_too_broad` / `unsupported_query_shape`, read `error.detail.fix` and present it as the
  action the user can take — the API refuses rather than degrading, so the fix is always in the
  response.

If this grammar has to change, the change is additive within `query_version: "1"`; anything else is
a new version and this file, the dashboard and the verifier all move together.
