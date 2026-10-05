// explore-render.js — the Explore page's state to HTML. Pure: no DOM access, no I/O, no state.
//
// Every string that came from the API is escaped. The honesty rules the rest of the dashboard
// keeps are kept here the same way:
//   * both clocks are shown and the device clock is marked as possibly skewed (§14 item 10);
//   * a missing value says what is missing ("none", "unknown", "not classified") and is never a
//     blank, because a blank and an unknown look identical and mean different things;
//   * rows are shown in the order the server returned them, with no client-side sort, so no list
//     here can be turned into a ranking of people (§14 item 1);
//   * a record's detail shows metadata and what its content state permits. Content appears only
//     after a retrieval the content vault approved for that record (docs/02 §11), and never from
//     a query: no query path returns content (§14.3).

import { escapeHtml } from './render.js';
import { formatBytes, formatCount, formatDuration, formatInstant, formatScore } from './format.js';
import { coverageText, freshnessText, emptyStateFor, fixSentence } from './states.js';
import { eventView } from './views.js';
import { EXPLORE_DATASETS, EXPLORE_DATASET_IDS, exploreWindows } from './explore-model.js';

/** Which values of which field get a tint. Only real warning and fault states are tinted. */
const EXPLORE_TONES = Object.freeze({
  action: Object.freeze({ blocked: 'bad', warned: 'warn' }),
  severity: Object.freeze({ critical: 'bad', high: 'warn' }),
  liveness: Object.freeze({ revoked: 'bad', never_reported: 'bad', stale: 'warn' }),
  collector_state: Object.freeze({ tampered: 'bad', absent: 'bad', degraded: 'warn' }),
  review_state: Object.freeze({ open: 'warn' }),
  merge_confidence: Object.freeze({ low: 'warn' }),
  confidence: Object.freeze({ degraded: 'warn' }),
});

/** What an absent value of a field is called. Anything not listed is "not recorded". */
const EXPLORE_ABSENT = Object.freeze({
  action: 'none',
  case: 'none',
  subject: 'none',
  object_id: 'none',
  collector: 'none',
  collector_state: 'unknown',
  last_seen_at: 'never',
  confidence: 'not classified',
});

const DEVICE_CLOCK_TITLE = 'The device clock: possibly skewed, never normalised into server time';
const LATE_FLUSH_MS = 3_600_000;

function exploreAbsent(key) {
  return `<span class="x-absent">${escapeHtml(EXPLORE_ABSENT[key] ?? 'not recorded')}</span>`;
}

function exploreChip(key, value) {
  const tone = EXPLORE_TONES[key]?.[value];
  return `<span class="x-chip${tone ? ` x-chip-${tone}` : ''}">${escapeHtml(value)}</span>`;
}

function exploreClasses(labels) {
  if (labels === null || labels === undefined) return '<span class="x-absent">not classified</span>';
  if (!Array.isArray(labels) || labels.length === 0) return '<span class="x-absent">none found</span>';
  return labels.map((l) => `<span class="x-chip">${escapeHtml(typeof l === 'string' ? l : l.class)}</span>`).join(' ');
}

/** One value, rendered by the kind its column declares. */
export function renderExploreValue(row, column, { full = false } = {}) {
  const raw = row[column.key];
  if (column.kind === 'classes') return exploreClasses(raw);
  if (raw === null || raw === undefined || raw === '') return exploreAbsent(column.key);
  switch (column.kind) {
    case 'server-clock':
      return `<time class="x-time" datetime="${escapeHtml(raw)}">${escapeHtml(formatInstant(raw))}</time>`;
    case 'device-clock': {
      const lag = row.received_at ? Date.parse(row.received_at) - Date.parse(raw) : 0;
      const late = lag > LATE_FLUSH_MS
        ? ` <span class="x-lag" title="Received long after the device clock said it happened: a spool flush, not a burst of activity">${escapeHtml(formatDuration(raw, row.received_at))} before receipt</span>`
        : '';
      return `<time class="x-time x-device-clock" datetime="${escapeHtml(raw)}" title="${DEVICE_CLOCK_TITLE}">${escapeHtml(formatInstant(raw))}</time>${late}`;
    }
    case 'vocab':
      return exploreChip(column.key, String(raw));
    case 'count':
      return `<span class="x-num">${escapeHtml(formatCount(Number(raw)))}</span>`;
    case 'id':
      return full
        ? `<span class="x-mono">${escapeHtml(raw)}</span>`
        : `<span class="x-mono" title="${escapeHtml(raw)}">${escapeHtml(String(raw).slice(0, 8))}</span>`;
    case 'rule':
      return `<span class="x-rule">${escapeHtml(raw)}</span>${row.rule ? ` <span class="x-mono x-sub">${escapeHtml(row.rule)}</span>` : ''}`;
    case 'mono':
      return `<span class="x-mono">${escapeHtml(raw)}</span>`;
    default:
      return `<span>${escapeHtml(raw)}</span>`;
  }
}

