// content.js — the two content reads query-api forwards to content-vault.
//
// WHY THIS IS A FORWARDER AND NOT A READ. query-api cannot see content: it has no SELECT on
// ops.content_object or ingest.search_text and no unwrap right (docs/06 §4.4). The one component
// that can read or return content is content-vault, which has internal ingress only, so a browser
// reaches it exclusively through here, under this service's identity (docs/02 §11, docs/04 §15.3).
// Every decision — the four-eyes rule, the single-use grant, the search tier, the audit row written
// before anything is served — is the vault's. This file carries the request across and renders the
// vault's answer; it decides nothing.
//
// What it adds is the one thing only this side knows: WHO is asking. The tenant and the actor come
// from the session, never from the body, exactly as they do for /v1/query.
//
// A retrieval relays the vault's short-lived retrieval URL and never the content: the browser
// fetches that URL from the analyst web tier, so no content byte transits this service (docs/02
// §11). The URL is a capability; the grant it names is single-use and the vault audits the read.

export const CONTENT_PATHS = Object.freeze({
  SEARCH: '/v1/content-search',
  RETRIEVAL: '/v1/content/retrieval',
});

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const MAX_FIELD = 512;
const MAX_EVENT_IDS = 16;
/** The collection modes a search may narrow by; the same closed set the event list uses. */
const MODES = Object.freeze(['m0', 'm1', 'm2', 'm3']);
/** The filters a prompt-text search composes by, beyond the terms (docs/04 §15.3). */
const TEXT_FILTERS = Object.freeze(['subject', 'tool', 'device', 'mode']);

/** A refusal in the shape the dashboard already renders: a state, a code, a sentence. */
function refusal(status, state, code, message) {
  return { status, body: { state, error: { code, message } } };
}

function field(body, name, { required = false } = {}) {
  const value = body?.[name];
  if (value === undefined || value === null || value === '') {
    return required ? { error: `${name} is required` } : { value: '' };
  }
  if (typeof value !== 'string' || value.length > MAX_FIELD) return { error: `${name} must be a string of at most ${MAX_FIELD} characters` };
  return { value: value.trim() };
}

/**
 * Build the forwarder.
 *
 * @param {object} input
 * @param {string} input.vaultUrl     content-vault's internal base URL; empty disables both routes
 * @param {string} [input.scope]      the search scope this deployment asks the vault for
 * @param {typeof fetch} [input.fetchImpl]
 */
