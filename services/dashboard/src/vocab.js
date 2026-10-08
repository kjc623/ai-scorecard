// vocab.js — the closed vocabularies of the query DSL, mirrored on the client.
//
// This file mirrors query-api's registry (services/query-api/src/registry.js). It exists so the
// dashboard can refuse to build a query the server would reject, before the request leaves the
// browser: the DSL is closed, so an unknown dimension is a 400 rather than a silent no-op, and a
// guessed field name is a hard failure. test/parity.test.mjs imports the server's registry and
// asserts that these lists are equal name for name.
//
// Nothing here is a query. It is a list of nouns the API admits.

/** The DSL version this client speaks. A different version is not a downgrade, it is a rejection. */
export const QUERY_VERSION = '1';

/**
 * The endpoints this client may call. test/guarantees.test.mjs asserts there are no others.
 *
 * The query endpoint takes a closed query document and never returns content. The two content
 * reads are forwarded by query-api to content-vault, which decides, audits before it serves, and is
 * the only component that can open content. Neither takes a query document.
 */
export const QUERY_ENDPOINT = '/v1/query';
export const CONTENT_SEARCH_ENDPOINT = '/v1/content-search';
export const CONTENT_RETRIEVAL_ENDPOINT = '/v1/content/retrieval';
/** The list export: the current filtered events or findings list, as a bounded CSV via a link. */
export const LIST_EXPORT_ENDPOINT = '/v1/list-export';

/**
 * The admin API behind Settings → Deployment. It is control-api's, not the read path's: the dashboard server forwards /admin/v1/* there with the session's product token, and
 * control-api checks the admin role and audits every write with the real actor. These are the
 * only writes this client makes. A key or token id is appended as /<id>/revoke.
 */
export const ADMIN_DEPLOYMENT_ENDPOINT = '/admin/v1/deployment';
export const ADMIN_PACKAGE_ENDPOINT = '/admin/v1/deployment/package';
export const ADMIN_VERIFICATION_ENDPOINT = '/admin/v1/deployment/verification';
export const ADMIN_KEYS_ENDPOINT = '/admin/v1/deployment/keys';
export const ADMIN_SCIM_TOKENS_ENDPOINT = '/admin/v1/scim/tokens';

/**
 * The admin API behind Settings → Settings. control-api's, admin-only and audited, like Deployment:
 * collection mode and its narrower per-tool overrides, event and content retention, tool sanction
 * decisions, the content search tier, the endpoint collector switches, TLS inspection, the
 * enforcement rules and the interception routes' kill switches. These are the only writes this client makes, besides Deployment's. A tool's endpoint switches are at
 * /tools/<tool_key> under the endpoint settings; a route's kill switch is at /<route> under the kill
 * switch endpoint.
 */
export const ADMIN_SETTINGS_ENDPOINT = '/admin/v1/settings';
export const ADMIN_SETTINGS_COLLECTION_MODE_ENDPOINT = '/admin/v1/settings/collection-mode';
export const ADMIN_SETTINGS_SCOPE_OVERRIDE_ENDPOINT = '/admin/v1/settings/scope-override';
export const ADMIN_SETTINGS_RETENTION_ENDPOINT = '/admin/v1/settings/retention';
export const ADMIN_SETTINGS_CONTENT_SEARCH_ENDPOINT = '/admin/v1/settings/content-search';
export const ADMIN_SETTINGS_TOOL_SANCTION_ENDPOINT = '/admin/v1/settings/tools';
export const ADMIN_SETTINGS_ENDPOINT_COLLECTORS_ENDPOINT = '/admin/v1/settings/endpoint';
export const ADMIN_SETTINGS_TLS_INSPECTION_ENDPOINT = '/admin/v1/settings/tls-inspection';
export const ADMIN_SETTINGS_RULES_ENDPOINT = '/admin/v1/settings/rules';
export const ADMIN_SETTINGS_KILL_SWITCH_ENDPOINT = '/admin/v1/settings/kill-switch';

/** k, the small-cell floor. A cell below it arrives suppressed and is never a number. */
export const K = 5;

/**
 * Sources, with the dimensions and measures each actually carries. A dimension or measure that
 * is not listed here is not offered by the API for that source.
 */
