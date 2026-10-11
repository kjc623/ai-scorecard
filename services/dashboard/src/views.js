// views.js — the dashboard's screens, as view models.
//
// Screens produce *view models*, not markup: a tile, a table, a series and a set of notes. render.js
// turns those into HTML. The split is what makes the honesty rules testable without a DOM — a test
// asserts on `tile.value.kind`, which is where "absent" and "zero" are still distinguishable.
//
// Every number a screen shows comes from `measureOf`/`sumMeasure` in states.js. There is no other
// path to a value, so an absent measure cannot be merged with a zero in one screen while being
// kept apart in another.

import { emptyStateFor, measureOf, vocabOf } from './states.js';
import { formatBytes, formatCount, formatInstant, formatLabels, formatScore, plural } from './format.js';
import { SOURCES, TEMPLATES, UNRECOGNISED_TOOL, COLLECTION_MODE_LABELS } from './vocab.js';

/** A total over a set of cells. A set in which no cell carries the measure has no total. */
export function sumMeasure(state, measure) {
  let total = 0;
  let counted = 0;
  for (const row of state.data) {
    const value = measureOf(row, measure);
    if (value.kind === 'number') {
      total += value.value;
      counted += 1;
    }
  }
  if (counted === 0) return { kind: 'absent' };
  return { kind: 'number', value: total };
}

/**
 * A tile: a label, a value and a note. It may carry an address, the shape of its series, and a
 * split of its value into parts that each open the rows they count.
 */
function tile(label, value, note, { href = null, trend = null, split = null, moreLabel = null } = {}) {
  return Object.freeze({
    label,
    value,
    note: note ?? null,
    ...(href ? { href } : {}),
    ...(trend ? { trend } : {}),
    ...(split ? { split } : {}),
    ...(moreLabel ? { moreLabel } : {}),
  });
}

/** Why a figure is a dash: the window holds nothing, or the read carried no cell with the figure. */
const NOTHING_RECORDED = 'Nothing recorded in this window.';
const NO_FIGURE = 'No figure came back for this window.';
const absentNote = (state) => (state && !state.isRefusal && state.data.length === 0 ? NOTHING_RECORDED : NO_FIGURE);

/** A tile from a summed measure, saying why when no returned cell carries it. */
export function measureTile(label, state, measure, formatter = formatCount, options = {}) {
  const total = sumMeasure(state, measure);
  if (total.kind === 'number') return tile(label, { kind: 'number', text: formatter(total.value) }, null, options);
  return tile(label, { kind: 'absent', text: '—' }, absentNote(state), options);
}

function column(key, label, kind = 'text') {
  return Object.freeze({ key, label, kind });
}

/** A table straight from a list or aggregate page, with the closed-vocabulary columns kept apart. */
export function tableFrom(state, { title, columns, emptyText }) {
  return Object.freeze({
    title,
    columns: Object.freeze(columns),
    rows: Object.freeze(state.data.map((row) => Object.freeze({ row, vocab: vocabOf(row) }))),
    emptyText: emptyText ?? 'No rows.',
  });
}

/**
 * A series for a chart. Only a precomputed aggregate can produce one: a chart computed by scanning
 * event rows would be approximate, so a screen whose source is an event list gets no series at all.
 */
export function seriesFrom(state, { measure, dim = 'tool', title }) {
  const source = state.meta?.source ?? '';
  if (!source.startsWith('mart.')) {
    return Object.freeze({ title, unavailable: true, reason: `${source || 'this source'} is not a precomputed aggregate, so it cannot back a chart.`, points: [] });
  }
  const byBucket = new Map();
  for (const row of state.data) {
    const key = row.bucket ?? 'total';
    if (!byBucket.has(key)) byBucket.set(key, []);
    byBucket.get(key).push(row);
  }
  const points = [...byBucket.entries()].map(([bucket, rows]) => {
    let value = 0;
    for (const row of rows) {
      const v = measureOf(row, measure);
      if (v.kind === 'number') value += v.value;
    }
    return Object.freeze({
      bucket,
      value: { kind: 'number', value },
      series: rows.length,
    });
  }).sort((a, b) => String(a.bucket).localeCompare(String(b.bucket)));
  return Object.freeze({ title, points, dim, measure, unavailable: false, reason: null });
}

/** The cells of one dimension, ranked by a measure — a cell that does not carry it ranks last. */
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

// ---------------------------------------------------------------------------------------------
// One figure per tool or team: the window's cells folded, then ranked or shared
// ---------------------------------------------------------------------------------------------

const ADDITIVE = Object.freeze(['submissions', 'bytes_total', 'blocked', 'warned', 'logged']);
const DISTINCT = Object.freeze(['users']);

/**
 * The cells of one dimension folded to one figure per value over every bucket in the window. An
 * additive measure is summed; a distinct count is kept at its largest cell, which is a floor for
 * the window rather than a total; a measure no cell carries stays absent. The submissions per
 * bucket are kept as the value's trend.
 */
export function foldCells(state, key, { sum = ADDITIVE, max = DISTINCT } = {}) {
  const groups = new Map();
  for (const row of state.data) {
    const id = row?.[key];
    if (id === null || id === undefined || id === '') continue;
    const k = String(id);
    if (!groups.has(k)) groups.set(k, { key: k, first: row, rows: [], measures: {}, trend: new Map() });
    const group = groups.get(k);
    group.rows.push(row);
    for (const measure of sum) {
      const value = measureOf(row, measure);
      if (value.kind === 'number') group.measures[measure] = (group.measures[measure] ?? 0) + value.value;
    }
    for (const measure of max) {
      const value = measureOf(row, measure);
      if (value.kind === 'number') group.measures[measure] = Math.max(group.measures[measure] ?? 0, value.value);
    }
    const submissions = measureOf(row, 'submissions');
    if (submissions.kind === 'number') {
      const bucket = row.bucket ?? 'total';
      group.trend.set(bucket, (group.trend.get(bucket) ?? 0) + submissions.value);
    }
  }
  return [...groups.values()].map((group) => Object.freeze({
    key: group.key,
    first: group.first,
    rows: Object.freeze(group.rows),
    measures: Object.freeze(group.measures),
    trend: Object.freeze([...group.trend.entries()]
      .sort((a, b) => String(a[0]).localeCompare(String(b[0])))
      .map(([bucket, value]) => Object.freeze({ bucket, value }))),
  }));
}

/** A folded measure as a value: absent when no cell of the group carried it. */
export function foldedMeasure(group, measure) {
  return Object.prototype.hasOwnProperty.call(group.measures, measure)
    ? { kind: 'number', value: group.measures[measure] }
    : { kind: 'absent' };
}

/** Largest first; a group that does not carry the measure ranks last; ties by key, so the order is stable. */
function byMeasure(measure) {
  const number = (group) => {
    const value = foldedMeasure(group, measure);
    return value.kind === 'number' ? value.value : -1;
  };
  return (a, b) => number(b) - number(a) || a.key.localeCompare(b.key);
}

function totalOf(groups, measure) {
  return groups.reduce((sum, group) => sum + (foldedMeasure(group, measure).value ?? 0), 0);
}