export function renderExploreDatasets(state) {
  return EXPLORE_DATASET_IDS.filter((id) => id !== 'audit').map((id) => {
    const on = id === state.dataset;
    return `<button type="button" class="x-seg-item" data-act="dataset" data-dataset="${id}" aria-pressed="${on}">${escapeHtml(EXPLORE_DATASETS[id].label)}</button>`;
  }).join('');
}

export function renderExploreWindow(state) {
  const dataset = EXPLORE_DATASETS[state.dataset];
  const windows = exploreWindows(dataset);
  if (windows.length === 0) return '<span class="x-window-none">Current state, so no time window</span>';
  return windows.map((w) => (
    `<button type="button" class="x-seg-item" data-act="window" data-window="${w.id}" aria-pressed="${w.id === state.windowPreset}">${escapeHtml(w.label)}</button>`
  )).join('');
}

/** Completions under the query bar. Each one is a button, so it works by pointer and by keyboard. */
export function renderExploreSuggestions(suggestion) {
  if (!suggestion || suggestion.items.length === 0) return '';
  const lead = suggestion.items[0].kind === 'field' ? 'Fields' : 'Values';
  return `<span class="x-suggest-lead">${lead}</span>` + suggestion.items.map((item, i) => (
    `<button type="button" class="x-suggest-item" data-act="suggest" data-index="${i}" title="${escapeHtml(item.title ?? item.label)}">${escapeHtml(item.label)}</button>`
  )).join('');
}

export function renderExploreProblems(state) {
  if (state.problems.length === 0) return '';
  const items = state.problems.map((p) => (
    `<li><strong>${escapeHtml(p.message)}</strong> <span>${escapeHtml(p.fix)}</span></li>`
  )).join('');
  return `<div class="x-problems"><p class="x-problems-head">Not searched. Nothing is dropped from a query, so fix ${state.problems.length === 1 ? 'this' : 'these'} first:</p><ul>${items}</ul></div>`;
}

/** The filters, as one row: a list for a field with known values, a short text field otherwise. */
export function renderExploreRail(state) {
  const dataset = EXPLORE_DATASETS[state.dataset];
  const active = dataset.fields.filter((f) => state.filters[f.name]).length;
  const fields = dataset.fields.map((field) => {
    const value = state.filters[field.name] ?? '';
    const id = `x-f-${field.name}`;
    const on = value ? ' x-filter-on' : '';
    if (field.values) {
      const values = field.values.includes(value) || !value ? field.values : [...field.values, value];
      const options = values.map((v) => (
        `<option value="${escapeHtml(v)}"${v === value ? ' selected' : ''}>${v === value ? `${escapeHtml(field.label)}: ` : ''}${escapeHtml(v)}</option>`
      )).join('');
      return `<select class="x-select x-filter${on}" id="${id}" data-field="${field.name}" aria-label="${escapeHtml(field.label)}"><option value="">${escapeHtml(field.label)}</option>${options}</select>`;
    }
    return `<input class="x-input x-filter${on}" id="${id}" type="text" data-field="${field.name}" value="${escapeHtml(value)}" autocomplete="off" spellcheck="false" placeholder="${escapeHtml(field.label)}" aria-label="${escapeHtml(field.label)}" title="${escapeHtml(field.hint ?? field.label)}">`;
  }).join('');
  return fields + (active > 0 ? `<button type="button" class="x-link" data-act="clear">Clear ${active}</button>` : '');
}

