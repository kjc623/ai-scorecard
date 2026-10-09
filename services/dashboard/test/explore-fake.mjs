// explore-fake.mjs — a fake query-api and content path for the Explore tests.
//
// It holds a few hundred generated rows and answers the four list templates and the record
// template the way query-api does: it applies the window and every filter, pages with a cursor
// bound to the query it was issued for, ends an iteration only with `next_cursor: null`, and
// answers a record read with one row per observation. The content reads behave like the vault's:
// only an uploaded event has anything to find, and a retrieval URL is single-use.
//
// It is seeded from a constant, so the same request at the same `now` gives the same rows.

import { freshness, coverage, STATE_ENVELOPES } from './fixtures.mjs';

const SAMPLE_EVENT_COUNT = 137;
const SAMPLE_SPAN_MS = 30 * 86_400_000;
const SAMPLE_CURSOR_TTL_MS = 15 * 60_000;

/** States a test can force on every read. */
export const EXPLORE_SCENARIOS = Object.freeze({
  realistic: 'Realistic mix',
  empty: 'Nothing found',
  too_broad: 'Refused: too broad',
  busy: 'Busy (429)',
  audit_unavailable: 'Audit unavailable',
  cursor_expired: 'Cursor expires on page two',
  destroyed: 'Record destroyed',
});

export const EXPLORE_SCENARIO_NAMES = Object.freeze(Object.keys(EXPLORE_SCENARIOS));

const SAMPLE_TOOLS = Object.freeze([
  'tls_11574658dafb8805', 'tls_11574658dafb8805', 'tls_11574658dafb8805', 'tls_b6681b043244c43f', 'tls_b6681b043244c43f', 'tls_f412811be7ac6539', 'tls_f412811be7ac6539',
  'tls_7659f7a5e64cd885', 'tls_a40a04ef6e27de9f', 'tls_9230903190dcf0dc', 'shadow_llm_gateway',
]);
const SAMPLE_DEPARTMENTS = Object.freeze(['Engineering', 'Finance', 'Legal', 'Customer Success', 'Sales', 'People']);
const SAMPLE_CLASSES = Object.freeze(['customer_pii', 'source_code', 'credential', 'payment_card', 'legal_commercial', 'government_id', 'health']);
const SAMPLE_RULES = Object.freeze({
  payment_card: Object.freeze({ rule: 'PAYMENT_CARD_PAN', rule_title: 'Payment card number', severity: 'critical' }),
  credential: Object.freeze({ rule: 'AWS_ACCESS_KEY_ID', rule_title: 'AWS access key ID', severity: 'critical' }),
  government_id: Object.freeze({ rule: 'US_SSN_STRUCTURE', rule_title: 'US Social Security number', severity: 'critical' }),
  customer_pii: Object.freeze({ rule: 'CUSTOMER_PII_LABEL', rule_title: 'Personal data field', severity: 'high' }),
  source_code: Object.freeze({ rule: 'SOURCE_DECLARATION', rule_title: 'Source code', severity: 'high' }),
  health: Object.freeze({ rule: 'HEALTH_CLINICAL_VOCABULARY', rule_title: 'Clinical information', severity: 'critical' }),
  legal_commercial: Object.freeze({ rule: 'LEGAL_CLAUSE_VOCABULARY', rule_title: 'Contract language', severity: 'medium' }),
});
const SAMPLE_ANALYSTS = Object.freeze(['r.okafor@customer.example', 'm.lindqvist@customer.example', 'd.haddad@customer.example']);