const shareOf = (value, total) => (value.kind === 'number' && total > 0 ? value.value / total : null);
const bytesValue = (value) => (value.kind === 'number' ? { kind: 'number', value: value.value, text: formatBytes(value.value) } : value);

/** The per-bucket shape of a measure, for a sparkline; nothing for a source that cannot back a chart. */
export function trendOf(state, measure = 'submissions') {
  const series = seriesFrom(state, { measure, title: '' });
  if (series.unavailable || series.points.length < 2) return null;
  return Object.freeze(series.points.map((p) => Object.freeze({ bucket: p.bucket, value: p.value.value })));
}

/**
 * The most people one cell counted. It is a floor for the distinct people in the window; the sum
 * of the cells is not, because a person using two tools, or active on two days, is in two cells.
 */
export function peopleTile(state, label = 'People (lower bound)', options = {}) {
  let most = null;
  for (const row of state.data) {
    const value = measureOf(row, 'users');
    if (value.kind === 'number' && (most === null || value.value > most)) most = value.value;
  }
  if (most === null) return tile(label, { kind: 'absent', text: '—' }, absentNote(state), options);
  return tile(label, { kind: 'number', text: formatCount(most) }, 'At least this many: the most counted in any one period of the window.', options);
}

/** A tile from one group's folded measure. */
function foldedTile(label, group, measure, formatter = formatCount, options = {}) {
  const value = group ? foldedMeasure(group, measure) : { kind: 'absent' };
  if (value.kind === 'number') return tile(label, { kind: 'number', text: formatter(value.value) }, null, options);
  return tile(label, { kind: 'absent', text: '—' }, group ? NO_FIGURE : NOTHING_RECORDED, options);
}

/** The fact beside a ranked item that counts people: its noun agrees with the count. */
const peopleFact = (value) => Object.freeze({ label: value.kind === 'number' && value.value === 1 ? 'person' : 'people', value });

const SANCTION_TEXT = Object.freeze({ sanctioned: 'Sanctioned', unsanctioned: 'Unsanctioned', unknown: 'No decision yet' });
const sanctionChip = (state) => (state ? Object.freeze({ key: String(state), text: SANCTION_TEXT[state] ?? String(state) }) : null);

/** The name the catalogue gives a tool's fingerprint; the fingerprint when it gives none. */
const toolName = (row) => (row.tool_name ? String(row.tool_name) : String(row.tool));

/** An address with the window carried along, so a drill-down keeps the window it was read in. */
export function withPreset(hash, preset) {
  if (!preset || preset === 'd7') return hash;
  return `${hash}${hash.includes('?') ? '&' : '?'}preset=${encodeURIComponent(preset)}`;
}
export const toolHref = (fingerprint, preset = null) => withPreset(`#tools?tool=${encodeURIComponent(fingerprint)}`, preset);
export const teamHref = (team, preset = null) => withPreset(`#teams?team=${encodeURIComponent(team)}`, preset);

/** The key the folded unrecognised tools rank under; no fingerprint has this shape. */
const UNRECOGNISED_KEY = 'unrecognised';

/** Several groups of one dimension folded into one, as if they were one value of it. */
function mergeGroups(groups, key, first) {
  const measures = {};
  const trend = new Map();
  for (const group of groups) {
    for (const [measure, value] of Object.entries(group.measures)) {
      measures[measure] = DISTINCT.includes(measure) ? Math.max(measures[measure] ?? 0, value) : (measures[measure] ?? 0) + value;
    }
    for (const point of group.trend) trend.set(point.bucket, (trend.get(point.bucket) ?? 0) + point.value);
  }
  return Object.freeze({
    key,
    first,
    rows: Object.freeze(groups.flatMap((group) => group.rows)),
    measures: Object.freeze(measures),
    count: groups.length,
    trend: Object.freeze([...trend.entries()].sort((a, b) => String(a[0]).localeCompare(String(b[0]))).map(([bucket, value]) => Object.freeze({ bucket, value }))),
  });
}

const isUnrecognised = (group) => toolName(group.first) === UNRECOGNISED_TOOL;

/**
 * The tools in use, by submissions over the window, each a link to its own screen. Rank is a
 * volume and never a sanction signal: the sanction is its own chip, in its own colour. Two or
 * more fingerprints the catalogue does not name are one line: each is a tool only by its hash, and
 * a run of identical "Unrecognised tool" lines says nothing a reader can act on; the fingerprints
 * are listed on their own under the Tools ranking.
 */
export function rankTools(state, { title = 'Tools in use', href = null, moreLabel = 'All tools', limit = null, link = toolHref, emptyText = 'No tool was in use in this window.' } = {}) {
  const groups = foldCells(state, 'tool');
  const unnamed = groups.filter(isUnrecognised);
  const listed = (unnamed.length >= 2
    ? [...groups.filter((group) => !isUnrecognised(group)), mergeGroups(unnamed, UNRECOGNISED_KEY, { tool_name: 'Unrecognised tools', sanctioned_state: 'unknown' })]
    : groups).sort(byMeasure('submissions'));
  const total = totalOf(listed, 'submissions');
  const items = (limit ? listed.slice(0, limit) : listed).map((group) => {
    const folded = group.key === UNRECOGNISED_KEY;
    const name = folded ? 'Unrecognised tools' : toolName(group.first);
    const submissions = foldedMeasure(group, 'submissions');
    return Object.freeze({
      key: group.key,
      href: folded || !link ? null : link(group.key),
      label: name,
      mono: name === group.key,
      title: folded ? 'Tools the shared catalogue does not name, together' : `Tool fingerprint ${group.key}`,
      sublabel: folded ? `${plural(group.count, 'fingerprint', 'fingerprints')} the catalogue does not name` : name === UNRECOGNISED_TOOL ? group.key : null,
      subMono: !folded,
      chip: sanctionChip(group.first.sanctioned_state ?? 'unknown'),
      value: submissions,
      share: shareOf(submissions, total),
      meta: Object.freeze([
        peopleFact(foldedMeasure(group, 'users')),
        Object.freeze({ label: 'blocked', value: foldedMeasure(group, 'blocked') }),
        Object.freeze({ label: 'sent', value: bytesValue(foldedMeasure(group, 'bytes_total')) }),
      ]),
      trend: group.trend,
    });
  });
  return Object.freeze({
    kind: 'ranking',
    title,
    href,
    moreLabel,
    emptyText,
    itemLabel: 'Tool',
    valueLabel: 'Submissions',
    metaLabels: Object.freeze(['People', 'Blocked', 'Data sent']),
    trendLabel: 'Trend',
    items: Object.freeze(items),
  });
}

/**
 * The fingerprints the catalogue does not name, one line each with its figures, so a tool the
 * ranking folds is still reachable. Nothing when there are fewer than two, since the ranking
 * then lists the one by itself.
 */
