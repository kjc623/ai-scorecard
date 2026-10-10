// transport.js — the data layer. Everything the dashboard knows about the network lives here.
//
// `createQueryApi({transport})` takes a transport and returns the only object the rest of the app
// talks to; `httpTransport()` POSTs a request body to /v1/query on the page's own origin.
// test/guarantees.test.mjs asserts that no other file in this package calls `fetch` or names an
// endpoint other than the ones in vocab.js: the browser reaches data through a closed query
// document, reaches content only through the two reads content-vault decides, and changes
// configuration only through control-api's admin API (`createAdminApi`, Settings → Deployment),
// which checks the admin role and audits each write.

import {
  QUERY_ENDPOINT, CONTENT_SEARCH_ENDPOINT, CONTENT_RETRIEVAL_ENDPOINT, LIST_EXPORT_ENDPOINT, RESULT_STATES,
  ADMIN_DEPLOYMENT_ENDPOINT, ADMIN_PACKAGE_ENDPOINT, ADMIN_VERIFICATION_ENDPOINT, ADMIN_KEYS_ENDPOINT, ADMIN_SCIM_TOKENS_ENDPOINT,
  ADMIN_SETTINGS_ENDPOINT, ADMIN_SETTINGS_COLLECTION_MODE_ENDPOINT, ADMIN_SETTINGS_SCOPE_OVERRIDE_ENDPOINT,
  ADMIN_SETTINGS_RETENTION_ENDPOINT, ADMIN_SETTINGS_CONTENT_SEARCH_ENDPOINT, ADMIN_SETTINGS_TOOL_SANCTION_ENDPOINT,
  ADMIN_SETTINGS_ENDPOINT_COLLECTORS_ENDPOINT, ADMIN_SETTINGS_TLS_INSPECTION_ENDPOINT, ADMIN_SETTINGS_RULES_ENDPOINT,
  ADMIN_SETTINGS_KILL_SWITCH_ENDPOINT,
} from './vocab.js';

/** An error the transport produced, carrying the same shape as an API refusal. */
export class TransportError extends Error {
  constructor(envelope, { network = false } = {}) {
    super(envelope?.error?.message ?? 'The query API could not be reached.');
    this.name = 'TransportError';
    this.envelope = envelope;
    this.network = network;
    this.resultState = envelope?.result_state ?? (network ? 'busy' : 'unsupported_query_shape');
  }
}

/**
 * @typedef {object} Transport
 * @property {(body: object) => Promise<object>} send  POST one request body, resolve one envelope
 */

/**
 * Wrap a transport in the only API surface the app uses.
 *
 * @param {object} input
 * @param {Transport} input.transport
 * @returns {{run: (body: object) => Promise<object>, page: (body: object) => Promise<object>}}
 */
export function createQueryApi({ transport }) {
  if (!transport || typeof transport.send !== 'function') {
    throw new TypeError('createQueryApi needs a transport with send(body).');
  }

  /**
   * Run one request and return its envelope, normalised far enough that a screen never has to
   * ask whether a field is missing: `result_state` is always present, and a body that is not a
   * query envelope at all is a refusal rather than a silently empty answer.
   */
  async function run(body) {
    let envelope;
    try {
      envelope = await transport.send(body);
    } catch (error) {
      if (error instanceof TransportError) throw error;
      throw new TransportError(
        { result_state: 'busy', error: { code: 'transport_unavailable', message: String(error?.message ?? error) } },
        { network: true },
      );
    }
    if (!envelope || typeof envelope !== 'object' || typeof envelope.result_state !== 'string') {
      throw new TransportError({
        result_state: 'audit_chain_broken',
        error: { code: 'malformed_envelope', message: 'The response was not a query envelope; refusing to render it as an answer.' },
      });
    }
    if (!RESULT_STATES[envelope.result_state]) {
      throw new TransportError({
        result_state: 'audit_chain_broken',
        error: { code: 'unknown_result_state', message: `Unknown result_state "${envelope.result_state}".` },
      });
    }
    return envelope;
  }

  /**
   * Fetch one page. Returning the envelope unchanged is deliberate: the caller decides what to
   * do with `next_cursor`, and no layer invents a page the API did not send.
   */
  const page = (body) => run(body);

  return Object.freeze({ run, page });
}

