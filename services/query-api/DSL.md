# query-api DSL, `query_version: "1"`

The grammar `POST /v1/query` accepts and the envelope it answers with. The browser never speaks SQL:
no part of a client-supplied value is ever concatenated into SQL text. A value is *bound*, and a
name that is not in the closed vocabulary is *rejected, not escaped*.

---

## 1. Two ways to ask

Both are `POST /v1/query` with `Authorization: Bearer <product access token>`. Nine of the ten
templates expand to a closed document and go through the same validate → guard → compile pipeline,
so they cannot express anything a document cannot. The exception is `q9_event_detail`: a
single-record read with its own fixed statements, whose two parameters are checked by the template.

**1a. A named template**, one of the ten questions of section 3:

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
  "rollup": false }
```

### Top-level keys — the complete set

| Key | Type | Notes |
|---|---|---|
| `query_version` | `"1"` | **required**; any other value is `unsupported_query_version`. There is no silent downgrade. |
| `template` | string | one of the ten names in section 3 |
| `params` | object | only with `template` |
| `source` | string | one of section 2.1 |
| `bucket` | `"hour"｜"day"｜"week"｜"month"` or absent | see section 2.4 |
| `dimensions` | string[] | ≤ 3, plus the bucket. Aggregates only. |
| `measures` | string[] | ≥ 1 on an aggregate; must exist on the chosen source (section 2.2) |
| `filters` | object[] | ≤ 16; `{field, op, value}` |
| `window` | `{from,to}` | ISO-8601 UTC; half-open `[from, to)`. Required except on `mart.v_device_liveness`, `ops.collector_state` and `mart.v_person`. |
| `order` | `{by,dir}[]` | ≤ 3; only a returned measure or a grouped dimension; aggregates only |
| `limit` | integer | page size on a list (≤ 500), row cap on an aggregate (≤ 2000) |
| `cursor` | string | list sources only; opaque, echo it back verbatim (section 7). An aggregate is not paged: a cursor on one is `aggregate_not_paged`. |
| `rollup` | boolean | aggregates only; adds a published total row |

Anything else is `unsupported_query_shape / unknown_key`. Some keys are rejected **by name**
because their presence is an attempt rather than a typo:

* `sql`, `raw`, `where`, `expression`, `order_by`, `filter_sql`, `having`, `select`, `from`, `join`,
  `group_by` → `prohibited_field`
* `tenant_id`, `tenant` → `tenant_in_request`. **The tenant comes from the access token and never
  from the request.** A request carrying one is rejected, not ignored: silently dropping it would
  turn an attempted cross-tenant read into an uneventful success.
* `content_match`, `snippet`, `search`, `match`, `tsv`, `body` → `text_predicate_in_request`.
  There is no text predicate on `/v1/query`; prompt-text search is `POST /v1/content-search`.
* `__proto__`, `prototype`, `constructor` → `not_closed`.

---

## 2. The closed vocabularies

### 2.1 Sources

| `source` | Kind | Answers | Indexes it relies on |
|---|---|---|---|
| `mart.v_tool_usage` | aggregate | Q1, Q2 tool set | `mart.agg_tool_period` PK |
| `mart.agg_tool_period` | aggregate | Q1 including `detections`/`rollup_events`/`degraded_events` | PK |
| `mart.agg_tool_user_period` | aggregate | Q2 people | PK + `(tenant, tool_fingerprint, bucket_start DESC, bucket_size)` |
| `mart.agg_org_period` | aggregate | usage by department | PK + `(tenant, department, bucket_start DESC)` |
| `mart.agg_team_period` | aggregate | Q3 teams | PK + `(tenant, team_id, bucket_start DESC)` |
| `mart.agg_class_period` | aggregate | Q4 classes | PK |
| `mart.agg_user_period` | aggregate | Q6 one subject | PK + `(tenant, user_ref, bucket_start DESC, bucket_size)` |
| `mart.agg_device_period` | aggregate | Q7 history | PK |
| `mart.v_device_liveness` | list | Q7 current state, one row per device with its collectors summed | `ops.device (tenant, last_seen_at)` |
| `mart.v_person` | list | the people the directory and the devices know, by name | `ops.user_dim` PK |
| `ops.collector_state` | list | one device's collectors: state, cause (`error_code`), last report | PK |
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
| `mart.agg_tool_user_period` | `bucket`, `tool`, `sanctioned_state`, `subject` | `submissions`, `bytes_total` |
| `mart.agg_org_period` | `bucket`, `department`, `population`, `tool` | `submissions`, `users` |
| `mart.agg_team_period` | `bucket`, `team`, `tool` | `submissions`, `users` |
| `mart.agg_class_period` | `bucket`, `class`, `tool`, `severity`, `classifier_version` | `submissions`, `users`, `max_score`, `degraded_events` |
| `mart.agg_user_period` | `bucket`, `subject` | `submissions`, `bytes_total`, `tools_used`, `block_events` |
| `mart.agg_device_period` | `bucket`, `device`, `collector` | `healthy_days`, `degraded_days`, `absent_days`, `tampered_days`, `spool_dropped` |
| `mart.v_device_liveness` | `device`, `device_os`, `managed_state`, `region`, `liveness`, `collector` (set), `collector_state` (set) | — (a list) |
| `mart.v_person` | `subject`, `name`, `name_key` (lower-cased name, `starts_with`), `department`, `directory_status` | — (a list) |
| `ops.collector_state` | `device`, `collector`, `collector_state` | — (a list) |
| `ops.coverage_snapshot` | `snapshot_day`, `device`, `collector`, `gap_reason`, `observed`, `expected` | — |
| `ingest.submission` | `subject`, `tool`, `device`, `mode`, `action`, `content_state`, `route`, `detection_basis`, `prompt_kind`, `merge_confidence`, `confidence`, `department`, `population`, `manager` | — |
| `mart.v_finding` | `severity`, `rule`, `class`, `subject`, `tool`, `review_state`, `mode`, `decided_locally` | — |
| `ops.audit` | `actor_type`, `actor`, `action`, `object_type`, `subject`, `case` | — |

Extra filter-only columns (not groupable): on `ingest.submission` — `submission_id`, `received_at`,
`first_occurred_at`, `last_occurred_at`, `expires_at`, `size_bytes`, `observation_count`, and the
predicate `class`; on `mart.v_finding` — `detected_at`, `submission_id`, `rule_id`; on
`ops.audit` — `audit_seq`, `occurred_at`, `object_id`; on `mart.v_device_liveness` — `enrolled_at`,
`last_seen_at`, `revoked_at`, `spool_depth`, `spool_dropped_total`; on `mart.v_person` — `last_active_day`.

Every source that shows a tool returns the raw fingerprint in `tool` and a display name in
`tool_name`, resolved at read time from the shared `ref.tool_catalogue`, falling back to
`Unrecognised tool`. `sanctioned_state` is present-tense
configuration joined at read time: the tenant's decision on the tool the fingerprint belongs to
(`ops.tool_sanction` through the catalogue's `app_key`); NULL (no decision, or a fingerprint outside
the catalogue) renders as `unknown`, never `unsanctioned`.

**`mart.agg_user_period` requires a subject filter** (`subject eq` or `subject in`). Its row grain
*is* a person, so a read without one would be a list of people, which the product never offers. The
refusal is `unsupported_query_shape / subject_scope_required` and the fix is in `error.detail.fix`.

The window column is filterable under its own name (`received_at`, `detected_at`, `occurred_at`,
`snapshot_day`) with the comparison operators below.

### 2.3 Operators

`eq`, `ne`, `in`, `not_in`, `lt`, `lte`, `gt`, `gte`, `between`, `starts_with`, `is_null`.

* `in` / `not_in` take a non-empty array of ≤ **200** values.
* `between` takes exactly `[low, high]`.
* `is_null` takes no value. `eq: null` is rejected — use `is_null` (a `= NULL` predicate that
  silently matches nothing is exactly the class of bug this DSL exists to prevent).
* `starts_with` is permitted **only** on `tool` fingerprints, on aggregate sources, and on
  `mart.v_person`'s `name_key`, a lower-cased name for a case-insensitive search; on
  `ingest.submission` and `mart.v_finding` it is `unsupported_query_shape / no_covering_index`,
  and the error names `fix.alternative_source: mart.agg_tool_period`.
* Type rules: text/uuid → `eq ne in not_in is_null`; timestamp/date/number → those plus
  `lt lte gt gte between`; boolean → `eq ne is_null`.
* Values are typed: timestamps must be ISO-8601 UTC (`2026-09-01T00:00:00Z`), dates `YYYY-MM-DD`,
  identifiers canonical uuids, strings ≤ 256 characters. Nested objects and arrays are not values.
* `class` on `ingest.submission` is a **predicate**, not a dimension: `eq` only, compiled as
  `labels @> jsonb_build_array(jsonb_build_object('class', $n))` and served by the `labels` GIN.
* `prompt_kind` on `ingest.submission` filters through `coalesce(prompt_kind, 'unknown')`, so a
  row whose kind the device did not decide is never dropped by `ne`; the raw column is returned.

### 2.4 Buckets, grouping and ordering

* `bucket`: `hour` and `day` are native; `week` (ISO, Monday) and `month` are read-side reductions
  over day rows. Whichever is applied, exactly one `bucket_size` is pinned, so an hour row is never
  added to a day total. The applied bucket is returned in `meta.applied_bucket`.
* Omitting `bucket` on an aggregate means a single window total, computed from **day** rows.
* A series is at most 400 points. A bucket the server chose (a document sent with no `bucket`) is
  auto-coarsened and reported; a bucket the caller pinned (every template pins one) that cannot fit
  is refused with `fix.coarser_bucket`.
* `dimensions` ≤ 3, and `bucket` is not a dimension name (pass it as `bucket`).
* A list source does not group: grouping a list is `unsupported_query_shape`. Any time-bucketed
  number comes from `mart`.
* `order` is ≤ 3 terms, each naming a returned measure or a grouped dimension. The server then
  **appends the deterministic tie-break** — bucket first (DESC) when bucketed, then every grouped
  key ASC — so the ordering is total: `ORDER BY bucket DESC, <your terms>, <tie-break>`.
* `limit` on an aggregate truncates in that order, so it cuts whole buckets rather than returning a
  top-N per bucket; `meta.truncated` says whether it cut anything.
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
`max(...)`, which **never overstates**.

`max_score` is a `MAX`, not a sum. `submissions` on `mart.agg_class_period` counts submissions
*carrying that class*: one submission with three labels appears in three rows. The Q4 template
returns the non-additive total separately, from `mart.agg_tool_period`, as `meta.extras.class_total`.

---

## 3. The ten templates

Every template declares exactly these parameters; anything else is an error.

| Template | Source | Parameters | Notes |
|---|---|---|---|
| `q1_tools_ranked` | `mart.v_tool_usage` | `window`, `bucket`, `limit`, `tool`, `sanctioned_state` | rank by `submissions`, tie-break `tool`; `mart.v_tool_usage` has no `detections`/`rollup_events`/`degraded_events` (`meta.warnings` says so; use `mart.agg_tool_period`) |
| `q2_unsanctioned_users` | `mart.agg_tool_user_period` | `window`, `bucket`, `limit`, `tool`, `subject`, `sanctioned_state` | subject-bearing → audited; the state defaults to `unsanctioned`, and `unknown` is a separate call; the k cell is the `(bucket, tool)` group; ordered by tool then person, never by volume; one response bounded by `limit` (default and maximum 500); > 7 days unscoped is refused |
| `q3_team_growth` | `mart.agg_team_period` | `window`, `bucket`, `limit`, `team` | one series per team, named in `team_name`; the people with usage, those in any team, and the number of teams in `meta.extras.team_coverage` |
| `q4_class_mix` | `mart.agg_class_period` | `window`, `bucket`, `limit`, `dimensions`, `class`, `severity` | `dimensions` ⊆ `{class, tool, severity, classifier_version}`, ≤ 3; default `[class, severity]` |
| `q5_findings` | `mart.v_finding` | `window`, `limit`, `cursor`, `severity`, `rule`, `review_state`, `subject`, `tool`, `class` | review state `open` means nobody has looked |
| `q6_subject_series` | `mart.agg_user_period` | `subject` (**required**), `window`, `bucket`, `limit` | carries `meta.extras.flush_check` |
| `q7_devices` | `mart.v_device_liveness` | `limit`, `cursor`, `liveness`, `collector_state`, `collector`, `device_os`, `managed_state`, `region` | **no window**; four liveness values stay distinct; one row per device, with its collectors' states summed; `collector` and `collector_state` filter on the device's set; fleet-wide counts in `meta.extras.device_status` |
| `q8_activity` | `ingest.submission` | `window`, `limit`, `cursor`, `subject`, `tool`, `device`, `class`, `content_state`, `action`, `mode`, `department`, `prompt_kind`, `prompt_kind_not` | window ≤ 31 days; both clocks returned; `prompt_kind` includes only that kind and `prompt_kind_not` excludes it |
| `q9_event_detail` | `ingest.submission` | `submission_id` (**required**), `received_at_hint` | single record; `data` is one row per observation, each carrying the submission's columns |
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
  "coverage": { "window": { "from": "2026-09-03T00:00:00.000Z", "to": "2026-10-02T00:00:00.000Z" },
                "devices_reporting": 4180,
                "devices_enrolled": 4620, "gap_reasons": {"not_enrolled": 440}, "state": "partial" },
  "audit": { "entry_id": "8123", "written_at": "2026-10-02T11:05:01Z" },
  "meta": { "…": "see below" } }
```

