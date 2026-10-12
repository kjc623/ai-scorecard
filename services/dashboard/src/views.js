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
import { formatBytes, formatCount, formatInstant, formatLabels, formatPercent, plural } from './format.js';
import { SOURCES, TEMPLATES, UNRECOGNISED_TOOL, SITE_PREFIX, COLLECTION_MODE_LABELS, COLLECTOR_DETAILS, WINDOWS } from './vocab.js';
import { EXPLORE_DATASETS, encodeExploreHash } from './explore-model.js';
import { RULE_ACTION_LABELS, RULE_CATEGORIES, RULE_MATCH_FIELDS, RULE_MATCH_TITLES, RULE_ROUTES } from './rules.js';
import { ENDPOINT_TOOLS } from './settings.js';

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

const ADDITIVE = Object.freeze([
  'submissions', 'bytes_total', 'blocked', 'warned', 'logged',
  'sensitive', 'sensitive_blocked', 'sensitive_warned', 'sensitive_logged', 'attachments', 'content_prompts',
]);
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
export function peopleTile(state, label, options = {}) {
  let most = null;
  for (const row of state.data) {
    const value = measureOf(row, 'users');
    if (value.kind === 'number' && (most === null || value.value > most)) most = value.value;
  }
  if (most === null) return tile(label, { kind: 'absent', text: '—' }, absentNote(state), options);
  return tile(label, { kind: 'number', text: formatCount(most) }, 'At least this many: the most counted in any one period of the window.', options);
}

/** Who a count of people is out of, in words. */
const PEOPLE_BASIS = Object.freeze({
  directory: 'active people in the directory',
  devices: 'people signed in on an enrolled device',
});

/**
 * The people a read of every tool counted once each, split by the sanction of the tools they used,
 * and who they are out of: the directory's active people when a directory is synced, else the
 * people signed in on an enrolled device. Nothing when the read did not count them.
 */
export function peopleOf(state) {
  const row = state?.meta?.extras?.people?.[0];
  if (!row) return null;
  const n = (value) => (value === null || value === undefined ? null : Number(value));
  const directory = n(row.directory_people) ?? 0;
  const devices = n(row.device_people) ?? 0;
  const basis = directory > 0 ? 'directory' : devices > 0 ? 'devices' : null;
  return Object.freeze({
    people: n(row.people),
    sanctionedOnly: n(row.people_sanctioned_only),
    both: n(row.people_both),
    notSanctionedOnly: n(row.people_unsanctioned_only),
    of: basis === 'directory' ? directory : basis === 'devices' ? devices : null,
    basis,
  });
}

/**
 * Who the people counted are out of, in words. People not in the directory can send prompts, so the
 * count can exceed it; it is then beside the directory, not a part of it.
 */
function outOf(counted) {
  if (counted?.of === null || counted?.of === undefined) return null;
  const whole = `${formatCount(counted.of)} ${PEOPLE_BASIS[counted.basis]}`;
  return (counted.people ?? 0) <= counted.of ? `of ${whole}` : `more than the ${whole}`;
}

/** The people who used AI in the window, each once, out of the people who could have; the busiest period's count when the read did not count them. */
function usersTile(state, options) {
  const counted = peopleOf(state);
  if (counted?.people === null || counted?.people === undefined) return peopleTile(state, 'Users', options);
  const note = outOf(counted) ?? 'Each person counted once, however many tools they used.';
  return tile('Users', { kind: 'number', text: formatCount(counted.people) }, note, options);
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
/** The window Policy reads usage over. */
const POLICY_WINDOW = 'd30';
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
/** A destination no catalogued app claims, named by its host. */
const isSite = (group) => String(group.key).startsWith(SITE_PREFIX);
const NOT_CATALOGUED = 'Not in the tool catalogue';

/**
 * The tools in use, by submissions over the window, each a link to its own screen. Rank is a
 * volume and never a sanction signal: the sanction is its own chip, in its own colour. Two or
 * more fingerprints the catalogue does not name are one line: each is a tool only by its hash, and
 * a run of identical "Unrecognised tool" lines says nothing a reader can act on; the fingerprints
 * are listed on their own under the Tools ranking. A site the catalogue does not name is a line of
 * its own, named by its host and marked as not catalogued.
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
      // The folded line opens the screen that lists its fingerprints, when this ranking is a
      // summary of one; on that screen itself the list is just below.
      href: folded ? href : link ? link(group.key) : null,
      label: name,
      mono: name === group.key,
      title: folded ? 'Tools the shared catalogue does not name, together' : `Tool fingerprint ${group.key}`,
      sublabel: folded ? `${plural(group.count, 'fingerprint', 'fingerprints')} the catalogue does not name`
        : isSite(group) ? NOT_CATALOGUED : name === UNRECOGNISED_TOOL ? group.key : null,
      subMono: !folded && !isSite(group),
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
    valueLabel: 'Prompts',
    metaLabels: Object.freeze(['People', 'Blocked', 'Data sent']),
    trendLabel: 'Trend',
    items: Object.freeze(items),
  });
}

/**
 * The sites and fingerprints the catalogue does not name, one line each with its figures: what to
 * review for the catalogue, and the way to a tool the ranking folds. Nothing when the only one is a
 * single fingerprint, which the ranking already lists by itself.
 */
export function uncataloguedTools(state, { link = toolHref } = {}) {
  const groups = foldCells(state, 'tool').filter((group) => isSite(group) || isUnrecognised(group)).sort(byMeasure('submissions'));
  if (!groups.some(isSite) && groups.length < 2) return null;
  const rows = groups.map((group) => Object.freeze({
    fingerprint_cell: isSite(group)
      ? Object.freeze({ primary: toolName(group.first), mono: false, title: `${NOT_CATALOGUED}: ${group.key}` })
      : Object.freeze({ primary: group.key, mono: true, title: `Tool fingerprint ${group.key}` }),
    submissions: foldedMeasure(group, 'submissions').value ?? null,
    users: foldedMeasure(group, 'users').value ?? null,
    bytes_total: foldedMeasure(group, 'bytes_total').value ?? null,
    last_active: group.trend.length > 0 ? group.trend[group.trend.length - 1].bucket : null,
    href: link(group.key),
  }));
  return Object.freeze({
    ...tableFrom({ data: rows }, {
      title: NOT_CATALOGUED,
      columns: [
        column('fingerprint_cell', 'Site or fingerprint', 'stack'),
        column('submissions', 'Prompts', 'measure'),
        column('users', 'People', 'measure'),
        column('bytes_total', 'Data sent', 'measure-bytes'),
        column('last_active', 'Last active', 'instant'),
      ],
      emptyText: 'Every tool in this window is in the catalogue.',
    }),
    rowHref: 'href',
    breakdowns: false,
  });
}

/** The sanction split, decided parts first. */
const SANCTION_PARTS = Object.freeze(['sanctioned', 'unsanctioned', 'unknown']);

/**
 * The tools detected in the window, split by their present-tense sanction. A row's `tool` is the
 * catalogue app its fingerprints belong to, so a tool counts once however many fingerprints it was
 * seen under; a fingerprint outside the catalogue is a tool of its own, with no decision. When no
 * tool has a decision, the block says so and opens where the decision is made.
 */
export function sanctionShare(state, { title = 'Applications', emptyText = 'No tool was detected in this window.', settingsHref = '#sanction' } = {}) {
  const tools = new Map();
  for (const row of state.data) {
    if (row.tool === null || row.tool === undefined) continue;
    tools.set(String(row.tool), SANCTION_PARTS.includes(row.sanctioned_state) ? row.sanctioned_state : 'unknown');
  }
  const counts = new Map(SANCTION_PARTS.map((key) => [key, 0]));
  for (const sanction of tools.values()) counts.set(sanction, counts.get(sanction) + 1);
  const total = tools.size;
  const undecided = total > 0 && counts.get('unknown') === total;
  return Object.freeze({
    kind: 'share',
    title,
    total,
    totalLabel: total === 1 ? 'tool detected' : 'tools detected',
    emptyText,
    ...(undecided ? { href: settingsHref, moreLabel: 'Decide which tools are sanctioned' } : {}),
    parts: Object.freeze(SANCTION_PARTS.map((key) => Object.freeze({ key, label: SANCTION_TEXT[key], count: counts.get(key), share: total > 0 ? counts.get(key) / total : 0 }))),
    note: undecided ? 'No tool has a sanction decision yet.' : 'A tool with no decision is neither sanctioned nor unsanctioned.',
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

/** The fleet's reporting figure beside its enrolled denominator, split into the devices that report and the ones that do not; it opens the device list. */
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
      split: reporting !== null && enrolled !== null ? Object.freeze([
        Object.freeze({ key: 'reporting', label: 'Reporting', count: reporting }),
        Object.freeze({ key: 'never_reported', label: 'Not reporting', count: Math.max(0, enrolled - reporting) }),
      ]) : null,
    });
}

/** What happened to a sensitive prompt. A rule that only logs it lets it through, so logged reads as allowed. */
const OUTCOMES = Object.freeze([
  Object.freeze({ key: 'allowed', label: 'Allowed', action: 'logged', measure: 'sensitive_logged', flagged: 'flagged_logged' }),
  Object.freeze({ key: 'blocked', label: 'Blocked', action: 'blocked', measure: 'sensitive_blocked', flagged: 'flagged_blocked' }),
  Object.freeze({ key: 'warned', label: 'Warned', action: 'warned', measure: 'sensitive_warned', flagged: 'flagged_warned' }),
]);

/** Search's events list with filters, at the window given when Search offers it, else the widest it does. */
function searchEventsLink(filters, preset) {
  const dataset = EXPLORE_DATASETS.events;
  const wanted = preset ?? dataset.defaultWindow;
  const windowPreset = dataset.windows.includes(wanted) ? wanted : dataset.windows[dataset.windows.length - 1];
  return { href: `explore.html${encodeExploreHash({ dataset, windowPreset, filters })}` };
}

const METADATA_ONLY = 'metadata-only collection (M0)';

/**
 * Every file attached to a prompt in the window, opening the prompts that carried one, narrowed by
 * the filters the page is read under. A prompt sent at metadata-only collection cannot describe an
 * attachment, so its count is unknown rather than zero: the tile says how many prompts it counted
 * on, and a window with none says so.
 */
function attachmentsTile(usage, { preset, filters = {} }) {
  const attachments = sumMeasure(usage, 'attachments');
  const described = sumMeasure(usage, 'content_prompts');
  const prompts = sumMeasure(usage, 'submissions');
  const link = searchEventsLink({ ...filters, attachments: 'yes' }, preset);
  if (attachments.kind !== 'number' || described.kind !== 'number') return tile('Attachments', { kind: 'absent', text: '—' }, absentNote(usage), link);
  if (described.value === 0 && prompts.kind === 'number' && prompts.value > 0) {
    return tile('Attachments', { kind: 'absent', text: '—' }, `Not collected: every prompt in this window was sent at ${METADATA_ONLY}, which does not describe attachments.`);
  }
  const partly = prompts.kind === 'number' && described.value < prompts.value;
  return tile('Attachments', { kind: 'number', text: formatCount(attachments.value) },
    partly
      ? `Counted on ${formatCount(described.value)} of ${formatCount(prompts.value)} prompts: ${METADATA_ONLY} does not describe attachments.`
      : 'Files attached to prompts in this window.',
    link);
}

/**
 * The prompts that matched an enforcement rule on one of their labels, out of every prompt, split by
 * what happened to them. It opens where they are broken down.
 */
function sensitiveTile(usage, { href }) {
  const sensitive = sumMeasure(usage, 'sensitive');
  const prompts = sumMeasure(usage, 'submissions');
  const options = { href };
  if (sensitive.kind !== 'number') return tile('Sensitive prompts', { kind: 'absent', text: '—' }, absentNote(usage), options);
  return tile('Sensitive prompts', { kind: 'number', text: formatCount(sensitive.value) },
    prompts.kind === 'number' ? `of ${formatCount(prompts.value)} prompts matched a rule on their labels` : null,
    {
      ...options,
      trend: trendOf(usage, 'sensitive'),
      split: sensitive.value > 0
        ? Object.freeze(OUTCOMES.map((o) => Object.freeze({ key: o.key, label: o.label, count: sumMeasure(usage, o.measure).value ?? 0 })))
        : null,
    });
}

/**
 * The rules that decided the most prompts in the window, each with its action, and what Policy
 * still waits on: the rules that matched nothing, and the tools with no sanction decision. The
 * whole card opens Policy. Without the rules or their matches, it says which could not be read.
 */
function policyAtWork(rules, matches, usage, { href, limit = 4 }) {
  const known = rules && !rules.isRefusal ? rules.data : null;
  const counted = matches && !matches.isRefusal ? matches : null;
  const base = { kind: 'ranking', title: 'Policy at work', cardHref: href, itemLabel: 'Rule', valueLabel: 'Prompts', metaLabels: Object.freeze([]) };
  if (!known || !counted) {
    const missing = !known ? 'The rules could not be read.' : 'What the rules matched could not be read for this window.';
    return Object.freeze({ ...base, items: Object.freeze([]), emptyText: missing });
  }
  if (known.length === 0) return Object.freeze({ ...base, items: Object.freeze([]), emptyText: 'No enforcement rule is set, so every prompt is logged.' });
  const groups = new Map(foldCells(counted, 'rule', { sum: ['matches'], max: [] }).map((g) => [g.key, g]));
  const matchesOf = (rule) => (groups.has(String(rule.rule)) ? foldedMeasure(groups.get(String(rule.rule)), 'matches').value ?? 0 : 0);
  const ranked = known.map((rule) => ({ rule, n: matchesOf(rule) })).filter((r) => r.n > 0)
    .sort((a, b) => b.n - a.n || String(a.rule.rule).localeCompare(String(b.rule.rule)));
  const total = ranked.reduce((sum, r) => sum + r.n, 0);
  const quiet = known.length - ranked.length;
  const undecided = new Set(usage.data.filter((row) => row.tool !== null && row.tool !== undefined && !['sanctioned', 'unsanctioned'].includes(row.sanctioned_state)).map((row) => String(row.tool))).size;
  const summary = [
    quiet === 0 ? 'Every rule matched a prompt in this window' : `${plural(quiet, 'rule', 'rules')} matched nothing in this window`,
    usage === NOTHING ? null : undecided === 0 ? 'every tool in use has a sanction decision' : `${plural(undecided, 'tool waits', 'tools wait')} for a sanction decision`,
  ].filter(Boolean).join(' · ');
  return Object.freeze({
    ...base,
    summary: `${summary}.`,
    emptyText: 'No rule matched a prompt in this window.',
    items: Object.freeze(ranked.slice(0, limit).map(({ rule, n }) => Object.freeze({
      key: String(rule.rule),
      href: null,
      label: String(rule.rule),
      mono: true,
      chip: Object.freeze({ key: RULE_ACTION_KEYS[rule.action] ?? 'unknown', text: RULE_ACTION_LABELS[rule.action] ?? String(rule.action ?? 'unknown') }),
      value: { kind: 'number', value: n },
      share: total > 0 ? n / total : null,
      meta: Object.freeze([]),
      trend: Object.freeze([]),
    }))),
  });
}

/**
 * The landing page: what a customer wants at a glance, each figure opening the screen that holds
 * the rest of it. Usage comes from its own question; when it does not come back its panels are
 * empty. The devices reporting tile keeps the enrolled denominator beside every figure here.
 *
 * @param {object} input
 * @param {object} input.devices    view state for the device list (it carries coverage)
 * @param {object} [input.tools]    view state for tool usage
 * @param {object} [input.rules]    view state for the enforcement rules as they stand
 * @param {object} [input.ruleMatches] view state for the prompts each rule matched in the window
 * @param {object} [input.coverage] a coverage block, when the device read did not carry one
 * @param {object} [options]
 * @param {boolean} [options.activity] whether Policy → Activity is offered, for the tile that opens it
 */
export function postureView({ devices, tools = null, rules = null, ruleMatches = null, coverage }, { preset = null, activity = true } = {}) {
  const cov = devices?.coverage ?? coverage ?? null;
  const usage = tools ?? NOTHING;
  const at = (hash) => withPreset(hash, preset);

  // The same warning arrives with each read; it is said once. Coverage is not a banner here: the
  // devices reporting tile carries it, and a panel that cannot answer says so in place.
  const banners = [];
  const said = new Set();
  for (const state of [devices, tools]) {
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
      measureTile('Prompts', usage, 'submissions', formatCount, { trend: trendOf(usage), href: at('#tools') }),
      attachmentsTile(usage, { preset }),
      // Activity breaks the sensitive prompts down; where it is not offered, the rules that
      // matched them are.
      sensitiveTile(usage, { href: activity ? at('#activity') : '#rules' }),
      usersTile(usage, { href: at('#person') }),
      reportingTile(cov),
    ]),
    blocks: Object.freeze([
      { ...sanctionShare(usage, { emptyText: emptyLine(tools, 'No tool was detected in this window.') }), cardHref: '#sanction' },
      policyAtWork(rules, ruleMatches, tools ? usage : NOTHING, { href: '#rules' }),
      rankTools(usage, { href: at('#tools'), limit: 6, link: (fingerprint) => toolHref(fingerprint, preset), emptyText: emptyLine(tools, 'No tool was in use in this window.') }),
    ]),
    series: Object.freeze([]),
    tables: Object.freeze([]),
    banners: Object.freeze(banners),
    notes: shared(devices ?? NOTHING).notes,
  });
}

