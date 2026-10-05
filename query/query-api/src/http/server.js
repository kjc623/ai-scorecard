// server.js — the HTTP surface of query-api.
//
// WHY THIS FILE EXISTS. Everything this service is known for was already built: the closed DSL
// (validate.js), the allow-list (registry.js), the compiler (compile.js), the pipeline
// (plan.js), the ten templates and the audit chain. What did not exist was anything that LISTENS.
// azure/main.bicep declared a container app named query-api with an image, docs/04 documented
// POST /v1/query, and there was no server: `grep -r 'createServer|listen(' query/query-api` returned
// nothing. The deployment template named a service that could not be built or run.
//
// WHAT THIS LAYER DOES, and deliberately does not do. It is transport only:
//   - it reads the request, and refuses one it cannot read;
//   - it puts the tenant on the query context from a TRUSTED source, never from the body;
//   - it calls plan() and executePlan(), which own validation, guard, compile, audit and suppress;
//   - it renders the result the pipeline produced, and the typed rejection the pipeline raised.
//
// It makes no decision about what a query means, what a value may be, or what an error is. Those
// live in the modules it calls, and duplicating any of them here would be a second source of truth
// for the product's central claim — that the browser never speaks SQL.

import { createServer } from 'node:http';
import { QueryError } from '../errors.js';
import { plan, executePlan } from '../plan.js';
import { RESULT_STATES } from '../errors.js';
import { CONTENT_PATHS, createContentForwarder } from './content.js';
import { validateReviewRequest, FINDING_FOR_REVIEW_SQL, UPSERT_REVIEW_SQL, findingReviewAuditStatement } from '../review.js';

const HIT_ID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/** The two paths azure/modules/container-app.bicep probes, and the one read path docs/04 §5 names. */
export const PATHS = Object.freeze({
  LIVENESS: '/healthz',
  READINESS: '/readyz',
  QUERY: '/v1/query',
  FINDING_REVIEW: '/v1/finding-review',
});

/** §6.2: a request body larger than this is refused before it is parsed, not after. */
export const MAX_BODY_BYTES = 256 * 1024;

/** The default ceiling on how long the whole request may take, including the queue wait. */
export const DEFAULT_REQUEST_TIMEOUT_MS = 30_000;

const LIVENESS_BODY = Object.freeze({ status: 'ok' });
const READY_BODY = Object.freeze({ status: 'ready' });
const NOT_READY_BODY = Object.freeze({ status: 'not-ready' });

/**
 * The one error shape this layer produces that is NOT a QueryError: a transport-level refusal.
 *
 * It uses the same envelope as a typed rejection because a client that has to handle two error
 * shapes will handle one of them wrongly. `busy` is §13's own state for "the service is shedding
 * load", so a shed request is indistinguishable, to a well-behaved client, from one the pipeline
 * shed itself.
 */
function transportError(resultState, code, message) {
  const spec = RESULT_STATES[resultState] ?? RESULT_STATES.busy;
  return { status: spec.http, body: { result_state: resultState, error: { code, message } } };
}

class HttpError extends Error {
  constructor(status, payload, { closeConnection = false } = {}) {
    super(payload?.error?.message ?? `HTTP ${status}`);
    this.status = status;
    this.payload = payload;
    this.closeConnection = closeConnection;
  }
}

/** Read a body with a hard ceiling, without ever buffering more than the ceiling. */
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
        // Settle the promise instead of destroying the socket here. Destroying first closes the
        // connection under the response the caller is about to write, so the client sees "fetch
        // failed" rather than the 413 that explains what happened. The caller answers, and the
        // response closes the connection (see `closeConnection`).
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

function sendJson(res, status, body) {
  const text = JSON.stringify(body);
  res.writeHead(status, {
    'content-type': 'application/json; charset=utf-8',
    'content-length': Buffer.byteLength(text),
    // A query response is per-tenant and per-user. Nothing between here and the browser may keep it.
    'cache-control': 'no-store',
    'x-content-type-options': 'nosniff',
  });
  res.end(text);
}