export function unrecognisedTools(state, { link = toolHref } = {}) {
  const groups = foldCells(state, 'tool').filter(isUnrecognised).sort(byMeasure('submissions'));
  if (groups.length < 2) return null;
  const rows = groups.map((group) => Object.freeze({
    fingerprint_cell: Object.freeze({ primary: group.key, mono: true, title: `Tool fingerprint ${group.key}` }),
    submissions: foldedMeasure(group, 'submissions').value ?? null,
    users: foldedMeasure(group, 'users').value ?? null,
    bytes_total: foldedMeasure(group, 'bytes_total').value ?? null,
    last_active: group.trend.length > 0 ? group.trend[group.trend.length - 1].bucket : null,
    href: link(group.key),
  }));
  return Object.freeze({
    ...tableFrom({ data: rows }, {
      title: 'Unrecognised tools',
      columns: [
        column('fingerprint_cell', 'Fingerprint', 'stack'),
        column('submissions', 'Submissions', 'measure'),
        column('users', 'People', 'measure'),
        column('bytes_total', 'Data sent', 'measure-bytes'),
        column('last_active', 'Last active', 'instant'),
      ],
      emptyText: 'Every tool in this window is named.',
    }),
    rowHref: 'href',
    breakdowns: false,
  });
}

const SANCTION_PARTS = Object.freeze([
  ['sanctioned', 'Sanctioned tools', 'sanctioned tools'],
  ['unsanctioned', 'Unsanctioned tools', 'unsanctioned tools'],
  ['unknown', 'No decision yet', 'tools with no decision yet'],
]);

/**
 * Where the window's submissions went, by the present-tense sanction of the tool they went to.
 * When no tool has a decision, the block says so and opens where the decision is made.
 */
export function sanctionShare(state, { title = 'Where submissions went', emptyText = 'No submissions in this window.', settingsHref = '#settings' } = {}) {
  const counts = new Map(SANCTION_PARTS.map(([key]) => [key, 0]));
  let total = 0;
  for (const row of state.data) {
    const value = measureOf(row, 'submissions');
    if (value.kind !== 'number') continue;
    const key = counts.has(row.sanctioned_state) ? row.sanctioned_state : 'unknown';
    counts.set(key, counts.get(key) + value.value);
    total += value.value;
  }
  const undecided = total > 0 && counts.get('unknown') === total;
  return Object.freeze({
    kind: 'share',
    title,
    total,
    emptyText,
    ...(undecided ? { href: settingsHref, moreLabel: 'Decide which tools are sanctioned' } : {}),
    parts: Object.freeze(SANCTION_PARTS.map(([key, label, phrase]) => Object.freeze({ key, label, phrase, count: counts.get(key), share: total > 0 ? counts.get(key) / total : 0 }))),
    note: undecided ? 'No tool has a sanction decision yet.' : 'A tool with no decision is neither sanctioned nor unsanctioned.',
  });
}

const CLASS_TEXT = Object.freeze({
  payment_card: 'Payment card numbers',
  government_id: 'Government IDs',
  credential: 'Credentials',
  customer_pii: 'Customer PII',
  source_code: 'Source code',
  legal_commercial: 'Legal and commercial',
  health: 'Health data',
});

/**
 * The sensitive-data classes seen, by the submissions carrying each. Class rows fan out, so no
 * share of a whole is said: a submission carrying two classes is in both.
 */