// ---------------------------------------------------------------------------------------------
// Policy → Activity
// ---------------------------------------------------------------------------------------------

/** The period of the same length just before the window, in words: "the previous 7 days". */
function previousPeriod(preset) {
  return (WINDOWS[preset] ?? WINDOWS.d7).label.replace(/^Last /, 'the previous ');
}

/** A read of the previous period that can be compared with: one that answered, in full. */
function comparable(previous) {
  return previous && !previous.isRefusal && (previous.resultState === 'ok' || previous.resultState === 'empty') ? previous : null;
}

/** A total over the previous period; a read that answered with no cell is a zero. */
function previousTotal(previous, measure) {
  if (!previous) return null;
  return previous.data.length === 0 ? { kind: 'number', value: 0 } : sumMeasure(previous, measure);
}

/** How a count moved against the previous period. Nothing unless both counts are known. */
function changeOf(current, previous, period) {
  if (current?.kind !== 'number' || previous?.kind !== 'number') return null;
  if (previous.value === 0) {
    return Object.freeze(current.value === 0
      ? { direction: 'flat', text: `None in ${period} either` }
      : { direction: 'up', text: `None in ${period}` });
  }
  const ratio = (current.value - previous.value) / previous.value;
  if (Math.abs(ratio) < 0.005) return Object.freeze({ direction: 'flat', text: `Same as ${period}` });
  return Object.freeze({
    direction: ratio > 0 ? 'up' : 'down',
    text: `${ratio > 0 ? '+' : '−'}${Math.max(1, Math.round(Math.abs(ratio) * 100))}% on ${period}`,
  });
}

/** One measure per bucket over a set of cells, for a sparkline. */
function bucketTrend(rows, measure) {
  const byBucket = new Map();
  for (const row of rows) {
    const value = measureOf(row, measure);
    if (value.kind === 'number') byBucket.set(row.bucket ?? 'total', (byBucket.get(row.bucket ?? 'total') ?? 0) + value.value);
  }
  return Object.freeze([...byBucket.entries()]
    .sort((a, b) => String(a[0]).localeCompare(String(b[0])))
    .map(([bucket, value]) => Object.freeze({ bucket, value })));
}

const countValue = (value) => (value.kind === 'number' ? { kind: 'number', text: formatCount(value.value) } : { kind: 'absent', text: '—' });

/**
 * Prompts, then the sensitive ones as a share of them, then what happened to those: each stage
 * with its change on the previous period.
 */
function exposureFunnel(usage, previous, { period, emptyText }) {
  const prompts = sumMeasure(usage, 'submissions');
  const sensitive = sumMeasure(usage, 'sensitive');
  const shareText = (part, whole, of) => (part.kind === 'number' && whole.kind === 'number' && whole.value > 0 ? `${formatPercent(part.value / whole.value)} of ${of}` : null);
  const stage = (key, label, value, detail, measure) => Object.freeze({
    key, label, value: countValue(value), count: value.value ?? 0, detail, change: changeOf(value, previousTotal(previous, measure), period),
  });
  return Object.freeze({
    kind: 'funnel',
    title: 'Exposure',
    stages: Object.freeze([
      stage('prompts', 'Prompts', prompts, null, 'submissions'),
      stage('sensitive', 'Sensitive prompts', sensitive, shareText(sensitive, prompts, 'prompts'), 'sensitive'),
    ]),
    outcomes: Object.freeze(OUTCOMES.map((o) => {
      const value = sumMeasure(usage, o.measure);
      return stage(o.key, o.label, value, shareText(value, sensitive, 'sensitive prompts'), o.measure);
    })),
    emptyText: prompts.kind === 'number' ? null : emptyText,
  });
}

/**
 * The tools sensitive prompts went to, ranked by them, each with how many were allowed, blocked
 * and warned, and its change on the previous period. Above it, the sensitive prompts allowed to
 * tools that are not sanctioned.
 */
function rankExposure(usage, previous, { link, period, emptyText }) {
  const groups = foldCells(usage, 'tool').filter((group) => (foldedMeasure(group, 'sensitive').value ?? 0) > 0).sort(byMeasure('sensitive'));
  const before = previous ? new Map(foldCells(previous, 'tool').map((group) => [group.key, group])) : null;
  const total = totalOf(groups, 'sensitive');
  const items = groups.map((group) => {
    const name = toolName(group.first);
    const sensitive = foldedMeasure(group, 'sensitive');
    const earlier = before ? (before.has(group.key) ? foldedMeasure(before.get(group.key), 'sensitive') : { kind: 'number', value: 0 }) : null;
    return Object.freeze({
      key: group.key,
      href: link ? link(group.key) : null,
      label: name,
      mono: name === group.key,
      title: `Tool fingerprint ${group.key}`,
      sublabel: [name === UNRECOGNISED_TOOL ? group.key : null, changeOf(sensitive, earlier, period)?.text].filter(Boolean).join(' · ') || null,
      chip: sanctionChip(group.first.sanctioned_state ?? 'unknown'),
      value: sensitive,
      share: shareOf(sensitive, total),
      meta: Object.freeze(OUTCOMES.map((o) => Object.freeze({ label: o.label.toLowerCase(), value: foldedMeasure(group, o.measure) }))),
      trend: bucketTrend(group.rows, 'sensitive'),
    });
  });
  const allowedTo = (state) => groups
    .filter((group) => (group.first.sanctioned_state ?? 'unknown') === state)
    .reduce((n, group) => n + (foldedMeasure(group, 'sensitive_logged').value ?? 0), 0);
  const unsanctioned = allowedTo('unsanctioned');
  const undecided = allowedTo('unknown');
  const summary = groups.length === 0 ? null : [
    unsanctioned === 0
      ? 'No sensitive prompt was allowed to an unsanctioned tool'
      : `${plural(unsanctioned, 'sensitive prompt was', 'sensitive prompts were')} allowed to unsanctioned tools`,
    undecided > 0 ? `, and ${formatCount(undecided)} to tools with no decision yet` : '',
    '.',
  ].join('');
  return Object.freeze({
    kind: 'ranking',
    title: 'Where sensitive prompts went',
    href: null,
    summary,
    emptyText,
    itemLabel: 'Tool',
    valueLabel: 'Sensitive',
    metaLabels: Object.freeze(OUTCOMES.map((o) => o.label)),
    trendLabel: 'Trend',
    items: Object.freeze(items),
  });
}

/**
 * The labels rules matched, each with its sensitive prompts and what happened to them. A prompt
 * with two such labels is in both rows, so the rows are not summed.
 */
function exposedTable(classes, { emptyText }) {
  const measures = ['flagged', ...OUTCOMES.map((o) => o.flagged)];
  const rows = classes && !classes.isRefusal
    ? foldCells(classes, 'class', { sum: measures, max: [] })
      .filter((group) => (foldedMeasure(group, 'flagged').value ?? 0) > 0)
      .sort(byMeasure('flagged'))
      .map((group) => Object.freeze({
        class: group.key,
        severity: group.first.severity ?? null,
        ...Object.fromEntries(measures.map((m) => [m, foldedMeasure(group, m).value ?? null])),
      }))
    : [];
  return Object.freeze({
    kind: 'table',
    ...tableFrom({ data: rows }, {
      title: 'What was exposed',
      columns: [
        column('class', 'Label', 'vocab'),
        column('severity', 'Severity', 'vocab'),
        column('flagged', 'Sensitive prompts', 'measure'),
        ...OUTCOMES.map((o) => column(o.flagged, o.label, 'measure')),
      ],
      emptyText,
    }),
    breakdowns: false,
  });
}