/**
 * The tenant, and the actor, for this request.
 *
 * TENANT IS NEVER READ FROM THE BODY. REASON.TENANT_IN_REQUEST exists because a request that tries
 * to choose its own tenant is refused as a validation error rather than ignored, and that only
 * means something if the value this server uses came from somewhere the caller cannot write.
 *
 * NOTE — this is the honest state, not a finished one. The authenticated session (an Entra
 * principal resolved to exactly one ops.tenant.tenant_id, docs/04 §4.2) is NOT BUILT YET. Until it
 * is, the only trusted source available is a development-only header, and it is accepted only when
 * SAC_DEV_TRUST_PRINCIPAL=1 — the same explicit escape hatch the Go services use for a local run.
 * Without it, every query is refused 403 with `unauthorised_role`, because a service that cannot
 * establish who is asking must not answer.
 */
function principalOf(req, cfg) {
  if (!cfg.devTrustPrincipal) return null;
  const tenant = req.headers['x-sac-dev-tenant'];
  if (typeof tenant !== 'string' || tenant.trim() === '') return null;
  const actor = req.headers['x-sac-dev-actor'];
  const caseRef = req.headers['x-sac-dev-case'];
  return {
    tenant: tenant.trim(),
    actorId: typeof actor === 'string' && actor.trim() !== '' ? actor.trim() : 'unknown',
    caseReference: typeof caseRef === 'string' && caseRef.trim() !== '' ? caseRef.trim() : null,
  };
}

/** Turn anything thrown by the pipeline or the driver into a wire-legal response. */
function renderError(error, log) {
  if (error instanceof QueryError) {
    // The pipeline already decided the state, the reason, the fix and the HTTP status. This layer
    // does not get a second opinion.
    return { status: error.http, body: error.toEnvelope() };
  }
  if (error instanceof HttpError) {
    return { status: error.status, body: error.payload };
  }
  // A driver error carries a SQLSTATE. Mapping it here rather than letting it become a 500 is what
  // keeps "the database refused this" distinguishable from "this service is broken".
  const code = typeof error?.code === 'string' ? error.code : '';
  if (code === '57014') {
    return transportError('busy', 'statement_timeout', 'the statement exceeded its server-side budget');
  }
  if (code === '40001' || code === '40P01') {
    return transportError('busy', 'serialization_failure', 'the transaction was rolled back and may be retried');
  }
  if (error instanceof QueryError) return { status: error.http, body: error.toEnvelope() };
  // Anything else is a defect, and a defect is logged in full and reported in outline: an internal
  // message can name a host, a column or a DSN fragment.
  log?.error?.('unhandled error in the read path', error);
  return {
    status: 500,
    body: { result_state: 'audit_chain_broken', error: { code: 'internal_error', message: 'the service could not complete this read' } },
  };
}

/**
 * A bounded concurrency gate with a bounded queue.
 *
 * §12.3: per-tenant concurrency is the limit that matters because the audit hash chain serialises
 * per tenant, and beyond the queue the answer is an immediate `busy` (429) — never unbounded
 * queueing, which turns one tenant's pathological query into every tenant's latency.
 */
export function createGate({ maxConcurrency, maxQueue }) {
  let active = 0;
  const queue = [];

  function release() {
    active -= 1;
    const next = queue.shift();
    if (next) {
      active += 1;
      next();
    }
  }

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
        return Promise.reject(
          new HttpError(429, { result_state: 'busy', error: { code: 'busy', message: 'the service is at its concurrency limit; retry shortly' } }),
        );
      }
      return new Promise((resolve) => queue.push(resolve));
    },
    release,
  };
}

/**
 * A pool borrowed from, or a bare client.
 *
 * The two are behind one shape because a test wants to hand in a fake and the service wants a pool,
 * and neither should have to know about the other. A bare client is wrapped as a one-connection
 * source: correct for a fake, and deliberately NOT how the service runs — a single session cannot
 * serve two requests, which is the defect the pool exists to fix.
 */
function connectionSource({ pool, client, log }) {
  if (pool) {
    return {
      async acquire() {
        return pool.acquire();
      },
      useTenant(conn, tenant) {
        // The pool owns this, because it also owns the reset on release. The two are one mechanism.
        return pool.useTenant(conn, tenant);
      },
      async release(conn) {
        await pool.release(conn);
      },
      async close() {
        await pool.close();
      },
      kind: 'pool',
    };
  }
  if (!client || typeof client.query !== 'function') throw new Error('createHandler needs a pool or a client with query(text, params).');
  return {
    async acquire() {
      return client;
    },
    async useTenant(conn, tenant) {
      // The same statement the pool issues, so a test exercises the tenant plumbing rather than
      // stepping around it. A fake client records it; that is how the test asserts it happened.
      await conn.query("SELECT set_config('app.tenant_id', $1, false)", [tenant]);
    },
    async release() {},
    async close() {
      await client.close?.();
    },
    kind: 'single',
  };
}

