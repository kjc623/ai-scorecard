#!/usr/bin/env node
// The database gate: builds a fresh database the way production does and proves its invariants.
//
//   node database/tools/test-database.mjs
//
// It starts a throwaway postgres:16 container, sets it up like Azure Database for PostgreSQL (a
// NOLOGIN azure_pg_admin role, a non-superuser administrator login with CREATEROLE that is a member
// of it, a database owned by azure_pg_admin, and an emulation of Azure's
// pgaadauth_create_principal_with_oid), runs the migrator as that administrator twice (the second
// run must change nothing), checks every component login holds its role, and runs
// database/invariants.test.sql. The container is always removed.
//
// Exit codes: 0 PASS, 1 FAIL, 3 SKIPPED (Docker or Go is unavailable). A skipped gate is not a pass.

import { spawnSync } from 'node:child_process';
import { randomBytes, randomUUID } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const DATABASE_DIR = join(dirname(fileURLToPath(import.meta.url)), '..');
const IMAGE = 'postgres:16';

const LOGINS = [
  { login: 'ingest-api', role: 'sac_ingest' },
  { login: 'control-api', role: 'sac_control' },
  { login: 'content-vault', role: 'sac_vault' },
  { login: 'query-api', role: 'sac_query' },
  { login: 'jobs', role: 'sac_ops' },
].map((l) => ({ ...l, objectId: randomUUID() }));

class GateFailure extends Error {}

function run(cmd, args, { input, cwd, env, timeout = 120_000 } = {}) {
  const r = spawnSync(cmd, args, { input, cwd, env, timeout, encoding: 'utf8', maxBuffer: 64 * 1024 * 1024 });
  return { ok: !r.error && r.status === 0, status: r.status, error: r.error, out: `${r.stdout ?? ''}${r.stderr ?? ''}`, stdout: r.stdout ?? '' };
}

function must(result, what) {
  if (!result.ok) {
    throw new GateFailure(`${what} failed${result.error ? `: ${result.error.message}` : ` (exit ${result.status})`}\n${tail(result.out)}`);
  }
  return result;
}

function tail(text, lines = 40) {
  return text.trimEnd().split(/\r?\n/).slice(-lines).map((l) => `    ${l}`).join('\n');
}

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

function skip(reason) {
  console.log(`SKIPPED database gate: ${reason}`);
  process.exit(3);
}

// --- preconditions -------------------------------------------------------------------------------

const docker = run('docker', ['version', '--format', '{{.Server.Version}}'], { timeout: 30_000 });
if (!docker.ok) skip(`Docker is unavailable (${docker.error ? docker.error.message : docker.out.trim().split(/\r?\n/).pop()})`);
const go = run('go', ['version'], { timeout: 30_000 });
if (!go.ok) skip(`the Go toolchain is unavailable (${go.error ? go.error.message : go.out.trim()})`);

const name = `sac-dbgate-${randomBytes(4).toString('hex')}`;
const superPassword = randomBytes(12).toString('hex');
const adminPassword = randomBytes(12).toString('hex');

let removed = false;
function removeContainer() {
  if (removed) return;
  removed = true;
  run('docker', ['rm', '-f', name], { timeout: 60_000 });
}
process.once('SIGINT', () => { removeContainer(); process.exit(130); });

const psql = (args, input) => run('docker', ['exec', '-i', name, 'psql', '-U', 'postgres', '-v', 'ON_ERROR_STOP=1', ...args], { input, timeout: 300_000 });

