// views.js — the information architecture of docs/04 §11, as view models.
//
// Screens produce *view models*, not markup: a tile, a table, a series and a set of notes. render.js
// turns those into HTML. The split is what makes the honesty rules testable without a DOM — a test
// asserts on `tile.value.kind`, which is where "suppressed" and "zero" are still distinguishable.
//
// Every number a screen shows comes from `measureOf`/`sumMeasure` in states.js. There is no other
// path to a value, so the merge §6.3 forbids cannot happen in one screen while being avoided in
// another.

import { measureOf, isSuppressed, vocabOf } from './states.js';
import { formatBytes, formatCount, formatDuration, formatInstant, formatLabels, formatScore, shortId } from './format.js';
import { K, SOURCES, TEMPLATES } from './vocab.js';

/**
 * A total over a set of cells, with the effect of suppression stated rather than hidden.
 *
 * If any contributing cell is suppressed, the sum is a FLOOR: the suppressed contribution is
 * unknown, so the number shown is a lower bound and says so.
 */
export function sumMeasure(state, measure) {
  let total = 0;
  let counted = 0;
  let suppressed = 0;
  let absent = 0;
  for (const row of state.data) {
    const value = measureOf(row, measure);
    if (value.kind === 'number') {
      total += value.value;
      counted += 1;
    } else if (value.kind === 'suppressed') {
      suppressed += 1;
    } else {
      absent += 1;
    }
  }
  if (counted === 0 && suppressed > 0) return { kind: 'suppressed', k: state.suppression?.k ?? K, suppressedCells: suppressed };
  if (counted === 0) return { kind: 'absent' };
  if (suppressed > 0) return { kind: 'floor', value: total, suppressedCells: suppressed };
  void absent;
  return { kind: 'number', value: total };
}

function tile(label, value, note) {
  return Object.freeze({ label, value, note: note ?? null });
}

/** A tile from a summed measure, carrying the floor/suppressed distinction into the label. */
export function measureTile(label, state, measure, formatter = formatCount) {
  const total = sumMeasure(state, measure);
  if (total.kind === 'number') return tile(label, { kind: 'number', text: formatter(total.value) }, null);
  if (total.kind === 'suppressed') {
    return tile(label, { kind: 'suppressed', text: 'suppressed', k: total.k, suppressedCells: total.suppressedCells },
      `Every contributing cell was below k = ${total.k} subjects.`);
  }
  if (total.kind === 'floor') {
    return tile(label, { kind: 'floor', text: `≥ ${formatter(total.value)}` },
      `${total.suppressedCells} cell(s) suppressed: this is a floor, not a total.`);
  }
  return tile(label, { kind: 'absent', text: '—' }, 'This measure is not carried by any returned cell.');
}

function column(key, label, kind = 'text') {
  return Object.freeze({ key, label, kind });
}

/** A table straight from a list or aggregate page, with the closed-vocabulary columns kept apart. */
export function tableFrom(state, { title, columns, emptyText }) {
  return Object.freeze({
    title,
    columns: Object.freeze(columns),
    rows: Object.freeze(state.data.map((row) => Object.freeze({ row, vocab: vocabOf(row), suppressed: isSuppressed(row) }))),
    emptyText: emptyText ?? 'No rows.',
    suppressedCells: state.data.filter(isSuppressed).length,
  });
}

/**
 * A series for a chart. Only an aggregate source can produce one: a chart computed by scanning
 * event rows is the thing C27 forbids, so a screen whose source is an event list gets no series at
 * all rather than an approximate one.
 */
export function seriesFrom(state, { measure, dim = 'tool', title }) {
  const source = state.meta?.source ?? '';
  if (!source.startsWith('mart.')) {
    return Object.freeze({ title, unavailable: true, reason: `${source || 'this source'} is not a precomputed aggregate, so it cannot back a chart (C27).`, points: [] });
  }
  const byBucket = new Map();
  for (const row of state.data) {
    const key = row.bucket ?? 'total';
    if (!byBucket.has(key)) byBucket.set(key, []);
    byBucket.get(key).push(row);
  }
  const points = [...byBucket.entries()].map(([bucket, rows]) => {
    let value = 0;
    let suppressed = 0;
    for (const row of rows) {
      const v = measureOf(row, measure);
      if (v.kind === 'number') value += v.value;
      else if (v.kind === 'suppressed') suppressed += 1;
    }
    return Object.freeze({
      bucket,
      value: suppressed > 0 ? { kind: 'floor', value, suppressedCells: suppressed } : { kind: 'number', value },
      series: rows.length,
    });
  }).sort((a, b) => String(a.bucket).localeCompare(String(b.bucket)));
  return Object.freeze({ title, points, dim, measure, unavailable: false, reason: null });
}