/**
 * The signed-in session, as the page sees it, or null when it could not be read.
 *
 * The browser asks its own server who it is signed in as, and the server answers with the pages
 * the roles may open. A failed read is not an error shown to the reader: the navigation is left
 * whole, and the server still refuses what the roles may not use.
 *
 * @param {object} [input]
 * @param {typeof fetch} [input.fetchImpl]
 */
export async function loadSession({ fetchImpl } = {}) {
  const doFetch = fetchImpl ?? (typeof fetch === 'function' ? fetch.bind(globalThis) : null);
  if (!doFetch) return null;
  try {
    const res = await doFetch('session', { headers: { accept: 'application/json' }, credentials: 'same-origin' });
    if (!res?.ok) return null;
    const body = await res.json();
    return body && typeof body === 'object' ? body : null;
  } catch {
    return null;
  }
}

/**
 * The real transport: one POST, one JSON envelope.
 *
 * @param {object} input
 * @param {string} [input.url]
 * @param {typeof fetch} [input.fetchImpl]
 * @param {() => Promise<Record<string,string>>} [input.headers]
 */
export function httpTransport({ url = QUERY_ENDPOINT, fetchImpl, headers } = {}) {
  const doFetch = fetchImpl ?? (typeof fetch === 'function' ? fetch.bind(globalThis) : null);
  if (!doFetch) {
    throw new TransportError({ result_state: 'busy', error: { code: 'no_fetch', message: 'This environment has no fetch implementation.' } }, { network: true });
  }
  return Object.freeze({
    async send(body) {
      const response = await doFetch(url, {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'content-type': 'application/json', ...(headers ? await headers() : {}) },
        body: JSON.stringify(body),
      });
      const envelope = await response.json();
      // A refusal is an envelope too. The transport returns it rather than throwing, so one
      // renderer handles "the answer is not a number" and "the request failed" the same way.
      return envelope;
    },
  });
}

/**
 * The content reads: prompt-text search, the retrieval request, and the minted retrieval URL.
 *
 * They are not queries. A query document cannot name content, and the two requests go to their own
 * endpoints, which query-api forwards to content-vault. The retrieval request answers with a
 * single-use URL, never the content: the browser then fetches that URL from the vault through
 * the page's own server. Every answer is the vault's — `state: "available"` with a
 * result, a state that says the content is gone, or a refusal with the vault's own reason — so a
 * screen never has to tell a network failure from a refusal by catching an exception.
 *
 * @param {object} input
 * @param {{search: (body: object) => Promise<object>, retrieve: (body: object) => Promise<object>, readUrl: (url: string) => Promise<object>}} input.transport
 */
export function createContentApi({ transport }) {
  if (!transport || typeof transport.search !== 'function' || typeof transport.retrieve !== 'function' || typeof transport.readUrl !== 'function') {
    throw new TypeError('createContentApi needs a transport with search(body), retrieve(body) and readUrl(url).');
  }
  async function ask(send, body) {
    try {
      const answer = await send(body);
      if (answer && typeof answer === 'object' && typeof answer.state === 'string') return answer;
      return { state: 'refused', error: { code: 'malformed_answer', message: 'The response was not a content answer; refusing to render it as one.' } };
    } catch (error) {
      return { state: 'refused', error: { code: 'transport_unavailable', message: String(error?.message ?? error) } };
    }
  }
  async function readUrl(url) {
    try {
      const answer = await transport.readUrl(url);
      if (answer && typeof answer === 'object' && typeof answer.state === 'string') return answer;
      return { state: 'refused', error: { code: 'malformed_answer', message: 'The retrieval URL answer was not a content answer; refusing to render it as one.' } };
    } catch (error) {
      return { state: 'refused', error: { code: 'transport_unavailable', message: String(error?.message ?? error) } };
    }
  }
  return Object.freeze({
    search: (body) => ask(transport.search, body),
    retrieve: (body) => ask(transport.retrieve, body),
    readUrl,
  });
}

