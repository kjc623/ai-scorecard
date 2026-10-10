// plan.js — the whole read path, in order.
//
//   request (template or DSL document)
//     -> expand template            (templates.js)
//     -> validate                   (validate.js: closed document, typed rejection)
//     -> guard                      (guard.js: cost refused before execution)
//     -> resolve cursor             (cursor.js: bound to tenant, query and ordering)
//     -> compile                    (compile.js: parameterised SQL)
//     -> audit decision             (audit.js: fail closed)
//     -> execute in one transaction (executePlan below)
//     -> suppress                   (suppress.js)
//     -> envelope                   (envelope.js)
//
// `plan()` is pure and synchronous: it is what tests and the hostile-input corpus run through.
// `executePlan()` takes a database connection and runs the plan inside one tenant-scoped
// transaction.

import { createHash } from 'node:crypto';
import { API_VERSION, QUERY_CLASSES, QUERY_VERSION, SOURCE_WATERMARK } from './registry.js';
import { PROHIBITED_FIELDS, REASON, fromDatabaseError, unsupported } from './errors.js';
import { validate, canonicalJson } from './validate.js';
import { guard } from './guard.js';
import { compile, effectiveOrderKeys } from './compile.js';
import { expandTemplate } from './templates.js';
import {
  AUDIT_ACTIONS,
  auditDecision,
  auditDetail,
  auditPlan,
  auditStatement,
  subjectRefOf,
  verifyAuditPage,
} from './audit.js';
import {
  buildEnvelope,
  coverageBlock,
  freshnessBlock,
  resultStateFor,
} from './envelope.js';
import {
  coverageStatement,
  freshnessStatement,
  newerEventsStatement,
  resolveMissingRecord,
} from './blocks.js';
import { decodeCursor, encodeCursor, paginate } from './cursor.js';
import { inTransaction } from './db.js';

/**
 * @typedef {object} Plan
 * @property {boolean} ok
 * @property {'query'|'single'} mode
 * @property {object|null} query
 * @property {object|null} source
 * @property {string} dsl_hash
 * @property {object|null} compiled
 * @property {ReadonlyArray<{id:string,text:string,params:ReadonlyArray<unknown>}>} statements
 * @property {object} audit
 * @property {object} meta
 */

const SIDE_READ_IDS = Object.freeze([
  'freshness',
  'coverage',
  'newer_events',
  'flush_check',
  'class_total',
  'org_coverage',
  'submission_detail',
  'erasure_evidence',
  'device_status',
]);

/**
 * Build the statement plan for one request. Throws QueryError for every rejection.
 *
 * @param {object} request   a DSL document, or {template, params}
 * @param {object} [ctx]
 * @param {string} [ctx.tenant]        the authenticated session's tenant (never from the body)
 * @param {string} [ctx.actorId]
 * @param {string} [ctx.caseReference]
 * @param {string|null} [ctx.sessionId]  the token's `sid`, carried into the audit row's detail
 * @param {Date|string} [ctx.now]
 * @param {string} [ctx.snapshotUpper]
 * @param {Buffer|string} [ctx.cursorKey]  required when the request carries a cursor
 * @returns {Plan}
 */