Three properties are enforced in code, not by convention:

1. **`freshness` and `coverage` are always present on a data-bearing response.** The envelope
   builder refuses to construct one without them. `page` is present on a list read. `audit` is
   present exactly when an audit row was written; `audit.entry_id` makes a screenshot traceable.
2. **`result_state` is a first-class answer, not an error channel** (section 5).
3. **`data` never contains a hidden column.** Internal columns (`__ord_*`, `__recomputed_hash`)
   never reach the wire.

`meta` carries, at least: `source`, `kind`, `applied_bucket`, `native_bucket_size`, `reduced_from`,
`dimensions`, `measures`, `measure_semantics`, `order`, `limit`, `probe_row`, `truncated` (an
aggregate with a `limit`), `rollup`, `has_cursor`, `query_class`,
`statement_timeout_ms`, `required_indexes`, `warnings`, `joins_used`, `dsl_hash`,
`snapshot_upper_bound`, `coarsened`, `guard{estimated_cells,bounded_cells,limited,estimated_bytes}`,
`notes`, and `extras` when the template produced a side read. New keys may appear in `meta`,
`freshness` and `coverage` within `query_version: "1"`; `data`, `page`, `result_state` and the
error codes are the stable surface.

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
| `no_longer_available` | **410** | the record existed and was destroyed | with the reason and the erasure receipt |
| `not_found` | 404 | no such record, and no erasure receipt covers its window | "No such record"; `detail.purge_window_unknown` when no `received_at_hint` was given, `detail.retention_evidence: "no_ledger_in_schema"` because retention expiry leaves no per-row receipt |
| `unsupported_query_shape` | 400 | filter combination not servable | the fix, named |
| `query_too_broad` | 400 | over budget or over a cap | the coarser bucket / narrower window that fits |
| `cursor_expired` | 400 | a cursor that is expired, tampered, or from another query or tenant | restart from page one, told why |
| `audit_unavailable` | 503 | the transaction could not be completed and was rolled back: `audit_write_failed` when the audit insert failed, `read_failed` for anything else | **no data at all** |
| `busy` | 429 | shed by the admission gate, the request exceeded its time budget (`request_timeout`), or the statement hit its timeout, a lock timeout or a serialisation failure (`statement_timeout`, `lock_timeout`, `serialization_failure`) | retry |
| `unauthorised_role` | 403 / 401 | the role cannot make this read (403, `role`); 401 with `unauthenticated` when there is no valid token | which role is needed, or sign in |
| `audit_chain_broken` | 500 | a page's hash links do not verify; also `error.code: internal_error` for an unexpected defect | an integrity alert, not a list |
| `not_captured` / `not_retrievable` / `key_unavailable` | 200 / 200 / 503 | content states | as named |