const RULE_MATCHES = Object.freeze(['matches', 'blocked', 'warned', 'logged']);
/** A rule's action, keyed by the outcome it produces so it takes that outcome's colour. */
const RULE_ACTION_KEYS = Object.freeze({ allow: 'allowed', warn: 'warned', block: 'blocked', redirect: 'blocked' });
const CATEGORY_TEXT = Object.freeze(Object.fromEntries(RULE_CATEGORIES.map((c) => [c.key, c.label])));
const ROUTE_TEXT = Object.freeze(Object.fromEntries(RULE_ROUTES.map((r) => [r.key, r.label])));

/** What a rule matches, in words: a custom label and a tool by the names the read carried. */
function ruleConditionsText(rule) {
  const match = rule.match ?? {};
  const name = (field, value) => ({
    labels: rule.label_names?.[value],
    tools: rule.tool_names?.[value],
    categories: CATEGORY_TEXT[value],
    sanction: SANCTION_TEXT[value],
    routes: ROUTE_TEXT[value],
  })[field] ?? value;
  const parts = RULE_MATCH_FIELDS
    .filter((field) => Array.isArray(match[field]) && match[field].length > 0)
    .map((field) => `${RULE_MATCH_TITLES[field]}: ${match[field].map((value) => name(field, value)).join(' or ')}`);
  return parts.length > 0 ? parts.join('; ') : 'Any prompt';
}

/**
 * Every enforcement rule, in the order a device applies it, with the prompts it matched in the
 * window split by what happened to them. A rule that matched none is listed with zeros, and a rule
 * since removed is listed after the rest under its id. Without the rules, the rows are the ids the
 * matches name; without the matches, the rules are listed with their counts absent.
 */
function rulesTable(rules, matches, { period = 'in this window' } = {}) {
  const known = rules && !rules.isRefusal ? rules.data : null;
  const counted = matches && !matches.isRefusal ? matches : null;
  const groups = new Map((counted ? foldCells(counted, 'rule', { sum: RULE_MATCHES, max: [] }) : []).map((g) => [g.key, g]));
  const count = (id, measure) => (groups.has(id) ? foldedMeasure(groups.get(id), measure).value ?? 0 : 0);
  const counts = (id) => Object.fromEntries(RULE_MATCHES.map((m) => [m, counted ? count(id, m) : null]));
  const rows = (known ?? []).map((rule, i) => Object.freeze({
    rule: Object.freeze({ primary: rule.rule, mono: true, secondary: `Rule ${i + 1} · ${ruleConditionsText(rule)}` }),
    action: RULE_ACTION_KEYS[rule.action] ?? 'unknown',
    action_text: RULE_ACTION_LABELS[rule.action] ?? String(rule.action ?? 'unknown'),
    ...counts(String(rule.rule)),
  }));
  const configured = new Set((known ?? []).map((rule) => String(rule.rule)));
  for (const group of [...groups.values()].filter((g) => !configured.has(g.key)).sort(byMeasure('matches'))) {
    rows.push(Object.freeze({
      rule: Object.freeze({ primary: group.key, mono: true, secondary: known ? 'No longer a rule' : null }),
      action: known ? 'removed' : 'unknown',
      action_text: known ? 'Removed' : 'Not read',
      ...counts(group.key),
    }));
  }
  let summary = null;
  if (known && counted && known.length > 0) {
    const quiet = known.filter((rule) => count(String(rule.rule), 'matches') === 0).length;
    summary = quiet === 0 ? `Every rule matched at least one prompt ${period}.`
      : quiet === known.length ? `No rule matched a prompt ${period}.`
        : `${formatCount(known.length - quiet)} of ${formatCount(known.length)} rules matched a prompt ${period}; ${formatCount(quiet)} matched none.`;
  } else if (known && !counted) {
    summary = `The prompts each rule matched could not be read ${period.replace(/^in /, 'for ')}.`;
  }
  const emptyText = known ? 'No enforcement rule is set, so every prompt is logged. Rules are set on Policy → Rules.'
    : rules?.isRefusal ? `The rules could not be read: ${rules.error?.message ?? rules.resultState}.`
      : 'The rules could not be read.';
  return Object.freeze({
    kind: 'table',
    ...tableFrom({ data: rows }, {
      title: 'Rules',
      columns: [
        column('rule', 'Rule', 'stack'),
        column('action', 'Action', 'status'),
        column('matches', 'Matched', 'measure'),
        ...OUTCOMES.map((o) => column(o.action, o.label, 'measure')),
      ],
      emptyText,
    }),
    ...(summary ? { summary } : {}),
    breakdowns: false,
  });
}

/** Sensitive prompts per bucket, each bucket split by what happened to them. */
function exposureSeries(usage) {
  if (!(usage.meta?.source ?? '').startsWith('mart.')) return [];
  const byBucket = new Map();
  for (const row of usage.data) {
    const bucket = row.bucket ?? 'total';
    if (!byBucket.has(bucket)) byBucket.set(bucket, new Map(OUTCOMES.map((o) => [o.key, 0])));
    const parts = byBucket.get(bucket);
    for (const o of OUTCOMES) {
      const value = measureOf(row, o.measure);
      if (value.kind === 'number') parts.set(o.key, parts.get(o.key) + value.value);
    }
  }
  const points = [...byBucket.entries()]
    .sort((a, b) => String(a[0]).localeCompare(String(b[0])))
    .map(([bucket, parts]) => Object.freeze({
      bucket,
      value: { kind: 'number', value: [...parts.values()].reduce((a, b) => a + b, 0) },
      parts: Object.freeze(OUTCOMES.map((o) => Object.freeze({ key: o.key, label: o.label, value: parts.get(o.key) }))),
    }));
  if (!points.some((p) => p.value.value > 0)) return [];
  return [Object.freeze({ title: seriesTitle(usage, 'Sensitive prompts'), points: Object.freeze(points), unavailable: false, reason: null })];
}

/**
 * Policy → Activity: the prompts that matched an enforcement rule on a label the rule names, as a
 * share of every prompt; what happened to them; the tools they went to; the labels involved; and
 * the shape over the window. Every count is compared with the period of the same length just
 * before it, when that period could be read.
 *
 * @param {object} input
 * @param {object} input.usage       view state for tool usage over the window (Q1)
 * @param {object} [input.previous]  the same read over the previous period
 * @param {object} [input.classes]   view state for the class mix over the window (Q4)
 * @param {object} [input.rules]     view state for the enforcement rules as they stand
 * @param {object} [input.ruleMatches] view state for the prompts each rule matched in the window
 */
export function activityView({ usage, previous = null, classes = null, rules = null, ruleMatches = null }, { preset = 'd7', link = toolHref } = {}) {
  const period = previousPeriod(preset);
  const before = comparable(previous);
  const notes = [
    'A prompt is sensitive when the enforcement rule it matched names one of its labels; only the labels that rule names count for it.',
    'Allowed counts the sensitive prompts the matching rule only logged, and those sent by a route that cannot block.',
    'Sensitive prompts are counted against the rules as they stand now, so a rule edited today can change the figures for recent days.',
    'A prompt with two labels a rule names is counted under each in What was exposed.',
    'What was exposed lists the built-in data classes: a prompt a rule matched on a custom label counts in the figures above it, but has no row there.',
    'Rules counts every prompt each rule matched. A rule that names no label matches on tool, category, sanction or route alone, so its prompts are not sensitive and are not in the figures above.',
  ];
  if (ruleMatches?.meta?.truncated) notes.push('Not every rule\'s matches could be read for this window, so Rules undercounts; a shorter window reads them all.');
  const prompts = sumMeasure(usage, 'submissions');
  const sensitive = sumMeasure(usage, 'sensitive');
  if (sensitive.kind === 'number' && sensitive.value === 0 && prompts.kind === 'number' && prompts.value > 0) {
    notes.unshift('No prompt in this window matched a rule that names one of its labels. Nothing counts as sensitive until an enforcement rule names a label; rules are set on Policy → Rules.');
  }
  notes.push(before
    ? `Each change compares this window with ${period}.`
    : `${period[0].toUpperCase()}${period.slice(1)} could not be read, so no change is shown.`);
  const degraded = classes && !classes.isRefusal ? sumMeasure(classes, 'degraded_events') : null;
  if (degraded?.kind === 'number' && degraded.value > 0) {
    notes.push(`${formatCount(degraded.value)} events in this window could not be classified, so they carry no label and none is counted as sensitive.`);
  }
  const exposedEmpty = !classes ? 'The labels could not be read for this window.'
    : classes.isRefusal ? `The labels could not be read: ${classes.error?.message ?? classes.resultState}.`
      : emptyLine(classes, 'No label a rule names was found in this window.');
  return screen('activity', 'Activity', null, 'mart.v_tool_usage', {
    subtitle: 'What the rules caught: the prompts that matched an enforcement rule on their labels, where they went and what happened to them.',
    lead: Object.freeze([exposureFunnel(usage, before, { period, emptyText: emptyLine(usage, 'No prompts in this window.') })]),
    tiles: Object.freeze([]),
    blocks: Object.freeze([
      rankExposure(usage, before, { link: link ? (fingerprint) => link(fingerprint, preset) : null, period, emptyText: emptyLine(usage, 'No sensitive prompt in this window.') }),
      exposedTable(classes, { emptyText: exposedEmpty }),
      rulesTable(rules, ruleMatches),
    ]),
    series: Object.freeze(exposureSeries(usage)),
    tables: Object.freeze([]),
    ...shared(usage, notes),
  });
}

/**
 * Policy → Rules as everyone but an admin reads it: the rules in the order a device applies them,
 * each with the prompts it decided over the last 30 days. An admin edits them on the same page.
 *
 * @param {object} input
 * @param {object} input.rules         view state for the enforcement rules as they stand
 * @param {object} [input.ruleMatches] view state for the prompts each rule matched over 30 days
 */
export function rulesView({ rules, ruleMatches = null }) {
  const table = rulesTable(rules, ruleMatches, { period: 'in the last 30 days' });
  const notes = [
    'The first rule that matches a prompt decides what happens to it; a prompt no rule matches is allowed and logged.',
    'Each rule\'s counts are the prompts it decided in the last 30 days, split by what happened to them.',
    'Only an admin can change the rules.',
  ];
  if (ruleMatches?.meta?.truncated) notes.push('Not every rule\'s matches could be read, so some counts are lower than they are.');
  const state = ruleMatches ?? rules;
  return screen('rules', 'Rules', null, 'ops.enforcement_rule', {
    subtitle: 'What happens to a prompt, by who sends it, the tool it goes to and what it holds. The first rule that matches decides.',
    tiles: Object.freeze([]),
    tables: Object.freeze([table]),
    series: Object.freeze([]),
    ...shared(state, notes),
  });
}

/** The order Policy → Tools lists decisions in: the tools still waiting for one first. */
const DECISION_ORDER = Object.freeze(['unknown', 'unsanctioned', 'sanctioned']);

/**
 * Policy → Tools: every tool in use over the last 30 days with its sanction decision, the tools
 * still waiting for one first, each opening its usage. An admin decides them above the list.
 *
 * @param {object} state view state for tool usage over the last 30 days (Q1)
 */
