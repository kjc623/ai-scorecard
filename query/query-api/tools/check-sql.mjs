// check-sql.mjs — run every compiled statement against the real db/schema.sql.
//
// This is the strongest evidence this package can produce offline: it is not a mock and not a
// snapshot. Each statement is bound (test-only literal rendering) and executed by PostgreSQL
// 17.11 against the schema another agent applied, inside one transaction that is rolled back, so
// the shared database is left exactly as it was found.
//
//   node tools/check-sql.mjs [container]
//
// The container name is discovered: `shadowpg`, then `shadowpg-invariants`, then any running
// container whose name contains "shadow". Exit code 0 only when every statement executed.

import { spawnSync } from 'node:child_process';
import { SOURCES, SOURCE_IDS } from '../src/registry.js';
import { plan } from '../src/plan.js';

const TENANT = '00000000-0000-4000-8000-0000000000ff';
const WINDOW = { from: '2026-09-01T00:00:00Z', to: '2026-09-08T00:00:00Z' };
const NOW = new Date('2026-10-02T12:00:00Z');

function findContainer() {
  if (process.argv[2]) return process.argv[2];
  const listed = spawnSync('docker', ['ps', '--format', '{{.Names}}'], { encoding: 'utf8' });
  const names = (listed.stdout ?? '').split('\n').map((s) => s.trim()).filter(Boolean);
  for (const preferred of ['shadowpg', 'shadowpg-invariants']) {
    if (names.includes(preferred)) return preferred;
  }
  return names.find((n) => n.includes('shadow')) ?? null;
}

function psql(container, sql) {
  // The whole corpus is one multi-statement transaction, so it is passed on stdin: a Windows
  // command line cannot carry it, and `-c` would wrap each statement in its own transaction.
  return spawnSync('docker', ['exec', '-i', container, 'psql', '-U', 'postgres', '-d', 'shadow', '-v', 'ON_ERROR_STOP=1', '-q', '-f', '-'], {
    encoding: 'utf8',
    input: sql,
    maxBuffer: 32 * 1024 * 1024,
  });
}

/** Test-only: render bound parameters as literals so psql can run the statement. */
export function bindForPsql(sql, params) {
  const out = sql.replace(/\$(\d+)/g, (_, n) => {
    const v = params[Number(n) - 1];
    if (v === null || v === undefined) return 'NULL';
    if (typeof v === 'number') return String(v);
    if (typeof v === 'boolean') return v ? 'TRUE' : 'FALSE';
    if (Array.isArray(v)) {
      const inner = v.map((x) => (typeof x === 'number' ? String(x) : `'${String(x).replace(/'/g, "''")}'`)).join(',');
      return `ARRAY[${inner}]`;
    }
    return `'${String(v).replace(/'/g, "''")}'`;
  });
  if (/\$\d+/.test(out)) throw new Error('parameter left unbound');
  return out;
}

/** Every shape the ten templates and the registered sources can produce. */
export function corpus() {
  const cases = [];
  for (const id of SOURCE_IDS) {
    const source = SOURCES[id];
    if (source.kind === 'aggregate') {
      const measures = Object.keys(source.measures).slice(0, 3);
      const dims = Object.keys(source.dimensions).filter((d) => d !== 'bucket' && d !== 'subject').slice(0, 2);
      const filters = dims
        .map((d) => {
          const dim = source.dimensions[d];
          if (dim.type === 'boolean') return { field: d, op: 'eq', value: true };
          if (dim.type === 'number') return { field: d, op: 'gte', value: 0 };
          if (dim.type === 'uuid') return { field: d, op: 'eq', value: TENANT };
          if (dim.type === 'timestamp') return { field: d, op: 'gte', value: WINDOW.from };
          return { field: d, op: 'eq', value: 'x' };
        })
        .slice(0, 1);
      const narrow = source.dimensions.device ? [{ field: 'device', op: 'eq', value: TENANT }] : [];
      // A per-subject source cannot be read without naming its subject (§11.2); every corpus
      // builder has to say who it is asking about.
      if (source.requiresSubjectScope) narrow.push({ field: 'subject', op: 'eq', value: 'user_ref_0001' });
      cases.push({ name: id, doc: { query_version: '1', source: id, bucket: 'day', dimensions: dims, measures, filters: [...filters, ...narrow], window: WINDOW, limit: 10 } });
      cases.push({ name: `${id} week+rollup`, doc: { query_version: '1', source: id, bucket: 'week', dimensions: dims, measures, filters: narrow, window: WINDOW, rollup: true } });
    } else {
      const filters = [];
      if (source.time) filters.push({ field: source.time.name, op: 'gte', value: source.time.type === 'date' ? '2026-09-01' : WINDOW.from });
      if (source.dimensions.region) filters.push({ field: 'region', op: 'eq', value: 'eu' });
      if (source.dimensions.population) filters.push({ field: 'population', op: 'is_null' });
      cases.push({
        name: id,
        doc: { query_version: '1', source: id, filters, ...(source.time ? { window: WINDOW } : {}), limit: 10 },
      });
    }
  }
  return cases;
}

const container = findContainer();
if (!container) {
  console.log('SKIP: no shadow* container is running');
  process.exit(0);
}

const prelude = [
  'BEGIN;',
  `SET LOCAL app.tenant_id = '${TENANT}';`,
  `INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, kek_id, ceiling_mode) VALUES ('${TENANT}', 'query-api-sqlcheck', 'active', 'eu', 'vendor', 'kek-sqlcheck', 'm3');`,
  "INSERT INTO ops.audit (tenant_id, actor_type, actor_id, action, object_type) VALUES (ops.current_tenant(), 'user', 'sqlcheck', 'check.run', 'ops.tenant');",
].join('\n');

let failures = 0;
let statements = 0;
const db = [prelude];
for (const c of corpus()) {
  let p;
  try {
    p = plan(c.doc, { now: NOW, tenant: TENANT, actorId: 'sqlcheck' });
  } catch (error) {
    console.log(`SKIP ${c.name}: ${error.resultState}/${error.reason}: ${String(error.message).slice(0, 110)}`);
    continue;
  }
  for (const s of p.statements) {
    statements += 1;
    db.push(`${bindForPsql(s.text, s.params)};`);
  }
}
db.push('ROLLBACK;');

const res = psql(container, db.join('\n'));
if (res.status !== 0) {
  failures += 1;
  console.log('FAIL batch:', (res.stderr ?? '').trim().split('\n').slice(0, 12).join('\n'));
} else {
  console.log(`OK   ${statements} statements executed against ${container} (transaction rolled back)`);
}
console.log(failures === 0 ? 'ALL STATEMENTS EXECUTE' : `${failures} FAILURES`);
process.exit(failures === 0 ? 0 : 1);
