// explore-model.js — what the Explore page can search, and how a typed query becomes closed filters.
//
// Explore is one page over the four list reads of the DSL: events (Q8), findings (Q5), devices (Q7)
// and the audit trail (Q10). It adds no read path. Every request it makes is one of the existing
// template builders in questions.js, so it cannot ask anything the DSL does not admit.
//
// The query bar is a convenience over the same closed filters the rail shows: `tool:tls_b6681b043244c43f
// action:blocked`. It is parsed here, and anything it cannot place is a *problem* that blocks the
// search rather than a token that is dropped: no unrecognised filter is silently ignored. A bare
// word is the commonest case. It is refused with the reason, because this API has no text
// predicate and a search box that quietly ignored free text would look like "no matches".
//
// Nothing here ranks or sorts. A list source has one fixed total ordering, set by the server, and
// the page shows rows in the order they arrive.

import { WINDOWS } from './vocab.js';
import { QUESTIONS, context } from './questions.js';
import { buildListDocument } from './dsl.js';

const DATA_CLASSES = Object.freeze(['payment_card', 'government_id', 'credential', 'customer_pii', 'source_code', 'legal_commercial', 'health']);
const SEVERITIES = Object.freeze(['low', 'medium', 'high', 'critical']);
const COLLECTION_MODES = Object.freeze(['m0', 'm1', 'm2', 'm3']);

/** Hash keys that carry page state rather than a filter. No dataset has a field with these names. */
const RESERVED_KEYS = Object.freeze(['window', 'open', 'include']);

/** Page sizes a prompt-text search offers. */
export const TEXT_PAGE_SIZES = Object.freeze([5, 10, 20]);

/** The longest string value the API admits. */
const MAX_VALUE_LENGTH = 256;

/**
 * A filter field. `values` with `closed: true` is a vocabulary the API enforces, so anything else
 * is refused here first; `values` with `closed: false` is a list of suggestions only.
 */
function exploreField(name, label, opts = {}) {
  return Object.freeze({
    name,
    label,
    values: opts.values ? Object.freeze([...opts.values]) : null,
    closed: opts.values ? opts.closed !== false : false,
    hint: opts.hint ?? null,
  });
}

function exploreColumn(key, label, kind = 'text') {
  return Object.freeze({ key, label, kind });
}

/** A user reference as the API mints it; anything else typed into the User field is a name. */
export const USER_REF = /^u_[A-Za-z0-9]+$/;
export const isUserRef = (value) => USER_REF.test(String(value ?? '').trim());

/** The User filter: a name is matched against the people list before the read, a reference is used as it is. */
const USER_FIELD = exploreField('subject', 'User', { hint: 'A name as the directory spells it, or a user reference such as u_4f21' });

/**
 * @typedef {object} ExploreDataset
 * @property {string} id
 * @property {string} label
 * @property {string} noun          singular, for sentences
 * @property {string} questionId    the template every read of this dataset uses
 * @property {'record'|'row'} detail 'record' re-reads one submission (Q9); 'row' shows the row held
 * @property {ReadonlyArray<string>} windows  window presets offered; empty means current state
 * @property {string} ordering      the server's fixed ordering, stated rather than implied
 */