export function sanctionView(state, { link = toolHref } = {}) {
  const decision = (group) => (DECISION_ORDER.includes(group.first.sanctioned_state) ? group.first.sanctioned_state : 'unknown');
  const groups = foldCells(state, 'tool').sort((a, b) => DECISION_ORDER.indexOf(decision(a)) - DECISION_ORDER.indexOf(decision(b))
    || toolName(a.first).localeCompare(toolName(b.first), undefined, { sensitivity: 'base' }) || a.key.localeCompare(b.key));
  const rows = groups.map((group) => {
    const name = toolName(group.first);
    return Object.freeze({
      decision: SANCTION_TEXT[decision(group)],
      tool_cell: Object.freeze({
        primary: name,
        mono: name === group.key,
        title: `Tool fingerprint ${group.key}`,
        secondary: isSite(group) ? NOT_CATALOGUED : name === UNRECOGNISED_TOOL ? group.key : null,
      }),
      submissions: foldedMeasure(group, 'submissions').value ?? null,
      users: foldedMeasure(group, 'users').value ?? null,
      href: link ? link(group.key, POLICY_WINDOW) : null,
    });
  });
  return screen('sanction', 'Tools', null, 'mart.v_tool_usage', {
    subtitle: 'Each tool in use over the last 30 days with its sanction decision. A tool with no decision is unknown, never unsanctioned.',
    lead: Object.freeze([sanctionShare(state, { emptyText: emptyLine(state, 'No tool was detected in the last 30 days.'), settingsHref: null })]),
    tiles: Object.freeze([]),
    tables: Object.freeze([
      Object.freeze({
        ...tableFrom({ data: rows }, {
          title: 'Tools by decision',
          columns: [
            column('decision', 'Decision'),
            column('tool_cell', 'Tool', 'stack'),
            column('submissions', 'Prompts', 'measure'),
            column('users', 'People', 'measure'),
          ],
          emptyText: emptyLine(state, 'No tool was in use in the last 30 days.'),
        }),
        groupBy: 'decision',
        rowHref: 'href',
        breakdowns: false,
      }),
    ]),
    series: Object.freeze([]),
    ...shared(state, [
      'A decision covers every fingerprint of the tool: each way a device recognises it. Only an admin can change it.',
      'Prompts and people are the last 30 days. A tool\'s people is the most counted in any one day, a floor for the 30 days.',
    ]),
  });
}

// ---------------------------------------------------------------------------------------------
// Insights: Adoption
// ---------------------------------------------------------------------------------------------

/** The people who used AI, out of the people who could have, split by the sanction of their tools. */
function peopleShare(counted, before, { period, emptyText }) {
  const total = counted?.people ?? 0;
  const part = (key, label, count) => Object.freeze({ key, label, count: count ?? 0, share: total > 0 ? (count ?? 0) / total : 0 });
  const of = counted?.of ?? null;
  const share = of !== null && of > 0 && total <= of ? ` (${formatPercent(total / of)})` : '';
  const change = counted && before ? changeOf({ kind: 'number', value: total }, { kind: 'number', value: before.people ?? 0 }, period) : null;
  return Object.freeze({
    kind: 'share',
    title: 'People using AI',
    total,
    totalLabel: of !== null ? `${outOf(counted)}${share}` : (total === 1 ? 'person' : 'people'),
    emptyText: counted ? emptyText : 'The people could not be counted for this window.',
    note: change?.text ?? null,
    parts: Object.freeze([
      part('sanctioned', 'Only sanctioned tools', counted?.sanctionedOnly),
      part('both', 'Both', counted?.both),
      part('unsanctioned', 'Only tools not sanctioned', counted?.notSanctionedOnly),
    ]),
  });
}

/**
 * The applications that sent their first prompt in the window, newest first: when, whether they
 * are sanctioned, and how many people have used them since. Each opens its tool's screen.
 */
function newToolsTable(firstSeen, { link, earliest }) {
  const rows = firstSeen && !firstSeen.isRefusal
    ? firstSeen.data.map((row) => Object.freeze({
      tool_cell: Object.freeze({
        primary: toolName(row),
        mono: !row.tool_name,
        title: `Tool fingerprint ${row.tool}`,
        secondary: isSite({ key: row.tool }) ? NOT_CATALOGUED : toolName(row) === UNRECOGNISED_TOOL ? String(row.tool) : null,
      }),
      sanctioned_state: SANCTION_TEXT[row.sanctioned_state] ? row.sanctioned_state : 'unknown',
      sanctioned_state_text: SANCTION_TEXT[row.sanctioned_state] ?? SANCTION_TEXT.unknown,
      first_seen_day: formatInstant(row.first_seen).slice(0, 10),
      people: row.people === null || row.people === undefined ? null : Number(row.people),
      href: link ? link(String(row.tool)) : null,
    }))
    : [];
  let summary = null;
  if (rows.length > 0) {
    const n = rows.length;
    const count = (state) => rows.filter((r) => r.sanctioned_state === state).length;
    const parts = [];
    if (count('unsanctioned') > 0) parts.push(`${formatCount(count('unsanctioned'))} ${count('unsanctioned') === 1 ? 'is' : 'are'} unsanctioned`);
    if (count('unknown') > 0) parts.push(`${formatCount(count('unknown'))} ${count('unknown') === 1 ? 'has' : 'have'} no decision yet`);
    const more = firstSeen.page?.next_cursor ? 'More than' : null;
    const said = `${more ? `${more} ` : ''}${plural(n, 'application', 'applications')} sent ${n === 1 && !more ? 'its' : 'their'} first prompt in this window`;
    summary = parts.length > 0 ? `${said}; ${parts.join(' and ')}.` : `${said}; ${n === 1 && !more ? 'it is' : 'all are'} sanctioned.`;
    if (earliest) summary = `${summary} ${earliest}`;
  }
  const emptyText = !firstSeen ? 'The applications new in this window could not be read.'
    : firstSeen.isRefusal ? `The applications new in this window could not be read: ${firstSeen.error?.message ?? firstSeen.resultState}.`
      : 'No application sent its first prompt in this window.';
  return Object.freeze({
    kind: 'table',
    ...tableFrom({ data: rows }, {
      title: 'New this period',
      columns: [
        column('tool_cell', 'Application', 'stack'),
        column('sanctioned_state', 'Sanction', 'status'),
        column('first_seen_day', 'First seen'),
        column('people', 'People since', 'measure'),
      ],
      emptyText,
    }),
    ...(summary ? { summary } : {}),
    rowHref: 'href',
    breakdowns: false,
  });
}

const SHIFT_PARTS = Object.freeze(['sanctioned', 'unknown', 'unsanctioned']);

/**
 * Prompts per bucket split by the sanction of the tool they went to, and the sanctioned share at
 * the start of the window against its end: whether use is moving to sanctioned tools.
 */
function sanctionShift(shift) {
  if (!shift || shift.isRefusal || !(shift.meta?.source ?? '').startsWith('mart.')) return null;
  const byBucket = new Map();
  for (const row of shift.data) {
    const bucket = row.bucket ?? 'total';
    if (!byBucket.has(bucket)) byBucket.set(bucket, new Map(SHIFT_PARTS.map((key) => [key, 0])));
    const value = measureOf(row, 'submissions');
    const key = SHIFT_PARTS.includes(row.sanctioned_state) ? row.sanctioned_state : 'unknown';
    if (value.kind === 'number') byBucket.get(bucket).set(key, byBucket.get(bucket).get(key) + value.value);
  }
  const points = [...byBucket.entries()]
    .sort((a, b) => String(a[0]).localeCompare(String(b[0])))
    .map(([bucket, parts]) => Object.freeze({
      bucket,
      value: { kind: 'number', value: [...parts.values()].reduce((a, b) => a + b, 0) },
      parts: Object.freeze(SHIFT_PARTS.map((key) => Object.freeze({ key, label: SANCTION_TEXT[key], value: parts.get(key) }))),
    }));
  const counted = points.filter((p) => p.value.value > 0);
  if (counted.length === 0) return null;
  const sanctioned = (point) => point.parts[0].value / point.value.value;
  const unit = shift.meta?.applied_bucket ?? 'bucket';
  const last = counted[counted.length - 1];
  const first = counted[0];
  let summary = `Sanctioned tools took ${formatPercent(sanctioned(last))} of prompts`;
  if (counted.length === 1) summary += ' in this window.';
  else {
    const move = Math.round(sanctioned(last) * 100) - Math.round(sanctioned(first) * 100);
    summary += ` in the latest ${unit}, ${move === 0 ? 'the same as' : move > 0 ? 'up from' : 'down from'} ${formatPercent(sanctioned(first))} in the first.`;
  }
  return Object.freeze({
    kind: 'series',
    title: `Prompts per ${unit}, by sanction`,
    points: Object.freeze(points),
    summary,
    unavailable: false,
    reason: null,
  });
}

/**
 * Insights, Adoption: how many people used AI out of how many could have, and on which side of the
 * sanction line; the applications new in the window; and whether prompts are moving to sanctioned
 * tools. The people are compared with the period of the same length just before the window.
 *
 * @param {object} input
 * @param {object} input.usage        view state for tool usage over the window (Q1), with its people
 * @param {object} [input.previous]   the same read over the previous period
 * @param {object} [input.firstSeen]  view state for the tools first seen in the window
 * @param {object} [input.shift]      view state for prompts per bucket by sanction
 */
export function adoptionView({ usage, previous = null, firstSeen = null, shift = null }, { preset = 'd7', link = toolHref } = {}) {
  const period = previousPeriod(preset);
  const counted = peopleOf(usage);
  const before = peopleOf(comparable(previous));
  const notes = [
    'People using AI counts everyone who sent a prompt in the window once, however many tools and days they used.',
    'A tool not sanctioned is one marked unsanctioned or with no decision yet.',
  ];
  if (counted?.basis === 'directory') notes.push('The people are out of everyone the directory lists as active.');
  else if (counted?.basis === 'devices') notes.push('No directory is synced, so the people are out of everyone last signed in on an enrolled device.');
  else if (counted) notes.push('No directory is synced and no enrolled device has said who is signed in, so there is no count to compare with.');
  notes.push(before
    ? `The change compares this window with ${period}.`
    : `${period[0].toUpperCase()}${period.slice(1)} could not be read, so no change is shown.`);
  notes.push('An application counts from the day it sent its first prompt here, under any of its fingerprints; People since counts everyone who has used it from then until now.');
  // Nothing before the window means collection began inside it, so every tool in use is new.
  const fresh = comparable(previous) && previous.data.length === 0 ? 'Nothing was recorded before this window, so every application in use is new.' : null;
  const shiftSeries = sanctionShift(shift);
  return screen('insights', 'Insights', null, 'mart.v_tool_usage', {
    subtitle: 'Adoption: how many people use AI, which applications are new, and whether use is moving to sanctioned tools.',
    lead: Object.freeze([peopleShare(counted, before, { period, emptyText: emptyLine(usage, 'No one sent a prompt in this window.') })]),
    tiles: Object.freeze([]),
    blocks: Object.freeze([
      newToolsTable(firstSeen, { link: link ? (fingerprint) => link(fingerprint, preset) : null, earliest: fresh }),
      ...(shiftSeries ? [shiftSeries] : []),
    ]),
    series: Object.freeze([]),
    tables: Object.freeze([]),
    ...shared(usage, notes),
  });
}

// ---------------------------------------------------------------------------------------------
// Insights: Discovered
// ---------------------------------------------------------------------------------------------

/**
 * A tool as one cell: its name with its sanction, and beneath it what tells it apart when the
 * catalogue does not name it.
 */
const toolCell = (row) => Object.freeze({
  primary: toolName(row),
  mono: !row.tool_name,
  title: `Tool fingerprint ${row.tool}`,
  chip: sanctionChip(row.sanctioned_state ?? 'unknown'),
  secondary: isSite({ key: row.tool }) ? NOT_CATALOGUED : toolName(row) === UNRECOGNISED_TOOL ? String(row.tool) : null,
});

/** What each kind of finding is called, in the order a tool lists them: local models and endpoints first. */
const FOUND_AS = Object.freeze({
  local_model: 'Local model',
  inference_connection: 'Inference endpoint',
  app_installed: 'Desktop app',
  cli_installed: 'CLI',
  ide_extension: 'IDE extension',
  app_running: 'Seen running',
});
const LOCAL = Object.freeze(['local_model', 'inference_connection']);
/** The models or hosts a row names before the rest are counted. */
const ITEMS_SHOWN = 3;

/**
 * The prompts each tool sent in the window, by the tool its rows count as, which is also its Tools
 * row. Nothing when the read did not answer.
 */
function promptsByTool(prompts) {
  if (!prompts || prompts.isRefusal) return null;
  const tools = new Map();
  for (const row of prompts.data) {
    const value = measureOf(row, 'submissions');
    if (value.kind === 'number') tools.set(String(row.tool), (tools.get(String(row.tool)) ?? 0) + value.value);
  }
  return Object.freeze({ truncated: Boolean(prompts.meta?.truncated), get: (tool) => tools.get(tool) ?? null });
}

/**
 * What discovery found, one entry per tool: its kinds of finding, the models and hosts it named, the
 * most devices any finding was on, and when it was first and last seen.
 */
