// registry.js — the frozen allow-list of sources, dimensions, measures, operators and buckets.
//
// Every identifier that can reach SQL text lives here and nowhere else. The compiler never
// accepts an identifier from a request: it accepts a *name* and looks it up in this table,
// taking the `sql` string from the table. That is the whole of INV-3 (ADR 0003): the DSL is
// closed rather than escaped, so there is no escaping code to get wrong.
//
// Derivation. Every `sql` below is a column or expression that exists in db/schema.sql. The
// reviewable mapping is:
//
//   ops.tenant, ops.user_dim, ops.device, ops.collector_state, ops.tool, ops.audit,
//   ops.finding_review, ops.coverage_snapshot, ops.aggregate_watermark, ops.reconciliation_run,
//   ops.erasure_receipt, ingest.submission, ingest.observation, mart.finding,
//   mart.agg_tool_period, mart.agg_tool_user_period, mart.agg_class_period, mart.agg_org_period,
//   mart.agg_user_period, mart.agg_device_period, mart.v_tool_usage, mart.v_finding,
//   mart.v_device_liveness
//
// Nothing is offered that is not one of those. Nothing in ingest.search_text,
// ops.content_object or ops.policy_bundle is offered at all: sac_query holds no grant on the
// first two (db/schema.sql §10), and the third is configuration rather than an analytical fact.
//
// Cardinality estimates are used only by the cost guard (§12.2) to refuse an over-budget
// request before it runs. Each carries its source; the ones marked ASSUMPTION: are this
// package's estimate and are listed in DSL.md §9.

/** Wire/DSL version. Additive-only within a major; a renamed dimension is a new major. */
export const QUERY_VERSION = '1';
/** URL major version. */
export const API_VERSION = '1';
/** Small-cell floor (§6.2). */
export const K = 5;

// ---------------------------------------------------------------------------------------------
// Operators
// ---------------------------------------------------------------------------------------------

/**
 * The closed operator vocabulary of §2.4. Nothing else exists; anything else is rejected.
 * @type {ReadonlyArray<string>}
 */
export const OPERATORS = Object.freeze([
  'eq',
  'ne',
  'in',
  'not_in',
  'lt',
  'lte',
  'gt',
  'gte',
  'between',
  'starts_with',
  'is_null',
]);

/** Operators that take no value. */
export const NULLARY_OPERATORS = Object.freeze(['is_null']);
/** Operators that take an array value. */
export const ARRAY_OPERATORS = Object.freeze(['in', 'not_in']);
/** Operators that take a two-element value. */
export const RANGE_OPERATORS = Object.freeze(['between']);
/** Operators that take exactly one scalar value. */
export const SCALAR_OPERATORS = Object.freeze(['eq', 'ne', 'lt', 'lte', 'gt', 'gte', 'starts_with']);

/**
 * Operator applicability by field type. `starts_with` is deliberately absent everywhere: it is
 * granted per-dimension on `tool` alone (§2.4, §12.3).
 */
const OPS_BY_TYPE = Object.freeze({
  text: Object.freeze(['eq', 'ne', 'in', 'not_in', 'is_null']),
  uuid: Object.freeze(['eq', 'ne', 'in', 'not_in', 'is_null']),
  timestamp: Object.freeze(['eq', 'ne', 'in', 'not_in', 'lt', 'lte', 'gt', 'gte', 'between', 'is_null']),
  date: Object.freeze(['eq', 'ne', 'in', 'not_in', 'lt', 'lte', 'gt', 'gte', 'between', 'is_null']),
  number: Object.freeze(['eq', 'ne', 'in', 'not_in', 'lt', 'lte', 'gt', 'gte', 'between', 'is_null']),
  boolean: Object.freeze(['eq', 'ne', 'is_null']),
});

export function operatorsForType(type) {
  return OPS_BY_TYPE[type] ?? Object.freeze([]);
}

// ---------------------------------------------------------------------------------------------
// Buckets
// ---------------------------------------------------------------------------------------------

/**
 * Native buckets are the ones `mart.agg_*_period.bucket_size` admits (db/schema.sql: CHECK
 * bucket_size IN ('hour','day')). Week and month are read-side reductions over day rows and
 * must therefore pin `bucket_size = 'day'` — compiling a week reduction over hour rows would
 * multiply the same hour into several weeks' totals.
 */
export const NATIVE_BUCKETS = Object.freeze(['hour', 'day']);
/** Reduction buckets, coarsest last. Used for auto-coarsening beyond 400 series points (§7.5). */
export const REDUCTION_BUCKETS = Object.freeze(['week', 'month']);
export const BUCKETS = Object.freeze(['hour', 'day', 'week', 'month']);
/** The source bucket a reduction bucket is computed from. */
export const REDUCTION_SOURCE_BUCKET = 'day';
/**
 * Frozen SQL for the reduction buckets. The bucket name is validated against the closed list
 * first, and it is *still* mapped through this table rather than concatenated: `date_trunc`
 * takes a literal, and "validated then concatenated" is one edit away from "concatenated".
 */
export const BUCKET_TRUNC_SQL = Object.freeze({ week: "'week'", month: "'month'" });
/** Coarsening order for §7.5's "auto-coarsened, applied bucket named in freshness". */
export const COARSENING_ORDER = Object.freeze(['hour', 'day', 'week', 'month']);

// ---------------------------------------------------------------------------------------------
// Dimension and measure helpers
// ---------------------------------------------------------------------------------------------

/**
 * @typedef {object} Dimension
 * @property {string} name        DSL name (the only identifier a client may utter).
 * @property {string} sql         Frozen column reference resolved into SQL text.
 * @property {string} [orderSql]  Frozen reference used in ORDER BY / keyset when `sql` is nullable.
 * @property {string} type        text | uuid | timestamp | date | number | boolean
 * @property {boolean} nullable   true only when the column itself admits NULL.
 * @property {boolean} [startsWith] `starts_with` permitted (tool fingerprints only).
 * @property {number} cardinality Cost-guard estimate for this dimension in one tenant.
 * @property {string} cardinalitySource Where the estimate comes from.
 * @property {ReadonlyArray<string>} [values] Closed vocabulary, where the schema CHECK fixes one.
 */

/**
 * @param {string} name
 * @param {string} sql
 * @param {'text'|'uuid'|'timestamp'|'date'|'number'|'boolean'} type
 * @param {object} opts
 * @returns {Dimension}
 */
function dim(name, sql, type, opts = {}) {
  const operators = opts.startsWith
    ? Object.freeze([...operatorsForType(type), 'starts_with'])
    : operatorsForType(type);
  return Object.freeze({
    name,
    sql,
    orderSql: opts.orderSql ?? sql,
    type,
    nullable: opts.nullable ?? false,
    startsWith: opts.startsWith ?? false,
    cardinality: opts.cardinality ?? 100,
    cardinalitySource: opts.cardinalitySource ?? 'ASSUMPTION: no source in the document set',
    values: opts.values ? Object.freeze([...opts.values]) : undefined,
    operators,
  });
}

/**
 * @typedef {object} Measure
 * @property {string} name
 * @property {string} column        Frozen column reference (already an aggregate in `mart`).
 * @property {'sum'|'max'} agg      `sum` for additive counters, `max` for distinct-subject
 *                                  columns (see DSL.md §5.3: a lower bound never overstates).
 * @property {string} semantics     additive | distinct_lower_bound
 * @property {boolean} nonNegative
 */

/** @returns {Measure} */
function measure(name, column, opts = {}) {
  return Object.freeze({
    name,
    column,
    agg: opts.agg ?? 'sum',
    semantics: opts.semantics ?? 'additive',
    nonNegative: opts.nonNegative ?? true,
  });
}

// ---------------------------------------------------------------------------------------------
// Sources
// ---------------------------------------------------------------------------------------------

