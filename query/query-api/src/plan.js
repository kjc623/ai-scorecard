// plan.js — the whole read path, in the order the document fixes it.
//
//   request (template or DSL document)
//     -> expand template            (templates.js)
//     -> validate                   (validate.js: closed document, typed rejection)
//     -> guard                      (guard.js: cost refused before execution)
//     -> resolve cursor             (cursor.js: bound to tenant, query and ordering)
//     -> compile                    (compile.js: parameterised SQL)
//     -> audit decision             (audit.js: §5, fail closed)
//     -> execute in one transaction (executePlan below)
//     -> suppress                   (suppress.js: §6)
//     -> envelope                   (envelope.js: §2.3, §13)
//
// `plan()` is pure and synchronous: it is what a test asserts against, and what a hostile-input
// corpus is run through. `executePlan()` needs a database client, which is injected because this
// package carries no PostgreSQL driver (offline host, zero dependencies).

import { createHash } from 'node:crypto';
import { API_VERSION, K, QUERY_VERSION, SOURCE_WATERMARK } from './registry.js';
import { PROHIBITED_FIELDS, REASON, QueryError, auditUnavailable, unsupported } from './errors.js';
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
import { anyCellBelowK, applySuppression } from './suppress.js';
import {
  buildEnvelope,
  coverageBlock,
  freshnessBlock,
  resultStateFor,
  suppressionBlock,
} from './envelope.js';
import {
  coverageStatement,
  erasureEvidenceStatement,
  freshnessStatement,
  newerEventsStatement,
  resolveMissingRecord,
  submissionDetailStatement,
} from './blocks.js';
import {
  decodeCursor,
  encodeCursor,
  isSubjectBearingOrder,
  paginate,
} from './cursor.js';

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
]);

/**
 * Build the statement plan for one request. Throws QueryError for every rejection §13 names.
 *
 * @param {object} request   a DSL document, or {template, params}
 * @param {object} [ctx]
 * @param {string} [ctx.tenant]        the authenticated session's tenant (never from the body)
 * @param {string} [ctx.actorId]
 * @param {string} [ctx.caseReference]
 * @param {Date|string} [ctx.now]
 * @param {string} [ctx.snapshotUpper]
 * @param {object} [ctx.cursorStore]   the server-side resume store (cursor.js)
 * @param {Buffer|string} [ctx.cursorKey]
 * @returns {Plan}
 */
export function plan(request, ctx = {}) {
  const now = toDate(ctx.now ?? new Date());
  const expansion = isTemplateRequest(request) ? expandTemplate(request) : null;
  if (expansion) assertTemplateRequestKeys(request);
  const document = expansion ? expansion.document : request;

  if (expansion && expansion.document === null) {
    // A single-record template (Q9) has no aggregate shape: it is a detail read with its own
    // statements, and its request is a record reference rather than a query document.
    return planSingle(expansion, ctx, now);
  }

  const validated = validate(document);
  const guarded = guard(validated);
  const query = guarded.query;
  const source = validated.source;
  const klass = validated.klass;

  const orderKeys = effectiveOrderKeys(query, source);
  const dslHash = hashOf(query);
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
      subjectRef: subjectRefOf(query),
      detail: auditDetail({ query, source }, { coarsened: guarded.coarsened }),
    })]
    : [];

  const sideReads = [];
  const watermark = SOURCE_WATERMARK[source.id];
  if (watermark) {
    sideReads.push(freshnessStatement(watermark, compiled.meta.native_bucket_size ?? 'day'));
  }
  sideReads.push(coverageStatement(query.window ?? { from: dayAgoIso(now), to: now.toISOString() }));
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
      cursor_mode: resolvedCursor.mode,
      coarsened: guarded.coarsened,
      guard: Object.freeze({
        buckets: guarded.buckets,
        estimated_cells: guarded.estimatedCells,
        bounded_cells: guarded.boundedCells,
        paged: guarded.paged,
        estimated_bytes: guarded.estimatedBytes,
      }),
      notes: Object.freeze([...(expansion?.notes ?? []), ...guarded.notes]),
      k: source.kSuppression ? K : null,
    }),
  });
}

function planSingle(expansion, ctx, now) {
  const statements = [];
  for (const statement of expansion.statements) {
    const built = statement();
    if (built) statements.push(built);
  }
  // A missing record is ambiguous between "never existed" and "existed and was purged", and §13
  // resolves it from the window the record would have been received in. The caller may name that
  // window with `received_at_hint`; without it the response says so rather than guessing.
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
    subjectRef: ctx.subjectRef ?? null,
    detail: Object.freeze({ source: 'ingest.submission', kind: 'single_record', template: expansion.name }),
  });
  const sideReads = [
    coverageStatement({ from: dayAgoIso(now), to: now.toISOString() }),
  ];
  return Object.freeze({
    ok: true,
    mode: 'single',
    template: { name: expansion.name, question: expansion.question, title: expansion.title },
    query: null,
    source: null,
    dsl_hash: hashOf({ template: expansion.name, params: expansion.params ?? null }),
    compiled: null,
    statements: Object.freeze([auditStatementBuilt, ...statements, ...sideReads]),
    audit: Object.freeze({ decision, plan: auditPlan(decision, true), subjectRef: ctx.subjectRef ?? null, action: decision.action, object_type: decision.object_type }),
    meta: Object.freeze({
      api_version: API_VERSION,
      query_version: QUERY_VERSION,
      single_record: true,
      received_at_hint: hint,
      notes: Object.freeze([...expansion.notes]),
    }),
  });
}