Error body:

```json
{ "api_version": "1", "query_version": "1", "result_state": "query_too_broad",
  "error": { "code": "cost_estimate_exceeded", "message": "…", "fixable": true,
             "detail": { "max_cells": 2000, "estimated_cells": 33600,
                         "fix": { "coarser_bucket": "week" } } } }
```

A rejection never carries `data`, `freshness` or `coverage` — there is no number to qualify. On
`query_too_broad` / `unsupported_query_shape`, `error.detail.fix` is the action the user can take.

---

## 6. Cursor pagination

* Only list reads are paged. An aggregate takes no `cursor`; its response is bounded by `limit`.
* **Only `page.next_cursor: null` ends an iteration.** A short page is not the end: rows deleted
  mid-pagination (retention expiry, erasure) make a page shorter, and stopping there would truncate
  the result set.
* Pass the cursor back verbatim as `cursor`, with the same document otherwise. Treat it as opaque.
* A cursor is signed with the deployment's cursor key, so any replica accepts a cursor any replica
  issued. It is bound to the tenant, the normalised query and the ordering key, and expires after
  **15 minutes**. Every failure — expired, tampered, another tenant, another query, a changed
  ordering key after a deploy — is `cursor_expired` (400): restart from page one.
* `page.snapshot_upper_bound` freezes the window at the first page: every later page carries
  `<time column> <= upper`, so an insert cannot shift a boundary and a row is neither seen twice nor
  skipped. `page.newer_events_exist` announces rows that arrived since the snapshot.