/** The cells of one dimension, ranked by a measure — with suppressed cells kept in the ranking. */
export function rankedCells(state, measure, key) {
  return [...state.data]
    .map((row) => ({ row, key: row[key], value: measureOf(row, measure) }))
    .sort((a, b) => {
      const av = a.value.kind === 'number' ? a.value.value : -1;
      const bv = b.value.kind === 'number' ? b.value.value : -1;
      if (av !== bv) return bv - av;
      return String(a.key ?? '').localeCompare(String(b.key ?? ''));
    });
}

/** Banner + notes shared by every screen, so no screen can forget them. */
function shared(state, extraNotes = []) {
  const notes = [];
  if (state.meta?.applied_bucket && state.meta.applied_bucket !== (state.meta.requested_bucket ?? state.meta.applied_bucket)) {
    notes.push(`Bucket applied: ${state.meta.applied_bucket} (requested ${state.meta.requested_bucket}).`);
  }
  if (state.meta?.coarsened) {
    notes.push(`The series was auto-coarsened from ${state.meta.coarsened.from} to ${state.meta.coarsened.to}: ${state.meta.coarsened.reason}.`);
  }
  if (state.meta?.measure_semantics) {
    const lower = Object.entries(state.meta.measure_semantics).filter(([, kind]) => kind === 'distinct_lower_bound');
    if (lower.length > 0) notes.push(`${lower.map(([m]) => m).join(', ')} is a distinct-subject count served as a lower bound: it can under-count when a cell combines rows.`);
  }
  if (state.meta?.warnings) notes.push(...state.meta.warnings);
  if (state.suppression?.k) notes.push(`k = ${state.suppression.k} distinct subjects per cell; ${state.suppression.suppressed_cells ?? 0} cell(s) suppressed in this response.`);
  if (state.audit?.entry_id) notes.push(`Audited read: entry ${state.audit.entry_id} at ${formatInstant(state.audit.written_at)}.`);
  notes.push(...extraNotes);
  return Object.freeze({ banners: state.banners, notes: Object.freeze(notes) });
}

function screen(id, title, question, source, body) {
  return Object.freeze({
    id,
    title,
    question,
    source,
    sourceLabel: SOURCES[source]?.label ?? source,
    ...body,
  });
}

// ---------------------------------------------------------------------------------------------
// Posture — the landing screen (§11.1)
// ---------------------------------------------------------------------------------------------

/**
 * Posture is the landing page, not "top tools": the health of the instrument comes before any
 * instrument reading, because a ranked list whose denominator is unknown is the most confident
 * looking and least trustworthy screen in the product.
 *
 * @param {object} input
 * @param {object} input.devices   view state for the device list
 * @param {object} [input.coverage] a coverage block, when the device read did not carry one
 * @param {ReadonlyArray<object>} [input.watermarks] one per aggregate the other screens rely on
 */
