// server.js — the HTTP surface of query-api.
//
// Transport only. It authenticates the caller from the product access token, reads the request,
// takes the tenant from the token (never from the body), and calls plan() and executePlan(), which
// own validation, cost, compilation, audit and suppression. It renders the envelope the pipeline
// produced or the typed rejection it raised, and makes no decision about what a query means.

import { createServer } from 'node:http';
import { QueryError, RESULT_STATES, fromDatabaseError } from '../errors.js';
import { plan, executePlan } from '../plan.js';
import { QUERY_CLASSES } from '../registry.js';
import { inTransaction } from '../db.js';
import { CONTENT_PATHS, createContentForwarder } from './content.js';
import { validateReviewRequest, FINDING_FOR_REVIEW_SQL, UPSERT_REVIEW_SQL, findingReviewAuditStatement } from '../review.js';
import { validateSanctionRequest, TOOL_SANCTION_FOR_REVIEW_SQL, UPSERT_TOOL_SANCTION_SQL, toolSanctionAuditStatement } from '../sanction.js';
import { capabilityForEndpoint, capabilityForSource, rolesAllow, unauthorisedRole } from '../roles.js';

const HIT_ID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export const PATHS = Object.freeze({
  LIVENESS: '/healthz',
  READINESS: '/readyz',
  QUERY: '/v1/query',
  FINDING_REVIEW: '/v1/finding-review',
  TOOL_SANCTION: '/v1/tool-sanction',
});

/** A request body larger than this is refused before it is parsed. */
export const MAX_BODY_BYTES = 256 * 1024;

/** The ceiling on one request, including its wait in the queue. */
export const DEFAULT_REQUEST_TIMEOUT_MS = 30_000;

/**
 * The process-wide admission gate: at most MAX_CONCURRENCY requests use the database at once and
 * MAX_QUEUE wait for a slot. Beyond that the answer is an immediate `busy` (429) rather than an
 * unbounded queue, which would turn one slow query into everyone's latency.
 */
export const MAX_CONCURRENCY = 8;
export const MAX_QUEUE = 32;

/** One connection per admitted request, plus one so a readiness probe never waits behind reads. */
export const POOL_MAX = MAX_CONCURRENCY + 1;

/** The statement budget for the two writes and the search-hit lookup. */
const OPERATIONAL_TIMEOUT_MS = QUERY_CLASSES.operational.statementTimeoutMs;

class HttpError extends Error {
  constructor(status, payload, { closeConnection = false } = {}) {
    super(payload?.error?.message ?? `HTTP ${status}`);
    this.status = status;
    this.payload = payload;
    this.closeConnection = closeConnection;
  }
}

/** Read a body with a hard ceiling, never buffering more than the ceiling. */
function readBody(req, limit = MAX_BODY_BYTES) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let size = 0;
    let done = false;
    req.on('data', (chunk) => {
      if (done) return;
      size += chunk.length;
      if (size > limit) {
        done = true;
        // Answer first and close afterwards (see closeConnection): destroying the socket here would
        // leave the client with "fetch failed" instead of the 413 that explains it.
        reject(new HttpError(413, { result_state: 'unsupported_query_shape', error: { code: 'body_too_large', message: `request body exceeds ${limit} bytes` } }, { closeConnection: true }));
        req.pause();
        return;
      }
      chunks.push(chunk);
    });
    req.on('error', (error) => {
      if (!done) {
        done = true;
        reject(error);
      }
    });
    req.on('end', () => {
      if (done) return;
      done = true;
      resolve(Buffer.concat(chunks).toString('utf8'));
    });
  });
}

function sendJson(res, status, body, extraHeaders = null) {
  const text = JSON.stringify(body);
  res.writeHead(status, {
    'content-type': 'application/json; charset=utf-8',
    'content-length': Buffer.byteLength(text),
    // Every response is per tenant and per user; nothing between here and the browser may keep it.
    'cache-control': 'no-store',
    'x-content-type-options': 'nosniff',
    ...(extraHeaders ?? {}),
  });
  res.end(text);
}

