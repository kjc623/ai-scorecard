// compile.js — DSL -> parameterised SQL.
//
// No part of a client-supplied value is ever concatenated into SQL text: a value is bound, and a
// dimension name that is not in the enumerated vocabulary is rejected rather than escaped.
//
// Read the code below as the argument for that property. Every character this module adds to
// `text` comes from one of three places:
//
//   1. a string literal written in this file (keywords, parentheses, operators);
//   2. `registry.js`'s `sql` / `column` / `orderSql` fields, which are frozen constants;
//   3. `$n`, a placeholder, for every value and for nothing else.
//
// There is no branch anywhere in this module that reads a request-derived string into the
// statement. `bind()` is the only path a value can take, and it returns a placeholder. The
// hostile-input test asserts the consequence directly: two documents that differ only in their
// values compile to byte-identical SQL.

import { BUCKET_TRUNC_SQL, NATIVE_BUCKETS, REDUCTION_SOURCE_BUCKET } from './registry.js';
import { REASON, unsupported } from './errors.js';

/** `${type}` -> the SQL cast appended to a placeholder. */
const CAST = Object.freeze({
  text: '::text',
  uuid: '::uuid',
  timestamp: '::timestamptz',
  date: '::date',
  number: '::numeric',
  boolean: '::boolean',
});

const ARRAY_CAST = Object.freeze({
  text: '::text[]',
  uuid: '::uuid[]',
  timestamp: '::timestamptz[]',
  date: '::date[]',
  number: '::numeric[]',
  boolean: '::boolean[]',
});

/**
 * @typedef {object} CompiledStatement
 * @property {string} text                 SQL with $n placeholders and no request-derived text.
 * @property {ReadonlyArray<unknown>} params Values, in placeholder order.
 * @property {object} meta                 Everything the executor, guard and envelope need.
 */

/**
 * Compile a normalised query.
 *
 * @param {{query: object, source: object, klass: object}} validated
 * @param {object} [opts]
 * @param {object[]} [opts.cursorPosition] Position values from a decoded cursor, if any
 * @returns {CompiledStatement}
 */