/** The real content transport: one POST per read, and one GET of a minted URL, on the page's own origin. */
export function httpContentTransport({ fetchImpl } = {}) {
  const doFetch = fetchImpl ?? (typeof fetch === 'function' ? fetch.bind(globalThis) : null);
  if (!doFetch) {
    throw new TransportError({ result_state: 'busy', error: { code: 'no_fetch', message: 'This environment has no fetch implementation.' } }, { network: true });
  }
  const post = async (url, body) => {
    const response = await doFetch(url, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(body),
    });
    // A refusal is a body too, and it carries the vault's reason.
    return response.json();
  };
  const get = async (url) => {
    const response = await doFetch(url, { credentials: 'same-origin' });
    if (response.ok) return { state: 'available', content: await response.text() };
    // A byte read cannot answer 200 with both bytes and a result, so gone carries its own status.
    let body = null;
    try { body = await response.json(); } catch { body = null; }
    if (body?.state === 'no_longer_available') {
      return { state: 'no_longer_available', reason: body.reason ?? null, receipt_ref: body.receipt_ref ?? null };
    }
    return { state: 'refused', error: body?.error ?? { code: 'retrieval_failed', message: `the retrieval URL answered ${response.status}` } };
  };
  return Object.freeze({
    search: (body) => post(CONTENT_SEARCH_ENDPOINT, body),
    retrieve: (body) => post(CONTENT_RETRIEVAL_ENDPOINT, body),
    readUrl: get,
  });
}

/**
 * The list export: the current filtered events or findings list as a bounded CSV. It is a query-api
 * write path in the sense that it produces a downloadable artifact server-side; it returns a
 * single-use, short-lived download link rather than the rows themselves, and the audit row records
 * the filters. The answer is `{state: 'available', download_url, row_count}` or a refusal, so the
 * page never tells a network failure from a refusal by catching an exception.
 *
 * @param {object} input
 * @param {(body: object) => Promise<object>} input.transport
 */
export function createListExportApi({ transport }) {
  if (!transport || typeof transport.post !== 'function') {
    throw new TypeError('createListExportApi needs a transport with post(body).');
  }
  async function list(body) {
    let answer;
    try {
      answer = await transport.post(body);
    } catch (error) {
      return { state: 'refused', error: { code: 'transport_unavailable', message: String(error?.message ?? error) } };
    }
    if (answer?.result_state === 'ok' && typeof answer.export?.download_url === 'string') {
      return { state: 'available', download_url: answer.export.download_url, row_count: answer.export.row_count ?? null };
    }
    return { state: 'refused', error: exportErrorOf(answer) };
  }
  return Object.freeze({ list });
}

/** A list-export refusal's code and sentence, whichever error spelling the server used. */
function exportErrorOf(body) {
  const code = body?.error?.code ?? body?.result_state ?? 'refused';
  const message = body?.error?.message ?? 'The export was not served.';
  return Object.freeze({ code: typeof code === 'string' && code !== '' ? code : 'refused', message });
}

/** The real list-export transport: one POST on the page's own origin. */
export function httpExportTransport({ fetchImpl } = {}) {
  const doFetch = fetchImpl ?? (typeof fetch === 'function' ? fetch.bind(globalThis) : null);
  if (!doFetch) {
    throw new TransportError({ result_state: 'busy', error: { code: 'no_fetch', message: 'This environment has no fetch implementation.' } }, { network: true });
  }
  return Object.freeze({
    async post(body) {
      const response = await doFetch(LIST_EXPORT_ENDPOINT, {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(body),
      });
      return response.json();
    },
  });
}

/**
 * The admin API behind Settings → Deployment (vocab.js names its endpoints). Not a read path: it
 * is control-api's, reached through the dashboard server with the session's product token, and its
 * writes are the only ones this client makes. Every answer is a value, as with the content reads:
 * `{state: 'available', …}` or `{state: 'done'}` on success, `{state: 'refused', status, error}`
 * otherwise, so the page never tells a network failure from a refusal by catching an exception.
 *
 * @param {object} input
 * @param {{request: (spec: {method: string, path: string, body?: object, expect?: 'json'|'file'}) => Promise<{status: number, body?: any, file?: object}>}} input.transport
 */
