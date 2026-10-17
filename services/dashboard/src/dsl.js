// dsl.js — the typed builder for template requests.
//
// The pages ask the ten questions as templates, and read a device's collectors as a list document.
// A request is checked here so a mistake is a red panel in the browser rather than a 400 round
// trip; query-api rejects anything outside its closed vocabulary either way, which is the security
// property. A caller supplies *values*; the template
// and parameter names come from vocab.js, which test/parity.test.mjs holds equal to query-api's.

import { QUERY_VERSION, SOURCES, TEMPLATES, WINDOWS } from './vocab.js';

/** A refusal this client produces itself, shaped exactly like the API's error envelope. */
export class DashboardQueryError extends Error {
  constructor(reason, message, detail = {}) {
    super(message);
    this.name = 'DashboardQueryError';
    this.reason = reason;
    this.detail = detail;
    /** Locally-produced refusals carry the same state the API would have used. */
    this.resultState = 'unsupported_query_shape';
    this.http = 400;
  }

  /** The same shape the API returns, so one renderer handles both. */
  toEnvelope() {
    return {
      api_version: '1',
      query_version: QUERY_VERSION,
      result_state: this.resultState,
      error: { code: this.reason, message: this.message, detail: this.detail },
    };
  }
}

function isObject(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

/**
 * An ISO-8601 instant, normalised to UTC.
 *
 * The wire format is `…Z` and only `…Z`; the API refuses anything else. An offset (`+02:00`) is
 * accepted here and converted, because a browser that hands back what a human typed should not
 * fail when the same instant was written a different way. A bare date or a time with no offset is
 * refused: both are ambiguous, and guessing a timezone for an analyst's window is exactly the kind
 * of silent reinterpretation this DSL exists to avoid.
 */
function isoUtc(value) {
  if (typeof value !== 'string') {
    throw new DashboardQueryError('bad_window', `"${String(value)}" is not an ISO-8601 instant.`);
  }
  const utc = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,6})?Z$/;
  const offset = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,6})?[+-]\d{2}:\d{2}$/;
  if (!utc.test(value) && !offset.test(value)) {
    throw new DashboardQueryError(
      'bad_window',
      `"${value}" is not an ISO-8601 instant with an explicit offset. Use 2026-09-24T00:00:00Z.`,
    );
  }
  const ms = Date.parse(value);
  if (!Number.isFinite(ms)) {
    throw new DashboardQueryError('bad_window', `"${value}" is not a real instant.`);
  }
  return new Date(ms).toISOString();
}

/** A window from a named preset, anchored at `now` so the label and the request cannot disagree. */
export function windowFor(preset, now = new Date()) {
  const spec = WINDOWS[preset];
  if (!spec) {
    throw new DashboardQueryError('unknown_window', `Unknown window preset "${String(preset)}".`, { known: Object.keys(WINDOWS) });
  }
  const to = new Date(now.getTime());
  const from = new Date(to.getTime() - spec.hours * 3_600_000);
  return Object.freeze({ from: from.toISOString(), to: to.toISOString(), label: spec.label, preset, bucket: spec.bucket });
}

function checkWindow(window) {
  if (!isObject(window)) {
    throw new DashboardQueryError('missing_window', 'A window is required: no read in this DSL is unbounded.');
  }
  const from = isoUtc(window.from);
  const to = isoUtc(window.to);
  if (Date.parse(to) <= Date.parse(from)) {
    throw new DashboardQueryError('bad_window', 'The window ends before it begins; it is half-open [from, to).');
  }
  return Object.freeze({ from, to });
}

/**
 * Build a list document over a source that needs no window, for a read that is not one of the
 * questions: equality filters on the source's own dimensions and one page of rows.
 *
 * @param {string} source a list source of vocab.js with `noWindow`
 * @param {Record<string, string>} filters
 * @param {number} limit
 */
export function buildListDocument(source, filters, limit) {
  const spec = SOURCES[source];
  if (!spec || spec.kind !== 'list' || !spec.noWindow) {
    throw new DashboardQueryError('unknown_source', `"${String(source)}" is not a list read that needs no window.`);
  }
  const out = [];
  for (const [field, value] of Object.entries(filters)) {
    if (!spec.dimensions.includes(field)) {
      throw new DashboardQueryError('unknown_dimension', `${source} has no field "${field}".`, { known_fields: spec.dimensions });
    }
    if (typeof value !== 'string' || value === '') {
      throw new DashboardQueryError('type_mismatch', `The ${field} filter needs a value.`, { field });
    }
    out.push(Object.freeze({ field, op: 'eq', value }));
  }
  return Object.freeze({ query_version: QUERY_VERSION, source, filters: Object.freeze(out), limit });
}

