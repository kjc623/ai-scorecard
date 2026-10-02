#!/usr/bin/env node
// =====================================================================================
// db/tools/check-schema.mjs -- static structural checker for db/schema.sql
// =====================================================================================
// Zero external dependencies: `node db/tools/check-schema.mjs`
//
// WHAT THIS IS: a fast, offline guard on the *structure* of the schema and the test file.
// It parses the SQL as text and asserts the properties the design claims. It runs in
// milliseconds, needs no server, and is safe in CI.
//
// WHAT THIS IS NOT: proof that the invariants hold. Only a real server executing
// db/invariants.test.sql proves that, because properties like "tenant B sees zero of tenant
// A's rows" are runtime properties. This checker exists so that a *structural* regression
// (someone adds a tenant table and forgets the RLS loop, or drops a partial unique index)
// is caught even when no server is available. A green run here is not acceptance.
//
// Exit codes: 0 all structural checks passed, 1 one or more failed, 2 the checker could not run.
// =====================================================================================

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const HERE = dirname(fileURLToPath(import.meta.url));
const DB = join(HERE, '..');

const results = [];
let failed = 0;

function check(id, ok, detail) {
  results.push({ id, ok, detail, severity: 'check' });
  if (!ok) failed++;
}

// A warning is reported loudly but does not fail the run: it flags documentation drift, which
// is a real defect in the deliverable but not a structural regression in the schema.
function warn(id, ok, detail) {
  results.push({ id, ok, detail, severity: 'warn' });
}

function readOrDie(path) {
  try {
    return readFileSync(path, 'utf8');
  } catch (err) {
    console.error(`check-schema: cannot read ${path}: ${err.message}`);
    process.exit(2);
  }
}

const schema = readOrDie(join(DB, 'schema.sql'));
const tests = readOrDie(join(DB, 'invariants.test.sql'));

