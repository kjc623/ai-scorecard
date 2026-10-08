// fixtures.mjs — canned envelopes for the tests: one per result state the API documents, an answer
// per question in query-api's response shapes, and a transport that serves them.
//
// They are not a happy path: the tests drive every screen through the states that say "we cannot
// say" as well as the ones that carry data.

const DAY = '2026-09-30T00:00:00Z';
const WINDOW = Object.freeze({ from: '2026-09-24T00:00:00Z', to: '2026-10-01T00:00:00Z' });

/** A watermark block. `stale` is three cadences past the last run. */
export function freshness(overrides = {}) {
  const state = overrides.state ?? 'fresh';
  return Object.freeze({
    aggregate: overrides.aggregate ?? 'mart.agg_tool_period',
    bucket_size: overrides.bucket_size ?? 'day',
    last_run_at: overrides.last_run_at ?? '2026-10-01T11:57:00Z',
    last_complete_bucket: overrides.last_complete_bucket ?? '2026-10-01T11:00:00Z',
    last_run_rows: overrides.last_run_rows ?? 1204,
    lag_seconds: state === 'fresh' ? (overrides.lag_seconds ?? 180) : (overrides.lag_seconds ?? 5400),
    state,
    ...(overrides.reason ? { reason: overrides.reason } : {}),
  });
}

/** A coverage block. The enrolled denominator is always present, never implied. */
export function coverage(overrides = {}) {
  const state = overrides.state ?? 'complete';
  const enrolled = overrides.devices_enrolled ?? 4620;
  const reporting = overrides.devices_reporting ?? (state === 'partial' ? 4180 : enrolled);
  return Object.freeze({
    window: overrides.window ?? ['2026-09-24', '2026-10-01'],
    devices_reporting: reporting,
    devices_enrolled: enrolled,
    expected_collector_days: overrides.expected_collector_days ?? 27720,
    observed_collector_days: overrides.observed_collector_days ?? (state === 'partial' ? 25080 : 27720),
    gap_reasons: Object.freeze(overrides.gap_reasons ?? (state === 'partial' ? { not_enrolled: 440, unknown: 12 } : {})),
    gaps_total: overrides.gaps_total ?? (state === 'partial' ? 452 : 0),
    state,
    ...(overrides.reason ? { reason: overrides.reason } : {}),
  });
}

function page(overrides = {}) {
  return Object.freeze({
    returned: overrides.returned ?? 0,
    next_cursor: overrides.next_cursor ?? null,
    snapshot_upper_bound: overrides.snapshot_upper_bound ?? '2026-10-01T11:58:00Z',
    newer_events_exist: overrides.newer_events_exist ?? false,
  });
}

const TOOL_ROWS = Object.freeze([
  Object.freeze({ bucket: DAY, tool: 'tls_b6681b043244c43f', sanctioned_state: 'unsanctioned', submissions: 812, users: 214, bytes_total: 41_000_000, blocked: 12, warned: 40, logged: 760 }),
  // Below k: the server has already replaced every measure with the suppression marker. The
  // client must not turn this back into a number.
  Object.freeze({ bucket: DAY, tool: 'shadow_llm_gateway', sanctioned_state: 'unknown', result_state: 'suppressed', reason: 'fewer_than_k_subjects', k: 5 }),
  // A genuine zero: we looked and there was none. This is an answer, not a secret.
  Object.freeze({ bucket: DAY, tool: 'legacy_summariser', sanctioned_state: 'sanctioned', submissions: 0, users: 0, bytes_total: 0, blocked: 0, warned: 0, logged: 0 }),
  Object.freeze({ bucket: DAY, tool: 'tls_f412811be7ac6539', sanctioned_state: 'sanctioned', submissions: 4021, users: 1877, bytes_total: 190_000_000, blocked: 3, warned: 210, logged: 3808 }),
]);