export function postureView({ devices, coverage, watermarks = [] }) {
  const cov = devices?.coverage ?? coverage ?? null;
  const byLiveness = new Map();
  for (const row of devices?.data ?? []) {
    const key = row.liveness ?? 'unknown';
    byLiveness.set(key, (byLiveness.get(key) ?? 0) + 1);
  }
  const notReporting = (devices?.data ?? []).filter((row) => row.liveness && row.liveness !== 'reporting');
  const dropped = (devices?.data ?? []).reduce((sum, row) => sum + (typeof row.spool_dropped_total === 'number' ? row.spool_dropped_total : 0), 0);

  const gaps = Object.entries(cov?.gap_reasons ?? {}).map(([reason, count]) => Object.freeze({ reason, count }));
  const coverageTiles = [
    tile('Devices reporting', cov?.devices_reporting === null || cov?.devices_reporting === undefined
      ? { kind: 'absent', text: '—' }
      : { kind: 'number', text: formatCount(cov.devices_reporting) },
    cov?.devices_enrolled === null || cov?.devices_enrolled === undefined
      ? 'The enrolled denominator is unavailable: no fleet figure is shown without it.'
      : `of ${formatCount(cov.devices_enrolled)} enrolled devices`),
    tile('Coverage state', { kind: 'vocab', text: cov?.state ?? 'unknown' }, null),
    tile('Gaps', { kind: 'number', text: formatCount(gaps.reduce((a, g) => a + g.count, 0)) }, `${gaps.length} categor${gaps.length === 1 ? 'y' : 'ies'}`),
  ];

  return screen('posture', 'Posture', null, devices?.meta?.source ?? 'mart.v_device_liveness', {
    subtitle: 'Coverage, freshness and the gaps the product can see. Every other screen is read through this one.',
    tiles: Object.freeze(coverageTiles),
    tables: Object.freeze([
      tableFrom(devices ?? { data: [] }, {
        title: 'Devices not reporting',
        columns: [
          column('device', 'Device'),
          column('device_os', 'OS', 'vocab'),
          column('managed_state', 'Managed', 'vocab'),
          column('liveness', 'Liveness', 'vocab'),
          column('collector_state', 'Collector', 'vocab'),
          column('last_seen_at', 'Last seen', 'instant'),
          column('spool_dropped_total', 'Dropped', 'count'),
        ],
        emptyText: 'No device is silent in this page.',
      }),
      Object.freeze({
        title: 'Coverage gaps by reason',
        columns: Object.freeze([column('reason', 'Reason', 'vocab'), column('count', 'Collector-days', 'count')]),
        rows: Object.freeze(gaps.map((g) => Object.freeze({ row: g, vocab: { gap_reason: g.reason }, suppressed: false }))),
        emptyText: 'No gaps recorded for this window.',
        suppressedCells: 0,
      }),
      Object.freeze({
        title: 'Aggregate watermarks',
        columns: Object.freeze([
          column('aggregate', 'Aggregate'),
          column('last_run_at', 'Last run', 'instant'),
          column('last_complete_bucket', 'Complete to', 'instant'),
          column('lag_seconds', 'Lag', 'seconds'),
          column('state', 'State', 'vocab'),
        ]),
        rows: Object.freeze(watermarks.map((w) => Object.freeze({ row: w, vocab: { state: w.state }, suppressed: false }))),
        emptyText: 'No watermark has been read yet.',
        suppressedCells: 0,
      }),
    ]),
    series: Object.freeze([]),
    extras: Object.freeze({
      livenessCounts: Object.freeze([...byLiveness.entries()].map(([state, count]) => Object.freeze({ state, count }))),
      notReportingCount: notReporting.length,
      spoolDroppedTotal: dropped,
      legend: Object.freeze(['reporting', 'stale', 'never_reported', 'revoked']),
    }),
    ...shared(devices ?? { data: [], banners: Object.freeze([]), meta: {} }, [
      'An absence of events is ambiguous; a health signal is not. A silent device is a row produced by the server, not a gap an analyst must notice.',
      'A revoked device is not a quiet one: revoked, stale, never_reported and reporting are four facts.',
    ]),
  });
}

// ---------------------------------------------------------------------------------------------
// The ten questions
// ---------------------------------------------------------------------------------------------

export function toolsView(state) {
  return screen('tools', 'Tools', TEMPLATES.q1_tools_ranked.title, 'mart.v_tool_usage', {
    subtitle: 'Ranked by submissions, with present-tense sanctioned state joined at read time.',
    tiles: Object.freeze([
      measureTile('Submissions', state, 'submissions'),
      measureTile('People (lower bound)', state, 'users'),
      measureTile('Bytes', state, 'bytes_total', formatBytes),
      measureTile('Blocked', state, 'blocked'),
    ]),
    tables: Object.freeze([
      tableFrom(state, {
        title: 'Tools by submissions',
        columns: [
          column('bucket', 'Bucket', 'instant'),
          column('tool', 'Tool'),
          column('sanctioned_state', 'Sanction', 'vocab'),
          column('submissions', 'Submissions', 'measure'),
          column('users', 'People', 'measure'),
          column('bytes_total', 'Bytes', 'measure-bytes'),
          column('blocked', 'Blocked', 'measure'),
        ],
        emptyText: 'No tool was in use in this window.',
      }),
    ]),
    series: Object.freeze([seriesFrom(state, { measure: 'submissions', title: 'Submissions per bucket' })]),
    ...shared(state, [
      'Rank is by submissions with a deterministic tie-break, and is never a sanction signal.',
      'Unknown is its own answer: a tool with no decision renders as unknown, never as unsanctioned.',
    ]),
  });
}