// Strip line comments so that prose (which discusses all of these constructs) never
// satisfies a structural check. Block comments are stripped too.
const strip = (sql) => sql.replace(/\/\*[\s\S]*?\*\//g, ' ').replace(/--[^\n]*/g, ' ');

const schemaCode = strip(schema);
const testsCode = strip(tests);

// -------------------------------------------------------------------------------------
// 1. Row-level security coverage
// -------------------------------------------------------------------------------------

const arrMatch = schemaCode.match(/tenant_tables\s+text\[\]\s*:=\s*ARRAY\s*\[([\s\S]*?)\]\s*;/);
check('rls.loop.table-list-exists', Boolean(arrMatch),
  arrMatch ? 'tenant_tables array found' : 'no tenant_tables array in schema.sql');

let tenantTables = [];
if (arrMatch) {
  tenantTables = [...arrMatch[1].matchAll(/'([a-z_]+\.[a-z_]+)'/g)].map((m) => m[1]);
  const unqualified = tenantTables.filter((t) => !/^[a-z_]+\.[a-z_]+$/.test(t));
  check('rls.loop.table-list-nonempty', tenantTables.length > 0,
    `${tenantTables.length} tenant-scoped tables: ${tenantTables.join(', ')}`);
  check('rls.loop.table-list-qualified', unqualified.length === 0,
    unqualified.length ? `unqualified: ${unqualified.join(', ')}` : 'all entries are schema-qualified');
  check('rls.loop.table-list-unique', new Set(tenantTables).size === tenantTables.length,
    new Set(tenantTables).size === tenantTables.length
      ? 'no duplicate table entries'
      : 'duplicate entries present');

  // The loop must ENABLE, FORCE, and create a policy comparing against the session tenant.
  const loopStart = schemaCode.indexOf(arrMatch[0]);
  const loopBody = schemaCode.slice(loopStart, schemaCode.indexOf('END $$;', loopStart));
  check('rls.loop.enable', /ENABLE ROW LEVEL SECURITY/.test(loopBody),
    'loop issues ALTER TABLE ... ENABLE ROW LEVEL SECURITY');
  check('rls.loop.force', /FORCE ROW LEVEL SECURITY/.test(loopBody),
    'loop issues ALTER TABLE ... FORCE ROW LEVEL SECURITY');
  check('rls.loop.policy-created', /CREATE POLICY\s+tenant_isolation\s+ON\s+%s/.test(loopBody),
    'loop creates a tenant_isolation policy per table');
  check('rls.loop.using-session-tenant', /USING\s*\(\s*tenant_id\s*=\s*ops\.current_tenant\(\)\s*\)/.test(loopBody),
    'policy USING compares tenant_id to ops.current_tenant()');
  check('rls.loop.with-check-session-tenant', /WITH CHECK\s*\(\s*tenant_id\s*=\s*ops\.current_tenant\(\)\s*\)/.test(loopBody),
    'policy WITH CHECK compares tenant_id to ops.current_tenant()');
}

// ops.tenant is the degenerate case and is handled outside the loop, by name.
{
  const idx = schemaCode.indexOf('ALTER TABLE ops.tenant ENABLE ROW LEVEL SECURITY');
  const block = idx >= 0 ? schemaCode.slice(idx, idx + 700) : '';
  check('rls.tenant-table.enable', /ALTER TABLE ops\.tenant ENABLE ROW LEVEL SECURITY/.test(schemaCode),
    'ops.tenant has RLS enabled');
  check('rls.tenant-table.force', /ALTER TABLE ops\.tenant FORCE ROW LEVEL SECURITY/.test(schemaCode),
    'ops.tenant has RLS forced');
  check('rls.tenant-table.policy', /CREATE POLICY\s+tenant_isolation\s+ON\s+ops\.tenant/.test(schemaCode),
    'ops.tenant has an explicit tenant_isolation policy');
  check('rls.tenant-table.using-and-check',
    /USING\s*\(\s*tenant_id\s*=\s*ops\.current_tenant\(\)\s*\)/.test(block) &&
    /WITH CHECK\s*\(\s*tenant_id\s*=\s*ops\.current_tenant\(\)\s*\)/.test(block),
    'ops.tenant policy has both USING and WITH CHECK against ops.current_tenant()');
}

// current_tenant() must fail closed: a session that has set no tenant must get NULL,
// because `tenant_id = NULL` is NULL and excludes every row.
{
  const fnIdx = schemaCode.indexOf('FUNCTION ops.current_tenant()');
  const fnBody = fnIdx >= 0 ? schemaCode.slice(fnIdx, fnIdx + 900) : '';
  check('rls.current-tenant.exists', fnIdx >= 0, 'ops.current_tenant() is defined');
  check('rls.current-tenant.fail-closed',
    /current_setting\(\s*'app\.tenant_id'\s*,\s*true\s*\)/.test(fnBody) && /nullif/i.test(fnBody),
    'current_tenant() reads app.tenant_id with missing_ok=true and NULLIF, so an unset tenant yields NULL');
  check('rls.current-tenant.not-security-definer',
    !new RegExp(`FUNCTION ops\\.current_tenant\\(\\)[\\s\\S]{0,400}?SECURITY DEFINER`).test(schemaCode),
    'current_tenant() is not SECURITY DEFINER (so it cannot be used to escalate)');
}

// FORCE matters wherever owner-run paths exist. Every tenant table must be forced, and the
// per-file count of FORCE statements must cover the loop plus ops.tenant.
{
  const forceStatements = (schemaCode.match(/FORCE ROW LEVEL SECURITY/g) || []).length;
  const enableStatements = (schemaCode.match(/ENABLE ROW LEVEL SECURITY/g) || []).length;
  check('rls.enable-force-paired', enableStatements === forceStatements,
    `ENABLE x${enableStatements} vs FORCE x${forceStatements} (must pair)`);
  check('rls.loop.force-present', forceStatements >= 2,
    'FORCE appears in the loop and for ops.tenant');
}

// -------------------------------------------------------------------------------------
// 2. The two-tier dedup ladder (brief R9)
// -------------------------------------------------------------------------------------

{
  const exact = schemaCode.match(/CREATE UNIQUE INDEX\s+submission_exact_key_uniq\s+ON\s+ingest\.submission\s*\(([^)]*)\)\s*WHERE\s+([^;]*);/);
  const weak = schemaCode.match(/CREATE UNIQUE INDEX\s+submission_weak_key_uniq\s+ON\s+ingest\.submission\s*\(([^)]*)\)\s*WHERE\s+([^;]*);/);

  check('dedup.exact-index.exists', Boolean(exact),
    exact ? 'submission_exact_key_uniq is a partial unique index' : 'submission_exact_key_uniq missing');
  if (exact) {
    check('dedup.exact-index.tenant-leading', /tenant_id/.test(exact[1]) && exact[1].trim().startsWith('tenant_id'),
      `index columns: ${exact[1].trim()} (tenant must lead, per C32)`);
    check('dedup.exact-index.covers-dedup-key', /dedup_key/.test(exact[1]),
      'exact index keys on dedup_key');
    check('dedup.exact-index.partial-on-exact-only', /dedup_key\s+IS\s+NOT\s+NULL/.test(exact[2]),
      'exact index applies only where dedup_key IS NOT NULL');
  }

  check('dedup.weak-index.exists', Boolean(weak),
    weak ? 'submission_weak_key_uniq is a partial unique index' : 'submission_weak_key_uniq missing');
  if (weak) {
    check('dedup.weak-index.tenant-leading', /tenant_id/.test(weak[1]) && weak[1].trim().startsWith('tenant_id'),
      `index columns: ${weak[1].trim()} (tenant must lead, per C32)`);
    check('dedup.weak-index.keys-weak-key', /dedup_weak_key/.test(weak[1]),
      'weak index keys on dedup_weak_key');
    check('dedup.weak-index.partial-on-weak-only', /dedup_key\s+IS\s+NULL/.test(weak[2]),
      'weak index applies only where dedup_key IS NULL, so the two tiers cannot collide');
  }

  check('dedup.tiers-are-disjoint', Boolean(exact && weak) &&
    /dedup_key\s+IS\s+NOT\s+NULL/.test(exact[2]) && /dedup_key\s+IS\s+NULL/.test(weak[2]),
    'the exact tier and the weak tier are disjoint predicates over dedup_key');

  check('dedup.weak-key-column-exists', /dedup_weak_key/.test(schemaCode),
    'dedup_weak_key column is declared on ingest.submission');
  check('dedup.merge-confidence-exists', /merge_confidence/.test(schemaCode),
    'merge_confidence distinguishes a weak merge from an exact one');
}