const DEVICE_ROWS = Object.freeze([
  Object.freeze({ device: '9f1c0b6e-0000-4000-8000-000000000001', hostname: 'FIN-LAPTOP-07', device_os: 'windows', os_version: '11', agent_version: '1.4.2', collection_mode: 'm3', managed_state: 'managed', user_ref: 'u_4f21', subject_name: 'alice@contoso.example', directory_name: 'Alice Smith', region: 'eu', liveness: 'reporting', collector: 'capture_extension', collector_state: 'healthy', spool_depth: 0, spool_dropped_total: 0, last_seen_at: '2026-10-01T11:57:00Z' }),
  Object.freeze({ device: '9f1c0b6e-0000-4000-8000-000000000002', hostname: 'MAC-DESIGN-2', device_os: 'macos', os_version: '15', agent_version: '1.4.2', collection_mode: 'm2', managed_state: 'managed', user_ref: 'u_9a02', subject_name: 'bob@contoso.example', region: 'eu', liveness: 'stale', collector: 'egress_proxy', collector_state: 'degraded', spool_depth: 812, spool_dropped_total: 0, last_seen_at: '2026-09-29T02:11:00Z' }),
  Object.freeze({ device: '9f1c0b6e-0000-4000-8000-000000000003', hostname: null, device_os: 'windows', os_version: '10', agent_version: null, collection_mode: 'm1', managed_state: 'unmanaged', user_ref: null, subject_name: null, region: 'us', liveness: 'never_reported', collector: null, collector_state: null, spool_depth: null, spool_dropped_total: null, last_seen_at: null }),
  Object.freeze({ device: '9f1c0b6e-0000-4000-8000-000000000004', hostname: 'OPS-BUILD-4', device_os: 'windows', os_version: '11', agent_version: '1.3.0', collection_mode: 'm0', managed_state: 'managed', user_ref: 'u_1b77', subject_name: 'svc-build', region: 'us', liveness: 'revoked', collector: 'cli_shim', collector_state: 'absent', spool_depth: 0, spool_dropped_total: 4412, last_seen_at: '2026-09-12T09:00:00Z' }),
]);

const ACTIVITY_ROWS = Object.freeze([
  Object.freeze({
    submission_id: '11111111-2222-4333-8444-555555555551',
    received_at: '2026-09-30T14:02:11Z',
    first_occurred_at: '2026-09-30T13:58:00Z',
    last_occurred_at: '2026-09-30T14:01:30Z',
    subject: 'u_4f21', tool: 'tls_b6681b043244c43f', device: '9f1c0b6e-0000-4000-8000-000000000001',
    mode: 'm2', action: 'blocked', content_state: 'uploaded', route: 'ext.page_context',
    detection_basis: 'prompt', merge_confidence: 'high', confidence: 'high',
    observation_count: 2, size_bytes: 2481, labels: [{ class: 'customer_pii', score: 0.91 }],
    observed_routes: ['ext.page_context', 'proxy.tls'],
  }),
  Object.freeze({
    submission_id: '11111111-2222-4333-8444-555555555552',
    received_at: '2026-09-30T13:41:02Z',
    first_occurred_at: '2026-09-23T08:00:00Z', // a spool flush: nine days between the two clocks
    last_occurred_at: '2026-09-23T08:04:00Z',
    subject: 'u_9a02', tool: 'tls_11574658dafb8805', device: '9f1c0b6e-0000-4000-8000-000000000002',
    mode: 'm1', action: 'logged', content_state: 'not_captured', route: 'proxy.tls',
    detection_basis: 'prompt', merge_confidence: 'low', confidence: 'degraded',
    observation_count: 1, size_bytes: 900, labels: [{ class: 'source_code', score: 0.62 }],
    observed_routes: ['proxy.tls'],
  }),
  Object.freeze({
    submission_id: '11111111-2222-4333-8444-555555555553',
    received_at: '2026-09-30T11:20:00Z',
    first_occurred_at: '2026-09-30T11:19:50Z',
    last_occurred_at: '2026-09-30T11:19:50Z',
    subject: 'u_4f21', tool: 'tls_b6681b043244c43f', device: '9f1c0b6e-0000-4000-8000-000000000001',
    mode: 'm0', action: null, content_state: 'not_captured', route: 'proc.detect',
    detection_basis: 'discovery', merge_confidence: 'high', confidence: null,
    observation_count: 1, size_bytes: null, labels: null, observed_routes: ['proc.detect'],
  }),
]);

/**
 * A record read (Q9) as query-api answers it: one row per observation, each repeating the
 * submission's columns under the store's names.
 */