export function unsanctionedView(state) {
  const tools = new Set(state.data.map((row) => row.tool));
  return screen('unsanctioned', 'Unsanctioned', TEMPLATES.q2_unsanctioned_users.title, 'mart.agg_tool_user_period', {
    subtitle: 'Which tools are unsanctioned, and which people are using them. Subject-bearing: this read is audited.',
    tiles: Object.freeze([
      measureTile('Submissions', state, 'submissions'),
      tile('Tools involved', { kind: 'number', text: formatCount(tools.size) }, null),
      tile('People in this page', { kind: 'number', text: formatCount(state.data.filter((r) => !isSuppressed(r)).length) }, 'A page count, not a fleet total.'),
      tile('Suppressed cells', { kind: 'number', text: formatCount(state.data.filter(isSuppressed).length) }, `Below k = ${state.suppression?.k ?? K} subjects.`),
    ]),
    tables: Object.freeze([
      tableFrom(state, {
        title: 'People by tool (ordered by tool, then person — not by volume)',
        columns: [
          column('tool', 'Tool'),
          column('subject', 'Person'),
          column('submissions', 'Submissions', 'measure'),
          column('bytes_total', 'Bytes', 'measure-bytes'),
        ],
        emptyText: 'Nobody used an unsanctioned tool in this window.',
      }),
    ]),
    series: Object.freeze([]),
    ...shared(state, [
      'This table is ordered by tool and then by person: it is a list, not a ranking. There is no view of people sorted by volume.',
      'A suppressed row is a row that exists: the cell had fewer than k subjects in it.',
    ]),
  });
}

export function teamsView(state) {
  const org = state.meta?.extras?.org_coverage?.[0] ?? null;
  const notes = [];
  if (org) {
    const unmapped = Math.max(0, (org.users_all ?? 0) - (org.users_mapped ?? 0));
    notes.push(
      `Unmapped people are an explicit series: ${formatCount(org.users_all)} user-days in the window, ${formatCount(org.users_mapped)} with a department, ${formatCount(unmapped)} without.`,
    );
  }
  return screen('teams', 'Teams', TEMPLATES.q3_team_growth.title, 'mart.agg_org_period', {
    subtitle: 'Per-team usage. Empty until the directory is synchronised, and it says so rather than drawing a zero line.',
    tiles: Object.freeze([
      measureTile('Submissions', state, 'submissions'),
      measureTile('People (lower bound)', state, 'users'),
      tile('Teams in this page', { kind: 'number', text: formatCount(new Set(state.data.map((r) => r.department)).size) }, null),
      tile('Mapped user-days', org ? { kind: 'number', text: formatCount(org.users_mapped) } : { kind: 'absent', text: '—' }, org ? `of ${formatCount(org.users_all)}` : 'No directory coverage read came back with this page.'),
    ]),
    tables: Object.freeze([
      tableFrom(state, {
        title: 'Teams by submissions',
        columns: [
          column('bucket', 'Bucket', 'instant'),
          column('department', 'Department'),
          column('submissions', 'Submissions', 'measure'),
          column('users', 'People', 'measure'),
        ],
        emptyText: 'No team has usage in this window.',
      }),
    ]),
    series: Object.freeze([seriesFrom(state, { measure: 'submissions', title: 'Submissions per bucket' })]),
    ...shared(state, notes.concat([
      'A team cell below k people is suppressed: a two-person team\'s daily count is that team\'s data.',
    ])),
  });
}