async function gate() {
  console.log(`database gate: starting ${IMAGE} as ${name} (Docker ${docker.out.trim()})`);
  must(run('docker', ['run', '-d', '--name', name, '-e', `POSTGRES_PASSWORD=${superPassword}`, '-p', '127.0.0.1::5432', IMAGE], { timeout: 600_000 }),
    `docker run ${IMAGE}`);

  // Ready on TCP means the final server: the image's init-time server listens on the socket only.
  let ready = false;
  for (let i = 0; i < 120 && !ready; i++) {
    ready = run('docker', ['exec', name, 'pg_isready', '-h', '127.0.0.1', '-U', 'postgres'], { timeout: 10_000 }).ok;
    if (!ready) await sleep(500);
  }
  if (!ready) throw new GateFailure('PostgreSQL did not become ready within 60 seconds');

  const portLine = must(run('docker', ['port', name, '5432/tcp']), 'docker port').stdout.trim().split(/\r?\n/)[0];
  const port = portLine.slice(portLine.lastIndexOf(':') + 1);

  must(psql(['-q'], `
CREATE ROLE azure_pg_admin NOLOGIN;
CREATE ROLE sac_admin LOGIN CREATEROLE PASSWORD '${adminPassword}' IN ROLE azure_pg_admin;
CREATE DATABASE sac OWNER azure_pg_admin;
\\connect sac
CREATE FUNCTION public.pgaadauth_create_principal_with_oid(rolename text, objectid text, objecttype text, isadmin boolean, ismfa boolean)
RETURNS text LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
BEGIN
  EXECUTE format('CREATE ROLE %I LOGIN', rolename);
  RETURN 'Created role for ' || rolename;
END $$;
REVOKE EXECUTE ON FUNCTION public.pgaadauth_create_principal_with_oid(text, text, text, boolean, boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION public.pgaadauth_create_principal_with_oid(text, text, text, boolean, boolean) TO azure_pg_admin;
`), 'Azure emulation setup');
  console.log('  ok    Azure emulation: azure_pg_admin, non-superuser sac_admin (CREATEROLE), database sac');

  const env = {
    ...process.env,
    SAC_PG_HOST: '127.0.0.1',
    SAC_PG_PORT: port,
    SAC_PG_DATABASE: 'sac',
    SAC_PG_USER: 'sac_admin',
    SAC_PG_PASSWORD: adminPassword,
    SAC_PG_SSLMODE: 'disable',
    SAC_DB_LOGINS: JSON.stringify(LOGINS),
  };
  for (const attempt of [1, 2]) {
    const r = must(run('go', ['run', './cmd/migrate'], { cwd: DATABASE_DIR, env, timeout: 600_000 }), `migrator run ${attempt}`);
    const records = r.stdout.split(/\r?\n/).filter(Boolean).flatMap((line) => {
      try { return [JSON.parse(line)]; } catch { return []; }
    });
    const done = records.find((rec) => rec.msg === 'schema up to date');
    if (!done) throw new GateFailure(`migrator run ${attempt} did not report the schema up to date\n${tail(r.out)}`);
    if (attempt === 1 && !(done.applied >= 1)) throw new GateFailure(`the first run applied ${done.applied} migrations on an empty database`);
    if (attempt === 2 && done.applied !== 0) throw new GateFailure(`the second run applied ${done.applied} migrations; a re-run must be a no-op`);
    console.log(`  ok    migrator run ${attempt}: applied ${done.applied}, schema version ${done.version}`);
  }

  const members = must(psql(['-d', 'sac', '-tA', '-c', `
SELECT string_agg(u.rolname || '=' || r.rolname, ',' ORDER BY u.rolname)
  FROM pg_auth_members m
  JOIN pg_roles r ON r.oid = m.roleid
  JOIN pg_roles u ON u.oid = m.member
 WHERE r.rolname LIKE 'sac\\_%' AND u.rolname <> 'sac_admin' AND u.rolcanlogin AND NOT u.rolsuper`]), 'login check').stdout.trim();
  const expected = [...LOGINS].sort((a, b) => a.login.localeCompare(b.login)).map((l) => `${l.login}=${l.role}`).join(',');
  if (members !== expected) throw new GateFailure(`component logins hold ${members || 'nothing'}, expected ${expected}`);
  console.log(`  ok    ${LOGINS.length} component logins created and granted exactly their roles`);

  const inv = psql(['-d', 'sac', '-f', '-'], readFileSync(join(DATABASE_DIR, 'invariants.test.sql'), 'utf8'));
  const ids = new Set([...inv.out.matchAll(/PASS (T\d+)/g)].map((m) => m[1]));
  if (!inv.ok || !inv.out.includes('all invariant tests completed')) {
    throw new GateFailure(`invariants.test.sql failed after ${ids.size} passing assertions\n${tail(inv.out)}`);
  }
  console.log(`  ok    invariants.test.sql: ${ids.size} assertions passed`);
  return ids.size;
}

try {
  const n = await gate();
  removeContainer();
  console.log(`PASS database gate: schema applied as a non-superuser, re-run is a no-op, ${n} invariants hold`);
  process.exit(0);
} catch (err) {
  removeContainer();
  console.log(`FAIL database gate: ${err instanceof GateFailure ? err.message : err.stack}`);
  process.exit(1);
}