const RECORD_HEAD = Object.freeze({
  submission_id: '11111111-2222-4333-8444-555555555551',
  received_at: '2026-09-30T14:02:11Z',
  first_occurred_at: '2026-09-30T13:58:00Z',
  last_occurred_at: '2026-09-30T14:01:30Z',
  user_ref: 'u_4f21', tool_fingerprint: 'tls_b6681b043244c43f', tool_name: 'Claude Code',
  kind: 'prompt', collection_mode: 'm2', policy_action: 'blocked',
  policy_rule_id: 'PAYMENT_CARD_PAN', decided_locally: true, confidence: 'high', merge_confidence: 'high',
  content_state: 'uploaded', shredded_reason: null, size_bytes: 2481, content_digest: 'sha256:aa', labels: [{ class: 'customer_pii', score: 0.91 }],
  classifier_version: 'c-2026.09', winning_source: 'ext.page_context', winning_fidelity: 'full', observed_routes: ['ext.page_context', 'proxy.tls'],
  observation_count: 2, expires_at: '2027-09-30T14:02:11Z',
});

const RECORD_ROWS = Object.freeze([
  Object.freeze({
    ...RECORD_HEAD,
    observation_event_id: '22222222-3333-4444-8555-666666666661', observation_source: 'ext.page_context', observation_kind: 'prompt', direction: 'egress',
    observation_occurred_at: '2026-09-30T13:58:00Z', observation_received_at: '2026-09-30T14:02:11Z', observation_size_bytes: 2481,
    observation_labels: [{ class: 'customer_pii', score: 0.91 }], policy_decision: { rule_id: 'PAYMENT_CARD_PAN', action: 'blocked' },
    detection_basis: 'prompt', window_start: null, window_end: null, submission_count: null, bytes_total: null,
  }),
  Object.freeze({
    ...RECORD_HEAD,
    observation_event_id: '22222222-3333-4444-8555-666666666662', observation_source: 'proxy.tls', observation_kind: 'prompt', direction: 'egress',
    observation_occurred_at: '2026-09-30T13:58:01Z', observation_received_at: '2026-09-30T14:02:12Z', observation_size_bytes: 2490,
    observation_labels: null, policy_decision: null,
    detection_basis: 'prompt', window_start: null, window_end: null, submission_count: null, bytes_total: null,
  }),
]);

const FINDING_ROWS = Object.freeze([
  Object.freeze({ submission_id: '11111111-2222-4333-8444-555555555551', detected_at: '2026-09-30T14:02:12Z', rule: 'PAYMENT_CARD_PAN', rule_title: 'Payment card number', class: 'payment_card', severity: 'critical', subject: 'u_4f21', tool: 'tls_b6681b043244c43f', mode: 'm2', review_state: 'open', decided_locally: true }),
  Object.freeze({ submission_id: '11111111-2222-4333-8444-555555555552', detected_at: '2026-09-30T13:41:05Z', rule: 'SOURCE_DECLARATION', rule_title: 'Source code', class: 'source_code', severity: 'high', subject: 'u_9a02', tool: 'tls_11574658dafb8805', mode: 'm1', review_state: 'disputed', decided_locally: false }),
]);

const AUDIT_ROWS = Object.freeze([
  Object.freeze({ audit_seq: 8123, occurred_at: '2026-10-01T11:05:01Z', actor_type: 'user', actor: 'analyst.one@customer.example', action: 'query.events', object_type: 'ingest.submission', object_id: null, subject: 'u_4f21', case: null, detail: { window: WINDOW }, prev_hash: 'aaaa', row_hash: 'bbbb' }),
  Object.freeze({ audit_seq: 8122, occurred_at: '2026-10-01T10:41:00Z', actor_type: 'user', actor: 'investigator.two@customer.example', action: 'query.record', object_type: 'ingest.submission', object_id: '11111111-2222-4333-8444-555555555551', subject: 'u_4f21', case: 'CASE-2026-118', detail: {}, prev_hash: 'cccc', row_hash: 'aaaa' }),
]);

const ORG_ROWS = Object.freeze([
  Object.freeze({ bucket: DAY, department: 'Engineering', submissions: 2210, users: 640 }),
  Object.freeze({ bucket: DAY, department: 'Legal', result_state: 'suppressed', reason: 'fewer_than_k_subjects', k: 5 }),
  Object.freeze({ bucket: DAY, department: 'Finance', submissions: 812, users: 96 }),
]);

