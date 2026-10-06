#!/usr/bin/env node
// Static structural checks for services/database/schema.sql: `node services/database/tools/check-schema.mjs`.
//
// It reads the SQL as text and asserts the properties the schema relies on: row-level security on
// every tenant table, the dedup indexes, the append-only triggers, least-privilege grants, the
// SECURITY DEFINER discipline, expiry indexes, and agreement with the event envelope contract and
// the wire reason codes. It needs no server. That the properties actually hold on a running server
// is proved by services/database/tools/test-database.mjs.
//
// SAC_SCHEMA_ROOT points it at another tree (the tests use perturbed copies).
// Exit codes: 0 all checks passed, 1 a check failed, 2 the checker could not run.

import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join, resolve } from 'node:path';

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = process.env.SAC_SCHEMA_ROOT ? resolve(process.env.SAC_SCHEMA_ROOT) : join(HERE, '..', '..', '..');

function read(rel) {
  try {
    return readFileSync(join(REPO, ...rel.split('/')), 'utf8');
  } catch {
    return null;
  }
}

const schema = read('services/database/schema.sql');
if (schema === null) {
  console.error(`check-schema: no services/database/schema.sql under ${REPO} (set SAC_SCHEMA_ROOT to a tree that has one)`);
  process.exit(2);
}
const tests = read('services/database/invariants.test.sql');
if (tests === null) {
  console.error(`check-schema: no services/database/invariants.test.sql under ${REPO}`);
  process.exit(2);
}

const results = [];
function check(id, ok, detail) {
  results.push({ id, ok: Boolean(ok), detail });
}

