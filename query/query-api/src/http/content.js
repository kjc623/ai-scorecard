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
// AS BUILT, and different from docs/02 §11: a redemption returns the content in this response
// body. The design has the vault mint a short-lived retrieval URL so content never transits
// query-api; the vault does not mint one yet, so the bytes it returns are relayed.

export const CONTENT_PATHS = Object.freeze({
  SEARCH: '/v1/content-search',
  RETRIEVAL: '/v1/content/retrieval',
});

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const MAX_FIELD = 512;
const MAX_EVENT_IDS = 16;

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
   * POST /v1/content-search: terms in, bounded snippets out. The vault audits the search in the
   * transaction that serves it.
   */
  async function search(principal, body) {
    const query = field(body, 'query', { required: true });
    if (query.error) return refusal(400, 'refused', 'bad_request', query.error);
    const answer = await call('/v1/content-search', principal, {
      scope, form: 'terms', query: query.value,
      limit: Number.isInteger(body?.limit) && body.limit > 0 && body.limit <= 50 ? body.limit : 20,
    });
    if (answer.status !== 200) return vaultRefusal('search', answer);
    return {
      status: 200,
      body: {
        state: 'available',
        hits: (answer.json?.hits ?? []).map((h) => ({ submission_id: h.submission_id, snippet: h.snippet, rank: h.rank })),
        truncated: Boolean(answer.json?.truncated),
      },
    };
  }

  /**
   * POST /v1/content/retrieval: docs/02 §11's two steps in one request — the retrieval request
   * that carries the case reference and the second approver, then the single-use redemption of the
   * grant it returns.
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
      const redeemed = await call('/v1/content/redeem', principal, { grant_id: granted.json.grant_id, event_id: eventId });
      if (redeemed.status !== 200) return vaultRefusal('redemption', redeemed);
      if (redeemed.json?.state !== 'available') {
        return { status: 200, body: { state: redeemed.json?.state ?? 'no_longer_available', reason: redeemed.json?.reason ?? null, receipt_ref: redeemed.json?.receipt_ref ?? null } };
      }
      if (typeof redeemed.json.content_b64 !== 'string' || redeemed.json.content_b64 === '') {
        return refusal(502, 'refused', 'content_not_served', 'the vault authorised the read but served no content');
      }
      return {
        status: 200,
        body: {
          state: 'available',
          event_id: eventId,
          grant_id: granted.json.grant_id,
          raw_digest: redeemed.json.raw_digest ?? granted.json.raw_digest ?? null,
          content: Buffer.from(redeemed.json.content_b64, 'base64').toString('utf8'),
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