const CLASS_ROWS = Object.freeze([
  Object.freeze({ bucket: DAY, class: 'customer_pii', severity: 'high', submissions: 1204, users: 388, max_score: 0.94, degraded_events: 12 }),
  Object.freeze({ bucket: DAY, class: 'credential', severity: 'critical', submissions: 402, users: 122, max_score: 1, degraded_events: 12 }),
  Object.freeze({ bucket: DAY, class: 'health', result_state: 'suppressed', reason: 'fewer_than_k_subjects', k: 5 }),
]);

const PERSON_ROWS = Object.freeze([
  Object.freeze({ bucket: '2026-09-24T00:00:00Z', subject: 'u_4f21', submissions: 41, bytes_total: 900_000, tools_used: 3, block_events: 1 }),
  Object.freeze({ bucket: '2026-09-25T00:00:00Z', subject: 'u_4f21', submissions: 44, bytes_total: 940_000, tools_used: 3, block_events: 0 }),
  Object.freeze({ bucket: '2026-09-26T00:00:00Z', subject: 'u_4f21', submissions: 39, bytes_total: 800_000, tools_used: 2, block_events: 0 }),
  Object.freeze({ bucket: '2026-09-27T00:00:00Z', subject: 'u_4f21', submissions: 42, bytes_total: 910_000, tools_used: 3, block_events: 2 }),
  Object.freeze({ bucket: '2026-09-28T00:00:00Z', subject: 'u_4f21', submissions: 44, bytes_total: 950_000, tools_used: 3, block_events: 0 }),
  Object.freeze({ bucket: '2026-09-29T00:00:00Z', subject: 'u_4f21', submissions: 41, bytes_total: 880_000, tools_used: 2, block_events: 1 }),
  Object.freeze({ bucket: '2026-09-30T00:00:00Z', subject: 'u_4f21', submissions: 260, bytes_total: 5_100_000, tools_used: 4, block_events: 9 }),
]);

function envelope(resultState, body) {
  return Object.freeze({
    api_version: '1',
    query_version: '1',
    result_state: resultState,
    ...body,
  });
}

function ok(body) {
  return envelope('ok', body);
}