export function compile(validated, opts = {}) {
  const { query, source } = validated;
  const params = [];
  /** @type {(value: unknown) => string} */
  const bind = (value) => {
    params.push(value);
    return `$${params.length}`;
  };

  const usedFields = new Set([
    ...query.dimensions,
    ...query.filters.map((f) => f.field),
    ...query.order.map((o) => o.by),
  ]);
  if (opts.cursorPosition) for (const key of Object.keys(opts.cursorPosition)) usedFields.add(key);

  const select = [];
  const groupBy = [];
  const meta = {
    source: source.id,
    kind: source.kind,
    applied_bucket: query.bucket,
    native_bucket_size: null,
    reduced_from: null,
    dimensions: [...query.dimensions],
    measures: [...query.measures],
    measure_semantics: {},
    order: [],
    limit: query.limit,
    probe_row: false,
    rollup: query.rollup,
    has_cursor: Boolean(query.cursor),
    query_class: source.costClass,
    statement_timeout_ms: validated.klass.statementTimeoutMs,
    required_indexes: [...source.indexes],
    warnings: [...(source.warnings ?? [])],
    joins_used: [],
  };

  // -------------------------------------------------------------------------------------------
  // SELECT / GROUP BY
  // -------------------------------------------------------------------------------------------
  const orderTerms = effectiveOrder(query, source, meta);

  if (query.bucket) {
    const { startColumn, sizeColumn } = source.bucket;
    if (query.bucket === 'hour' || query.bucket === 'day') {
      meta.native_bucket_size = query.bucket;
      select.push(`${startColumn} AS bucket`);
      groupBy.push(startColumn);
    } else {
      // Week and month are read-side reductions over day rows: still aggregate-only,
      // and the applied bucket is named in freshness.
      meta.native_bucket_size = REDUCTION_SOURCE_BUCKET;
      meta.reduced_from = REDUCTION_SOURCE_BUCKET;
      select.push(`date_trunc(${BUCKET_TRUNC_SQL[query.bucket]}, ${startColumn}) AS bucket`);
      groupBy.push(`date_trunc(${BUCKET_TRUNC_SQL[query.bucket]}, ${startColumn})`);
    }
    void sizeColumn;
  }

  for (const name of query.dimensions) {
    const dimension = source.dimensions[name];
    select.push(`${dimension.sql} AS ${quoteIdent(name)}`);
    groupBy.push(dimension.sql);
  }

  // A bounded list selects its own frozen column list — dimensions on a list source are filters
  // and order keys, not a grouping, so they are not derived from `query.dimensions`.
  for (const entry of source.listSelect ?? []) select.push(entry);

  // An ordering key on a nullable column is ordered through a sentinel expression (see
  // registry.js), and that expression is not the value the client receives. It is added to the
  // select list under a private name so the keyset cursor carries the *ordered* value: binding
  // the cursor to the displayed NULL would make every later comparison NULL-false and silently
  // drop the rest of the page.
  for (const t of orderTerms) {
    if (t.expr === t.selectExpr) continue;
    select.push(`${t.expr} AS ${quoteIdent(`__ord_${t.by}`)}`);
    // A list is not grouped; only an aggregate needs the ordering expression in its GROUP BY,
    // and only when it actually groups (a single-row aggregate must stay single-row).
    if (source.kind === 'aggregate' && groupBy.length > 0 && !groupBy.includes(t.expr)) groupBy.push(t.expr);
  }

  const collapsing = collapsesGrain(query, source);
  for (const name of query.measures) {
    const measure = source.measures[name];
    select.push(`${measure.agg.toUpperCase()}(${measure.column}) AS ${quoteIdent(name)}`);
    // `users` is a distinct-subject count, not a counter. When the cell may combine
    // several source rows it is served as `max(...)`, a lower bound that never overstates —
    // and the response says which of the two it is, so a rendering cannot present a bound as
    // an exact count.
    if (measure.semantics === 'distinct_lower_bound') {
      meta.measure_semantics[name] = collapsing ? 'distinct_lower_bound' : 'exact';
    } else {
      meta.measure_semantics[name] = measure.semantics;
    }
  }

  for (const entry of source.extraSelect ?? []) {
    // A derived display column belongs in the SELECT only when its grouping key is actually
    // grouped: `tool_name` is a function of `tool_fingerprint`, so a query that does not GROUP BY
    // tool cannot select it at all. A plain string is unconditional; an object with
    // `whenDimensions` is emitted only when every named dimension is grouped.
    if (typeof entry === 'string') {
      select.push(entry);
      continue;
    }
    if (entry.whenDimensions && !entry.whenDimensions.every((d) => query.dimensions.includes(d))) continue;
    select.push(entry.sql);
  }

  // -------------------------------------------------------------------------------------------
  // FROM and the lazy directory join
  // -------------------------------------------------------------------------------------------
  const fromParts = [source.from];
  for (const join of source.joins ?? []) {
    if (join.when.some((field) => usedFields.has(field))) {
      fromParts.push(join.sql);
      meta.joins_used.push(join.id);
      for (const entry of join.select ?? []) select.push(entry);
    }
  }

  // -------------------------------------------------------------------------------------------
  // WHERE
  // -------------------------------------------------------------------------------------------
  const where = [];
  // Defence in depth: row-level security already scopes every table to ops.current_tenant(),
  // and the predicate is repeated here so that the statement is tenant-scoped on its own face.
  // The tenant is never a parameter: it cannot be supplied by a caller at all.
  where.push(`${source.tenantColumn} = ops.current_tenant()`);

  const timeColumn = source.time ? source.time.sql : source.bucket?.startColumn ?? null;
  // A `date` window (ops.coverage_snapshot.snapshot_day) is compared as a date, not cast up to
  // timestamptz: the predicate is the covering index and must stay in its own type.
  const windowCast = !source.time && source.bucket ? '::timestamptz' : (source.time?.type === 'date' ? '::date' : '::timestamptz');
  if (query.window) {
    where.push(`${timeColumn} >= ${bind(query.window.from)}${windowCast}`);
    where.push(`${timeColumn} < ${bind(query.window.to)}${windowCast}`);
    if (opts.snapshotUpper) {
      // The window is frozen at the first page. Rows are appended at the head of a DESC
      // ordering, so without this an insert after page one would shift boundaries and a row
      // would be seen twice or skipped.
      where.push(`${timeColumn} <= ${bind(opts.snapshotUpper)}${windowCast}`);
    }
  } else if (opts.snapshotUpper && timeColumn) {
    where.push(`${timeColumn} <= ${bind(opts.snapshotUpper)}${windowCast}`);
  }

  if (source.bucket && meta.native_bucket_size) {
    // Pinning bucket_size is not decoration: without it a window total would add hour rows to
    // day rows and double-count every event.
    where.push(`${source.bucket.sizeColumn} = ${bind(meta.native_bucket_size)}::text`);
  }

  for (const filter of query.filters) {
    where.push(compileFilter(filter, source, bind));
  }

  const cursorPredicate = compileCursorPredicate(validated, opts, bind, meta, orderTerms);
  if (cursorPredicate) where.push(cursorPredicate);

  // -------------------------------------------------------------------------------------------
  // Assemble
  // -------------------------------------------------------------------------------------------
  let text = `SELECT ${select.join(',\n       ')}\n  FROM ${fromParts.join('\n  ')}\n WHERE ${where.join('\n   AND ')}`;
  if (query.rollup) {
    // A published total over the same dimension, which complementary suppression needs.
    const sets = groupBy.length > 0 ? `(${groupBy.join(', ')}), ()` : '()';
    text += `\n GROUP BY GROUPING SETS (${sets})`;
  } else if (groupBy.length > 0) {
    text += `\n GROUP BY ${groupBy.join(', ')}`;
  }
  if (orderTerms.length > 0) {
    text += `\n ORDER BY ${orderTerms.map((t) => t.sql).join(', ')}`;
  }
  if (query.limit !== null) {
    // Fetch one row beyond the page so the executor can answer "is there a next page?" from
    // the page itself, rather than from a second COUNT over the same predicate.
    meta.probe_row = true;
    text += `\n LIMIT ${bind(query.limit + 1)}::int`;
  }

  return Object.freeze({
    text,
    params: Object.freeze([...params]),
    meta: Object.freeze({ ...meta, order: Object.freeze(orderTerms.map((t) => ({ by: t.by, dir: t.dir, key: t.key }))) }),
  });
}