export function plan(request, ctx = {}) {
  const now = toDate(ctx.now ?? new Date());
  const expansion = isTemplateRequest(request) ? expandTemplate(request) : null;
  if (expansion) assertTemplateRequestKeys(request);
  const document = expansion ? expansion.document : request;

  if (expansion && expansion.document === null) {
    // A single-record template (q9) has no aggregate shape: it is a detail read with its own
    // statements, and its request is a record reference rather than a query document.
    return planSingle(expansion, ctx, now);
  }

  const validated = validate(document);
  const guarded = guard(validated);
  const query = guarded.query;
  const source = validated.source;
  const klass = validated.klass;

  const orderKeys = effectiveOrderKeys(query, source);
  // The cursor is not part of the question it pages through, so it is left out of the hash.
  const dslHash = hashOf({ ...query, cursor: null });
  const resolvedCursor = resolveCursorFor(query, source, orderKeys, dslHash, ctx);

  const compiled = compile({ query, source, klass }, {
    cursorPosition: resolvedCursor.position,
    snapshotUpper: resolvedCursor.snapshotUpper,
  });

  const decision = auditDecision({ query, source }, { singleRecord: false });
  const auditStatements = decision.required && decision.phase === 'pre_read'
    ? [auditStatement(decision, {
      actorId: ctx.actorId ?? 'unknown',
      caseReference: ctx.caseReference ?? null,
      sessionId: ctx.sessionId ?? null,
      subjectRef: subjectRefOf(query),
      detail: auditDetail({ query, source }, { coarsened: guarded.coarsened }),
    })]
    : [];

  const sideReads = [];
  const watermark = SOURCE_WATERMARK[source.id];
  if (watermark) {
    sideReads.push(freshnessStatement(watermark, compiled.meta.native_bucket_size ?? 'day'));
  }
  sideReads.push(coverageStatement(query.window ?? currentDayWindow(now)));
  if (source.time && query.window) {
    sideReads.push(newerEventsStatement(source, resolvedCursor.snapshotUpper));
  }
  for (const extra of expansion?.extras ?? []) {
    const statement = extra(query);
    if (statement) sideReads.push(statement);
  }
  for (const statement of expansion?.statements ?? []) {
    const built = statement();
    if (built) sideReads.push(built);
  }

  const statements = [
    ...auditStatements,
    { id: 'read', text: compiled.text, params: compiled.params },
    ...sideReads,
  ];

  return Object.freeze({
    ok: true,
    mode: 'query',
    template: expansion ? { name: expansion.name, question: expansion.question, title: expansion.title } : null,
    query,
    source,
    // The registry id, so the role gate maps a read to its capability without re-deriving it.
    source_id: source.id,
    dsl_hash: dslHash,
    compiled,
    statements: Object.freeze(statements),
    audit: Object.freeze({
      decision,
      plan: auditPlan(decision, false),
      subjectRef: subjectRefOf(query),
      action: decision.action,
      object_type: decision.object_type,
    }),
    meta: Object.freeze({
      ...compiled.meta,
      api_version: API_VERSION,
      query_version: QUERY_VERSION,
      dsl_hash: dslHash,
      snapshot_upper_bound: resolvedCursor.snapshotUpper,
      coarsened: guarded.coarsened,
      guard: Object.freeze({
        buckets: guarded.buckets,
        estimated_cells: guarded.estimatedCells,
        bounded_cells: guarded.boundedCells,
        limited: guarded.limited,
        estimated_bytes: guarded.estimatedBytes,
      }),
      notes: Object.freeze([...(expansion?.notes ?? []), ...guarded.notes]),
    }),
  });
}

function planSingle(expansion, ctx, now) {
  const statements = [];
  for (const statement of expansion.statements) {
    const built = statement();
    if (built) statements.push(built);
  }
  // A missing record is ambiguous between "never existed" and "existed and was purged", and the
  // window it would have been received in decides which. The caller may name that window with
  // `received_at_hint`; without it the response says so rather than guessing.
  const hint = expansion.params?.received_at_hint ?? null;
  const decision = Object.freeze({
    required: true,
    phase: 'pre_read',
    action: AUDIT_ACTIONS.single_record,
    object_type: 'ingest.submission',
    reasons: Object.freeze(['single_record_detail']),
    selfAudited: true,
  });
  const auditStatementBuilt = auditStatement(decision, {
    actorId: ctx.actorId ?? 'unknown',
    caseReference: ctx.caseReference ?? null,
    sessionId: ctx.sessionId ?? null,
    subjectRef: ctx.subjectRef ?? null,
    detail: Object.freeze({ source: 'ingest.submission', kind: 'single_record', template: expansion.name }),
  });
  const sideReads = [
    coverageStatement(currentDayWindow(now)),
  ];
  return Object.freeze({
    ok: true,
    mode: 'single',
    template: { name: expansion.name, question: expansion.question, title: expansion.title },
    query: null,
    source: null,
    source_id: expansion.source ?? null,
    dsl_hash: hashOf({ template: expansion.name, params: expansion.params ?? null }),
    compiled: null,
    statements: Object.freeze([auditStatementBuilt, ...statements, ...sideReads]),
    audit: Object.freeze({ decision, plan: auditPlan(decision, true), subjectRef: ctx.subjectRef ?? null, action: decision.action, object_type: decision.object_type }),
    meta: Object.freeze({
      api_version: API_VERSION,
      query_version: QUERY_VERSION,
      single_record: true,
      query_class: 'single',
      statement_timeout_ms: QUERY_CLASSES.single.statementTimeoutMs,
      received_at_hint: hint,
      notes: Object.freeze([...expansion.notes]),
    }),
  });
}