export const SOURCES = Object.freeze({
  'mart.v_tool_usage': Object.freeze({
    kind: 'aggregate',
    label: 'Tool usage (view)',
    dimensions: Object.freeze(['bucket', 'tool', 'sanctioned_state']),
    measures: Object.freeze(['submissions', 'users', 'bytes_total', 'blocked', 'warned', 'logged']),
    answers: 'Q1',
    note: 'sanctioned state is joined at read time; a tool with no decision arrives as unknown, never as unsanctioned',
  }),
  'mart.agg_tool_period': Object.freeze({
    kind: 'aggregate',
    label: 'Tool usage (full measure set)',
    dimensions: Object.freeze(['bucket', 'tool', 'sanctioned_state']),
    measures: Object.freeze([
      'submissions', 'users', 'bytes_total', 'blocked', 'warned', 'logged',
      'detections', 'rollup_events', 'degraded_events',
    ]),
    answers: 'Q1',
    note: 'detection-only and rollup-only tools appear here and not in the view',
  }),
  'mart.agg_tool_user_period': Object.freeze({
    kind: 'aggregate',
    label: 'Usage per tool per person',
    dimensions: Object.freeze(['bucket', 'tool', 'sanctioned_state', 'subject']),
    measures: Object.freeze(['submissions', 'bytes_total']),
    answers: 'Q2',
    subjectBearing: true,
    note: 'subject-bearing: every read is audited; sanctioned state is joined at read time and the k cell is the tool, not the person',
  }),
  'mart.agg_org_period': Object.freeze({
    kind: 'aggregate',
    label: 'Usage per team',
    dimensions: Object.freeze(['bucket', 'department', 'population', 'tool']),
    measures: Object.freeze(['submissions', 'users']),
    answers: 'Q3',
    note: 'empty until the directory sync; the answer is then not_yet_covered, never a zero line',
  }),
  'mart.agg_class_period': Object.freeze({
    kind: 'aggregate',
    label: 'Sensitive-data class mix',
    dimensions: Object.freeze(['bucket', 'class', 'tool', 'severity', 'classifier_version']),
    measures: Object.freeze(['submissions', 'users', 'max_score', 'degraded_events']),
    answers: 'Q4',
    note: 'submissions counts submissions carrying the class: the rows fan out and are not additive',
  }),
  'mart.agg_user_period': Object.freeze({
    kind: 'aggregate',
    label: 'One person over time',
    dimensions: Object.freeze(['bucket', 'subject']),
    measures: Object.freeze(['submissions', 'bytes_total', 'tools_used', 'block_events']),
    answers: 'Q6',
    subjectBearing: true,
    requiresSubjectFilter: true,
    note: 'requires a subject filter: a read without one would enumerate people',
  }),
  'mart.agg_device_period': Object.freeze({
    kind: 'aggregate',
    label: 'Device health history',
    dimensions: Object.freeze(['bucket', 'device', 'collector']),
    measures: Object.freeze(['healthy_days', 'degraded_days', 'absent_days', 'tampered_days', 'spool_dropped']),
    answers: 'Q7',
  }),
  'mart.v_device_liveness': Object.freeze({
    kind: 'list',
    label: 'Devices and their collection state',
    dimensions: Object.freeze(['device', 'hostname', 'agent_version', 'device_os', 'managed_state', 'collection_mode', 'region', 'liveness', 'collector', 'collector_state']),
    measures: Object.freeze([]),
    answers: 'Q7',
    noWindow: true,
    note: 'current state, not a stream: it needs no window and is bounded by its page',
  }),
  'ops.coverage_snapshot': Object.freeze({
    kind: 'list',
    label: 'Coverage gaps',
    dimensions: Object.freeze(['snapshot_day', 'device', 'collector', 'gap_reason', 'observed', 'expected']),
    measures: Object.freeze([]),
    answers: 'Posture',
  }),
  'ingest.submission': Object.freeze({
    kind: 'list',
    label: 'Events',
    dimensions: Object.freeze([
      'subject', 'tool', 'device', 'mode', 'prompt_kind', 'action', 'content_state', 'route',
      'detection_basis', 'merge_confidence', 'confidence', 'department', 'population', 'manager',
    ]),
    measures: Object.freeze([]),
    answers: 'Q8, Q9',
    subjectBearing: true,
  }),
  'mart.v_finding': Object.freeze({
    kind: 'list',
    label: 'Findings',
    dimensions: Object.freeze(['severity', 'rule', 'class', 'subject', 'tool', 'review_state', 'mode', 'decided_locally']),
    measures: Object.freeze([]),
    answers: 'Q5',
    subjectBearing: true,
  }),
  'ops.audit': Object.freeze({
    kind: 'list',
    label: 'Audit trail',
    dimensions: Object.freeze(['actor_type', 'actor', 'action', 'object_type', 'subject', 'case']),
    measures: Object.freeze([]),
    answers: 'Q10',
    subjectBearing: true,
  }),
});