// Comments are stripped so that prose never satisfies a structural check.
const strip = (sql) => sql.replace(/\/\*[\s\S]*?\*\//g, ' ').replace(/--[^\n]*/g, ' ');
const schemaCode = strip(schema);
const testsCode = strip(tests);

const RUNTIME_ROLES = ['sac_ingest', 'sac_control', 'sac_vault', 'sac_query', 'sac_ops'];
const ROLES = [...RUNTIME_ROLES, 'sac_resolver'];

// -------------------------------------------------------------------------------------
// 1. Row-level security
// -------------------------------------------------------------------------------------

const arrMatch = schemaCode.match(/tenant_tables\s+text\[\]\s*:=\s*ARRAY\s*\[([\s\S]*?)\]\s*;/);
check('rls.loop.table-list-exists', arrMatch, arrMatch ? 'tenant_tables array found' : 'no tenant_tables array in schema.sql');

let tenantTables = [];
if (arrMatch) {
  tenantTables = [...arrMatch[1].matchAll(/'([a-z_]+\.[a-z_]+)'/g)].map((m) => m[1]);
  check('rls.loop.table-list-nonempty', tenantTables.length > 0, `${tenantTables.length} tenant-scoped tables`);
  check('rls.loop.table-list-unique', new Set(tenantTables).size === tenantTables.length,
    new Set(tenantTables).size === tenantTables.length ? 'no duplicate entries' : 'duplicate entries present');

  const loopStart = schemaCode.indexOf(arrMatch[0]);
  const loopBody = schemaCode.slice(loopStart, schemaCode.indexOf('END $$;', loopStart));
  check('rls.loop.enable', /ENABLE ROW LEVEL SECURITY/.test(loopBody), 'the loop enables row-level security');
  check('rls.loop.force', /FORCE ROW LEVEL SECURITY/.test(loopBody), 'the loop forces row-level security');
  check('rls.loop.policy-created', /CREATE POLICY\s+tenant_isolation\s+ON\s+%s/.test(loopBody),
    'the loop creates a tenant_isolation policy per table');
  check('rls.loop.using-session-tenant', /USING\s*\(\s*tenant_id\s*=\s*ops\.current_tenant\(\)\s*\)/.test(loopBody),
    'policy USING compares tenant_id with ops.current_tenant()');
  check('rls.loop.with-check-session-tenant', /WITH CHECK\s*\(\s*tenant_id\s*=\s*ops\.current_tenant\(\)\s*\)/.test(loopBody),
    'policy WITH CHECK compares tenant_id with ops.current_tenant()');
}

{
  const idx = schemaCode.indexOf('ALTER TABLE ops.tenant ENABLE ROW LEVEL SECURITY');
  const block = idx >= 0 ? schemaCode.slice(idx, idx + 700) : '';
  check('rls.tenant-table.forced',
    /ALTER TABLE ops\.tenant ENABLE ROW LEVEL SECURITY/.test(schemaCode) && /ALTER TABLE ops\.tenant FORCE ROW LEVEL SECURITY/.test(schemaCode),
    'ops.tenant has row-level security enabled and forced');
  check('rls.tenant-table.policy',
    /CREATE POLICY\s+tenant_isolation\s+ON\s+ops\.tenant/.test(schemaCode)
      && /USING\s*\(\s*tenant_id\s*=\s*ops\.current_tenant\(\)\s*\)/.test(block)
      && /WITH CHECK\s*\(\s*tenant_id\s*=\s*ops\.current_tenant\(\)\s*\)/.test(block),
    'ops.tenant has a tenant_isolation policy with USING and WITH CHECK against ops.current_tenant()');
}

// A session that has set no tenant gets NULL, and `tenant_id = NULL` excludes every row.
{
  const fnIdx = schemaCode.indexOf('FUNCTION ops.current_tenant()');
  const fnBody = fnIdx >= 0 ? schemaCode.slice(fnIdx, fnIdx + 400) : '';
  check('rls.current-tenant.fail-closed',
    /current_setting\(\s*'app\.tenant_id'\s*,\s*true\s*\)/.test(fnBody) && /nullif/i.test(fnBody) && !/SECURITY DEFINER/.test(fnBody),
    'ops.current_tenant() reads app.tenant_id with missing_ok and NULLIF, as invoker');
}

{
  const forced = (schemaCode.match(/FORCE ROW LEVEL SECURITY/g) || []).length;
  const enabled = (schemaCode.match(/ENABLE ROW LEVEL SECURITY/g) || []).length;
  check('rls.enable-force-paired', enabled === forced, `ENABLE x${enabled}, FORCE x${forced}`);
}

// Tables whose rows exist before their tenant is known. They have no tenant_id, so they sit outside
// the loop, but they are still forced with an explicit policy.
const PRE_TENANT_TABLES = ['ops.auth_signin'];

const tableBodies = new Map(
  [...schemaCode.matchAll(/CREATE TABLE\s+([a-z_]+\.[a-z_]+)\s*\(([\s\S]*?)\n\);/g)].map((m) => [m[1], m[2]]));

for (const table of PRE_TENANT_TABLES) {
  const esc = table.replace('.', '\\.');
  const body = tableBodies.get(table);
  check(`rls.pre-tenant.${table}.forced`,
    new RegExp(`ALTER TABLE ${esc} ENABLE ROW LEVEL SECURITY`).test(schemaCode)
      && new RegExp(`ALTER TABLE ${esc} FORCE ROW LEVEL SECURITY`).test(schemaCode),
    `${table} has row-level security enabled and forced`);
  check(`rls.pre-tenant.${table}.policy`, new RegExp(`CREATE POLICY\\s+\\w+\\s+ON\\s+${esc}\\b`).test(schemaCode),
    `${table} has an explicit policy`);
  check(`rls.pre-tenant.${table}.has-no-tenant`, body !== undefined && !/^\s*tenant_id\s/m.test(body),
    body === undefined
      ? `${table} is listed as pre-tenant but is not created`
      : /^\s*tenant_id\s/m.test(body)
        ? `${table} has a tenant_id column, so it belongs in the tenant_tables loop`
        : `${table} has no tenant_id column`);
}

// Every table outside ref is covered. ref holds shared vocabulary with no tenant_id.
{
  const created = [...tableBodies.keys()];
  const nonRef = created.filter((t) => !t.startsWith('ref.'));
  const covered = new Set([...tenantTables, 'ops.tenant', ...PRE_TENANT_TABLES]);
  const missing = nonRef.filter((t) => !covered.has(t));
  const extra = [...covered].filter((t) => !created.includes(t));
  const refWithTenant = created.filter((t) => t.startsWith('ref.') && /^\s*tenant_id\s/m.test(tableBodies.get(t)));
  check('rls.every-non-ref-table-is-covered', missing.length === 0 && extra.length === 0 && refWithTenant.length === 0,
    missing.length || extra.length || refWithTenant.length
      ? `uncovered: [${missing.join(', ')}]; policies for missing tables: [${extra.join(', ')}]; ref tables with tenant_id: [${refWithTenant.join(', ')}]`
      : `all ${nonRef.length} tables outside ref are under row-level security`);
}

// -------------------------------------------------------------------------------------
// 2. The two-tier dedup ladder
// -------------------------------------------------------------------------------------

{
  const exact = schemaCode.match(/CREATE UNIQUE INDEX\s+submission_exact_key_uniq\s+ON\s+ingest\.submission\s*\(([^)]*)\)\s*WHERE\s+([^;]*);/);
  const weak = schemaCode.match(/CREATE UNIQUE INDEX\s+submission_weak_key_uniq\s+ON\s+ingest\.submission\s*\(([^)]*)\)\s*WHERE\s+([^;]*);/);

  check('dedup.exact-index.exists', exact, exact ? 'submission_exact_key_uniq is a partial unique index' : 'submission_exact_key_uniq missing');
  if (exact) {
    check('dedup.exact-index.shape',
      exact[1].trim().startsWith('tenant_id') && /dedup_key/.test(exact[1]) && /dedup_key\s+IS\s+NOT\s+NULL/.test(exact[2]),
      `(${exact[1].trim()}) WHERE ${exact[2].trim()}`);
  }
  check('dedup.weak-index.exists', weak, weak ? 'submission_weak_key_uniq is a partial unique index' : 'submission_weak_key_uniq missing');
  if (weak) {
    check('dedup.weak-index.shape',
      weak[1].trim().startsWith('tenant_id') && /dedup_weak_key/.test(weak[1]),
      `(${weak[1].trim()}) WHERE ${weak[2].trim()}`);
  }
  check('dedup.tiers-are-disjoint',
    exact && weak && /dedup_key\s+IS\s+NOT\s+NULL/.test(exact[2]) && /dedup_key\s+IS\s+NULL/.test(weak[2]),
    'the exact tier binds rows with an exact key and the weak tier only rows without one');
}