function exploreNoun(dataset, count) {
  if (count === 1) return dataset.noun;
  return dataset.noun.endsWith('y') ? `${dataset.noun.slice(0, -1)}ies` : `${dataset.noun}s`;
}

/** What is on screen, how it is ordered, how far it reaches, and the audit entry that recorded it. */
export function renderExploreSummary(state) {
  const dataset = EXPLORE_DATASETS[state.dataset];
  if (state.status !== 'ready' || !state.result) return '';
  const more = Boolean(state.result.page?.next_cursor);
  const parts = [
    `<strong>${escapeHtml(formatCount(state.rows.length))} ${escapeHtml(exploreNoun(dataset, state.rows.length))}</strong> loaded${more ? ', more available' : ''}`,
  ];
  if (state.result.audit?.entry_id) parts.push(`Audited read, entry ${escapeHtml(state.result.audit.entry_id)}`);
  const newer = state.result.page?.newer_events_exist
    ? '<button type="button" class="x-link" data-act="run">Newer rows have arrived. Search again</button>'
    : '';
  return `<p class="x-summary-line">${parts.map((p) => `<span>${p}</span>`).join('')}${newer}</p>`;
}

function exploreHead(dataset) {
  return `<thead><tr>${dataset.columns.map((c) => `<th scope="col">${escapeHtml(c.label)}</th>`).join('')}</tr></thead>`;
}

function exploreSkeleton(dataset) {
  const widths = [72, 64, 40, 56, 44, 60, 48, 36, 28];
  const rows = Array.from({ length: 8 }, (_, r) => (
    `<tr>${dataset.columns.map((_c, i) => `<td><span class="x-skel" style="width:${widths[(i + r) % widths.length]}%"></span></td>`).join('')}</tr>`
  )).join('');
  return `<table class="x-table" aria-hidden="true">${exploreHead(dataset)}<tbody>${rows}</tbody></table><p class="x-sr" role="status">Searching</p>`;
}

/** The message, the fix the API named, and the one action that makes sense for a refusal. */
function exploreRefusal(result, { again = 'Search again' } = {}) {
  const banner = result.banners[0] ?? { title: 'The read was refused', text: result.error?.message ?? result.resultState };
  const fix = fixSentence({ error: result.error });
  const showFix = fix && !banner.text.includes(fix);
  const label = result.resultState === 'busy' ? 'Retry' : result.resultState === 'cursor_expired' ? 'Restart from page one' : again;
  return `<div class="x-state x-state-refusal" role="alert"><h3>${escapeHtml(banner.title)}</h3>`
    + `<p>${escapeHtml(banner.text)}</p>`
    + (showFix ? `<p>${escapeHtml(fix)}</p>` : '')
    + `<p class="x-state-code">${escapeHtml(result.resultState)}${result.error?.code ? `, ${escapeHtml(result.error.code)}` : ''}. No rows were served.</p>`
    + `<button type="button" class="x-btn" data-act="run">${label}</button></div>`;
}

function exploreEmpty(state, dataset) {
  const known = emptyStateFor(state.result) ?? { title: 'No rows', text: 'The read succeeded and returned nothing.' };
  const active = dataset.fields.filter((f) => state.filters[f.name]).length;
  const widest = dataset.windows[dataset.windows.length - 1];
  const actions = [
    active > 0 ? `<button type="button" class="x-btn" data-act="clear">Clear ${active} filter${active === 1 ? '' : 's'}</button>` : '',
    widest && widest !== state.windowPreset
      ? `<button type="button" class="x-btn x-btn-quiet" data-act="window" data-window="${widest}">Widen the window</button>`
      : '',
  ].join('');
  return `<div class="x-state"><h3>${escapeHtml(known.title)}</h3><p>${escapeHtml(known.text)}</p>`
    + (actions ? `<div class="x-state-actions">${actions}</div>` : '')
    + '</div>';
}