export function classesView(state) {
  const classTotal = state.meta?.extras?.class_total?.[0]?.submissions_total ?? null;
  const notes = [];
  const degraded = sumMeasure(state, 'degraded_events');
  if (degraded.kind === 'number' && degraded.value > 0) {
    notes.push(`${formatCount(degraded.value)} events in this window could not be classified: a classifier failure is not a fall in sensitive data.`);
  } else if (degraded.kind === 'absent') {
    notes.push('This response carries no degraded_events measure, so classifier health for this window is unknown.');
  }
  if (classTotal !== null) {
    notes.push(`The non-additive total for the same window is ${formatCount(classTotal)} submissions, from a different source: class rows fan out and must not be summed as submissions.`);
  }
  return screen('classes', 'Classes', TEMPLATES.q4_class_mix.title, 'mart.agg_class_period', {
    subtitle: 'Sensitive-data classes, with the classifier health that produced them.',
    tiles: Object.freeze([
      measureTile('Class-carrying submissions (fan-out)', state, 'submissions'),
      measureTile('People (lower bound)', state, 'users'),
      measureTile('Max score', state, 'max_score', formatScore),
      tile('Non-additive total', classTotal === null ? { kind: 'absent', text: '—' } : { kind: 'number', text: formatCount(classTotal) },
        classTotal === null ? 'No separately-computed total came back with this page.' : 'From the tool aggregate, not from summing class rows.'),
    ]),
    tables: Object.freeze([
      tableFrom(state, {
        title: 'Classes by submissions',
        columns: [
          column('class', 'Class', 'vocab'),
          column('severity', 'Severity', 'vocab'),
          column('tool', 'Tool'),
          column('classifier_version', 'Classifier', 'vocab'),
          column('submissions', 'Submissions carrying it', 'measure'),
          column('users', 'People', 'measure'),
          column('max_score', 'Max score', 'measure-score'),
          column('degraded_events', 'Degraded', 'measure'),
        ],
        emptyText: 'No sensitive data was classified in this window.',
      }),
    ]),
    series: Object.freeze([seriesFrom(state, { measure: 'submissions', title: 'Class-carrying submissions per bucket' })]),
    ...shared(state, notes),
  });
}

export function findingsView(state) {
  const reviews = new Map();
  for (const row of state.data) if (row.review_state) reviews.set(row.review_state, (reviews.get(row.review_state) ?? 0) + 1);
  return screen('findings', 'Findings', TEMPLATES.q5_findings.title, 'mart.v_finding', {
    subtitle: 'Submissions that hit a policy rule, with the review state nobody has defaulted for them.',
    tiles: Object.freeze([
      tile('Findings in page', { kind: 'number', text: formatCount(state.data.length) }, null),
      ...[...reviews.entries()].map(([name, count]) => tile(`Review: ${name}`, { kind: 'number', text: formatCount(count) }, name === 'open' ? 'Nobody has looked' : null)),
    ]),
    tables: Object.freeze([
      tableFrom(state, {
        title: 'Findings',
        columns: [
          column('detected_at', 'Detected', 'instant'),
          column('severity', 'Severity', 'vocab'),
          column('rule_title', 'Rule'),
          column('class', 'Class', 'vocab'),
          column('subject', 'Person'),
          column('tool', 'Tool'),
          column('review_state', 'Review', 'vocab'),
        ],
        emptyText: 'No finding was raised in this window.',
      }),
    ]),
    series: Object.freeze([]),
    ...shared(state, [
      'Severity is as-of-detection: reclassifying a rule today does not relabel this history.',
      'Open means nobody has looked. It is not "reviewed and unremarkable".',
    ]),
  });
}

export function activityView(state) {
  const lowMerge = state.data.filter((row) => row.merge_confidence === 'low').length;
  const lateFlush = state.data.filter((row) => row.received_at && row.first_occurred_at
    && Date.parse(row.received_at) - Date.parse(row.first_occurred_at) > 3_600_000).length;
  return screen('activity', 'Activity', TEMPLATES.q8_activity.title, 'ingest.submission', {
    subtitle: 'A bounded, cursor-paged list of events. The only path in this product that touches event rows.',
    tiles: Object.freeze([
      tile('Rows in page', { kind: 'number', text: formatCount(state.data.length) }, state.page?.next_cursor ? 'More rows exist: page only while next_cursor is present.' : 'This is the last page.'),
      tile('Low-confidence merges', { kind: 'number', text: formatCount(lowMerge) }, `of ${state.data.length} rows in this page — a page count, not a window total.`),
      tile('Flushed late (> 1 h)', { kind: 'number', text: formatCount(lateFlush) }, 'Received long after the device clock said it happened: a spool flush, not a burst of activity.'),
      tile('Newer events since snapshot', { kind: 'vocab', text: state.page?.newer_events_exist ? 'true' : 'false' }, state.page?.snapshot_upper_bound ? `Snapshot frozen at ${formatInstant(state.page.snapshot_upper_bound)}` : null),
    ]),
    tables: Object.freeze([
      tableFrom(state, {
        title: 'Events',
        columns: [
          column('received_at', 'Received (server)', 'instant'),
          column('first_occurred_at', 'Occurred (device)', 'device-clock'),
          column('subject', 'Person'),
          column('tool', 'Tool'),
          column('action', 'Action', 'vocab'),
          column('content_state', 'Content', 'vocab'),
          column('mode', 'Mode', 'vocab'),
          column('merge_confidence', 'Merge', 'vocab'),
          column('observation_count', 'Routes', 'count'),
        ],
        emptyText: 'Nothing happened in this window.',
      }),
    ]),
    series: Object.freeze([]),
    ...shared(state, [
      'Both clocks are shown. Neither is normalised into the other: the device clock may be skewed.',
      'A short page is not the end of the results. Only next_cursor: null ends an iteration.',
    ]),
  });
}