function foundTools(found) {
  const tools = new Map();
  for (const row of found.data) {
    const key = String(row.tool);
    const tool = tools.get(key) ?? { row, types: new Set(), items: new Set(), devices: null, firstSeen: null, lastSeen: null };
    if (FOUND_AS[row.discovery_type]) tool.types.add(row.discovery_type);
    if (row.item) tool.items.add(String(row.item));
    const devices = measureOf(row, 'devices');
    if (devices.kind === 'number') tool.devices = Math.max(tool.devices ?? 0, devices.value);
    if (row.first_seen && (!tool.firstSeen || String(row.first_seen) < tool.firstSeen)) tool.firstSeen = String(row.first_seen);
    if (row.last_seen && (!tool.lastSeen || String(row.last_seen) > tool.lastSeen)) tool.lastSeen = String(row.last_seen);
    tools.set(key, tool);
  }
  return [...tools.entries()].map(([key, tool]) => ({ key, ...tool }));
}

/** A list of names in a cell: the first few, and how many more. */
function namesList(names) {
  const sorted = [...names].sort((a, b) => a.localeCompare(b));
  const shown = sorted.slice(0, ITEMS_SHOWN).join(', ');
  return sorted.length > ITEMS_SHOWN ? `${shown} and ${formatCount(sorted.length - ITEMS_SHOWN)} more` : shown;
}

/**
 * Every tool discovery found in the window, local models and inference endpoints first, then by
 * the devices it was on: what it was found as, the models and hosts it named, when it was first and
 * last seen, and the prompts it sent in the window. A tool that sent prompts opens its Tools row.
 */
function foundTable(tools, { reporting, link, prompted, truncated, emptyText }) {
  const local = (tool) => LOCAL.some((type) => tool.types.has(type));
  const rows = [...tools]
    .sort((a, b) => Number(local(b)) - Number(local(a)) || (b.devices ?? -1) - (a.devices ?? -1) || toolName(a.row).localeCompare(toolName(b.row)))
    .map((tool) => {
      const sent = prompted ? prompted.get(tool.key) : null;
      // A tool the read of prompts left out is unknown when that read was cut short, and silent otherwise.
      const prompts = !prompted ? null : sent !== null ? sent : prompted.truncated ? null : 0;
      const cell = toolCell(tool.row);
      return Object.freeze({
        tool_cell: Object.freeze({ ...cell, secondary: [tool.items.size > 0 ? namesList(tool.items) : null, cell.secondary].filter(Boolean).join(' · ') || null }),
        found_as: Object.keys(FOUND_AS).filter((type) => tool.types.has(type)).map((type) => FOUND_AS[type]).join(', '),
        devices: tool.devices,
        first_seen_day: tool.firstSeen ? formatInstant(tool.firstSeen).slice(0, 10) : null,
        last_seen_day: tool.lastSeen ? formatInstant(tool.lastSeen).slice(0, 10) : null,
        prompts,
        href: sent > 0 && link ? link(tool.key) : null,
      });
    });
  return Object.freeze({
    kind: 'table',
    ...tableFrom({ data: rows }, {
      title: 'Found on devices',
      columns: [
        column('tool_cell', 'Application', 'stack'),
        column('found_as', 'Found as'),
        column('devices', reporting === null ? 'Devices' : `Devices of ${formatCount(reporting)}`, 'measure'),
        column('first_seen_day', 'First seen'),
        column('last_seen_day', 'Last seen'),
        column('prompts', 'Prompts', 'measure'),
      ],
      emptyText,
    }),
    ...(rows.length > 0 ? { summary: foundSummary(tools, rows, { truncated }) } : {}),
    rowHref: 'href',
    breakdowns: false,
  });
}

function foundSummary(tools, rows, { truncated }) {
  const n = tools.length;
  const count = (type) => tools.filter((tool) => tool.types.has(type)).length;
  const parts = [];
  if (count('local_model') > 0) parts.push(`${formatCount(count('local_model'))} ${count('local_model') === 1 ? 'holds' : 'hold'} local models`);
  if (count('inference_connection') > 0) parts.push(`${formatCount(count('inference_connection'))} reached an inference endpoint`);
  const silent = rows.filter((row) => row.prompts === 0).length;
  if (silent > 0) parts.push(`${formatCount(silent)} sent no prompt`);
  const said = `${truncated ? 'More than ' : ''}${plural(n, 'application was', 'applications were')} found on devices in this window`;
  if (parts.length === 0) return `${said}.`;
  const last = parts.pop();
  return `${said}; ${parts.length > 0 ? `${parts.join(', ')} and ${last}` : last}.`;
}

/** Where the inventory scan stands on the enrolled fleet, from the side read beside what it found. */
function scanOf(state) {
  const row = state?.meta?.extras?.inventory_scan?.[0];
  if (!row) return null;
  const n = (value) => (value === null || value === undefined ? 0 : Number(value));
  return Object.freeze({ enrolled: n(row.devices_enrolled), scanned: n(row.scanned), notScanned: n(row.not_scanned), switchedOff: n(row.switched_off) });
}

/** The devices whose inventory scan is not reporting, out of the enrolled fleet, opening Devices. */
function notScannedTile(scan) {
  const options = { href: '#devices', moreLabel: 'Devices' };
  if (!scan) return tile('Not scanned', { kind: 'absent', text: '—' }, 'Where the inventory scan stands could not be read.', options);
  const off = scan.switchedOff > 0 ? ` ${plural(scan.switchedOff, 'more has', 'more have')} it switched off by policy.` : '';
  return tile('Not scanned', { kind: 'number', text: formatCount(scan.notScanned) },
    `of ${plural(scan.enrolled, 'enrolled device', 'enrolled devices')}: the inventory scan has not reported in the last 24 hours, so what they hold is not known.${off}`,
    options);
}

/**
 * Insights, Discovered: every AI application, CLI, IDE extension, local model and inference
 * endpoint device discovery found in the window, whether or not it sent prompts, and the devices
 * whose inventory scan is not reporting, where a tool can be present and not found.
 *
 * @param {object} input
 * @param {object} input.found      view state for what discovery found, with the inventory scan beside it
 * @param {object} [input.prompts]  view state for the prompts per tool in the window
 */
export function discoveredView({ found, prompts = null }, { preset = 'd7', link = toolHref } = {}) {
  const tools = foundTools(found);
  const prompted = promptsByTool(prompts);
  const reporting = found.coverage?.devices_reporting ?? null;
  const table = foundTable(tools, {
    reporting: reporting === null ? null : Number(reporting),
    link: link ? (fingerprint) => link(fingerprint, preset) : null,
    prompted,
    truncated: Boolean(found.meta?.truncated),
    emptyText: emptyLine(found, 'Discovery found no AI application on a device in this window.'),
  });
  const silent = table.rows.filter(({ row }) => row.prompts === 0).length;
  // A month is read over day rows, so its count is the busiest day's.
  const busiest = found.meta?.applied_bucket === 'hour' ? 'hour' : 'day';
  const notes = [
    `Devices counts the devices that reported a finding on its busiest ${busiest} in the window, out of the devices reporting now; a device can hold a tool without it being found that ${busiest}.`,
    'First seen is the first day discovery found the application on any device, before this window or in it.',
    prompted
      ? 'Prompts counts what each application sent in the window under any of its fingerprints; a row with prompts opens its Tools row.'
      : 'The prompts each application sent could not be read, so the Prompts column is empty.',
  ];
  return screen('insights', 'Insights', null, 'mart.agg_discovery_period', {
    subtitle: 'Discovered: the AI applications, CLIs, IDE extensions, local models and inference endpoints found on devices, whether or not they sent prompts.',
    tiles: Object.freeze([
      tools.length > 0
        ? tile('Found on devices', { kind: 'number', text: formatCount(tools.length) }, `${tools.length === 1 ? 'application' : 'applications'} found in this window`)
        : tile('Found on devices', { kind: 'absent', text: '—' }, absentNote(found)),
      prompted
        ? tile('Not sending prompts', { kind: 'number', text: formatCount(silent) }, 'found on a device but sent no prompt in this window')
        : tile('Not sending prompts', { kind: 'absent', text: '—' }, 'The prompts could not be read for this window.'),
      notScannedTile(scanOf(found)),
    ]),
    blocks: Object.freeze([table]),
    series: Object.freeze([]),
    tables: Object.freeze([]),
    ...shared(found, notes),
  });
}

// ---------------------------------------------------------------------------------------------
// Insights: Demand
// ---------------------------------------------------------------------------------------------

/** The tools across the top of the teams table; the seats table lists every one. */
const DEMAND_TOOLS_SHOWN = 8;
/** The teams a seats row names before the rest are counted. */
const DEMAND_TEAMS_NAMED = 3;

/**
 * The demand side read, split into one entry per tool not sanctioned (its people across everyone,
 * those in a team, and per team) and the teams that use any of them. Null when the read did not
 * carry it.
 */
function demandOf(state) {
  const rows = state?.meta?.extras?.demand;
  if (!Array.isArray(rows)) return null;
  const n = (value) => (value === null || value === undefined ? null : Number(value));
  const tools = new Map();
  const teams = new Map();
  for (const row of rows) {
    const key = String(row.tool);
    const tool = tools.get(key) ?? { row, people: null, inTeams: null, teams: new Map() };
    if (row.team_id === null || row.team_id === undefined) {
      tool.people = n(row.people);
      tool.inTeams = n(row.people_in_teams);
    } else {
      const team = teams.get(row.team_id) ?? { id: String(row.team_id), name: String(row.team_name ?? row.team_id), size: n(row.team_size), total: 0 };
      team.total += n(row.people) ?? 0;
      teams.set(team.id, team);
      tool.teams.set(team.id, n(row.people));
    }
    tools.set(key, tool);
  }
  const byPeople = (a, b) => (b.people ?? -1) - (a.people ?? -1) || toolName(a.row).localeCompare(toolName(b.row));
  return Object.freeze({
    tools: [...tools.entries()].map(([key, tool]) => ({ key, ...tool })).sort(byPeople),
    teams: [...teams.values()].sort((a, b) => b.total - a.total || a.name.localeCompare(b.name)),
  });
}

/** How many teams there are, from the side read beside the demand; null when it did not come back. */
function teamCountOf(state) {
  const row = state?.meta?.extras?.team_coverage?.[0];
  return row && row.teams !== null && row.teams !== undefined ? Number(row.teams) : null;
}

/**
 * Every tool not sanctioned that people used in the window, by the people who used it, which is
 * the seats a licence would need: the people across everyone, how many of them are in no team,
 * and the teams they are in. Each opens its Tools row.
 */
function seatsTable(demand, teamNames, { link, emptyText }) {
  const rows = (demand?.tools ?? []).map((tool) => {
    const named = [...tool.teams.entries()]
      .filter(([, people]) => people > 0)
      .sort((a, b) => b[1] - a[1] || teamNames.get(a[0]).localeCompare(teamNames.get(b[0])))
      .map(([id, people]) => `${teamNames.get(id)} ${formatCount(people)}`);
    const shown = named.slice(0, DEMAND_TEAMS_NAMED).join(', ');
    return Object.freeze({
      tool_cell: toolCell(tool.row),
      people: tool.people,
      no_team: tool.people === null || tool.inTeams === null ? null : tool.people - tool.inTeams,
      teams: named.length > DEMAND_TEAMS_NAMED ? `${shown} and ${formatCount(named.length - DEMAND_TEAMS_NAMED)} more` : shown || null,
      href: link ? link(String(tool.row.tool)) : null,
    });
  });
  const top = demand?.tools?.[0];
  return Object.freeze({
    kind: 'table',
    ...tableFrom({ data: rows }, {
      title: 'Seats to license',
      columns: [
        column('tool_cell', 'Application', 'stack'),
        column('people', 'People', 'measure'),
        column('no_team', 'In no team', 'measure'),
        column('teams', 'Teams they are in'),
      ],
      emptyText,
    }),
    ...(top && top.people !== null
      ? { summary: `${plural(rows.length, 'tool not sanctioned was', 'tools not sanctioned were')} used in this window; ${toolName(top.row)} had the most people, ${formatCount(top.people)}${top.teams.size > 0 ? ` in ${plural(top.teams.size, 'team', 'teams')}` : ''}.` }
      : {}),
    rowHref: 'href',
    breakdowns: false,
  });
}