/** The ten questions, with the parameters each template admits. */
export const TEMPLATES = Object.freeze({
  q1_tools_ranked: Object.freeze({
    question: 1,
    title: 'Which AI tools are in use, ranked, over time?',
    source: 'mart.v_tool_usage',
    screen: 'tools',
    params: Object.freeze(['window', 'bucket', 'limit', 'tool', 'sanctioned_state']),
  }),
  q2_unsanctioned_users: Object.freeze({
    question: 2,
    title: 'Which are unsanctioned, and who is using them?',
    source: 'mart.agg_tool_user_period',
    screen: 'unsanctioned',
    params: Object.freeze(['window', 'bucket', 'limit', 'tool', 'subject', 'sanctioned_state']),
  }),
  q3_team_growth: Object.freeze({
    question: 3,
    title: 'How much is usage growing, per team?',
    source: 'mart.agg_org_period',
    screen: 'teams',
    params: Object.freeze(['window', 'bucket', 'limit', 'department', 'population']),
  }),
  q4_class_mix: Object.freeze({
    question: 4,
    title: 'What classes of sensitive data are going into AI?',
    source: 'mart.agg_class_period',
    screen: 'classes',
    params: Object.freeze(['window', 'bucket', 'limit', 'dimensions', 'class', 'severity']),
  }),
  q5_findings: Object.freeze({
    question: 5,
    title: 'Which specific submissions hit a policy rule?',
    source: 'mart.v_finding',
    screen: 'findings',
    params: Object.freeze(['window', 'limit', 'cursor', 'severity', 'rule', 'review_state', 'subject', 'tool', 'class']),
  }),
  q6_subject_series: Object.freeze({
    question: 6,
    title: "Has a given person's usage changed, or spiked?",
    source: 'mart.agg_user_period',
    screen: 'person',
    params: Object.freeze(['subject', 'window', 'bucket', 'limit']),
    requires: Object.freeze(['subject']),
  }),
  q7_devices: Object.freeze({
    question: 7,
    title: "What is the state of every device's collection?",
    source: 'mart.v_device_liveness',
    screen: 'devices',
    params: Object.freeze(['limit', 'cursor', 'liveness', 'collector_state', 'collector', 'device_os', 'managed_state', 'region']),
  }),
  q8_activity: Object.freeze({
    question: 8,
    title: 'What happened in this window, for this tool or person?',
    source: 'ingest.submission',
    screen: 'activity',
    params: Object.freeze(['window', 'limit', 'cursor', 'subject', 'tool', 'device', 'class', 'content_state', 'action', 'mode', 'department', 'prompt_kind', 'prompt_kind_not']),
  }),
  q9_event_detail: Object.freeze({
    question: 9,
    title: 'What exactly was sent?',
    source: 'ingest.submission',
    screen: 'event',
    params: Object.freeze(['submission_id', 'received_at_hint']),
    requires: Object.freeze(['submission_id']),
  }),
  q10_audit_trail: Object.freeze({
    question: 10,
    title: 'What has been accessed, and by whom?',
    source: 'ops.audit',
    screen: 'audit',
    params: Object.freeze(['window', 'limit', 'cursor', 'actor', 'action', 'object_type', 'subject', 'case']),
  }),
});

export const TEMPLATE_NAMES = Object.freeze(Object.keys(TEMPLATES));

