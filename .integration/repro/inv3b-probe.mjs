// .integration/repro/inv3b-probe.mjs
//
// Independent INV-3b probe: can a hostile *value* reach SQL text through query-api's compiler?
//
// tools/check-invariants.mjs drives the compiler with hostile values in *identifier* positions
// (group_by, order.by, filters[].field) and reports 15/15 typed rejections. That leaves the other
// half unprobed: a hostile value in the *value* position, where the correct behaviour is not
// rejection but parameter binding. This probe uses the component's own validate -> guard -> compile
// path and its own document builder, with a different payload set.
//
// Run: node .integration/repro/inv3b-probe.mjs
// Exit 0 when no payload reaches SQL text and every identifier payload is refused.

import { dirname, join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const ROOT = resolve(HERE, '..', '..');
const QA = join(ROOT, 'services', 'query-api');

const { validate } = await import(pathToFileURL(join(QA, 'src', 'validate.js')).href);
const { guard } = await import(pathToFileURL(join(QA, 'src', 'guard.js')).href);
const { compile } = await import(pathToFileURL(join(QA, 'src', 'compile.js')).href);
const { QueryError, RESULT_STATES, REASON } = await import(pathToFileURL(join(QA, 'src', 'errors.js')).href);
const { baseDoc, NOW, quotedLiterals } = await import(pathToFileURL(join(QA, 'test', 'helpers.mjs')).href);

const REASON_CODES = new Set(Object.values(REASON));
const RESULT_STATE_NAMES = new Set(Object.keys(RESULT_STATES));

/** The project's typed rejection is QueryError with a closed `reason` and a §13 `resultState`. */
function typedRejection(error) {
  return (
    error instanceof QueryError &&
    typeof error.reason === 'string' &&
    REASON_CODES.has(error.reason) &&
    RESULT_STATE_NAMES.has(error.resultState)
  );
}

const problems = [];
const note = (ok, message) => {
  console.log(`  ${ok ? 'ok   ' : 'FAIL '} ${message}`);
  if (!ok) problems.push(message);
};

function run(doc) {
  const validated = validate(doc, { now: NOW, actorId: 'verifier' });
  const guarded = guard(validated);
  return compile({ ...validated, query: guarded.query });
}

// ---------------------------------------------------------------------------------------------
// 1. Hostile VALUES, valid field: must be bound, never interpolated.
// ---------------------------------------------------------------------------------------------
const hostileValues = [
  "'; DROP TABLE ingest.observation--",
  "x' OR '1'='1",
  '$$; SELECT pg_sleep(10); --',
  "\\'; UNION SELECT tenant_id FROM ops.tenant--",
  'claude_web\'; --',
  'a"b`c',
  '\u0000null-byte',
  "100%'; SELECT 1; --",
];

console.log('INV-3b a) hostile values in the value position (tool eq <payload>)');
for (const payload of hostileValues) {
  let statement;
  try {
    statement = run(baseDoc({ filters: [{ field: 'tool', op: 'eq', value: payload }] }));
  } catch (error) {
    // A rejection here is also safe (fail closed), but it must be the project's typed rejection.
    note(typedRejection(error), `value ${JSON.stringify(payload.slice(0, 28))} rejected as a QueryError (${error?.reason ?? error?.name ?? 'no reason'})`);
    continue;
  }
  const text = statement.text;
  const containsRaw = text.includes(payload);
  const containsKeyword = /DROP TABLE|UNION SELECT|pg_sleep/i.test(text);
  const literals = quotedLiterals(text);
  const asLiteral = literals.some((l) => l.includes('DROP') || l.includes('pg_sleep') || l.includes('UNION'));
  const bound = statement.params.some((p) => (Array.isArray(p) ? p.includes(payload) : p === payload));
  note(!containsRaw, `payload ${JSON.stringify(payload.slice(0, 28))} is not present in the SQL text`);
  note(!containsKeyword && !asLiteral, `payload ${JSON.stringify(payload.slice(0, 28))} contributes no SQL keyword or literal`);
  note(bound, `payload ${JSON.stringify(payload.slice(0, 28))} is carried as a bound parameter`);
  note(/\$\d+/.test(text), 'the statement carries at least one $n placeholder');
}

// ---------------------------------------------------------------------------------------------
// 2. Hostile IDENTIFIERS: group_by, order.by, filters[].field must be refused.
// ---------------------------------------------------------------------------------------------
const hostileIdentifiers = ['tool); DROP TABLE ingest.observation--', '__ord_tool', 'tool_fingerprint) UNION SELECT 1--', 'o.tenant_id'];
console.log('INV-3b b) hostile identifiers in identifier positions');
let refused = 0;
for (const payload of hostileIdentifiers) {
  for (const [label, doc] of [
    ['group_by', baseDoc({ dimensions: [payload] })],
    ['order.by', baseDoc({ order: [{ by: payload, dir: 'desc' }] })],
    ['filters[].field', baseDoc({ filters: [{ field: payload, op: 'eq', value: 'x' }] })],
  ]) {
    try {
      const statement = run(doc);
      if (statement.text.includes(payload)) {
        note(false, `${label} payload ${JSON.stringify(payload)} reached SQL text`);
      } else {
        // Resolved to something else (an allow-listed identifier) rather than echoed.
        note(true, `${label} payload ${JSON.stringify(payload)} was not echoed into SQL text`);
      }
    } catch (error) {
      refused += 1;
      note(typedRejection(error), `${label} payload rejected as a QueryError (${error?.reason ?? error?.name ?? 'no reason'})`);
    }
  }
}
console.log(`  refused: ${refused} of ${hostileIdentifiers.length * 3} identifier payloads`);

console.log('');
if (problems.length > 0) {
  console.log(`INV-3b probe: ${problems.length} problem(s)`);
  process.exit(1);
}
console.log('INV-3b probe: no hostile value reached SQL text and every identifier payload was refused or resolved away');