// -------------------------------------------------------------------------------------
// 3. Every digest-shaped column carries a format CHECK
// -------------------------------------------------------------------------------------

{
  const digestish = [];
  const unconstrained = [];
  for (const [table, body] of tableBodies) {
    for (const m of body.matchAll(/^\s{2}([a-z_][a-z0-9_]*)\s+(?:text|character varying)\b/gm)) {
      const col = m[1];
      if (/(_digest|_sha256)$/.test(col) || col === 'dedup_key' || col === 'dedup_weak_key') {
        digestish.push(`${table}.${col}`);
        // The CHECK must be in the same table: another table's column of the same name is not it.
        if (!new RegExp(`\\b${col}\\s*~\\s*'\\^sha256:\\[0-9a-f\\]\\{64\\}\\$'`).test(body)) unconstrained.push(`${table}.${col}`);
      }
    }
  }
  check('digest.columns-found', digestish.length >= 6, `${digestish.length} digest-shaped columns`);
  check('digest.every-column-format-checked', unconstrained.length === 0,
    unconstrained.length ? `digest columns without a '^sha256:[0-9a-f]{64}$' CHECK: ${unconstrained.join(', ')}` : 'every digest column accepts only sha256:<64 lowercase hex>');
}

// -------------------------------------------------------------------------------------
// 4. Append-only and tamper-evidence triggers
// -------------------------------------------------------------------------------------