export function rankClasses(state, { title = 'Sensitive data by class', href = null, moreLabel = 'Data classes', limit = null, preset = null, emptyText = 'No sensitive data was classified in this window.' } = {}) {
  const groups = foldCells(state, 'class', { sum: ['submissions'], max: ['users', 'max_score'] }).sort(byMeasure('submissions'));
  const items = (limit ? groups.slice(0, limit) : groups).map((group) => Object.freeze({
    key: group.key,
    href: withPreset(`#tools?view=classes&class=${encodeURIComponent(group.key)}`, preset),
    label: CLASS_TEXT[group.key] ?? group.key.replace(/_/g, ' '),
    title: group.key,
    // Severity has its own colours; `high` is a bad severity, though it is a good confidence.
    chip: group.first.severity ? Object.freeze({ key: `severity_${group.first.severity}`, text: String(group.first.severity) }) : null,
    value: foldedMeasure(group, 'submissions'),
    share: null,
    meta: Object.freeze([
      peopleFact(foldedMeasure(group, 'users')),
      Object.freeze({ label: 'max score', value: (() => { const v = foldedMeasure(group, 'max_score'); return v.kind === 'number' ? { kind: 'number', value: v.value, text: formatScore(v.value) } : v; })() }),
    ]),
    trend: group.trend,
  }));
  return Object.freeze({
    kind: 'ranking',
    title,
    href,
    moreLabel,
    emptyText,
    itemLabel: 'Class',
    valueLabel: 'Submissions carrying it',
    metaLabels: Object.freeze(['People', 'Max score']),
    trendLabel: 'Trend',
    items: Object.freeze(items),
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
// Overview — the landing screen
// ---------------------------------------------------------------------------------------------

/** A state that carries nothing, for a panel whose read did not come back. */
const NOTHING = Object.freeze({ data: Object.freeze([]), banners: Object.freeze([]), meta: Object.freeze({}) });

/** An overview panel's empty line: a read that could not answer says so, never "none". */
function emptyLine(state, none) {
  const said = state ? emptyStateFor(state) : null;
  return said && said.kind !== 'empty' ? `${said.title}. ${said.text}` : none;
}

/** A table with a link to the screen that shows the whole of it. */
function linked(table, href) {
  return Object.freeze({ ...table, href });
}

/** The fleet's reporting figure beside its enrolled denominator, split into the devices that report and the ones that do not, each opening its list. */
function reportingTile(cov) {
  const known = (v) => v !== null && v !== undefined;
  const reporting = known(cov?.devices_reporting) ? Number(cov.devices_reporting) : null;
  const enrolled = known(cov?.devices_enrolled) ? Number(cov.devices_enrolled) : null;
  return tile('Devices reporting',
    reporting === null ? { kind: 'absent', text: '—' } : { kind: 'number', text: formatCount(reporting) },
    enrolled === null
      ? 'The enrolled denominator is unavailable: no fleet figure is shown without it.'
      : `of ${formatCount(enrolled)} enrolled devices`,
    {
      href: '#devices',
      moreLabel: 'All devices',
      split: reporting !== null && enrolled !== null ? Object.freeze([
        Object.freeze({ key: 'reporting', label: 'Reporting', count: reporting, href: '#devices?status=reporting' }),
        Object.freeze({ key: 'never_reported', label: 'Not reporting', count: Math.max(0, enrolled - reporting), href: '#devices?status=attention' }),
      ]) : null,
    });
}

/**
 * The landing page: what a customer wants at a glance, each figure opening the screen that holds
 * the rest of it. Usage, data classes and findings each come from their own question; one that did
 * not come back leaves its panel empty. The devices reporting tile keeps the enrolled denominator
 * beside every figure here.
 *
 * @param {object} input
 * @param {object} input.devices    view state for the device list (it carries coverage)
 * @param {object} [input.tools]    view state for tool usage
 * @param {object} [input.classes]  view state for the data-class mix
 * @param {object} [input.findings] view state for findings
 * @param {object} [input.coverage] a coverage block, when the device read did not carry one
 */
export function postureView({ devices, tools = null, classes = null, findings = null, coverage }, { preset = null, exploreHref = 'explore.html' } = {}) {
  const cov = devices?.coverage ?? coverage ?? null;
  const usage = tools ?? NOTHING;
  const open = (findings?.data ?? []).filter((row) => row.review_state === 'open').length;
  const at = (hash) => withPreset(hash, preset);

  // The same warning arrives with each read; it is said once. Coverage is not a banner here: the
  // devices reporting tile carries it, and a panel that cannot answer says so in place.
  const banners = [];
  const said = new Set();
  for (const state of [devices, tools, classes, findings]) {
    for (const banner of state?.banners ?? []) {
      if (banner.about === 'coverage') continue;
      const key = `${banner.level}|${banner.title}|${banner.text}`;
      if (said.has(key)) continue;
      said.add(key);
      banners.push(banner);
    }
  }

  return screen('posture', 'Overview', null, null, {
    subtitle: null,
    tiles: Object.freeze([
      measureTile('Submissions', usage, 'submissions', formatCount, { trend: trendOf(usage), href: at('#tools'), moreLabel: 'Tools' }),
      peopleTile(usage, undefined, { href: '#person', moreLabel: 'Users' }),
      tile('Open findings', findings ? { kind: 'number', text: formatCount(open) } : { kind: 'absent', text: '—' }, findings ? 'In the latest page' : null,
        { href: `${exploreHref}#findings?review_state=open`, moreLabel: 'Review them' }),
      reportingTile(cov),
    ]),
    blocks: Object.freeze([
      sanctionShare(usage, { emptyText: emptyLine(tools, 'No submissions in this window.') }),
      rankTools(usage, { href: at('#tools'), limit: 6, link: (fingerprint) => toolHref(fingerprint, preset), emptyText: emptyLine(tools, 'No tool was in use in this window.') }),
      rankClasses(classes ?? NOTHING, { href: at('#tools?view=classes'), limit: 6, preset, emptyText: emptyLine(classes, 'No sensitive data was classified in this window.') }),
    ]),
    series: Object.freeze([]),
    tables: Object.freeze([
      linked(tableFrom(findings ?? NOTHING, {
        title: 'Findings',
        columns: [
          column('detected_at', 'Detected', 'instant'),
          column('severity', 'Severity', 'vocab'),
          column('rule_title', 'Rule'),
          column('subject', 'User', 'person'),
          column('tool', 'Tool'),
          column('review_state', 'Review', 'vocab'),
        ],
        emptyText: emptyLine(findings, 'No finding was raised in this window.'),
      }), `${exploreHref}#findings`),
    ]),
    banners: Object.freeze(banners),
    notes: shared(devices ?? NOTHING).notes,
  });
}

// ---------------------------------------------------------------------------------------------
// The ten questions
// ---------------------------------------------------------------------------------------------

/** The chart's title names the bucket the series was served in. */
function seriesTitle(state, what = 'Submissions') {
  const bucket = state.meta?.applied_bucket ?? state.meta?.requested_bucket ?? null;
  return bucket ? `${what} per ${bucket}` : `${what} per bucket`;
}

/**
 * Every AI tool seen in the window, as one ranked list: a tool is one line with its share, its
 * people, its blocks and its trend, and opens its own screen. The per-bucket cells are folded
 * into the window; the chart below is the window over time.
 */
export function toolsView(state, { link = toolHref, preset = null } = {}) {
  const linkTo = link ? (fingerprint) => link(fingerprint, preset) : null;
  return screen('tools', 'Tools', null, 'mart.v_tool_usage', {
    subtitle: 'Every AI tool seen in this window, by submissions. Open a tool to see who uses it and what was sent.',
    tiles: Object.freeze([
      measureTile('Submissions', state, 'submissions', formatCount, { trend: trendOf(state) }),
      peopleTile(state),
      measureTile('Blocked', state, 'blocked'),
      measureTile('Data sent', state, 'bytes_total', formatBytes),
    ]),
    blocks: Object.freeze([
      sanctionShare(state, { emptyText: emptyLine(state, 'No submissions in this window.') }),
      rankTools(state, { title: 'Tools by submissions', link: linkTo, emptyText: emptyLine(state, 'No tool was in use in this window.') }),
    ]),
    tables: Object.freeze([unrecognisedTools(state, { link: linkTo })].filter(Boolean)),
    // A window with no cell has no shape to draw: the ranking's empty line says why.
    series: Object.freeze(state.data.length > 0 ? [seriesFrom(state, { measure: 'submissions', title: seriesTitle(state) })] : []),
    ...shared(state, [
      'Rank is by submissions with a deterministic tie-break, and is never a sanction signal.',
      'Unknown is its own answer: a tool with no decision renders as unknown, never as unsanctioned.',
      'A tool\'s people is the most one cell counted, a floor for the window: a person active on two days is in two cells.',
    ]),
  });
}

/**
 * One tool: its figures over the window, who is using it, and the findings it produced. The
 * people are a roster in the API's order, with their submissions; a role that may not see people
 * sees the totals and is told why the roster is missing.
 */
export function toolView({ usage, people = null, findings = null, names = {} }, { tool, preset = null, exploreHref = 'explore.html' }) {
  const group = foldCells(usage, 'tool').find((g) => g.key === String(tool)) ?? null;
  const name = group ? toolName(group.first) : String(tool);
  // A tool in use with no decision recorded is unknown, which is a state of its own; a tool with
  // no cell in the window has no state to show.
  const sanction = group?.first.sanctioned_state ?? (group ? 'unknown' : null);
  const roster = people && !people.isRefusal
    ? foldCells(people, 'subject', { sum: ['submissions', 'bytes_total'], max: [] }).map((g) => ({
      subject: g.key,
      name: names[g.key] ?? null,
      submissions: foldedMeasure(g, 'submissions').value ?? null,
      bytes_total: foldedMeasure(g, 'bytes_total').value ?? null,
    }))
    : null;
  const notes = [];
  if (people?.isRefusal) {
    notes.push(people.resultState === 'unauthorised_role'
      ? 'Your role shows the tool\'s totals only; an analyst or admin also sees who is using it.'
      : `Who is using it could not be read: ${people.error?.message ?? people.resultState}.`);
  } else if (roster) {
    notes.push('People are listed in the order the read returned them, never by volume. Reading this list is audited.');
  } else if (group) {
    notes.push('Who is using it was not read for this window.');
  }
  if (findings?.isRefusal) notes.push(`Findings could not be read: ${findings.error?.message ?? findings.resultState}.`);
  return screen('tool', name, null, 'mart.v_tool_usage', {
    subtitle: name === String(tool) ? 'A tool known only by its fingerprint.' : `Fingerprint ${tool}`,
    badges: Object.freeze(sanction ? [sanctionChip(sanction)] : []),
    actions: Object.freeze([
      Object.freeze({ label: 'Prompts and events', href: `${exploreHref}#events?tool=${encodeURIComponent(String(tool))}` }),
      Object.freeze({ label: 'All tools', href: withPreset('#tools', preset) }),
    ]),
    tiles: Object.freeze([
      foldedTile('Submissions', group, 'submissions', formatCount, { trend: group && group.trend.length > 1 ? group.trend : null }),
      foldedTile('People (lower bound)', group, 'users'),
      foldedTile('Blocked', group, 'blocked'),
      foldedTile('Data sent', group, 'bytes_total', formatBytes),
    ]),
    tables: Object.freeze([
      ...(roster ? [tableFrom({ data: roster }, {
        title: 'People using it',
        columns: [
          column('name', 'User', 'person'),
          column('submissions', 'Submissions', 'measure'),
          column('bytes_total', 'Data sent', 'measure-bytes'),
        ],
        emptyText: emptyLine(people, 'Nobody used this tool in this window.'),
      })] : []),
      tableFrom(findings && !findings.isRefusal ? findings : NOTHING, {
        title: 'Findings',
        columns: [
          column('detected_at', 'Detected', 'instant'),
          column('severity', 'Severity', 'vocab'),
          column('rule_title', 'Rule'),
          column('subject', 'User', 'person'),
          column('review_state', 'Review', 'vocab'),
        ],
        emptyText: emptyLine(findings && !findings.isRefusal ? findings : null, 'No finding was raised for this tool in this window.'),
      }),
    ]),
    series: Object.freeze(group ? [seriesFrom(usage, { measure: 'submissions', title: seriesTitle(usage) })] : []),
    ...shared(usage, [
      ...notes,
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
      tile('People in this page', { kind: 'number', text: formatCount(state.data.length) }, 'A page count, not a fleet total.'),
    ]),
    tables: Object.freeze([
      tableFrom(state, {
        title: 'People by tool (ordered by tool, then person — not by volume)',
        columns: [
          column('tool', 'Tool'),
          column('subject', 'User', 'person'),
          column('submissions', 'Submissions', 'measure'),
          column('bytes_total', 'Bytes', 'measure-bytes'),
        ],
        emptyText: 'Nobody used an unsanctioned tool in this window.',
      }),
    ]),
    series: Object.freeze([]),
    ...shared(state, [
      'This table is ordered by tool and then by person: it is a list, not a ranking. There is no view of people sorted by volume.',
    ]),
  });
}

/**
 * The people behind the team totals, by team and then by name: a roster with numbers, not a
 * ranking. Null when there was nothing to read; a refusal says why the people are missing.
 */
function teamMembersTable(members, { byTeam = true } = {}) {
  const byName = (a, b) => String(a ?? '').localeCompare(String(b ?? ''), undefined, { sensitivity: 'base' });
  const rows = [...members.data].sort((a, b) => byName(a.team_name, b.team_name) || byName(a.name, b.name) || byName(a.subject, b.subject));
  return Object.freeze({
    title: 'Submissions by person',
    columns: Object.freeze([
      ...(byTeam ? [column('team_name', 'Team')] : []),
      column('name', 'User', 'person'),
      column('submissions', 'Submissions', 'measure'),
    ]),
    // The team heads its run of people rather than repeating on every line.
    ...(byTeam ? { groupBy: 'team_name' } : {}),
    rows: Object.freeze(rows.map((row) => Object.freeze({ row, vocab: vocabOf(row) }))),
    emptyText: byTeam ? 'Nobody in these teams sent a prompt in this window.' : 'Nobody in this team sent a prompt in this window.',
  });
}

function teamMembersNotes(members) {
  if (!members) return [];
  if (members.isRefusal) {
    return [members.resultState === 'unauthorised_role'
      ? 'Your role shows team totals only; an analyst or admin also sees each person\'s submissions.'
      : `Each person's submissions could not be read: ${members.error?.message ?? members.resultState}.`];
  }
  const notes = ['Each person is listed under every team they are in, ordered by team and name. Reading this list is audited.'];
  if (members.meta?.truncated) notes.push(`Only the first ${formatCount(members.data.length)} people are listed.`);
  if (members.audit?.entry_id) notes.push(`Audited read: entry ${members.audit.entry_id} at ${formatInstant(members.audit.written_at)}.`);
  return notes;
}

/** How a team is made, as the directory page says it: its members, and what it follows. */
function teamFacts(team) {
  if (!team) return null;
  const source = { console: 'Chosen in the console', group: `Group ${team.group_name ?? team.group_object_id ?? ''}`.trim(), department: `Department ${team.match_value ?? ''}`.trim(), org_unit: `Unit ${team.match_value ?? ''}`.trim() }[team.source] ?? null;
  return [team.members === null || team.members === undefined ? null : plural(team.members, 'member', 'members'), source].filter(Boolean).join(' · ') || null;
}

/**
 * The teams, as one ranked list by submissions, each opening its own screen. A team the admin
 * list knows that has no usage is listed at the end with a zero, so a quiet team is seen rather
 * than dropped; under partial coverage that zero is a floor like every other figure on the page,
 * and the coverage line says so. A read that could not say lists nothing.
 */
function rankTeams(state, { teams = null, link = teamHref, emptyText }) {
  const groups = foldCells(state, 'team', { sum: ['submissions'], max: ['users'] }).sort(byMeasure('submissions'));
  const total = totalOf(groups, 'submissions');
  const known = new Map((teams ?? []).map((team) => [team.team_id, team]));
  const items = groups.map((group) => {
    const team = known.get(group.key) ?? null;
    const submissions = foldedMeasure(group, 'submissions');
    return Object.freeze({
      key: group.key,
      href: link(group.key),
      label: String(group.first.team_name ?? team?.name ?? group.key),
      sublabel: teamFacts(team),
      value: submissions,
      share: shareOf(submissions, total),
      meta: Object.freeze([peopleFact(foldedMeasure(group, 'users'))]),
      trend: group.trend,
    });
  });
  const answered = !state.isRefusal && state.resultState !== 'not_yet_covered';
  for (const team of answered ? teams ?? [] : []) {
    if (groups.some((group) => group.key === team.team_id)) continue;
    items.push(Object.freeze({
      key: team.team_id,
      href: link(team.team_id),
      label: team.name,
      sublabel: [teamFacts(team), 'No usage recorded in this window'].filter(Boolean).join(' · '),
      value: { kind: 'number', value: 0 },
      share: total > 0 ? 0 : null,
      meta: Object.freeze([]),
      trend: Object.freeze([]),
    }));
  }
  return Object.freeze({
    kind: 'ranking',
    title: 'Teams by submissions',
    href: null,
    emptyText,
    itemLabel: 'Team',
    valueLabel: 'Submissions',
    metaLabels: Object.freeze(['People']),
    trendLabel: 'Trend',
    items: Object.freeze(items),
  });
}

/**
 * The teams: who uses AI, how it splits across the teams, and the people behind each team's
 * total. Every team is one line that opens its own screen; the people are a roster under their
 * team, by name, never by volume.
 *
 * @param {object} state view state for the team totals (Q3)
 * @param {object} [options]
 * @param {string|null} [options.manageHref] where an admin manages teams
 * @param {object|null} [options.members]    view state for the people behind the teams
 * @param {Array|null}  [options.teams]      the admin's team list, so a quiet team is still listed
 */
export function teamsView(state, { manageHref = null, members = null, teams = null, link = teamHref, preset = null } = {}) {
  const cov = state.meta?.extras?.team_coverage?.[0] ?? null;
  const num = (v) => (v === null || v === undefined ? null : Number(v));
  const all = num(cov?.users_all);
  const inTeams = num(cov?.users_in_teams);
  const teamCount = num(cov?.teams) ?? (teams ? teams.length : null);
  // People in no team are said beside the people in one, so a team view cannot quietly stand for
  // everyone.
  const outside = all === null || inTeams === null ? null : Math.max(0, all - inTeams);
  const notes = [];
  if (cov && all !== null && inTeams !== null) {
    notes.push(`${plural(all, 'person', 'people')} used AI in this window; ${inTeams === 1 ? '1 is' : `${formatCount(inTeams)} are`} in at least one team and ${outside === 1 ? '1 is' : `${formatCount(outside)} are`} in none.`);
  }
  if (teamCount === 0) {
    notes.push(manageHref
      ? 'No team exists yet. Create one from the console, a directory group, a department or an organisational unit under Settings → Directory.'
      : 'No team exists yet. An admin creates teams under Settings → Directory.');
  }
  const absent = () => ({ kind: 'absent', text: '—' });
  return screen('teams', 'Teams', null, 'mart.agg_team_period', {
    subtitle: 'Usage per team. A team counts the usage of the people in it now; a person in two teams counts in both.',
    tiles: Object.freeze([
      measureTile('Submissions', state, 'submissions', formatCount, { trend: trendOf(state) }),
      tile('People using AI', all === null ? absent() : { kind: 'number', text: formatCount(all) },
        all === null ? 'No team coverage read came back with this page.' : null,
        {
          split: all === null ? null : Object.freeze([
            Object.freeze({ key: 'in_team', label: 'In a team', count: inTeams }),
            Object.freeze({ key: 'no_team', label: 'In no team', count: outside }),
          ]),
        }),
      tile('Teams', teamCount === null ? absent() : { kind: 'number', text: formatCount(teamCount) }, manageHref ? 'Manage teams' : null, manageHref ? { href: manageHref } : undefined),
    ]),
    blocks: Object.freeze([
      rankTeams(state, { teams, link: (team) => link(team, preset), emptyText: teamCount === 0 ? 'There are no teams yet.' : emptyLine(state, 'No team has usage in this window.') }),
    ]),
    tables: Object.freeze(members && !members.isRefusal ? [teamMembersTable(members)] : []),
    series: Object.freeze(state.data.length > 0 ? [seriesFrom(state, { measure: 'submissions', title: seriesTitle(state) })] : []),
    ...shared(state, [...notes, ...teamMembersNotes(members)]),
  });
}

/**
 * One team: its figures over the window and the people behind them, by name. The team is named
 * by the totals read, or by the roster when the totals carry no row for it.
 */
export function teamView(state, { team, members = null, teams = null, manageHref = null, preset = null }) {
  const group = foldCells(state, 'team', { sum: ['submissions'], max: ['users'] }).find((g) => g.key === String(team)) ?? null;
  const known = (teams ?? []).find((t) => t.team_id === String(team)) ?? null;
  const name = String(group?.first.team_name ?? known?.name ?? members?.data?.find((row) => row.team_name)?.team_name ?? 'Team');
  const listed = members && !members.isRefusal ? members.data.length : null;
  const facts = teamFacts(known);
  return screen('team', name, null, 'mart.agg_team_period', {
    subtitle: facts ? `${facts}. A team counts the usage of the people in it now.` : 'One team\'s usage. A team counts the usage of the people in it now.',
    actions: Object.freeze([
      Object.freeze({ label: 'All teams', href: withPreset('#teams', preset) }),
      ...(manageHref ? [Object.freeze({ label: 'Manage teams', href: manageHref })] : []),
    ]),
    tiles: Object.freeze([
      foldedTile('Submissions', group, 'submissions', formatCount, { trend: group && group.trend.length > 1 ? group.trend : null }),
      foldedTile('People (lower bound)', group, 'users'),
      tile('People listed', listed === null ? { kind: 'absent', text: '—' } : { kind: 'number', text: formatCount(listed) },
        listed === null ? 'The roster could not be read.' : 'Members who sent a prompt in this window.'),
    ]),
    tables: Object.freeze(members && !members.isRefusal ? [teamMembersTable(members, { byTeam: false })] : []),
    series: Object.freeze(group ? [seriesFrom(state, { measure: 'submissions', title: seriesTitle(state) })] : []),
    ...shared(state, teamMembersNotes(members)),
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
  return screen('classes', 'Data classes', TEMPLATES.q4_class_mix.title, 'mart.agg_class_period', {
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

export function personView(state, { subject, person = null, exploreHref = 'explore.html' }) {
  const series = seriesFrom(state, { measure: 'submissions', title: 'Submissions per day' });
  const flush = state.meta?.extras?.flush_check?.[0] ?? null;
  const notes = [];
  if (flush) {
    notes.push(`${formatCount(flush.rows_in_window)} rows for this person in the window, ${formatCount(flush.late_flush_rows)} received more than an hour after they occurred: a spool flush spikes received time, not behaviour.`);
  }
  const name = person?.name && person.name !== subject ? person.name : null;
  const facts = [person?.department, person?.org_unit].filter(Boolean).join(' · ');
  return screen('person', name ?? 'Users', null, 'mart.agg_user_period', {
    subtitle: name ? `${facts ? `${facts} · ` : ''}${subject}` : null,
    search: Object.freeze({ value: name ? '' : subject }),
    actions: Object.freeze([
      Object.freeze({ label: 'Prompts and events', href: `${exploreHref}#events?subject=${encodeURIComponent(subject)}` }),
      Object.freeze({ label: 'All people', href: '#person' }),
    ]),
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

/**
 * The people the directory and the devices know, by name, to open one person's page. It has no
 * usage column to sort by: it is a directory, not a ranking.
 */
export function peopleView(state, { search = '' } = {}) {
  const next = state.page?.next_cursor ?? null;
  const query = (extra) => new URLSearchParams({ ...(search ? { q: search } : {}), ...extra }).toString();
  const named = state.data.filter((row) => row.directory_name || row.subject_name).length;
  const table = tableFrom(state, {
    title: search ? `People whose name starts with "${search}"` : 'People',
    columns: [
      column('name', 'Name', 'person'),
      column('department', 'Department'),
      column('org_unit', 'Organisational unit'),
      column('directory_status', 'Directory', 'vocab'),
      column('last_active_day', 'Last active', 'instant'),
    ],
    emptyText: search ? 'Nobody\'s name starts with that.' : 'Nobody is known yet: people arrive from the directory, or with their first prompt.',
  });
  return screen('person', 'Users', null, 'mart.v_person', {
    subtitle: 'Everyone the directory or a device has named. Open a person to see their usage and prompts.',
    search: Object.freeze({ value: search }),
    tiles: Object.freeze([]),
    tables: Object.freeze([Object.freeze({
      ...table,
      breakdowns: false,
      ...(next ? { href: `#person?${query({ cursor: next })}`, moreLabel: 'Next page' } : {}),
    })]),
    series: Object.freeze([]),
    ...shared(state, state.data.length > 0 && named === 0
      ? ['No name is known for anyone on this page: the tenant keeps people hashed, or no directory has been read yet. Each is listed by reference.']
      : []),
    // A list of people is current state, not a measurement: fleet coverage and aggregate age say
    // nothing about it, so their banners are kept for a page that came back without rows.
    ...(state.data.length > 0 ? { banners: Object.freeze([]) } : {}),
  });
}

const count = (v) => (typeof v === 'number' && Number.isFinite(v) ? v : Number(v) || 0);

/**
 * One device's state in the words a customer uses, from its liveness and the worst state among
 * its collectors. The key keeps the vocabulary value's colour.
 */
function deviceStatus(row) {
  if (row.liveness === 'revoked') return { key: 'revoked', text: 'Revoked' };
  if (count(row.collectors_tampered) > 0) return { key: 'tampered', text: 'Tampered' };
  if (row.liveness === 'never_reported') return { key: 'never_reported', text: 'Never checked in' };
  if (row.liveness === 'stale') return { key: 'stale', text: row.last_seen_at ? `Quiet since ${formatInstant(row.last_seen_at).slice(0, 10)}` : 'Quiet' };
  if (row.liveness === 'reporting') return count(row.collectors_degraded) > 0 ? { key: 'degraded', text: 'Degraded' } : { key: 'reporting', text: 'Reporting' };
  return { key: 'unknown', text: 'Unknown' };
}

/** The device's collectors by state, worst first, as "1 tampered · 12 healthy"; null when none reported. */
function collectorsSummary(row) {
  const parts = [['tampered', row.collectors_tampered], ['degraded', row.collectors_degraded], ['absent', row.collectors_absent], ['healthy', row.collectors_healthy]]
    .filter(([, n]) => count(n) > 0)
    .map(([state, n]) => `${formatCount(count(n))} ${state}`);
  return parts.length ? parts.join(' · ') : null;
}

/** How long ago an instant was, in the coarsest unit that is still true. */
function ago(iso, now) {
  const ms = now.getTime() - Date.parse(iso);
  if (!Number.isFinite(ms) || ms < 0) return formatInstant(iso);
  const minutes = Math.floor(ms / 60_000);
  if (minutes < 1) return 'Just now';
  if (minutes < 60) return `${minutes} min ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return `${hours} h ago`;
  return `${Math.floor(hours / 24)} days ago`;
}

const DEVICE_LABELS = Object.freeze({ windows: 'Windows', macos: 'macOS', linux: 'Linux', managed: 'Managed', unmanaged: 'Unmanaged', unknown: 'Unknown' });

/** A device state in the words of the status filter and the attention split. */
const STATUS_LABELS = Object.freeze({ attention: 'Needs attention', reporting: 'Reporting', stale: 'Quiet', never_reported: 'Never checked in', degraded: 'Degraded', tampered: 'Tampered', revoked: 'Revoked' });
const ATTENTION_KEYS = Object.freeze(['stale', 'never_reported', 'degraded', 'tampered']);

/** The words beside a device's name: what it runs, whether it is managed, its agent and its mode. */
function deviceFacts(row) {
  const os = [DEVICE_LABELS[row.device_os] ?? (row.device_os ? String(row.device_os) : null), row.os_version ? String(row.os_version) : null].filter(Boolean).join(' ');
  return [
    os || null,
    DEVICE_LABELS[row.managed_state] ?? (row.managed_state ? String(row.managed_state) : null),
    row.agent_version ? `Agent ${row.agent_version}` : null,
    row.collection_mode ? COLLECTION_MODE_LABELS[row.collection_mode] ?? `Mode ${row.collection_mode}` : null,
  ].filter(Boolean).join(' · ') || null;
}

/**
 * Devices, as a customer reads them: how much of the fleet is reporting, which devices need
 * attention and why, and one row per device in three cells: the device and what it runs, the
 * person on it, and its state with when it was last seen. A row opens the device's activity.
 *
 * @param {object} state view state for the device list
 * @param {object} [options]
 * @param {object} [options.filters]    status (attention | reporting | one state), device_os, managed_state
 * @param {Date}   [options.now]        for "last seen"
 * @param {string} [options.exploreHref] the search page, which lists one device's events
 */
export function devicesView(state, { filters = {}, now = new Date(), exploreHref = 'explore.html' } = {}) {
  const all = state.data.map((row) => {
    const status = deviceStatus(row);
    const lastSeen = row.last_seen_at ? ago(row.last_seen_at, now) : null;
    const collectors = collectorsSummary(row);
    // The device name is the hostname, with the UUID kept for the hover and as the fallback. The
    // user is the clear account name of the most recent submission, with the pseudonymous ref as
    // the fallback. directory_name is the directory's own current name, shown above it when a
    // sync has supplied one; both are absent for a 'hashed' tenant.
    const user = row.subject_name ?? row.user_ref ?? null;
    const directoryName = row.directory_name ?? null;
    return {
      ...row,
      status: status.key,
      status_text: status.text,
      last_seen_at_ago: lastSeen,
      device_name: row.hostname || null,
      user,
      directory_name: directoryName,
      mode: row.collection_mode ?? null,
      collectors,
      activity: row.device ? `${exploreHref}#events?device=${encodeURIComponent(String(row.device))}` : null,
      device_cell: Object.freeze({
        primary: row.hostname || (row.device ? `${String(row.device).slice(0, 8)}…` : null),
        mono: !row.hostname,
        title: row.device ? String(row.device) : null,
        secondary: deviceFacts(row),
      }),
      user_cell: Object.freeze({
        primary: directoryName ?? user,
        secondary: directoryName && user && directoryName !== user ? user : null,
        title: row.user_ref ? String(row.user_ref) : null,
      }),
      status_cell: Object.freeze({
        chip: Object.freeze({ key: status.key, text: status.text }),
        secondary: [lastSeen ? `Last seen ${lastSeen}` : null, collectors ? `Collectors ${collectors}` : null].filter(Boolean).join(' · ') || null,
      }),
    };
  });
  const needsAttention = all.filter((row) => row.status !== 'reporting');

  const specific = ATTENTION_KEYS.includes(filters.status) || filters.status === 'revoked' ? filters.status : '';
  const chosen = {
    status: filters.status === 'attention' || filters.status === 'reporting' ? filters.status : specific,
    device_os: filters.device_os ?? '',
    managed_state: filters.managed_state ?? '',
  };
  const byStatus = (row) => (chosen.status === '' ? true : chosen.status === 'attention' ? row.status !== 'reporting' : row.status === chosen.status);
  const rows = all.filter((row) => (
    byStatus(row)
    && (chosen.device_os === '' || (row.device_os ?? 'unknown') === chosen.device_os)
    && (chosen.managed_state === '' || (row.managed_state ?? 'unknown') === chosen.managed_state)
  ));

  // Each filter is a row of links, so a filtered list has an address.
  const href = (change) => {
    const query = new URLSearchParams(Object.entries({ ...chosen, ...change }).filter(([, value]) => value !== '')).toString();
    return `#devices${query ? `?${query}` : ''}`;
  };
  const filter = (label, key, values) => Object.freeze({
    label,
    current: chosen[key],
    items: Object.freeze([{ id: '', label: 'All', href: href({ [key]: '' }) },
      ...values.map(([id, text]) => ({ id, label: text, href: href({ [key]: id }) }))]),
  });
  const present = (key) => [...new Set([...all.map((row) => row[key] ?? 'unknown'), ...(chosen[key] ? [chosen[key]] : [])])]
    .sort().map((value) => [value, DEVICE_LABELS[value] ?? value]);

  // The fleet figures come from the server when it sent them. `device_status` is the fleet-wide
  // count by status, so "Need attention" and the fleet card describe the same population; without
  // it, only the listed page is counted and the card says so.
  const cov = state.coverage ?? null;
  const fleet = typeof cov?.devices_enrolled === 'number' && typeof cov?.devices_reporting === 'number';
  const status = state.meta?.extras?.device_status?.[0] ?? null;
  const num = (v) => (typeof v === 'number' ? v : Number(v));
  const hasStatus = status && Number.isFinite(num(status.devices_enrolled));
  const total = hasStatus ? num(status.devices_enrolled) : (fleet ? cov.devices_enrolled : all.length);
  const attention = hasStatus
    ? num(status.stale) + num(status.never_reported) + num(status.degraded) + num(status.tampered)
    : needsAttention.length;
  const reporting = hasStatus
    ? total - attention
    : (fleet ? cov.devices_reporting : all.length - needsAttention.length);
  // Why devices need attention, each reason opening the devices it counts.
  const reasons = (hasStatus ? ATTENTION_KEYS : [...ATTENTION_KEYS, 'revoked'])
    .map((key) => Object.freeze({ key, label: STATUS_LABELS[key], count: hasStatus ? num(status[key]) || 0 : needsAttention.filter((row) => row.status === key).length, href: href({ status: key }) }))
    .filter((part) => part.count > 0);

  return screen('devices', 'Devices', null, 'mart.v_device_liveness', {
    subtitle: null,
    tiles: Object.freeze([
      Object.freeze({
        label: hasStatus || fleet ? 'Devices enrolled' : 'Devices listed',
        value: { kind: 'number', text: formatCount(total) },
        note: null,
        split: Object.freeze([
          Object.freeze({ key: 'reporting', label: 'Reporting', count: reporting, href: href({ status: 'reporting' }) }),
          Object.freeze({ key: 'never_reported', label: 'Not reporting', count: Math.max(0, total - reporting), href: href({ status: 'attention' }) }),
        ]),
      }),
      Object.freeze({
        label: 'Need attention',
        value: { kind: 'number', text: formatCount(attention) },
        note: attention > 0 ? null : 'Every enrolled device is reporting',
        href: attention > 0 ? href({ status: 'attention' }) : null,
        moreLabel: 'Show them',
        split: reasons.length > 0 ? Object.freeze(reasons) : null,
      }),
    ]),
    // A filter is offered when it can change the list: a status filter while some device needs
    // attention, an OS or management filter while the fleet is mixed. One already chosen stays,
    // so it can be cleared.
    filters: Object.freeze([
      ...(attention > 0 || chosen.status ? [filter('Status', 'status', [['attention', 'Needs attention'], ['reporting', 'Reporting'], ...(specific ? [[specific, STATUS_LABELS[specific]]] : [])])] : []),
      ...[['OS', 'device_os'], ['Management', 'managed_state']].map(([label, key]) => [label, key, present(key)])
        .filter(([, key, values]) => values.length > 1 || chosen[key])
        .map(([label, key, values]) => filter(label, key, values)),
    ]),
    tables: Object.freeze([
      Object.freeze({
        ...tableFrom({ data: rows }, {
          title: 'Devices',
          columns: [
            column('device_cell', 'Device', 'stack'),
            column('status_cell', 'Status', 'stack'),
            column('user_cell', 'User', 'stack'),
            Object.freeze({ key: 'activity', label: '', kind: 'link', linkLabel: 'View activity' }),
          ],
          emptyText: 'No device matches these filters.',
        }),
        rowHref: 'activity',
        breakdowns: false,
      }),
    ]),
    series: Object.freeze([]),
    ...shared(state, [
      'Reporting, quiet, never checked in and revoked are four different facts, and none is inferred from silence.',
      'One row per device. Its status is the worst of its collectors\' states; the collectors themselves are listed from the device\'s activity.',
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
 * The columns of a record read that describe one observation rather than the submission. The read
 * answers with one row per observation, each repeating the submission's own columns.
 */
const OBSERVATION_COLUMNS = Object.freeze([
  'observation_event_id', 'observation_source', 'observation_kind', 'direction', 'observation_occurred_at',
  'observation_received_at', 'observation_size_bytes', 'observation_labels', 'policy_decision',
  'detection_basis', 'window_start', 'window_end', 'submission_count', 'bytes_total',
]);

/** The observation rows of a record read. A submission with none comes back as one row with them null. */
export function eventObservations(rows) {
  return (rows ?? []).filter((row) => typeof row?.observation_event_id === 'string' && row.observation_event_id !== '');
}

/**
 * Event detail (Q9). What is shown depends on content_state, and each of the four is a different
 * answer — never a blank and never content.
 */
export function eventView(state) {
  const head = state.data[0] ?? null;
  const observations = eventObservations(state.data);
  const contentState = head?.content_state ?? state.meta?.content_state ?? 'not_captured';
  const contentAnswer = {
    not_captured: 'Content was never read at this mode. Labels, digest, size and the policy action are all there is — and there is no index entry either.',
    local_only: 'Content was taken and remains on the device. It was never uploaded, so it cannot be retrieved.',
    uploaded: 'Content is stored. Open the event in Search to read the prompt; this screen shows metadata only.',
    shredded: 'The content existed and has been destroyed. The reason and the receipt are the answer.',
  }[contentState] ?? 'Unknown content state.';
  return screen('event', 'Event detail', TEMPLATES.q9_event_detail.title, 'ingest.submission', {
    subtitle: 'One event, its observation routes, and what the content state permits. No query returns content.',
    tiles: Object.freeze([
      tile('Content state', { kind: 'vocab', text: contentState }, contentAnswer),
      tile('Routes', { kind: 'number', text: formatCount(head?.observation_count ?? observations.length) }, 'One observation per route, so an overlapping count can be explained.'),
      tile('Mode', { kind: 'vocab', text: head?.collection_mode ?? '—' }, null),
      tile('Merge confidence', { kind: 'vocab', text: head?.merge_confidence ?? '—' }, head?.merge_confidence === 'low' ? 'A weak dedup key produced this row: counted, never discarded.' : null),
    ]),
    tables: Object.freeze([
      Object.freeze({
        title: 'Metadata',
        columns: Object.freeze([column('field', 'Field'), column('value', 'Value')]),
        rows: Object.freeze(head ? Object.entries(head)
          .filter(([key]) => !OBSERVATION_COLUMNS.includes(key))
          .map(([field, value]) => Object.freeze({ row: { field, value: renderable(value) }, vocab: {} })) : []),
        emptyText: 'The record is not here. See the state above for why.',
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
      'The prompt itself is read in Search, through the content vault\'s approved retrieval; no query returns content.',
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
export function needsInputView({ id, title, question, hint, search = null }) {
  return Object.freeze({
    id,
    title,
    question,
    source: null,
    sourceLabel: null,
    subtitle: search ? null : hint,
    search,
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
        rows: Object.freeze(rows.map((row) => Object.freeze({ row, vocab: {} }))),
        emptyText: 'No detail.',
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