// ---------------------------------------------------------------------------------------------
// Cursor resolution
// ---------------------------------------------------------------------------------------------

function resolveCursorFor(query, source, orderKeys, dslHash, ctx) {
  const upper = ctx.snapshotUpper ?? defaultUpper(query, source, ctx);
  if (!query.cursor) return Object.freeze({ position: null, snapshotUpper: upper });
  const payload = decodeCursor(query.cursor, { tenant: ctx.tenant, dslHash, order: orderKeys }, { key: ctx.cursorKey });
  return Object.freeze({ position: payload.pos, snapshotUpper: payload.upper });
}

/**
 * The snapshot upper bound: every page after the first carries `<time column> <= upper`.
 *
 * For a list it is the moment page one is served, and rows appended after it are announced by
 * `newer_events_exist` rather than shifting page boundaries. An aggregate is not paged, so the
 * window's own upper bound is used.
 */
function defaultUpper(query, source, ctx) {
  if (source.kind === 'list' && source.time) return toDate(ctx.now ?? new Date()).toISOString();
  return query.window ? query.window.to : new Date().toISOString();
}

/** The cursor that resumes a list after `row`. */
function resumeAfter(orderKeys, dslHash, ctx, upper) {
  return (row) => {
    const position = {};
    for (const key of orderKeys) {
      const value = row[key];
      position[key] = value instanceof Date ? value.toISOString() : value;
    }
    return encodeCursor({ tenant: ctx.tenant, dslHash, order: orderKeys, upper, position }, { key: ctx.cursorKey });
  };
}

// ---------------------------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------------------------

/**
 * Run a plan inside one tenant-scoped transaction and return the response envelope.
 *
 * Nothing is returned before COMMIT, so a cancelled read has served nothing and a read whose audit
 * row cannot be committed serves no rows.
 *
 * @param {Plan} planResult
 * @param {object} ctx
 * @param {{query: Function}} ctx.client  a pooled connection
 * @param {string} ctx.tenant
 * @param {Buffer|string} [ctx.cursorKey]
 * @param {Date} [ctx.now]
 * @returns {Promise<object>} the response envelope
 */
export async function executePlan(planResult, ctx = {}) {
  const { client } = ctx;
  if (!client || typeof client.query !== 'function') {
    throw new Error('executePlan needs a client with query(text, params).');
  }
  const now = toDate(ctx.now ?? new Date());
  const results = new Map();
  let current = null;
  const run = async (statement) => {
    current = statement.id;
    results.set(statement.id, await client.query(statement.text, statement.params));
  };

  try {
    await inTransaction(client, { tenant: ctx.tenant, statementTimeoutMs: planResult.meta.statement_timeout_ms }, async () => {
      for (const statement of planResult.statements) await run(statement);
      current = 'commit';
    });
  } catch (error) {
    const auditFailed = current === 'audit_insert' || (current === 'commit' && results.has('audit_insert'));
    throw fromDatabaseError(error, { auditFailed });
  }
  return assemble(planResult, results, now, ctx);
}