{
  // `\b`, so a trigger renamed to audit_append_only_old does not read as still present.
  const required = ['audit_append_only', 'audit_chain_before_insert', 'observation_append_only', 'policy_bundle_ceiling', 'retrieval_grant_single_use'];
  const missing = required.filter((n) => !new RegExp(`CREATE TRIGGER\\s+${n}\\b`).test(schemaCode));
  check('triggers.present', missing.length === 0, missing.length ? `missing triggers: ${missing.join(', ')}` : `all ${required.length} triggers present`);
  check('triggers.audit-blocks-update-and-delete',
    /CREATE TRIGGER\s+audit_append_only\b[\s\S]{0,200}?BEFORE\s+(UPDATE\s+OR\s+DELETE|DELETE\s+OR\s+UPDATE)/.test(schemaCode),
    'audit_append_only fires BEFORE UPDATE OR DELETE');
  check('triggers.observation-blocks-mutation',
    /CREATE TRIGGER\s+observation_append_only\b[\s\S]{0,200}?BEFORE\s+(UPDATE\s+OR\s+DELETE|DELETE\s+OR\s+UPDATE)/.test(schemaCode),
    'observation_append_only fires BEFORE UPDATE OR DELETE');
  check('triggers.retention-path-gated', /current_setting\('sac\.retention_delete'/.test(schemaCode),
    'observation deletion is gated on the sac.retention_delete session setting');
}

// -------------------------------------------------------------------------------------
// 5. Roles and grants
// -------------------------------------------------------------------------------------

{
  const roleStmts = new Map([...schemaCode.matchAll(/CREATE ROLE\s+(\w+)([^;]*);/g)].map((m) => [m[1], m[2]]));
  const missing = ROLES.filter((r) => !roleStmts.has(r));
  const extra = [...roleStmts.keys()].filter((r) => !ROLES.includes(r));
  check('roles.exactly-the-expected-set', missing.length === 0 && extra.length === 0,
    missing.length || extra.length ? `missing [${missing.join(', ')}], unexpected [${extra.join(', ')}]` : `roles: ${ROLES.join(', ')}`);
  const offenders = [];
  for (const [name, tail] of roleStmts) {
    if (!/\bNOLOGIN\b/.test(tail)) offenders.push(`${name} is not NOLOGIN`);
    for (const attr of ['SUPERUSER', 'BYPASSRLS', 'CREATEDB', 'CREATEROLE']) {
      if (new RegExp(`\\b${attr}\\b`).test(tail)) offenders.push(`${name} ${attr}`);
    }
  }
  check('roles.no-login-no-bypass', offenders.length === 0,
    offenders.length ? offenders.join('; ') : 'every role is NOLOGIN without SUPERUSER, BYPASSRLS, CREATEDB or CREATEROLE');
}

// GRANT <privileges> ON <objects> TO <grantees>; a privilege may carry a column list.
const grants = [...schemaCode.matchAll(/GRANT\s+([^;]*?)\s+ON\s+(?!SCHEMA\b)([^;]*?)\s+TO\s+([^;]*);/g)].map(([, privs, objs, to]) => ({
  privileges: [...privs.matchAll(/([A-Z]+)\s*(\(([^)]*)\))?/g)].map((m) => ({
    name: m[1],
    columns: m[3] ? m[3].split(',').map((c) => c.trim()) : null,
  })),
  objects: objs.replace(/^(TABLE|FUNCTION)\s+/, '').split(/,(?![^(]*\))/).map((o) => o.trim()),
  grantees: to.split(',').map((s) => s.trim()),
}));
const grantsOn = (table) => grants.filter((g) => g.objects.includes(table));

check('grants.parsed', grants.length > 30, `${grants.length} GRANT statements parsed`);

{
  const offenders = [];
  for (const g of grantsOn('ops.audit')) {
    if (g.privileges.some((p) => ['UPDATE', 'DELETE', 'ALL', 'TRUNCATE'].includes(p.name))) offenders.push(g.grantees.join(','));
  }
  check('grants.audit-is-append-only', offenders.length === 0,
    offenders.length ? `UPDATE/DELETE on ops.audit granted to ${offenders.join('; ')}` : 'no role holds UPDATE or DELETE on ops.audit');
}

{
  const writers = new Set();
  for (const g of grantsOn('ingest.observation')) {
    if (g.privileges.some((p) => ['UPDATE', 'DELETE', 'ALL'].includes(p.name))) g.grantees.forEach((r) => writers.add(r));
  }
  check('grants.observation-deleted-only-by-jobs', [...writers].every((r) => r === 'sac_ops'),
    `UPDATE/DELETE on ingest.observation held by: ${[...writers].join(', ') || 'nobody'} (the observation_append_only trigger refuses UPDATE regardless)`);
}