/**
 * Build the request handler.
 *
 * Everything is injected: the pool (or, in a test, a single client), the clock, the logger. That is
 * what lets the whole surface be tested without a database, and it is the same seam executePlan()
 * already exposes.
 */
export function createHandler({
  cfg,
  pool = null,
  client = null,
  cursorStore = null,
  cursorKey = null,
  now = () => new Date(),
  log = console,
  requestTimeoutMs = DEFAULT_REQUEST_TIMEOUT_MS,
  gate = createGate({ maxConcurrency: cfg?.limits?.maxConcurrency ?? 8, maxQueue: cfg?.limits?.maxQueue ?? 32 }),
  out = null,
  contentForwarder = null,
} = {}) {
  if (!cfg) throw new Error('createHandler needs a configuration');
  const source = connectionSource({ pool, client, log });
  // Exposed so createQueryServer can close the same source the handler borrows from. Without this
  // the server would close its listener and leave the pool's connections open, which a test notices
  // as a process that will not exit.
  if (typeof out?.captureSource === 'function') out.captureSource(source);  if (source.kind === 'single') {
    // Said out loud rather than discovered under load. A single connection is a test fixture; in the
    // service it produced SAC_BUSY for the second concurrent request and for any probe during a read.
    log?.warn?.(
      'query-api: serving from a single database connection. This is a test configuration: a real ' +
        'deployment must pass a pool, because one session cannot run two transactions.',
    );
  }

  /** Liveness: is the process up. Never touches a dependency, so the platform may restart on it. */
  function liveness(res) {
    sendJson(res, 200, { ...LIVENESS_BODY, role: cfg.role });
  }

  /**
   * Readiness: deliberately not liveness. A broken database connection means "take me out of
   * rotation", not "kill me" — the distinction azure/modules/container-app.bicep's own comment asks
   * for, and the same one ingestion/ingest-api/cmd/ingest-api/probes.go implements.
   *
   * It borrows its own connection rather than sharing one with a read: a probe that waits behind a
   * query is a probe that reports a busy service as unready, and one that borrows a session mid
   * transaction cannot run at all.
   */
  async function readiness(res) {
    let conn = null;
    try {
      conn = await source.acquire();
      await conn.query('SELECT 1');
      sendJson(res, 200, { ...READY_BODY, role: cfg.role });
    } catch {
      // The reason is not returned: its reader is the platform, and a driver error can carry a
      // hostname. It is logged instead.
      sendJson(res, 503, NOT_READY_BODY);
    } finally {
      if (conn) await source.release(conn);
    }
  }

  async function query(req, res) {
    const started = Date.now();
    const principal = principalOf(req, cfg);
    if (!principal) {
      // Fail closed. A read path that cannot say who is asking does not answer, and it says which
      // of the two reasons it was: no session, or a session that is not built yet.
      const body = {
        result_state: 'unauthorised_role',
        error: {
          code: 'role',
          message: cfg.devTrustPrincipal
            ? 'no tenant was established for this request'
            : 'the authenticated session is not built yet; this service refuses to guess a tenant',
        },
      };
      sendJson(res, 403, body);
      return;
    }

    let raw;
    try {
      raw = await readBody(req);
    } catch (error) {
      const rendered = renderError(error, log);
      if (error instanceof HttpError && error.closeConnection) {
        // An oversized body cannot be drained for ever: answer, then close. The client that kept
        // uploading is told why, which is the whole point of answering before disconnecting.
        res.setHeader('connection', 'close');
        sendJson(res, rendered.status, rendered.body);
        res.on('finish', () => req.destroy());
        return;
      }
      sendJson(res, rendered.status, rendered.body);
      return;
    }

    let request;
    try {
      request = raw.trim() === '' ? {} : JSON.parse(raw);
    } catch {
      const rendered = renderError(
        new HttpError(400, { result_state: 'unsupported_query_shape', error: { code: 'malformed_document', message: 'the request body is not JSON' } }),
        log,
      );
      sendJson(res, rendered.status, rendered.body);
      return;
    }

    let conn = null;
    try {
      await gate.acquire();
    } catch (error) {
      const rendered = renderError(error, log);
      sendJson(res, rendered.status, rendered.body);
      return;
    }

    try {
      conn = await source.acquire();

      // The tenant goes on the SESSION before anything is planned or executed.
      //
      // database/schema.sql enforces isolation with FORCE ROW LEVEL SECURITY, and every scoped
      // policy compares against ops.current_tenant(), which reads `app.tenant_id`. Without this the
      // read path returns nothing for every query — fail-closed, so not a leak, but an absence of
      // data presented where the honest answer is "this session asked as nobody".
      //
      // `set_config(name, value, false)` rather than `SET LOCAL`: a utility statement cannot take a
      // bind parameter, and a tenant interpolated into SQL text is the one thing this whole service
      // exists to avoid. The pool clears it again on release — that reset is what makes it safe to
      // hand the same connection to the next tenant.
      await source.useTenant(conn, principal.tenant);

      // The whole read happens inside one transaction that COMMITs before anything is served
      // (§5.1: subject-level responses are not streamed), so a cancelled read has served nothing.
      const ctx = {
        client: conn,
        tenant: principal.tenant,
        actorId: principal.actorId,
        caseReference: principal.caseReference,
        cursorStore,
        cursorKey,
        now: now(),
      };
      const planned = plan(request, ctx);
      const envelope = await executePlan(planned, ctx);
      sendJson(res, 200, envelope);
    } catch (error) {
      const rendered = renderError(error, log);
      sendJson(res, rendered.status, rendered.body);
    } finally {
      if (conn) {
        try {
          await source.release(conn);
        } catch (error) {
          // A connection that cannot be returned is closed by the pool; the request is already
          // answered, so this is a warning and not a second error for the caller.
          log?.warn?.(`query-api: could not return a connection to the pool: ${error?.message ?? error}`);
        }
      }
      gate.release();
      const ms = Date.now() - started;
      log?.info?.(`query-api ${request?.source ?? request?.template ?? '?'} ${ms}ms`);
    }
  }

  /**
   * Finding review (review.js). The one WRITE this service accepts, and it is here because
   * `sac_query` already holds INSERT+UPDATE on `ops.finding_review` and INSERT on `ops.audit`
   * (db/schema.sql §10), and a review is meaningless except beside the read that shows it. The
   * tenant is the session's and the reviewer is the authenticated principal; neither is read from
   * the body. The review and its audit row commit together, so a response is never served for a
   * judgement that was not recorded.
   */
  async function findingReview(req, res) {
    const principal = principalOf(req, cfg);
    if (!principal) {
      sendJson(res, 403, {
        result_state: 'unauthorised_role',
        error: {
          code: 'role',
          message: cfg.devTrustPrincipal
            ? 'no tenant was established for this request'
            : 'the authenticated session is not built yet; this service refuses to guess a tenant',
        },
      });
      return;
    }

    let body;
    try {
      const raw = await readBody(req);
      body = raw.trim() === '' ? {} : JSON.parse(raw);
    } catch (error) {
      const rendered = renderError(error, log);
      sendJson(res, rendered.status, rendered.body);
      return;
    }

    let review;
    try {
      review = validateReviewRequest(body);
    } catch (error) {
      const rendered = renderError(error, log);
      sendJson(res, rendered.status, rendered.body);
      return;
    }

    try {
      await gate.acquire();
    } catch (error) {
      const rendered = renderError(error, log);
      sendJson(res, rendered.status, rendered.body);
      return;
    }

    let conn = null;
    try {
      conn = await source.acquire();
      await source.useTenant(conn, principal.tenant);
      await conn.begin();

      // The finding must exist for this tenant. Because the predicate is ops.current_tenant(), a
      // submission id from another tenant is not found rather than found-and-refused.
      const found = await conn.query(FINDING_FOR_REVIEW_SQL, [review.submissionId, review.ruleId]);
      if (!found?.rows?.length) {
        await conn.rollback();
        sendJson(res, 404, {
          result_state: 'not_found',
          error: { code: 'no_such_finding', message: 'No finding with that submission and rule exists for this tenant.' },
        });
        return;
      }
      const subjectRef = found.rows[0].user_ref ?? null;

      const saved = await conn.query(UPSERT_REVIEW_SQL, [
        review.submissionId,
        review.ruleId,
        review.reviewState,
        principal.actorId,
        review.note,
      ]);
      const audit = findingReviewAuditStatement({
        actorId: principal.actorId,
        submissionId: review.submissionId,
        ruleId: review.ruleId,
        reviewState: review.reviewState,
        note: review.note,
        subjectRef,
        caseReference: principal.caseReference,
      });
      const auditRow = await conn.query(audit.text, audit.params);
      await conn.commit();

      const row = saved?.rows?.[0] ?? {};
      const auditEntry = auditRow?.rows?.[0] ?? null;
      sendJson(res, 200, {
        api_version: '1',
        result_state: 'ok',
        data: {
          submission_id: review.submissionId,
          rule_id: review.ruleId,
          review_state: row.review_state ?? review.reviewState,
          reviewed_by: row.reviewed_by ?? principal.actorId,
          reviewed_at: row.reviewed_at instanceof Date ? row.reviewed_at.toISOString() : (row.reviewed_at ?? null),
        },
        ...(auditEntry
          ? {
              audit: {
                entry_id: auditEntry.audit_seq === undefined ? null : String(auditEntry.audit_seq),
                written_at: auditEntry.occurred_at instanceof Date ? auditEntry.occurred_at.toISOString() : (auditEntry.occurred_at ?? null),
              },
            }
          : {}),
      });
    } catch (error) {
      if (conn) {
        try {
          await conn.rollback();
        } catch {
          // A rollback failure must not mask the original error.
        }
      }
      const rendered = renderError(error, log);
      sendJson(res, rendered.status, rendered.body);
    } finally {
      if (conn) {
        try {
          await source.release(conn);
        } catch (error) {
          log?.warn?.(`query-api: could not return a connection to the pool: ${error?.message ?? error}`);
        }
      }
      gate.release();
    }
  }

  const forwarder = contentForwarder ?? createContentForwarder({ vaultUrl: cfg.contentVaultUrl, scope: cfg.contentSearchScope, log });

  /**
   * The two content reads (content.js). They are forwarded, never answered here: this service
   * establishes who is asking and the vault decides everything else. They share the concurrency
   * gate with /v1/query so a slow vault cannot exhaust the process.
   */
  async function content(req, res, path) {
    const principal = principalOf(req, cfg);
    if (!principal) {
      sendJson(res, 403, { state: 'refused', error: { code: 'role', message: 'no tenant was established for this request' } });
      return;
    }
    let body;
    try {
      const raw = await readBody(req);
      body = raw.trim() === '' ? {} : JSON.parse(raw);
    } catch (error) {
      const status = error instanceof HttpError ? error.status : 400;
      sendJson(res, status, { state: 'refused', error: { code: status === 413 ? 'body_too_large' : 'malformed_document', message: 'the request body could not be read as JSON' } });
      return;
    }
    try {
      await gate.acquire();
    } catch {
      sendJson(res, 429, { state: 'refused', error: { code: 'busy', message: 'the service is at its concurrency limit; retry shortly' } });
      return;
    }
    try {
      const answer = await forwarder.handle(path, principal, body);
      if (path === CONTENT_PATHS.SEARCH && answer.status === 200 && Array.isArray(answer.body?.hits)) {
        answer.body.hits = await describeHits(principal, answer.body.hits);
      }
      sendJson(res, answer.status, answer.body);
    } finally {
      gate.release();
    }
  }

  /**
   * Who and where each search hit is: the person, the device and the tool of its submission.
   *
   * The vault answers a search with a submission and a fragment, because that is all it knows.
   * The submission's metadata is this service's to read (ingest.submission, under the tenant's
   * row-level security), and a hit an analyst cannot place is one they must open to understand.
   * The search itself is already audited by the vault; this adds no content. A lookup that fails
   * leaves the hits as the vault served them.
   */
  async function describeHits(principal, hits) {
    const ids = [...new Set(hits.map((h) => h?.submission_id).filter((id) => typeof id === 'string' && HIT_ID.test(id)))];
    if (ids.length === 0) return hits;
    let conn = null;
    try {
      conn = await source.acquire();
      await source.useTenant(conn, principal.tenant);
      const result = await conn.query(
        `SELECT s.submission_id::text AS submission_id, s.user_ref AS user_ref, s.subject_name AS subject_name,
                s.tool_fingerprint AS tool, s.device_id::text AS device, d.hostname AS hostname
           FROM ingest.submission s
           LEFT JOIN ops.device d
             ON d.tenant_id = s.tenant_id AND d.device_id = s.device_id
          WHERE s.tenant_id = $1::uuid AND s.submission_id = ANY(string_to_array($2::text, ',')::uuid[])`,
        [principal.tenant, ids.join(',')],
      );
      const known = new Map((result?.rows ?? []).map((row) => [row.submission_id, row]));
      return hits.map((hit) => {
        const row = known.get(hit.submission_id);
        if (!row) return hit;
        return {
          ...hit,
          // The clear name at submission time when there is one, else the pseudonymous ref, so a hit
          // is always attributable to something an analyst can act on (ADR 0021, docs/04 §15.3).
          subject: row.subject_name ?? row.user_ref ?? null,
          tool: row.tool ?? null,
          device: row.device ?? null,
          hostname: row.hostname ?? null,
        };
      });
    } catch (error) {
      log?.warn?.(`query-api: search hits could not be described: ${error?.message ?? error}`);
      return hits;
    } finally {
      if (conn) {
        try {
          await source.release(conn);
        } catch (error) {
          log?.warn?.(`query-api: could not return a connection to the pool: ${error?.message ?? error}`);
        }
      }
    }
  }

  return function handler(req, res) {
    const url = req.url ?? '/';
    if (req.method === 'GET' && url.split('?')[0] === PATHS.LIVENESS) return liveness(res);
    if (req.method === 'GET' && url.split('?')[0] === PATHS.READINESS) return void readiness(res);
    if (req.method === 'POST' && url.split('?')[0] === PATHS.QUERY) return void query(req, res);
    if (req.method === 'POST' && url.split('?')[0] === PATHS.FINDING_REVIEW) return void findingReview(req, res);
    if (req.method === 'POST' && Object.values(CONTENT_PATHS).includes(url.split('?')[0])) return void content(req, res, url.split('?')[0]);

    if (url.split('?')[0] === PATHS.QUERY || url.split('?')[0] === PATHS.LIVENESS || url.split('?')[0] === PATHS.READINESS || url.split('?')[0] === PATHS.FINDING_REVIEW) {
      res.writeHead(405, { 'content-type': 'application/json; charset=utf-8', allow: 'GET, POST' });
      res.end(JSON.stringify({ result_state: 'not_found', error: { code: 'method_not_allowed', message: `${req.method} is not allowed on ${url}` } }));
      return;
    }
    res.writeHead(404, { 'content-type': 'application/json; charset=utf-8' });
    res.end(JSON.stringify({ result_state: 'not_found', error: { code: 'not_found', message: 'no such path' } }));
  };
}