/** Turn executed statements into the response envelope. */
function assemble(planResult, results, now, ctx) {
  const readRows = results.get('read')?.rows ?? [];
  const meta = { ...planResult.meta };

  if (planResult.mode === 'single') {
    const detailRows = results.get('submission_detail')?.rows ?? [];
    const receipts = results.get('erasure_evidence')?.rows ?? [];
    const coverage = coverageBlock({ window: null, row: results.get('coverage')?.rows?.[0] ?? null });
    const freshness = Object.freeze({
      source: 'ingest.submission',
      last_run_at: null,
      state: 'fresh',
      note: 'Read directly from the event table: an event is visible as soon as its ingest transaction commits.',
    });
    const auditRow = results.get('audit_insert')?.rows?.[0] ?? null;
    if (detailRows.length === 0) {
      const verdict = resolveMissingRecord({
        found: false,
        receivedAtHint: ctx.receivedAtHint ?? planResult.meta.received_at_hint ?? null,
        receipts,
      });
      if (verdict.result_state === 'no_longer_available') {
        return Object.freeze({
          api_version: API_VERSION,
          query_version: QUERY_VERSION,
          result_state: 'no_longer_available',
          error: Object.freeze({
            code: verdict.reason,
            message: 'The record existed and has been destroyed; the receipt is attached.',
            receipt: verdict.receipt,
          }),
          freshness,
          coverage,
          ...(auditRow ? { audit: auditBlock(auditRow) } : {}),
        });
      }
      return Object.freeze({
        api_version: API_VERSION,
        query_version: QUERY_VERSION,
        result_state: 'not_found',
        error: Object.freeze({ code: 'not_found', message: 'No such record.', detail: verdict.detail }),
        freshness,
        coverage,
        ...(auditRow ? { audit: auditBlock(auditRow) } : {}),
      });
    }
    const contentState = detailRows[0].content_state;
    const data = Object.freeze(detailRows.map((row) => Object.freeze({ ...row })));
    return buildEnvelope({
      resultState: contentState === 'shredded' && detailRows.length === 1 ? 'no_longer_available' : 'ok',
      data,
      freshness,
      coverage,
      ...(auditRow ? { audit: auditBlock(auditRow) } : {}),
      meta: Object.freeze({ ...meta, content_state: contentState }),
    });
  }

  const orderKeys = planResult.meta.order.map((t) => t.by);
  const upper = planResult.meta.snapshot_upper_bound;

  // The audit page's hash links are verified before anything is returned, and a mismatch is an
  // integrity alert rather than a list that looks fine.
  if (planResult.source.id === 'ops.audit') {
    const verdict = verifyAuditPage(readRows);
    if (!verdict.ok) {
      return Object.freeze({
        api_version: API_VERSION,
        query_version: QUERY_VERSION,
        result_state: 'audit_chain_broken',
        error: Object.freeze({
          code: verdict.broken[0]?.reason ?? REASON.CHAIN_MISMATCH,
          message: 'The audit page\'s hash links do not verify; this is an integrity alert, not a list.',
          detail: Object.freeze({ broken: verdict.broken }),
        }),
      });
    }
  }

  let rows = [...readRows];
  let page = null;
  const limit = planResult.query.limit;
  if (planResult.source.kind === 'list') {
    const newerEventsExist = Boolean(results.get('newer_events')?.rows?.[0]?.newer_events_exist);
    const resume = resumeAfter(orderKeys, planResult.dsl_hash, ctx, upper);
    const paged = paginate({ rows, limit: limit ?? rows.length, resume, snapshotUpper: upper, newerEventsExist });
    rows = [...paged.rows];
    page = paged.page;
  } else if (limit !== null) {
    // An aggregate is not paged. The statement fetched one row beyond the limit so the response can
    // say whether the limit cut the result short.
    meta.truncated = rows.length > limit;
    rows = rows.slice(0, limit);
  }

  // Hidden columns (`__ord_*`) served the ordering and never reach the wire.
  const published = rows.map((row) => Object.freeze(Object.fromEntries(Object.entries(row).filter(([key]) => !key.startsWith('__')))));

  const freshness = freshnessBlock({
    aggregate: SOURCE_WATERMARK[planResult.source.id] ?? planResult.source.id,
    bucketSize: planResult.meta.native_bucket_size,
    row: results.get('freshness')?.rows?.[0] ?? null,
    reason: planResult.source.id === 'mart.agg_org_period' ? REASON.DIRECTORY_NOT_SYNCED : undefined,
    now: now.getTime(),
  });
  const coverage = coverageBlock({
    window: planResult.query.window ?? null,
    row: results.get('coverage')?.rows?.[0] ?? null,
  });

  const resultState = results.has('audit_insert') || planResult.source.id !== 'ops.audit'
    ? resultStateFor({
      rowCount: published.length,
      freshness,
      coverage,
    })
    : 'ok';

  const extras = {};
  for (const id of SIDE_READ_IDS) {
    const result = results.get(id);
    if (result && id !== 'coverage' && id !== 'freshness') {
      extras[id] = Object.freeze((result.rows ?? []).map((r) => Object.freeze({ ...r })));
    }
  }

  const auditRow = results.get('audit_insert')?.rows?.[0] ?? null;
  return buildEnvelope({
    resultState,
    data: Object.freeze(published),
    page,
    freshness,
    coverage,
    ...(auditRow ? { audit: auditBlock(auditRow) } : {}),
    meta: Object.freeze({
      ...meta,
      ...(Object.keys(extras).length > 0 ? { extras: Object.freeze(extras) } : {}),
    }),
  });
}