// Stored content and the content search index: content-vault reads them; the expire job may delete
// rows and read only non-content columns; nobody else holds anything.
for (const [table, contentColumns] of [['ops.content', ['ciphertext', 'raw_digest']], ['ingest.search_text', ['body', 'tsv']]]) {
  const gs = grantsOn(table);
  const grantees = new Set(gs.flatMap((g) => g.grantees));
  const stray = [...grantees].filter((r) => r !== 'sac_vault' && r !== 'sac_ops');
  check(`grants.${table}.only-vault-and-jobs`, stray.length === 0,
    stray.length ? `${table} is granted to ${stray.join(', ')}` : `${table} granted only to ${[...grantees].join(', ')}`);
  const jobPrivs = gs.filter((g) => g.grantees.includes('sac_ops')).flatMap((g) => g.privileges);
  const unreadable = jobPrivs.every((p) => p.name === 'DELETE' || (p.name === 'SELECT' && p.columns && !p.columns.some((c) => contentColumns.includes(c))));
  check(`grants.${table}.jobs-delete-without-reading`, unreadable && jobPrivs.some((p) => p.name === 'DELETE'),
    unreadable
      ? `sac_ops holds ${jobPrivs.map((p) => p.columns ? `${p.name}(${p.columns.join(', ')})` : p.name).join(', ')}`
      : `sac_ops can read ${contentColumns.join('/')} or write ${table}`);
}

check('grants.nothing-to-public', !grants.some((g) => g.grantees.includes('PUBLIC')),
  'no privilege is granted to PUBLIC');

// -------------------------------------------------------------------------------------
// 6. SECURITY DEFINER functions
// -------------------------------------------------------------------------------------

{
  const defs = [...schemaCode.matchAll(/CREATE(?: OR REPLACE)? FUNCTION\s+([\w.]+)\s*\(([\s\S]*?)\)\s*([\s\S]*?)\$\$/g)]
    .map(([, name, args, tail]) => ({ name, sig: `${name}(${args.replace(/\s+/g, ' ').trim()})`, tail }));
  const secdef = defs.filter((d) => /SECURITY DEFINER/.test(d.tail));

  check('secdef.found', secdef.length > 0, `${secdef.length} SECURITY DEFINER functions`);

  const unpinned = secdef.filter((d) => !/SET\s+search_path/i.test(d.tail));
  check('secdef.pinned-search-path', unpinned.length === 0,
    unpinned.length ? `without a pinned search_path: ${unpinned.map((d) => d.sig).join(', ')}` : 'every definer pins search_path');

  const revoked = new Set();
  let firstRevoke = Infinity;
  for (const m of schemaCode.matchAll(/REVOKE\s+EXECUTE\s+ON\s+FUNCTION\s+([\s\S]*?)\s+FROM\s+PUBLIC\s*;/g)) {
    firstRevoke = Math.min(firstRevoke, m.index);
    for (const f of m[1].matchAll(/([a-z_]+\.[a-z_]+)\s*\(/g)) revoked.add(f[1]);
  }
  const publicExec = secdef.filter((d) => !revoked.has(d.name));
  check('secdef.execute-revoked-from-public', publicExec.length === 0,
    publicExec.length ? `still executable by PUBLIC: ${publicExec.map((d) => d.sig).join(', ')}` : 'EXECUTE is revoked from PUBLIC on every definer');

  // A definer's reach is its owner's: a NOLOGIN role that neither bypasses row-level security nor is
  // a runtime role (sac_resolver and its SELECT-only policies).
  const ownerStmts = [...schemaCode.matchAll(/ALTER FUNCTION\s+([a-z_]+\.[a-z_]+)\s*\([^)]*\)\s+OWNER TO\s+(\w+)\s*;/g)];
  const owners = new Map(ownerStmts.map((m) => [m[1], m[2]]));
  const badOwner = secdef.filter((d) => owners.get(d.name) !== 'sac_resolver');
  check('secdef.owned-by-resolver', badOwner.length === 0,
    badOwner.length ? `not owned by sac_resolver: ${badOwner.map((d) => `${d.name} (${owners.get(d.name) ?? 'the applying role'})`).join(', ')}` : 'every definer is owned by sac_resolver');

  // Privileges must be set while the applying role still owns the functions: afterwards it can no
  // longer change them, and PUBLIC would keep EXECUTE.
  const firstAlter = ownerStmts.length ? Math.min(...ownerStmts.map((m) => m.index)) : -1;
  check('secdef.privileges-set-before-handover', firstRevoke < firstAlter,
    firstRevoke < firstAlter ? 'EXECUTE is revoked from PUBLIC before ownership moves' : 'ownership moves before EXECUTE is revoked from PUBLIC, so the revoke cannot apply');

  // The hand-over needs the applying role to be a member of sac_resolver and sac_resolver to have
  // CREATE on ops; both are taken back afterwards.
  const handover = [
    [/GRANT\s+sac_resolver\s+TO\s+CURRENT_USER\s*;/, /REVOKE\s+sac_resolver\s+FROM\s+CURRENT_USER\s*;/],
    [/GRANT\s+CREATE\s+ON\s+SCHEMA\s+ops\s+TO\s+sac_resolver\s*;/, /REVOKE\s+CREATE\s+ON\s+SCHEMA\s+ops\s+FROM\s+sac_resolver\s*;/],
  ];
  const lastAlter = ownerStmts.length ? Math.max(...ownerStmts.map((m) => m.index)) : -1;
  const handoverOk = handover.every(([g, r]) => {
    const gi = schemaCode.search(g);
    const ri = schemaCode.search(r);
    return gi >= 0 && ri > lastAlter && gi < firstAlter;
  });
  check('secdef.handover-revoked', handoverOk,
    handoverOk ? 'sac_resolver keeps no members and no CREATE on ops after the hand-over' : 'the sac_resolver membership or CREATE on ops is not taken back after the ownership hand-over');

  const recordEvent = defs.find((d) => d.name === 'ingest.record_event');
  check('secdef.record-event-is-invoker', recordEvent && !/SECURITY DEFINER/.test(recordEvent.tail),
    'ingest.record_event runs with the caller\'s rights, so row-level security applies to it');
}