/** @type {Record<string, ExploreDataset>} */
export const EXPLORE_DATASETS = Object.freeze({
  events: Object.freeze({
    id: 'events',
    label: 'Events',
    noun: 'event',
    questionId: 'q8_activity',
    detail: 'record',
    exportable: true,
    windows: Object.freeze(['h6', 'h24', 'd7', 'd30']),
    defaultWindow: 'd7',
    ordering: 'Newest received first',
    rowKey: (row) => String(row.submission_id),
    fields: Object.freeze([
      USER_FIELD,
      exploreField('tool', 'Tool', { hint: 'A tool fingerprint, such as tls_b6681b043244c43f' }),
      exploreField('action', 'Policy action', { values: ['blocked', 'warned', 'logged'] }),
      exploreField('class', 'Data class', { values: DATA_CLASSES, closed: false }),
      exploreField('content_state', 'Content', { values: ['not_captured', 'local_only', 'uploaded', 'shredded'] }),
      exploreField('mode', 'Collection mode', { values: COLLECTION_MODES }),
      exploreField('prompt_kind', 'Request kind', { values: ['user', 'client_generated', 'unknown'] }),
      exploreField('department', 'Department', { hint: 'As the directory spells it' }),
      exploreField('device', 'Device', { hint: 'A device id' }),
    ]),
    columns: Object.freeze([
      exploreColumn('received_at', 'Received (server)', 'server-clock'),
      exploreColumn('first_occurred_at', 'Occurred (device)', 'device-clock'),
      exploreColumn('subject', 'User', 'person'),
      exploreColumn('tool', 'Tool', 'mono'),
      exploreColumn('action', 'Action', 'vocab'),
      exploreColumn('labels', 'Classes', 'classes'),
      exploreColumn('content_state', 'Content', 'vocab'),
      exploreColumn('mode', 'Mode', 'vocab'),
      exploreColumn('observation_count', 'Routes', 'count'),
    ]),
  }),

  findings: Object.freeze({
    id: 'findings',
    label: 'Findings',
    noun: 'finding',
    questionId: 'q5_findings',
    detail: 'record',
    exportable: true,
    windows: Object.freeze(['h24', 'd7', 'd30']),
    defaultWindow: 'd7',
    ordering: 'Newest detected first',
    rowKey: (row) => `${row.submission_id}|${row.rule}`,
    fields: Object.freeze([
      exploreField('severity', 'Severity', { values: SEVERITIES }),
      exploreField('review_state', 'Review', { values: ['open', 'disputed', 'confirmed'] }),
      exploreField('class', 'Data class', { values: DATA_CLASSES, closed: false }),
      exploreField('rule', 'Rule', { hint: 'A rule code, such as PAYMENT_CARD_PAN' }),
      USER_FIELD,
      exploreField('tool', 'Tool', { hint: 'A tool fingerprint, such as tls_b6681b043244c43f' }),
    ]),
    columns: Object.freeze([
      exploreColumn('detected_at', 'Detected', 'server-clock'),
      exploreColumn('severity', 'Severity', 'vocab'),
      exploreColumn('rule_title', 'Rule', 'rule'),
      exploreColumn('class', 'Class', 'vocab'),
      exploreColumn('subject', 'User', 'person'),
      exploreColumn('tool', 'Tool', 'mono'),
      exploreColumn('review_state', 'Review', 'vocab'),
    ]),
  }),

  devices: Object.freeze({
    id: 'devices',
    label: 'Devices',
    noun: 'device row',
    questionId: 'q7_devices',
    detail: 'row',
    windows: Object.freeze([]),
    defaultWindow: null,
    ordering: 'By device',
    rowKey: (row) => String(row.device),
    fields: Object.freeze([
      exploreField('liveness', 'Liveness', { values: ['reporting', 'stale', 'never_reported', 'revoked'] }),
      exploreField('collector_state', 'Collector state', { values: ['healthy', 'degraded', 'absent', 'tampered'] }),
      exploreField('device_os', 'OS', { values: ['windows', 'macos'] }),
      exploreField('managed_state', 'Managed', { values: ['managed', 'unmanaged', 'unknown'] }),
      exploreField('collector', 'Collector', { hint: 'Such as capture_extension' }),
      exploreField('region', 'Region', { hint: 'A residency region, such as eu' }),
    ]),
    columns: Object.freeze([
      exploreColumn('device', 'Device', 'id'),
      exploreColumn('device_os', 'OS', 'vocab'),
      exploreColumn('managed_state', 'Managed', 'vocab'),
      exploreColumn('region', 'Region', 'mono'),
      exploreColumn('liveness', 'Liveness', 'vocab'),
      exploreColumn('collectors_reporting', 'Collectors', 'count'),
      exploreColumn('collectors_degraded', 'Degraded', 'count'),
      exploreColumn('collectors_tampered', 'Tampered', 'count'),
      exploreColumn('last_seen_at', 'Last seen', 'server-clock'),
      exploreColumn('spool_dropped_total', 'Dropped', 'count'),
    ]),
    detailFields: Object.freeze([
      exploreColumn('device', 'Device', 'mono'),
      exploreColumn('device_os', 'OS', 'vocab'),
      exploreColumn('managed_state', 'Managed', 'vocab'),
      exploreColumn('region', 'Region', 'mono'),
      exploreColumn('liveness', 'Liveness', 'vocab'),
      exploreColumn('collectors_reporting', 'Collectors', 'count'),
      exploreColumn('collectors_healthy', 'Healthy', 'count'),
      exploreColumn('collectors_degraded', 'Degraded', 'count'),
      exploreColumn('collectors_absent', 'Absent', 'count'),
      exploreColumn('collectors_tampered', 'Tampered', 'count'),
      exploreColumn('last_seen_at', 'Last seen', 'server-clock'),
      exploreColumn('spool_depth', 'Spool depth', 'count'),
      exploreColumn('spool_dropped_total', 'Dropped from spool', 'count'),
    ]),
  }),

  audit: Object.freeze({
    id: 'audit',
    label: 'Audit trail',
    noun: 'audit entry',
    questionId: 'q10_audit_trail',
    detail: 'row',
    windows: Object.freeze(['h24', 'd7', 'd30', 'd90']),
    defaultWindow: 'd7',
    ordering: 'Newest first',
    rowKey: (row) => String(row.audit_seq),
    fields: Object.freeze([
      exploreField('actor', 'Actor', { hint: 'Who made the read' }),
      exploreField('action', 'Action', {
        values: ['query.aggregate', 'query.events', 'query.findings', 'query.devices', 'query.coverage', 'query.record', 'audit.read'],
        closed: false,
      }),
      exploreField('object_type', 'Object type', { hint: 'Such as ingest.submission' }),
      USER_FIELD,
      exploreField('case', 'Case', { hint: 'A case reference' }),
    ]),
    columns: Object.freeze([
      exploreColumn('occurred_at', 'When', 'server-clock'),
      exploreColumn('actor', 'Actor', 'text'),
      exploreColumn('action', 'Action', 'mono'),
      exploreColumn('object_type', 'Object type', 'mono'),
      exploreColumn('subject', 'User', 'mono'),
      exploreColumn('case', 'Case', 'mono'),
    ]),
    detailFields: Object.freeze([
      exploreColumn('audit_seq', 'Entry', 'mono'),
      exploreColumn('occurred_at', 'When', 'server-clock'),
      exploreColumn('actor_type', 'Actor type', 'vocab'),
      exploreColumn('actor', 'Actor', 'text'),
      exploreColumn('action', 'Action', 'mono'),
      exploreColumn('object_type', 'Object type', 'mono'),
      exploreColumn('object_id', 'Object', 'mono'),
      exploreColumn('subject', 'User', 'mono'),
      exploreColumn('case', 'Case', 'mono'),
      exploreColumn('row_hash', 'Row hash', 'mono'),
      exploreColumn('prev_hash', 'Previous hash', 'mono'),
    ]),
  }),
});