function auditBlock(row) {
  return Object.freeze({
    entry_id: row.audit_seq === undefined ? null : String(row.audit_seq),
    written_at: row.occurred_at instanceof Date ? row.occurred_at.toISOString() : row.occurred_at ?? null,
  });
}

// ---------------------------------------------------------------------------------------------

function isTemplateRequest(request) {
  return Boolean(request) && typeof request === 'object' && typeof request.template === 'string';
}

/**
 * A template request is closed too: a request carrying both a template and `{"sql": "..."}` is
 * refused, not answered with the SQL key dropped. Only the three template keys exist, and the
 * prohibited ones are named rather than merely unknown.
 */
function assertTemplateRequestKeys(request) {
  const allowed = ['query_version', 'template', 'params'];
  for (const key of Object.keys(request)) {
    if (allowed.includes(key)) continue;
    const prohibited = PROHIBITED_FIELDS[key];
    if (prohibited === REASON.TENANT_IN_REQUEST) {
      throw unsupported(REASON.TENANT_IN_REQUEST, 'The tenant comes from the authenticated session and never from the request; a request carrying a tenant identifier is rejected.', { key });
    }
    if (prohibited === REASON.TEXT_PREDICATE_IN_REQUEST) {
      throw unsupported(REASON.TEXT_PREDICATE_IN_REQUEST, `"${key}" is a content-search field. /v1/query has no text predicate; use POST /v1/content-search.`, { key, endpoint: '/v1/content-search' });
    }
    if (prohibited === REASON.PROHIBITED_FIELD) {
      throw unsupported(REASON.PROHIBITED_FIELD, `"${key}" is not part of the DSL; the DSL has no SQL, expression or ordering-text field.`, { key });
    }
    throw unsupported(REASON.UNKNOWN_KEY, `Unknown key "${key}" in a template request.`, { key, known_keys: allowed });
  }
  if (request.query_version === undefined) {
    throw unsupported(REASON.UNSUPPORTED_QUERY_VERSION, 'query_version is required; the DSL is a versioned contract.', { served: [QUERY_VERSION] });
  }
  if (request.query_version !== QUERY_VERSION) {
    throw unsupported(REASON.UNSUPPORTED_QUERY_VERSION, `Unsupported query_version "${String(request.query_version)}"; this service serves version ${QUERY_VERSION} and never silently downgrades.`, { requested: request.query_version, served: [QUERY_VERSION] });
  }
}

/** The hash of the normalised query a cursor binds to. */
export function hashOf(value) {
  return createHash('sha256').update(canonicalJson(value), 'utf8').digest('hex');
}

function toDate(value) {
  return value instanceof Date ? value : new Date(value);
}

/**
 * The coverage window for a read with no window of its own: the current UTC day, half-open
 * [day start, next day start). Coverage is a per-day fact and the device read is current state, so
 * the question is "what is happening today".
 */
function currentDayWindow(now) {
  const start = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate()));
  return { from: start.toISOString(), to: new Date(start.getTime() + 86_400_000).toISOString() };
}