/** mulberry32: small, seedable, and good enough to spread sample rows. */
function sampleRng(seed) {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6D2B79F5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

function sampleHex(value, width) {
  return (value >>> 0).toString(16).padStart(width, '0').slice(-width);
}

/** A canonical version-4-shaped uuid from two small integers. */
function sampleUuid(space, index) {
  const a = Math.imul(index + 1, 2654435761) >>> 0;
  const b = Math.imul(index + 7, 40503) >>> 0;
  return `${sampleHex(a, 8)}-${sampleHex(space, 4)}-4${sampleHex(b, 3)}-9${sampleHex(a >>> 12, 3)}-${sampleHex(b, 4)}${sampleHex(a ^ b, 8)}`;
}

function sampleIso(ms) {
  return new Date(ms).toISOString().replace(/\.\d{3}Z$/, 'Z');
}

/**
 * Build the sample corpus, anchored at `now` so the window presets always have something in them.
 *
 * @param {Date} now
 */
export function buildExploreSample(now) {
  const rand = sampleRng(0x5ac0ffee);
  const pick = (list) => list[Math.floor(rand() * list.length)];
  const end = now.getTime() - 4 * 60_000;

  const people = Array.from({ length: 23 }, (_, i) => Object.freeze({
    subject: `u_${sampleHex(Math.imul(i + 3, 48271), 4)}`,
    department: SAMPLE_DEPARTMENTS[i % SAMPLE_DEPARTMENTS.length],
    device: sampleUuid(0xd0, i % 14),
  }));

  const events = [];
  let cursorMs = end;
  for (let i = 0; i < SAMPLE_EVENT_COUNT; i += 1) {
    cursorMs -= Math.floor(rand() * 2 * (SAMPLE_SPAN_MS / SAMPLE_EVENT_COUNT)) + 1000;
    const person = pick(people);
    const tool = pick(SAMPLE_TOOLS);
    const roll = rand();
    const mode = roll < 0.15 ? 'm0' : roll < 0.55 ? 'm1' : roll < 0.85 ? 'm2' : 'm3';
    const detectionOnly = mode === 'm0';
    const classified = !detectionOnly && rand() < 0.55;
    const labels = detectionOnly ? null : classified
      ? [{ class: pick(SAMPLE_CLASSES), score: Math.round((0.55 + rand() * 0.44) * 100) / 100 }]
      : [];
    if (labels && labels.length === 1 && rand() < 0.2) {
      const second = pick(SAMPLE_CLASSES);
      if (second !== labels[0].class) labels.push({ class: second, score: Math.round((0.5 + rand() * 0.4) * 100) / 100 });
    }
    const strong = labels && labels.some((l) => l.score >= 0.8);
    const actionRoll = rand();
    const action = detectionOnly ? null : strong ? (actionRoll < 0.3 ? 'blocked' : actionRoll < 0.7 ? 'warned' : 'logged') : 'logged';
    const contentState = mode === 'm2' ? 'local_only' : mode === 'm3' ? (rand() < 0.15 ? 'shredded' : 'uploaded') : 'not_captured';
    const lateFlush = rand() < 0.06;
    const occurred = lateFlush ? cursorMs - Math.floor((1 + rand() * 8) * 86_400_000) : cursorMs - Math.floor(2000 + rand() * 240_000);
    const routes = detectionOnly ? ['proc.detect'] : rand() < 0.35 ? ['ext.page_context', 'proxy.tls'] : [rand() < 0.7 ? 'ext.page_context' : 'proxy.tls'];
    events.push(Object.freeze({
      submission_id: sampleUuid(0xe0, i),
      received_at: sampleIso(cursorMs),
      first_occurred_at: sampleIso(occurred),
      last_occurred_at: sampleIso(occurred + Math.floor(rand() * 180_000)),
      subject: person.subject,
      tool,
      device: person.device,
      department: person.department,
      mode,
      // The sample does not model a device that decided the kind, so every event reads as
      // 'unknown' (schema: NULL reads as 'unknown'). The list still honours the prompt_kind and
      // prompt_kind_not predicates; a request for client_generated simply finds no rows.
      prompt_kind: 'unknown',
      action,
      content_state: contentState,
      route: routes[0],
      detection_basis: detectionOnly ? 'discovery' : rand() < 0.08 ? 'usage_rollup' : 'prompt',
      merge_confidence: rand() < 0.08 ? 'low' : 'high',
      confidence: detectionOnly ? null : rand() < 0.07 ? 'degraded' : strong ? 'high' : 'medium',
      observation_count: routes.length,
      size_bytes: detectionOnly ? null : Math.floor(180 + rand() * 9200),
      labels: labels ? Object.freeze(labels.map((l) => Object.freeze(l))) : null,
      observed_routes: Object.freeze(routes),
    }));
  }

  const findings = [];
  for (const event of events) {
    if (!event.labels || event.action === null) continue;
    for (const label of event.labels) {
      if (label.score < 0.7) continue;
      const rule = SAMPLE_RULES[label.class];
      const reviewRoll = rand();
      findings.push(Object.freeze({
        submission_id: event.submission_id,
        detected_at: sampleIso(Date.parse(event.received_at) + 1000),
        rule: rule.rule,
        rule_title: rule.rule_title,
        class: label.class,
        severity: rule.severity,
        subject: event.subject,
        tool: event.tool,
        mode: event.mode,
        review_state: reviewRoll < 0.6 ? 'open' : reviewRoll < 0.8 ? 'disputed' : 'confirmed',
        decided_locally: event.mode !== 'm3',
      }));
    }
  }

  const devices = [];
  for (let i = 0; i < 14; i += 1) {
    const device = sampleUuid(0xd0, i);
    const os = i % 3 === 0 ? 'macos' : 'windows';
    const region = i % 4 === 0 ? 'us' : 'eu';
    if (i === 11) {
      devices.push(Object.freeze({ device, device_os: os, managed_state: 'unmanaged', region, liveness: 'never_reported', collector: null, collector_state: null, spool_depth: null, spool_dropped_total: null, last_seen_at: null }));
      continue;
    }
    const liveness = i === 5 ? 'stale' : i === 9 ? 'revoked' : 'reporting';
    const seen = liveness === 'reporting' ? end - Math.floor(rand() * 600_000) : end - Math.floor((2 + rand() * 16) * 86_400_000);
    for (const collector of ['capture_core', 'capture_extension']) {
      const state = liveness === 'revoked' ? 'absent' : liveness === 'stale' ? 'degraded' : i === 2 && collector === 'capture_extension' ? 'tampered' : 'healthy';
      devices.push(Object.freeze({
        device,
        device_os: os,
        managed_state: i === 7 ? 'unknown' : 'managed',
        region,
        liveness,
        collector,
        collector_state: state,
        spool_depth: state === 'degraded' ? Math.floor(200 + rand() * 900) : 0,
        spool_dropped_total: liveness === 'revoked' ? 4412 : 0,
        last_seen_at: sampleIso(seen),
      }));
    }
  }
  devices.sort((a, b) => (a.device === b.device ? String(a.collector).localeCompare(String(b.collector)) : a.device.localeCompare(b.device)));

  // Each device's collector rows as ops.collector_state holds them: the rows above, plus Claude
  // Code's configuration (tampered on every third device) and Cursor's, switched off by policy, on
  // a device that reports.
  const collectors = [];
  for (const row of devices) {
    if (row.collector === null) continue;
    collectors.push(Object.freeze({
      device: row.device, collector: row.collector, collector_state: row.collector_state,
      error_code: row.collector_state === 'tampered' ? 'bundle_signature_invalid' : null,
      last_report_at: row.last_seen_at, last_success_at: row.collector_state === 'healthy' ? row.last_seen_at : null,
    }));
    if (row.collector !== 'capture_extension' || row.liveness !== 'reporting') continue;
    const tampered = parseInt(row.device.slice(-2), 16) % 3 === 0;
    collectors.push(Object.freeze({
      device: row.device, collector: 'tool_config_claude_code', collector_state: tampered ? 'degraded' : 'healthy',
      error_code: tampered ? 'config_tampered' : null, last_report_at: row.last_seen_at, last_success_at: row.last_seen_at,
    }));
    collectors.push(Object.freeze({
      device: row.device, collector: 'tool_config_cursor', collector_state: 'absent',
      error_code: 'disabled_by_policy', last_report_at: row.last_seen_at, last_success_at: null,
    }));
  }

  const audit = [];
  let auditMs = end;
  for (let i = 0; i < 44; i += 1) {
    auditMs -= Math.floor(rand() * 2 * (SAMPLE_SPAN_MS / 44)) + 1000;
    const kind = rand();
    const event = pick(events);
    const record = kind < 0.3;
    const action = record ? 'query.record' : kind < 0.65 ? 'query.events' : kind < 0.85 ? 'query.findings' : 'audit.read';
    audit.push(Object.freeze({
      audit_seq: 8200 - i,
      occurred_at: sampleIso(auditMs),
      actor_type: 'user',
      actor: pick(SAMPLE_ANALYSTS),
      action,
      object_type: action === 'audit.read' ? 'ops.audit' : action === 'query.findings' ? 'mart.v_finding' : 'ingest.submission',
      object_id: record ? event.submission_id : null,
      subject: action === 'audit.read' ? null : event.subject,
      case: record && rand() < 0.6 ? `CASE-2026-${100 + Math.floor(rand() * 60)}` : null,
      detail: Object.freeze(record ? {} : { filters: action === 'audit.read' ? [] : ['subject'] }),
      prev_hash: sampleHex(Math.imul(8199 - i, 2246822519), 8),
      row_hash: sampleHex(Math.imul(8200 - i, 2246822519), 8),
    }));
  }

  return Object.freeze({
    events: Object.freeze(events),
    findings: Object.freeze(findings),
    devices: Object.freeze(devices),
    collectors: Object.freeze(collectors),
    audit: Object.freeze(audit),
  });
}

/** Template name to the rows it reads, the clock its window bounds, and whether the read is audited. */
const SAMPLE_LISTS = Object.freeze({
  q8_activity: Object.freeze({ rows: 'events', clock: 'received_at', source: 'ingest.submission', audited: true }),
  q5_findings: Object.freeze({ rows: 'findings', clock: 'detected_at', source: 'mart.v_finding', audited: true }),
  q7_devices: Object.freeze({ rows: 'devices', clock: null, source: 'mart.v_device_liveness', audited: false }),
  q10_audit_trail: Object.freeze({ rows: 'audit', clock: 'occurred_at', source: 'ops.audit', audited: true }),
});

const SAMPLE_NON_FILTERS = Object.freeze(['window', 'limit', 'cursor']);

function sampleMatches(row, name, value) {
  // On an event, `class` is a predicate over the labels; on a finding it is a column.
  if (name === 'class' && 'labels' in row) return Array.isArray(row.labels) && row.labels.some((l) => l.class === value);
  // prompt_kind is an eq filter; prompt_kind_not is the ne filter the default-hide uses. NULL
  // reads as 'unknown', exactly as the read layer coalesces it.
  if (name === 'prompt_kind') return samplePromptKind(row) === value;
  if (name === 'prompt_kind_not') return samplePromptKind(row) !== value;
  return row[name] === value;
}

function samplePromptKind(row) {
  return row.prompt_kind ?? 'unknown';
}

function sampleEnvelope(resultState, body) {
  return Object.freeze({ api_version: '1', query_version: '1', result_state: resultState, ...body });
}

function sampleObservations(event) {
  return event.observed_routes.map((route, i) => Object.freeze({
    observation_event_id: `obs_${event.submission_id.slice(0, 8)}_${i + 1}`,
    observation_source: route,
    observation_kind: event.detection_basis,
    direction: event.detection_basis === 'discovery' ? 'none' : 'egress',
    observation_occurred_at: i === 0 ? event.first_occurred_at : event.last_occurred_at,
    observation_size_bytes: event.size_bytes,
  }));
}

/** A record read as query-api answers it: one row per observation, the submission's columns repeated under the store's names. */
function sampleRecordRows(event) {
  const head = {
    submission_id: event.submission_id,
    received_at: event.received_at,
    first_occurred_at: event.first_occurred_at,
    last_occurred_at: event.last_occurred_at,
    user_ref: event.subject,
    tool_fingerprint: event.tool,
    tool_name: null,
    kind: event.detection_basis,
    collection_mode: event.mode,
    policy_action: event.action,
    policy_rule_id: null,
    decided_locally: event.mode !== 'm3',
    confidence: event.confidence,
    merge_confidence: event.merge_confidence,
    content_state: event.content_state,
    shredded_reason: event.content_state === 'shredded' ? 'retention_expired' : null,
    size_bytes: event.size_bytes,
    content_digest: null,
    labels: event.labels,
    classifier_version: null,
    winning_source: event.route,
    winning_fidelity: null,
    observed_routes: event.observed_routes,
    observation_count: event.observation_count,
    expires_at: null,
  };
  return sampleObservations(event).map((o) => Object.freeze({
    ...head,
    ...o,
    observation_received_at: event.received_at,
    observation_labels: event.labels,
    policy_decision: null,
    detection_basis: event.detection_basis,
    window_start: null,
    window_end: null,
    submission_count: null,
    bytes_total: null,
  }));
}

const SAMPLE_PROMPTS = Object.freeze([
  'Summarise the Q4 revenue deck for the board in five bullet points.',
  'Rewrite this customer complaint reply so it sounds less defensive.',
  'Why does this function return undefined when the list is empty?',
  'Draft an email to the supplier about the late delivery of the March order.',
  'Explain the indemnity clause in this contract in plain English.',
  'Turn these meeting notes into action items with owners and dates.',
  'What is the capital of Australia?',
  'Write a SQL query that finds customers with no orders in the last 90 days.',
]);

/** What the person typed for an event. Invented, and stable for a given event. */
function sampleTyped(event) {
  const n = parseInt(event.submission_id.slice(0, 8), 16);
  return SAMPLE_PROMPTS[n % SAMPLE_PROMPTS.length];
}

/** Everything the sample device captured: the typed text inside the context a client adds. */
function sampleCapture(event) {
  return `<system-reminder>\nClient context for ${event.tool}.\n</system-reminder>\n${sampleTyped(event)}`;
}

/**
 * The fake.
 *
 * @param {object} [input]
 * @param {() => Date} [input.now]
 * @param {string} [input.scenario]  one of EXPLORE_SCENARIO_NAMES
 * @param {number} [input.latencyMs] a pause before each answer, so a loading state can be seen
 */
export function createExploreFake({ now = () => new Date(), scenario = 'realistic', latencyMs = 0 } = {}) {
  let current = EXPLORE_SCENARIO_NAMES.includes(scenario) ? scenario : 'realistic';
  const sample = buildExploreSample(now());
  const cursors = new Map();
  let cursorSerial = 0;
  let auditSerial = 8200;

  const freshFor = (source) => freshness({ aggregate: source, last_run_at: sampleIso(now().getTime() - 180_000), last_complete_bucket: sampleIso(now().getTime() - 240_000) });
  const audited = () => {
    auditSerial += 1;
    return Object.freeze({ entry_id: String(auditSerial), written_at: sampleIso(now().getTime()) });
  };

  function listReply(body) {
    const spec = SAMPLE_LISTS[body.template];
    const params = body.params ?? {};
    if (current === 'busy') return STATE_ENVELOPES.busy;
    if (current === 'audit_unavailable' && spec.audited) return STATE_ENVELOPES.audit_unavailable;
    if (current === 'too_broad' && spec.clock) {
      return sampleEnvelope('query_too_broad', {
        error: { code: 'window_too_wide', message: 'A list read is bounded to a 31-day window.', fixable: true, detail: { max_days: 31 } },
      });
    }

    const queryKey = JSON.stringify(Object.entries(params).filter(([name]) => name !== 'cursor').sort());
    let offset = 0;
    if (typeof params.cursor === 'string') {
      const held = cursors.get(params.cursor);
      cursors.delete(params.cursor);
      if (current === 'cursor_expired' || !held || held.queryKey !== queryKey || held.expires < now().getTime()) return STATE_ENVELOPES.cursor_expired;
      offset = held.offset;
    }

    let rows = current === 'empty' ? [] : sample[spec.rows];
    if (spec.clock && params.window) {
      const from = Date.parse(params.window.from);
      const to = Date.parse(params.window.to);
      rows = rows.filter((row) => {
        const at = Date.parse(row[spec.clock]);
        return at >= from && at < to;
      });
    }
    for (const [name, value] of Object.entries(params)) {
      if (SAMPLE_NON_FILTERS.includes(name)) continue;
      rows = rows.filter((row) => sampleMatches(row, name, value));
    }

    const limit = typeof params.limit === 'number' ? params.limit : 50;
    const slice = rows.slice(offset, offset + limit);
    let nextCursor = null;
    if (offset + limit < rows.length) {
      cursorSerial += 1;
      nextCursor = `sample.${cursorSerial}.${sampleHex(Math.imul(cursorSerial, 2654435761), 8)}`;
      cursors.set(nextCursor, { offset: offset + limit, queryKey, expires: now().getTime() + SAMPLE_CURSOR_TTL_MS });
    }
    const covered = current === 'empty' ? coverage() : coverage({ state: 'partial' });
    // No rows on partial coverage is not "nothing happened": the absence is a floor too.
    const resultState = rows.length > 0 ? 'ok' : covered.state === 'partial' ? 'coverage_degraded' : 'empty';
    return sampleEnvelope(resultState, {
      data: slice,
      page: Object.freeze({
        returned: slice.length,
        next_cursor: nextCursor,
        snapshot_upper_bound: sampleIso(now().getTime()),
        newer_events_exist: false,
      }),
      freshness: freshFor(spec.source),
      coverage: covered,
      ...(spec.audited ? { audit: audited() } : {}),
      meta: { source: spec.source, kind: 'list' },
    });
  }

  function recordReply(body) {
    if (current === 'busy') return STATE_ENVELOPES.busy;
    if (current === 'audit_unavailable') return STATE_ENVELOPES.audit_unavailable;
    if (current === 'destroyed') return STATE_ENVELOPES.no_longer_available;
    const event = sample.events.find((row) => row.submission_id === body.params?.submission_id);
    if (!event) return STATE_ENVELOPES.not_found;
    return sampleEnvelope('ok', {
      data: sampleRecordRows(event),
      freshness: freshFor('ingest.submission'),
      coverage: coverage({ state: 'partial' }),
      audit: audited(),
      meta: { source: 'ingest.submission', kind: 'record', content_state: event.content_state },
    });
  }

  // One device's collector rows: a list document with an equality filter on the device, not audited.
  function collectorsReply(body) {
    if (current === 'busy') return STATE_ENVELOPES.busy;
    const device = (body.filters ?? []).find((f) => f.field === 'device' && f.op === 'eq')?.value;
    if (!device) return STATE_ENVELOPES.refused_shape;
    const rows = current === 'empty' ? [] : sample.collectors.filter((row) => row.device === device);
    return sampleEnvelope(rows.length > 0 ? 'ok' : 'empty', {
      data: rows,
      page: Object.freeze({ returned: rows.length, next_cursor: null, snapshot_upper_bound: sampleIso(now().getTime()), newer_events_exist: false }),
      freshness: freshFor('ops.collector_state'),
      coverage: coverage(),
      meta: { source: 'ops.collector_state', kind: 'list' },
    });
  }

  const answer = (body) => {
    if (body?.source === 'ops.collector_state') return collectorsReply(body);
    if (SAMPLE_LISTS[body?.template]) return listReply(body);
    if (body?.template === 'q9_event_detail') return recordReply(body);
    return STATE_ENVELOPES.refused_shape;
  };

  // ── the content reads: prompt-text search and approved retrieval, over the same sample ──────
  //
  // Only an event whose content was uploaded has anything to find or retrieve, exactly as in the
  // product. The refusals are the vault's own reasons, so the page's handling of them can be seen.

  const pause = async () => {
    if (latencyMs > 0) await new Promise((resolve) => { setTimeout(resolve, latencyMs); });
  };
  const contentRefusal = (code, message) => Object.freeze({ state: 'refused', error: Object.freeze({ code, message }) });
  const uploaded = () => (current === 'empty' ? [] : sample.events.filter((event) => event.content_state === 'uploaded'));

  async function searchContent(body) {
    await pause();
    if (current === 'busy') return contentRefusal('busy', 'The service is at its concurrency limit; retry shortly.');
    if (current === 'audit_unavailable') return contentRefusal('audit_unavailable', 'The search audit row could not be committed, so the search fails closed.');
    const words = String(body?.query ?? '').toLowerCase().split(/[^a-z0-9_]+/).filter(Boolean);
    if (words.length === 0) return contentRefusal('search_disabled', 'An empty search is refused rather than returning the whole index.');
    const window = body?.window ?? null;
    const hits = [];
    for (const event of uploaded()) {
      // The same filters the vault composes against ingest.submission: person, tool, device, mode
      // and the received-at window. The sample carries all five on each event.
      if (body?.subject && (event.subject ?? '') !== body.subject) continue;
      if (body?.tool && (event.tool ?? '') !== body.tool) continue;
      if (body?.device && (event.device ?? '') !== body.device) continue;
      if (body?.mode && (event.mode ?? '') !== body.mode) continue;
      if (window?.from && Date.parse(event.received_at) < Date.parse(window.from)) continue;
      if (window?.to && Date.parse(event.received_at) >= Date.parse(window.to)) continue;
      const typed = sampleTyped(event);
      const lower = typed.toLowerCase();
      if (!words.every((word) => lower.includes(word))) continue;
      const snippet = typed.replace(new RegExp(`\\b(${words.join('|')})\\b`, 'gi'), '<em>$1</em>');
      hits.push(Object.freeze({ submission_id: event.submission_id, snippet, rank: 1, subject: event.subject ?? null, device: event.device ?? null, tool: event.tool ?? null, received_at: event.received_at }));
    }
    // Newest first, the order the vault returns. The cursor is a plain offset here; the real one is
    // the vault's opaque keyset.
    hits.sort((a, b) => Date.parse(b.received_at) - Date.parse(a.received_at));
    const limit = typeof body?.limit === 'number' && body.limit > 0 ? body.limit : 20;
    const offset = body?.cursor ? Number(body.cursor) || 0 : 0;
    const page = hits.slice(offset, offset + limit);
    const next = offset + limit < hits.length ? String(offset + limit) : null;
    return Object.freeze({ state: 'available', hits: Object.freeze(page), truncated: next !== null, next_cursor: next });
  }

  // The retrieval request mints a URL; the content is served only when that URL is fetched, and
  // only once — the same single-use shape the vault has.
  const pendingRetrievals = new Map();

  async function retrieveContent(body) {
    await pause();
    if (current === 'busy') return contentRefusal('busy', 'The service is at its concurrency limit; retry shortly.');
    if (current === 'audit_unavailable') return contentRefusal('audit_unavailable', 'The audit row could not be committed, so no content is served.');
    const ids = Array.isArray(body?.event_ids) ? body.event_ids : [];
    const event = sample.events.find((row) => sampleObservations(row).some((o) => ids.includes(o.observation_event_id)));
    if (!event || (event.content_state !== 'uploaded' && event.content_state !== 'shredded')) {
      return contentRefusal('no_content_object', 'No content object is stored for this event.');
    }
    if (event.content_state === 'shredded' || current === 'destroyed') {
      return Object.freeze({ state: 'no_longer_available', reason: 'retention_expired', receipt_ref: 'rcpt_sample_0001' });
    }
    const url = `/v1/content/retrieval/fake/${sampleUuid(0x9a, sample.events.indexOf(event))}`;
    pendingRetrievals.set(url, sampleCapture(event));
    return Object.freeze({
      state: 'available',
      event_id: ids[0],
      grant_id: sampleUuid(0x9a, sample.events.indexOf(event)),
      raw_digest: `sha256:${sampleHex(Math.imul(sample.events.indexOf(event) + 1, 2246822519), 8).repeat(8)}`,
      retrieval_url: url,
    });
  }

  async function readRetrieval(url) {
    await pause();
    if (!pendingRetrievals.has(url)) return contentRefusal('grant_already_used', 'The retrieval URL was already redeemed.');
    const content = pendingRetrievals.get(url);
    pendingRetrievals.delete(url);
    return Object.freeze({ state: 'available', content });
  }

  return Object.freeze({
    content: Object.freeze({ search: searchContent, retrieve: retrieveContent, readUrl: readRetrieval }),
    async send(body) {
      if (latencyMs > 0) await new Promise((resolve) => { setTimeout(resolve, latencyMs); });
      return answer(body);
    },
    setScenario(name) {
      if (EXPLORE_SCENARIO_NAMES.includes(name)) current = name;
      return current;
    },
    scenario() {
      return current;
    },
    sample,
  });
}