* Never log a cursor in full or echo it in an error.

---

## 7. Cost guard and audit

### 8.1 What is refused, before anything runs

| Bound | Value | Refusal |
|---|---|---|
| Aggregate cells | 2,000 | `query_too_broad` naming the coarser bucket that fits |
| Aggregate with a `limit` | ≤ 2,000 rows | bounded by the limit; the full estimate is disclosed in `meta.guard` |
| List rows | 50 default, 500 max | `query_too_broad` on a document; a template `limit` above the template's own cap is `unsupported_query_shape / cost_estimate_exceeded` |
| Event/finding window | 31 days | `query_too_broad` |
| Audit window | 366 days | `query_too_broad` |
| Coverage window | 366 days | `query_too_broad` |
| Subject-grouped window | 7 days unless narrowed by a tool or subject filter | `query_too_broad` naming the narrowing |
| Time series | 400 points | auto-coarsened or refused (section 2.4) |
| Response body | 8 MB | `query_too_broad` |

Each read runs in one transaction with the statement timeout of its query class — aggregate 3 s,
list, audit and operational 5 s, single record 3 s, reported as `meta.statement_timeout_ms` — and a
lock timeout of half that. An overrun rolls the read back and answers `busy`. The process admits 8
requests to the database at once and queues 32; beyond that the answer is an immediate `busy`.

### 8.2 When a read is audited

Audited **before** the rows are served: any query that filters on `subject`; any query that returns
a subject reference; a single-record detail; any read of `ops.audit` (one row per query, not
re-audited).

Not audited: tool-, class- and team-level aggregates; coverage state; a device's collector rows
(`ops.collector_state`, which names no person); reference data.

Fail closed: if the audit row cannot be committed the transaction is rolled back, the response is
`503 audit_unavailable`, and **zero rows** are served. Nothing is streamed; a response is
materialised and committed before its first byte.