export function renderExploreResults(state) {
  const dataset = EXPLORE_DATASETS[state.dataset];
  if (state.status === 'blocked') {
    return '<div class="x-state"><h3>Not searched</h3><p>The query has something this page cannot place. Fix it above and search again. Nothing was sent.</p></div>';
  }
  if (state.status === 'loading' || state.status === 'idle') return exploreSkeleton(dataset);
  if (state.status === 'refused') return exploreRefusal(state.result);
  if (state.rows.length === 0) return exploreEmpty(state, dataset);

  const body = state.rows.map((row) => {
    const key = dataset.rowKey(row);
    const cells = dataset.columns.map((c) => `<td>${renderExploreValue(row, c)}</td>`).join('');
    return `<tr class="x-row" tabindex="0" data-act="open" data-key="${escapeHtml(key)}" aria-selected="${key === state.detail.key}">${cells}</tr>`;
  }).join('');

  let foot;
  if (state.moreProblem) {
    foot = exploreRefusal(state.moreProblem);
  } else if (state.result.page?.next_cursor) {
    foot = `<button type="button" class="x-btn" data-act="more"${state.loadingMore ? ' disabled' : ''}>${state.loadingMore ? 'Loading' : 'Load next page'}</button>`;
  } else {
    foot = `<p class="x-end">End of results.</p>`;
  }
  return `<table class="x-table"><caption class="x-sr">${escapeHtml(dataset.label)}. ${escapeHtml(dataset.ordering)}. Press Enter on a row for its detail.</caption>`
    + `${exploreHead(dataset)}<tbody>${body}</tbody></table><div class="x-more">${foot}</div>`;
}

function exploreFacts(pairs) {
  return `<dl class="x-facts">${pairs.map(([label, html]) => `<div><dt>${escapeHtml(label)}</dt><dd>${html}</dd></div>`).join('')}</dl>`;
}

function exploreFilterButton(dataset, field, value) {
  if (!value || !dataset.fields.some((f) => f.name === field)) return '';
  return ` <button type="button" class="x-link" data-act="filter" data-field="${field}" data-value="${escapeHtml(value)}">Filter to this</button>`;
}

function exploreDetailHead(title, id) {
  return `<header class="x-detail-head"><div><h2 id="x-detail-title" tabindex="-1">${escapeHtml(title)}</h2>`
    + (id ? `<p class="x-mono x-detail-id">${escapeHtml(id)}</p>` : '')
    + '</div><button type="button" class="x-btn x-btn-quiet" data-act="close">Close</button></header>';
}

function exploreFindingFacts(row) {
  const col = (key, kind) => ({ key, kind });
  return '<section><h3>Finding</h3>' + exploreFacts([
    ['Rule', renderExploreValue(row, col('rule_title', 'rule'))],
    ['Severity at detection', renderExploreValue(row, col('severity', 'vocab'))],
    ['Class', renderExploreValue(row, col('class', 'vocab'))],
    ['Review', `${renderExploreValue(row, col('review_state', 'vocab'))}${row.review_state === 'open' ? ' <span class="x-sub">Nobody has looked yet</span>' : ''}`],
    ['Detected', renderExploreValue(row, col('detected_at', 'server-clock'))],
    ['Decided on the device', `<span>${row.decided_locally ? 'yes' : 'no'}</span>`],
  ]) + '</section>';
}

/** A vault refusal or a transport failure: its reason, in the vault's own code where it has one. */
function exploreContentProblem(problem) {
  return `<div class="x-content-problem" role="alert"><p>${escapeHtml(problem?.message || 'The request was not served.')}</p>`
    + (problem?.code ? `<p class="x-state-code">${escapeHtml(problem.code)}</p>` : '') + '</div>';
}

/** Who typed a matching prompt, on which device, into which tool. A part the answer lacks is left out. */
function exploreHitMeta(hit) {
  // Prefer the device hostname to the UUID (ADR 0021): an analyst places a machine by its name, and
  // the UUID is still the identity behind the "open the event" action.
  const device = typeof hit.hostname === 'string' && hit.hostname !== '' ? hit.hostname : hit.device;
  const parts = [hit.subject, device, hit.tool].filter((part) => typeof part === 'string' && part !== '');
  if (parts.length === 0) return `<span class="x-mono x-sub">${escapeHtml(hit.submissionId)}</span>`;
  return `<span class="x-hit-meta x-mono">${parts.map((part) => `<span>${escapeHtml(part)}</span>`).join('<span class="x-hit-sep" aria-hidden="true">|</span>')}</span>`;
}