export const EXPLORE_DATASET_IDS = Object.freeze(Object.keys(EXPLORE_DATASETS));
export const EXPLORE_DEFAULT_DATASET = 'events';

/** The dataset for an id, falling back to the default rather than to nothing. */
export function exploreDataset(id) {
  return EXPLORE_DATASETS[id] ?? EXPLORE_DATASETS[EXPLORE_DEFAULT_DATASET];
}

function fieldOf(dataset, name) {
  return dataset.fields.find((f) => f.name === name) ?? null;
}

function fieldList(dataset) {
  return dataset.fields.map((f) => f.name).join(', ');
}

/** Datasets other than this one that do have the field, so a refusal can point somewhere. */
function elsewhere(dataset, name) {
  return EXPLORE_DATASET_IDS
    .filter((id) => id !== dataset.id && fieldOf(EXPLORE_DATASETS[id], name))
    .map((id) => EXPLORE_DATASETS[id].label);
}

/**
 * Check one field/value pair against a dataset. Returns a problem, or null when it is admissible.
 * The rail, the query bar and the address bar all go through this, so none can admit what another
 * refuses.
 */
export function checkExploreFilter(dataset, name, value) {
  const field = fieldOf(dataset, name);
  if (!field) {
    const others = elsewhere(dataset, name);
    return Object.freeze({
      code: 'unknown_field',
      token: name,
      message: `"${name}" is not a filter of ${dataset.label}.`,
      fix: others.length > 0
        ? `It is a filter of ${others.join(' and ')}. ${dataset.label} takes: ${fieldList(dataset)}.`
        : `${dataset.label} takes: ${fieldList(dataset)}.`,
    });
  }
  if (typeof value !== 'string' || value.length === 0) {
    return Object.freeze({
      code: 'missing_value',
      token: `${name}:`,
      message: `${field.label} needs a value.`,
      fix: field.values ? `One of: ${field.values.join(', ')}.` : `Write it as ${name}:value.`,
    });
  }
  if (value.length > MAX_VALUE_LENGTH) {
    return Object.freeze({
      code: 'value_too_long',
      token: `${name}:`,
      message: `The value for ${field.label} is longer than ${MAX_VALUE_LENGTH} characters.`,
      fix: 'The API takes whole values up to that length, not text to search within.',
    });
  }
  if (field.closed && !field.values.includes(value)) {
    return Object.freeze({
      code: 'unknown_value',
      token: `${name}:${value}`,
      message: `"${value}" is not a value of ${field.label}.`,
      fix: `One of: ${field.values.join(', ')}.`,
    });
  }
  return null;
}

