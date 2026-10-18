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
import { formatBytes, formatCount, formatInstant, formatLabels, formatPercent, formatRate, formatWhen, plural } from './format.js';
import { SOURCES, TEMPLATES, UNRECOGNISED_TOOL, SITE_PREFIX, COLLECTION_MODE_LABELS, COLLECTION_MODE_CODES, COLLECTOR_DETAILS, COLLECTORS, DATA_CLASS_NAMES, WINDOWS, AUDIT_ACTIONS, AUDIT_REPORTS } from './vocab.js';
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
 * A tile: a label, a value and a short note under it. It may carry an address, or the title of the
 * block on its own page that holds its rows (`jump`); what the figure counts, when the label alone
 * does not say, as `info`; the shape of its series; a split of its value into parts that each open
 * the rows they count; and how it moved against the period before, as `change`.
 */
function tile(label, value, note, { href = null, jump = null, info = null, trend = null, split = null, moreLabel = null, change = null } = {}) {
  return Object.freeze({
    label,
    value,
    note: note ?? null,
    ...(href ? { href } : {}),
    ...(jump ? { jump } : {}),
    ...(info ? { info } : {}),
    ...(trend ? { trend } : {}),
    ...(split ? { split } : {}),
    ...(moreLabel ? { moreLabel } : {}),
    ...(change ? { change } : {}),
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

function column(key, label, kind = 'text', extra = {}) {
  return Object.freeze({ key, label, kind, ...extra });
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

/** Why a count of users is a floor: the most any one cell counted, not a sum. */
const USERS_FLOOR = 'The most users seen in any one period of the window, never a sum across periods.';

/**
 * The most users one cell counted. It is a floor for the distinct users in the window; the sum
 * of the cells is not, because a user using two tools, or active on two days, is in two cells.
 */
export function peopleTile(state, label, options = {}) {
  let most = null;
  for (const row of state.data) {
    const value = measureOf(row, 'users');
    if (value.kind === 'number' && (most === null || value.value > most)) most = value.value;
  }
  if (most === null) return tile(label, { kind: 'absent', text: '—' }, absentNote(state), options);
  return tile(label, { kind: 'number', text: formatCount(most) }, null, { ...options, info: USERS_FLOOR });
}

/** Who a count of users is out of, in words. */
const PEOPLE_BASIS = Object.freeze({
  directory: 'active users in the directory',
  devices: 'users signed in on an enrolled device',
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
 * Who the users counted are out of, in words. Users not in the directory can send prompts, so the
 * count can exceed it; it is then beside the directory, not a part of it.
 */
function outOf(counted) {
  if (counted?.of === null || counted?.of === undefined) return null;
  const whole = `${formatCount(counted.of)} ${PEOPLE_BASIS[counted.basis]}`;
  return (counted.people ?? 0) <= counted.of ? `of ${whole}` : `more than the ${whole}`;
}

/** The users who sent a prompt in the window, each once, out of the users who could have; the busiest period's count when the read did not count them. */
function usersTile(state, options) {
  const counted = peopleOf(state);
  if (counted?.people === null || counted?.people === undefined) return peopleTile(state, 'Users', options);
  return tile('Users', { kind: 'number', text: formatCount(counted.people) }, outOf(counted), {
    ...options,
    info: 'Everyone who sent a prompt in this window, each counted once however many tools they used.',
  });
}

/** A tile from one group's folded measure. */
function foldedTile(label, group, measure, formatter = formatCount, options = {}) {
  const value = group ? foldedMeasure(group, measure) : { kind: 'absent' };
  if (value.kind === 'number') return tile(label, { kind: 'number', text: formatter(value.value) }, null, options);
  return tile(label, { kind: 'absent', text: '—' }, group ? NO_FIGURE : NOTHING_RECORDED, options);
}

/** The fact beside a ranked item that counts users: its noun agrees with the count. */
const peopleFact = (value) => Object.freeze({ label: value.kind === 'number' && value.value === 1 ? 'user' : 'users', value });

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
    metaLabels: Object.freeze(['Users', 'Blocked', 'Data sent']),
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
        column('users', 'Users', 'measure'),
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
export function sanctionShare(state, { title = 'Tools by decision', emptyText = 'No tool was detected in this window.', settingsHref = '#sanction' } = {}) {
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
    note: undecided ? 'No tool has a sanction decision yet.' : null,
    info: 'The tools detected in this window, by their sanction decision. A tool with no decision is neither sanctioned nor unsanctioned.',
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
  const silent = reporting !== null && enrolled !== null ? Math.max(0, enrolled - reporting) : null;
  return tile('Devices reporting',
    reporting === null ? { kind: 'absent', text: '—' } : { kind: 'number', text: formatCount(reporting) },
    enrolled === null ? 'The enrolled count could not be read.' : `of ${formatCount(enrolled)} enrolled${silent ? ` · ${formatCount(silent)} not reporting` : ''}`,
    {
      href: '#devices',
      info: 'Enrolled devices whose agent has checked in recently, out of every device enrolled.',
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
const ATTACHMENTS_INFO = 'Files attached to prompts in this window.';

/**
 * Every file attached to a prompt in the window, opening the prompts that carried one, narrowed by
 * the filters the page is read under. A prompt sent at metadata-only collection cannot describe an
 * attachment, so its count is unknown rather than zero: the tile says how many prompts it counted
 * on, and a window with none says so.
 */
function attachmentsTile(usage, { preset, filters = {}, change = null }) {
  const attachments = sumMeasure(usage, 'attachments');
  const described = sumMeasure(usage, 'content_prompts');
  const prompts = sumMeasure(usage, 'submissions');
  const link = { ...searchEventsLink({ ...filters, attachments: 'yes' }, preset), change };
  if (attachments.kind !== 'number' || described.kind !== 'number') return tile('Attachments', { kind: 'absent', text: '—' }, absentNote(usage), { ...link, info: ATTACHMENTS_INFO });
  if (described.value === 0 && prompts.kind === 'number' && prompts.value > 0) {
    return tile('Attachments', { kind: 'absent', text: '—' }, 'Not collected', {
      info: `Every prompt in this window was sent at ${METADATA_ONLY}, which does not describe attachments.`,
    });
  }
  const partly = prompts.kind === 'number' && described.value < prompts.value;
  return tile('Attachments', { kind: 'number', text: formatCount(attachments.value) },
    partly ? `on ${formatCount(described.value)} of ${formatCount(prompts.value)} prompts` : null,
    {
      ...link,
      info: partly
        ? `${ATTACHMENTS_INFO} Only the prompts sent above ${METADATA_ONLY} describe their attachments, so the rest are not counted.`
        : ATTACHMENTS_INFO,
    });
}

/**
 * The prompts that matched an enforcement rule on one of their labels, out of every prompt, split by
 * what happened to them. It opens where they are broken down.
 */
function sensitiveTile(usage, { href, change = null }) {
  const sensitive = sumMeasure(usage, 'sensitive');
  const prompts = sumMeasure(usage, 'submissions');
  const blocked = sumMeasure(usage, 'sensitive_blocked');
  const options = { href, change, info: 'Prompts that matched an enforcement rule on one of their labels, and how many of them the rule blocked.' };
  if (sensitive.kind !== 'number') return tile('Sensitive prompts', { kind: 'absent', text: '—' }, absentNote(usage), options);
  const parts = [
    prompts.kind === 'number' ? `of ${formatCount(prompts.value)} prompts` : null,
    blocked.kind === 'number' && sensitive.value > 0 ? `${formatCount(blocked.value)} blocked` : null,
  ].filter(Boolean);
  return tile('Sensitive prompts', { kind: 'number', text: formatCount(sensitive.value) }, parts.length > 0 ? parts.join(' · ') : null, {
    ...options,
    trend: trendOf(usage, 'sensitive'),
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
export function postureView({ devices, tools = null, previous = null, rules = null, ruleMatches = null, coverage }, { preset = null, activity = true } = {}) {
  const cov = devices?.coverage ?? coverage ?? null;
  const usage = tools ?? NOTHING;
  const at = (hash) => withPreset(hash, preset);
  // Each figure moves against the same window before it, when that window was read.
  const prior = tools ? comparable(previous) : null;
  const period = previousPeriod(preset ?? 'd7');
  const change = (measure) => (prior ? changeOf(sumMeasure(usage, measure), previousTotal(prior, measure), period) : null);
  const usersNow = peopleOf(usage);
  const usersBefore = peopleOf(prior);
  const usersChange = prior && typeof usersNow?.people === 'number' && typeof usersBefore?.people === 'number'
    ? changeOf({ kind: 'number', value: usersNow.people }, { kind: 'number', value: usersBefore.people }, period)
    : null;

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
      measureTile('Prompts', usage, 'submissions', formatCount, { trend: trendOf(usage), href: at('#tools'), change: change('submissions') }),
      attachmentsTile(usage, { preset, change: change('attachments') }),
      // Activity breaks the sensitive prompts down; where it is not offered, the rules that
      // matched them are.
      sensitiveTile(usage, { href: activity ? at('#activity') : '#rules', change: change('sensitive') }),
      usersTile(usage, { href: at('#person'), change: usersChange }),
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

/** How a count moved against the period before, as a figure: "+12%", "0%", or "New" for a count that was nothing. */
function changeFigure(current, before) {
  if (before === 0) return current === 0 ? '0%' : 'New';
  const ratio = (current - before) / before;
  if (Math.abs(ratio) < 0.005) return '0%';
  return `${ratio > 0 ? '+' : '−'}${Math.max(1, Math.round(Math.abs(ratio) * 100))}%`;
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
    cardJump: 'Rules',
    info: 'Prompts, the sensitive ones among them, and what happened to those. A prompt is sensitive when the rule it matched names one of its labels; the Rules table below counts them by rule.',
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

/** The group a tool is listed under on Policy → Tools: the undecided tools in use first, then the rest, then the tools not seen. */
const IN_USE_UNDECIDED = 'In use, no decision yet';
const IN_USE = 'In use';
const UNOBSERVED = 'Unobserved in the last 30 days';

/** The catalogue tool each fingerprint belongs to: by the fingerprints the catalogue lists, and by the app key itself. */
function fingerprintIndex(catalogue) {
  const index = new Map();
  for (const tool of catalogue) {
    index.set(`app:${tool.tool_key}`, tool);
    for (const fingerprint of tool.fingerprints ?? []) index.set(String(fingerprint), tool);
  }
  return index;
}

/** The rules whose match names a tool, by the tool's catalogue key. */
function rulesNaming(rules) {
  const known = rules && !rules.isRefusal ? rules.data : null;
  if (!known) return null;
  const byTool = new Map();
  for (const rule of known) {
    for (const key of rule.match?.tools ?? []) {
      if (!byTool.has(key)) byTool.set(key, []);
      byTool.get(key).push(String(rule.rule));
    }
  }
  return byTool;
}

/**
 * Policy → Tools: one table, the decision in the row beside what it is decided on. Each tool in use
 * over the last 30 days has its users out of everyone, its prompts, the sensitive ones among them,
 * the rules that name it and its sanction decision; the tools waiting for a decision come first. The
 * catalogue tools no device has seen are folded away under their own heading, each with every
 * fingerprint it covers. An admin decides in the row; a tool outside the catalogue has no decision to
 * make and says so. A tool with no decision is unknown, never unsanctioned.
 *
 * @param {object} state view state for tool usage over the last 30 days (Q1)
 * @param {object} [options]
 * @param {object} [options.rules]      view state for the enforcement rules, for the rules naming each tool
 * @param {Array}  [options.catalogue]  the catalogue as Settings holds it: tool_key, display_name, sanctioned_state, fingerprints
 * @param {object} [options.decisions]  whether the reader may decide, and the decision a write is pending on
 */
export function sanctionView(state, { link = toolHref, rules = null, catalogue = null, decisions = null } = {}) {
  const decided = (value) => (DECISION_ORDER.includes(value) ? value : 'unknown');
  const byName = (a, b) => a.localeCompare(b, undefined, { sensitivity: 'base' });
  const naming = rulesNaming(rules);
  const counted = peopleOf(state);
  const index = catalogue ? fingerprintIndex(catalogue) : null;
  // The usage is read per fingerprint; the catalogue folds the fingerprints of one tool into one row.
  const groups = foldCells(state, 'tool');
  const folded = new Map();
  const loose = [];
  for (const group of groups) {
    const tool = index?.get(String(group.key)) ?? null;
    if (!tool) {
      loose.push(group);
      continue;
    }
    if (!folded.has(tool.tool_key)) folded.set(tool.tool_key, []);
    folded.get(tool.tool_key).push(group);
  }
  const observed = [
    ...[...folded.entries()].map(([key, parts]) => {
      const tool = catalogue.find((t) => t.tool_key === key);
      const first = { tool: `app:${key}`, tool_name: tool.display_name ?? key, sanctioned_state: decided(tool.sanctioned_state) };
      return { tool, group: parts.length === 1 ? { ...parts[0], first: { ...parts[0].first, ...first } } : mergeGroups(parts, parts[0].key, first) };
    }),
    ...loose.map((group) => ({ tool: null, group })),
  ];
  const decisionOf = (entry) => (entry.tool ? decided(entry.tool.sanctioned_state) : decided(entry.group.first.sanctioned_state));
  const nameOf = (entry) => (entry.tool ? entry.tool.display_name ?? entry.tool.tool_key : toolName(entry.group.first));
  observed.sort((a, b) => DECISION_ORDER.indexOf(decisionOf(a)) - DECISION_ORDER.indexOf(decisionOf(b)) || byName(nameOf(a), nameOf(b)) || a.group.key.localeCompare(b.group.key));
  const decisionCell = (tool, current) => (tool
    ? Object.freeze({ tool_key: tool.tool_key, current, editable: Boolean(decisions?.editable), pending: decisions?.pending === tool.tool_key, name: tool.display_name ?? tool.tool_key })
    : Object.freeze({ tool_key: null, current, editable: false, pending: false, name: null }));
  const ruleIds = (tool) => (!naming || !tool ? null : naming.get(tool.tool_key)?.join(', ') ?? 'None');
  const users = (group) => {
    const value = foldedMeasure(group, 'users').value ?? null;
    return value === null ? null : Object.freeze({ count: value, of: counted?.of ?? null });
  };
  const rows = observed.map((entry) => {
    const { tool, group } = entry;
    const name = nameOf(entry);
    const current = decisionOf(entry);
    return Object.freeze({
      group: current === 'unknown' ? IN_USE_UNDECIDED : IN_USE,
      tool_cell: Object.freeze({
        primary: name,
        mono: !tool && name === group.key,
        title: tool ? `${plural((tool.fingerprints ?? []).length, 'fingerprint', 'fingerprints')} in the catalogue` : `Tool fingerprint ${group.key}`,
        secondary: tool ? null : isSite(group) ? NOT_CATALOGUED : name === UNRECOGNISED_TOOL ? group.key : 'Not in the tool catalogue, so it has no decision',
      }),
      users: users(group),
      submissions: foldedMeasure(group, 'submissions').value ?? null,
      sensitive: foldedMeasure(group, 'sensitive').value ?? null,
      rules: ruleIds(tool),
      decision: decisionCell(tool, current),
      // The next step after a decision not to sanction: a rule naming the tool.
      next: tool && decisions?.editable && current !== 'sanctioned' ? `#rules?tool=${encodeURIComponent(tool.tool_key)}` : null,
      href: link ? link(group.key, POLICY_WINDOW) : null,
    });
  });
  const unobserved = (catalogue ?? []).filter((tool) => !folded.has(tool.tool_key))
    .sort((a, b) => byName(a.display_name ?? a.tool_key, b.display_name ?? b.tool_key))
    .map((tool) => Object.freeze({
      group: UNOBSERVED,
      tool_cell: Object.freeze({
        primary: tool.display_name ?? tool.tool_key,
        mono: false,
        secondary: (tool.fingerprints ?? []).length > 0 ? `Covers ${(tool.fingerprints ?? []).join(', ')}` : `Covers app:${tool.tool_key}`,
      }),
      users: null,
      submissions: null,
      sensitive: null,
      rules: ruleIds(tool),
      decision: decisionCell(tool, decided(tool.sanctioned_state)),
      next: decisions?.editable && decided(tool.sanctioned_state) !== 'sanctioned' ? `#rules?tool=${encodeURIComponent(tool.tool_key)}` : null,
      href: null,
    }));
  const undecided = rows.filter((row) => row.group === IN_USE_UNDECIDED).length;
  const summary = state.data.length === 0 ? null
    : undecided === 0 ? `Every tool in use has a decision${unobserved.length > 0 ? `; ${plural(unobserved.length, 'catalogue tool has', 'catalogue tools have')} not been seen` : ''}.`
      : `${plural(undecided, 'tool in use waits', 'tools in use wait')} for a decision${unobserved.length > 0 ? `; ${plural(unobserved.length, 'catalogue tool has', 'catalogue tools have')} not been seen` : ''}.`;
  const banners = decisions?.problem
    ? [Object.freeze({ level: 'warning', title: 'Not changed', text: decisions.problem.message ?? decisions.problem.code ?? 'The decision was refused.' })]
    : [];
  return screen('sanction', 'Tools', null, 'mart.v_tool_usage', {
    subtitle: 'Each tool seen in the last 30 days, what it was used for, and the decision on it. A tool with no decision is neither sanctioned nor unsanctioned.',
    tiles: Object.freeze([]),
    tables: Object.freeze([
      Object.freeze({
        ...tableFrom({ data: [...rows, ...unobserved] }, {
          title: 'Tools',
          columns: [
            column('group', 'Group'),
            column('tool_cell', 'Tool', 'stack'),
            column('users', 'Users', 'share'),
            column('submissions', 'Prompts', 'measure'),
            column('sensitive', 'Sensitive', 'measure'),
            column('rules', 'Rules'),
            column('decision', 'Decision', 'decision'),
            ...(decisions?.editable ? [column('next', 'Add a rule', 'link', { linkLabel: 'Add a rule' })] : []),
          ],
          emptyText: emptyLine(state, 'No tool was in use in the last 30 days.'),
        }),
        ...(summary ? { summary } : {}),
        info: 'Every tool in use over the last 30 days, the ones waiting for a decision first, then the catalogue tools no device has seen. Users are out of everyone who could have sent a prompt; sensitive prompts are those a rule matched on a label.',
        groupBy: 'group',
        collapsed: Object.freeze([UNOBSERVED]),
        rowHref: 'href',
        breakdowns: false,
      }),
    ]),
    series: Object.freeze([]),
    ...shared(state, [
      'A decision covers every fingerprint of the tool: each way a device recognises it. Only an admin can change it, and every change is written to the audit trail.',
      'Users and prompts are the last 30 days. A tool\'s users is the most counted in any one day, a floor for the 30 days.',
      ...(catalogue ? [] : ['The tool catalogue is read by an admin; without it each fingerprint is its own row.']),
    ]),
    ...(banners.length > 0 ? { banners: Object.freeze([...banners, ...(state.banners ?? [])]) } : {}),
  });
}

// ---------------------------------------------------------------------------------------------
// Insights: the last 30 days against the 30 before
// ---------------------------------------------------------------------------------------------

/** The window every Insights tab reads; each change compares it with the 30 days before. */
export const INSIGHTS_WINDOW = 'd30';

const insightsHref = (view) => `#insights?view=${view}`;
const PREVIOUS = 'Previous 30 days';

/**
 * How a figure moved against the previous period, as the size of the move: "+32 (5%) on the 30 days
 * before". A move from nothing has no share. Nothing unless both figures are known.
 */
function movedFrom(current, before, format = formatCount) {
  if (typeof current !== 'number' || typeof before !== 'number') return null;
  if (format(current) === format(before)) return Object.freeze({ direction: 'flat', text: 'No change' });
  const diff = current - before;
  // A share moves in points; a count or a rate moves by its difference, with the share of the before.
  const amount = format === formatPercent ? `${formatCount(Math.round(Math.abs(diff) * 100))} points` : format(Math.abs(diff));
  const share = format !== formatPercent && before > 0 ? ` (${formatPercent(Math.abs(diff) / before)})` : '';
  return Object.freeze({ direction: diff > 0 ? 'up' : 'down', text: `${diff > 0 ? '+' : '−'}${amount}${share} on the 30 days before` });
}

/** Names in a sentence: "a, b and c". */
function listText(names) {
  return names.length > 1 ? `${names.slice(0, -1).join(', ')} and ${names[names.length - 1]}` : names.join('');
}

/** Why a figure out of the window's prompts is a dash: there were none, or the read did not say. */
function promptsNote(state) {
  const prompts = sumMeasure(state, 'submissions');
  return prompts.kind === 'number' && prompts.value === 0 ? 'No prompts in the last 30 days.' : absentNote(state);
}

/** A built-in label as a sentence names what a prompt held, and as a table heads its row. */
const labelText = (code) => DATA_CLASS_NAMES[code] ?? String(code);
const labelTitle = (code) => {
  const text = labelText(code);
  return `${text[0].toUpperCase()}${text.slice(1)}`;
};

/** A tool's name in a sentence: the fingerprint of one the catalogue does not name, which tells two apart. */
const namedTool = (row) => (toolName(row) === UNRECOGNISED_TOOL ? String(row.tool) : toolName(row));

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

// ---------------------------------------------------------------------------------------------
// Insights: Adoption
// ---------------------------------------------------------------------------------------------

/** The users who sent a prompt, out of the users who could have, split by the sanction of their tools. */
function peopleShare(counted, before, { period, emptyText }) {
  const total = counted?.people ?? 0;
  const part = (key, label, count) => Object.freeze({ key, label, count: count ?? 0, share: total > 0 ? (count ?? 0) / total : 0 });
  const of = counted?.of ?? null;
  const share = of !== null && of > 0 && total <= of ? ` (${formatPercent(total / of)})` : '';
  const change = counted && before ? changeOf({ kind: 'number', value: total }, { kind: 'number', value: before.people ?? 0 }, period) : null;
  return Object.freeze({
    kind: 'share',
    title: 'Users',
    cardHref: '#person',
    info: 'Everyone who sent a prompt in this window, each counted once, by whether the tools they used are sanctioned.',
    total,
    totalLabel: of !== null ? `${outOf(counted)}${share}` : (total === 1 ? 'user' : 'users'),
    emptyText: counted ? emptyText : 'The users could not be counted for this window.',
    note: change?.text ?? null,
    parts: Object.freeze([
      part('sanctioned', 'Sanctioned only', counted?.sanctionedOnly),
      part('both', 'Both', counted?.both),
      part('unsanctioned', 'Unsanctioned only', counted?.notSanctionedOnly),
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
    const said = `${more ? `${more} ` : ''}${plural(n, 'tool', 'tools')} sent ${n === 1 && !more ? 'its' : 'their'} first prompt in this window`;
    summary = parts.length > 0 ? `${said}; ${parts.join(' and ')}.` : `${said}; ${n === 1 && !more ? 'it is' : 'all are'} sanctioned.`;
    if (earliest) summary = `${summary} ${earliest}`;
  }
  const emptyText = !firstSeen ? 'The tools new in this window could not be read.'
    : firstSeen.isRefusal ? `The tools new in this window could not be read: ${firstSeen.error?.message ?? firstSeen.resultState}.`
      : 'No tool sent its first prompt in this window.';
  return Object.freeze({
    kind: 'table',
    ...tableFrom({ data: rows }, {
      title: 'New this period',
      columns: [
        column('tool_cell', 'Tool', 'stack'),
        column('sanctioned_state', 'Sanction', 'status'),
        column('first_seen_day', 'First seen'),
        column('people', 'Users', 'measure'),
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
    return Object.freeze({
      tool_cell: toolCell(tool.row),
      people: tool.people,
      actions: tool.actions,
      input_tokens: tool.input,
      output_tokens: tool.output,
      change: earlier === null || tool.input === null ? null : changeFigure(tool.input, earlier),
    });
  });
  return Object.freeze({
    kind: 'table',
    ...tableFrom({ data: rows }, {
      title: 'Coding agents',
      columns: [
        column('tool_cell', 'Tool', 'stack'),
        column('people', 'Users', 'measure'),
        column('actions', 'Agent actions', 'measure'),
        column('input_tokens', 'Input tokens', 'measure'),
        column('output_tokens', 'Output tokens', 'measure'),
        column('change', 'Change'),
      ],
      emptyText,
    }),
    info: `What each coding agent sent to models in this window, from the telemetry it exports. Change is in input tokens against ${period}.`,
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
      title: 'Models called',
      columns: [
        column('model', 'Model'),
        column('tools', 'Used by'),
        column('input_tokens', 'Input tokens', 'measure'),
        column('people', 'Users', 'measure'),
      ],
      emptyText,
    }),
    ...(top ? { summary: `${plural(reported, 'model', 'models')} received code and context from coding agents in this window; ${top.name} received the most.` } : {}),
    breakdowns: false,
  });
}

/** The coding agents' tables: by tool, compared with the period before, and by model. */
function agentTables(agents, previous, { period }) {
  const unread = agents ? null : 'What coding agents reported could not be read.';
  const before = comparable(previous);
  const beforeTools = before ? new Map(agentTools(before).map((tool) => [tool.key, tool])) : null;
  return [
    agentToolsTable(agentTools(agents), beforeTools, { period, emptyText: unread ?? emptyLine(agents, 'No coding agent reported in this window.') }),
    agentModelsTable(agentModels(agents), { emptyText: unread ?? emptyLine(agents, 'No coding agent called a model in this window.') }),
  ];
}

/** How to read the coding agents' tables: which agents can report, and what their figures count. */
function agentNotes(agents) {
  const exporting = ENDPOINT_TOOLS.filter((tool) => tool.otel).map((tool) => tool.label);
  const silent = ENDPOINT_TOOLS.filter((tool) => !tool.otel && tool.hooks).map((tool) => tool.label);
  const one = silent.length === 1;
  return [
    `${listText(exporting)} report coding agent figures while their OpenTelemetry switch is on in Settings${silent.length > 0 ? `; ${listText(silent)} ${one ? 'has' : 'have'} no telemetry the device can collect, so ${one ? 'its' : 'their'} prompts are on Tools but not ${one ? 'its' : 'their'} tokens` : ''}. A tool with no row reported nothing in this window.`,
    'Input tokens are the code and context an agent sent to a model; agent actions are the tool calls it made, such as running a command or editing a file.',
    `A coding agent's users is the most users on any one ${agents?.meta?.applied_bucket === 'hour' ? 'hour' : 'day'} in the window: a floor for the window, as on Tools.`,
  ];
}

/**
 * Insights, Adoption: how many people used AI out of how many could have, and on which side of the
 * sanction line; the applications new in the window; whether prompts are moving to sanctioned
 * tools; and what coding agents send, and to which models. The people and the agents are compared
 * with the period of the same length just before the window.
 *
 * @param {object} input
 * @param {object} input.usage        view state for tool usage over the window (Q1), with its people
 * @param {object} [input.previous]   the same read over the previous period
 * @param {object} [input.firstSeen]  view state for the tools first seen in the window
 * @param {object} [input.shift]      view state for prompts per bucket by sanction
 * @param {object} [input.agents]     view state for what coding agents reported in the window
 * @param {object} [input.previousAgents] the same read over the previous period
 */
export function adoptionView({ usage, previous = null, firstSeen = null, shift = null, agents = null, previousAgents = null }, { preset = INSIGHTS_WINDOW, link = toolHref } = {}) {
  const period = previousPeriod(preset);
  const counted = peopleOf(usage);
  const before = peopleOf(comparable(previous));
  const notes = [
    'Users counts everyone who sent a prompt in the window once, however many tools and days they used.',
    'A tool not sanctioned is one marked unsanctioned or with no decision yet.',
  ];
  if (counted?.basis === 'directory') notes.push('The users are out of everyone the directory lists as active.');
  else if (counted?.basis === 'devices') notes.push('No directory is synced, so the users are out of everyone last signed in on an enrolled device.');
  else if (counted) notes.push('No directory is synced and no enrolled device has said who is signed in, so there is no count to compare with.');
  notes.push(before
    ? `Each change compares this window with ${period}.`
    : `${period[0].toUpperCase()}${period.slice(1)} could not be read, so no change is shown.`);
  notes.push('A tool is new from the day it sent its first prompt here, under any of its fingerprints; its users are everyone who has used it from then until now.');
  notes.push(...agentNotes(agents));
  // Nothing before the window means collection began inside it, so every tool in use is new.
  const fresh = comparable(previous) && previous.data.length === 0 ? 'Nothing was recorded before this window, so every tool in use is new.' : null;
  const shiftSeries = sanctionShift(shift);
  return screen('insights', 'Insights', null, 'mart.v_tool_usage', {
    subtitle: 'Adoption: how many users send prompts to AI, which applications are new, whether use is moving to sanctioned tools, and what coding agents send.',
    lead: Object.freeze([peopleShare(counted, before, { period, emptyText: emptyLine(usage, 'No one sent a prompt in this window.') })]),
    tiles: Object.freeze([]),
    blocks: Object.freeze([
      newToolsTable(firstSeen, { link: link ? (fingerprint) => link(fingerprint, preset) : null, earliest: fresh }),
      ...(shiftSeries ? [shiftSeries] : []),
      ...agentTables(agents, previousAgents, { period }),
    ]),
    series: Object.freeze([]),
    tables: Object.freeze([]),
    ...shared(usage, notes),
  });
}

// ---------------------------------------------------------------------------------------------
// Insights: Risk
// ---------------------------------------------------------------------------------------------

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
/** The models, hosts or tools a line names before the rest are counted. */
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

/** The prompts a found tool sent: unknown when the read of prompts left it out because it was cut short, else none. */
function promptsOf(tool, prompted) {
  const sent = prompted.get(tool.key);
  return sent !== null ? sent : prompted.truncated ? null : 0;
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

/** What a tool was found as, in words. */
const foundAs = (tool) => Object.keys(FOUND_AS).filter((type) => tool.types.has(type)).map((type) => FOUND_AS[type]).join(', ');

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
      const prompts = prompted ? promptsOf(tool, prompted) : null;
      const cell = toolCell(tool.row);
      return Object.freeze({
        tool_cell: Object.freeze({ ...cell, secondary: [tool.items.size > 0 ? namesList(tool.items) : null, cell.secondary].filter(Boolean).join(' · ') || null }),
        found_as: foundAs(tool),
        devices: tool.devices,
        first_seen_day: tool.firstSeen ? formatInstant(tool.firstSeen).slice(0, 10) : null,
        last_seen_day: tool.lastSeen ? formatInstant(tool.lastSeen).slice(0, 10) : null,
        prompts,
        href: prompts > 0 && link ? link(tool.key) : null,
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
    info: 'The AI applications, CLIs, IDE extensions, local models and inference endpoints found on devices, whether or not they sent prompts.',
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

/** The prompts that matched a rule naming one of their labels, out of every prompt, as a rate per 1,000. */
function exposureOf(state) {
  if (!state) return null;
  const sensitive = sumMeasure(state, 'sensitive');
  const prompts = sumMeasure(state, 'submissions');
  if (sensitive.kind !== 'number' || prompts.kind !== 'number' || prompts.value === 0) return null;
  return Object.freeze({ sensitive: sensitive.value, prompts: prompts.value, rate: (sensitive.value / prompts.value) * 1000 });
}

const EXPOSURE_INFO = 'Prompts that matched a rule naming one of their labels, per 1,000 prompts in the last 30 days. It is a rate, so more use alone does not raise it.';

/** The sensitive prompts per 1,000, with the counts it is made of and how it moved. */
function exposureTile(usage, previous, options = {}) {
  const label = 'Sensitive per 1,000 prompts';
  const now = exposureOf(usage);
  if (!now) return tile(label, { kind: 'absent', text: '—' }, promptsNote(usage), { ...options, info: EXPOSURE_INFO });
  const before = exposureOf(previous);
  return tile(label, { kind: 'number', text: formatRate(now.rate) }, `${formatCount(now.sensitive)} of ${formatCount(now.prompts)} prompts`, {
    ...options,
    info: EXPOSURE_INFO,
    change: before ? movedFrom(now.rate, before.rate, formatRate) : null,
  });
}

/**
 * The built-in labels on prompts sent outside sanctioned tools, one entry per label: the prompts
 * holding it, those no rule acted on for it, the tools those went to, and the rules that name it.
 * A prompt with two labels is in both entries. Null when the read did not answer.
 */
function labelGaps(labels, rules) {
  if (!labels || labels.isRefusal) return null;
  const known = rules && !rules.isRefusal ? rules.data : null;
  const gaps = new Map();
  for (const row of labels.data) {
    if (!row.class || row.sanctioned_state === 'sanctioned') continue;
    const prompts = measureOf(row, 'submissions');
    const flagged = measureOf(row, 'flagged');
    if (prompts.kind !== 'number' || flagged.kind !== 'number') continue;
    const key = String(row.class);
    const gap = gaps.get(key) ?? { key, prompts: 0, uncovered: 0, tools: new Map() };
    const missed = Math.max(0, prompts.value - flagged.value);
    gap.prompts += prompts.value;
    gap.uncovered += missed;
    if (missed > 0) {
      const tool = gap.tools.get(String(row.tool)) ?? { row, count: 0 };
      tool.count += missed;
      gap.tools.set(String(row.tool), tool);
    }
    gaps.set(key, gap);
  }
  return [...gaps.values()]
    .map((gap) => Object.freeze({
      key: gap.key,
      prompts: gap.prompts,
      uncovered: gap.uncovered,
      tools: Object.freeze([...gap.tools.values()].sort((a, b) => b.count - a.count || toolName(a.row).localeCompare(toolName(b.row)))),
      rules: known ? Object.freeze(known.filter((rule) => Array.isArray(rule.match?.labels) && rule.match.labels.includes(gap.key)).map((rule) => String(rule.rule))) : null,
    }))
    .sort((a, b) => b.uncovered - a.uncovered || b.prompts - a.prompts || a.key.localeCompare(b.key));
}

/** The prompts no rule acted on, per label, in a read that answered; a label it does not hold had none. */
const uncoveredBy = (gaps) => (gaps ? new Map(gaps.map((gap) => [gap.key, gap.uncovered])) : null);

/** The tools a line counts, the busiest few by name with their counts. */
function toolCounts(tools) {
  const shown = tools.slice(0, ITEMS_SHOWN).map((tool) => `${namedTool(tool.row)} ${formatCount(tool.count)}`).join(' · ');
  return tools.length > ITEMS_SHOWN ? `${shown} · ${formatCount(tools.length - ITEMS_SHOWN)} more` : shown;
}

/** The labels found outside sanctioned tools that no rule acted on, out of all of them, opening Policy gaps. */
function gapsTile(gaps) {
  const label = 'Labels with no rule';
  const options = { jump: 'Policy gaps', info: 'Built-in labels on prompts sent outside sanctioned tools with no rule acting on them, out of every label found on those prompts.' };
  if (!gaps) return tile(label, { kind: 'absent', text: '—' }, 'The labels could not be read.', options);
  if (gaps.length === 0) return tile(label, { kind: 'number', text: '0' }, 'No labelled prompt went outside sanctioned tools.', options);
  const open = gaps.filter((gap) => gap.uncovered > 0).length;
  return tile(label, { kind: 'number', text: formatCount(open) }, `of ${plural(gaps.length, 'label', 'labels')} found outside sanctioned tools`, options);
}

/**
 * Every built-in label on prompts sent outside sanctioned tools: its prompts, the rules that name
 * it, the prompts no rule acted on and where they went, and how those moved.
 */
function gapsTable(gaps, before, { emptyText }) {
  const rows = (gaps ?? []).map((gap) => Object.freeze({
    label: labelTitle(gap.key),
    prompts: gap.prompts,
    rules: gap.rules === null ? null : gap.rules.length > 0 ? gap.rules.join(', ') : 'No rule',
    uncovered: gap.uncovered,
    tools: gap.tools.length > 0 ? toolCounts(gap.tools) : null,
    change: before ? movedFrom(gap.uncovered, before.get(gap.key) ?? 0)?.text ?? null : null,
  }));
  const open = rows.filter((row) => row.uncovered > 0).length;
  let summary = null;
  if (rows.length > 0) {
    summary = open === 0 ? 'A rule acted on every labelled prompt sent outside sanctioned tools.'
      : `${formatCount(open)} of the ${plural(rows.length, 'label', 'labels')} found outside sanctioned tools had prompts no rule acted on.`;
  }
  return Object.freeze({
    kind: 'table',
    ...tableFrom({ data: rows }, {
      title: 'Policy gaps',
      columns: [
        column('label', 'Label'),
        column('prompts', 'Prompts', 'measure'),
        column('rules', 'Rules'),
        column('uncovered', 'No rule acted', 'measure'),
        column('tools', 'Where'),
        column('change', 'Change'),
      ],
      emptyText,
    }),
    ...(summary ? { summary } : {}),
    info: 'The prompts holding each built-in label that went to a tool not sanctioned, and those no rule acted on for that label.',
    href: '#rules',
    moreLabel: 'Rules',
    breakdowns: false,
  });
}

/**
 * Each rule that warns, with what its warnings did: the prompts the person cancelled, those that
 * went ahead, and those on a route where no warning could be shown. Null without the rules or what
 * they matched.
 */
function warningsOf(rules, matches) {
  if (!rules || rules.isRefusal || !matches || matches.isRefusal) return null;
  const groups = new Map(foldCells(matches, 'rule', { sum: RULE_MATCHES, max: [] }).map((g) => [g.key, g]));
  const count = (id, measure) => (groups.has(id) ? foldedMeasure(groups.get(id), measure).value ?? 0 : 0);
  return rules.data.filter((rule) => rule.action === 'warn').map((rule) => {
    const id = String(rule.rule);
    return Object.freeze({ rule, id, stopped: count(id, 'blocked'), wentAhead: count(id, 'warned'), unshown: count(id, 'logged') });
  });
}

/** The prompts a warning was shown on. */
const shownOn = (warning) => warning.stopped + warning.wentAhead;

/** Whether a rule's warning can be answered: only the browser extension asks the person, who can cancel. */
const asks = (rule) => !Array.isArray(rule.match?.routes) || rule.match.routes.length === 0 || rule.match.routes.some((route) => String(route).startsWith('ext.'));

/** The warned prompts a warning stopped, out of those it was shown on, opening Rules at work. */
function warningsTile(warnings) {
  const label = 'Warnings heeded';
  const info = 'Prompts a warning stopped because the person cancelled, out of the prompts a warning was shown on in the last 30 days.';
  if (!warnings) return tile(label, { kind: 'absent', text: '—' }, 'The rules or what they matched could not be read.', { jump: 'Rules at work', info });
  const answerable = warnings.filter((warning) => asks(warning.rule));
  if (answerable.length === 0) return tile(label, { kind: 'absent', text: '—' }, 'No rule warns in the browser.', { href: '#rules', moreLabel: 'Rules', info });
  const stopped = answerable.reduce((sum, warning) => sum + warning.stopped, 0);
  const shown = answerable.reduce((sum, warning) => sum + shownOn(warning), 0);
  if (shown === 0) return tile(label, { kind: 'absent', text: '—' }, 'No warning was shown in the last 30 days.', { jump: 'Rules at work', info });
  return tile(label, { kind: 'number', text: formatPercent(stopped / shown) }, `${formatCount(stopped)} of ${plural(shown, 'warned prompt', 'warned prompts')} stopped`, { jump: 'Rules at work', info });
}

/** What a rule did to the prompts it matched, in words, by its action. */
function ruleOutcome(action, counts) {
  if (counts.matches === 0) return 'Did not fire';
  const { blocked, warned, logged } = counts;
  const parts = {
    warn: [[blocked, 'stopped'], [warned, 'went ahead'], [logged, 'where no warning could be shown']],
    block: [[blocked, 'blocked'], [warned, 'warned'], [logged, 'on routes that cannot block']],
    redirect: [[blocked, 'redirected'], [warned, 'warned'], [logged, 'on routes that cannot redirect']],
    allow: [[logged, 'allowed'], [blocked, 'blocked'], [warned, 'warned']],
  }[action] ?? [[blocked, 'blocked'], [warned, 'warned'], [logged, 'allowed']];
  // A warning's two answers are always said; any other part only when it happened.
  const said = parts.filter(([n], i) => i === 0 || n > 0 || (action === 'warn' && i === 1));
  return said.map(([n, what]) => `${formatCount(n)} ${what}`).join(', ');
}

/**
 * Every enforcement rule, in the order a device applies it, with how often it fired in the window
 * and what happened then. Without what they matched, the rules are listed with that absent.
 */
function rulesAtWork(rules, matches) {
  const known = rules && !rules.isRefusal ? rules.data : null;
  const counted = matches && !matches.isRefusal ? matches : null;
  const groups = new Map((counted ? foldCells(counted, 'rule', { sum: RULE_MATCHES, max: [] }) : []).map((g) => [g.key, g]));
  const counts = (id) => Object.fromEntries(RULE_MATCHES.map((m) => [m, groups.has(id) ? foldedMeasure(groups.get(id), m).value ?? 0 : 0]));
  const rows = (known ?? []).map((rule, i) => {
    const n = counted ? counts(String(rule.rule)) : null;
    return Object.freeze({
      rule: Object.freeze({ primary: String(rule.rule), mono: true, secondary: `Rule ${i + 1} · ${ruleConditionsText(rule)}` }),
      action: RULE_ACTION_KEYS[rule.action] ?? 'unknown',
      action_text: RULE_ACTION_LABELS[rule.action] ?? String(rule.action ?? 'unknown'),
      matches: n ? n.matches : null,
      outcome: n ? ruleOutcome(rule.action, n) : null,
    });
  });
  let summary = null;
  if (known && known.length > 0 && !counted) summary = 'What each rule matched could not be read for the last 30 days.';
  else if (known && known.length > 0) {
    const quiet = rows.filter((row) => row.matches === 0).length;
    summary = quiet === 0 ? 'Every rule matched a prompt in the last 30 days.' : `${formatCount(known.length - quiet)} of ${plural(known.length, 'rule', 'rules')} matched a prompt in the last 30 days; ${formatCount(quiet)} matched none.`;
  }
  return Object.freeze({
    kind: 'table',
    ...tableFrom({ data: rows }, {
      title: 'Rules at work',
      columns: [
        column('rule', 'Rule', 'stack'),
        column('action', 'Action', 'status'),
        column('matches', 'Prompts', 'measure'),
        column('outcome', 'What happened'),
      ],
      emptyText: known ? 'No enforcement rule is set, so every prompt is logged.' : 'The rules could not be read.',
    }),
    ...(summary ? { summary } : {}),
    info: 'Each rule, how many prompts it decided in the last 30 days, and what happened to them.',
    href: '#rules',
    moreLabel: 'Rules',
    breakdowns: false,
  });
}

/** The applications found on devices from which no prompt was seen, out of every one found, opening Found on devices. */
function silentTile(tools, prompted) {
  const label = 'Installed but silent';
  const options = { jump: 'Found on devices', info: 'Tools found on a device in the last 30 days from which no prompt was seen. Their use is unknown, not zero.' };
  if (!tools) return tile(label, { kind: 'absent', text: '—' }, 'What discovery found could not be read.', options);
  if (!prompted) return tile(label, { kind: 'absent', text: '—' }, 'The prompts could not be read.', options);
  const silent = tools.filter((tool) => promptsOf(tool, prompted) === 0).length;
  return tile(label, { kind: 'number', text: formatCount(silent) }, `of ${plural(tools.length, 'tool', 'tools')} found`, options);
}

/**
 * Insights, Risk: whether sensitive data is leaving and whether the rules stop it. The sensitive
 * rate against the previous 30 days; the labels on prompts sent outside sanctioned tools that no
 * rule acted on; what each rule did when it fired; and the applications found on devices, with
 * those from which no prompt was seen.
 *
 * @param {object} input
 * @param {object} input.usage          view state for tool usage over the last 30 days (Q1)
 * @param {object} [input.previous]     the same read over the 30 days before
 * @param {object} [input.rules]        view state for the enforcement rules as they stand
 * @param {object} [input.ruleMatches]  view state for the prompts each rule matched
 * @param {object} [input.labels]       view state for the labels on prompts, per tool and sanction
 * @param {object} [input.previousLabels] the same read over the 30 days before
 * @param {object} [input.found]        view state for what discovery found, with the inventory scan beside it
 * @param {object} [input.prompts]      view state for the prompts per tool
 * @param {object} [options]
 * @param {boolean} [options.activity]  whether Policy → Activity is offered, for the tile that opens it
 */
export function riskView({ usage, previous = null, rules = null, ruleMatches = null, labels = null, previousLabels = null, found = null, prompts = null }, { link = toolHref, activity = false } = {}) {
  const before = comparable(previous);
  const gaps = labelGaps(labels, rules);
  const warnings = warningsOf(rules, ruleMatches);
  const prompted = promptsByTool(prompts);
  const tools = found && !found.isRefusal ? foundTools(found) : null;
  const reporting = found?.coverage?.devices_reporting ?? null;
  const scan = scanOf(found);
  // A month is read over day rows, so its count is the busiest day's.
  const busiest = found?.meta?.applied_bucket === 'hour' ? 'hour' : 'day';
  const notes = [
    'Sensitive per 1,000 prompts counts the prompts that matched a rule naming one of their labels. Policy gaps counts the labelled prompts no rule acted on, which that rate leaves out.',
    'Outside sanctioned tools means tools marked unsanctioned or with no decision yet. A prompt with two labels is counted under each in Policy gaps.',
    'A warning stops a prompt only when the person cancels it, which the browser extension asks. On other routes the prompt goes ahead with the warning recorded.',
    `Devices counts the devices that reported a finding on its busiest ${busiest} in the window, out of the devices reporting now; a device can hold a tool without it being found that ${busiest}.`,
    prompted
      ? 'Prompts counts what each application sent in the last 30 days under any of its fingerprints; a row with prompts opens its Tools row.'
      : 'The prompts each application sent could not be read, so the Prompts column is empty.',
    ...(scan && scan.notScanned > 0 ? [`${plural(scan.notScanned, 'enrolled device has', 'enrolled devices have')} no inventory scan in the last 24 hours, of ${formatCount(scan.enrolled)}, so what they hold is not known.`] : []),
    'Each change compares the last 30 days with the 30 before; there is none where that period could not be read.',
  ];
  if (ruleMatches?.meta?.truncated) notes.push('Not every rule\'s matches could be read, so Rules at work undercounts.');
  const gapsEmpty = !labels ? 'The labels could not be read.'
    : labels.isRefusal ? `The labels could not be read: ${labels.error?.message ?? labels.resultState}.`
      : emptyLine(labels, 'No labelled prompt went outside sanctioned tools in the last 30 days.');
  return screen('insights', 'Insights', null, 'mart.v_tool_usage', {
    subtitle: 'Risk: whether sensitive data is leaving, and whether the rules stop it.',
    tiles: Object.freeze([
      exposureTile(usage, before, activity ? { href: withPreset('#activity', INSIGHTS_WINDOW) } : {}),
      gapsTile(gaps),
      warningsTile(warnings),
      silentTile(tools, prompted),
    ]),
    blocks: Object.freeze([
      gapsTable(gaps, uncoveredBy(labelGaps(comparable(previousLabels), rules)), { emptyText: gapsEmpty }),
      rulesAtWork(rules, ruleMatches),
      foundTable(tools ?? [], {
        reporting: reporting === null ? null : Number(reporting),
        link: link ? (fingerprint) => link(fingerprint, INSIGHTS_WINDOW) : null,
        prompted,
        truncated: Boolean(found?.meta?.truncated),
        emptyText: found ? emptyLine(found, 'Discovery found no AI application on a device in the last 30 days.') : 'What discovery found could not be read.',
      }),
    ]),
    series: Object.freeze(exposureSeries(usage)),
    tables: Object.freeze([]),
    ...shared(usage, notes),
  });
}

// ---------------------------------------------------------------------------------------------
// Insights: Licensing
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

/** The teams a tool's users are in, the largest few by name with their counts; null when none is. */
function teamsText(tool, teamNames) {
  const named = [...tool.teams.entries()]
    .filter(([, people]) => people > 0)
    .sort((a, b) => b[1] - a[1] || teamNames.get(a[0]).localeCompare(teamNames.get(b[0])))
    .map(([id, people]) => `${teamNames.get(id)} ${formatCount(people)}`);
  const shown = named.slice(0, DEMAND_TEAMS_NAMED).join(', ');
  return named.length > DEMAND_TEAMS_NAMED ? `${shown} and ${formatCount(named.length - DEMAND_TEAMS_NAMED)} more` : shown || null;
}

/** The demand's team names by id. */
const teamNamesOf = (demand) => new Map((demand?.teams ?? []).map((team) => [team.id, team.name]));

/**
 * Every tool not sanctioned that people used in the window, by the people who used it, which is
 * the seats a licence would need: the people across everyone, how many of them are in no team,
 * and the teams they are in. Each opens its Tools row.
 */
function seatsTable(demand, teamNames, { link, emptyText }) {
  const rows = (demand?.tools ?? []).map((tool) => Object.freeze({
    tool_cell: toolCell(tool.row),
    people: tool.people,
    no_team: tool.people === null || tool.inTeams === null ? null : tool.people - tool.inTeams,
    teams: teamsText(tool, teamNames),
    href: link ? link(String(tool.row.tool)) : null,
  }));
  const top = demand?.tools?.[0];
  return Object.freeze({
    kind: 'table',
    ...tableFrom({ data: rows }, {
      title: 'Seats to license',
      columns: [
        column('tool_cell', 'Tool', 'stack'),
        column('people', 'Users', 'measure'),
        column('no_team', 'In no team', 'measure'),
        column('teams', 'Teams they are in'),
      ],
      emptyText,
    }),
    ...(top && top.people !== null
      ? { summary: `${plural(rows.length, 'tool not sanctioned was', 'tools not sanctioned were')} used in this window; ${toolName(top.row)} had the most users, ${formatCount(top.people)}${top.teams.size > 0 ? ` in ${plural(top.teams.size, 'team', 'teams')}` : ''}.` }
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
      columns: [column('team_cell', 'Team', 'stack'), ...tools.map((tool, i) => column(`tool_${i}`, namedTool(tool.row), 'share'))],
      emptyText,
    }),
    ...(href ? { href, moreLabel: 'Teams' } : {}),
    rowHref: 'href',
    breakdowns: false,
  });
}

/**
 * Insights, Licensing: the tools not sanctioned, by team, and the seats each would need: what to
 * license, and for whom. People are counted, never named.
 *
 * @param {object} state  view state for teams crossed with tools, with the demand beside it
 */
export function licensingView(state, { preset = INSIGHTS_WINDOW, tools = toolHref, teams = teamHref } = {}) {
  const demand = demandOf(state);
  const teamCount = teamCountOf(state);
  const unread = 'The demand could not be read for this window.';
  const seats = seatsTable(demand, teamNamesOf(demand), {
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
    'Users counts everyone who sent the tool a prompt in the window once, under any of its fingerprints: the seats a licence would need.',
    'A tool not sanctioned is one marked unsanctioned or with no decision yet.',
    ...(demand && demand.tools.length > DEMAND_TOOLS_SHOWN
      ? [`The ${DEMAND_TOOLS_SHOWN} tools with the most users are shown by team; Seats to license lists all ${formatCount(demand.tools.length)}.`]
      : []),
  ];
  return screen('insights', 'Insights', null, 'mart.agg_team_period', {
    subtitle: 'Licensing: the tools not sanctioned that users send prompts to, by team, and the seats each would need.',
    tiles: Object.freeze([
      demand
        ? tile('Tools not sanctioned', { kind: 'number', text: formatCount(demand.tools.length) }, 'used in this window', {
          jump: 'Seats to license',
          info: 'Tools marked unsanctioned or with no decision yet that someone sent a prompt to in this window.',
        })
        : tile('Tools not sanctioned', { kind: 'absent', text: '—' }, unread),
      demand && teamCount !== null && teamCount > 0
        ? tile('Teams affected', { kind: 'number', text: formatCount(demand.teams.length) }, `of ${plural(teamCount, 'team', 'teams')}`, {
          jump: 'Tools not sanctioned, by team',
          info: 'Teams with at least one member who sent a prompt to a tool not sanctioned in this window.',
        })
        : tile('Teams affected', { kind: 'absent', text: '—' }, teamCount === 0 ? 'No team exists yet.' : unread, teamCount === 0 ? { href: '#teams', moreLabel: 'Teams' } : {}),
      top && top.people !== null
        ? tile('Largest licence', { kind: 'number', text: formatCount(top.people) }, `users of ${toolName(top.row)}`, {
          ...(tools ? { href: tools(String(top.row.tool), preset) } : {}),
          info: 'The tool not sanctioned with the most users in this window: the seats a licence for it would need.',
        })
        : tile('Largest licence', { kind: 'absent', text: '—' }, demand ? 'No tool that is not sanctioned was used.' : unread),
    ]),
    blocks: Object.freeze([seats, byTeam]),
    series: Object.freeze([]),
    tables: Object.freeze([]),
    ...shared(state, notes),
  });
}

// ---------------------------------------------------------------------------------------------
// Insights: Briefing
// ---------------------------------------------------------------------------------------------

/** The users who sent a prompt in the window, each once, out of those who could have, and how that moved. */
function adoptersTile(usage, previous, options = {}) {
  const label = 'Users';
  const info = 'Everyone who sent a prompt in the last 30 days, each counted once however many tools they used.';
  const counted = peopleOf(usage);
  if (counted?.people === null || counted?.people === undefined) return tile(label, { kind: 'absent', text: '—' }, absentNote(usage), { ...options, info });
  return tile(label, { kind: 'number', text: formatCount(counted.people) }, outOf(counted), {
    ...options,
    info,
    change: movedFrom(counted.people, peopleOf(previous)?.people ?? null),
  });
}

/** The prompts sent to sanctioned tools, out of every prompt, and whether any tool in use has a decision. */
function sanctionedOf(state) {
  const prompts = state ? sumMeasure(state, 'submissions') : null;
  if (prompts?.kind !== 'number' || prompts.value === 0) return null;
  let sanctioned = 0;
  for (const row of state.data) {
    const value = measureOf(row, 'submissions');
    if (row.sanctioned_state === 'sanctioned' && value.kind === 'number') sanctioned += value.value;
  }
  const decided = state.data.some((row) => row.sanctioned_state === 'sanctioned' || row.sanctioned_state === 'unsanctioned');
  return Object.freeze({ prompts: prompts.value, share: sanctioned / prompts.value, decided });
}

/** The share of prompts sent to sanctioned tools, and how it moved; until a tool has a decision, where to make one. */
function sanctionedTile(usage, previous, options = {}) {
  const label = 'Sanctioned share';
  const info = 'Prompts sent to sanctioned tools, out of every prompt in the last 30 days. A tool with no decision is not sanctioned.';
  const now = sanctionedOf(usage);
  if (!now) return tile(label, { kind: 'absent', text: '—' }, promptsNote(usage), { ...options, info });
  if (!now.decided) return tile(label, { kind: 'absent', text: '—' }, 'No tool in use has a sanction decision yet.', { href: '#sanction', moreLabel: 'Decide which tools are sanctioned', info });
  const before = sanctionedOf(previous);
  return tile(label, { kind: 'number', text: formatPercent(now.share) }, `of ${formatCount(now.prompts)} prompts`, {
    ...options,
    info,
    change: before?.decided ? movedFrom(now.share, before.share, formatPercent) : null,
  });
}

/** What raises a finding: fixed counts a reader can check against the tables they come from. */
const FINDING_THRESHOLDS = Object.freeze({ uncovered: 10, warned: 10, stopped: 1 / 3, undecided: 10, silent: 10 });
/** The findings the Briefing shows, at most. */
const FINDINGS_SHOWN = 6;
/** What a finding is about, in the order the Briefing lists them. */
const FINDING_TOPICS = Object.freeze({ risk: 'Risk', licensing: 'Licensing', coverage: 'Coverage' });
const TOPIC_ORDER = Object.freeze(Object.keys(FINDING_TOPICS));

const FINDING_NOTES = Object.freeze([
  `A label is a finding when ${FINDING_THRESHOLDS.uncovered} or more prompts holding it went outside sanctioned tools and no rule acted on them.`,
  `A warning is a finding when it was shown on ${FINDING_THRESHOLDS.warned} or more prompts and stopped fewer than a third of them. A warning limited to routes that cannot ask the person is not one.`,
  `A tool with no sanction decision is a finding when ${FINDING_THRESHOLDS.undecided} or more users sent it a prompt.`,
  `An application is a finding when it was found on ${FINDING_THRESHOLDS.silent} or more devices and no prompt from it was seen.`,
  `At most ${FINDINGS_SHOWN} findings show: risk first, then licensing, then coverage, each largest first.`,
]);

/** One finding: what crossed a threshold, the figures behind it, and the one action it calls for. */
function finding(topic, key, text, { evidence, action, size }) {
  return Object.freeze({
    key,
    topic,
    topicText: FINDING_TOPICS[topic],
    text,
    evidence: evidence.filter(Boolean).join(' · ') || null,
    action: Object.freeze(action),
    size,
  });
}

/** The labels whose prompts went outside sanctioned tools with no rule acting on them. */
function uncoveredFindings(gaps, before) {
  return gaps.filter((gap) => gap.uncovered >= FINDING_THRESHOLDS.uncovered).map((gap) => {
    const name = labelText(gap.key);
    // Only a rulebase that was read can be said to have no rule for the label.
    const unnamed = gap.rules !== null && gap.rules.length === 0;
    return finding('risk', `uncovered:${gap.key}`, `${plural(gap.uncovered, 'prompt', 'prompts')} with ${name} went outside sanctioned tools. ${unnamed ? `No rule covers ${name}.` : 'No rule acted on them.'}`, {
      evidence: [toolCounts(gap.tools), before ? `${PREVIOUS}: ${formatCount(before.get(gap.key) ?? 0)}` : null],
      action: unnamed ? { label: `Add a rule for ${name}`, href: '#rules' } : { label: 'Review the rules', href: '#rules' },
      size: gap.uncovered,
    });
  });
}

/** The warnings the person can answer that stopped too few of the prompts they were shown on. */
function warningFindings(warnings, before) {
  const earlier = before ? new Map(before.map((warning) => [warning.id, warning])) : null;
  return warnings
    .filter((warning) => asks(warning.rule) && shownOn(warning) >= FINDING_THRESHOLDS.warned && warning.stopped / shownOn(warning) < FINDING_THRESHOLDS.stopped)
    .map((warning) => {
      const then = earlier?.get(warning.id);
      const previously = then && shownOn(then) > 0 ? `${formatCount(then.stopped)} of ${formatCount(shownOn(then))} stopped` : 'none warned';
      return finding('risk', `warning:${warning.id}`, `The warning ${warning.id} stopped ${formatCount(warning.stopped)} of ${plural(shownOn(warning), 'prompt', 'prompts')}. ${formatCount(warning.wentAhead)} went ahead.`, {
        evidence: [ruleConditionsText(warning.rule), earlier ? `${PREVIOUS}: ${previously}` : null],
        action: { label: `Edit ${warning.id}`, href: '#rules' },
        size: warning.wentAhead,
      });
    });
}

/** The tools with no sanction decision that enough people use to need one. */
function undecidedFindings(demand, before) {
  const teamNames = teamNamesOf(demand);
  const earlier = before ? new Map(before.tools.map((tool) => [tool.key, tool.people])) : null;
  return demand.tools
    .filter((tool) => !['sanctioned', 'unsanctioned'].includes(tool.row.sanctioned_state) && (tool.people ?? 0) >= FINDING_THRESHOLDS.undecided)
    .map((tool) => {
      const name = namedTool(tool.row);
      const teams = teamsText(tool, teamNames);
      return finding('licensing', `undecided:${tool.key}`, `${name} has ${plural(tool.people, 'user', 'users')} and no sanction decision.`, {
        evidence: [teams ? `Teams: ${teams}` : null, earlier ? `${PREVIOUS}: ${plural(earlier.get(tool.key) ?? 0, 'user', 'users')}` : null],
        action: { label: `Decide on ${name}`, href: '#sanction' },
        size: tool.people,
      });
    });
}

/** The applications found on enough devices from which no prompt was seen: their use is unknown. */
function silentFindings(tools, prompted) {
  return tools
    .filter((tool) => (tool.devices ?? 0) >= FINDING_THRESHOLDS.silent && promptsOf(tool, prompted) === 0)
    .map((tool) => finding('coverage', `silent:${tool.key}`, `${namedTool(tool.row)} is on ${plural(tool.devices, 'device', 'devices')}, but no prompt from it was seen.`, {
      evidence: [`Found as ${foundAs(tool)}`, 'Its use is unknown, not zero'],
      action: { label: 'See what was found', href: insightsHref('risk') },
      size: tool.devices,
    }));
}

/**
 * The findings, risk first, then licensing, then coverage, each largest first, up to the most the
 * Briefing shows. A check whose read did not come back is named, so its silence is not read as clear.
 */
function findingsBlock(checks) {
  const all = checks.flatMap((check) => check.found ?? []);
  const shown = [...all]
    .sort((a, b) => TOPIC_ORDER.indexOf(a.topic) - TOPIC_ORDER.indexOf(b.topic) || b.size - a.size || a.key.localeCompare(b.key))
    .slice(0, FINDINGS_SHOWN);
  const unchecked = checks.filter((check) => check.found === null).map((check) => check.name);
  const summary = [
    all.length > shown.length ? `The ${FINDINGS_SHOWN} largest of ${formatCount(all.length)} findings are shown.` : null,
    unchecked.length > 0 ? `Not checked, because a read did not come back: ${listText(unchecked)}.` : null,
  ].filter(Boolean).join(' ');
  return Object.freeze({
    kind: 'findings',
    title: 'Findings',
    info: 'What crossed a fixed threshold in the last 30 days, each with the action it calls for. The thresholds are under About this data.',
    items: Object.freeze(shown),
    ...(summary ? { summary } : {}),
    emptyText: unchecked.length === checks.length ? 'Nothing could be checked: none of the reads came back.' : 'Nothing crossed a threshold in the last 30 days.',
  });
}

/**
 * Insights, Briefing: three figures against the previous 30 days, then the findings that crossed a
 * fixed threshold, each with the one action it calls for. Each figure opens the tab that holds the
 * rest of it.
 *
 * @param {object} input
 * @param {object} input.usage            view state for tool usage over the last 30 days (Q1), with its people
 * @param {object} [input.previous]       the same read over the 30 days before
 * @param {object} [input.rules]          view state for the enforcement rules as they stand
 * @param {object} [input.ruleMatches]    view state for the prompts each rule matched
 * @param {object} [input.previousMatches] the same read over the 30 days before
 * @param {object} [input.labels]         view state for the labels on prompts, per tool and sanction
 * @param {object} [input.previousLabels] the same read over the 30 days before
 * @param {object} [input.demand]         view state for the users of each tool not sanctioned
 * @param {object} [input.previousDemand] the same read over the 30 days before
 * @param {object} [input.found]          view state for what discovery found
 * @param {object} [input.prompts]        view state for the prompts per tool
 */
export function briefingView({
  usage, previous = null, rules = null, ruleMatches = null, previousMatches = null, labels = null, previousLabels = null,
  demand = null, previousDemand = null, found = null, prompts = null,
}) {
  const before = comparable(previous);
  const gaps = labelGaps(labels, rules);
  const warnings = warningsOf(rules, ruleMatches);
  const wanted = demandOf(demand);
  const prompted = promptsByTool(prompts);
  const tools = found && !found.isRefusal ? foundTools(found) : null;
  const checks = [
    { name: 'labels no rule covers', found: gaps ? uncoveredFindings(gaps, uncoveredBy(labelGaps(comparable(previousLabels), rules))) : null },
    { name: 'warnings', found: warnings ? warningFindings(warnings, warningsOf(rules, comparable(previousMatches))) : null },
    { name: 'tools with no decision', found: wanted ? undecidedFindings(wanted, demandOf(comparable(previousDemand))) : null },
    { name: 'applications on devices', found: tools && prompted ? silentFindings(tools, prompted) : null },
  ];
  const notes = [
    'Every figure is the last 30 days; each change compares it with the 30 days before.',
    ...(before ? [] : ['The previous 30 days could not be read, so the figures show no change.']),
    'Outside sanctioned tools means tools marked unsanctioned or with no decision yet.',
    ...FINDING_NOTES,
  ];
  return screen('insights', 'Insights', null, 'mart.v_tool_usage', {
    subtitle: 'Briefing: the last 30 days against the 30 before, and what needs a decision.',
    tiles: Object.freeze([
      adoptersTile(usage, before, { href: insightsHref('adoption') }),
      sanctionedTile(usage, before, { href: insightsHref('licensing') }),
      exposureTile(usage, before, { href: insightsHref('risk') }),
    ]),
    blocks: Object.freeze([findingsBlock(checks)]),
    series: Object.freeze([]),
    tables: Object.freeze([]),
    ...shared(usage, notes),
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
  // Overview has just shown the totals and the split by decision; this page is the whole list.
  const ranking = 'Tools by prompts';
  return screen('tools', 'Tools', null, 'mart.v_tool_usage', {
    subtitle: 'Every AI tool seen in this window, by prompts. Open a tool to see who uses it and what was sent.',
    tiles: Object.freeze([]),
    filters: Object.freeze([Object.freeze({
      label: 'Sanction',
      current: sanction ?? '',
      items: Object.freeze([{ id: '', label: 'All', href: href(null) }, ...SANCTION_PARTS.map((key) => ({ id: key, label: SANCTION_TEXT[key], href: href(key) }))]),
    })]),
    blocks: Object.freeze([
      rankTools(shown, { title: ranking, link: linkTo, emptyText: emptyLine(state, none) }),
    ]),
    tables: Object.freeze([uncataloguedTools(shown, { link: linkTo })].filter(Boolean)),
    // A window with no cell has no shape to draw: the ranking's empty line says why.
    series: Object.freeze(shown.data.length > 0 ? [seriesFrom(shown, { measure: 'submissions', title: seriesTitle(shown) })] : []),
    ...shared(state, [
      'Rank is by prompts with a deterministic tie-break, and is never a sanction signal.',
      'Unknown is its own answer: a tool with no decision renders as unknown, never as unsanctioned.',
      'A tool\'s users is the most counted in any one period, a floor for the window: a user active on two days is counted on both.',
    ]),
  });
}

/** A roster is read in full and shown this many users at a time; Show more adds another page. */
const ROSTER_PAGE = 25;

/** The first rows of a roster and, when more follow, where the next page is. */
function pageOf(rows, shown, href) {
  const n = Number.isInteger(shown) && shown > 0 ? shown : ROSTER_PAGE;
  if (rows.length <= n) return { rows, href: null };
  return { rows: rows.slice(0, n), href: href(n + ROSTER_PAGE) };
}

/** The line that stands in for the users a device named but the directory does not: one sentence, and where the fix is. */
function unmatchedLine(count) {
  if (count === 0) return {};
  return {
    summary: `${plural(count, 'user is', 'users are')} known only by reference and not listed: the directory has no record of them.`,
    summaryLink: Object.freeze({ href: '#directory', label: 'Identity Providers' }),
  };
}

/**
 * One tool: its figures over the window, the findings it produced, and who is using it. The
 * people are a roster in the API's order, a page at a time, with their prompts; a user the
 * directory cannot name is counted in one line rather than listed by reference. A role that may
 * not see people sees the totals and is told why the roster is missing.
 */
export function toolView({ usage, people = null, findings = null, names = {} }, { tool, preset = null, exploreHref = 'explore.html', people: shown = ROSTER_PAGE }) {
  // The read is filtered to this tool, so its rows are the tool's own, under the id every list
  // names it by, which a link naming one of its fingerprints does not spell.
  const groups = foldCells(usage, 'tool');
  const group = groups.find((g) => g.key === String(tool)) ?? (groups.length === 1 ? groups[0] : null);
  tool = group ? group.key : String(tool);
  const name = group ? toolName(group.first) : String(tool);
  // A tool in use with no decision recorded is unknown, which is a state of its own; a tool with
  // no cell in the window has no state to show.
  const sanction = group?.first.sanctioned_state ?? (group ? 'unknown' : null);
  const everyone = people && !people.isRefusal
    ? foldCells(people, 'subject', { sum: ['submissions', 'bytes_total'], max: [] }).map((g) => ({
      subject: g.key,
      name: names[g.key] ?? null,
      submissions: foldedMeasure(g, 'submissions').value ?? null,
      bytes_total: foldedMeasure(g, 'bytes_total').value ?? null,
    }))
    : null;
  const roster = everyone ? everyone.filter((row) => row.name !== null) : null;
  const unmatched = everyone ? everyone.length - roster.length : 0;
  const page = roster ? pageOf(roster, shown, (n) => withPreset(`#tools?tool=${encodeURIComponent(String(tool))}&people=${n}`, preset)) : null;
  const notes = [];
  if (people?.isRefusal) {
    notes.push(people.resultState === 'unauthorised_role'
      ? 'Your role shows the tool\'s totals only; an analyst or admin also sees who is using it.'
      : `Who is using it could not be read: ${people.error?.message ?? people.resultState}.`);
  } else if (roster) {
    notes.push('Users are listed in the order the read returned them, never by volume. Reading this list is audited.');
  } else if (group) {
    notes.push('Who is using it was not read for this window.');
  }
  if (findings?.isRefusal) notes.push(`Findings could not be read: ${findings.error?.message ?? findings.resultState}.`);
  // Each figure opens the events behind it in Search, narrowed to this tool; the users open their roster below.
  const events = (filters = {}) => searchEventsLink({ tool: String(tool), ...filters }, preset);
  return screen('tool', name, null, 'mart.v_tool_usage', {
    subtitle: String(tool).startsWith(SITE_PREFIX) ? 'A site the tool catalogue does not name.'
      : name === String(tool) ? 'A tool known only by its fingerprint.' : null,
    badges: Object.freeze(sanction ? [sanctionChip(sanction)] : []),
    actions: Object.freeze([
      Object.freeze({ label: 'Prompts and events', href: `${exploreHref}#events?tool=${encodeURIComponent(String(tool))}` }),
      Object.freeze({ label: 'All tools', href: withPreset('#tools', preset) }),
    ]),
    tiles: Object.freeze([
      foldedTile('Prompts', group, 'submissions', formatCount, { trend: group && group.trend.length > 1 ? group.trend : null, ...events() }),
      foldedTile('Users', group, 'users', formatCount, { ...(roster ? { jump: 'Users' } : events()), info: USERS_FLOOR }),
      foldedTile('Blocked', group, 'blocked', formatCount, { ...events({ action: 'blocked' }), info: 'Prompts an enforcement rule blocked before they were sent.' }),
      foldedTile('Data sent', group, 'bytes_total', formatBytes, { ...events(), info: 'The size of every prompt and attachment sent to this tool, together.' }),
      attachmentsTile(usage, { preset, filters: { tool: String(tool) } }),
    ]),
    tables: Object.freeze([
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
      ...(roster ? [Object.freeze({
        ...tableFrom({ data: page.rows }, {
          title: 'Users',
          columns: [
            column('name', 'User', 'person'),
            column('submissions', 'Prompts', 'measure'),
            column('bytes_total', 'Data sent', 'measure-bytes'),
          ],
          emptyText: emptyLine(people, unmatched > 0 ? 'Nobody the directory names used this tool in this window.' : 'Nobody used this tool in this window.'),
        }),
        ...unmatchedLine(unmatched),
        ...(page.href ? { href: page.href, moreLabel: 'Show more' } : {}),
        // A count is a count; a bar beside each user would be a league table.
        bars: false,
        breakdowns: false,
      })] : []),
    ]),
    series: Object.freeze(group ? [seriesFrom(usage, { measure: 'submissions', title: seriesTitle(usage) })] : []),
    ...shared(usage, [
      ...notes,
      ...(name !== String(tool) && !String(tool).startsWith(SITE_PREFIX) ? [`This page is read under the fingerprint ${tool}, one of the ways a device recognises ${name}.`] : []),
      'Unknown is its own answer: a tool with no decision renders as unknown, never as unsanctioned.',
    ]),
  });
}

/**
 * The users behind the team totals, by team and then by name: a roster with numbers, not a
 * ranking. Null when there was nothing to read; a refusal says why the users are missing.
 */
function teamMembersTable(members, { byTeam = true, title = 'Prompts by user' } = {}) {
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
      ? 'Your role does not show each user\'s prompts; an analyst or admin sees them.'
      : `Each user's prompts could not be read: ${members.error?.message ?? members.resultState}.`];
  }
  const notes = ['Each user is listed once, by name, with the teams they are in. Reading this list is audited.'];
  if (members.meta?.truncated) notes.push(`The roster was cut short at ${formatCount(members.data.length)} rows, so some users or days are missing.`);
  if (members.audit?.entry_id) notes.push(`Audited read: entry ${members.audit.entry_id} at ${formatInstant(members.audit.written_at)}.`);
  return notes;
}

const INCLUDE_LABELS = Object.freeze({ group: 'Group', department: 'Department', org_unit: 'Unit', team: 'Team' });

/** How a team is made: its members, the users chosen for it, and the first things it includes. */
function teamFacts(team) {
  if (!team) return null;
  const includes = team.includes ?? [];
  const shown = includes.slice(0, 2).map((i) => `${INCLUDE_LABELS[i.kind]} ${i.name}`.trim());
  if (includes.length > shown.length) shown.push(`${formatCount(includes.length - shown.length)} more`);
  return [
    team.members === null || team.members === undefined ? null : plural(team.members, 'member', 'members'),
    team.people ? plural(team.people, 'user chosen', 'users chosen') : null,
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
    metaLabels: Object.freeze(['Users']),
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
  // Users in no team are said beside the users in one, so a team view cannot quietly stand for
  // everyone.
  const outside = all === null || inTeams === null ? null : Math.max(0, all - inTeams);
  const notes = [];
  if (cov && all !== null && inTeams !== null) {
    notes.push(`${plural(all, 'user', 'users')} used AI in this window; ${inTeams === 1 ? '1 is' : `${formatCount(inTeams)} are`} in at least one team and ${outside === 1 ? '1 is' : `${formatCount(outside)} are`} in none.`);
  }
  if (teamCount === 0) {
    notes.push(mayManage
      ? 'No team exists yet. Add one with + Add a Team: from a group in your identity provider, from people and groups synced over SCIM, or by ticking people.'
      : 'No team exists yet. An admin adds teams on this page.');
  }
  return screen('teams', 'Teams', null, 'mart.agg_team_period', {
    subtitle: 'Usage per team. A team counts the usage of the users in it now; a user in two teams counts in both.',
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
 * One team: its figures over the window and the users behind them, by name. The team is named
 * by the totals read, or by the roster when the totals carry no row for it.
 */
export function teamView(state, { team, members = null, teams = null, preset = null, people: shown = ROSTER_PAGE }) {
  const group = foldCells(state, 'team', { sum: ['submissions'], max: ['users'] }).find((g) => g.key === String(team)) ?? null;
  const known = (teams ?? []).find((t) => t.team_id === String(team)) ?? null;
  const name = String(group?.first.team_name ?? known?.name ?? members?.data?.find((row) => row.team_name)?.team_name ?? 'Team');
  const whole = members && !members.isRefusal ? teamMembersTable(members, { byTeam: false }) : null;
  // A member the directory cannot name is counted in one line, not listed by reference.
  const named = whole ? whole.rows.filter(({ row }) => row.name !== row.subject) : null;
  const page = named ? pageOf(named, shown, (n) => withPreset(`#teams?team=${encodeURIComponent(String(team))}&people=${n}`, preset)) : null;
  const roster = whole
    ? Object.freeze({
      ...whole,
      rows: Object.freeze(page.rows),
      ...unmatchedLine(whole.rows.length - named.length),
      ...(page.href ? { href: page.href, moreLabel: 'Show more' } : {}),
      bars: false,
      breakdowns: false,
    })
    : null;
  const listed = roster ? new Set(members.data.map((row) => row.subject)).size : null;
  const facts = teamFacts(known);
  // Every figure is the roster's, folded: each opens it.
  const below = roster ? { jump: roster.title } : {};
  return screen('team', name, null, 'mart.agg_team_period', {
    subtitle: facts ? `${facts}. A team counts the usage of the users in it now.` : 'One team\'s usage. A team counts the usage of the users in it now.',
    actions: Object.freeze([Object.freeze({ label: 'All teams', href: withPreset('#teams', preset) })]),
    tiles: Object.freeze([
      foldedTile('Prompts', group, 'submissions', formatCount, { trend: group && group.trend.length > 1 ? group.trend : null, ...below }),
      foldedTile('Users', group, 'users', formatCount, { ...below, info: USERS_FLOOR }),
      tile('Users listed', listed === null ? { kind: 'absent', text: '—' } : { kind: 'number', text: formatCount(listed) },
        listed === null ? 'The roster could not be read.' : 'sent a prompt in this window',
        { ...below, info: 'The members named in the roster below: each who sent a prompt in this window.' }),
    ]),
    tables: Object.freeze(roster ? [roster] : []),
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
    notes.push(`${formatCount(flush.rows_in_window)} rows for this user in the window, ${formatCount(flush.late_flush_rows)} received more than an hour after they occurred: a spool flush spikes received time, not behaviour.`);
  }
  const name = person?.name && person.name !== subject ? person.name : null;
  const facts = [person?.department, person?.org_unit].filter(Boolean).join(' · ');
  // Each figure opens the events behind it in Search, narrowed to this user.
  const events = (filters = {}) => searchEventsLink({ subject, ...filters }, preset);
  return screen('person', name ?? 'Users', null, 'mart.agg_user_period', {
    subtitle: name ? `${facts ? `${facts} · ` : ''}${subject}` : null,
    search: Object.freeze({ value: name ? '' : subject }),
    actions: Object.freeze([
      Object.freeze({ label: 'Prompts and events', href: `${exploreHref}#events?subject=${encodeURIComponent(subject)}` }),
      Object.freeze({ label: 'All users', href: '#person' }),
    ]),
    tiles: Object.freeze([
      measureTile('Prompts', state, 'submissions', formatCount, events()),
      tools === null
        ? tile('Tools used', { kind: 'absent', text: '—' }, absentNote(state))
        : tile('Tools used', { kind: 'number', text: formatCount(Number(tools)) }, null, { ...events(), info: 'Each tool this user sent a prompt to in this window, counted once.' }),
      measureTile('Blocked', state, 'block_events', formatCount, { ...events({ action: 'blocked' }), info: 'Prompts an enforcement rule blocked before they were sent.' }),
      measureTile('Data sent', state, 'bytes_total', formatBytes, { ...events(), info: 'The size of every prompt and attachment this user sent, together.' }),
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
        emptyText: 'No usage recorded for this user in this window.',
      }),
    ]),
    series: Object.freeze([series]),
    ...shared(state, notes.concat([
      'No cohort percentile, no ranking, no users sorted by volume. A spike is compared against this user\'s own trailing baseline only.',
      'The spike rule needs at least 14 observed buckets. Below that the answer is not_yet_covered, not "no change".',
    ])),
  });
}

/**
 * The users the directory and the devices know, by name, to open one user's page, each with
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
    title: search ? `Users whose name starts with "${search}"` : 'Users',
    columns: [
      column('name', 'Name', 'person'),
      column('team_names', 'Teams'),
      column('department', 'Department'),
      column('directory_status', 'Directory', 'vocab'),
      column('last_active_day', 'Last active', 'day'),
      column('submissions', 'Prompts', 'measure'),
    ],
    emptyText: search ? 'Nobody\'s name starts with that.' : 'No user is known yet: users arrive from the directory, or with their first prompt.',
  });
  return screen('person', 'Users', null, 'mart.v_person', {
    subtitle: 'Everyone the directory or a device has named. Open a user to see their usage and prompts.',
    search: Object.freeze({ value: search }),
    tiles: Object.freeze([]),
    tables: Object.freeze([
      Object.freeze({
        ...table,
        breakdowns: false,
        ...(next ? { href: `#person?${query({ cursor: next })}`, moreLabel: 'Show more' } : {}),
      }),
    ]),
    series: Object.freeze([]),
    ...shared(state, [
      'Users are listed as the directory names them, never ranked by use.',
      ...(state.data.length > 0 && named === 0
        ? ['No name is known for anyone on this page: the tenant keeps users hashed, or no directory has been read yet. Each is listed by reference.']
        : []),
      ...(roster ? ['Teams and prompts come from the rosters of the teams with usage in this window: a user in none of them shows a dash, not a zero.'] : []),
      ...teamMembersNotes(members),
    ]),
    // A list of people is current state, not a measurement: fleet coverage and aggregate age say
    // nothing about it, so their banners are kept for a page that came back without rows.
    ...(state.data.length > 0 ? { banners: Object.freeze([]) } : {}),
  });
}

const count = (v) => (typeof v === 'number' && Number.isFinite(v) ? v : Number(v) || 0);

/** The device's collectors that report an app running without the settings it reads at start. */
const restartCollectors = (row) => (Array.isArray(row.restart_required) ? row.restart_required.filter((c) => typeof c === 'string' && c !== '') : []);

/**
 * Which apps need a restart, in words: "Claude Code needs a restart". A tool_config_<key>
 * collector is named by its tool, any other by the collector; null when none needs one.
 */
function restartText(row) {
  const names = restartCollectors(row).map((c) => ENDPOINT_TOOLS.find((tool) => `tool_config_${tool.key}` === c)?.label ?? c);
  if (names.length === 0) return null;
  const list = names.length === 1 ? names[0] : `${names.slice(0, -1).join(', ')} and ${names[names.length - 1]}`;
  return `${list} ${names.length === 1 ? 'needs' : 'need'} a restart`;
}

/**
 * One device's state in the words a customer uses, from its liveness and the worst state among
 * its collectors. The key keeps the vocabulary value's colour. A device whose only degraded
 * collectors are apps waiting for a restart says so, under the degraded key.
 */
function deviceStatus(row) {
  if (row.liveness === 'revoked') return { key: 'revoked', text: 'Revoked' };
  if (count(row.collectors_tampered) > 0) return { key: 'tampered', text: 'Tampered' };
  if (row.liveness === 'never_reported') return { key: 'never_reported', text: 'Never checked in' };
  if (row.liveness === 'stale') return { key: 'stale', text: 'Quiet' };
  if (row.liveness === 'reporting') {
    const degraded = count(row.collectors_degraded);
    if (degraded === 0) return { key: 'reporting', text: 'Reporting' };
    return { key: 'degraded', text: degraded === restartCollectors(row).length ? 'Restart needed' : 'Degraded' };
  }
  return { key: 'unknown', text: 'Unknown' };
}

/** What a device's status means, beside its Status tile. */
const STATUS_INFO = Object.freeze({
  reporting: 'The agent checks in, and none of its collectors is degraded or tampered with.',
  stale: 'The agent has not checked in lately. The device may be switched off or offline, or its agent cannot reach the service; the collectors below are as it last reported them.',
  never_reported: 'The device enrolled, and its agent has not checked in since.',
  degraded: 'The agent checks in, but a collector below is not working as it should.',
  tampered: 'A collector below reports a change made outside the agent.',
  revoked: 'The device\'s enrolment was revoked, so it no longer checks in.',
  unknown: 'The device reported a state this console does not know.',
});

/** A device's collectors that are degraded or tampered with, as "1 collector degraded"; an app waiting for a restart is restartText's. */
function collectorProblems(row) {
  const tampered = count(row.collectors_tampered);
  const degraded = Math.max(0, count(row.collectors_degraded) - restartCollectors(row).length);
  return [
    tampered > 0 ? `${plural(tampered, 'collector', 'collectors')} tampered with` : null,
    degraded > 0 ? `${plural(degraded, 'collector', 'collectors')} degraded` : null,
  ].filter(Boolean).join(' · ') || null;
}

/** How long ago an instant was, in the coarsest unit that is still true. */
function ago(iso, now) {
  const ms = now.getTime() - Date.parse(iso);
  if (!Number.isFinite(ms) || ms < 0) return formatWhen(iso);
  const minutes = Math.floor(ms / 60_000);
  if (minutes < 1) return 'just now';
  if (minutes < 60) return `${minutes} min ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return `${hours} h ago`;
  return `${Math.floor(hours / 24)} days ago`;
}

const DEVICE_LABELS = Object.freeze({ windows: 'Windows', macos: 'macOS', linux: 'Linux', managed: 'Managed', unmanaged: 'Unmanaged', unknown: 'Unknown' });

/** Whether an MDM manages the device, as its page says it. */
const MANAGEMENT_TEXT = Object.freeze({ managed: 'Managed by MDM', unmanaged: 'Not managed by MDM', unknown: 'Not known' });

/** A device state in the words of the status filter and the attention split. */
const STATUS_LABELS = Object.freeze({ attention: 'Needs attention', reporting: 'Reporting', stale: 'Quiet', never_reported: 'Never checked in', degraded: 'Degraded', tampered: 'Tampered', revoked: 'Revoked' });
const ATTENTION_KEYS = Object.freeze(['stale', 'never_reported', 'degraded', 'tampered']);

/** What the device runs: "Windows 11"; null when it reported nothing. */
const osText = (row) => [DEVICE_LABELS[row.device_os] ?? (row.device_os ? String(row.device_os) : null), row.os_version ? String(row.os_version) : null].filter(Boolean).join(' ') || null;

/** The words beside a device's name: what it runs, whether it is managed, and its agent. */
function deviceFacts(row) {
  return [
    osText(row),
    DEVICE_LABELS[row.managed_state] ?? (row.managed_state ? String(row.managed_state) : null),
    row.agent_version ? `Agent ${row.agent_version}` : null,
  ].filter(Boolean).join(' · ') || null;
}

/** The collection mode a device reported it applied, in words; null when it reported none. */
const modeText = (row) => (row.collection_mode ? (COLLECTION_MODE_LABELS[row.collection_mode] ? `${COLLECTION_MODE_LABELS[row.collection_mode]} · ${COLLECTION_MODE_CODES[row.collection_mode]}` : `Mode ${row.collection_mode}`) : null);

/** One device's page. */
export const deviceHref = (device) => `#devices?device=${encodeURIComponent(String(device))}`;

/** One device's row, in the cells the list and the device's page both show. */
function deviceRow(row, now) {
  const status = deviceStatus(row);
  // The device name is the hostname, with the UUID kept for the hover and as the fallback. The
  // user is the clear account name of the most recent submission, with the pseudonymous ref as
  // the fallback. directory_name is the directory's own current name, shown above it when a
  // sync has supplied one; both are absent for a 'hashed' tenant.
  const user = row.subject_name ?? row.user_ref ?? null;
  const directoryName = row.directory_name ?? null;
  // When the device last checked in, with the time itself on hover; one that never has says when it enrolled.
  const seen = row.last_seen_at ? `Last seen ${ago(row.last_seen_at, now)}`
    : row.liveness === 'never_reported' && row.enrolled_at ? `Enrolled ${formatWhen(row.enrolled_at)}` : null;
  return {
    ...row,
    status: status.key,
    status_text: status.text,
    device_name: row.hostname || null,
    user,
    directory_name: directoryName,
    mode: modeText(row),
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
      secondary: [restartText(row), seen, collectorProblems(row)].filter(Boolean).join(' · ') || null,
      title: row.last_seen_at ? `Last seen ${formatInstant(row.last_seen_at)}` : null,
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
            column('mode', 'Collection mode'),
          ],
          emptyText: 'No device matches these filters.',
        }),
        rowHref: 'href',
        breakdowns: false,
      }),
    ]),
    series: Object.freeze([]),
    ...shared(state, [
      'Reporting, quiet, never checked in and revoked are four different facts, and none is inferred from silence. A quiet device has checked in before, but not lately.',
      'One row per device. Its status is the worst of its collectors\' states; open a device to see each collector.',
      'Collection mode is the mode the device reported it applied, which can trail the mode set in Settings until the device next checks in.',
    ]),
  });
}

/** Where a collector belongs on its device's page, in the order the groups are shown. */
const COLLECTOR_GROUPS = Object.freeze({ attention: 'Needs attention', working: 'Working', not_installed: 'Not installed', off: 'Off in policy' });

/**
 * A collector's group. A tool that is not installed, or that the agent does not set up on this
 * operating system, and a collector switched off by policy are not faults; any state but healthy
 * otherwise is.
 */
function collectorGroup(r) {
  if (r.error_code === 'disabled_by_policy') return 'off';
  if (r.collector_state === 'absent' && (r.error_code === 'tool_not_installed' || r.error_code === 'tool_version_unsupported')) return 'not_installed';
  return r.collector_state === 'healthy' ? 'working' : 'attention';
}

/** A collector's state in words: healthy reads as working and absent as not running; the key keeps its colour. */
const COLLECTOR_STATE_TEXT = Object.freeze({ healthy: 'Working', degraded: 'Degraded', absent: 'Not running', tampered: 'Tampered' });

/**
 * What each collector reported, grouped: those that need attention first, open, with the cause in
 * words and when each last worked; then those working, not installed and off in policy, each
 * folded under its heading. A collector is named by what it does, with its code on hover; one
 * this console has no name for is its code.
 */
function collectorRows(collectors) {
  const order = Object.keys(COLLECTOR_GROUPS);
  const rows = collectors.data.map((r) => {
    const group = collectorGroup(r);
    const code = r.collector ?? null;
    const known = code ? COLLECTORS[code] : null;
    const detail = group === 'attention' && r.error_code ? COLLECTOR_DETAILS[r.error_code] ?? String(r.error_code)
      : group === 'not_installed' && r.error_code === 'tool_version_unsupported' ? 'Not set up on this operating system' : null;
    return Object.freeze({
      group,
      name: known?.name ?? String(code ?? ''),
      collector: Object.freeze(known ? { primary: known.name, secondary: known.about, title: code } : { primary: code, mono: true }),
      state: group === 'off' ? 'disabled' : group === 'not_installed' ? 'not_installed' : r.collector_state ?? 'unknown',
      state_text: group === 'off' ? 'Off' : group === 'not_installed' ? 'Not installed' : COLLECTOR_STATE_TEXT[r.collector_state] ?? String(r.collector_state ?? 'Unknown'),
      // The cause in words, with the device's code kept on hover.
      detail: Object.freeze({
        primary: detail,
        secondary: group === 'attention' && r.last_success_at ? `Last worked ${formatWhen(r.last_success_at)}` : null,
        title: detail && r.error_code ? String(r.error_code) : null,
      }),
    });
  }).sort((a, b) => order.indexOf(a.group) - order.indexOf(b.group) || a.name.localeCompare(b.name));
  return Object.freeze({
    rows: Object.freeze(rows.map((r) => Object.freeze({ ...r, group: COLLECTOR_GROUPS[r.group] }))),
    collapsed: Object.freeze(order.filter((g) => g !== 'attention' && rows.some((r) => r.group === g)).map((g) => COLLECTOR_GROUPS[g])),
  });
}

/** Partial coverage is a fact about the fleet's figures, which one device's page holds none of. */
const deviceBanners = (state) => Object.freeze((state.banners ?? []).filter((b) => b.about !== 'coverage'));

/**
 * One device: its state and when it last checked in, the mode it applied, what it is and who last
 * used it, and every collector it reported, grouped by whether it needs attention. A device the
 * fleet list does not hold is said to be unknown.
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
      banners: deviceBanners(row),
    });
  }
  const read = collectors && !collectors.isRefusal;
  const grouped = read ? collectorRows(collectors) : null;
  // The status opens the collectors' table when it was read. The mode the device applied is set on
  // Settings → Collection.
  const below = read ? { jump: 'Collectors' } : {};
  const seen = found.last_seen_at ? `Last seen ${ago(found.last_seen_at, now)}, ${formatWhen(found.last_seen_at)}`
    : found.enrolled_at ? `Enrolled ${formatWhen(found.enrolled_at)}` : null;
  const mode = found.collection_mode ?? null;
  const tiles = [
    tile('Status', { kind: 'vocab', text: d.status_text, key: d.status }, seen, { ...below, info: STATUS_INFO[d.status] }),
    tile('Collection mode', mode ? { kind: 'vocab', text: COLLECTION_MODE_LABELS[mode] ?? `Mode ${mode}`, key: mode } : { kind: 'absent', text: '—' },
      mode ? COLLECTION_MODE_CODES[mode] ?? null : 'The device has not reported a mode.',
      { href: '#settings', info: 'The collection mode the device last reported it applied. It can trail the mode set in Settings until the device next checks in.' }),
  ];
  const facts = Object.freeze({
    kind: 'facts',
    title: 'About this device',
    items: Object.freeze([
      Object.freeze({ label: 'Last used by', text: d.user_cell.primary, secondary: d.user_cell.secondary, href: found.user_ref ? `#person?subject=${encodeURIComponent(found.user_ref)}` : null }),
      Object.freeze({ label: 'Operating system', text: osText(found) }),
      Object.freeze({ label: 'Device management', text: MANAGEMENT_TEXT[found.managed_state] ?? found.managed_state ?? null }),
      Object.freeze({ label: 'Agent version', text: found.agent_version ?? null }),
      Object.freeze({ label: 'Enrolled', text: found.enrolled_at ? formatWhen(found.enrolled_at) : null, title: found.enrolled_at ? formatInstant(found.enrolled_at) : null }),
      Object.freeze({ label: 'Device ID', text: device, mono: true }),
    ]),
  });
  const notes = [];
  if (collectors?.isRefusal) notes.push(`The collectors could not be read: ${collectors.error?.message ?? collectors.resultState}.`);
  return screen('device', d.device_cell.primary ?? device, null, 'mart.v_device_liveness', {
    subtitle: null,
    actions,
    tiles: Object.freeze(tiles),
    compactTiles: true,
    blocks: Object.freeze([facts]),
    tables: Object.freeze(read ? [
      Object.freeze({
        ...tableFrom({ data: grouped.rows }, {
          title: 'Collectors',
          columns: [
            column('group', 'Group'),
            column('collector', 'Collector', 'stack'),
            column('state', 'State', 'status'),
            column('detail', 'Detail', 'stack'),
          ],
          emptyText: 'This device has not reported any collector.',
        }),
        // A quiet device's collectors are as it last reported them.
        ...(found.liveness === 'stale' && found.last_seen_at ? { summary: `As the device last reported them, ${formatWhen(found.last_seen_at)}.` } : {}),
        groupBy: 'group',
        collapsed: grouped.collapsed,
        breakdowns: false,
      }),
    ] : []),
    series: Object.freeze([]),
    // When the device last checked in, for its diagnostics: a request waits for the next check-in.
    quietSince: found.liveness === 'stale' ? found.last_seen_at ?? null : null,
    ...shared(row, [
      ...notes,
      'A collector for a tool that is not installed, or one switched off by policy, is not a fault. Every other state is the one the device last reported.',
    ]),
    banners: deviceBanners(row),
  });
}

/** The audit trail's filters, as q10 takes them. */
const AUDIT_FIELDS = Object.freeze([
  Object.freeze({ name: 'actor', label: 'Actor', placeholder: 'Who made the read' }),
  Object.freeze({ name: 'action', label: 'Action', placeholder: 'Such as query.events' }),
  Object.freeze({ name: 'object_type', label: 'Object type', placeholder: 'Such as ingest.submission' }),
  Object.freeze({ name: 'case', label: 'Case', placeholder: 'A case reference' }),
]);

/** What an audit entry did, in words: the report it read, the decision it made, else its action's sentence. */
function auditSentence(row) {
  const action = String(row.action ?? '');
  if (action === 'query.aggregate' && AUDIT_REPORTS[row.object_type]) return `Read ${AUDIT_REPORTS[row.object_type]}`;
  if (action === 'tool.sanction' && row.object_id && SANCTION_TEXT[row.detail?.new]) {
    return row.detail.new === 'unknown' ? `Cleared the decision on ${row.object_id}` : `Marked ${row.object_id} ${SANCTION_TEXT[row.detail.new].toLowerCase()}`;
  }
  return AUDIT_ACTIONS[action] ?? null;
}

/** An audit entry's row: its action as a sentence with the code beneath, its object with its type beneath. */
function auditRow(row) {
  const sentence = auditSentence(row);
  const code = row.action === null || row.action === undefined ? null : String(row.action);
  return Object.freeze({
    ...row,
    action_cell: Object.freeze(sentence ? { primary: sentence, secondary: code } : { primary: code, mono: true }),
    object_cell: Object.freeze({ primary: row.object_id ?? null, mono: true, secondary: row.object_type ?? null }),
  });
}

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
      tile('Entries', { kind: 'number', text: formatCount(state.data.length) }, 'in this page', { jump: 'Audit entries' }),
      tile('Integrity', state.resultState === 'audit_chain_broken' ? { kind: 'vocab', text: 'Broken', key: 'broken' } : { kind: 'vocab', text: 'Checks out', key: 'ok' },
        state.resultState === 'audit_chain_broken' ? 'The chain on this page is broken' : 'Every entry is chained to the one before', {
          jump: 'Audit entries',
          info: 'Each entry carries the hash of the one before it. The links between the entries on this page were checked; the whole chain is verified by the reconciler.',
        }),
    ]),
    tables: Object.freeze([
      Object.freeze({
        ...tableFrom({ ...state, data: state.data.map(auditRow) }, {
          title: 'Audit entries',
          columns: [
            column('occurred_at', 'When', 'instant'),
            column('actor', 'Actor', 'actor'),
            column('action_cell', 'Action', 'stack'),
            column('object_cell', 'Object', 'stack'),
            column('subject', 'User', 'person'),
            column('case', 'Case'),
          ],
          emptyText: filtered ? 'Nothing matches these filters in this window.' : 'Nothing has been accessed in this window.',
        }),
        ...(next ? { href: at({ ...given, cursor: next }), moreLabel: 'Show more' } : {}),
        breakdowns: false,
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
      tile('Content state', { kind: 'vocab', text: contentState }, null, { jump: 'Metadata', info: contentAnswer }),
      tile('Routes', { kind: 'number', text: formatCount(head?.observation_count ?? observations.length) }, null,
        { jump: 'Observation routes', info: 'The routes that collected this event, one observation each, so an overlapping count can be explained.' }),
      tile('Mode', { kind: 'vocab', text: head?.collection_mode ?? '—' }, null, { href: '#settings', info: 'The collection mode the device applied when it collected this event.' }),
      tile('Merge confidence', { kind: 'vocab', text: head?.merge_confidence ?? '—' }, null, {
        jump: 'Observation routes',
        info: head?.merge_confidence === 'low'
          ? 'A weak dedup key produced this row: counted, never discarded.'
          : 'How surely the observations of this event were matched as one: a strong dedup key is high, a weak one low.',
      }),
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
      tile('Result state', { kind: 'vocab', text: state.resultState }, state.spec?.label ?? null, { jump: 'What the API said' }),
      tile('Rows served', { kind: 'number', text: '0' }, null, { jump: 'What the API said', info: 'A refusal carries no data, by construction.' }),
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