export function createAdminApi({ transport }) {
  if (!transport || typeof transport.request !== 'function') {
    throw new TypeError('createAdminApi needs a transport with request(spec).');
  }
  async function call(spec) {
    try {
      return await transport.request(spec);
    } catch (error) {
      return { status: 0, body: { error: 'transport_unavailable', message: String(error?.message ?? error) } };
    }
  }
  const ok = (status) => status >= 200 && status < 300;
  const refused = (answer) => ({ state: 'refused', status: answer.status, error: adminErrorOf(answer.body, answer.status) });
  const done = async (spec) => {
    const answer = await call(spec);
    return ok(answer.status) ? { state: 'done' } : refused(answer);
  };
  const revokePath = (base, id) => `${base}/${encodeURIComponent(String(id))}/revoke`;

  return Object.freeze({
    async deployment() {
      const answer = await call({ method: 'GET', path: ADMIN_DEPLOYMENT_ENDPOINT });
      if (!ok(answer.status) || !answer.body || typeof answer.body !== 'object') return refused(answer);
      return { state: 'available', data: normaliseDeployment(answer.body) };
    },
    setVerification(value) {
      return done({ method: 'PUT', path: ADMIN_VERIFICATION_ENDPOINT, body: { device_verification: value } });
    },
    async downloadPackage({ format, label }) {
      const body = label ? { format, label } : { format };
      const answer = await call({ method: 'POST', path: ADMIN_PACKAGE_ENDPOINT, body, expect: 'file' });
      if (!ok(answer.status) || !answer.file) return refused(answer);
      return { state: 'available', file: answer.file };
    },
    revokeKey(keyId) {
      return done({ method: 'POST', path: revokePath(ADMIN_KEYS_ENDPOINT, keyId) });
    },
    async createScimToken(label) {
      const answer = await call({ method: 'POST', path: ADMIN_SCIM_TOKENS_ENDPOINT, body: { label } });
      if (!ok(answer.status) || typeof answer.body?.token !== 'string' || answer.body.token === '') return refused(answer);
      return { state: 'available', token_id: answer.body.token_id ?? null, token: answer.body.token, base_url: answer.body.base_url ?? null };
    },
    revokeScimToken(tokenId) {
      return done({ method: 'POST', path: revokePath(ADMIN_SCIM_TOKENS_ENDPOINT, tokenId) });
    },
    async settings() {
      const answer = await call({ method: 'GET', path: ADMIN_SETTINGS_ENDPOINT });
      if (!ok(answer.status) || !answer.body || typeof answer.body !== 'object') return refused(answer);
      return { state: 'available', data: normaliseSettings(answer.body) };
    },
    setCollectionMode(mode) {
      return done({ method: 'PUT', path: ADMIN_SETTINGS_COLLECTION_MODE_ENDPOINT, body: { collection_mode: mode } });
    },
    setScopeOverride(toolKey, mode) {
      return done({ method: 'PUT', path: ADMIN_SETTINGS_SCOPE_OVERRIDE_ENDPOINT, body: { tool_key: toolKey, collection_mode: mode } });
    },
    setRetention(appliesTo, ttlDays) {
      return done({ method: 'PUT', path: ADMIN_SETTINGS_RETENTION_ENDPOINT, body: { applies_to: appliesTo, ttl_days: ttlDays } });
    },
    setContentSearch(tier) {
      return done({ method: 'PUT', path: ADMIN_SETTINGS_CONTENT_SEARCH_ENDPOINT, body: { content_search: tier } });
    },
    setToolSanction(toolKey, state) {
      return done({ method: 'PUT', path: `${ADMIN_SETTINGS_TOOL_SANCTION_ENDPOINT}/${encodeURIComponent(String(toolKey))}/sanction`, body: { sanctioned_state: state } });
    },
    /** Every collector switch at once: control-api requires all six, so none is turned off by omission. */
    setEndpointCollectors({ inventory, processes, flows, otel, hooks, hooks_managed_only }) {
      return done({ method: 'PUT', path: ADMIN_SETTINGS_ENDPOINT_COLLECTORS_ENDPOINT, body: { inventory, processes, flows, otel, hooks, hooks_managed_only } });
    },
    /** One tool's switches: otel and hooks for a tool with native collectors, loopback for Ollama. */
    setEndpointTool(toolKey, switches) {
      return done({ method: 'PUT', path: `${ADMIN_SETTINGS_ENDPOINT_COLLECTORS_ENDPOINT}/tools/${encodeURIComponent(String(toolKey))}`, body: { ...switches } });
    },
    setTLSInspection(enabled) {
      return done({ method: 'PUT', path: ADMIN_SETTINGS_TLS_INSPECTION_ENDPOINT, body: { enabled } });
    },
    async rules() {
      const answer = await call({ method: 'GET', path: ADMIN_SETTINGS_RULES_ENDPOINT });
      if (!ok(answer.status) || !Array.isArray(answer.body?.rules)) return refused(answer);
      return { state: 'available', data: normaliseRules(answer.body.rules) };
    },
    /** The whole ordered list: control-api replaces the tenant's rules with exactly these. */
    setRules(rules) {
      return done({ method: 'PUT', path: ADMIN_SETTINGS_RULES_ENDPOINT, body: { rules } });
    },
    /** Trip (with its reason code) or clear one interception route's kill switch. */
    setKillSwitch(route, on, reasonCode) {
      const body = reasonCode ? { on, reason_code: reasonCode } : { on };
      return done({ method: 'PUT', path: `${ADMIN_SETTINGS_KILL_SWITCH_ENDPOINT}/${encodeURIComponent(String(route))}`, body });
    },
  });
}

