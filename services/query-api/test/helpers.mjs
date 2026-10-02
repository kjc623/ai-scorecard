// helpers.mjs — shared test scaffolding. Not a test file itself.
import { spawnSync } from 'node:child_process';

export const TENANT = '00000000-0000-4000-8000-0000000000aa';
export const WINDOW = Object.freeze({ from: '2026-09-01T00:00:00Z', to: '2026-09-08T00:00:00Z' });
export const NOW = new Date('2026-10-02T12:00:00Z');

/** A minimal, well-formed query document that each test perturbs in exactly one way. */
export function baseDoc(overrides = {}) {
  return {
    query_version: '1',
    source: 'mart.v_tool_usage',
    bucket: 'day',
    dimensions: ['tool'],
    measures: ['submissions', 'users'],
    filters: [],
    window: { ...WINDOW },
    limit: 50,
    ...overrides,
  };
}

/** Capture the typed rejection from a call, or rethrow if it did not reject. */
export function rejection(fn) {
  try {
    fn();
  } catch (error) {
    return error;
  }
  throw new Error('expected a rejection, but the call succeeded');
}

/**
 * Every quoted literal in a compiled statement, so a test can assert that a hostile value never
 * appears as one.
 */
export function quotedLiterals(text) {
  return [...text.matchAll(/'([^']*)'/g)].map((m) => m[1]);
}

const CONTAINERS = ['shadowpg', 'shadowpg-invariants'];

/** Find a running PostgreSQL container with the shadow database, or null. */
export function findContainer() {
  const listed = spawnSync('docker', ['ps', '--format', '{{.Names}}'], { encoding: 'utf8' });
  if (listed.error || listed.status !== 0) return null;
  const names = (listed.stdout ?? '').split('\n').map((s) => s.trim()).filter(Boolean);
  for (const preferred of CONTAINERS) if (names.includes(preferred)) return preferred;
  return names.find((n) => n.includes('shadow')) ?? null;
}

/** Run a multi-statement SQL script inside the container, on stdin. */
export function psqlScript(container, sql) {
  return spawnSync(
    'docker',
    ['exec', '-i', container, 'psql', '-U', 'postgres', '-d', 'shadow', '-v', 'ON_ERROR_STOP=1', '-q', '-f', '-'],
    { encoding: 'utf8', input: sql, maxBuffer: 32 * 1024 * 1024 },
  );
}

/** Test-only: render bound parameters as literals so psql can execute the statement. */
export function bindForPsql(text, params) {
  const out = text.replace(/\$(\d+)/g, (_, n) => {
    const value = params[Number(n) - 1];
    if (value === null || value === undefined) return 'NULL';
    if (typeof value === 'number') return String(value);
    if (typeof value === 'boolean') return value ? 'TRUE' : 'FALSE';
    if (Array.isArray(value)) {
      const inner = value.map((x) => (typeof x === 'number' ? String(x) : `'${String(x).replace(/'/g, "''")}'`)).join(',');
      return `ARRAY[${inner}]`;
    }
    return `'${String(value).replace(/'/g, "''")}'`;
  });
  if (/\$\d+/.test(out)) throw new Error('a parameter was left unbound');
  return out;
}

/** The tenant prelude used by the integration test; rolled back in every case. */
export function tenantPrelude(tenant = TENANT) {
  return [
    'BEGIN;',
    `SET LOCAL app.tenant_id = '${tenant}';`,
    `INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, kek_id, ceiling_mode)
       VALUES ('${tenant}', 'query-api-test', 'active', 'eu', 'vendor', 'kek-test', 'm3');`,
  ].join('\n');
}