/**
 * The prompt-text search: its hits, or why there are none. It sits apart from the list because it
 * is a different read. A hit is a fragment and a reference; opening it reads the event.
 */
export function renderExploreText(state) {
  const text = state.text;
  if (text.status === 'idle') return '';
  const head = `<div class="x-text-head"><h2>Prompts containing <span class="x-mono">${escapeHtml(text.query)}</span></h2>`
    + '<button type="button" class="x-btn x-btn-quiet" data-act="text-clear">Back to the list</button></div>';
  if (text.status === 'loading') return `<div class="x-text-panel" aria-busy="true">${head}<p class="x-sub" role="status">Searching</p></div>`;
  if (text.status === 'refused') return `<div class="x-text-panel">${head}${exploreContentProblem(text.problem)}</div>`;
  if (text.hits.length === 0) {
    return `<div class="x-text-panel">${head}<p class="x-sub">No uploaded prompt contains every one of those words.</p></div>`;
  }
  const rows = text.hits.map((hit) => {
    // The fragment is escaped whole; only the search's own highlight marks are put back.
    const snippet = escapeHtml(hit.snippet).replaceAll('&lt;em&gt;', '<em>').replaceAll('&lt;/em&gt;', '</em>');
    return `<li><button type="button" class="x-hit" data-act="hit" data-submission="${escapeHtml(hit.submissionId)}" aria-pressed="${hit.submissionId === state.detail.submissionId}">`
      + `<span class="x-hit-snippet">${snippet}</span>${exploreHitMeta(hit)}</button></li>`;
  }).join('');
  return `<div class="x-text-panel">${head}<ul class="x-hits">${rows}</ul>`
    + (text.truncated ? '<p class="x-sub">More prompts match than are shown; add a word to narrow it.</p>' : '') + '</div>';
}

/**
 * What the content state permits, for the open record. Stored content is read when the record is
 * opened and shown here: what the person typed first, the whole capture behind a disclosure. It is
 * dropped when the record is closed.
 */
function exploreContent(state, contentState, note) {
  const content = state.content;
  const chip = `<p>${exploreChip('content_state', contentState)}</p>`;
  if (contentState !== 'uploaded') return `<section class="x-content"><h3>Prompt</h3>${chip}<p>${escapeHtml(note)}</p></section>`;
  if (!state.contentAvailable) {
    return `<section class="x-content"><h3>Prompt</h3>${chip}<p class="x-sub">This page has no content path behind it, so the prompt cannot be read here.</p></section>`;
  }

  if (content.status === 'ready') {
    const typed = content.typed
      ? `<pre class="x-pre x-typed">${escapeHtml(content.typed)}</pre>`
      : `<p class="x-absent">Nothing typed: this capture is ${escapeHtml({ prompt: 'a prompt', internal: 'client telemetry', other: 'a request with no user message' }[content.kind] ?? 'not a prompt')}.</p>`;
    return `<section class="x-content"><h3>Prompt</h3>${typed}`
      + `<details class="x-capture"><summary>Everything captured for this request (${escapeHtml(formatBytes(content.bytes))})</summary><pre class="x-pre">${escapeHtml(content.full)}</pre></details>`
      + '</section>';
  }
  if (content.status === 'gone') {
    return `<section class="x-content"><h3>Prompt</h3>${chip}<div class="x-content-problem"><p>The content is no longer available${content.gone?.reason ? `: ${escapeHtml(content.gone.reason)}` : ''}.</p>`
      + (content.gone?.receipt ? `<p class="x-state-code">receipt ${escapeHtml(content.gone.receipt)}</p>` : '') + '</div></section>';
  }
  if (content.status === 'refused') {
    return `<section class="x-content"><h3>Prompt</h3>${exploreContentProblem(content.problem)}`
      + '<button type="button" class="x-btn" data-act="retrieve">Try again</button></section>';
  }
  return '<section class="x-content" aria-busy="true"><h3>Prompt</h3>'
    + '<div class="x-detail-skel" aria-hidden="true"><span class="x-skel" style="width:82%"></span><span class="x-skel" style="width:58%"></span></div>'
    + '<p class="x-sr" role="status">Reading the prompt</p></section>';
}