// ---------------------------------------------------------------------------------------------
// Cursor resolution
// ---------------------------------------------------------------------------------------------

function resolveCursorFor(query, source, orderKeys, dslHash, ctx) {
  const subjectBearing = isSubjectBearingOrder(orderKeys);
  const fallbackUpper = ctx.snapshotUpper ?? defaultUpper(query, source, ctx);
  if (!query.cursor) {
    return Object.freeze({ position: null, snapshotUpper: fallbackUpper, mode: subjectBearing ? 'server_side' : 'self_contained' });
  }

  // §7.2: a cursor whose ordering key contains a subject reference is never a client-held
  // encoding of that reference. A self-contained token here is refused, not decoded.
  const looksSelfContained = query.cursor.includes('.');
  if (subjectBearing) {
    if (looksSelfContained) {
      throw unsupported(REASON.CURSOR_REQUIRES_TOTAL_ORDER, 'A cursor over a subject-bearing ordering key must be an opaque server-side id.', {});
    }
    if (!ctx.cursorStore) {
      throw unsupported(REASON.CURSOR_UNKNOWN, 'No server-side cursor store is configured.', {});
    }
    const resume = ctx.cursorStore.take(query.cursor);
    if (resume.dsl_hash !== dslHash) {
      throw unsupported(REASON.CURSOR_MISMATCH, 'Cursor belongs to a different query; restart from page one.', {});
    }
    if (ctx.tenant !== undefined && resume.tenant !== ctx.tenant) {
      throw unsupported(REASON.CURSOR_TENANT_MISMATCH, 'Cursor was issued for a different session.', {});
    }
    return Object.freeze({ position: resume.position, snapshotUpper: resume.snapshotUpper, mode: 'server_side' });
  }

  const payload = decodeCursor(query.cursor, {
    tenant: ctx.tenant,
    dslHash,
    order: orderKeys,
  }, { key: ctx.cursorKey });
  return Object.freeze({ position: payload.pos, snapshotUpper: payload.upper, mode: 'self_contained' });
}

/**
 * §7.3: "snapshot_upper_bound is the maximum received_at visible when the first page was served;
 * every later page carries WHERE received_at <= :upper."
 *
 * For a list that bound is the moment page one is served — rows appended after it are announced
 * by `newer_events_exist` rather than shifting the boundary. For an aggregate there is nothing to
 * shift (the read re-derives its buckets), so the window's own upper bound is the honest answer
 * and it is deterministic, which a cursor needs.
 */
function defaultUpper(query, source, ctx) {
  if (source.kind === 'list' && source.time) return (ctx.now instanceof Date ? ctx.now : new Date(ctx.now ?? Date.now())).toISOString();
  return query.window ? query.window.to : new Date().toISOString();
}

/**
 * The resume token for the row a page ended on. Subject-bearing orderings go through the store;
 * everything else is signed and handed to the client.
 */
export function makeResume(query, source, orderKeys, dslHash, ctx, upper) {
  return (row) => {
    const position = positionFrom(row, orderKeys);
    if (isSubjectBearingOrder(orderKeys)) {
      if (!ctx.cursorStore) {
        throw unsupported(REASON.CURSOR_UNKNOWN, 'No server-side cursor store is configured.', {});
      }
      return ctx.cursorStore.put({ dsl_hash: dslHash, tenant: ctx.tenant, position, snapshotUpper: upper });
    }
    return encodeCursor({
      tenant: ctx.tenant,
      dslHash,
      order: orderKeys,
      upper,
      position,
    }, { key: ctx.cursorKey });
  };
}

function positionFrom(row, orderKeys) {
  const out = {};
  for (const key of orderKeys) {
    const value = row[key];
    out[key] = value instanceof Date ? value.toISOString() : value;
  }
  return out;
}

// ---------------------------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------------------------