/**
 * A bounded concurrency gate with a bounded queue.
 */
export function createGate({ maxConcurrency = MAX_CONCURRENCY, maxQueue = MAX_QUEUE } = {}) {
  let active = 0;
  const queue = [];

  return {
    get active() {
      return active;
    },
    get queued() {
      return queue.length;
    },
    /** Resolves when the caller may proceed; rejects with a 429 when the queue is full. */
    acquire() {
      if (active < maxConcurrency) {
        active += 1;
        return Promise.resolve();
      }
      if (queue.length >= maxQueue) {
        return Promise.reject(new HttpError(429, { result_state: 'busy', error: { code: 'busy', message: 'the service is at its concurrency limit; retry shortly' } }));
      }
      return new Promise((resolve) => queue.push(resolve));
    },
    release() {
      active -= 1;
      const next = queue.shift();
      if (next) {
        active += 1;
        next();
      }
    },
  };
}

/**
 * Build the request handler.
 *
 * @param {object} input
 * @param {object} input.cfg                 loadConfig()
 * @param {import('pg').Pool} input.pool     anything with connect() and query()
 * @param {{verify: (token: string) => Promise<object>}} input.verifier
 * @param {object} [input.contentForwarder]
 * @param {() => Date} [input.now]
 * @param {object} [input.log]
 * @param {object} [input.gate]
 */