/**
 * The people list: everyone, a case-insensitive name prefix, one reference, or the references
 * named. The search is lower-cased here because `name_key` is the lower-cased name. `synced` keeps
 * the people the directory holds and has not marked as gone: a person seen only on a device has no
 * directory status at all.
 *
 * @param {{search?: string, subject?: string, subjects?: string[], synced?: boolean, cursor?: string|null, limit?: number}} input
 */
export function buildPeopleDocument({ search = '', subject = '', subjects = null, synced = false, cursor = null, limit = 50 } = {}) {
  const filters = [];
  const term = String(search).trim().toLowerCase();
  if (term !== '') filters.push(Object.freeze({ field: 'name_key', op: 'starts_with', value: term.slice(0, 120) }));
  if (synced) filters.push(Object.freeze({ field: 'directory_status', op: 'in', value: Object.freeze(['active', 'unknown']) }));
  if (subject !== '') filters.push(Object.freeze({ field: 'subject', op: 'eq', value: String(subject) }));
  if (Array.isArray(subjects)) {
    const ids = [...new Set(subjects.filter((s) => typeof s === 'string' && s !== ''))].slice(0, 200);
    if (ids.length > 0) filters.push(Object.freeze({ field: 'subject', op: 'in', value: Object.freeze(ids) }));
  }
  return Object.freeze({ query_version: QUERY_VERSION, source: 'mart.v_person', filters: Object.freeze(filters), limit, ...(cursor ? { cursor } : {}) });
}

/**
 * The people behind the teams on the Teams page: each member's submissions in the window, for the
 * teams named. Naming the teams is what lets the read span more than a week.
 *
 * @param {{window: {from: string, to: string}, teams: string[], limit?: number}} input
 */
export function buildTeamMembersDocument({ window, teams, limit = 1000 }) {
  const ids = [...new Set(teams.filter((t) => typeof t === 'string' && t !== ''))].slice(0, 200);
  if (ids.length === 0) {
    throw new DashboardQueryError('missing_filter', 'A team roster names at least one team.');
  }
  return Object.freeze({
    query_version: QUERY_VERSION,
    source: 'mart.v_team_member_period',
    dimensions: Object.freeze(['team', 'subject']),
    measures: Object.freeze(['submissions']),
    filters: Object.freeze([Object.freeze({ field: 'team', op: 'in', value: Object.freeze(ids) })]),
    window: checkWindow(window),
    limit,
  });
}

/**
 * The prompts each enforcement rule decided in the window, for Insights. The rows are summed per
 * rule on the page, so the read takes the coarsest bucket the window allows: a week over day rows,
 * the hour for a window shorter than a day. The limit keeps a tenant with many rules within the
 * cell cap; a response it cut short says so.
 *
 * @param {{window: {from: string, to: string, bucket?: string}}} input
 */
export function buildRuleMatchesDocument({ window }) {
  return Object.freeze({
    query_version: QUERY_VERSION,
    source: 'mart.agg_rule_period',
    bucket: window.bucket === 'hour' ? 'hour' : 'week',
    dimensions: Object.freeze(['rule']),
    measures: Object.freeze(['matches', 'blocked', 'warned', 'logged']),
    window: checkWindow(window),
    limit: 1999,
  });
}

/**
 * What device discovery found in the window, per tool, kind of finding and item, for Insights. The
 * rows are folded per tool on the page, so the read takes the coarsest bucket the window allows: a
 * month over day rows, the hour for a window shorter than a day. A response the limit cut short
 * says so.
 *
 * @param {{window: {from: string, to: string, bucket?: string}}} input
 */
export function buildDiscoveryDocument({ window }) {
  return Object.freeze({
    query_version: QUERY_VERSION,
    source: 'mart.agg_discovery_period',
    bucket: window.bucket === 'hour' ? 'hour' : 'month',
    dimensions: Object.freeze(['tool', 'discovery_type', 'item']),
    measures: Object.freeze(['devices']),
    window: checkWindow(window),
    limit: 1999,
  });
}

/**
 * The prompts each tool sent in the window, for Insights: one total per tool, in the coarsest bucket
 * the window allows, as for what discovery found.
 *
 * @param {{window: {from: string, to: string, bucket?: string}}} input
 */
export function buildToolPromptsDocument({ window }) {
  return Object.freeze({
    query_version: QUERY_VERSION,
    source: 'mart.v_tool_usage',
    bucket: window.bucket === 'hour' ? 'hour' : 'month',
    dimensions: Object.freeze(['tool']),
    measures: Object.freeze(['submissions']),
    window: checkWindow(window),
    limit: 1999,
  });
}