export function personView(state, { subject }) {
  const series = seriesFrom(state, { measure: 'submissions', title: 'Submissions per day' });
  const flush = state.meta?.extras?.flush_check?.[0] ?? null;
  const notes = [];
  if (flush) {
    notes.push(`${formatCount(flush.rows_in_window)} rows for this person in the window, ${formatCount(flush.late_flush_rows)} received more than an hour after they occurred: a spool flush spikes received time, not behaviour.`);
  }
  return screen('person', 'Person', TEMPLATES.q6_subject_series.title, 'mart.agg_user_period', {
    subtitle: `A lookup for ${subject}: this person's own baseline, never a peer comparison.`,
    tiles: Object.freeze([
      measureTile('Submissions', state, 'submissions'),
      measureTile('Tools used', state, 'tools_used'),
      measureTile('Block events', state, 'block_events'),
      measureTile('Bytes', state, 'bytes_total', formatBytes),
    ]),
    tables: Object.freeze([
      tableFrom(state, {
        title: 'Daily series',
        columns: [
          column('bucket', 'Day', 'instant'),
          column('subject', 'Person'),
          column('submissions', 'Submissions', 'measure'),
          column('tools_used', 'Tools', 'measure'),
          column('block_events', 'Blocked', 'measure'),
        ],
        emptyText: 'No usage recorded for this person in this window.',
      }),
    ]),
    series: Object.freeze([series]),
    ...shared(state, notes.concat([
      'No cohort percentile, no ranking, no people sorted by volume. A spike is compared against this person\'s own trailing baseline only.',
      'The spike rule needs at least 14 observed buckets. Below that the answer is not_yet_covered, not "no change".',
    ])),
  });
}

export function devicesView(state) {
  const counts = new Map();
  for (const row of state.data) {
    const liveness = row.liveness ?? 'unknown';
    const key = `${liveness}/${row.collector_state ?? 'no-collector'}`;
    counts.set(key, (counts.get(key) ?? 0) + 1);
  }
  const dropped = state.data.reduce((sum, row) => sum + (typeof row.spool_dropped_total === 'number' ? row.spool_dropped_total : 0), 0);
  return screen('devices', 'Devices', TEMPLATES.q7_devices.title, 'mart.v_device_liveness', {
    subtitle: 'Every device, including the ones that are not reporting: silence is a row produced by the server.',
    tiles: Object.freeze([
      tile('Devices in page', { kind: 'number', text: formatCount(state.data.length) }, null),
      tile('Not reporting', { kind: 'number', text: formatCount(state.data.filter((r) => r.liveness && r.liveness !== 'reporting').length) }, 'Stale, never reported or revoked — three different facts.'),
      tile('Spool dropped (page)', { kind: 'number', text: formatCount(dropped) }, 'Events the device dropped because its spool was full: a visible undercount.'),
      tile('Coverage', { kind: 'vocab', text: state.coverage?.state ?? 'unknown' }, state.coverageText?.text ?? null),
    ]),
    tables: Object.freeze([
      tableFrom(state, {
        title: 'Devices',
        columns: [
          column('device', 'Device'),
          column('device_os', 'OS', 'vocab'),
          column('managed_state', 'Managed', 'vocab'),
          column('liveness', 'Liveness', 'vocab'),
          column('collector', 'Collector', 'vocab'),
          column('collector_state', 'State', 'vocab'),
          column('last_seen_at', 'Last seen', 'instant'),
          column('spool_depth', 'Spool depth', 'count'),
          column('spool_dropped_total', 'Dropped', 'count'),
        ],
        emptyText: 'No device matched this filter.',
      }),
      Object.freeze({
        title: 'Liveness × collector state',
        columns: Object.freeze([column('pair', 'State pair', 'vocab'), column('count', 'Devices', 'count')]),
        rows: Object.freeze([...counts.entries()].map(([pair, count]) => Object.freeze({ row: { pair, count }, vocab: {}, suppressed: false }))),
        emptyText: 'No devices.',
        suppressedCells: 0,
      }),
    ]),
    series: Object.freeze([]),
    ...shared(state, [
      'Four liveness values stay distinct, and no health is inferred from silence.',
      'The denominator is the enrolled fleet. No fleet-wide percentage is shown that the product cannot compute.',
    ]),
  });
}