export function createContentForwarder({ vaultUrl, scope = '', fetchImpl = globalThis.fetch, log = console } = {}) {
  const base = String(vaultUrl ?? '').replace(/\/$/, '');

  async function call(path, principal, body) {
    const res = await fetchImpl(`${base}${path}`, {
      method: 'POST',
      headers: {
        'content-type': 'application/json',
        'x-sac-service': 'query-api',
        'x-sac-subject': principal.actorId,
        'x-sac-tenant': principal.tenant,
        // The roles are the session's, taken from the verified token. The vault checks them
        // against the route; it is the component that returns content, so it does not rely on
        // its caller having enforced the role. It never reads a role from the browser.
        'x-sac-roles': (principal.roles ?? []).join(','),
      },
      body: JSON.stringify(body),
    });
    let json = null;
    try {
      json = await res.json();
    } catch {
      json = null;
    }
    return { status: res.status, json };
  }

  /** The vault's own refusal, carried through with its closed reason. */
  function vaultRefusal(step, { status, json }) {
    const code = json?.error?.code ?? json?.reason ?? `vault_${status}`;
    const message = json?.error?.detail || json?.detail || `the content vault refused the ${step}`;
    return refusal(status >= 400 && status < 600 ? status : 502, json?.state ?? 'refused', code, message);
  }

  /**
   * POST /v1/content-search: terms plus optional filters in, bounded snippets out. The vault
   * composes the filters against its own index and audits the search in the transaction that serves
   * it, so this service only carries a validated request across.
   */
  async function search(principal, body) {
    const query = field(body, 'query', { required: true });
    if (query.error) return refusal(400, 'refused', 'bad_request', query.error);
    const cursor = field(body, 'cursor');
    if (cursor.error) return refusal(400, 'refused', 'bad_request', cursor.error);

    const filters = {};
    for (const name of TEXT_FILTERS) {
      const parsed = field(body, name);
      if (parsed.error) return refusal(400, 'refused', 'bad_request', parsed.error);
      if (parsed.value) filters[name] = parsed.value;
    }
    if (filters.mode && !MODES.includes(filters.mode)) {
      return refusal(400, 'refused', 'bad_request', `mode must be one of ${MODES.join(', ')}`);
    }

    // The received-at window, from the page's window switch. Each bound is optional; an absent
    // bound is open, which is how "the last 24 hours" reaches the vault as a single from-instant.
    const window = body?.window;
    if (window !== undefined && window !== null) {
      if (typeof window !== 'object' || Array.isArray(window)) return refusal(400, 'refused', 'bad_request', 'window must be an object with from and to');
      for (const [name, key] of [['from', 'received_from'], ['to', 'received_to']]) {
        const value = window[name];
        if (value === undefined || value === null || value === '') continue;
        if (typeof value !== 'string' || Number.isNaN(Date.parse(value))) {
          return refusal(400, 'refused', 'bad_request', `window.${name} must be an ISO 8601 instant`);
        }
        filters[key] = value;
      }
    }

    const answer = await call('/v1/content-search', principal, {
      scope, form: 'terms', query: query.value,
      limit: Number.isInteger(body?.limit) && body.limit > 0 && body.limit <= 50 ? body.limit : 20,
      ...(cursor.value ? { cursor: cursor.value } : {}),
      ...filters,
    });
    if (answer.status !== 200) return vaultRefusal('search', answer);
    return {
      status: 200,
      body: {
        state: 'available',
        hits: (answer.json?.hits ?? []).map((h) => ({ submission_id: h.submission_id, snippet: h.snippet, rank: h.rank })),
        truncated: Boolean(answer.json?.truncated),
        // The vault's opaque page position, passed through untouched: this service cannot read the
        // index, so it cannot invent or inspect a cursor.
        next_cursor: answer.json?.next_cursor || null,
      },
    };
  }

  /**
   * POST /v1/content/retrieval: the vault authorises one read and mints a single-use retrieval
   * URL. This service relays the URL and nothing else; the browser redeems it directly through the
   * analyst web tier, so content never passes through query-api's response body (docs/02 §11).
   *
   * `event_ids` are the observations of the submission the analyst is looking at (the record read
   * already returned them). A submission has one stored object, held against the event the device
   * uploaded it for, and this service cannot look up which: it asks the vault about each until one
   * is not `no_content_object`. Every attempt is a request the vault audits.
   */
  async function retrieve(principal, body) {
    const ids = Array.isArray(body?.event_ids) ? body.event_ids.filter((id) => typeof id === 'string' && UUID.test(id)) : [];
    if (ids.length === 0 || ids.length > MAX_EVENT_IDS) return refusal(400, 'refused', 'bad_request', `event_ids must name between 1 and ${MAX_EVENT_IDS} events`);
    const fields = {};
    for (const name of ['case_reference', 'second_approver', 'justification']) {
      const parsed = field(body, name);
      if (parsed.error) return refusal(400, 'refused', 'bad_request', parsed.error);
      fields[name] = parsed.value;
    }

    let last = null;
    for (const eventId of ids) {
      const granted = await call('/v1/content/retrieval', principal, { event_id: eventId, ...fields });
      if (granted.status !== 200) {
        last = vaultRefusal('retrieval', granted);
        if (granted.json?.error?.code === 'no_content_object') continue;
        return last;
      }
      if (granted.json?.state !== 'available') {
        // Content that is gone is a result, not an error (C17): the vault says why, with a receipt.
        return { status: 200, body: { state: granted.json?.state ?? 'no_longer_available', reason: granted.json?.reason ?? null, receipt_ref: granted.json?.receipt_ref ?? null } };
      }
      if (typeof granted.json.retrieval_url !== 'string' || granted.json.retrieval_url === '') {
        // The vault authorised the read but gave the browser nothing to fetch. Relaying an empty
        // answer would look like content the analyst may not see; say which side failed instead.
        return refusal(502, 'refused', 'retrieval_url_missing', 'the vault authorised the read but minted no retrieval URL');
      }
      return {
        status: 200,
        body: {
          state: 'available',
          event_id: eventId,
          grant_id: granted.json.grant_id ?? null,
          raw_digest: granted.json.raw_digest ?? null,
          expires_at: granted.json.expires_at ?? null,
          // The capability the browser redeems. It is opaque to this service, which must not and
          // cannot read the content behind it.
          retrieval_url: granted.json.retrieval_url,
        },
      };
    }
    return last;
  }

  return Object.freeze({
    enabled: base !== '',
    /** Answer one content request, or null when the path is not one of the two. */
    async handle(path, principal, body) {
      const run = path === CONTENT_PATHS.SEARCH ? search : path === CONTENT_PATHS.RETRIEVAL ? retrieve : null;
      if (!run) return null;
      if (base === '') return refusal(503, 'refused', 'content_vault_not_configured', 'this deployment has no content vault configured');
      try {
        return await run(principal, body);
      } catch (error) {
        log?.warn?.(`query-api: content vault call failed: ${error?.cause?.code ?? error?.message ?? error}`);
        return refusal(503, 'refused', 'content_vault_unreachable', 'the content vault could not be reached');
      }
    },
  });
}