// -------------------------------------------------------------------------------------
// 3. Append-only / tamper-evidence triggers
// -------------------------------------------------------------------------------------

{
  const required = [
    ['audit_append_only', /CREATE TRIGGER\s+audit_append_only/],
    ['audit_chain_before_insert', /CREATE TRIGGER\s+audit_chain_before_insert/],
    ['observation_append_only', /CREATE TRIGGER\s+observation_append_only/],
    ['policy_bundle_ceiling', /CREATE TRIGGER\s+policy_bundle_ceiling/],
  ];
  const missing = required.filter(([, re]) => !re.test(schemaCode)).map(([n]) => n);
  check('triggers.present', missing.length === 0,
    missing.length ? `missing triggers: ${missing.join(', ')}` : `all 4 triggers present: ${required.map(([n]) => n).join(', ')}`);

  // BOTH BEFORE UPDATE OR DELETE and BEFORE INSERT OR UPDATE OR DELETE forms must appear for
  // the append-only guarantee, and the guard functions must actually RAISE.
  check('triggers.audit-blocks-update-and-delete',
    /CREATE TRIGGER\s+audit_append_only[\s\S]{0,400}?BEFORE\s+(UPDATE\s+OR\s+DELETE|INSERT\s+OR\s+UPDATE\s+OR\s+DELETE|DELETE\s+OR\s+UPDATE)/.test(schemaCode),
    'audit_append_only fires BEFORE UPDATE OR DELETE');
  check('triggers.observation-blocks-mutation',
    /CREATE TRIGGER\s+observation_append_only[\s\S]{0,400}?BEFORE\s+(UPDATE\s+OR\s+DELETE|INSERT\s+OR\s+UPDATE\s+OR\s+DELETE|DELETE\s+OR\s+UPDATE)/.test(schemaCode),
    'observation_append_only fires BEFORE UPDATE OR DELETE');

  const raiseCount = (schemaCode.match(/RAISE\s+EXCEPTION/g) || []).length;
  check('triggers.guards-raise', raiseCount >= 3,
    `${raiseCount} RAISE EXCEPTION sites in the guard functions`);

  check('triggers.retention-path-gated', /sac\.retention_delete/.test(schemaCode),
    'observation deletion is gated on the sac.retention_delete session flag');
}