/** One submission, read on its own (Q9): metadata, both clocks, routes, and the content answer. */
function exploreRecord(detail, dataset, state) {
  const record = detail.record;
  const first = record.data[0];
  // The API answers with one row per observation, each repeating the submission's columns under
  // the store's own names (user_ref, tool_fingerprint, collection_mode, policy_action). A list row
  // names the same things subject, tool, mode and action, and carries the device, so the head is
  // read through both spellings rather than shown as "not recorded" when only one is present.
  const head = {
    ...first,
    subject: first.subject ?? first.user_ref ?? detail.row?.subject,
    tool: first.tool ?? first.tool_fingerprint ?? detail.row?.tool,
    device: first.device ?? first.device_id ?? detail.row?.device,
    department: first.department ?? detail.row?.department,
    mode: first.mode ?? first.collection_mode,
    action: first.action ?? first.policy_action,
    detection_basis: first.detection_basis ?? first.kind ?? detail.row?.detection_basis,
  };
  const observations = record.data.filter((row, i) => (row.observation_event_id ? true : i > 0));
  const view = eventView(record);
  const contentTile = view.tiles[0];
  const col = (key, kind) => ({ key, kind });
  const labels = Array.isArray(head.labels) && head.labels.length > 0
    ? head.labels.map((l) => `<span class="x-chip">${escapeHtml(l.class)}</span>${typeof l.score === 'number' ? ` <span class="x-sub">score ${escapeHtml(formatScore(l.score))}</span>` : ''}`).join('<br>')
    : exploreClasses(head.labels);

  const routes = observations.length === 0
    ? '<p class="x-absent">This response carries no observation rows.</p>'
    : `<table class="x-table x-table-plain"><thead><tr><th scope="col">Route</th><th scope="col">Kind</th><th scope="col">Direction</th><th scope="col">Occurred (device)</th><th scope="col">Size</th></tr></thead><tbody>${observations.map((o) => (
      `<tr><td><span class="x-mono">${escapeHtml(o.observation_source ?? 'unknown')}</span></td>`
      + `<td>${escapeHtml(o.observation_kind ?? 'unknown')}</td><td>${escapeHtml(o.direction ?? 'unknown')}</td>`
      + `<td>${o.observation_occurred_at ? `<time class="x-time x-device-clock" title="${DEVICE_CLOCK_TITLE}">${escapeHtml(formatInstant(o.observation_occurred_at))}</time>` : exploreAbsent('observation_occurred_at')}</td>`
      + `<td>${typeof o.observation_size_bytes === 'number' ? escapeHtml(formatBytes(o.observation_size_bytes)) : exploreAbsent('size')}</td></tr>`
    )).join('')}</tbody></table>`;

  return (detail.row && 'rule' in detail.row ? exploreFindingFacts(detail.row) : '')
    + exploreContent(state, String(contentTile.value.text), contentTile.note)
    + '<section><h3>Who and where</h3>' + exploreFacts([
      ['Person', `${renderExploreValue(head, col('subject', 'mono'))}${exploreFilterButton(dataset, 'subject', head.subject)}`],
      ['Tool', `${renderExploreValue(head, col('tool', 'mono'))}${exploreFilterButton(dataset, 'tool', head.tool)}`],
      ['Device', `${renderExploreValue(head, col('device', 'id'), { full: true })}${exploreFilterButton(dataset, 'device', head.device)}`],
      ...(head.department ? [['Department', `<span>${escapeHtml(head.department)}</span>${exploreFilterButton(dataset, 'department', head.department)}`]] : []),
    ]) + '</section>'
    + '<section><h3>What was decided</h3>' + exploreFacts([
      ['Policy action', renderExploreValue(head, col('action', 'vocab'))],
      ['Classes', labels],
      ['Classifier confidence', renderExploreValue(head, col('confidence', 'vocab'))],
      ['Collection mode', renderExploreValue(head, col('mode', 'vocab'))],
      ['Detection basis', renderExploreValue(head, col('detection_basis', 'mono'))],
      ['Size', typeof head.size_bytes === 'number' ? `<span class="x-num">${escapeHtml(formatBytes(head.size_bytes))}</span>` : exploreAbsent('size_bytes')],
    ]) + '</section>'
    + '<section><h3>Two clocks</h3>' + exploreFacts([
      ['Received (server)', renderExploreValue(head, col('received_at', 'server-clock'))],
      ['First occurred (device)', renderExploreValue(head, col('first_occurred_at', 'device-clock'))],
      ['Last occurred (device)', head.last_occurred_at
        ? `<time class="x-time x-device-clock" title="${DEVICE_CLOCK_TITLE}">${escapeHtml(formatInstant(head.last_occurred_at))}</time>`
        : exploreAbsent('last_occurred_at')],
    ]) + '<p class="x-sub">Neither clock is converted into the other. The device clock may be skewed.</p></section>'
    + `<section><h3>Observation routes</h3>${routes}`
    + (head.merge_confidence === 'low' ? '<p class="x-sub">Merge confidence is low: a weak key joined these observations. The row is counted, not discarded.</p>' : '')
    + '</section>'
    + (record.audit?.entry_id ? `<p class="x-audit">Opening this record was an audited read: entry ${escapeHtml(record.audit.entry_id)} at ${escapeHtml(formatInstant(record.audit.written_at))}.</p>` : '');
}