/** Envelopes that do not depend on the request. */
export const STATE_ENVELOPES = Object.freeze({
  stale: ok({
    data: TOOL_ROWS,
    freshness: freshness({ state: 'stale', lag_seconds: 5400 }),
    coverage: coverage(),
    suppression: { k: 5, suppressed_cells: 1, subject_count_basis: 'exact' },
    audit: { entry_id: '8123', written_at: '2026-10-01T11:05:01Z' },
    meta: { source: 'mart.v_tool_usage', applied_bucket: 'day', measure_semantics: { submissions: 'additive', users: 'exact' } },
  }),
  empty: ok({
    data: [],
    freshness: freshness(),
    coverage: coverage(),
    meta: { source: 'mart.v_tool_usage', applied_bucket: 'day' },
  }),
  blind: ok({
    data: [],
    freshness: freshness({ state: 'not_yet_covered', reason: 'no_watermark_row', last_run_at: null }),
    coverage: coverage({ state: 'not_yet_covered', reason: 'no_coverage_snapshot', devices_reporting: null, devices_enrolled: null, expected_collector_days: null, observed_collector_days: null, gap_reasons: {}, gaps_total: 0 }),
    meta: { source: 'mart.v_tool_usage' },
  }),
  directory_not_synced: ok({
    data: [],
    freshness: freshness({ state: 'not_yet_covered', aggregate: 'mart.agg_org_period', reason: 'directory_not_synced', last_run_at: null }),
    coverage: coverage(),
    meta: { source: 'mart.agg_org_period', applied_bucket: 'day' },
  }),
  refused_too_broad: envelope('query_too_broad', {
    error: {
      code: 'cost_estimate_exceeded',
      message: 'This shape may return about 33600 cells, above the 2000-cell cap for a aggregate read.',
      fixable: true,
      detail: { max_cells: 2000, estimated_cells: 33600, fix: { coarser_bucket: 'week' } },
    },
  }),
  refused_shape: envelope('unsupported_query_shape', {
    error: {
      code: 'no_covering_index',
      message: 'ingest.submission has no index for a prefix predicate (tool).',
      fixable: true,
      detail: { fix: { alternative_source: 'mart.agg_tool_period', or_use: 'tool eq / in' } },
    },
  }),
  cursor_expired: envelope('cursor_expired', {
    error: { code: 'cursor_expired', message: 'Cursor has expired; restart from page one.', detail: { expired_at: '2026-10-01T10:00:00Z' } },
  }),
  audit_unavailable: envelope('audit_unavailable', {
    error: { code: 'audit_write_failed', message: 'The audit entry could not be committed, so no rows were served.' },
  }),
  busy: envelope('busy', {
    error: { code: 'shed', message: 'Shed by concurrency for this tenant.', detail: { retry_after_seconds: 2 } },
  }),
  chain_broken: envelope('audit_chain_broken', {
    error: {
      code: 'chain_mismatch',
      message: "The audit page's hash links do not verify; this is an integrity alert, not a list.",
      detail: { broken: [{ audit_seq: 8122, reason: 'link_mismatch' }] },
    },
  }),
  no_longer_available: envelope('no_longer_available', {
    error: {
      code: 'erasure',
      message: 'The record existed and has been destroyed; the receipt is attached.',
      receipt: { receipt_id: 'r_88', scope_kind: 'subject', completed_at: '2026-09-30T09:00:00Z', mechanisms: ['db_delete', 'blob_lifecycle'], removed_counts: { submission: 214 }, remaining_counts: { audit: 3 } },
    },
    freshness: freshness({ aggregate: 'ingest.submission' }),
    coverage: coverage(),
  }),
  not_found: envelope('not_found', {
    error: { code: 'not_found', message: 'No such record.', detail: { purge_window_unknown: true } },
    freshness: freshness({ aggregate: 'ingest.submission' }),
    coverage: coverage(),
  }),
  all_suppressed: ok({
    data: [
      { bucket: DAY, tool: 'a', result_state: 'suppressed', reason: 'fewer_than_k_subjects', k: 5 },
      { bucket: DAY, tool: 'b', result_state: 'suppressed', reason: 'fewer_than_k_subjects', k: 5 },
    ],
    freshness: freshness(),
    coverage: coverage(),
    suppression: { k: 5, suppressed_cells: 2, subject_count_basis: 'lower_bound' },
    meta: { source: 'mart.v_tool_usage', applied_bucket: 'day', subject_count_basis: 'lower_bound' },
  }),
});