/** An admin refusal's code and sentence, whichever of the two error spellings the server used. */
export function adminErrorOf(body, status) {
  const code = typeof body?.error === 'string' ? body.error : body?.error?.code;
  const message = typeof body?.message === 'string' ? body.message : body?.error?.message;
  const fallback = status === 0 ? 'The admin API could not be reached.'
    : status === 401 ? 'There is no signed-in session.'
      : status === 403 ? 'Your role cannot use these settings.'
        : `The admin API answered ${status}.`;
  return Object.freeze({ code: typeof code === 'string' && code !== '' ? code : `http_${status}`, message: message || fallback });
}

const nullableNumber = (value) => (typeof value === 'number' && Number.isFinite(value) ? value : null);
const nullableText = (value) => (typeof value === 'string' && value !== '' ? value : null);

/**
 * The deployment read, with every field present: a value the server did not send is null, never a
 * zero or an empty string, because "not reported" and "none" are different answers.
 */
export function normaliseDeployment(body) {
  const connection = body.connection && typeof body.connection === 'object'
    ? Object.freeze({
      provider: nullableText(body.connection.provider),
      status: nullableText(body.connection.status),
      entra_tenant_id: nullableText(body.connection.entra_tenant_id),
      issuer: nullableText(body.connection.issuer),
    })
    : null;
  const keys = (Array.isArray(body.deployment_keys) ? body.deployment_keys : []).filter((k) => k && typeof k === 'object').map((k) => Object.freeze({
    key_id: String(k.key_id ?? ''), label: nullableText(k.label), created_at: nullableText(k.created_at), created_by: nullableText(k.created_by),
    expires_at: nullableText(k.expires_at), revoked_at: nullableText(k.revoked_at),
    enrolment_count: nullableNumber(k.enrolment_count), last_used_at: nullableText(k.last_used_at),
  }));
  const scim = body.scim && typeof body.scim === 'object' ? body.scim : {};
  const tokens = (Array.isArray(scim.tokens) ? scim.tokens : []).filter((t) => t && typeof t === 'object').map((t) => Object.freeze({
    token_id: String(t.token_id ?? ''), label: nullableText(t.label), created_at: nullableText(t.created_at), revoked_at: nullableText(t.revoked_at),
  }));
  const devices = body.devices && typeof body.devices === 'object' ? body.devices : {};
  const release = body.release && typeof body.release === 'object'
    ? Object.freeze({ version: nullableText(body.release.version), product_code: nullableText(body.release.product_code) })
    : null;
  return Object.freeze({
    connection,
    device_verification: body.device_verification === 'intune' ? 'intune' : 'none',
    deployment_keys: Object.freeze(keys),
    scim: Object.freeze({
      tokens: Object.freeze(tokens), base_url: nullableText(scim.base_url),
      users: nullableNumber(scim.users), groups: nullableNumber(scim.groups), last_provisioned_at: nullableText(scim.last_provisioned_at),
    }),
    devices: Object.freeze({ enrolled: nullableNumber(devices.enrolled), last_enrolled_at: nullableText(devices.last_enrolled_at) }),
    release,
  });
}

