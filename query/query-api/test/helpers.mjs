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

/**
 * The lab's own PostgreSQL, which is the container this suite is actually written against.
 *
 * It has to be preferred by name rather than found by the `*shadow*` heuristic, and that is a real
 * bug this fixes rather than a tidy-up: `database/tools/run-invariants.ps1` starts its own throwaway
 * server with a RANDOM password, and it was still running when this suite picked it by preference
 * order. The credentials below are the lab's (`localdev/docker-compose.yml`: POSTGRES_PASSWORD
 * sac-lab-only, POSTGRES_DB shadow), so the suite then failed 7 integration tests with
 * `password authentication failed for user "postgres"` — which reads as a client defect and was a
 * discovery defect. A container this suite cannot authenticate to is not its test target.
 */
const LAB_CONTAINER = 'sac-lab-postgres-1';

/** Find a running PostgreSQL container with the shadow database, or null. */
export function findContainer() {
  const listed = spawnSync('docker', ['ps', '--format', '{{.Names}}'], { encoding: 'utf8' });
  if (listed.error || listed.status !== 0) return null;
  const names = (listed.stdout ?? '').split('\n').map((s) => s.trim()).filter(Boolean);
  // The lab first: predictable name, documented credential, and the schema the compose file applied.
  if (names.includes(LAB_CONTAINER)) return LAB_CONTAINER;
  for (const preferred of CONTAINERS) if (names.includes(preferred)) return preferred;
  return names.find((n) => n.includes('shadow')) ?? null;
}

/**
 * SQLSTATE 42P01, as psql prints it when a relation does not exist.
 *
 * The distinction this exists for: "no database to test against" and "a database with no schema in
 * it" are different situations, and collapsing them is how a suite reports a pass — or a failure —
 * that means nothing. Before this helper, the DB tests asked only whether a container was RUNNING;
 * a container that had never had database/schema.sql applied then failed seven tests with
 * `relation "ops.tenant" does not exist`, which reads as a defect in the compiled SQL and is not
 * one. The precondition these tests actually need is the schema.
 */
export function hasSchema(container, { db = 'shadow', user = 'postgres' } = {}) {
  if (!container) return false;
  const res = spawnSync(
    'docker',
    ['exec', container, 'psql', '-U', user, '-d', db, '-Atc', "select 1 from information_schema.schemata where schema_name='ingest'"],
    { encoding: 'utf8' },
  );
  return (res.stdout ?? '').trim() === '1';
}

/**
 * The skip reason for a database-dependent test, or false to run it.
 *
 * Three outcomes, and each one says which it is: no container, a container with no schema, or a
 * container that is ready. A test that skipped without saying which of the two it was would hide the
 * difference between "you did not run the lab" and "you ran it against an empty database".
 */
export function dbSkipReason() {
  const container = findContainer();
  if (!container) return 'no PostgreSQL container is running (tried shadowpg, shadowpg-invariants, *shadow*): this test SKIPS and is not a pass';
  if (!hasSchema(container)) {
    return `${container} is running but database/schema.sql has not been applied to it: this test SKIPS and is not a pass. Run: node localdev/build.mjs && node localdev/run.mjs`;
  }
  return false;
}

/** Run a multi-statement SQL script inside the container, on stdin. */
export function psqlScript(container, sql) {
  return spawnSync(
    'docker',
    ['exec', '-i', container, 'psql', '-U', 'postgres', '-d', 'shadow', '-v', 'ON_ERROR_STOP=1', '-q', '-tA', '-f', '-'],
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