// -------------------------------------------------------------------------------------
// 4. Roles and least privilege
// -------------------------------------------------------------------------------------

const EXPECTED_ROLES = ['sac_owner', 'sac_migrator', 'sac_ingest', 'sac_control', 'sac_vault', 'sac_query', 'sac_ops'];
const RUNTIME_ROLES = ['sac_ingest', 'sac_control', 'sac_vault', 'sac_query', 'sac_ops'];

{
  const missing = EXPECTED_ROLES.filter((r) => !new RegExp(`CREATE ROLE\\s+${r}\\b`).test(schemaCode));
  check('roles.all-present', missing.length === 0,
    missing.length ? `missing roles: ${missing.join(', ')}` : `all 7 roles created: ${EXPECTED_ROLES.join(', ')}`);

  // No role may log in: the process authenticates as a service, not as a database principal
  // that can be reused from a laptop. Only sac_migrator may bypass RLS, and only because DDL
  // and cluster bootstrap must be able to cross tenants.
  const roleStmts = [...schemaCode.matchAll(/CREATE ROLE\s+(\w+)([^;]*);/g)];
  const offenders = [];
  for (const [, name, tail] of roleStmts) {
    if (/\bLOGIN\b/.test(tail)) offenders.push(`${name} LOGIN`);
    const bypass = /\bBYPASSRLS\b/.test(tail);
    if (bypass && name !== 'sac_migrator') offenders.push(`${name} BYPASSRLS`);
    if (/\bSUPERUSER\b/.test(tail)) offenders.push(`${name} SUPERUSER`);
    if (/\bCREATEDB\b/.test(tail) || /\bCREATEROLE\b/.test(tail)) offenders.push(`${name} CREATEDB/CREATEROLE`);
  }
  check('roles.no-login-no-superuser', offenders.length === 0,
    offenders.length ? `over-privileged: ${offenders.join(', ')}` : 'no role is LOGIN, SUPERUSER, CREATEDB or CREATEROLE');
  check('roles.only-migrator-bypasses-rls',
    /CREATE ROLE\s+sac_migrator\s+NOLOGIN\s+BYPASSRLS/.test(schemaCode),
    'sac_migrator is the single BYPASSRLS role');
}

// Parse GRANT statements: privilege list, object list, grantee list.
const grants = [...schemaCode.matchAll(/GRANT\s+([\s\S]*?)\s+ON\s+([\s\S]*?)\s+TO\s+([^;]*);/g)]
  .map(([, privs, objs, to]) => ({
    privileges: privs.replace(/\([^)]*\)/g, ' ').replace(/\s+/g, ' ').trim().toUpperCase(),
    rawPrivileges: privs.replace(/\s+/g, ' ').trim(),
    objects: objs.replace(/\s+/g, ' ').trim(),
    grantees: to.split(',').map((s) => s.trim()).filter(Boolean),
  }));