/**
 * @typedef {object} Source
 * @property {string} id             The DSL `source` value; also the SQL relation name.
 * @property {'aggregate'|'list'} kind
 * @property {string} from           Frozen FROM clause.
 * @property {string} tenantColumn   Frozen tenant column reference for the belt-and-braces predicate.
 * @property {string} label          Human name, used in errors and metadata.
 * @property {string} costClass      §12.1 query class.
 * @property {object|null} time      Window column: { name, sql, maxDays, maxDaysWhenNarrowed }
 * @property {object|null} bucket    { startColumn, sizeColumn, sizes }
 * @property {Record<string,Dimension>} dimensions
 * @property {Record<string,Measure>} measures
 * @property {ReadonlyArray<string>} grain   Source dimension names of the primary key (minus tenant, minus bucket_size)
 * @property {ReadonlyArray<{dim:string, dir:'asc'|'desc'}>} order  Total ordering key
 * @property {object|null} subjectCount  { column } | { distinct } | null
 * @property {boolean} kSuppression
 * @property {boolean} subjectBearing    true when the source's rows name a data subject
 * @property {boolean} requiresSubjectScope  grouping by `subject` needs an eq/in subject filter
 * @property {ReadonlyArray<string>} indexes  Indexes the shape requires (§3.11, §12.2)
 * @property {ReadonlyArray<{id:string,sql:string,when:ReadonlyArray<string>}>} [joins]
 * @property {ReadonlyArray<string>} [extraSelect] Frozen extra select-list entries
 * @property {ReadonlyArray<string>} [warnings]
 */

const TOOL_CARD = { cardinality: 200, cardinalitySource: 'docs/04 §3 sizing: "T = distinct tool fingerprints (tens to low hundreds)"' };
const USER_CARD = { cardinality: 4000, cardinalitySource: 'docs/04 §3 sizing: "U = AI-active users (<= 4,000)"' };
const DEPT_CARD = { cardinality: 40, cardinalitySource: 'docs/04 §3 sizing: "D = departments (tens)"' };
const CLASS_CARD = { cardinality: 7, cardinalitySource: 'docs/04 §3.4 "≈ 7 classes"; db/schema.sql seeds 7 ref.data_class rows' };
const SEVERITY_CARD = { cardinality: 4, cardinalitySource: 'db/schema.sql CHECK severity IN (low,medium,high,critical)' };
const ACTION_CARD = { cardinality: 3, cardinalitySource: 'db/schema.sql CHECK policy_action IN (blocked,warned,logged)' };
const MODE_CARD = { cardinality: 4, cardinalitySource: 'db/schema.sql CHECK collection_mode IN (m0,m1,m2,m3)' };
const CONTENT_STATE_CARD = { cardinality: 4, cardinalitySource: 'db/schema.sql CHECK content_state IN (not_captured,local_only,uploaded,shredded)' };
const KIND_CARD = { cardinality: 3, cardinalitySource: 'db/schema.sql CHECK kind IN (prompt,usage_rollup,model_detection)' };
const PROMPT_KIND_CARD = { cardinality: 3, cardinalitySource: 'db/schema.sql CHECK prompt_kind IN (user,client_generated,unknown)' };
const REVIEW_CARD = { cardinality: 3, cardinalitySource: 'db/schema.sql CHECK review_state IN (open,disputed,confirmed)' };
const ROUTE_CARD = { cardinality: 7, cardinalitySource: 'db/schema.sql seeds 7 ref.route_fidelity rows' };
const COLLECTOR_CARD = { cardinality: 6, cardinalitySource: 'db/schema.sql seeds 6 ref.collector rows; docs/04 §3.7 "5,000 × 6 collectors"' };
const COLLECTOR_STATE_CARD = { cardinality: 4, cardinalitySource: 'db/schema.sql CHECK state IN (healthy,degraded,absent,tampered)' };
const LIVENESS_CARD = { cardinality: 4, cardinalitySource: 'mart.v_device_liveness CASE yields reporting,stale,never_reported,revoked' };
const MANAGED_CARD = { cardinality: 3, cardinalitySource: 'db/schema.sql CHECK managed_state IN (managed,unmanaged,unknown)' };
const HOSTNAME_CARD = { cardinality: 5000, cardinalitySource: 'one hostname per device; docs/04 §3 sizing "<= 5,000 devices"' };
const AGENT_VERSION_CARD = { cardinality: 100, cardinalitySource: 'ASSUMPTION: a bounded number of agent builds across a fleet' };
const OS_CARD = { cardinality: 2, cardinalitySource: 'db/schema.sql CHECK os IN (windows,macos)' };
const GAP_CARD = { cardinality: 8, cardinalitySource: 'db/schema.sql CHECK gap_reason enumerates 8 values' };
const SANCTIONED_CARD = { cardinality: 3, cardinalitySource: 'db/schema.sql CHECK sanctioned_state IN (sanctioned,unsanctioned,unknown)' };
const DEVICE_CARD = { cardinality: 5000, cardinalitySource: 'docs/04 §3 sizing: "<= 5,000 devices"' };
const CONFIDENCE_CARD = { cardinality: 4, cardinalitySource: 'db/schema.sql CHECK confidence IN (high,medium,low,degraded)' };
const MERGE_CONF_CARD = { cardinality: 2, cardinalitySource: 'db/schema.sql CHECK merge_confidence IN (high,low)' };