// -------------------------------------------------------------------------------------
// 7. Expiry indexes for the tables the expire job sweeps
// -------------------------------------------------------------------------------------

{
  const swept = ['ingest.observation', 'ingest.submission', 'ingest.rejected', 'ingest.search_text', 'ops.content'];
  const missing = swept.filter((t) => !new RegExp(`CREATE INDEX\\s+\\w+\\s+ON\\s+${t.replace('.', '\\.')}\\s*\\(\\s*tenant_id\\s*,\\s*expires_at\\s*\\)`).test(schemaCode));
  check('expiry.indexes', missing.length === 0,
    missing.length ? `no (tenant_id, expires_at) index on ${missing.join(', ')}` : `(tenant_id, expires_at) indexed on ${swept.join(', ')}`);
}

// -------------------------------------------------------------------------------------
// 8. The invariant tests
// -------------------------------------------------------------------------------------

{
  const ids = [...new Set([...testsCode.matchAll(/PASS\s+T(\d+)/g)].map((m) => Number(m[1])))].sort((a, b) => a - b);
  const max = ids.length ? ids[ids.length - 1] : 0;
  const gaps = [];
  for (let i = 1; i <= max; i++) if (!ids.includes(i)) gaps.push(`T${i}`);
  check('tests.has-assertions', ids.length > 0, `${ids.length} assertions`);
  check('tests.contiguous-ids', ids.length > 0 && gaps.length === 0, gaps.length ? `gaps: ${gaps.join(', ')}` : `T1..T${max}`);
  const roles = [...new Set([...testsCode.matchAll(/SET ROLE\s+(sac_\w+)/g)].map((m) => m[1]))];
  check('tests.run-as-runtime-roles', roles.some((r) => RUNTIME_ROLES.includes(r)),
    roles.length ? `the tests switch into ${roles.join(', ')}` : 'no SET ROLE: isolation would be tested as the session role');
  check('tests.on-error-stop', /\\set\s+ON_ERROR_STOP\s+on/.test(tests), 'ON_ERROR_STOP is set, so a raised assertion fails the run');
}

// -------------------------------------------------------------------------------------
// 9. The quarantine reason codes are exactly the codes ingest-api quarantines
// -------------------------------------------------------------------------------------
// ingest-api's per-event validator rejects an event with reject(protocol.ReasonX, ...) (directly or
// through a `reason` variable) and stores the code verbatim in ingest.rejected, except the codes it
// compares away with `Reason != protocol.ReasonX`. The CHECK must accept exactly the rest: a code the
// CHECK refuses would fail the whole batch at runtime.