/**
 * Teams down the side and the tools not sanctioned with the most people across the top: each cell
 * is the team's members who used the tool, out of the team's size. Each team opens its page.
 */
function teamsByToolTable(demand, { link, emptyText, href = null }) {
  const tools = (demand?.tools ?? []).slice(0, DEMAND_TOOLS_SHOWN);
  const rows = (demand?.teams ?? []).map((team) => Object.freeze({
    team_cell: Object.freeze({ primary: team.name, secondary: team.size === null ? null : plural(team.size, 'member', 'members') }),
    ...Object.fromEntries(tools.map((tool, i) => [`tool_${i}`, Object.freeze({ count: tool.teams.get(team.id) ?? 0, of: team.size })])),
    href: link ? link(team.id) : null,
  }));
  return Object.freeze({
    kind: 'table',
    ...tableFrom({ data: rows }, {
      title: 'Tools not sanctioned, by team',
      // A tool the catalogue does not name heads its column by its fingerprint, which tells two apart.
      columns: [column('team_cell', 'Team', 'stack'), ...tools.map((tool, i) => column(`tool_${i}`, toolName(tool.row) === UNRECOGNISED_TOOL ? String(tool.row.tool) : toolName(tool.row), 'share'))],
      emptyText,
    }),
    ...(href ? { href, moreLabel: 'Teams' } : {}),
    rowHref: 'href',
    breakdowns: false,
  });
}

/**
 * Insights, Demand: the tools not sanctioned, by team, and the seats each would need: what to
 * license, and for whom. People are counted, never named.
 *
 * @param {object} state  view state for teams crossed with tools, with the demand beside it
 */
export function demandView(state, { preset = 'd7', tools = toolHref, teams = teamHref } = {}) {
  const demand = demandOf(state);
  const teamCount = teamCountOf(state);
  const unread = 'The demand could not be read for this window.';
  const teamNames = new Map((demand?.teams ?? []).map((team) => [team.id, team.name]));
  const seats = seatsTable(demand, teamNames, {
    link: tools ? (fingerprint) => tools(fingerprint, preset) : null,
    emptyText: demand ? emptyLine(state, 'No tool that is not sanctioned was used in this window.') : unread,
  });
  let byTeamEmpty = unread;
  if (demand && teamCount === 0) byTeamEmpty = 'No team exists yet, so demand cannot be shown per team. Teams are added on the Teams page.';
  else if (demand) byTeamEmpty = 'No team member used a tool that is not sanctioned in this window.';
  const byTeam = teamsByToolTable(demand, {
    link: teams ? (team) => teams(team, preset) : null,
    emptyText: byTeamEmpty,
    href: teamCount === 0 ? '#teams' : null,
  });
  const top = demand?.tools?.[0];
  const notes = [
    'People counts everyone who sent the tool a prompt in the window once, under any of its fingerprints: the seats a licence would need.',
    'A tool not sanctioned is one marked unsanctioned or with no decision yet.',
    ...(demand && demand.tools.length > DEMAND_TOOLS_SHOWN
      ? [`The ${DEMAND_TOOLS_SHOWN} tools with the most people are shown by team; Seats to license lists all ${formatCount(demand.tools.length)}.`]
      : []),
  ];
  return screen('insights', 'Insights', null, 'mart.agg_team_period', {
    subtitle: 'Demand: the tools not sanctioned that people use, by team, and the seats each would need.',
    tiles: Object.freeze([
      demand
        ? tile('Tools not sanctioned', { kind: 'number', text: formatCount(demand.tools.length) }, 'unsanctioned or with no decision yet, used in this window')
        : tile('Tools not sanctioned', { kind: 'absent', text: '—' }, unread),
      demand && teamCount !== null && teamCount > 0
        ? tile('Teams using them', { kind: 'number', text: formatCount(demand.teams.length) }, `of ${plural(teamCount, 'team', 'teams')} have a member using one`)
        : tile('Teams using them', { kind: 'absent', text: '—' }, teamCount === 0 ? 'No team exists yet.' : unread, teamCount === 0 ? { href: '#teams', moreLabel: 'Teams' } : {}),
      top && top.people !== null
        ? tile('Most seats', { kind: 'number', text: formatCount(top.people) }, `people use ${toolName(top.row)}, the most of any tool not sanctioned`)
        : tile('Most seats', { kind: 'absent', text: '—' }, demand ? 'No tool that is not sanctioned was used.' : unread),
    ]),
    blocks: Object.freeze([seats, byTeam]),
    series: Object.freeze([]),
    tables: Object.freeze([]),
    ...shared(state, notes),
  });
}

// ---------------------------------------------------------------------------------------------
// Insights: Coding agents
// ---------------------------------------------------------------------------------------------

const AGENT_SUMS = Object.freeze(['requests', 'actions', 'input_tokens', 'output_tokens']);

/** The sums of an agent read's rows, and the most people any one of them counted. */
function agentTotals(rows, people) {
  const totals = {};
  for (const row of rows) {
    for (const measure of [...AGENT_SUMS, people]) {
      const value = measureOf(row, measure);
      if (value.kind !== 'number') continue;
      totals[measure] = measure === people ? Math.max(totals[measure] ?? 0, value.value) : (totals[measure] ?? 0) + value.value;
    }
  }
  const of = (measure) => totals[measure] ?? null;
  return Object.freeze({ requests: of('requests'), actions: of('actions'), input: of('input_tokens'), output: of('output_tokens'), people: of(people) });
}

/** An agent read folded by tool across its models, most input tokens first. */
function agentTools(state) {
  const tools = new Map();
  for (const row of state?.data ?? []) {
    const key = String(row.tool);
    const tool = tools.get(key) ?? { key, row, rows: [] };
    tool.rows.push(row);
    tools.set(key, tool);
  }
  return [...tools.values()]
    .map((tool) => Object.freeze({ ...tool, ...agentTotals(tool.rows, 'tool_users') }))
    .sort((a, b) => (b.input ?? -1) - (a.input ?? -1) || toolName(a.row).localeCompare(toolName(b.row)));
}

/**
 * An agent read folded by model, most input tokens first, with the tools that called it. The
 * agents' actions name no model and are left out; a request whose model the agent did not report
 * is kept, as a row of its own.
 */
function agentModels(state) {
  const models = new Map();
  for (const row of state?.data ?? []) {
    const name = String(row.model ?? '');
    if (name === '' && (measureOf(row, 'requests').value ?? 0) === 0) continue;
    const model = models.get(name) ?? { name, rows: [], tools: new Set() };
    model.rows.push(row);
    model.tools.add(toolName(row));
    models.set(name, model);
  }
  return [...models.values()]
    .map((model) => Object.freeze({ ...model, ...agentTotals(model.rows, 'users') }))
    .sort((a, b) => (b.input ?? -1) - (a.input ?? -1) || a.name.localeCompare(b.name));
}

/**
 * Each coding agent that reported in the window: the people who used it, the actions it took, the
 * tokens it sent and received, and how its input tokens moved against the period before.
 */
function agentToolsTable(tools, before, { period, emptyText }) {
  const rows = tools.map((tool) => {
    const earlier = before ? before.get(tool.key)?.input ?? 0 : null;
    const change = earlier === null || tool.input === null ? null : changeOf({ kind: 'number', value: tool.input }, { kind: 'number', value: earlier }, period);
    return Object.freeze({
      tool_cell: toolCell(tool.row),
      people: tool.people,
      actions: tool.actions,
      input_tokens: tool.input,
      output_tokens: tool.output,
      change: change?.text ?? null,
    });
  });
  return Object.freeze({
    kind: 'table',
    ...tableFrom({ data: rows }, {
      title: 'By tool',
      columns: [
        column('tool_cell', 'Tool', 'stack'),
        column('people', 'People', 'measure'),
        column('actions', 'Agent actions', 'measure'),
        column('input_tokens', 'Input tokens', 'measure'),
        column('output_tokens', 'Output tokens', 'measure'),
        column('change', 'Change in input tokens'),
      ],
      emptyText,
    }),
    breakdowns: false,
  });
}

/** Each model the agents called: the tools that called it, the input tokens it received, and its people. */
function agentModelsTable(models, { emptyText }) {
  const rows = models.map((model) => Object.freeze({
    model: model.name || 'Not reported by the agent',
    tools: [...model.tools].sort((a, b) => a.localeCompare(b)).join(', '),
    input_tokens: model.input,
    people: model.people,
  }));
  const top = models.find((model) => model.name !== '' && model.input !== null);
  const reported = models.filter((model) => model.name !== '').length;
  return Object.freeze({
    kind: 'table',
    ...tableFrom({ data: rows }, {
      title: 'By model',
      columns: [
        column('model', 'Model'),
        column('tools', 'Used by'),
        column('input_tokens', 'Input tokens', 'measure'),
        column('people', 'People', 'measure'),
      ],
      emptyText,
    }),
    ...(top ? { summary: `${plural(reported, 'model', 'models')} received code and context from coding agents in this window; ${top.name} received the most.` } : {}),
    breakdowns: false,
  });
}

/** A count tile with its change against the period before, when that period was read. */
function agentTile(label, value, previous, note, period) {
  if (value === null) return null;
  const change = previous === null ? null : changeOf({ kind: 'number', value }, { kind: 'number', value: previous }, period);
  return tile(label, { kind: 'number', text: formatCount(value) }, change ? `${note}. ${change.text}.` : note);
}

/**
 * Insights, Coding agents: what coding agents send, and to which models. A coding agent reports
 * only while it exports its own telemetry, so a tool with no row is unreported, not idle.
 *
 * @param {{agents: object, previous?: object|null}} states  the window's agent read and the period before's
 */
export function agentsView({ agents, previous = null }, { preset = 'd7' } = {}) {
  const period = previousPeriod(preset);
  const before = comparable(previous);
  const tools = agentTools(agents);
  const models = agentModels(agents);
  const beforeTools = before ? new Map(agentTools(before).map((tool) => [tool.key, tool])) : null;
  const total = agentTotals(agents.data, 'tool_users');
  const earlier = before ? agentTotals(before.data, 'tool_users') : null;
  const fallback = (label) => tile(label, { kind: 'absent', text: '—' }, absentNote(agents));
  const exporting = ENDPOINT_TOOLS.filter((tool) => tool.otel).map((tool) => tool.label);
  const silent = ENDPOINT_TOOLS.filter((tool) => !tool.otel && tool.hooks).map((tool) => tool.label);
  const names = (list) => (list.length > 1 ? `${list.slice(0, -1).join(', ')} and ${list[list.length - 1]}` : list.join(''));
  const notes = [
    `${names(exporting)} report here while their OpenTelemetry switch is on in Settings${silent.length > 0 ? `; ${names(silent)} ${silent.length === 1 ? 'has' : 'have'} no telemetry the device can collect, so ${silent.length === 1 ? 'its' : 'their'} prompts are on Tools but not ${silent.length === 1 ? 'its' : 'their'} tokens` : ''}. A tool with no row reported nothing in this window.`,
    'Input tokens are the code and context an agent sent to a model; agent actions are the tool calls it made, such as running a command or editing a file.',
    `People counts the most people on any one ${agents.meta?.applied_bucket === 'hour' ? 'hour' : 'day'} in the window: a floor for the window, as on Tools.`,
    before
      ? `Each change compares this window with ${period}.`
      : `${period[0].toUpperCase()}${period.slice(1)} could not be read, so no change is shown.`,
  ];
  const reported = models.filter((model) => model.name !== '').length;
  return screen('insights', 'Insights', null, 'mart.agg_agent_period', {
    subtitle: 'Coding agents: what coding agents send, and to which models.',
    tiles: Object.freeze([
      agentTile('Input tokens', total.input, earlier ? earlier.input ?? 0 : null, 'code and context sent to models in this window', period) ?? fallback('Input tokens'),
      agentTile('Agent actions', total.actions, earlier ? earlier.actions ?? 0 : null, 'tool calls the agents made in this window', period) ?? fallback('Agent actions'),
      tools.length > 0
        ? tile('Models', { kind: 'number', text: formatCount(reported) }, `called by ${plural(tools.length, 'coding agent', 'coding agents')} in this window`)
        : fallback('Models'),
    ]),
    blocks: Object.freeze([
      agentToolsTable(tools, beforeTools, { period, emptyText: emptyLine(agents, 'No coding agent reported in this window.') }),
      agentModelsTable(models, { emptyText: emptyLine(agents, 'No coding agent called a model in this window.') }),
    ]),
    series: Object.freeze([]),
    tables: Object.freeze([]),
    ...shared(agents, notes),
  });
}