/**
 * Run a plan inside one transaction and return the §2.3 envelope.
 *
 * `client` is injected and must provide `query(text, params)`, `begin()`, `commit()` and
 * `rollback()`. Nothing is returned to the caller before COMMIT: §5.1's "nothing is streamed"
 * is what makes cancellation safe and what makes fail-closed possible.
 *
 * @param {Plan} planResult
 * @param {object} ctx
 * @param {object} ctx.client
 * @param {object} [ctx.cursorStore]
 * @param {Buffer|string} [ctx.cursorKey]
 * @param {string} [ctx.tenant]
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
  const statements = [...planResult.statements];

  // A post-read audit (the small-cell trigger of §5.2) is decided from the read's own
  // `__k_subjects` column and inserted before anything is served.
  if (planResult.audit.plan.phase === 'post_read') {
    const index = statements.findIndex((s) => s.id === 'read');
    const readStatement = statements[index];
    try {
      await client.begin();
      const readResult = await client.query(readStatement.text, readStatement.params);
      results.set('read', readResult);
      if (anyCellBelowK(readResult.rows ?? [])) {
        const statement = auditStatement(planResult.audit.decision, {
          actorId: ctx.actorId ?? 'unknown',
          caseReference: ctx.caseReference ?? null,
          subjectRef: planResult.audit.subjectRef,
          detail: auditDetail({ query: planResult.query, source: planResult.source }, { rows: (readResult.rows ?? []).length }),
        });
        const auditResult = await client.query(statement.text, statement.params);
        results.set('audit_insert', auditResult);
      }
      await runSideReads(statements.filter((s) => s.id !== 'read'), client, results);
      await client.commit();
    } catch (error) {
      await safeRollback(client);
      throw asQueryError(error);
    }
    return assemble(planResult, results, now, ctx);
  }

  try {
    await client.begin();
    for (const statement of statements) {
      // eslint-disable-next-line no-await-in-loop
      const result = await client.query(statement.text, statement.params);
      results.set(statement.id, result);
    }
    await client.commit();
  } catch (error) {
    await safeRollback(client);
    throw asQueryError(error);
  }
  return assemble(planResult, results, now, ctx);
}

async function runSideReads(statements, client, results) {
  for (const statement of statements) {
    // eslint-disable-next-line no-await-in-loop
    const result = await client.query(statement.text, statement.params);
    results.set(statement.id, result);
  }
}

async function safeRollback(client) {
  try {
    if (typeof client.rollback === 'function') await client.rollback();
  } catch {
    // A rollback failure must not mask the original error.
  }
}

function asQueryError(error) {
  if (error instanceof QueryError) return error;
  return auditUnavailable(REASON.AUDIT_WRITE_FAILED, 'The read could not be completed inside one transaction, so nothing was served.', {
    cause: String(error?.message ?? error).slice(0, 200),
  });
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
      note: 'read directly from the event table: an event is visible as soon as its ingest transaction commits (brief §8).',
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

  // §3.10: the audit page's hash links are verified before anything is returned, and a mismatch
  // is an integrity alert rather than a list that looks fine.
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

  // Hidden columns (`__k_subjects`, `__ord_*`) are stripped by applySuppression, after it has
  // used them: the k decision needs the distinct-subject count, and stripping first would make
  // every cell look wide.
  let rows = [...readRows];
  let page = null;
  if (planResult.source.kind === 'list') {
    const limit = planResult.query.limit ?? rows.length;
    const newerEventsExist = Boolean(results.get('newer_events')?.rows?.[0]?.newer_events_exist);
    const resume = makeResume(planResult.query, planResult.source, orderKeys, planResult.dsl_hash, ctx, upper);
    const paged = paginate({ rows, limit, resume, snapshotUpper: upper, newerEventsExist });
    rows = [...paged.rows];
    page = paged.page;
  }

  const suppressionResult = applySuppression(rows, {
    k: K,
    measures: planResult.query.measures,
    subjectScoped: planResult.query.filters.some((f) => f.field === 'subject' && (f.op === 'eq' || f.op === 'in')),
    groupKeys: [
      ...(planResult.query.bucket ? ['bucket'] : []),
      ...planResult.query.dimensions,
    ],
  });

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
      rowCount: suppressionResult.rows.length,
      freshness,
      coverage,
      suppressedCells: suppressionResult.suppressedCells,
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
    data: Object.freeze([...suppressionResult.rows]),
    page,
    freshness,
    coverage,
    suppression: suppressionBlock({
      k: planResult.source.kSuppression ? K : null,
      suppressedCells: suppressionResult.suppressedCells,
      totalSuppressed: suppressionResult.totalSuppressed,
      subjectCountBasis: planResult.meta.subject_count_basis,
      notes: suppressionResult.notes,
    }),
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

/** Hidden columns are for the executor, not the wire. */
function stripPrivate(row) {
  const out = {};
  for (const [key, value] of Object.entries(row)) {
    if (key.startsWith('__')) continue;
    out[key] = value;
  }
  return out;
}

export { stripPrivate };

// ---------------------------------------------------------------------------------------------

function isTemplateRequest(request) {
  return Boolean(request) && typeof request === 'object' && typeof request.template === 'string';
}

/**
 * A template request is validated for closure too. Without this, a request carrying both a
 * template and `{"sql": "..."}` would have the SQL key dropped by the expansion and answered —
 * which is exactly the "ignore what you do not recognise" failure §2.4 forbids. Only the three
 * template keys exist, and the prohibited ones are named rather than merely unknown.
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

/** §7.2: the cursor binds to `dsl_hash`, the hash of the normalised query. */
export function hashOf(value) {
  return createHash('sha256').update(canonicalJson(value), 'utf8').digest('hex');
}

function toDate(value) {
  return value instanceof Date ? value : new Date(value);
}

function dayAgoIso(now) {
  return new Date(now.getTime() - 86_400_000).toISOString();
}

export { submissionDetailStatement, erasureEvidenceStatement };