/** The per-question answers used by the default "realistic" scenario. */
const REALISTIC = Object.freeze({
  q1_tools_ranked: ok({
    data: TOOL_ROWS,
    freshness: freshness(),
    coverage: coverage({ state: 'partial' }),
    suppression: { k: 5, suppressed_cells: 1, subject_count_basis: 'exact' },
    audit: { entry_id: '8123', written_at: '2026-10-01T11:05:01Z' },
    meta: {
      source: 'mart.v_tool_usage',
      applied_bucket: 'day',
      measure_semantics: { submissions: 'additive', users: 'exact' },
      warnings: ['mart.v_tool_usage exposes no detections/rollup_events/degraded_events: a detection-only or rollup-only tool cannot appear in this source. Use source mart.agg_tool_period for those measures.'],
    },
  }),
  q2_unsanctioned_users: ok({
    data: [
      { bucket: DAY, tool: 'tls_b6681b043244c43f', subject: 'u_9a02', submissions: 214, bytes_total: 12_000_000 },
      { bucket: DAY, tool: 'tls_b6681b043244c43f', subject: 'u_4f21', submissions: 188, bytes_total: 9_400_000 },
      { bucket: DAY, tool: 'shadow_llm_gateway', result_state: 'suppressed', reason: 'fewer_than_k_subjects', k: 5 },
    ],
    freshness: freshness(),
    coverage: coverage({ state: 'partial' }),
    suppression: { k: 5, suppressed_cells: 1, subject_count_basis: 'exact' },
    audit: { entry_id: '8124', written_at: '2026-10-01T11:05:02Z' },
    meta: { source: 'mart.agg_tool_user_period', applied_bucket: 'day', k: 5 },
  }),
  q3_team_growth: ok({
    data: ORG_ROWS,
    freshness: freshness({ aggregate: 'mart.agg_org_period' }),
    coverage: coverage({ state: 'partial' }),
    suppression: { k: 5, suppressed_cells: 1, subject_count_basis: 'exact' },
    meta: {
      source: 'mart.agg_org_period',
      applied_bucket: 'day',
      extras: { org_coverage: [{ users_all: 4210, users_mapped: 2680, org_rows: 96 }] },
    },
  }),
  q4_class_mix: ok({
    data: CLASS_ROWS,
    freshness: freshness({ aggregate: 'mart.agg_class_period' }),
    coverage: coverage({ state: 'partial' }),
    suppression: { k: 5, suppressed_cells: 1, subject_count_basis: 'exact' },
    meta: {
      source: 'mart.agg_class_period',
      applied_bucket: 'day',
      measure_semantics: { submissions: 'additive', users: 'exact', max_score: 'additive' },
      extras: { class_total: [{ submissions_total: 4210 }] },
    },
  }),
  q5_findings: ok({
    data: FINDING_ROWS,
    page: page({ returned: 2, next_cursor: 'eyJvIjoiYzEifQ.c2ln', newer_events_exist: true }),
    freshness: freshness({ aggregate: 'ingest.submission' }),
    coverage: coverage({ state: 'partial' }),
    audit: { entry_id: '8125', written_at: '2026-10-01T11:05:03Z' },
    meta: { source: 'mart.v_finding' },
  }),
  q6_subject_series: ok({
    data: PERSON_ROWS,
    freshness: freshness({ aggregate: 'mart.agg_user_period' }),
    coverage: coverage({ state: 'partial' }),
    suppression: { k: null, suppressed_cells: 0 },
    audit: { entry_id: '8126', written_at: '2026-10-01T11:05:04Z' },
    meta: {
      source: 'mart.agg_user_period',
      applied_bucket: 'day',
      extras: { flush_check: [{ rows_in_window: 902, late_flush_rows: 118 }] },
    },
  }),
  q7_devices: ok({
    data: DEVICE_ROWS,
    page: page({ returned: 4, next_cursor: 'eyJkIjoiZGV2In0.c2ln', newer_events_exist: false }),
    freshness: freshness({ aggregate: 'ops.device', state: 'fresh', aggregate_note: 'current state' }),
    coverage: coverage({ state: 'partial' }),
    meta: { source: 'mart.v_device_liveness' },
  }),
  q8_activity: ok({
    data: ACTIVITY_ROWS,
    page: page({ returned: 3, next_cursor: 'eyJhIjoiZXZlbnQifQ.c2ln', newer_events_exist: true }),
    freshness: freshness({ aggregate: 'ingest.submission' }),
    coverage: coverage({ state: 'partial' }),
    audit: { entry_id: '8127', written_at: '2026-10-01T11:05:05Z' },
    meta: { source: 'ingest.submission' },
  }),
  q9_event_detail: ok({
    data: RECORD_ROWS,
    freshness: freshness({ aggregate: 'ingest.submission' }),
    coverage: coverage({ state: 'partial' }),
    audit: { entry_id: '8128', written_at: '2026-10-01T11:05:06Z' },
    meta: { source: 'ingest.submission', content_state: 'uploaded' },
  }),
  q10_audit_trail: ok({
    data: AUDIT_ROWS,
    page: page({ returned: 2, next_cursor: null, newer_events_exist: false }),
    freshness: freshness({ aggregate: 'ops.audit' }),
    coverage: coverage(),
    audit: { entry_id: '8129', written_at: '2026-10-01T11:05:07Z' },
    meta: { source: 'ops.audit' },
  }),
});

/** A second page for the list questions, so the pagination path is real in the demo. */
const SECOND_PAGE = Object.freeze({
  q5_findings: ok({
    data: [FINDING_ROWS[1]],
    page: page({ returned: 1, next_cursor: null, newer_events_exist: false }),
    freshness: freshness({ aggregate: 'ingest.submission' }),
    coverage: coverage({ state: 'partial' }),
    meta: { source: 'mart.v_finding' },
  }),
  q7_devices: ok({
    data: [DEVICE_ROWS[3]],
    page: page({ returned: 1, next_cursor: null, newer_events_exist: false }),
    freshness: freshness({ aggregate: 'ops.device' }),
    coverage: coverage({ state: 'partial' }),
    meta: { source: 'mart.v_device_liveness' },
  }),
  q8_activity: ok({
    data: [ACTIVITY_ROWS[2]],
    page: page({ returned: 1, next_cursor: null, newer_events_exist: false }),
    freshness: freshness({ aggregate: 'ingest.submission' }),
    coverage: coverage({ state: 'partial' }),
    meta: { source: 'ingest.submission' },
  }),
});