{
  check('grants.parsed', grants.length > 20, `${grants.length} GRANT statements parsed`);

  // No runtime role may hold UPDATE or DELETE on the audit log, or UPDATE on observations.
  // No runtime role may hold UPDATE or DELETE on the audit log: it is append-only, and the
  // only sanctioned mutation is the erasure path that runs as the owner. ingest.observation
  // is different -- the retention scheduler legitimately holds DELETE, and the binding gate
  // there is the observation_append_only trigger, not the GRANT.
  const auditOffenders = [];
  const obsWriteGrantees = new Set();
  for (const g of grants) {
    const privs = g.privileges.split(/[\s,]+/).filter(Boolean);
    const hasUpd = privs.includes('UPDATE') || privs.includes('ALL');
    const hasDel = privs.includes('DELETE') || privs.includes('ALL');
    if (!hasUpd && !hasDel) continue;
    for (const grantee of g.grantees) {
      if (!RUNTIME_ROLES.includes(grantee)) continue;
      if (/ops\.audit/.test(g.objects)) {
        auditOffenders.push(`${grantee} has ${hasUpd ? 'UPDATE ' : ''}${hasDel ? 'DELETE' : ''} on ops.audit`);
      }
      if (/ingest\.observation/.test(g.objects)) obsWriteGrantees.add(grantee);
    }
  }
  check('grants.audit-is-insert-and-select-only', auditOffenders.length === 0,
    auditOffenders.length
      ? auditOffenders.join('; ')
      : 'no runtime role holds UPDATE or DELETE on ops.audit');
  check('grants.observation-writes-only-by-retention-scheduler',
    [...obsWriteGrantees].every((r) => r === 'sac_ops'),
    [...obsWriteGrantees].every((r) => r === 'sac_ops')
      ? `ingest.observation UPDATE/DELETE held only by the retention scheduler (${[...obsWriteGrantees].join(', ') || 'none'}); the binding gate is the observation_append_only trigger + sac.retention_delete`
      : `unexpected grantees with UPDATE/DELETE on ingest.observation: ${[...obsWriteGrantees].filter((r) => r !== 'sac_ops').join(', ')}`);

  // ops.content_object holds wrapped keys: only the vault may touch it, plus the
  // scheduler that has to destroy keys at erasure time.
  const contentGrantees = new Set();
  for (const g of grants) {
    if (/ops\.content_object/.test(g.objects)) g.grantees.forEach((r) => contentGrantees.add(r));
  }
  const allowed = new Set(['sac_vault', 'sac_ops']);
  const stray = [...contentGrantees].filter((r) => !allowed.has(r));
  check('grants.content-object-restricted', stray.length === 0,
    stray.length ? `unexpected grantees on ops.content_object: ${stray.join(', ')}` : `ops.content_object granted only to: ${[...contentGrantees].join(', ')}`);

  // query-api must not reach wrapped keys at all.
  check('grants.query-cannot-see-content-object', !contentGrantees.has('sac_query'),
    contentGrantees.has('sac_query') ? 'sac_query can reach ops.content_object' : 'sac_query has no ops.content_object grant');

  // The ingest role must not be able to read content or keys.
  const ingestOnContent = grants.some((g) => /ops\.content_object/.test(g.objects) && g.grantees.includes('sac_ingest'));
  check('grants.ingest-cannot-read-content-object', !ingestOnContent,
    ingestOnContent ? 'sac_ingest can reach ops.content_object' : 'sac_ingest has no ops.content_object grant');

  // PUBLIC must be revoked by default privileges.
  check('grants.public-revoked',
    /ALTER DEFAULT PRIVILEGES[\s\S]{0,400}?REVOKE ALL ON TABLES FROM PUBLIC/.test(schemaCode),
    'ALTER DEFAULT PRIVILEGES ... REVOKE ALL ON TABLES FROM PUBLIC is present');
}

// -------------------------------------------------------------------------------------
// 5. SECURITY DEFINER functions must pin search_path
// -------------------------------------------------------------------------------------

{
  const defs = [...schemaCode.matchAll(/CREATE(?: OR REPLACE)? FUNCTION\s+([\w.]+)\s*\(([\s\S]*?)\)\s*([\s\S]*?)\$\$/g)]
    .map(([, name, args, tail]) => ({ name, sig: `${name}(${args.replace(/\s+/g, ' ').trim()})`, tail }));

  const secdef = defs.filter((d) => /SECURITY DEFINER/.test(d.tail));
  const unpinned = secdef.filter((d) => !/SET\s+search_path/i.test(d.tail));

  check('secdef.pinned-search-path', unpinned.length === 0,
    unpinned.length
      ? `SECURITY DEFINER without a pinned search_path: ${unpinned.map((d) => d.sig).join(', ')}`
      : secdef.length === 0
        ? 'no SECURITY DEFINER functions exist (check is satisfied vacuously; it will bite if one is added)'
        : `all ${secdef.length} SECURITY DEFINER functions pin search_path`);

  // The write path must be invoker-rights, because RLS is the isolation mechanism and a
  // SECURITY DEFINER write path would silently bypass it.
  const recordEvent = defs.find((d) => /ingest\.record_event/.test(d.name) || /record_event/.test(d.sig));
  check('secdef.record-event-is-invoker', !recordEvent || !/SECURITY DEFINER/.test(recordEvent.tail),
    recordEvent
      ? `ingest.record_event is ${/SECURITY DEFINER/.test(recordEvent.tail) ? 'SECURITY DEFINER (would bypass RLS)' : 'invoker-rights'}`
      : 'ingest.record_event not found');
}