/** The result states, with the treatment each one demands of the UI. */
export const RESULT_STATES = Object.freeze({
  ok: Object.freeze({ http: 200, carriesData: true, treatment: 'data', label: 'A real answer' }),
  empty: Object.freeze({ http: 200, carriesData: true, treatment: 'empty', label: 'We looked, coverage was adequate, there is nothing' }),
  not_yet_covered: Object.freeze({ http: 200, carriesData: true, treatment: 'hatched', label: 'Window predates collection, or the dimension has no source' }),
  stale_aggregate: Object.freeze({ http: 200, carriesData: true, treatment: 'warning', label: 'Watermark older than three cadences' }),
  coverage_degraded: Object.freeze({ http: 200, carriesData: true, treatment: 'warning', label: 'The value is a floor, not a total' }),
  suppressed: Object.freeze({ http: 200, carriesData: true, treatment: 'hatched', label: 'Fewer than k subjects in the cell' }),
  no_longer_available: Object.freeze({ http: 410, carriesData: false, treatment: 'gone', label: 'The record or its content existed and was destroyed' }),
  not_captured: Object.freeze({ http: 200, carriesData: true, treatment: 'plain', label: 'The mode never permitted reading content' }),
  not_retrievable: Object.freeze({ http: 200, carriesData: true, treatment: 'plain', label: 'Content remains on the device' }),
  not_found: Object.freeze({ http: 404, carriesData: false, treatment: 'plain', label: 'No such record, and no purge covers its window' }),
  unsupported_query_shape: Object.freeze({ http: 400, carriesData: false, treatment: 'refusal', label: 'Filter combination not servable' }),
  query_too_broad: Object.freeze({ http: 400, carriesData: false, treatment: 'refusal', label: 'Over budget or over a cap' }),
  cursor_expired: Object.freeze({ http: 400, carriesData: false, treatment: 'restart', label: 'Cursor expired, mismatched, or from another query' }),
  audit_unavailable: Object.freeze({ http: 503, carriesData: false, treatment: 'refusal', label: 'The audit row could not be committed: no data at all' }),
  key_unavailable: Object.freeze({ http: 503, carriesData: false, treatment: 'refusal', label: 'Tenant key disabled or unreachable' }),
  busy: Object.freeze({ http: 429, carriesData: false, treatment: 'retry', label: 'Shed by concurrency' }),
  unauthorised_role: Object.freeze({ http: 403, carriesData: false, treatment: 'refusal', label: 'The role cannot make this read' }),
  content_search_not_enabled: Object.freeze({ http: 403, carriesData: false, treatment: 'refusal', label: "The tenant's tier does not include this search" }),
  audit_chain_broken: Object.freeze({ http: 500, carriesData: false, treatment: 'refusal', label: "A page's hash links do not verify" }),
});

/** Values that must never be merged in a rendering. */
export const STATE_PAIRS = Object.freeze({
  liveness: Object.freeze(['reporting', 'stale', 'never_reported', 'revoked']),
  collector_state: Object.freeze(['healthy', 'degraded', 'absent', 'tampered']),
  content_state: Object.freeze(['not_captured', 'local_only', 'uploaded', 'shredded']),
  policy_action: Object.freeze(['blocked', 'warned', 'logged']),
  review_state: Object.freeze(['open', 'disputed', 'confirmed']),
  sanctioned_state: Object.freeze(['sanctioned', 'unsanctioned', 'unknown']),
  merge_confidence: Object.freeze(['high', 'low']),
});

/** Windows the UI offers, in hours. Named so the label and the request cannot disagree. */
export const WINDOWS = Object.freeze({
  // Offered on the events dataset, so a list or a prompt search can be narrowed below a day.
  h6: Object.freeze({ label: 'Last 6 hours', hours: 6, bucket: 'hour' }),
  h24: Object.freeze({ label: 'Last 24 hours', hours: 24, bucket: 'hour' }),
  d7: Object.freeze({ label: 'Last 7 days', hours: 24 * 7, bucket: 'day' }),
  d30: Object.freeze({ label: 'Last 30 days', hours: 24 * 30, bucket: 'day' }),
  d90: Object.freeze({ label: 'Last 90 days', hours: 24 * 90, bucket: 'day' }),
});