/**
 * Parse the query bar into filters and problems.
 *
 * Grammar: whitespace-separated `field:value` terms, with `field:"a value"` for a value containing
 * spaces. One value per field, because every template parameter takes exactly one.
 *
 * @param {string} text
 * @param {ExploreDataset} dataset
 * @returns {{filters: Record<string,string>, problems: ReadonlyArray<object>}}
 */
export function parseExploreQuery(text, dataset) {
  const filters = {};
  const problems = [];
  const source = String(text ?? '');
  const term = /([A-Za-z_][\w.]*):"([^"]*)"?|([A-Za-z_][\w.]*):(\S*)|"([^"]*)"?|(\S+)/g;
  for (const match of source.matchAll(term)) {
    const name = match[1] ?? match[3];
    if (name === undefined) {
      const word = match[5] ?? match[6];
      if (!word) continue;
      problems.push(Object.freeze({
        code: 'free_text',
        token: word,
        message: `"${word}" is free text, and this API has no text search.`,
        fix: `Filters are named: write field:value. ${dataset.label} takes: ${fieldList(dataset)}. To find a prompt by its words, use "Search prompt text" below the filter query.`,
      }));
      continue;
    }
    const value = match[1] !== undefined ? match[2] : match[4];
    if (Object.prototype.hasOwnProperty.call(filters, name)) {
      problems.push(Object.freeze({
        code: 'duplicate_field',
        token: `${name}:${value}`,
        message: `${name} is given twice.`,
        fix: 'Each filter takes one value. Remove one of them.',
      }));
      continue;
    }
    const problem = checkExploreFilter(dataset, name, value);
    if (problem) problems.push(problem);
    else filters[name] = value;
  }
  return Object.freeze({ filters: Object.freeze(filters), problems: Object.freeze(problems) });
}

/**
 * The query bar text for a set of filters, in the dataset's field order. A user reference whose
 * name is known is written as the name, which parses back to the same person.
 */
export function formatExploreQuery(filters, dataset, names = {}) {
  return dataset.fields
    .filter((f) => typeof filters[f.name] === 'string' && filters[f.name] !== '')
    .map((f) => {
      const value = f.name === 'subject' && names[filters[f.name]] ? names[filters[f.name]] : filters[f.name];
      return /\s/.test(value) ? `${f.name}:"${value}"` : `${f.name}:${value}`;
    })
    .join(' ');
}

/** The window preset in force for a dataset: the one asked for when it is offered, else the default. */
export function exploreWindowPreset(dataset, preset) {
  if (dataset.windows.length === 0) return null;
  return dataset.windows.includes(preset) ? preset : dataset.defaultWindow;
}

/** Window presets a dataset offers, with their labels. */
export function exploreWindows(dataset) {
  return dataset.windows.map((id) => Object.freeze({ id, label: WINDOWS[id].label }));
}

/**
 * The request for one page of a dataset. `window` is passed in already resolved, and the caller
 * reuses the same one for every later page: a cursor is bound to the query it was issued for, so
 * a window recomputed from the clock on page two would be a different query.
 *
 * @param {object} input
 * @param {ExploreDataset} input.dataset
 * @param {Record<string,string>} input.filters
 * @param {{from:string,to:string}|null} input.window
 * @param {string|null} [input.cursor]
 * @param {number} [input.limit]
 * @param {boolean} [input.includeClientGenerated]
 */
export function buildExploreRequest({ dataset, filters, window, cursor = null, limit = 50, includeClientGenerated = false }) {
  // By default the event list hides the requests a client made for itself. The API models that as
  // a `prompt_kind_not = 'client_generated'` predicate; the rail's toggle removes it. When the
  // person has already named a prompt_kind, their choice governs and no predicate is added, so the
  // two cannot contradict.
  const effective = { ...filters };
  if (dataset.id === 'events' && !includeClientGenerated && !effective.prompt_kind) {
    effective.prompt_kind_not = 'client_generated';
  }
  return QUESTIONS[dataset.questionId].request(context({
    ...(window ? { window } : {}),
    limit,
    cursor,
    filters: effective,
  }));
}

/** The single-record read behind an event or finding row. */
export function buildExploreRecordRequest({ submissionId, receivedAtHint }) {
  return QUESTIONS.q9_event_detail.request(context({
    filters: { submission_id: submissionId, received_at_hint: receivedAtHint },
  }));
}

/** Every collector one device has reported, behind a device row. A device reports a few dozen at most. */
export function buildExploreCollectorsRequest(device) {
  return buildListDocument('ops.collector_state', { device: String(device ?? '') }, 100);
}

/** `#events?window=d7&tool=tls_b6681b043244c43f&open=<key>`: the whole page state, so a search can be linked. */
export function encodeExploreHash({ dataset, windowPreset, filters, open, includeClientGenerated = false }) {
  const params = new URLSearchParams();
  if (windowPreset && windowPreset !== dataset.defaultWindow) params.set('window', windowPreset);
  for (const field of dataset.fields) {
    const value = filters[field.name];
    if (typeof value === 'string' && value !== '') params.set(field.name, value);
  }
  if (includeClientGenerated) params.set('include', '1');
  if (open) params.set('open', open);
  const query = params.toString();
  return `#${dataset.id}${query ? `?${query}` : ''}`;
}

/**
 * Read a hash back. A parameter the dataset does not know is a problem, exactly as it would be in
 * the query bar: a link that carried a filter this page dropped would show a broader result than
 * the one that was shared.
 */
export function decodeExploreHash(hash) {
  const raw = String(hash ?? '').replace(/^#/, '');
  const [path, queryString] = raw.split('?');
  const dataset = exploreDataset(path);
  const params = new URLSearchParams(queryString ?? '');
  const filters = {};
  const problems = [];
  if (path && !EXPLORE_DATASETS[path]) {
    problems.push(Object.freeze({
      code: 'unknown_dataset',
      token: path,
      message: `"${path}" is not something this page can search.`,
      fix: `Showing ${dataset.label} instead. It can search: ${EXPLORE_DATASET_IDS.map((id) => EXPLORE_DATASETS[id].label).join(', ')}.`,
    }));
  }
  for (const [name, value] of params.entries()) {
    if (RESERVED_KEYS.includes(name)) continue;
    const problem = checkExploreFilter(dataset, name, value);
    if (problem) problems.push(problem);
    else filters[name] = value;
  }
  return Object.freeze({
    dataset,
    windowPreset: exploreWindowPreset(dataset, params.get('window')),
    filters: Object.freeze(filters),
    problems: Object.freeze(problems),
    open: params.get('open') || null,
    includeClientGenerated: params.get('include') === '1',
  });
}

// ── retrieved content ────────────────────────────────────────────────────────────────────────
//
// What a device captures for one request is more than what a person typed. A client wraps the
// typed text in context of its own (Claude Code prepends <system-reminder> blocks to the user
// message), resends the whole conversation on every turn, and makes requests no person wrote at
// all (telemetry batches). Retrieved content is shown both ways: what was typed, and everything
// that was captured. The split is made here, for display only; nothing about the capture changes.

const EXPLORE_INJECTED_BLOCK = /<system-reminder>[\s\S]*?<\/system-reminder>/g;

function exploreStripInjected(text) {
  return String(text ?? '').replace(EXPLORE_INJECTED_BLOCK, '').trim();
}

/**
 * A message's text: a string, the text blocks of a block list, or the string parts of ChatGPT's
 * `{content_type, parts}` object. Tool results and images are not typed text.
 */
function exploreMessageText(content) {
  if (typeof content === 'string') return content;
  if (Array.isArray(content?.parts)) return content.parts.filter((part) => typeof part === 'string' && part.trim() !== '').join('\n');
  if (!Array.isArray(content)) return '';
  return content
    .filter((block) => block && block.type === 'text' && typeof block.text === 'string' && block.text.trim() !== '')
    .map((block) => block.text)
    .join('\n');
}

/**
 * Split retrieved content into what the person typed and what kind of capture it is.
 *
 * The content is either the prompt text the device extracted, or the request body as observed
 * when it could not extract one. In a body, the latest user message that carries text is this
 * turn's input: earlier ones are the conversation being resent.
 *
 * @param {string} content
 * @returns {{typed: string, kind: 'prompt'|'internal'|'other'}}
 */
export function exploreUserInput(content) {
  const trimmed = String(content ?? '').trim();
  let body = null;
  if (trimmed.startsWith('{')) {
    try {
      body = JSON.parse(trimmed);
    } catch {
      body = null;
    }
  }
  if (!body || typeof body !== 'object') return Object.freeze({ typed: exploreStripInjected(trimmed), kind: 'prompt' });
  const messages = Array.isArray(body.messages) ? body.messages : [];
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    // The model APIs name the sender as `role`; ChatGPT's web requests as `author.role`.
    if (messages[i]?.role !== 'user' && messages[i]?.author?.role !== 'user') continue;
    const typed = exploreStripInjected(exploreMessageText(messages[i].content));
    if (typed !== '') return Object.freeze({ typed, kind: 'prompt' });
  }
  return Object.freeze({ typed: '', kind: Array.isArray(body.events) ? 'internal' : 'other' });
}