/** The sealed-format guard: a `source` value is only ever one of these keys. */
export const SOURCES = Object.freeze({
  // -------------------------------------------------------------------------------------------
  // Q1 / Q2 — tool aggregates, with present-tense sanctioned state joined at read time.
  // -------------------------------------------------------------------------------------------
  'mart.v_tool_usage': Object.freeze({
    id: 'mart.v_tool_usage',
    kind: 'aggregate',
    label: 'Tool usage by bucket (mart.v_tool_usage)',
    from: 'mart.v_tool_usage t',
    tenantColumn: 't.tenant_id',
    costClass: 'aggregate',
    time: null,
    bucket: { startColumn: 't.bucket_start', sizeColumn: 't.bucket_size', sizes: NATIVE_BUCKETS },
    dimensions: Object.freeze({
      bucket: dim('bucket', 't.bucket_start', 'timestamp', { cardinalitySource: 'derived from the window and the applied bucket' }),
      tool: dim('tool', 't.tool_fingerprint', 'text', { startsWith: true, ...TOOL_CARD }),
      sanctioned_state: dim('sanctioned_state', 't.sanctioned_state', 'text', {
        nullable: true,
        ...SANCTIONED_CARD,
      }),
    }),
    measures: Object.freeze({
      submissions: measure('submissions', 't.submissions'),
      users: measure('users', 't.users', { agg: 'max', semantics: 'distinct_lower_bound' }),
      bytes_total: measure('bytes_total', 't.bytes_total'),
      blocked: measure('blocked', 't.blocked'),
      warned: measure('warned', 't.warned'),
      logged: measure('logged', 't.logged'),
    }),
    grain: Object.freeze(['bucket', 'tool']),
    order: Object.freeze([
      { dim: 'bucket', dir: 'desc' },
      { dim: 'tool', dir: 'asc' },
    ]),
    subjectCount: Object.freeze({ column: 't.users' }),
    kSuppression: true,
    subjectBearing: false,
    requiresSubjectScope: false,
    indexes: Object.freeze(['mart.agg_tool_period PK (tenant_id, bucket_start, bucket_size, tool_fingerprint)']),
    // The display name is joined at read time from ops.tool / ref.tool_catalogue through
    // ops.tool_display_name(); `tool` keeps returning the raw fingerprint, so the name can never
    // hide which behaviour-derived tool a row is about, and an unknown fingerprint is returned as
    // "Unrecognised tool" with `tool` beside it (brief §2, C8). It is selected only when `tool` is
    // a grouping dimension, because the name is a function of the grouped fingerprint.
    extraSelect: Object.freeze([
      Object.freeze({ sql: 'ops.tool_display_name(t.tool_fingerprint) AS "tool_name"', whenDimensions: Object.freeze(['tool']) }),
    ]),
    warnings: Object.freeze([
      'mart.v_tool_usage exposes no detections/rollup_events/degraded_events: a detection-only tool (mode I) or a rollup-only tool cannot appear in this source. Use source mart.agg_tool_period for those measures (docs/04 §3.1 "Gap", §4.6).',
    ]),
  }),

  'mart.agg_tool_period': Object.freeze({
    id: 'mart.agg_tool_period',
    kind: 'aggregate',
    label: 'Tool usage by bucket, full measure set (mart.agg_tool_period)',
    from: 'mart.agg_tool_period t',
    tenantColumn: 't.tenant_id',
    costClass: 'aggregate',
    time: null,
    bucket: { startColumn: 't.bucket_start', sizeColumn: 't.bucket_size', sizes: NATIVE_BUCKETS },
    dimensions: Object.freeze({
      bucket: dim('bucket', 't.bucket_start', 'timestamp', { cardinalitySource: 'derived from the window and the applied bucket' }),
      tool: dim('tool', 't.tool_fingerprint', 'text', { startsWith: true, ...TOOL_CARD }),
      sanctioned_state: dim('sanctioned_state', 'ot.sanctioned_state', 'text', { nullable: true, ...SANCTIONED_CARD }),
    }),
    measures: Object.freeze({
      submissions: measure('submissions', 't.submissions'),
      users: measure('users', 't.users', { agg: 'max', semantics: 'distinct_lower_bound' }),
      bytes_total: measure('bytes_total', 't.bytes_total'),
      blocked: measure('blocked', 't.blocked'),
      warned: measure('warned', 't.warned'),
      logged: measure('logged', 't.logged'),
      detections: measure('detections', 't.detections'),
      rollup_events: measure('rollup_events', 't.rollup_events'),
      degraded_events: measure('degraded_events', 't.degraded_events'),
    }),
    grain: Object.freeze(['bucket', 'tool']),
    order: Object.freeze([
      { dim: 'bucket', dir: 'desc' },
      { dim: 'tool', dir: 'asc' },
    ]),
    subjectCount: Object.freeze({ column: 't.users' }),
    kSuppression: true,
    subjectBearing: false,
    requiresSubjectScope: false,
    indexes: Object.freeze(['mart.agg_tool_period PK (tenant_id, bucket_start, bucket_size, tool_fingerprint)']),
    joins: Object.freeze([
      Object.freeze({
        id: 'tool',
        sql: 'LEFT JOIN ops.tool ot ON ot.tenant_id = t.tenant_id AND ot.tool_fingerprint = t.tool_fingerprint',
        when: Object.freeze(['sanctioned_state']),
      }),
    ]),
    extraSelect: Object.freeze([
      Object.freeze({ sql: 'ops.tool_display_name(t.tool_fingerprint) AS "tool_name"', whenDimensions: Object.freeze(['tool']) }),
    ]),
    warnings: Object.freeze([
      'sanctioned_state is present-tense configuration joined at read time (ops.tool), never a property of the aggregate row (docs/04 §4.1).',
    ]),
  }),

  // -------------------------------------------------------------------------------------------
  // Q2 — subject-bearing tool aggregates. Every read here is subject-level (§5.2) and audited.
  // -------------------------------------------------------------------------------------------
  'mart.agg_tool_user_period': Object.freeze({
    id: 'mart.agg_tool_user_period',
    kind: 'aggregate',
    label: 'Usage per tool per subject per bucket (mart.agg_tool_user_period)',
    from: 'mart.agg_tool_user_period a',
    tenantColumn: 'a.tenant_id',
    costClass: 'aggregate',
    time: null,
    bucket: { startColumn: 'a.bucket_start', sizeColumn: 'a.bucket_size', sizes: NATIVE_BUCKETS },
    dimensions: Object.freeze({
      bucket: dim('bucket', 'a.bucket_start', 'timestamp', { cardinalitySource: 'derived from the window and the applied bucket' }),
      tool: dim('tool', 'a.tool_fingerprint', 'text', { startsWith: true, ...TOOL_CARD }),
      sanctioned_state: dim('sanctioned_state', 'ot.sanctioned_state', 'text', { nullable: true, ...SANCTIONED_CARD }),
      subject: dim('subject', 'a.user_ref', 'text', { ...USER_CARD }),
    }),
    measures: Object.freeze({
      submissions: measure('submissions', 'a.submissions'),
      bytes_total: measure('bytes_total', 'a.bytes_total'),
    }),
    grain: Object.freeze(['bucket', 'tool', 'subject']),
    order: Object.freeze([
      { dim: 'bucket', dir: 'desc' },
      { dim: 'tool', dir: 'asc' },
      { dim: 'subject', dir: 'asc' },
    ]),
    // The suppression cell for Q2 is the tool, not the person: docs/04 §3.2 says "per-tool per-day
    // cells below k subjects suppressed", and a read that names who uses an unsanctioned tool must
    // suppress a tool used by fewer than k people while still naming the tool's users when there
    // are enough of them to make the count a fact about a group rather than about a person. The
    // window count `count(*)` over the grouped (bucket, tool) cell is exactly that number because
    // the grain is one row per subject per bucket; see compile.js.
    subjectCount: Object.freeze({ distinct: 'a.user_ref', perToolCell: true }),
    kSuppression: true,
    subjectBearing: true,
    requiresSubjectScope: false,
    indexes: Object.freeze([
      'mart.agg_tool_user_period PK (tenant_id, bucket_start, bucket_size, tool_fingerprint, user_ref)',
      'docs/04 §3.11 (tenant_id, tool_fingerprint, bucket_start DESC, bucket_size)',
    ]),
    // SANCTION is joined present-tense from ops.tool, exactly as Q1 does: a decision made after a
    // bucket was written changes what the next read says about it, and never rewrites the bucket.
    joins: Object.freeze([
      Object.freeze({
        id: 'tool',
        sql: 'LEFT JOIN ops.tool ot ON ot.tenant_id = a.tenant_id AND ot.tool_fingerprint = a.tool_fingerprint',
        when: Object.freeze(['sanctioned_state']),
      }),
    ]),
    extraSelect: Object.freeze([
      Object.freeze({ sql: 'ops.tool_display_name(a.tool_fingerprint) AS "tool_name"', whenDimensions: Object.freeze(['tool']) }),
    ]),
    warnings: Object.freeze([
      'docs/04 §3.2: an unscoped window beyond 7 days is refused by the cost guard when subject is a grouping dimension.',
      'sanctioned_state is present-tense configuration joined at read time (ops.tool), never a property of the aggregate row; NULL means no decision, rendered as `unknown`, never as `unsanctioned`.',
      'k-suppression applies to the (bucket, tool) cell, not the person: a tool used by fewer than k people is suppressed, and a tool with enough people publishes its per-person rows, which are the point of naming who uses it (docs/04 §3.2, §6.4).',
    ]),
  }),

  // -------------------------------------------------------------------------------------------
  // Q3 — team usage. Empty until the directory sync (Q2 of the master doc).
  // -------------------------------------------------------------------------------------------
  'mart.agg_org_period': Object.freeze({
    id: 'mart.agg_org_period',
    kind: 'aggregate',
    label: 'Usage per department and population per bucket (mart.agg_org_period)',
    from: 'mart.agg_org_period o',
    tenantColumn: 'o.tenant_id',
    costClass: 'aggregate',
    time: null,
    bucket: { startColumn: 'o.bucket_start', sizeColumn: 'o.bucket_size', sizes: NATIVE_BUCKETS },
    dimensions: Object.freeze({
      bucket: dim('bucket', 'o.bucket_start', 'timestamp', { cardinalitySource: 'derived from the window and the applied bucket' }),
      department: dim('department', 'o.department', 'text', { ...DEPT_CARD }),
      population: dim('population', 'o.population', 'text', {
        nullable: true,
        cardinality: 8,
        cardinalitySource: 'ASSUMPTION: ops.user_dim.population is a directory attribute; a handful of values',
        orderSql: "coalesce(o.population, chr(1))",
      }),
      tool: dim('tool', 'o.tool_fingerprint', 'text', { startsWith: true, ...TOOL_CARD }),
    }),
    measures: Object.freeze({
      submissions: measure('submissions', 'o.submissions'),
      users: measure('users', 'o.users', { agg: 'max', semantics: 'distinct_lower_bound' }),
    }),
    grain: Object.freeze(['bucket', 'department', 'tool', 'population']),
    order: Object.freeze([
      { dim: 'bucket', dir: 'desc' },
      { dim: 'department', dir: 'asc' },
      { dim: 'tool', dir: 'asc' },
      { dim: 'population', dir: 'asc' },
    ]),
    subjectCount: Object.freeze({ column: 'o.users' }),
    kSuppression: true,
    subjectBearing: false,
    requiresSubjectScope: false,
    indexes: Object.freeze([
      'mart.agg_org_period PK (tenant_id, bucket_start, bucket_size, department, tool_fingerprint, population)',
      'docs/04 §3.11 (tenant_id, department, bucket_start DESC)',
    ]),
    warnings: Object.freeze([
      'This source is empty for a tenant with no directory sync: the answer is not_yet_covered / directory_not_synced, never a zero line (docs/04 §3.3).',
      'o.department is NOT NULL in the table, so users with no directory row are absent here rather than present as an `unmapped` series; the q3 template computes the unmapped residual from mart.agg_tool_period.',
    ]),
  }),

  // -------------------------------------------------------------------------------------------
  // Q4 — data classes.
  // -------------------------------------------------------------------------------------------
  'mart.agg_class_period': Object.freeze({
    id: 'mart.agg_class_period',
    kind: 'aggregate',
    label: 'Sensitive-data class mix per bucket (mart.agg_class_period)',
    from: 'mart.agg_class_period c',
    tenantColumn: 'c.tenant_id',
    costClass: 'aggregate',
    time: null,
    bucket: { startColumn: 'c.bucket_start', sizeColumn: 'c.bucket_size', sizes: NATIVE_BUCKETS },
    dimensions: Object.freeze({
      bucket: dim('bucket', 'c.bucket_start', 'timestamp', { cardinalitySource: 'derived from the window and the applied bucket' }),
      class: dim('class', 'c.class_code', 'text', {
        ...CLASS_CARD,
        values: ['payment_card', 'government_id', 'credential', 'customer_pii', 'source_code', 'legal_commercial', 'health'],
      }),
      tool: dim('tool', 'c.tool_fingerprint', 'text', { startsWith: true, ...TOOL_CARD }),
      severity: dim('severity', 'c.severity', 'text', {
        ...SEVERITY_CARD,
        values: ['low', 'medium', 'high', 'critical'],
      }),
      classifier_version: dim('classifier_version', 'c.classifier_version', 'text', {
        cardinality: 50,
        cardinalitySource: 'ASSUMPTION: classifier releases accumulate over time; 50 is a conservative year-scale bound',
      }),
    }),
    measures: Object.freeze({
      submissions: measure('submissions', 'c.submissions'),
      users: measure('users', 'c.users', { agg: 'max', semantics: 'distinct_lower_bound' }),
      max_score: measure('max_score', 'c.max_score', { agg: 'max', nonNegative: false }),
      degraded_events: measure('degraded_events', 'c.degraded_events'),
    }),
    grain: Object.freeze(['bucket', 'class', 'tool', 'severity', 'classifier_version']),
    order: Object.freeze([
      { dim: 'bucket', dir: 'desc' },
      { dim: 'class', dir: 'asc' },
      { dim: 'tool', dir: 'asc' },
      { dim: 'severity', dir: 'asc' },
      { dim: 'classifier_version', dir: 'asc' },
    ]),
    subjectCount: Object.freeze({ column: 'c.users' }),
    kSuppression: true,
    subjectBearing: false,
    requiresSubjectScope: false,
    indexes: Object.freeze(['mart.agg_class_period PK (tenant_id, bucket_start, bucket_size, class_code, tool_fingerprint, severity, classifier_version)']),
    warnings: Object.freeze([
      'submissions counts submissions CARRYING that class: one submission with three labels contributes to three rows. Summing class rows and calling the result "submissions" overstates volume (docs/04 §3.4). The q4 template returns the non-additive total separately, from mart.agg_tool_period.',
      'max_score is MAX over the collapsed rows, not a sum: it is a score, not a counter.',
    ]),
  }),

  // -------------------------------------------------------------------------------------------
  // Q6 — one subject's series. Always audited (§3.6). Never a list of people (§11.2).
  // -------------------------------------------------------------------------------------------
  'mart.agg_user_period': Object.freeze({
    id: 'mart.agg_user_period',
    kind: 'aggregate',
    label: 'One subject per bucket (mart.agg_user_period)',
    from: 'mart.agg_user_period u',
    tenantColumn: 'u.tenant_id',
    costClass: 'aggregate',
    time: null,
    bucket: { startColumn: 'u.bucket_start', sizeColumn: 'u.bucket_size', sizes: NATIVE_BUCKETS },
    dimensions: Object.freeze({
      bucket: dim('bucket', 'u.bucket_start', 'timestamp', { cardinalitySource: 'derived from the window and the applied bucket' }),
      subject: dim('subject', 'u.user_ref', 'text', { ...USER_CARD }),
    }),
    measures: Object.freeze({
      submissions: measure('submissions', 'u.submissions'),
      bytes_total: measure('bytes_total', 'u.bytes_total'),
      tools_used: measure('tools_used', 'u.tools_used'),
      block_events: measure('block_events', 'u.block_events'),
    }),
    grain: Object.freeze(['bucket', 'subject']),
    order: Object.freeze([
      { dim: 'bucket', dir: 'desc' },
      { dim: 'subject', dir: 'asc' },
    ]),
    subjectCount: Object.freeze({ distinct: 'u.user_ref' }),
    kSuppression: false,
    subjectBearing: true,
    requiresSubjectScope: true,
    indexes: Object.freeze([
      'mart.agg_user_period PK (tenant_id, bucket_start, bucket_size, user_ref)',
      'docs/04 §3.11 (tenant_id, user_ref, bucket_start DESC, bucket_size)',
    ]),
    warnings: Object.freeze([
      'Carries no score, rank or efficiency measure, by construction (brief §1.2); the DSL cannot express one either.',
      'Requires a subject filter: an unfiltered read would enumerate people, which docs/04 §3.6 and §11.2 forbid.',
    ]),
  }),

  // -------------------------------------------------------------------------------------------
  // Q7 — device collection state.
  // -------------------------------------------------------------------------------------------
  'mart.agg_device_period': Object.freeze({
    id: 'mart.agg_device_period',
    kind: 'aggregate',
    label: 'Per-device collector state by bucket (mart.agg_device_period)',
    from: 'mart.agg_device_period d',
    tenantColumn: 'd.tenant_id',
    costClass: 'operational',
    time: null,
    bucket: { startColumn: 'd.bucket_start', sizeColumn: 'd.bucket_size', sizes: NATIVE_BUCKETS },
    dimensions: Object.freeze({
      bucket: dim('bucket', 'd.bucket_start', 'timestamp', { cardinalitySource: 'derived from the window and the applied bucket' }),
      device: dim('device', 'd.device_id', 'uuid', { ...DEVICE_CARD }),
      collector: dim('collector', 'd.collector', 'text', { ...COLLECTOR_CARD }),
    }),
    measures: Object.freeze({
      healthy_days: measure('healthy_days', 'd.healthy_days'),
      degraded_days: measure('degraded_days', 'd.degraded_days'),
      absent_days: measure('absent_days', 'd.absent_days'),
      tampered_days: measure('tampered_days', 'd.tampered_days'),
      spool_dropped: measure('spool_dropped', 'd.spool_dropped'),
    }),
    grain: Object.freeze(['bucket', 'device', 'collector']),
    order: Object.freeze([
      { dim: 'bucket', dir: 'desc' },
      { dim: 'device', dir: 'asc' },
      { dim: 'collector', dir: 'asc' },
    ]),
    subjectCount: null,
    kSuppression: false,
    subjectBearing: false,
    requiresSubjectScope: false,
    indexes: Object.freeze(['mart.agg_device_period PK (tenant_id, bucket_start, bucket_size, device_id, collector)']),
  }),

  'mart.v_device_liveness': Object.freeze({
    id: 'mart.v_device_liveness',
    kind: 'list',
    label: 'Devices with liveness and current collector state (mart.v_device_liveness ⋈ ops.collector_state)',
    from: 'mart.v_device_liveness d LEFT JOIN ops.collector_state cs ON cs.tenant_id = d.tenant_id AND cs.device_id = d.device_id',
    tenantColumn: 'd.tenant_id',
    costClass: 'operational',
    time: null,
    bucket: null,
    dimensions: Object.freeze({
      device: dim('device', 'd.device_id', 'uuid', { ...DEVICE_CARD }),
      hostname: dim('hostname', 'd.hostname', 'text', { nullable: true, ...HOSTNAME_CARD }),
      agent_version: dim('agent_version', 'd.agent_version', 'text', { nullable: true, ...AGENT_VERSION_CARD }),
      device_os: dim('device_os', 'd.os', 'text', { ...OS_CARD, values: ['windows', 'macos'] }),
      managed_state: dim('managed_state', 'd.managed_state', 'text', {
        ...MANAGED_CARD,
        values: ['managed', 'unmanaged', 'unknown'],
      }),
      collection_mode: dim('collection_mode', 'd.collection_mode', 'text', {
        ...MODE_CARD,
        nullable: true,
        values: ['m0', 'm1', 'm2', 'm3'],
      }),
      region: dim('region', 'dd.residency_region', 'text', {
        nullable: true,
        cardinality: 4,
        cardinalitySource: 'ASSUMPTION: residency regions per deployment; ops.tenant.residency_region is NOT NULL',
        orderSql: 'coalesce(dd.residency_region, chr(1))',
      }),
      liveness: dim('liveness', "CASE WHEN d.revoked_at IS NOT NULL THEN 'revoked' WHEN d.last_seen_at IS NULL THEN 'never_reported' WHEN d.last_seen_at < now() - interval '24 hours' THEN 'stale' ELSE 'reporting' END", 'text', {
        ...LIVENESS_CARD,
        values: ['reporting', 'stale', 'never_reported', 'revoked'],
      }),
      collector: dim('collector', 'cs.collector', 'text', {
        nullable: true,
        ...COLLECTOR_CARD,
        orderSql: "coalesce(cs.collector, chr(1))",
      }),
      collector_state: dim('collector_state', 'cs.state', 'text', {
        nullable: true,
        ...COLLECTOR_STATE_CARD,
        values: ['healthy', 'degraded', 'absent', 'tampered'],
      }),
    }),
    measures: Object.freeze({}),
    grain: Object.freeze(['device', 'collector']),
    order: Object.freeze([
      { dim: 'device', dir: 'asc' },
      { dim: 'collector', dir: 'asc' },
    ]),
    subjectCount: null,
    kSuppression: false,
    subjectBearing: true,
    requiresSubjectScope: false,
    indexes: Object.freeze([
      'ops.device PK (tenant_id, device_id), docs/04 §3.11 (tenant_id, last_seen_at)',
      'ops.collector_state PK (tenant_id, device_id, collector), docs/04 §3.11 (tenant_id, state)',
    ]),
    joins: Object.freeze([
      Object.freeze({
        // mart.v_device_liveness does not expose ops.device.residency_region, and §2.4 lists
        // `region` among the dimensions. The join is keyed on the device primary key and is
        // emitted only when the dimension is actually used.
        id: 'device_region',
        sql: 'LEFT JOIN ops.device dd ON dd.tenant_id = d.tenant_id AND dd.device_id = d.device_id',
        when: Object.freeze(['region']),
        // The column travels with the join: a SELECT list that names `dd` without the join is a
        // missing-FROM-clause error, and the integration test caught exactly that.
        select: Object.freeze(['dd.residency_region AS "region"']),
      }),
    ]),
    /**
     * The complete SELECT list of this bounded list. Dimensions on a list source are filters and
     * order keys rather than a grouping, so the output shape is frozen here instead of being
     * derived from the request: no filter can add a column to a response.
     */
    listSelect: Object.freeze([
      'd.device_id AS "device"',
      'd.hostname AS "hostname"',
      'd.os AS "device_os"',
      'd.os_version AS "os_version"',
      'd.agent_version AS "agent_version"',
      'd.managed_state AS "managed_state"',
      'd.collection_mode AS "collection_mode"',
      'd.last_user_ref AS "user_ref"',
      'd.last_subject_name AS "subject_name"',
      // The directory's own display name, current as of the last sync, resolved per row. It is a
      // scalar subquery rather than a join because the devices list is a frozen column list and a
      // join would only be emitted when a dimension happened to use it; this is one lookup on a
      // page-sized list. NULL when there is no sync, no display name, or a 'hashed' tenant (the
      // sync stores no clear name then). See ops.user_dim.display_name.
      '(SELECT ud.display_name FROM ops.user_dim ud WHERE ud.tenant_id = d.tenant_id AND ud.user_ref = d.last_user_ref) AS "directory_name"',
      "CASE WHEN d.revoked_at IS NOT NULL THEN 'revoked' WHEN d.last_seen_at IS NULL THEN 'never_reported' WHEN d.last_seen_at < now() - interval '24 hours' THEN 'stale' ELSE 'reporting' END AS \"liveness\"",
      'cs.collector AS "collector"',
      'cs.state AS "collector_state"',
      'd.enrolled_at AS enrolled_at',
      'd.revoked_at AS revoked_at',
      'd.last_seen_at AS last_seen_at',
      'd.collectors_reporting AS collectors_reporting',
      'cs.spool_depth AS spool_depth',
      'cs.spool_dropped_total AS spool_dropped_total',
      'cs.last_success_at AS last_success_at',
      'cs.last_report_at AS last_report_at',
      'cs.error_code AS error_code',
    ]),
    columns: Object.freeze({
      user_ref: dim('user_ref', 'd.last_user_ref', 'text', { nullable: true, cardinalitySource: 'the pseudonymous ref of the most recent submission' }),
      subject_name: dim('subject_name', 'd.last_subject_name', 'text', { nullable: true, cardinalitySource: 'the clear account name of the most recent submission' }),
      enrolled_at: dim('enrolled_at', 'd.enrolled_at', 'timestamp', { cardinalitySource: 'device timeline' }),
      last_seen_at: dim('last_seen_at', 'd.last_seen_at', 'timestamp', { nullable: true, cardinalitySource: 'device timeline' }),
      revoked_at: dim('revoked_at', 'd.revoked_at', 'timestamp', { nullable: true, cardinalitySource: 'device timeline' }),
      spool_dropped_total: dim('spool_dropped_total', 'cs.spool_dropped_total', 'number', { nullable: true, cardinalitySource: 'monotonic counter' }),
      spool_depth: dim('spool_depth', 'cs.spool_depth', 'number', { nullable: true, cardinalitySource: 'monotonic counter' }),
    }),
    warnings: Object.freeze([
      'docs/04 §3.7 names a three-way join with ops.coverage_snapshot. That join is NOT applied here: coverage_snapshot is one row per device per collector per DAY, so joining it without a snapshot_day predicate multiplies every device row by the number of days in the window. Coverage is a separate registered source (ops.coverage_snapshot) and the gap_reason breakdown is read from it.',
      'docs/04 §7.1 gives the device cursor as (device_id) alone. The row grain here is (device_id, collector) — the join fans out per collector — so the cursor is (device_id, collector); a device-only key would not be total and a keyset page would be inexact.',
      'docs/04 §3.7 says do not merge revoked with stale: liveness keeps four distinct values and never infers health from silence.',
    ]),
  }),

  'ops.coverage_snapshot': Object.freeze({
    id: 'ops.coverage_snapshot',
    kind: 'list',
    label: 'Coverage gaps (ops.coverage_snapshot)',
    from: 'ops.coverage_snapshot v',
    tenantColumn: 'v.tenant_id',
    costClass: 'operational',
    time: { name: 'snapshot_day', sql: 'v.snapshot_day', type: 'date', maxDays: 366 },
    bucket: null,
    dimensions: Object.freeze({
      snapshot_day: dim('snapshot_day', 'v.snapshot_day', 'date', { cardinalitySource: 'one per day in the window' }),
      device: dim('device', 'v.device_id', 'uuid', { ...DEVICE_CARD }),
      collector: dim('collector', 'v.collector', 'text', { ...COLLECTOR_CARD }),
      gap_reason: dim('gap_reason', 'v.gap_reason', 'text', {
        nullable: true,
        ...GAP_CARD,
        values: ['not_enrolled', 'not_managed', 'client_bypassed_proxy', 'pinned_certificate', 'permission_denied', 'process_excluded', 'tampered', 'unknown'],
      }),
      observed: dim('observed', 'v.observed', 'boolean', { cardinality: 2, cardinalitySource: 'boolean' }),
      expected: dim('expected', 'v.expected', 'boolean', { cardinality: 2, cardinalitySource: 'boolean' }),
    }),
    measures: Object.freeze({}),
    grain: Object.freeze(['snapshot_day', 'device', 'collector']),
    listSelect: Object.freeze([
      'v.snapshot_day AS "snapshot_day"',
      'v.device_id AS "device"',
      'v.collector AS "collector"',
      'v.expected AS "expected"',
      'v.observed AS "observed"',
      'v.gap_reason AS "gap_reason"',
    ]),
    order: Object.freeze([
      { dim: 'snapshot_day', dir: 'desc' },
      { dim: 'device', dir: 'asc' },
      { dim: 'collector', dir: 'asc' },
    ]),
    subjectCount: null,
    kSuppression: false,
    subjectBearing: false,
    requiresSubjectScope: false,
    indexes: Object.freeze([
      'ops.coverage_snapshot PK (tenant_id, snapshot_day, device_id, collector)',
      'docs/04 §3.11 partial (tenant_id, snapshot_day) WHERE NOT observed',
    ]),
    warnings: Object.freeze([
      'gap_reason is NULL exactly when observed is true (CHECK coverage_gap_requires_reason), so `is_null` on gap_reason means "this collector reported".',
    ]),
  }),

  // -------------------------------------------------------------------------------------------
  // Q8 — the one path that touches event rows, and a bounded list (§3.8, C29).
  // -------------------------------------------------------------------------------------------
  'ingest.submission': Object.freeze({
    id: 'ingest.submission',
    kind: 'list',
    label: 'Bounded event list (ingest.submission)',
    from: 'ingest.submission s',
    tenantColumn: 's.tenant_id',
    costClass: 'event_list',
    time: { name: 'received_at', sql: 's.received_at', type: 'timestamp', maxDays: 31 },
    bucket: null,
    dimensions: Object.freeze({
      subject: dim('subject', 's.user_ref', 'text', { ...USER_CARD }),
      tool: dim('tool', 's.tool_fingerprint', 'text', { startsWith: true, ...TOOL_CARD }),
      device: dim('device', 's.device_id', 'uuid', { ...DEVICE_CARD }),
      mode: dim('mode', 's.collection_mode', 'text', { ...MODE_CARD, values: ['m0', 'm1', 'm2', 'm3'] }),
      action: dim('action', 's.policy_action', 'text', {
        nullable: true,
        ...ACTION_CARD,
        values: ['blocked', 'warned', 'logged'],
      }),
      content_state: dim('content_state', 's.content_state', 'text', {
        ...CONTENT_STATE_CARD,
        values: ['not_captured', 'local_only', 'uploaded', 'shredded'],
      }),
      route: dim('route', 's.winning_source', 'text', { ...ROUTE_CARD }),
      detection_basis: dim('detection_basis', 's.kind', 'text', {
        ...KIND_CARD,
        values: ['prompt', 'usage_rollup', 'model_detection'],
      }),
      // The request kind (task 08). NULL means the device did not decide, so the dimension is
      // compiled through coalesce(..., 'unknown'): a NULL row filters as `unknown`, and the `ne`
      // operator (the dashboard's default-hide of client_generated) does not drop it. The raw
      // column still travels in listSelect so a NULL row renders as `unknown` on the client.
      prompt_kind: dim('prompt_kind', "coalesce(s.prompt_kind, 'unknown')", 'text', {
        ...PROMPT_KIND_CARD,
        values: ['user', 'client_generated', 'unknown'],
      }),
      merge_confidence: dim('merge_confidence', 's.merge_confidence', 'text', {
        ...MERGE_CONF_CARD,
        values: ['high', 'low'],
      }),
      confidence: dim('confidence', 's.confidence', 'text', {
        nullable: true,
        ...CONFIDENCE_CARD,
        values: ['high', 'medium', 'low', 'degraded'],
      }),
      department: dim('department', 'ud.department', 'text', { nullable: true, ...DEPT_CARD }),
      population: dim('population', 'ud.population', 'text', {
        nullable: true,
        cardinality: 8,
        cardinalitySource: 'ASSUMPTION: ops.user_dim.population is a directory attribute; a handful of values',
        orderSql: "coalesce(ud.population, chr(1))",
      }),
      manager: dim('manager', 'ud.manager_ref', 'text', {
        nullable: true,
        ...USER_CARD,
        orderSql: "coalesce(ud.manager_ref, chr(1))",
      }),
    }),
    /**
     * Filter-only fields. `class` on an event row is a predicate over the `labels` jsonb
     * column, not a dimension: `labels` is an array of {class, score} (contracts/
     * event-envelope.schema.json $defs.label), so grouping by it would need an unnest that
     * C27 forbids on the event table. docs/04 §3.8: "A GIN index on labels serves the class
     * filter: label search".
     */
    predicates: Object.freeze({
      class: Object.freeze({
        name: 'class',
        type: 'text',
        nullable: false,
        startsWith: false,
        cardinality: 7,
        cardinalitySource: 'db/schema.sql seeds 7 ref.data_class rows',
        // `labels` is an array, so the value is wrapped in a one-element array too:
        // `labels @> [{"class": $n}]`. An object operand always matches nothing. jsonb_path_ops
        // serves this form.
        operators: Object.freeze(['eq']),
        sql: 's.labels',
        containment: true,
      }),
    }),
    measures: Object.freeze({}),
    grain: Object.freeze(['received_at', 'submission_id']),
    order: Object.freeze([
      { dim: 'received_at', dir: 'desc' },
      { dim: 'submission_id', dir: 'desc' },
    ]),
    subjectCount: null,
    kSuppression: false,
    subjectBearing: true,
    requiresSubjectScope: false,
    indexes: Object.freeze([
      'docs/04 §3.11 submission_by_received (tenant_id, received_at DESC, submission_id DESC)',
      'docs/04 §3.11 submission_by_user (tenant_id, user_ref, received_at DESC)',
      'docs/04 §3.11 submission_by_tool (tenant_id, tool_fingerprint, received_at DESC)',
      'docs/04 §3.11 submission_by_device (tenant_id, device_id, received_at DESC)',
      'docs/04 §3.11 submission_labels_gin USING GIN (labels jsonb_path_ops)',
    ]),
    joins: Object.freeze([
      Object.freeze({
        id: 'user_dim',
        sql: 'LEFT JOIN ops.user_dim ud ON ud.tenant_id = s.tenant_id AND ud.user_ref = s.user_ref',
        when: Object.freeze(['department', 'population', 'manager']),
        select: Object.freeze([
          'ud.department AS "department"',
          'ud.population AS "population"',
          'ud.manager_ref AS "manager"',
        ]),
      }),
    ]),
    listSelect: Object.freeze([
      's.submission_id AS submission_id',
      's.received_at AS received_at',
      's.first_occurred_at AS first_occurred_at',
      's.last_occurred_at AS last_occurred_at',
      's.user_ref AS "subject"',
      's.tool_fingerprint AS "tool"',
      // The display name joined at read time; the raw fingerprint travels beside it in `tool`.
      'ops.tool_display_name(s.tool_fingerprint) AS "tool_name"',
      's.device_id AS "device"',
      's.collection_mode AS "mode"',
      's.policy_action AS "action"',
      's.policy_rule_id AS policy_rule_id',
      's.content_state AS "content_state"',
      's.shredded_reason AS shredded_reason',
      's.winning_source AS "route"',
      's.kind AS "detection_basis"',
      's.prompt_kind AS "prompt_kind"',
      's.merge_confidence AS "merge_confidence"',
      's.confidence AS "confidence"',
      's.observation_count AS observation_count',
      's.labels AS labels',
      's.size_bytes AS size_bytes',
      's.winning_fidelity AS winning_fidelity',
      's.observed_routes AS observed_routes',
    ]),
    /** Columns usable as filters and as order keys but not as grouping dimensions. */
    columns: Object.freeze({
      submission_id: dim('submission_id', 's.submission_id', 'uuid', { cardinalitySource: 'primary key' }),
      received_at: dim('received_at', 's.received_at', 'timestamp', { cardinalitySource: 'event timeline' }),
      first_occurred_at: dim('first_occurred_at', 's.first_occurred_at', 'timestamp', { cardinalitySource: 'event timeline' }),
      last_occurred_at: dim('last_occurred_at', 's.last_occurred_at', 'timestamp', { cardinalitySource: 'event timeline' }),
      expires_at: dim('expires_at', 's.expires_at', 'timestamp', { cardinalitySource: 'retention' }),
      size_bytes: dim('size_bytes', 's.size_bytes', 'number', { nullable: true, cardinalitySource: 'size distribution' }),
      observation_count: dim('observation_count', 's.observation_count', 'number', { cardinalitySource: 'routes per submission' }),
    }),
    warnings: Object.freeze([
      'No bucketing and no measures on this source: any time-bucketed number must come from mart (C27), and this source exists only to serve a bounded, cursor-paged list (§3.8).',
      'Ordering and windowing are on received_at, the server-assigned clock (brief §3.6). first_occurred_at / last_occurred_at are returned beside it, never normalised into it (C26).',
    ]),
  }),

  // -------------------------------------------------------------------------------------------
  // Q5 — findings.
  // -------------------------------------------------------------------------------------------
  'mart.v_finding': Object.freeze({
    id: 'mart.v_finding',
    kind: 'list',
    label: 'Findings with review state (mart.v_finding)',
    from: 'mart.v_finding f',
    tenantColumn: 'f.tenant_id',
    costClass: 'event_list',
    time: { name: 'detected_at', sql: 'f.detected_at', type: 'timestamp', maxDays: 31 },
    bucket: null,
    dimensions: Object.freeze({
      severity: dim('severity', 'f.severity', 'text', { ...SEVERITY_CARD, values: ['low', 'medium', 'high', 'critical'] }),
      rule: dim('rule', 'f.rule_id', 'text', {
        cardinality: 200,
        cardinalitySource: 'ASSUMPTION: ref.rule is a curated rule set; a few hundred rules',
      }),
      class: dim('class', 'f.class_code', 'text', {
        ...CLASS_CARD,
        values: ['payment_card', 'government_id', 'credential', 'customer_pii', 'source_code', 'legal_commercial', 'health'],
      }),
      subject: dim('subject', 'f.user_ref', 'text', { ...USER_CARD }),
      tool: dim('tool', 'f.tool_fingerprint', 'text', { startsWith: true, ...TOOL_CARD }),
      review_state: dim('review_state', 'f.review_state', 'text', {
        ...REVIEW_CARD,
        values: ['open', 'disputed', 'confirmed'],
      }),
      mode: dim('mode', 'f.collection_mode', 'text', { ...MODE_CARD, values: ['m0', 'm1', 'm2', 'm3'] }),
      decided_locally: dim('decided_locally', 'f.decided_locally', 'boolean', { cardinality: 2, cardinalitySource: 'boolean' }),
    }),
    measures: Object.freeze({}),
    grain: Object.freeze(['detected_at', 'submission_id', 'rule']),
    order: Object.freeze([
      { dim: 'detected_at', dir: 'desc' },
      { dim: 'submission_id', dir: 'desc' },
      { dim: 'rule', dir: 'asc' },
    ]),
    subjectCount: null,
    kSuppression: false,
    subjectBearing: true,
    requiresSubjectScope: false,
    indexes: Object.freeze([
      'docs/04 §3.11 mart.finding (tenant_id, detected_at DESC, submission_id)',
      'mart.finding PK (tenant_id, submission_id, rule_id)',
    ]),
    listSelect: Object.freeze([
      'f.submission_id AS submission_id',
      'f.detected_at AS detected_at',
      'f.rule_id AS "rule"',
      'f.rule_title AS rule_title',
      'f.class_code AS "class"',
      'f.severity AS "severity"',
      'f.user_ref AS "subject"',
      'f.tool_fingerprint AS "tool"',
      'ops.tool_display_name(f.tool_fingerprint) AS "tool_name"',
      'f.collection_mode AS "mode"',
      'f.decided_locally AS "decided_locally"',
      'f.review_state AS "review_state"',
      'f.reviewed_by AS reviewed_by',
      'f.reviewed_at AS reviewed_at',
      'f.policy_action AS policy_action',
    ]),
    columns: Object.freeze({
      detected_at: dim('detected_at', 'f.detected_at', 'timestamp', { cardinalitySource: 'finding timeline' }),
      submission_id: dim('submission_id', 'f.submission_id', 'uuid', { cardinalitySource: 'primary key part' }),
      rule_id: dim('rule_id', 'f.rule_id', 'text', { cardinalitySource: 'rule set' }),
    }),
    warnings: Object.freeze([
      'review_state comes from the view as coalesce(ops.finding_review.review_state, \'open\'). `open` means nobody has looked; it is not "reviewed and unremarkable" (docs/04 §3.5).',
      'severity and class are present-tense: mart.v_finding reads them from the current ref.rule row, so editing a rule shows on every finding that names it (backlog/03-findings/DECISIONS.md).',
    ]),
  }),

  // -------------------------------------------------------------------------------------------
  // Q10 — the audit trail. Reading it is itself a subject-level read, audited once per query.
  // -------------------------------------------------------------------------------------------
  'ops.audit': Object.freeze({
    id: 'ops.audit',
    kind: 'list',
    label: 'Audit trail (ops.audit)',
    from: 'ops.audit au',
    tenantColumn: 'au.tenant_id',
    costClass: 'audit',
    time: { name: 'occurred_at', sql: 'au.occurred_at', type: 'timestamp', maxDays: 366 },
    bucket: null,
    dimensions: Object.freeze({
      actor_type: dim('actor_type', 'au.actor_type', 'text', {
        cardinality: 4,
        cardinalitySource: 'db/schema.sql CHECK actor_type IN (user,device,service,system)',
        values: ['user', 'device', 'service', 'system'],
      }),
      actor: dim('actor', 'au.actor_id', 'text', {
        cardinality: 500,
        cardinalitySource: 'ASSUMPTION: analysts plus service identities per tenant',
      }),
      action: dim('action', 'au.action', 'text', {
        cardinality: 40,
        cardinalitySource: 'ASSUMPTION: closed set of audited actions, grown additively',
      }),
      object_type: dim('object_type', 'au.object_type', 'text', {
        cardinality: 20,
        cardinalitySource: 'ASSUMPTION: submission, finding, content_object, export, ...',
      }),
      subject: dim('subject', 'au.subject_ref', 'text', { nullable: true, ...USER_CARD }),
      case: dim('case', 'au.case_reference', 'text', {
        nullable: true,
        cardinality: 500,
        cardinalitySource: 'ASSUMPTION: open cases per tenant',
      }),
    }),
    measures: Object.freeze({}),
    grain: Object.freeze(['occurred_at', 'audit_seq']),
    order: Object.freeze([
      { dim: 'occurred_at', dir: 'desc' },
      { dim: 'audit_seq', dir: 'desc' },
    ]),
    subjectCount: null,
    kSuppression: false,
    subjectBearing: true,
    requiresSubjectScope: false,
    indexes: Object.freeze([
      'ops.audit PK (tenant_id, audit_seq)',
      'docs/04 §3.11 audit_by_time (tenant_id, occurred_at DESC, audit_seq DESC)',
      'docs/04 §3.11 (tenant_id, object_type, object_id)',
    ]),
    /**
     * Chain verification is computed in SQL, by the same expression the ops.audit_chain()
     * trigger uses. Doing it in JS would duplicate `detail::text`'s exact jsonb rendering and
     * would therefore produce false alarms; asking the database that owns the format cannot.
     */
    listSelect: Object.freeze([
      'au.audit_seq AS audit_seq',
      'au.occurred_at AS occurred_at',
      'au.actor_type AS "actor_type"',
      'au.actor_id AS "actor"',
      'au.action AS "action"',
      'au.object_type AS "object_type"',
      'au.object_id AS object_id',
      'au.subject_ref AS "subject"',
      'au.case_reference AS "case"',
      'au.detail AS detail',
      'au.prev_hash AS prev_hash',
      'au.row_hash AS row_hash',
      'encode(sha256(convert_to(concat_ws(E\'\\x1f\'::text, au.tenant_id::text, au.audit_seq::text, ' +
        "to_char(au.occurred_at AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS.US'), au.actor_type, au.actor_id, " +
        "au.action, au.object_type, coalesce(au.object_id, ''), coalesce(au.subject_ref, ''), " +
        "coalesce(au.case_reference, ''), au.detail::text, coalesce(au.prev_hash, '')), 'UTF8')), 'hex') " +
        'AS __recomputed_hash',
      'lag(au.prev_hash) OVER (ORDER BY au.occurred_at DESC, au.audit_seq DESC) AS __newer_prev_hash',
    ]),
    columns: Object.freeze({
      audit_seq: dim('audit_seq', 'au.audit_seq', 'number', { cardinalitySource: 'identity sequence' }),
      occurred_at: dim('occurred_at', 'au.occurred_at', 'timestamp', { cardinalitySource: 'audit timeline' }),
      object_id: dim('object_id', 'au.object_id', 'text', { nullable: true, cardinalitySource: 'object identity' }),
    }),
    warnings: Object.freeze([
      'Chain verification here covers the returned page only: whole-chain verification is the reconciler\'s job (docs/04 §3.10). A mismatch returns audit_chain_broken (500) instead of a list that looks fine.',
      'One audit row per query is written for a read of this table, and that row is not re-audited (docs/04 §3.10, ASSUMPTION: one-level recursion is the only terminating reading of C30).',
    ]),
  }),
});