export function createHandler({
  cfg,
  pool,
  verifier,
  contentForwarder = null,
  now = () => new Date(),
  log = console,
  gate = createGate(),
} = {}) {
  if (!cfg) throw new Error('createHandler needs a configuration');
  if (!pool || typeof pool.connect !== 'function') throw new Error('createHandler needs a pool');
  if (!verifier || typeof verifier.verify !== 'function') throw new Error('createHandler needs a token verifier');
  const forwarder = contentForwarder ?? createContentForwarder({ vaultUrl: cfg.contentVaultUrl, log });

  /** Render a thrown error. A QueryError already carries its state, reason and status. */
  function renderError(error) {
    if (error instanceof QueryError) return { status: error.http, body: error.toEnvelope() };
    if (error instanceof HttpError) return { status: error.status, body: error.payload };
    // Anything else is a defect: logged in full, reported in outline, because an internal message
    // can name a host or a column.
    log.error('unhandled error', { error: String(error?.stack ?? error) });
    return {
      status: 500,
      body: { result_state: 'audit_chain_broken', error: { code: 'internal_error', message: 'the service could not complete this request' } },
    };
  }

  function sendError(res, error) {
    const rendered = renderError(error);
    sendJson(res, rendered.status, rendered.body);
  }

  /** A failure on the database path: log what the driver said, answer with the typed rejection. */
  function databaseFailure(res, error, opts) {
    if (!(error instanceof QueryError)) log.warn('database operation failed', { error: String(error?.message ?? error), code: error?.code ?? null });
    sendError(res, fromDatabaseError(error, opts));
  }

  /**
   * The session for this request, from the bearer token, or null after answering 401.
   *
   * The principal carries the token as a non-enumerable `bearer`, so the content forwarder can hand
   * the vault the same credential while a log line or a spread of the principal cannot.
   */
  async function authenticate(req, res, { contentShape = false } = {}) {
    const header = req.headers.authorization;
    const token = typeof header === 'string' && /^Bearer\s+\S+/i.test(header) ? header.replace(/^Bearer\s+/i, '').trim() : '';
    let refused = false;
    if (token !== '') {
      try {
        const principal = await verifier.verify(token);
        return Object.defineProperty({ ...principal }, 'bearer', { value: token, enumerable: false });
      } catch (error) {
        // The token is a credential and is never logged; the reason is.
        log.warn('bearer token refused', { reason: String(error?.message ?? error) });
        refused = true;
      }
    }
    const message = refused ? 'the bearer token was refused; sign in again' : 'no signed-in session; sign in and present the bearer token';
    // The body keeps `unauthorised_role` because the result-state vocabulary is closed; the code
    // says what happened, and `invalid_token` tells a client its token is bad rather than absent.
    const body = contentShape
      ? { state: 'refused', error: { code: 'unauthenticated', message } }
      : { result_state: 'unauthorised_role', error: { code: 'unauthenticated', message } };
    sendJson(res, 401, body, { 'www-authenticate': refused ? 'Bearer realm="sac-query", error="invalid_token"' : 'Bearer realm="sac-query"' });
    return null;
  }

  /** The JSON body, or undefined after answering. */
  async function readJson(req, res, { contentShape = false } = {}) {
    let raw;
    try {
      raw = await readBody(req);
    } catch (error) {
      const refusal = error instanceof HttpError
        ? error
        : new HttpError(400, { result_state: 'unsupported_query_shape', error: { code: 'malformed_document', message: 'the request body could not be read' } });
      if (refusal.closeConnection) {
        // An oversized body cannot be drained for ever: answer, then close.
        res.setHeader('connection', 'close');
        res.on('finish', () => req.destroy());
      }
      sendJson(res, refusal.status, contentShape
        ? { state: 'refused', error: { code: refusal.payload.error.code, message: refusal.payload.error.message } }
        : refusal.payload);
      return undefined;
    }
    try {
      return raw.trim() === '' ? {} : JSON.parse(raw);
    } catch {
      sendJson(res, 400, contentShape
        ? { state: 'refused', error: { code: 'malformed_document', message: 'the request body is not JSON' } }
        : { result_state: 'unsupported_query_shape', error: { code: 'malformed_document', message: 'the request body is not JSON' } });
      return undefined;
    }
  }

  /** Run `fn` holding a gate slot, or answer 429 when the queue is full. */
  async function admitted(res, fn, { contentShape = false } = {}) {
    try {
      await gate.acquire();
    } catch (error) {
      if (contentShape) sendJson(res, 429, { state: 'refused', error: { code: 'busy', message: 'the service is at its concurrency limit; retry shortly' } });
      else sendError(res, error);
      return;
    }
    try {
      await fn();
    } finally {
      gate.release();
    }
  }

  /** Run `fn(client)` on a pooled connection. */
  async function withClient(fn) {
    const client = await pool.connect();
    try {
      return await fn(client);
    } finally {
      client.release();
    }
  }

  /** Liveness: the process is up. It touches no dependency, so the platform may restart on it. */
  function liveness(res) {
    sendJson(res, 200, { status: 'ok' });
  }

  /** Readiness: one database round trip. A failure takes the replica out of rotation. */
  async function readiness(res) {
    try {
      await pool.query('SELECT 1');
      sendJson(res, 200, { status: 'ready' });
    } catch (error) {
      // Its reader is the platform, and a driver error can carry a host name: logged, not returned.
      log.warn('readiness check failed', { error: String(error?.message ?? error) });
      sendJson(res, 503, { status: 'not-ready' });
    }
  }

  async function query(req, res) {
    const principal = await authenticate(req, res);
    if (!principal) return;
    const request = await readJson(req, res);
    if (request === undefined) return;

    const ctx = {
      tenant: principal.tenant,
      actorId: principal.actorId,
      sessionId: principal.sessionId ?? null,
      caseReference: principal.caseReference ?? null,
      cursorKey: cfg.cursorKey,
      now: now(),
    };
    let planned;
    try {
      planned = plan(request, ctx);
      // The plan names the source and the source names the capability: a role without it is
      // refused before a row is read.
      const capability = capabilityForSource(planned.source_id);
      if (!rolesAllow(principal.roles, capability)) throw unauthorisedRole(`use ${capability}`, { role: principal.roles.join(','), path: PATHS.QUERY });
    } catch (error) {
      sendError(res, error);
      return;
    }

    await admitted(res, async () => {
      try {
        const envelope = await withClient((client) => executePlan(planned, { ...ctx, client }));
        sendJson(res, 200, envelope);
      } catch (error) {
        databaseFailure(res, error);
      }
    });
  }

  /**
   * An audited write: the change and its audit row commit together in one tenant-scoped
   * transaction, so a response is never served for a change that was not recorded. The actor is
   * the authenticated principal and the tenant is the session's; neither is read from the body.
   */
  async function auditedWrite(req, res, path, validateBody, perform) {
    const principal = await authenticate(req, res);
    if (!principal) return;
    const capability = capabilityForEndpoint(path);
    if (!rolesAllow(principal.roles, capability)) {
      sendError(res, unauthorisedRole(`use ${capability}`, { role: principal.roles.join(','), path }));
      return;
    }
    const body = await readJson(req, res);
    if (body === undefined) return;
    let input;
    try {
      input = validateBody(body);
    } catch (error) {
      sendError(res, error);
      return;
    }
    await admitted(res, async () => {
      let auditing = false;
      try {
        const answer = await withClient((client) => inTransaction(client, { tenant: principal.tenant, statementTimeoutMs: OPERATIONAL_TIMEOUT_MS }, () => perform(client, principal, input, () => {
          auditing = true;
        })));
        sendJson(res, answer.status, answer.body);
      } catch (error) {
        databaseFailure(res, error, { auditFailed: auditing });
      }
    });
  }

  function auditBlock(row) {
    if (!row) return {};
    return {
      audit: {
        entry_id: row.audit_seq === undefined ? null : String(row.audit_seq),
        written_at: row.occurred_at instanceof Date ? row.occurred_at.toISOString() : (row.occurred_at ?? null),
      },
    };
  }

  const isoOrNull = (value) => (value instanceof Date ? value.toISOString() : (value ?? null));

  /** POST /v1/finding-review: confirm or dispute one finding. */
  async function reviewFinding(client, principal, review, auditing) {
    // The predicate is the session tenant, so a finding from another tenant is not found rather
    // than found and refused.
    const found = await client.query(FINDING_FOR_REVIEW_SQL, [review.submissionId, review.ruleId]);
    if (!found.rows.length) {
      return { status: 404, body: { result_state: 'not_found', error: { code: 'no_such_finding', message: 'No finding with that submission and rule exists for this tenant.' } } };
    }
    const saved = await client.query(UPSERT_REVIEW_SQL, [review.submissionId, review.ruleId, review.reviewState, principal.actorId, review.note]);
    const audit = findingReviewAuditStatement({
      actorId: principal.actorId,
      submissionId: review.submissionId,
      ruleId: review.ruleId,
      reviewState: review.reviewState,
      note: review.note,
      subjectRef: found.rows[0].user_ref ?? null,
      caseReference: principal.caseReference ?? null,
      sessionId: principal.sessionId ?? null,
    });
    auditing();
    const auditRow = await client.query(audit.text, audit.params);
    const row = saved.rows[0] ?? {};
    return {
      status: 200,
      body: {
        api_version: '1',
        result_state: 'ok',
        data: {
          submission_id: review.submissionId,
          rule_id: review.ruleId,
          review_state: row.review_state ?? review.reviewState,
          reviewed_by: row.reviewed_by ?? principal.actorId,
          reviewed_at: isoOrNull(row.reviewed_at),
        },
        ...auditBlock(auditRow.rows[0]),
      },
    };
  }

  /** POST /v1/tool-sanction: record a tool's sanction state. */
  async function sanctionTool(client, principal, sanction, auditing) {
    // The previous decision, for the audit detail. An absent row is `unknown`, a real answer.
    const existing = await client.query(TOOL_SANCTION_FOR_REVIEW_SQL, [sanction.toolFingerprint]);
    const previousState = existing.rows[0]?.sanctioned_state ?? 'unknown';
    const saved = await client.query(UPSERT_TOOL_SANCTION_SQL, [sanction.toolFingerprint, sanction.displayName, sanction.sanctionedState, principal.actorId]);
    const audit = toolSanctionAuditStatement({
      actorId: principal.actorId,
      toolFingerprint: sanction.toolFingerprint,
      sanctionedState: sanction.sanctionedState,
      previousState,
      displayName: sanction.displayName,
      note: sanction.note,
      caseReference: principal.caseReference ?? null,
      sessionId: principal.sessionId ?? null,
    });
    auditing();
    const auditRow = await client.query(audit.text, audit.params);
    const row = saved.rows[0] ?? {};
    return {
      status: 200,
      body: {
        api_version: '1',
        result_state: 'ok',
        data: {
          tool_fingerprint: row.tool_fingerprint ?? sanction.toolFingerprint,
          display_name: row.display_name ?? sanction.displayName,
          sanctioned_state: row.sanctioned_state ?? sanction.sanctionedState,
          previous_state: previousState,
          decided_by: row.decided_by ?? (sanction.sanctionedState === 'unknown' ? null : principal.actorId),
          decided_at: isoOrNull(row.decided_at),
        },
        ...auditBlock(auditRow.rows[0]),
      },
    };
  }

  /**
   * The two content reads (content.js), forwarded to the vault, which decides everything. They
   * share the admission gate with /v1/query so a slow vault cannot exhaust the process.
   */
  async function content(req, res, path) {
    const principal = await authenticate(req, res, { contentShape: true });
    if (!principal) return;
    // Checked here before the vault is called; the vault checks the same role again, because it is
    // the component that returns content.
    const capability = capabilityForEndpoint(path);
    if (!rolesAllow(principal.roles, capability)) {
      sendJson(res, 403, { state: 'refused', error: { code: 'role', message: `role ${principal.roles.join(',')} may not use ${capability}` } });
      return;
    }
    const body = await readJson(req, res, { contentShape: true });
    if (body === undefined) return;
    await admitted(res, async () => {
      const answer = await forwarder.handle(path, principal, body);
      if (path === CONTENT_PATHS.SEARCH && answer.status === 200 && Array.isArray(answer.body?.hits)) {
        answer.body.hits = await describeHits(principal, answer.body.hits);
      }
      sendJson(res, answer.status, answer.body);
    }, { contentShape: true });
  }

  /**
   * Who and where each search hit is: the person, the device and the tool of its submission.
   *
   * The vault answers a search with a submission and a fragment, which is all it knows. The
   * submission's metadata is this service's to read under the tenant's row-level security, and it
   * adds no content. A lookup that fails leaves the hits as the vault served them.
   */
  async function describeHits(principal, hits) {
    const ids = [...new Set(hits.map((h) => h?.submission_id).filter((id) => typeof id === 'string' && HIT_ID.test(id)))];
    if (ids.length === 0) return hits;
    let rows;
    try {
      rows = await withClient((client) => inTransaction(client, { tenant: principal.tenant, statementTimeoutMs: OPERATIONAL_TIMEOUT_MS }, async () => (await client.query(
        `SELECT s.submission_id::text AS submission_id, s.user_ref AS user_ref, s.subject_name AS subject_name,
                ud.display_name AS directory_name,
                s.tool_fingerprint AS tool, ops.tool_display_name(s.tool_fingerprint) AS tool_name,
                s.device_id::text AS device, d.hostname AS hostname
           FROM ingest.submission s
           LEFT JOIN ops.device d
             ON d.tenant_id = s.tenant_id AND d.device_id = s.device_id
           LEFT JOIN ops.user_dim ud
             ON ud.tenant_id = s.tenant_id AND ud.user_ref = s.user_ref
          WHERE s.tenant_id = $1::uuid AND s.submission_id = ANY($2::uuid[])`,
        [principal.tenant, ids],
      )).rows));
    } catch (error) {
      log.warn('search hits could not be described', { error: String(error?.message ?? error) });
      return hits;
    }
    const known = new Map(rows.map((row) => [row.submission_id, row]));
    return hits.map((hit) => {
      const row = known.get(hit.submission_id);
      if (!row) return hit;
      return {
        ...hit,
        // The name at submission time when there is one, else the pseudonymous ref, so a hit is
        // always attributable to something an analyst can act on.
        subject: row.subject_name ?? row.user_ref ?? null,
        // The directory's current display name, beside the account name the device reported. The
        // directory sync stores none for a tenant whose device identity is hashed.
        directory_name: row.directory_name ?? null,
        tool: row.tool ?? null,
        tool_name: row.tool_name ?? null,
        device: row.device ?? null,
        hostname: row.hostname ?? null,
      };
    });
  }

  const ROUTES = new Map([
    [`GET ${PATHS.LIVENESS}`, (req, res) => liveness(res)],
    [`GET ${PATHS.READINESS}`, (req, res) => readiness(res)],
    [`POST ${PATHS.QUERY}`, query],
    [`POST ${PATHS.FINDING_REVIEW}`, (req, res) => auditedWrite(req, res, PATHS.FINDING_REVIEW, validateReviewRequest, reviewFinding)],
    [`POST ${PATHS.TOOL_SANCTION}`, (req, res) => auditedWrite(req, res, PATHS.TOOL_SANCTION, validateSanctionRequest, sanctionTool)],
    [`POST ${CONTENT_PATHS.SEARCH}`, (req, res) => content(req, res, CONTENT_PATHS.SEARCH)],
    [`POST ${CONTENT_PATHS.RETRIEVAL}`, (req, res) => content(req, res, CONTENT_PATHS.RETRIEVAL)],
  ]);
  const KNOWN_PATHS = new Set([...ROUTES.keys()].map((key) => key.split(' ')[1]));

  return function handler(req, res) {
    const path = (req.url ?? '/').split('?')[0];
    const route = ROUTES.get(`${req.method} ${path}`);
    if (route) {
      Promise.resolve(route(req, res)).catch((error) => {
        if (!res.headersSent) sendError(res, error);
        else res.destroy();
      });
      return;
    }
    if (KNOWN_PATHS.has(path)) {
      sendJson(res, 405, { result_state: 'not_found', error: { code: 'method_not_allowed', message: `${req.method} is not allowed on ${path}` } }, { allow: 'GET, POST' });
      return;
    }
    sendJson(res, 404, { result_state: 'not_found', error: { code: 'not_found', message: 'no such path' } });
  };
}