{
  const protocolSrc = read('endpoint/protocol/batch.go');
  const serviceSrc = read('services/ingest-api/internal/ingest/service.go');
  const m = schemaCode.match(/reason_code\s+text\s+NOT NULL\s+CHECK\s*\(\s*reason_code\s+IN\s*\(([\s\S]*?)\)\)/);
  const sqlCodes = m ? [...m[1].matchAll(/'([a-z_]+)'/g)].map((x) => x[1]) : [];
  check('vocab.check-found', m && sqlCodes.length > 0, m ? `${sqlCodes.length} codes in the ingest.rejected CHECK` : 'the reason_code CHECK was not found');

  const wire = new Map(protocolSrc ? [...strip(protocolSrc).matchAll(/(Reason\w+)\s+ReasonCode\s*=\s*"([a-z_]+)"/g)].map((x) => [x[1], x[2]]) : []);
  const service = serviceSrc ? strip(serviceSrc) : '';
  const rejected = new Set([...service.matchAll(/(?:reject\(\s*|reason\s*:?=\s*)protocol\.(Reason\w+)/g)].map((x) => x[1]));
  const notStored = new Set([...service.matchAll(/Reason\s*!=\s*protocol\.(Reason\w+)/g)].map((x) => x[1]));
  const unknown = [...rejected].filter((n) => !wire.has(n));
  check('vocab.emitters-found', wire.size > 0 && rejected.size > 0 && unknown.length === 0,
    wire.size === 0 || rejected.size === 0
      ? 'endpoint/protocol/batch.go or services/ingest-api/internal/ingest/service.go is missing, or no reject(protocol.Reason...) call was found'
      : unknown.length
        ? `service.go rejects with codes batch.go does not declare: ${unknown.join(', ')}`
        : `ingest-api rejects events with ${[...rejected].map((n) => wire.get(n)).join(', ')}; never stored: ${[...notStored].map((n) => wire.get(n)).join(', ') || 'none'}`);

  const expected = [...rejected].filter((n) => !notStored.has(n)).map((n) => wire.get(n));
  const missing = expected.filter((c) => !sqlCodes.includes(c));
  const extra = sqlCodes.filter((c) => !expected.includes(c));
  check('vocab.check-equals-quarantined-codes', expected.length > 0 && missing.length === 0 && extra.length === 0 && new Set(sqlCodes).size === sqlCodes.length,
    missing.length || extra.length
      ? `the CHECK refuses [${missing.join(', ')}], which ingest-api quarantines, and accepts [${extra.join(', ')}], which it never does`
      : `the CHECK accepts exactly the ${expected.length} codes ingest-api quarantines`);
}

// -------------------------------------------------------------------------------------
// 10. ingest.observation's kind and mode boundaries equal the envelope contract's
// -------------------------------------------------------------------------------------
// contracts/event-envelope.schema.json is authoritative. `attachments` has no column on
// ingest.observation, so it is excused from every comparison by name.

const NO_COLUMN_ON_OBSERVATION = new Set(['attachments']);

{
  const contractText = read('contracts/event-envelope.schema.json');
  let contract = null;
  try {
    contract = contractText === null ? null : JSON.parse(contractText);
  } catch {
    contract = null;
  }
  check('kinds.contract-readable', contract, contract ? 'contracts/event-envelope.schema.json parses' : 'contracts/event-envelope.schema.json is missing or not JSON');

  if (contract) {
    const branches = [];
    const walk = (node) => {
      if (Array.isArray(node)) { node.forEach(walk); return; }
      if (node && typeof node === 'object') {
        const kind = node.if?.properties?.kind?.const;
        if (kind && node.then) {
          const mode = node.if.properties.collection_mode;
          branches.push({ key: kind + (mode?.const ? `:${mode.const}` : mode?.enum ? ':m1+' : ''), then: node.then });
        }
        Object.values(node).forEach(walk);
      }
    };
    walk(contract);
    check('kinds.branches-found', branches.length >= 6, `${branches.length} contract branches: ${branches.map((b) => b.key).join(', ')}`);

    // One CHECK constraint's text: from its name to the next constraint or the end of the table.
    const constraintBody = (name) => {
      const i = schemaCode.indexOf(`CONSTRAINT ${name}`);
      if (i < 0) return null;
      let j = schemaCode.indexOf('CONSTRAINT ', i + 1);
      const k = schemaCode.indexOf('\n);', i);
      if (j < 0 || (k >= 0 && k < j)) j = k;
      return schemaCode.slice(i, j < 0 ? i + 1500 : j);
    };
    const fields = (body, nullness) => [...new Set([...body.matchAll(new RegExp(`(\\w+)\\s+IS\\s+${nullness}`, 'gi'))].map((x) => x[1]))].sort();

    for (const [key, constraint] of [
      ['usage_rollup', 'observation_rollup_shape'],
      ['model_detection', 'observation_detection_shape'],
      ['prompt', 'observation_prompt_shape'],
      ['prompt:m0', 'observation_m0_carries_no_content'],
    ]) {
      const branch = branches.find((b) => b.key === key);
      const body = constraintBody(constraint);
      if (!branch || !body) {
        check(`kinds.${constraint}.matches-contract`, false, `${branch ? '' : `no contract branch ${key}; `}${body ? '' : `no CONSTRAINT ${constraint}`}`);
        continue;
      }
      const expected = (branch.then.not?.anyOf ?? []).map((r) => (r.required ?? [])[0]).filter((f) => f && !NO_COLUMN_ON_OBSERVATION.has(f)).sort();
      const actual = fields(body, 'NULL');
      const missing = expected.filter((f) => !actual.includes(f));
      const surplus = actual.filter((f) => !expected.includes(f));
      check(`kinds.${constraint}.matches-contract`, missing.length === 0 && surplus.length === 0,
        missing.length || surplus.length
          ? `${constraint} does not forbid [${missing.join(', ')}] and forbids extra [${surplus.join(', ')}] relative to the ${key} branch`
          : `${constraint} forbids exactly the ${expected.length} fields of the ${key} branch`);
    }

    {
      const branch = branches.find((b) => b.key === 'prompt:m1+');
      const body = constraintBody('observation_m1_plus_carries_labels');
      const expected = [...(branch?.then.required ?? [])].sort();
      const actual = body ? fields(body, 'NOT NULL') : [];
      check('kinds.observation_m1_plus_carries_labels.matches-contract',
        branch && body && JSON.stringify(expected) === JSON.stringify(actual),
        `contract requires [${expected.join(', ')}], the store requires [${actual.join(', ')}]`);
    }

    // The contract requires an excerpt at M2; the store permits but does not require one (ingest
    // enforces the requirement). Asserted, so the difference cannot change silently.
    {
      const branch = branches.find((b) => b.key === 'prompt:m2');
      const contractRequires = Boolean(branch?.then.required?.includes('content_excerpt'));
      const body = constraintBody('observation_excerpt_only_at_m2');
      const storeRequires = body ? /content_excerpt\s+IS\s+NOT\s+NULL/i.test(body) : false;
      check('kinds.m2-excerpt-permitted-not-required', body && contractRequires && !storeRequires,
        !body
          ? 'CONSTRAINT observation_excerpt_only_at_m2 not found'
          : !contractRequires
            ? 'the contract no longer requires content_excerpt at M2; update observation_excerpt_only_at_m2 and this check'
            : storeRequires
              ? 'the store now requires content_excerpt at M2; update this check'
              : 'the contract requires content_excerpt at M2 and the store permits it only there');
    }
  }
}

// -------------------------------------------------------------------------------------
// Report
// -------------------------------------------------------------------------------------

const failed = results.filter((r) => !r.ok);
const width = Math.max(...results.map((r) => r.id.length));
console.log('services/database/tools/check-schema.mjs: static checks of services/database/schema.sql');
console.log('');
for (const r of results) console.log(`  ${r.ok ? 'PASS' : 'FAIL'}  ${r.id.padEnd(width)}  ${r.detail}`);
console.log('');
console.log(`  ${results.length - failed.length} passed, ${failed.length} failed`);
if (failed.length > 0) {
  console.log('  RESULT: FAIL');
  process.exit(1);
}
console.log('  RESULT: PASS (run node services/database/tools/test-database.mjs to prove the invariants on a server)');