/**
 * The prompts holding each built-in label, per tool with its sanction, and those a rule naming the
 * label matched, for Insights: what reached a tool not sanctioned with no rule covering it. One total
 * per tool, in the coarsest bucket the window allows, as for what discovery found.
 *
 * @param {{window: {from: string, to: string, bucket?: string}}} input
 */
export function buildLabelCoverageDocument({ window }) {
  return Object.freeze({
    query_version: QUERY_VERSION,
    source: 'mart.agg_class_period',
    bucket: window.bucket === 'hour' ? 'hour' : 'month',
    dimensions: Object.freeze(['class', 'tool', 'sanctioned_state']),
    measures: Object.freeze(['submissions', 'flagged']),
    window: checkWindow(window),
    limit: 1999,
  });
}

/**
 * Teams crossed with tools in the window, for Insights: the read that carries the demand for each
 * tool not sanctioned beside it, counted once over the window per team and across everyone. Its
 * own rows are each team's prompts per fingerprint, in the coarsest bucket the window allows.
 *
 * @param {{window: {from: string, to: string, bucket?: string}}} input
 */
export function buildDemandDocument({ window }) {
  return Object.freeze({
    query_version: QUERY_VERSION,
    source: 'mart.agg_team_period',
    bucket: window.bucket === 'hour' ? 'hour' : 'month',
    dimensions: Object.freeze(['team', 'tool']),
    measures: Object.freeze(['submissions']),
    window: checkWindow(window),
    limit: 1999,
  });
}

/**
 * What coding agents report in the window, per tool and model, for Insights. The rows are folded
 * per tool and per model on the page, so the read takes the coarsest bucket the window allows, as
 * for what discovery found.
 *
 * @param {{window: {from: string, to: string, bucket?: string}}} input
 */
export function buildAgentDocument({ window }) {
  return Object.freeze({
    query_version: QUERY_VERSION,
    source: 'mart.agg_agent_period',
    bucket: window.bucket === 'hour' ? 'hour' : 'month',
    dimensions: Object.freeze(['tool', 'model']),
    measures: Object.freeze(['requests', 'actions', 'input_tokens', 'output_tokens', 'users', 'tool_users']),
    window: checkWindow(window),
    limit: 1999,
  });
}

/**
 * The tools first seen in the window, newest first, for Insights. A tenant's first window can see
 * every tool it has for the first time; the page says so when the list runs past this limit.
 *
 * @param {{window: {from: string, to: string}}} input
 */
export function buildFirstSeenDocument({ window }) {
  return Object.freeze({
    query_version: QUERY_VERSION,
    source: 'mart.v_tool_first_seen',
    filters: Object.freeze([]),
    window: checkWindow(window),
    limit: 200,
  });
}

/**
 * Prompts per bucket by the sanction of the tool they went to, for Insights: the hour for a window
 * shorter than a day, the day up to two weeks, the week beyond.
 *
 * @param {{window: {from: string, to: string, bucket?: string}}} input
 */
export function buildSanctionShiftDocument({ window }) {
  const checked = checkWindow(window);
  const days = (Date.parse(checked.to) - Date.parse(checked.from)) / 86_400_000;
  return Object.freeze({
    query_version: QUERY_VERSION,
    source: 'mart.v_tool_usage',
    bucket: window.bucket === 'hour' ? 'hour' : days > 14 ? 'week' : 'day',
    dimensions: Object.freeze(['sanctioned_state']),
    measures: Object.freeze(['submissions']),
    window: checked,
  });
}

/**
 * Build a template request. A template parameter that the template does not declare is refused,
 * because a typo in a parameter name would otherwise answer a broader question than the one asked.
 *
 * @param {string} name one of TEMPLATE_NAMES
 * @param {object} params
 */
export function buildTemplate(name, params = {}) {
  const template = TEMPLATES[name];
  if (!template) {
    throw new DashboardQueryError('unknown_template', `Unknown template "${String(name)}".`, { known_templates: Object.keys(TEMPLATES) });
  }
  for (const key of Object.keys(params)) {
    if (!template.params.includes(key)) {
      throw new DashboardQueryError('unknown_key', `Template ${name} has no parameter "${key}".`, {
        template: name,
        key,
        known_params: template.params,
      });
    }
  }
  for (const required of template.requires ?? []) {
    if (params[required] === undefined || params[required] === null || params[required] === '') {
      throw new DashboardQueryError('subject_scope_required', `Template ${name} requires "${required}".`, { template: name, required });
    }
  }
  const body = { query_version: QUERY_VERSION, template: name, params: { ...params } };
  if (isObject(params.window)) body.params.window = checkWindow(params.window);
  if (body.params.cursor === null) delete body.params.cursor;
  return Object.freeze(body);
}