/**
 * The ordering key, as column names, without compiling anything. The cursor needs it before the
 * statement exists, because a cursor is bound to the exact ordering it was issued under.
 *
 * @returns {ReadonlyArray<string>}
 */
export function effectiveOrderKeys(query, source) {
  return Object.freeze(effectiveOrder(query, source).map((t) => t.by));
}

/**
 * The effective total ordering. Bucket leads (descending) when the read is bucketed, then the
 * caller's ranking, then every remaining grouping key ascending.
 */
function effectiveOrder(query, source, meta) {
  const terms = [];
  const seen = new Set();
  const add = (by, dir) => {
    if (seen.has(by)) return;
    seen.add(by);
    terms.push(term(by, dir, query, source));
  };
  if (query.bucket) add('bucket', 'desc');
  for (const o of query.order) add(o.by, o.dir);
  for (const name of query.dimensions) add(name, 'asc');
  if (!query.bucket && query.dimensions.length === 0 && query.kind === 'list') {
    // A list with no grouping: its declared total key is the ordering, not a tie-break.
    for (const o of source.order) add(o.dim, o.dir);
  }
  if (meta) meta.ordering_is_list_key = query.kind === 'list' && query.dimensions.length === 0;
  return terms;
}

function term(by, dir, query, source) {
  let expr;
  let selectExpr;
  const key = by;
  if (by === 'bucket') {
    expr = query.bucket === 'hour' || query.bucket === 'day'
      ? source.bucket.startColumn
      : `date_trunc(${BUCKET_TRUNC_SQL[query.bucket]}, ${source.bucket.startColumn})`;
    selectExpr = expr;
  } else if (source.dimensions[by]) {
    expr = source.dimensions[by].orderSql;
    selectExpr = source.dimensions[by].sql;
  } else if (source.measures[by]) {
    const measure = source.measures[by];
    expr = `${measure.agg.toUpperCase()}(${measure.column})`;
    selectExpr = expr;
  } else if (source.columns?.[by]) {
    expr = source.columns[by].orderSql;
    selectExpr = source.columns[by].sql;
  } else {
    // Unreachable: validate() has already proved `by` is a returned column. Throwing rather
    // than defaulting keeps a compiler bug from becoming a silently different question.
    throw unsupported(REASON.UNKNOWN_MEASURE, `Internal: no order expression for "${by}".`, { by });
  }
  return {
    by,
    dir,
    key,
    expr,
    selectExpr,
    sql: `${expr} ${dir.toUpperCase()} NULLS LAST`,
  };
}