/**
 * Start listening.
 *
 * The returned object exposes `close()` so a test can start and stop one without leaking a port,
 * and `url` so a caller can print where it is.
 */
export function createQueryServer(options) {
  // The handler creates the connection source; this collects it so close() can shut it down with
  // the listener. One source, one owner, one place that closes it.
  const shared = { source: null };
  const handler = createHandler({ ...options, out: { captureSource: (source) => (shared.source = source) } });
  const requestTimeoutMs = options.requestTimeoutMs ?? DEFAULT_REQUEST_TIMEOUT_MS;
  const server = createServer((req, res) => {
    // A hung request must not hold a connection slot for ever.
    //
    // The timer is set here and cleared on 'close', deliberately NOT with res.setTimeout: a
    // response-object timeout is a *socket* timeout, so it outlives the response, keeps one timer
    // per idle keep-alive connection alive, and holds the process open for its full duration after
    // the last request has been answered. That is invisible in production and very visible in a
    // test suite, which then sits idle for the whole timeout.
    const timer = setTimeout(() => {
      if (!res.headersSent) {
        const rendered = transportError('busy', 'request_timeout', 'the request exceeded its time budget');
        res.writeHead(rendered.status, { 'content-type': 'application/json; charset=utf-8', connection: 'close' });
        res.end(JSON.stringify(rendered.body));
      }
      req.destroy();
    }, requestTimeoutMs);
    timer.unref?.();
    res.on('close', () => clearTimeout(timer));
    handler(req, res);
  });
  return {
    server,
    handler,
    /** The connection source, so a caller can close the pool with the listener. */
    closeConnections: () => shared.source?.close(),
    listen(address) {
      return new Promise((resolve, reject) => {
        server.once('error', reject);
        server.listen(address.port, address.host, () => {
          server.removeListener('error', reject);
          resolve(server.address());
        });
      });
    },
    close() {
      // Close idle keep-alive sockets as well as the listener. Without this, close() waits for a
      // client that is holding a connection open, which is exactly what a fetch-based test does.
      server.closeAllConnections?.();
      return new Promise((resolve) => server.close(() => resolve())).then(() => shared.source?.close());
    },
  };
}