/**
 * The HTTP server. `close()` stops the listener, drops idle keep-alive sockets, and ends the pool.
 */
export function createQueryServer(options) {
  const handler = createHandler(options);
  const log = options.log ?? console;
  const requestTimeoutMs = options.requestTimeoutMs ?? DEFAULT_REQUEST_TIMEOUT_MS;
  const server = createServer((req, res) => {
    // A per-request timer cleared on 'close', rather than res.setTimeout: a socket timeout outlives
    // the response and keeps idle keep-alive connections (and the process) alive.
    const started = Date.now();
    const timer = setTimeout(() => {
      if (!res.headersSent) {
        const body = JSON.stringify({ result_state: 'busy', error: { code: 'request_timeout', message: 'the request exceeded its time budget' } });
        res.writeHead(RESULT_STATES.busy.http, { 'content-type': 'application/json; charset=utf-8', connection: 'close' });
        res.end(body);
      }
      req.destroy();
    }, requestTimeoutMs);
    timer.unref?.();
    res.on('close', () => clearTimeout(timer));
    const path = (req.url ?? '/').split('?')[0];
    if (path !== PATHS.LIVENESS && path !== PATHS.READINESS) {
      res.on('finish', () => log.info?.('request', { method: req.method, path, status: res.statusCode, ms: Date.now() - started }));
    }
    handler(req, res);
  });
  return {
    server,
    handler,
    listen(address) {
      return new Promise((resolve, reject) => {
        server.once('error', reject);
        server.listen(address.port, address.host, () => {
          server.removeListener('error', reject);
          resolve(server.address());
        });
      });
    },
    async close() {
      server.closeAllConnections?.();
      await new Promise((resolve) => server.close(() => resolve()));
      await options.pool?.end?.();
    },
  };
}
