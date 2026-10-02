// transport.js — THE data layer. Everything the dashboard knows about the network lives here.
//
// One module, one seam. `createQueryApi({transport})` takes a transport and returns the only
// object the rest of the app is allowed to talk to. Two transports ship:
//
//   * `stubTransport(fixtures)` — canned envelopes, so `index.html` opens from the filesystem and
//     every state (fresh, stale, partial coverage, suppressed, empty, refusal, 410, cursor) is
//     visible to a human without a server, a database, or a session.
//   * `httpTransport({fetchImpl, url})` — a real POST of the same body to `/v1/query`.
//
// `test/section14.test.mjs` asserts that no other file in this package calls `fetch`, builds a
// URL, or names any endpoint other than QUERY_ENDPOINT: the browser has exactly one way to reach
// data, and it is a closed query document.
//
// Pagination lives here too, because it is a property of the transport rather than of a screen:
// a page is only finished when `next_cursor` is null. A short page is NOT the end — rows deleted
// underneath an iteration make a page shorter, and stopping on that would truncate the answer.

import { QUERY_ENDPOINT, RESULT_STATES } from './vocab.js';

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
 * The canned transport. Keys are matched against the request body's template, source and filter
 * values, so a fixture can answer "the classes screen with a suppressed cell" without the screen
 * knowing a fixture exists.
 *
 * @param {object} input
 * @param {ReadonlyArray<{match: (body: object) => boolean, reply: (body: object) => object}>} input.routes
 * @param {object} [input.fallback] envelope used when no route matches
 */
export function stubTransport({ routes, fallback }) {
  return Object.freeze({
    async send(body) {
      for (const route of routes) {
        if (route.match(body)) return route.reply(body);
      }
      if (fallback) return fallback;
      return {
        api_version: '1',
        query_version: '1',
        result_state: 'unsupported_query_shape',
        error: {
          code: 'no_stub_route',
          message: 'The stub transport has no canned answer for this request.',
          detail: { body },
        },
      };
    },
  });
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
 * Page through a list read. Iteration stops when `next_cursor` is null and at no other time:
 * a short page is not the end of the results, because rows deleted mid-iteration make a page
 * shorter and stopping there would truncate a result set whose rows were erased underneath it.
 *
 * @param {object} input
 * @param {(body: object) => Promise<object>} input.fetchPage
 * @param {object} input.body          the request without a cursor
 * @param {number} [input.maxPages]    a safety bound for the UI, never a correctness one
 */
export async function collectPages({ fetchPage, body, maxPages = 20 }) {
  const pages = [];
  const rows = [];
  let cursor = null;
  let guard = 0;
  do {
    const request = cursor === null ? body : { ...body, cursor };
    // eslint-disable-next-line no-await-in-loop
    const envelope = await fetchPage(request);
    pages.push(envelope);
    if (!RESULT_STATES[envelope.result_state]) return { rows, pages, envelope, error: 'unknown_result_state' };
    if (!envelope.result_state.startsWith('ok') && !envelope.data) {
      return { rows, pages, envelope, error: envelope.result_state };
    }
    rows.push(...(envelope.data ?? []));
    cursor = envelope.page?.next_cursor ?? null;
    guard += 1;
  } while (cursor !== null && guard < maxPages);
  return { rows, pages, envelope: pages[pages.length - 1], truncated: cursor !== null };
}

export { QUERY_ENDPOINT };