/**
 * The Settings read, with every field present: a value the server did not send is null, never a
 * zero or an empty string, because "not reported" and "none" are different answers. An empty
 * collection_mode means "follow the ceiling".
 */
export function normaliseSettings(body) {
  const tools = (Array.isArray(body.tools) ? body.tools : []).filter((t) => t && typeof t === 'object').map((t) => Object.freeze({
    tool_key: String(t.tool_key ?? ''),
    display_name: nullableText(t.display_name),
    sanctioned_state: ['sanctioned', 'unsanctioned', 'unknown'].includes(t.sanctioned_state) ? t.sanctioned_state : 'unknown',
    fingerprints: Object.freeze((Array.isArray(t.fingerprints) ? t.fingerprints : []).filter((f) => typeof f === 'string')),
  }));
  const devices = (Array.isArray(body.devices) ? body.devices : []).filter((d) => d && typeof d === 'object').map((d) => Object.freeze({
    device_id: String(d.device_id ?? ''),
    hostname: nullableText(d.hostname),
    collection_mode: nullableText(d.collection_mode),
    last_seen_at: nullableText(d.last_seen_at),
  }));
  const overrides = body.scope_overrides && typeof body.scope_overrides === 'object' ? body.scope_overrides : {};
  const scoped = {};
  for (const [key, mode] of Object.entries(overrides)) scoped[key] = nullableText(mode);
  return Object.freeze({
    ceiling_mode: nullableText(body.ceiling_mode),
    collection_mode: nullableText(body.collection_mode),
    scope_overrides: Object.freeze(scoped),
    event_retention_days: nullableNumber(body.event_retention_days),
    content_retention_days: nullableNumber(body.content_retention_days),
    retention_defaults: Object.freeze({
      event_days: nullableNumber(body.retention_defaults?.event_days),
      content_days: nullableNumber(body.retention_defaults?.content_days),
    }),
    content_search: ['disabled', 'attachment_names', 'full_text'].includes(body.content_search) ? body.content_search : 'disabled',
    tls_inspection: nullableBoolean(body.tls_inspection),
    data_classes: Array.isArray(body.data_classes) ? Object.freeze(body.data_classes.filter((c) => typeof c === 'string' && c !== '')) : null,
    app_categories: Array.isArray(body.app_categories) ? Object.freeze(body.app_categories.filter((c) => typeof c === 'string' && c !== '')) : null,
    tools: Object.freeze(tools),
    devices: Object.freeze(devices),
    endpoint: normaliseEndpoint(body.endpoint),
    kill_switches: normaliseKillSwitches(body.kill_switches),
  });
}

/** The tripped kill switches: null when the server sent none, each with its route, reason, time and actor. */
function normaliseKillSwitches(switches) {
  if (!Array.isArray(switches)) return null;
  return Object.freeze(switches.filter((k) => k && typeof k === 'object' && typeof k.route === 'string' && k.route !== '').map((k) => Object.freeze({
    route: k.route,
    reason_code: nullableText(k.reason_code),
    effective_at: nullableText(k.effective_at),
    set_by: nullableText(k.set_by),
  })));
}

const nullableBoolean = (value) => (typeof value === 'boolean' ? value : null);

const RULE_MATCH_LISTS = Object.freeze(['labels', 'tools', 'categories', 'sanction', 'routes']);