/** Every source id, sorted, for error messages and tests. */
export const SOURCE_IDS = Object.freeze(Object.keys(SOURCES).sort());

// ---------------------------------------------------------------------------------------------
// Query classes (§12.1) — timeout and cap per class, and the page-size bounds of §7.5.
// ---------------------------------------------------------------------------------------------

export const QUERY_CLASSES = Object.freeze({
  aggregate: Object.freeze({ statementTimeoutMs: 3000, maxCells: 2000, defaultPageSize: null, maxPageSize: null, capKind: 'cells' }),
  event_list: Object.freeze({ statementTimeoutMs: 5000, maxCells: null, defaultPageSize: 50, maxPageSize: 500, capKind: 'rows' }),
  operational: Object.freeze({ statementTimeoutMs: 5000, maxCells: null, defaultPageSize: 50, maxPageSize: 500, capKind: 'rows' }),
  audit: Object.freeze({ statementTimeoutMs: 5000, maxCells: null, defaultPageSize: 50, maxPageSize: 500, capKind: 'rows' }),
  single: Object.freeze({ statementTimeoutMs: 3000, maxCells: 1, defaultPageSize: 1, maxPageSize: 1, capKind: 'rows' }),
  export_planning: Object.freeze({ statementTimeoutMs: 10000, maxCells: null, defaultPageSize: null, maxPageSize: null, capKind: 'none' }),
});