/** Scenario name -> the answer a question gets: the realistic mix, or one state forced on every read. */
export const SCENARIOS = Object.freeze({
  realistic: Object.freeze({ label: 'Realistic mix', answers: REALISTIC, pageTwo: SECOND_PAGE }),
  stale: Object.freeze({ label: 'Stale aggregate', answers: null, forced: STATE_ENVELOPES.stale }),
  degraded: Object.freeze({
    label: 'Degraded collection',
    answers: null,
    forced: ok({
      data: TOOL_ROWS,
      freshness: freshness({ state: 'stale', lag_seconds: 5400 }),
      coverage: coverage({ state: 'partial', devices_reporting: 3100, gap_reasons: { not_enrolled: 440, permission_denied: 980, unknown: 100 } }),
      suppression: { k: 5, suppressed_cells: 1, subject_count_basis: 'lower_bound' },
      meta: { source: 'mart.v_tool_usage', applied_bucket: 'day', subject_count_basis: 'lower_bound' },
    }),
  }),
  empty: Object.freeze({ label: 'Empty, coverage adequate', answers: null, forced: STATE_ENVELOPES.empty }),
  blind: Object.freeze({ label: 'Not yet covered', answers: null, forced: STATE_ENVELOPES.blind }),
  suppressed: Object.freeze({ label: 'Every cell suppressed', answers: null, forced: STATE_ENVELOPES.all_suppressed }),
  refused: Object.freeze({ label: 'Refused: too broad', answers: null, forced: STATE_ENVELOPES.refused_too_broad }),
  refused_shape: Object.freeze({ label: 'Refused: shape', answers: null, forced: STATE_ENVELOPES.refused_shape }),
  cursor_expired: Object.freeze({ label: 'Cursor expired', answers: null, forced: STATE_ENVELOPES.cursor_expired }),
  audit_unavailable: Object.freeze({ label: 'Audit unavailable', answers: null, forced: STATE_ENVELOPES.audit_unavailable }),
  busy: Object.freeze({ label: 'Busy (429)', answers: null, forced: STATE_ENVELOPES.busy }),
  chain_broken: Object.freeze({ label: 'Audit chain broken', answers: null, forced: STATE_ENVELOPES.chain_broken }),
});

export const SCENARIO_NAMES = Object.freeze(Object.keys(SCENARIOS));

function keyOf(body) {
  return body?.template ?? body?.source ?? 'unknown';
}

/** A template request carries its cursor inside `params`; a document carries it at the top level. */
function cursorOf(body) {
  const value = body?.params?.cursor ?? body?.cursor;
  return typeof value === 'string' && value.length > 0 ? value : null;
}

/**
 * A query transport for a scenario. It answers by template name, and answers a request that
 * carries a cursor it issued with the second page; a cursor it never issued gets cursor_expired.
 *
 * @param {string} scenario
 */
export function fixtureTransport(scenario = 'realistic') {
  const spec = SCENARIOS[scenario] ?? SCENARIOS.realistic;
  const issued = new Set();
  return Object.freeze({
    async send(body) {
      const name = keyOf(body);
      const cursor = cursorOf(body);
      if (cursor !== null) {
        if (!issued.has(cursor)) return STATE_ENVELOPES.cursor_expired;
        issued.delete(cursor);
        return spec.pageTwo?.[name] ?? spec.forced ?? STATE_ENVELOPES.cursor_expired;
      }
      if (spec.forced) return spec.forced;
      const reply = spec.answers?.[name] ?? STATE_ENVELOPES.refused_shape;
      if (reply?.page?.next_cursor) issued.add(reply.page.next_cursor);
      return reply;
    },
  });
}

export { TOOL_ROWS, DEVICE_ROWS, ACTIVITY_ROWS, RECORD_ROWS, FINDING_ROWS, AUDIT_ROWS, ORG_ROWS, CLASS_ROWS, PERSON_ROWS, WINDOW };