/** The enforcement rules in their order, every match list an array of strings and no link as ''. */
export function normaliseRules(rules) {
  return Object.freeze(rules.filter((r) => r && typeof r === 'object').map((r) => Object.freeze({
    rule_id: String(r.rule_id ?? ''),
    action: String(r.action ?? ''),
    match: Object.freeze(Object.fromEntries(RULE_MATCH_LISTS.map((f) => [f,
      Object.freeze((Array.isArray(r.match?.[f]) ? r.match[f] : []).filter((v) => typeof v === 'string'))]))),
    message: typeof r.message === 'string' ? r.message : '',
    link: typeof r.link === 'string' ? r.link : '',
  })));
}

/** The endpoint collector switches: null when the server sent none, and null for a switch it left out. */
function normaliseEndpoint(endpoint) {
  if (!endpoint || typeof endpoint !== 'object') return null;
  const tools = {};
  const sent = endpoint.tools && typeof endpoint.tools === 'object' ? endpoint.tools : {};
  for (const [key, value] of Object.entries(sent)) {
    if (!value || typeof value !== 'object') continue;
    tools[key] = Object.freeze({ otel: nullableBoolean(value.otel), hooks: nullableBoolean(value.hooks), loopback: nullableBoolean(value.loopback) });
  }
  return Object.freeze({
    inventory: nullableBoolean(endpoint.inventory),
    processes: nullableBoolean(endpoint.processes),
    flows: nullableBoolean(endpoint.flows),
    otel: nullableBoolean(endpoint.otel),
    hooks: nullableBoolean(endpoint.hooks),
    hooks_managed_only: nullableBoolean(endpoint.hooks_managed_only),
    tools: Object.freeze(tools),
  });
}

/**
 * The file name a download is saved under, from Content-Disposition. Only the last path segment
 * is kept, so a server cannot name a file outside the browser's download folder.
 */
export function filenameFromDisposition(header) {
  if (typeof header !== 'string' || header === '') return null;
  let name = null;
  const extended = /filename\*\s*=\s*(?:UTF-8|utf-8)''([^;]+)/.exec(header);
  if (extended) {
    try {
      name = decodeURIComponent(extended[1].trim());
    } catch {
      name = null;
    }
  }
  if (!name) {
    const plain = /filename\s*=\s*(?:"([^"]*)"|([^;\s]+))/.exec(header);
    name = plain ? (plain[1] ?? plain[2]) : null;
  }
  if (!name) return null;
  const base = name.split(/[\\/]/).pop().replace(/[\u0000-\u001f"<>|:*?]/g, '').trim().slice(0, 128);
  return base === '' || base === '.' || base === '..' ? null : base;
}

/** The real admin transport: JSON in and out on the page's own origin, and one binary download. */
export function httpAdminTransport({ fetchImpl } = {}) {
  const doFetch = fetchImpl ?? (typeof fetch === 'function' ? fetch.bind(globalThis) : null);
  if (!doFetch) {
    throw new TransportError({ result_state: 'busy', error: { code: 'no_fetch', message: 'This environment has no fetch implementation.' } }, { network: true });
  }
  return Object.freeze({
    async request({ method, path, body, expect = 'json' }) {
      const response = await doFetch(path, {
        method,
        credentials: 'same-origin',
        headers: { accept: expect === 'file' ? '*/*' : 'application/json', ...(body === undefined ? {} : { 'content-type': 'application/json' }) },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      if (expect === 'file' && response.ok) {
        // The deployment key a package carries, when the server names it (x-sac-*); otherwise the
        // page works it out from the key list, which gained exactly one key.
        return {
          status: response.status,
          file: {
            data: await response.blob(),
            filename: filenameFromDisposition(response.headers.get('content-disposition')),
            contentType: response.headers.get('content-type'),
            keyId: response.headers.get('x-sac-deployment-key-id'),
            keyLabel: response.headers.get('x-sac-deployment-key-label'),
          },
        };
      }
      let parsed = null;
      if (response.status !== 204) {
        try {
          parsed = await response.json();
        } catch {
          parsed = null;
        }
      }
      return { status: response.status, body: parsed };
    },
  });
}