/**
 * Keyset predicate over a mixed-direction total order. A row-value comparison cannot express
 * mixed ASC/DESC, so the standard expanded form is emitted:
 *
 *   (k1 < v1) OR (k1 = v1 AND k2 > v2) OR (k1 = v1 AND k2 = v2 AND k3 > v3) …
 *
 * Every v is a bound parameter, every k comes from the registry.
 */
function compileCursorPredicate(validated, opts, bind, meta, terms) {
  const position = opts.cursorPosition;
  if (!position) return null;
  const { query, source } = validated;
  const parts = [];
  const equalitySoFar = [];
  for (const t of terms) {
    const value = position[t.by];
    if (value === undefined) {
      throw unsupported(REASON.CURSOR_REQUIRES_TOTAL_ORDER, `The cursor does not carry a position for ordering key "${t.by}".`, { key: t.by });
    }
    const type = typeOfOrderKey(t.by, source, query);
    const placeholder = `${bind(value)}${CAST[type]}`;
    const comparison = t.dir === 'desc' ? '<' : '>';
    const clause = [...equalitySoFar.map((e) => `${e.expr} = ${e.placeholder}`), `${t.expr} ${comparison} ${placeholder}`].join(' AND ');
    parts.push(`(${clause})`);
    equalitySoFar.push({ expr: t.expr, placeholder });
  }
  meta.cursor_keys = terms.map((t) => t.by);
  return `(${parts.join(' OR ')})`;
}

function typeOfOrderKey(by, source, query) {
  if (by === 'bucket') return 'timestamp';
  if (source.dimensions[by]) return source.dimensions[by].type;
  if (source.columns?.[by]) return source.columns[by].type;
  if (source.measures[by]) return 'number';
  void query;
  return 'text';
}