/** §7.5: time series ≤ 400 points, response body ≤ 8 MB. */
export const MAX_SERIES_POINTS = 400;
export const MAX_RESPONSE_BYTES = 8 * 1024 * 1024;
/** §7.5 / §12.1: a list window, and the cap that applies once a narrowing predicate is present. */
export const MAX_LIST_WINDOW_DAYS = 31;
export const MAX_UNNARROWED_SUBJECT_WINDOW_DAYS = 7;
/** §6.2 small-cell floor. */
export const SUPPRESSION_K = K;
/** §7.2: cursor lifetime. */
export const CURSOR_TTL_MS = 15 * 60 * 1000;
/** §4.5: staleness bound is 3× the five-minute cadence. */
export const FRESHNESS_CADENCE_MS = 5 * 60 * 1000;
export const FRESHNESS_STALE_MULTIPLIER = 3;

/** The aggregate a source's freshness comes from, for the `freshness` block (§4.5). */
export const SOURCE_WATERMARK = Object.freeze({
  'mart.v_tool_usage': 'mart.agg_tool_period',
  'mart.agg_tool_period': 'mart.agg_tool_period',
  'mart.agg_tool_user_period': 'mart.agg_tool_user_period',
  'mart.agg_org_period': 'mart.agg_org_period',
  'mart.agg_class_period': 'mart.agg_class_period',
  'mart.agg_user_period': 'mart.agg_user_period',
  'mart.agg_device_period': 'mart.agg_device_period',
  'mart.v_device_liveness': null,
  'ops.coverage_snapshot': null,
  'ingest.submission': null,
  'mart.v_finding': null,
  'ops.audit': null,
});

/** Maximum filters, list membership, and string length in one request (§12.2 bounds). */
export const MAX_FILTERS = 16;
export const MAX_IN_VALUES = 200;
export const MAX_VALUE_LENGTH = 256;
export const MAX_JSON_DEPTH = 4;
export const MAX_DIMENSIONS = 3;
