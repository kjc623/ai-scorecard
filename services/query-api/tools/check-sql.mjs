// check-sql.mjs — one-off: run every compiled statement against the real schema.
import { spawnSync } from 'node:child_process';
import { SOURCES, SOURCE_IDS } from '../src/registry.js';
import { plan } from '../src/plan.js';

const container = process.argv[2] ?? 'shadowpg-invariants';

function psql(sql) {
  return spawnSync('docker', ['exec', container, 'psql', '-U', 'postgres', '-d', 'shadow', '-v', 'ON_ERROR_STOP=1', '-tAc', sql], { encoding: 'utf8' });
}

function bind(sql, params) {
  return sql.replace(/\$(\d+)/g, (_, n) => {
    const v = params[Number(n) - 1];
    if (v === null || v === undefined) return 'NULL';
    if (typeof v === 'number') return String(v);
    if (typeof v === 'boolean') return v ? 'TRUE' : 'FALSE';
    if (Array.isArray(v)) return `ARRAY[${v.map((x) => `'${String(x).replace(/'/g, "''")}'`).join(',')}]`;
    return `'${String(v).replace(/'/g, "''")}'`;
  });
}

const WINDOW = { from: '2026-09-01T00:00:00Z', to: '2026-09-08T00:00:00Z' };
const cases = [];

for (const id of SOURCE_IDS) {
  const source = SOURCES[id];
  if (source.kind === 'aggregate') {
    const measures = Object.keys(source.measures).slice(0, 3);
    const dims = Object.keys(source.dimensions).filter((d) => d !== 'bucket' && d !== 'subject').slice(0, 2);
    const filters = dims.slice(0, 1).map((d) => {
      const dim = source.dimensions[d];
      if (dim.type === 'boolean') return { field: d, op: 'eq', value: true };
      if (dim.type === 'number') return { field: d, op: 'gte', value: 0 };
      if (dim.type === 'timestamp') return { field: d, op: 'gte', value: WINDOW.from };
      if (dim.type === 'uuid') return null;
      return { field: d, op: 'eq', value: 'x' };
    }).filter(Boolean);
    cases.push({ name: id, doc: { query_version: '1', source: id, bucket: 'day', dimensions: dims, measures, filters, window: WINDOW, limit: 10 } });
    cases.push({ name: `${id} (week, rollup)`, doc: { query_version: '1', source: id, bucket: 'week', dimensions: dims, measures, window: WINDOW, rollup: true } });
  } else {
    const filters = [];
    if (source.time) filters.push({ field: source.time.name, op: 'gte', value: source.time.type === 'date' ? '2026-09-01' : WINDOW.from });
    cases.push({ name: id, doc: { query_version: '1', source: id, filters, window: source.time ? WINDOW : undefined, limit: 10 } });
  }
}

let failures = 0;
for (const c of cases) {
  let p;
  try {
    p = plan(c.doc, { now: new Date('2026-10-02T12:00:00Z') });
  } catch (error) {
    console.log(`SKIP ${c.name}: ${error.resultState}/${error.reason}: ${String(error.message).slice(0, 100)}`);
    continue;
  }
  for (const s of p.statements) {
    const sql = bind(s.text, s.params);
    const res = psql(sql);
    const status = res.status === 0 ? 'OK  ' : 'FAIL';
    if (res.status !== 0) failures += 1;
    if (res.status !== 0) {
      console.log(`${status} ${c.name} [${s.id}]: ${(res.stderr || '').trim().split('\n').slice(0, 4).join(' | ')}`);
      console.log('     ' + sql.replace(/\n/g, ' ').slice(0, 400));
    } else {
      console.log(`${status} ${c.name} [${s.id}]`);
    }
  }
}
console.log(failures === 0 ? 'ALL STATEMENTS EXECUTE' : `${failures} FAILURES`);
process.exit(failures === 0 ? 0 : 1);