/** A device or audit row: the row already held, laid out in full. No second read is made. */
function exploreRowDetail(row, dataset) {
  const facts = dataset.detailFields.map((c) => [
    c.label,
    `${renderExploreValue(row, c, { full: true })}${exploreFilterButton(dataset, c.key, typeof row[c.key] === 'string' ? row[c.key] : null)}`,
  ]);
  const extra = row.detail && typeof row.detail === 'object' && Object.keys(row.detail).length > 0
    ? `<section><h3>Recorded detail</h3><pre class="x-pre">${escapeHtml(JSON.stringify(row.detail, null, 2))}</pre></section>`
    : '';
  return `<section>${exploreFacts(facts)}</section>${extra}`;
}

export function renderExploreDetail(state) {
  const detail = state.detail;
  if (detail.status === 'closed') return '';
  const dataset = EXPLORE_DATASETS[state.dataset];
  const title = dataset.noun.charAt(0).toUpperCase() + dataset.noun.slice(1);

  if (dataset.detail === 'row') {
    return exploreDetailHead(title, null) + exploreRowDetail(detail.row, dataset);
  }
  const id = detail.submissionId;
  if (detail.status === 'loading') {
    return exploreDetailHead('Event', id)
      + `<div class="x-detail-skel" aria-hidden="true">${Array.from({ length: 9 }, (_, i) => `<span class="x-skel" style="width:${[88, 62, 74, 40, 81, 56, 69, 47, 77][i]}%"></span>`).join('')}</div>`
      + '<p class="x-sr" role="status">Reading the record</p>';
  }
  if (detail.status === 'refused') {
    const record = detail.record;
    const banner = record.banners[0] ?? { title: 'The record was not served', text: record.error?.message ?? record.resultState };
    const receipt = record.error?.receipt;
    return exploreDetailHead('Event', id)
      + `<div class="x-state x-state-refusal" role="alert"><h3>${escapeHtml(banner.title)}</h3><p>${escapeHtml(banner.text)}</p>`
      + (receipt ? exploreFacts([
        ['Receipt', `<span class="x-mono">${escapeHtml(receipt.receipt_id)}</span>`],
        ['Scope', `<span>${escapeHtml(receipt.scope_kind)}</span>`],
        ['Completed', `<time class="x-time">${escapeHtml(formatInstant(receipt.completed_at))}</time>`],
        ['Mechanisms', `<span class="x-mono">${escapeHtml((receipt.mechanisms ?? []).join(', '))}</span>`],
      ]) : '')
      + `<p class="x-state-code">${escapeHtml(record.resultState)}${record.error?.code ? `, ${escapeHtml(record.error.code)}` : ''}</p></div>`
      + (detail.row && 'rule' in detail.row ? exploreFindingFacts(detail.row) : '');
  }
  return exploreDetailHead('Event', id) + exploreRecord(detail, dataset, state);
}