export function auditView(state) {
  return screen('audit', 'Audit', TEMPLATES.q10_audit_trail.title, 'ops.audit', {
    subtitle: 'What has been accessed, and by whom. Reading this log is itself a subject-level read.',
    tiles: Object.freeze([
      tile('Entries in page', { kind: 'number', text: formatCount(state.data.length) }, null),
      tile('Hash links', { kind: 'vocab', text: state.resultState === 'audit_chain_broken' ? 'broken' : 'verified in this page' }, 'Whole-chain verification is the reconciler\'s job, not a page\'s.'),
      tile('Anchored head', { kind: 'absent', text: 'not exposed' }, 'This API returns no anchor head or anchor time; the dashboard cannot show the last anchored entry.'),
    ]),
    tables: Object.freeze([
      tableFrom(state, {
        title: 'Audit entries',
        columns: [
          column('occurred_at', 'When', 'instant'),
          column('actor_type', 'Actor type', 'vocab'),
          column('actor', 'Actor'),
          column('action', 'Action', 'vocab'),
          column('object_type', 'Object type', 'vocab'),
          column('object_id', 'Object'),
          column('subject', 'Subject ref'),
          column('case', 'Case'),
        ],
        emptyText: 'Nothing has been accessed in this window.',
      }),
    ]),
    series: Object.freeze([]),
    ...shared(state, [
      'One audit row per query describes this read, and that row is not re-audited: recursion has to terminate somewhere.',
    ]),
  });
}

/**
 * Event detail (Q9 second half). What is shown depends on content_state, and each of the four is a
 * different answer — never a blank and never content.
 */
export function eventView(state) {
  const head = state.data[0] ?? null;
  const observations = state.data.slice(1);
  const contentState = head?.content_state ?? state.meta?.content_state ?? 'not_captured';
  const contentAnswer = {
    not_captured: 'Content was never read at this mode. Labels, digest, size and the policy action are all there is — and there is no index entry either.',
    local_only: 'Content was taken and remains on the device. It is not retrievable in v1: grants are device-initiated and there is no device-facing pull endpoint.',
    uploaded: 'Content is stored. Reaching it needs an approved, case-referenced, second-approved retrieval; this screen shows metadata only.',
    shredded: 'The content existed and has been destroyed. The reason and the receipt are the answer.',
  }[contentState] ?? 'Unknown content state.';
  return screen('event', 'Event detail', TEMPLATES.q9_event_detail.title, 'ingest.submission', {
    subtitle: 'One event, its observation routes, and what the content state permits. No query path returns full content.',
    tiles: Object.freeze([
      tile('Content state', { kind: 'vocab', text: contentState }, contentAnswer),
      tile('Routes', { kind: 'number', text: formatCount(head?.observation_count ?? observations.length) }, 'One observation per route, so an overlapping count can be explained.'),
      tile('Mode', { kind: 'vocab', text: head?.mode ?? '—' }, null),
      tile('Merge confidence', { kind: 'vocab', text: head?.merge_confidence ?? '—' }, head?.merge_confidence === 'low' ? 'A weak dedup key produced this row: counted, never discarded.' : null),
    ]),
    tables: Object.freeze([
      Object.freeze({
        title: 'Metadata',
        columns: Object.freeze([column('field', 'Field'), column('value', 'Value')]),
        rows: Object.freeze(head ? Object.entries(head)
          .filter(([key]) => !key.startsWith('observation_'))
          .map(([field, value]) => Object.freeze({ row: { field, value: renderable(value) }, vocab: {}, suppressed: false })) : []),
        emptyText: 'The record is not here. See the state above for why.',
        suppressedCells: 0,
      }),
      tableFrom({ data: observations }, {
        title: 'Observation routes',
        columns: [
          column('observation_source', 'Route', 'vocab'),
          column('observation_kind', 'Kind', 'vocab'),
          column('direction', 'Direction', 'vocab'),
          column('observation_occurred_at', 'Occurred', 'instant'),
          column('observation_size_bytes', 'Bytes', 'count'),
        ],
        emptyText: 'This event has no observation rows in this response.',
      }),
    ]),
    series: Object.freeze([]),
    ...shared(state, [
      'A hit is a reference, not a reservation: between a search and this read the record can be shredded, and no_longer_available with a receipt is the answer.',
      'Full content is reached only through the approved retrieval path, which this API does not expose.',
    ]),
  });
}