// ---------------------------------------------------------------------------------------------
// The ten questions
// ---------------------------------------------------------------------------------------------

/** The chart's title names the bucket the series was served in. */
function seriesTitle(state, what = 'Prompts') {
  const bucket = state.meta?.applied_bucket ?? state.meta?.requested_bucket ?? null;
  return bucket ? `${what} per ${bucket}` : `${what} per bucket`;
}

/** A filtered tools list with nothing in it, by the sanction it was filtered to. */
const NONE_WITH = Object.freeze({
  sanctioned: 'No sanctioned tool was in use in this window.',
  unsanctioned: 'No unsanctioned tool was in use in this window.',
  unknown: 'Every tool in use in this window has a sanction decision.',
});

/**
 * Every AI tool seen in the window, as one ranked list: a tool is one line with its share, its
 * people, its blocks and its trend, and opens its own screen. The per-bucket cells are folded
 * into the window; the chart below is the window over time. A sanction filter narrows everything
 * but the split of tools by sanction, which is what it chooses from.
 */
export function toolsView(state, { link = toolHref, preset = null, sanction = null } = {}) {
  const linkTo = link ? (fingerprint) => link(fingerprint, preset) : null;
  const decisionOf = (row) => (SANCTION_PARTS.includes(row.sanctioned_state) ? row.sanctioned_state : 'unknown');
  const shown = sanction ? Object.freeze({ ...state, data: Object.freeze(state.data.filter((row) => decisionOf(row) === sanction)) }) : state;
  const href = (value) => withPreset(value ? `#tools?sanction=${value}` : '#tools', preset);
  const none = sanction ? NONE_WITH[sanction] : 'No tool was in use in this window.';
  return screen('tools', 'Tools', null, 'mart.v_tool_usage', {
    subtitle: 'Every AI tool seen in this window, by prompts. Open a tool to see who uses it and what was sent.',
    lead: Object.freeze([
      sanctionShare(state, { emptyText: emptyLine(state, 'No tool was detected in this window.') }),
    ]),
    tiles: Object.freeze([
      measureTile('Prompts', shown, 'submissions', formatCount, { trend: trendOf(shown) }),
      measureTile('Blocked', shown, 'blocked'),
      measureTile('Data sent', shown, 'bytes_total', formatBytes),
      attachmentsTile(shown, { preset }),
    ]),
    filters: Object.freeze([Object.freeze({
      label: 'Sanction',
      current: sanction ?? '',
      items: Object.freeze([{ id: '', label: 'All', href: href(null) }, ...SANCTION_PARTS.map((key) => ({ id: key, label: SANCTION_TEXT[key], href: href(key) }))]),
    })]),
    blocks: Object.freeze([
      rankTools(shown, { title: 'Tools by prompts', link: linkTo, emptyText: emptyLine(state, none) }),
    ]),
    tables: Object.freeze([uncataloguedTools(shown, { link: linkTo })].filter(Boolean)),
    // A window with no cell has no shape to draw: the ranking's empty line says why.
    series: Object.freeze(shown.data.length > 0 ? [seriesFrom(shown, { measure: 'submissions', title: seriesTitle(shown) })] : []),
    ...shared(state, [
      'Rank is by prompts with a deterministic tie-break, and is never a sanction signal.',
      'Unknown is its own answer: a tool with no decision renders as unknown, never as unsanctioned.',
      'A tool\'s people is the most counted in any one period, a floor for the window: a person active on two days is counted on both.',
    ]),
  });
}

/**
 * One tool: its figures over the window, who is using it, and the findings it produced. The
 * people are a roster in the API's order, with their prompts; a role that may not see people
 * sees the totals and is told why the roster is missing.
 */