// -------------------------------------------------------------------------------------
// 6. The test file: assertion inventory, and it must run as a runtime role
// -------------------------------------------------------------------------------------

{
  const ids = [...new Set([...testsCode.matchAll(/PASS\s+(T\d+)/g)].map((m) => m[1]))]
    .map((t) => Number(t.slice(1)))
    .sort((a, b) => a - b);
  const sites = [...testsCode.matchAll(/PASS\s+T\d+/g)].length;

  check('tests.has-assertions', ids.length > 0, `${ids.length} distinct assertions (${sites} PASS sites)`);
  const gaps = [];
  for (let i = 1; i <= Math.max(...ids); i++) if (!ids.includes(i)) gaps.push(i);
  check('tests.contiguous-ids', gaps.length === 0,
    gaps.length ? `gaps in the assertion numbering: T${gaps.join(', T')}` : `T1..T${Math.max(...ids)} contiguous`);

  // The isolation assertions must execute as a runtime role. The test file does this with
  // SET ROLE; a superuser session would bypass RLS and prove nothing.
  check('tests.uses-set-role', /SET ROLE\s+(sac_\w+)/.test(testsCode),
    /SET ROLE\s+(sac_\w+)/.test(testsCode)
      ? `test file drops into ${[...new Set([...testsCode.matchAll(/SET ROLE\s+(sac_\w+)/g)].map((m) => m[1]))].join(', ')}`
      : 'no SET ROLE in the test file; isolation assertions would run with the session role');
  check('tests.role-is-not-bypassrls', /SET ROLE\s+(sac_ingest|sac_control|sac_query|sac_ops|sac_vault)/.test(testsCode),
    'the role the tests drop into is a runtime role with neither SUPERUSER nor BYPASSRLS');
  check('tests.on-error-stop', /ON_ERROR_STOP\s+on/.test(testsCode) || /ON_ERROR_STOP/.test(testsCode),
    'the test file sets ON_ERROR_STOP, so a raised assertion fails the run');

  // Report the count the design documents claim, so a drift is visible rather than silent.
  // This is a WARNING, not a failure: the stale number is in the documents, and the test
  // file is the artifact under test. Fixing it means editing README/project.json, not the SQL.
  const DOC_CLAIM = 27;
  warn('tests.count-matches-docs', ids.length === DOC_CLAIM,
    ids.length === DOC_CLAIM
      ? `assertion count matches the documented ${DOC_CLAIM}`
      : `DOC DRIFT: the file contains ${ids.length} assertions (T1..T${Math.max(...ids)}), but README.md:54, README.md:154, docs/00-architecture.md:855, docs/03-data-platform.md:375 and .cockpit/project.json all claim ${DOC_CLAIM}. The documents are stale; the SQL is the artifact under test.`);
}

// -------------------------------------------------------------------------------------
// Report
// -------------------------------------------------------------------------------------

const width = Math.max(...results.map((r) => r.id.length));
console.log('db/tools/check-schema.mjs -- static structural checks (NOT a substitute for a real-server run)');
console.log('');
for (const r of results) {
  const tag = r.ok ? 'PASS' : r.severity === 'warn' ? 'WARN' : 'FAIL';
  console.log(`  ${tag}  ${r.id.padEnd(width)}  ${r.detail}`);
}
const warns = results.filter((r) => r.severity === 'warn' && !r.ok).length;
console.log('');
console.log(`  ${results.length - failed - warns} passed, ${failed} failed, ${warns} warning(s)`);
console.log('');
if (failed > 0) {
  console.log('  RESULT: FAIL -- structural regression detected.');
  process.exit(1);
}
console.log('  RESULT: PASS -- structure is intact. Runtime invariants still require:');
console.log('          psql -v ON_ERROR_STOP=1 -f db/schema.sql && psql -v ON_ERROR_STOP=1 -f db/invariants.test.sql');