function renderable(value) {
  if (value === null || value === undefined) return '—';
  if (Array.isArray(value)) return formatLabels(value);
  if (typeof value === 'object') return JSON.stringify(value);
  return String(value);
}

/** A screen for a question that needs a parameter the analyst has not supplied yet. */
export function needsInputView({ id, title, question, hint }) {
  return Object.freeze({
    id,
    title,
    question,
    source: null,
    sourceLabel: null,
    subtitle: hint,
    needsInput: true,
    tiles: Object.freeze([]),
    tables: Object.freeze([]),
    series: Object.freeze([]),
    banners: Object.freeze([]),
    notes: Object.freeze([]),
  });
}

/**
 * A refusal, rendered as a screen. A refusal is a state with a sentence, not a blank page: the
 * dashboard must never show "nothing happened" where the API said "I cannot answer that".
 */
export function refusalView(state, { title }) {
  const error = state.error ?? {};
  const detail = error.detail ?? {};
  const fix = [];
  if (detail.fix?.coarser_bucket) fix.push(`coarsen the bucket to ${detail.fix.coarser_bucket}`);
  if (detail.fix?.drop_dimension) fix.push(`drop ${detail.fix.drop_dimension}`);
  if (detail.fix?.narrow_window || detail.fix?.or_narrow_window) fix.push('narrow the window');
  if (detail.fix?.lower_limit) fix.push('lower the page size');
  if (detail.fix?.add_filter) fix.push(`add a filter on ${Array.isArray(detail.fix.add_filter) ? detail.fix.add_filter.join(' or ') : detail.fix.add_filter}`);
  if (detail.fix?.alternative_source) fix.push(`read ${detail.fix.alternative_source}`);
  if (detail.or_use) fix.push(`or use ${detail.or_use}`);
  if (detail.max_days) fix.push(`window ≤ ${detail.max_days} days`);
  if (detail.max_values) fix.push(`≤ ${detail.max_values} values in a membership list`);
  if (detail.max_limit) fix.push(`limit ≤ ${detail.max_limit}`);
  const rows = [
    { field: 'result_state', value: state.resultState },
    { field: 'code', value: error.code ?? '—' },
    { field: 'message', value: error.message ?? '—' },
    ...(Object.keys(detail).filter((k) => k !== 'fix').map((k) => ({ field: `detail.${k}`, value: typeof detail[k] === 'object' ? JSON.stringify(detail[k]) : String(detail[k]) }))),
    ...(fix.length > 0 ? [{ field: 'fix', value: fix.join('; ') }] : []),
  ];
  return Object.freeze({
    id: `refusal-${state.resultState}`,
    title,
    question: null,
    source: null,
    sourceLabel: null,
    subtitle: state.spec?.label ?? 'The read was refused.',
    tiles: Object.freeze([
      tile('Result state', { kind: 'vocab', text: state.resultState }, state.spec?.label ?? null),
      tile('Rows served', { kind: 'number', text: '0' }, 'A refusal carries no data, by construction.'),
    ]),
    tables: Object.freeze([
      Object.freeze({
        title: 'What the API said',
        columns: Object.freeze([column('field', 'Field'), column('value', 'Value')]),
        rows: Object.freeze(rows.map((row) => Object.freeze({ row, vocab: {}, suppressed: false }))),
        emptyText: 'No detail.',
        suppressedCells: 0,
      }),
    ]),
    series: Object.freeze([]),
    banners: state.banners,
    notes: Object.freeze([
      'The API refuses rather than degrading, so the fix is always in the response. Present it as the action to take.',
      ...(fix.length > 0 ? [`Suggested fix: ${fix.join('; ')}.`] : []),
    ]),
  });
}

export { screen, tile, column };