/** One filter -> one SQL predicate. The operator's identity picks the shape; the value is bound. */
function compileFilter(filter, source, bind) {
  const dimension = source.dimensions[filter.field] ?? source.columns?.[filter.field] ?? source.predicates?.[filter.field];
  if (!dimension) {
    throw unsupported(REASON.UNKNOWN_DIMENSION, `Internal: no column for filter field "${filter.field}".`, { field: filter.field });
  }
  const expr = dimension.sql;
  const cast = CAST[dimension.type];
  const arrayCast = ARRAY_CAST[dimension.type];

  // A predicate (rather than a column) owns its own comparison form. Today there is exactly
  // one: class containment over ingest.submission.labels, served by the jsonb_path_ops GIN.
  if (dimension.containment) {
    if (filter.op !== 'eq') {
      throw unsupported(REASON.OPERATOR_NOT_APPLICABLE, `"${filter.field}" supports eq only on this source.`, { field: filter.field, operator: filter.op });
    }
    // `labels` is a jsonb ARRAY of {class, score}. `array @> object` is false for every value, because containment requires the
    // same JSON type at the top level: the right operand must be a one-element ARRAY too, or the
    // filter silently matches nothing. `jsonb_path_ops` serves this form unchanged.
    return `${expr} @> jsonb_build_array(jsonb_build_object('class', ${bind(filter.value)}${cast}))`;
  }

  // An array column: equality and membership test whether any element matches, and is_null asks
  // whether the array is empty. Ordering comparisons have no meaning over a set.
  if (dimension.array) {
    switch (filter.op) {
      case 'eq':
        return `${bind(filter.value)}${cast} = ANY(${expr})`;
      case 'ne':
        return `NOT (${bind(filter.value)}${cast} = ANY(${expr}))`;
      case 'in':
        return `${expr} && ${bind(filter.value)}${arrayCast}`;
      case 'not_in':
        return `NOT (${expr} && ${bind(filter.value)}${arrayCast})`;
      case 'is_null':
        return `cardinality(${expr}) = 0`;
      default:
        throw unsupported(REASON.OPERATOR_NOT_APPLICABLE, `"${filter.field}" is a set: it supports eq, ne, in, not_in and is_null.`, { field: filter.field, operator: filter.op });
    }
  }

  switch (filter.op) {
    case 'eq':
      return `${expr} = ${bind(filter.value)}${cast}`;
    case 'ne':
      return `${expr} <> ${bind(filter.value)}${cast}`;
    case 'in':
      return `${expr} = ANY(${bind(filter.value)}${arrayCast})`;
    case 'not_in':
      return `NOT (${expr} = ANY(${bind(filter.value)}${arrayCast}))`;
    case 'lt':
      return `${expr} < ${bind(filter.value)}${cast}`;
    case 'lte':
      return `${expr} <= ${bind(filter.value)}${cast}`;
    case 'gt':
      return `${expr} > ${bind(filter.value)}${cast}`;
    case 'gte':
      return `${expr} >= ${bind(filter.value)}${cast}`;
    case 'between':
      return `${expr} BETWEEN ${bind(filter.value[0])}${cast} AND ${bind(filter.value[1])}${cast}`;
    case 'starts_with':
      // starts_with() is a literal prefix test. `LIKE $1` with a caller-supplied value would
      // make `%` and `_` wildcards, which is a different question than the one asked.
      return `starts_with(${expr}, ${bind(filter.value)}${cast})`;
    case 'is_null':
      return `${expr} IS NULL`;
    default:
      throw unsupported(REASON.UNKNOWN_OPERATOR, `Internal: no SQL form for operator "${filter.op}".`, { operator: filter.op });
  }
}

/**
 * True when the result cell may combine several source rows, so a distinct-subject column is
 * no longer exact for the cell. Used to label measure semantics rather than to change them.
 */
function collapsesGrain(query, source) {
  if (source.kind !== 'aggregate') return false;
  if (query.bucket === 'week' || query.bucket === 'month') return true;
  const grouped = new Set(query.dimensions);
  // The bucket is part of the source grain and is always grouped when it is asked for.
  if (query.bucket) grouped.add('bucket');
  return source.grain.some((name) => !grouped.has(name));
}

/**
 * Identifiers placed into SQL text are always registry names, but quoting them here means a
 * future registry entry with an awkward character cannot break the statement's shape.
 */
function quoteIdent(name) {
  return `"${String(name).replace(/"/g, '""')}"`;
}

/** True when `bucket` is a native `mart.agg_*_period.bucket_size` value. */
export function isNativeBucket(bucket) {
  return NATIVE_BUCKETS.includes(bucket);
}