export function toolView({ usage, people = null, findings = null, names = {} }, { tool, preset = null, exploreHref = 'explore.html' }) {
  // The read is filtered to this tool, so its rows are the tool's own, under the id every list
  // names it by, which a link naming one of its fingerprints does not spell.
  const groups = foldCells(usage, 'tool');
  const group = groups.find((g) => g.key === String(tool)) ?? (groups.length === 1 ? groups[0] : null);
  tool = group ? group.key : String(tool);
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
    subtitle: String(tool).startsWith(SITE_PREFIX) ? 'A site the tool catalogue does not name.'
      : name === String(tool) ? 'A tool known only by its fingerprint.' : `Fingerprint ${tool}`,
    badges: Object.freeze(sanction ? [sanctionChip(sanction)] : []),
    actions: Object.freeze([
      Object.freeze({ label: 'Prompts and events', href: `${exploreHref}#events?tool=${encodeURIComponent(String(tool))}` }),
      Object.freeze({ label: 'All tools', href: withPreset('#tools', preset) }),
    ]),
    tiles: Object.freeze([
      foldedTile('Prompts', group, 'submissions', formatCount, { trend: group && group.trend.length > 1 ? group.trend : null }),
      foldedTile('People (lower bound)', group, 'users'),
      foldedTile('Blocked', group, 'blocked'),
      foldedTile('Data sent', group, 'bytes_total', formatBytes),
      attachmentsTile(usage, { preset, filters: { tool: String(tool) } }),
    ]),
    tables: Object.freeze([
      ...(roster ? [tableFrom({ data: roster }, {
        title: 'People using it',
        columns: [
          column('name', 'User', 'person'),
          column('submissions', 'Prompts', 'measure'),
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

/**
 * The people behind the team totals, by team and then by name: a roster with numbers, not a
 * ranking. Null when there was nothing to read; a refusal says why the people are missing.
 */
function teamMembersTable(members, { byTeam = true, title = 'Prompts by person' } = {}) {
  const byName = (a, b) => String(a ?? '').localeCompare(String(b ?? ''), undefined, { sensitivity: 'base' });
  const rows = foldMembers(members.data).sort((a, b) => byName(a.name, b.name) || byName(a.subject, b.subject));
  return Object.freeze({
    title,
    columns: Object.freeze([
      column('name', 'User', 'person'),
      ...(byTeam ? [column('team_names', 'Teams')] : []),
      column('submissions', 'Prompts', 'measure'),
    ]),
    rows: Object.freeze(rows.map((row) => Object.freeze({ row, vocab: vocabOf(row) }))),
    emptyText: byTeam ? 'Nobody in these teams sent a prompt in this window.' : 'Nobody in this team sent a prompt in this window.',
  });
}

/**
 * One row per person from the roster's rows, which come one per team, person and bucket: the
 * person's teams by name, and their prompts in the window. A person's prompts are the same in every
 * team they are in, so they are summed over one team's buckets, not over the teams.
 */
function foldMembers(data) {
  const people = new Map();
  for (const row of data) {
    const subject = row.subject ?? null;
    if (!people.has(subject)) people.set(subject, { subject, name: row.name ?? subject, teams: new Map() });
    const person = people.get(subject);
    const team = row.team ?? row.team_name ?? '';
    const prior = person.teams.get(team);
    person.teams.set(team, {
      name: row.team_name ?? prior?.name ?? null,
      submissions: (prior?.submissions ?? 0) + (Number(row.submissions) || 0),
    });
  }
  return [...people.values()].map(({ subject, name, teams }) => Object.freeze({
    subject,
    name,
    team_names: [...new Set([...teams.values()].map((t) => t.name).filter(Boolean))]
      .sort((a, b) => a.localeCompare(b, undefined, { sensitivity: 'base' })).join(', ') || null,
    submissions: Math.max(0, ...[...teams.values()].map((t) => t.submissions)),
  }));
}

function teamMembersNotes(members) {
  if (!members) return [];
  if (members.isRefusal) {
    return [members.resultState === 'unauthorised_role'
      ? 'Your role does not show each person\'s prompts; an analyst or admin sees them.'
      : `Each person's prompts could not be read: ${members.error?.message ?? members.resultState}.`];
  }
  const notes = ['Each person is listed once, by name, with the teams they are in. Reading this list is audited.'];
  if (members.meta?.truncated) notes.push(`The roster was cut short at ${formatCount(members.data.length)} rows, so some people or days are missing.`);
  if (members.audit?.entry_id) notes.push(`Audited read: entry ${members.audit.entry_id} at ${formatInstant(members.audit.written_at)}.`);
  return notes;
}

const INCLUDE_LABELS = Object.freeze({ group: 'Group', department: 'Department', org_unit: 'Unit', team: 'Team' });

/** How a team is made: its members, the people chosen for it, and the first things it includes. */
function teamFacts(team) {
  if (!team) return null;
  const includes = team.includes ?? [];
  const shown = includes.slice(0, 2).map((i) => `${INCLUDE_LABELS[i.kind]} ${i.name}`.trim());
  if (includes.length > shown.length) shown.push(`${formatCount(includes.length - shown.length)} more`);
  return [
    team.members === null || team.members === undefined ? null : plural(team.members, 'member', 'members'),
    team.people ? plural(team.people, 'person chosen', 'people chosen') : null,
    ...shown,
  ].filter(Boolean).join(' · ') || null;
}

/**
 * The teams, as one ranked list by prompts, each opening its own screen. A team the admin
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
    title: 'Teams by usage',
    href: null,
    emptyText,
    itemLabel: 'Team',
    valueLabel: 'Prompts',
    metaLabels: Object.freeze(['People']),
    trendLabel: 'Trend',
    items: Object.freeze(items),
  });
}

/**
 * The teams: how usage splits across them. Every team is one line that opens its own screen,
 * where the people behind its total are listed.
 *
 * @param {object} state view state for the team totals (Q3)
 * @param {object} [options]
 * @param {boolean} [options.mayManage]   the reader is an admin, who adds teams on this page
 * @param {Array|null}  [options.teams]   the admin's team list, so a quiet team is still listed
 */
export function teamsView(state, { mayManage = false, teams = null, link = teamHref, preset = null } = {}) {
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
    notes.push(mayManage
      ? 'No team exists yet. Add one with + Add a Team: from a group in your identity provider, from people and groups synced over SCIM, or by ticking people.'
      : 'No team exists yet. An admin adds teams on this page.');
  }
  return screen('teams', 'Teams', null, 'mart.agg_team_period', {
    subtitle: 'Usage per team. A team counts the usage of the people in it now; a person in two teams counts in both.',
    tiles: Object.freeze([]),
    blocks: Object.freeze([
      rankTeams(state, { teams, link: (team) => link(team, preset), emptyText: teamCount === 0 ? 'There are no teams yet.' : emptyLine(state, 'No team has usage in this window.') }),
    ]),
    tables: Object.freeze([]),
    series: Object.freeze(state.data.length > 0 ? [seriesFrom(state, { measure: 'submissions', title: seriesTitle(state) })] : []),
    ...shared(state, notes),
  });
}

/**
 * One team: its figures over the window and the people behind them, by name. The team is named
 * by the totals read, or by the roster when the totals carry no row for it.
 */
export function teamView(state, { team, members = null, teams = null, preset = null }) {
  const group = foldCells(state, 'team', { sum: ['submissions'], max: ['users'] }).find((g) => g.key === String(team)) ?? null;
  const known = (teams ?? []).find((t) => t.team_id === String(team)) ?? null;
  const name = String(group?.first.team_name ?? known?.name ?? members?.data?.find((row) => row.team_name)?.team_name ?? 'Team');
  const listed = members && !members.isRefusal ? new Set(members.data.map((row) => row.subject)).size : null;
  const facts = teamFacts(known);
  return screen('team', name, null, 'mart.agg_team_period', {
    subtitle: facts ? `${facts}. A team counts the usage of the people in it now.` : 'One team\'s usage. A team counts the usage of the people in it now.',
    actions: Object.freeze([Object.freeze({ label: 'All teams', href: withPreset('#teams', preset) })]),
    tiles: Object.freeze([
      foldedTile('Prompts', group, 'submissions', formatCount, { trend: group && group.trend.length > 1 ? group.trend : null }),
      foldedTile('People (lower bound)', group, 'users'),
      tile('People listed', listed === null ? { kind: 'absent', text: '—' } : { kind: 'number', text: formatCount(listed) },
        listed === null ? 'The roster could not be read.' : 'Members who sent a prompt in this window.'),
    ]),
    tables: Object.freeze(members && !members.isRefusal ? [teamMembersTable(members, { byTeam: false })] : []),
    series: Object.freeze(group ? [seriesFrom(state, { measure: 'submissions', title: seriesTitle(state) })] : []),
    ...shared(state, teamMembersNotes(members)),
  });
}

export function personView(state, { subject, person = null, preset = null, exploreHref = 'explore.html' }) {
  const series = seriesFrom(state, { measure: 'submissions', title: 'Prompts per day' });
  const flush = state.meta?.extras?.flush_check?.[0] ?? null;
  // Each tool once across the window: the per-day counts cannot be added up into it.
  const tools = state.meta?.extras?.subject_tools?.[0]?.tools ?? null;
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
      measureTile('Prompts', state, 'submissions'),
      tools === null
        ? tile('Tools used', { kind: 'absent', text: '—' }, absentNote(state))
        : tile('Tools used', { kind: 'number', text: formatCount(Number(tools)) }),
      measureTile('Block events', state, 'block_events'),
      measureTile('Bytes', state, 'bytes_total', formatBytes),
      attachmentsTile(state, { preset, filters: { subject } }),
    ]),
    tables: Object.freeze([
      tableFrom(state, {
        title: 'Daily series',
        columns: [
          column('bucket', 'Day', 'instant'),
          column('submissions', 'Prompts', 'measure'),
          column('tools_used', 'Tools', 'measure'),
          column('block_events', 'Blocked', 'measure'),
          column('attachments', 'Attachments', 'measure'),
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
 * The people the directory and the devices know, by name, to open one person's page, each with
 * the teams they are in and their prompts in the window from those teams' rosters. It is a
 * directory, not a ranking: it is in name order, and nothing sorts it by usage.
 */
export function peopleView(state, { search = '', members = null } = {}) {
  const next = state.page?.next_cursor ?? null;
  const query = (extra) => new URLSearchParams({ ...(search ? { q: search } : {}), ...extra }).toString();
  const named = state.data.filter((row) => row.directory_name || row.subject_name).length;
  const roster = members && !members.isRefusal ? new Map(foldMembers(members.data).map((row) => [row.subject, row])) : null;
  const rows = state.data.map((row) => {
    const member = roster?.get(row.subject) ?? null;
    return Object.freeze({ ...row, team_names: member?.team_names ?? null, submissions: member ? member.submissions : null });
  });
  const table = tableFrom({ ...state, data: rows }, {
    title: search ? `People whose name starts with "${search}"` : 'People',
    columns: [
      column('name', 'Name', 'person'),
      column('team_names', 'Teams'),
      column('department', 'Department'),
      column('directory_status', 'Directory', 'vocab'),
      column('last_active_day', 'Last active', 'instant'),
      column('submissions', 'Prompts', 'measure'),
    ],
    emptyText: search ? 'Nobody\'s name starts with that.' : 'Nobody is known yet: people arrive from the directory, or with their first prompt.',
  });
  return screen('person', 'Users', null, 'mart.v_person', {
    subtitle: 'Everyone the directory or a device has named. Open a person to see their usage and prompts.',
    search: Object.freeze({ value: search }),
    tiles: Object.freeze([]),
    tables: Object.freeze([
      Object.freeze({
        ...table,
        breakdowns: false,
        ...(next ? { href: `#person?${query({ cursor: next })}`, moreLabel: 'Next page' } : {}),
      }),
    ]),
    series: Object.freeze([]),
    ...shared(state, [
      ...(state.data.length > 0 && named === 0
        ? ['No name is known for anyone on this page: the tenant keeps people hashed, or no directory has been read yet. Each is listed by reference.']
        : []),
      ...(roster ? ['Teams and prompts come from the rosters of the teams with usage in this window: a person in none of them shows a dash, not a zero.'] : []),
      ...teamMembersNotes(members),
    ]),
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

/** The words beside a device's name: what it runs, whether it is managed, and its agent. */
function deviceFacts(row) {
  const os = [DEVICE_LABELS[row.device_os] ?? (row.device_os ? String(row.device_os) : null), row.os_version ? String(row.os_version) : null].filter(Boolean).join(' ');
  return [
    os || null,
    DEVICE_LABELS[row.managed_state] ?? (row.managed_state ? String(row.managed_state) : null),
    row.agent_version ? `Agent ${row.agent_version}` : null,
  ].filter(Boolean).join(' · ') || null;
}

/** The collection mode a device reported it applied, in words; null when it reported none. */
const modeText = (row) => (row.collection_mode ? COLLECTION_MODE_LABELS[row.collection_mode] ?? `Mode ${row.collection_mode}` : null);

/** One device's page. */
export const deviceHref = (device) => `#devices?device=${encodeURIComponent(String(device))}`;

/** One device's row, in the cells the list and the device's page both show. */
function deviceRow(row, now) {
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
    mode: modeText(row),
    collectors,
    href: row.device ? deviceHref(row.device) : null,
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
}

/**
 * Devices, as a customer reads them: how much of the fleet is reporting, which devices need
 * attention and why, and one row per device: the device and what it runs, its state with when it
 * was last seen, the person on it, and the collection mode it applied. A row opens the device.
 *
 * @param {object} state view state for the device list
 * @param {object} [options]
 * @param {object} [options.filters]    status (attention | reporting | one state), device_os, managed_state
 * @param {Date}   [options.now]        for "last seen"
 */
export function devicesView(state, { filters = {}, now = new Date() } = {}) {
  const all = state.data.map((row) => deviceRow(row, now));
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
            column('mode', 'Applied mode'),
          ],
          emptyText: 'No device matches these filters.',
        }),
        rowHref: 'href',
        breakdowns: false,
      }),
    ]),
    series: Object.freeze([]),
    ...shared(state, [
      'Reporting, quiet, never checked in and revoked are four different facts, and none is inferred from silence.',
      'One row per device. Its status is the worst of its collectors\' states; open a device to see each collector.',
      'Applied mode is the collection mode the device reported it applied, which can trail the mode set in Settings until the device next checks in.',
    ]),
  });
}

/** What a collector reported, in the order the server returned them. Off in policy is not a fault. */
function collectorRows(collectors) {
  return collectors.data.map((r) => {
    const off = r.error_code === 'disabled_by_policy';
    return Object.freeze({
      collector: r.collector ?? null,
      state: off ? 'disabled' : r.collector_state ?? 'unknown',
      state_text: off ? 'Off in policy' : String(r.collector_state ?? 'unknown'),
      // The cause in words, with the device's detail code kept on hover.
      cause: Object.freeze(off || !r.error_code ? { primary: 'None' } : { primary: COLLECTOR_DETAILS[r.error_code] ?? String(r.error_code), title: String(r.error_code) }),
      last_report_at: r.last_report_at ?? null,
    });
  });
}

/**
 * One device: what it runs, its state and the person on it, and every collector it reported, with
 * the cause each gave. A device the fleet list does not hold is said to be unknown.
 *
 * @param {object} input
 * @param {string} input.device       the device id the address names
 * @param {object} input.row          view state for the device's row in the fleet list
 * @param {object} [input.collectors] view state for its collectors
 */
export function deviceView({ device, row, collectors = null }, { now = new Date(), exploreHref = 'explore.html' } = {}) {
  const found = row.data[0] ?? null;
  const d = found ? deviceRow(found, now) : null;
  const actions = Object.freeze([
    Object.freeze({ label: 'Prompts and events', href: `${exploreHref}#events?device=${encodeURIComponent(device)}` }),
    Object.freeze({ label: 'All devices', href: '#devices' }),
  ]);
  if (!d) {
    return screen('device', `${device.slice(0, 8)}…`, null, 'mart.v_device_liveness', {
      subtitle: `Device ${device}`,
      actions,
      tiles: Object.freeze([]),
      tables: Object.freeze([]),
      series: Object.freeze([]),
      ...shared(row, row.isRefusal ? [] : ['No enrolled device has this id.']),
    });
  }
  const read = collectors && !collectors.isRefusal;
  const reported = read ? collectors.data.length : found.collectors_reporting ?? null;
  const tiles = [
    tile('Status', { kind: 'vocab', text: d.status_text, key: d.status }, d.last_seen_at_ago ? `Last seen ${d.last_seen_at_ago}` : 'Never seen'),
    tile('Applied mode', d.mode ? { kind: 'vocab', text: d.mode, key: found.collection_mode } : { kind: 'absent', text: '—' }, d.mode ? null : 'The device has not reported a mode.'),
    tile('Collectors', reported === null ? { kind: 'absent', text: '—' } : { kind: 'number', text: formatCount(count(reported)) }, d.collectors),
  ];
  const notes = [];
  if (collectors?.isRefusal) notes.push(`The collectors could not be read: ${collectors.error?.message ?? collectors.resultState}.`);
  const user = d.user_cell.primary ? `Last used by ${d.user_cell.primary}` : null;
  return screen('device', d.device_cell.primary ?? device, null, 'mart.v_device_liveness', {
    subtitle: [d.device_cell.secondary, user, device].filter(Boolean).join(' · '),
    actions,
    tiles: Object.freeze(tiles),
    tables: Object.freeze(read ? [
      Object.freeze({
        ...tableFrom({ data: collectorRows(collectors) }, {
          title: 'Collectors',
          columns: [
            column('collector', 'Collector'),
            column('state', 'State', 'status'),
            column('cause', 'Cause', 'stack'),
            column('last_report_at', 'Last report', 'instant'),
          ],
          emptyText: 'This device has not reported any collector.',
        }),
        breakdowns: false,
      }),
    ] : []),
    series: Object.freeze([]),
    ...shared(row, [
      ...notes,
      'A collector switched off by policy is off, not a fault. Every other state is the one the device last reported.',
    ]),
  });
}

/** The audit trail's filters, as q10 takes them. */
const AUDIT_FIELDS = Object.freeze([
  Object.freeze({ name: 'actor', label: 'Actor', placeholder: 'Who made the read' }),
  Object.freeze({ name: 'action', label: 'Action', placeholder: 'Such as query.events' }),
  Object.freeze({ name: 'object_type', label: 'Object type', placeholder: 'Such as ingest.submission' }),
  Object.freeze({ name: 'case', label: 'Case', placeholder: 'A case reference' }),
]);

/**
 * The audit trail: what has been accessed and changed, and by whom, newest first, narrowed by the
 * filters given. A page that holds more continues on the next.
 *
 * @param {object} state view state for the audit trail (Q10)
 * @param {object} [options]
 * @param {object} [options.filters] the filters and the cursor in the address
 * @param {string} [options.preset]  the window, kept by the filters and the next page
 */
export function auditView(state, { filters = {}, preset = null } = {}) {
  const given = Object.fromEntries(AUDIT_FIELDS.map((f) => [f.name, typeof filters[f.name] === 'string' ? filters[f.name] : '']).filter(([, v]) => v !== ''));
  const window = preset ? { preset } : {};
  const at = (extra) => {
    const query = new URLSearchParams({ ...window, ...extra }).toString();
    return `#audit${query ? `?${query}` : ''}`;
  };
  const next = state.page?.next_cursor ?? null;
  const filtered = Object.keys(given).length > 0;
  return screen('audit', 'Audit trail', TEMPLATES.q10_audit_trail.title, 'ops.audit', {
    subtitle: 'What has been accessed, and by whom. Reading this log is itself a subject-level read.',
    form: Object.freeze({
      screen: 'audit',
      fields: Object.freeze(AUDIT_FIELDS.map((f) => Object.freeze({ ...f, value: given[f.name] ?? '' }))),
      clearHref: filtered ? at({}) : null,
    }),
    tiles: Object.freeze([
      tile('Entries in page', { kind: 'number', text: formatCount(state.data.length) }, null),
      tile('Hash links', { kind: 'vocab', text: state.resultState === 'audit_chain_broken' ? 'broken' : 'verified in this page' }, 'Whole-chain verification is the reconciler\'s job, not a page\'s.'),
      tile('Anchored head', { kind: 'absent', text: 'not exposed' }, 'This API returns no anchor head or anchor time; the dashboard cannot show the last anchored entry.'),
    ]),
    tables: Object.freeze([
      Object.freeze({
        ...tableFrom(state, {
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
          emptyText: filtered ? 'Nothing matches these filters in this window.' : 'Nothing has been accessed in this window.',
        }),
        ...(next ? { href: at({ ...given, cursor: next }), moreLabel: 'Next page' } : {}),
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
