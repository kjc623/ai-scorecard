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
import { dirname, join, resolve, sep } from 'node:path';

const HERE = dirname(fileURLToPath(import.meta.url));

// The tree to check. Normally the repository this file lives in. SAC_SCHEMA_ROOT points the
// checker at a fixture instead, which is what db/tools/check-schema.test.mjs uses to prove each
// check can FAIL: a checker that can only ever be run against a correct tree cannot show that it
// checks anything. Overriding the root changes only WHICH files are read, never how.
const REPO = process.env.SAC_SCHEMA_ROOT ? resolve(process.env.SAC_SCHEMA_ROOT) : join(HERE, '..', '..');
const DB = join(REPO, 'db');

if (!readable(join(DB, 'schema.sql'))) {
  console.error(`check-schema: no db/schema.sql under ${REPO} (set SAC_SCHEMA_ROOT to a fixture tree)`);
  process.exit(2);
}

function readable(p) {
  try { readFileSync(p); return true; } catch { return false; }
}

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

function readMaybe(path) {
  try {
    return readFileSync(path, 'utf8');
  } catch {
    return null;
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

// Every table outside `ref` must be covered by the RLS set, so a tenant-scoped table added later
// cannot quietly ship without a policy. `ref` is the deliberate exception: six tables of shared
// vocabulary (classifier_release, collector, data_class, retention_class, route_fidelity, rule),
// none of which has a tenant_id column -- verified against the live catalog -- and all of whose
// per-tenant variation lives in ops.* (policy_bundle, retention_policy, tool), which IS covered.
// This is the static half; the runtime half counts forced tables against the live catalog.
{
  const created = [...schemaCode.matchAll(/CREATE TABLE\s+([a-z_]+)\.([a-z_]+)/g)]
    .map((m) => `${m[1]}.${m[2]}`);
  const nonRef = [...new Set(created.filter((t) => !t.startsWith('ref.')))];
  const rlsCovered = new Set([...tenantTables, 'ops.tenant']);

  const missing = nonRef.filter((t) => !rlsCovered.has(t));   // created outside ref, no policy
  const extra = [...rlsCovered].filter((t) => !created.includes(t)); // policy for a table that is gone

  check('rls.every-non-ref-table-is-covered', missing.length === 0 && extra.length === 0,
    missing.length || extra.length
      ? `tables outside ref without a tenant_isolation policy: [${missing.join(', ')}]${extra.length ? `; policies for tables that do not exist: [${extra.join(', ')}]` : ''}`
      : `all ${nonRef.length} tables outside ref are in the RLS set; the ${created.length - nonRef.length} ref tables are the documented exception (shared vocabulary, no tenant_id column)`);
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
// 2b. Every digest-shaped column carries a format CHECK
// -------------------------------------------------------------------------------------
// A digest column with no format CHECK accepts any text at all, so a caller can store a value that
// is not a digest and nothing notices until something tries to verify against it -- and the value's
// whole purpose is to be verified against. ops.retrieval_grant.raw_digest was exactly that, found
// while checking content-vault's claim that it copies ops.content_object.ciphertext_sha256.
//
// Two columns are EXCUSED rather than tightened: no owner has stated that they are always sha256,
// and adding a restriction on a guess is the direction this schema deliberately avoids (the same
// reasoning that keeps the M2 excerpt a permission rather than a requirement). The excused set is
// asserted to be exactly these two, so it cannot grow silently and a stale entry fails.
const DIGEST_COLUMNS_EXCUSED = ['ops.policy_bundle.signed_digest', 'ref.classifier_release.artifact_digest'];

{
  // Each `CREATE TABLE` block, so a column can be attributed to its table.
  const tables = [...schemaCode.matchAll(/CREATE TABLE\s+([a-z_]+)\.([a-z_]+)\s*\(([\s\S]*?)\n\);/g)];
  const digestish = [];
  for (const [, schemaName, tableName, body] of tables) {
    for (const m of body.matchAll(/^\s{2}([a-z_][a-z0-9_]*)\s+(?:text|character varying)\b/gm)) {
      const col = m[1];
      if (!/(_digest|_sha256)$/.test(col) && col !== 'dedup_key' && col !== 'dedup_weak_key') continue;
      digestish.push({ qualified: `${schemaName}.${tableName}.${col}`, col });
    }
  }

  check('digest.columns-found', digestish.length >= 6,
    `${digestish.length} digest-shaped columns: ${digestish.map((d) => d.col).join(', ')}`);

  const unconstrained = digestish.filter(
    (d) => !new RegExp(`${d.col}\\s*~\\s*'\\^sha256:`).test(schemaCode),
  );
  const unconstrainedNames = unconstrained.map((d) => d.qualified);

  const notExcused = unconstrainedNames.filter((q) => !DIGEST_COLUMNS_EXCUSED.includes(q));
  const staleExemptions = DIGEST_COLUMNS_EXCUSED.filter((q) => !unconstrainedNames.includes(q));

  check('digest.every-column-format-checked-or-excused',
    notExcused.length === 0 && staleExemptions.length === 0,
    notExcused.length || staleExemptions.length
      ? `${notExcused.length ? `digest columns with no sha256 format CHECK and no recorded exemption: [${notExcused.join(', ')}]` : ''}` +
        `${staleExemptions.length ? `${notExcused.length ? '; ' : ''}exemptions recorded for columns that no longer need one: [${staleExemptions.join(', ')}]` : ''}`
      : `all ${digestish.length} digest-shaped columns carry a sha256 format CHECK, except the ${DIGEST_COLUMNS_EXCUSED.length} recorded as excused: ${DIGEST_COLUMNS_EXCUSED.join(', ')}`);
}

// -------------------------------------------------------------------------------------
// 3. Append-only / tamper-evidence triggers
// -------------------------------------------------------------------------------------

{
  // `\b` matters: without it `CREATE TRIGGER\s+audit_append_only` also matches a trigger named
  // `audit_append_only_moved`, so renaming a guard would have read as the guard still being there.
  // Found by db/tools/check-schema.test.mjs, which renames one and expects this check to fail.
  const required = [
    ['audit_append_only', /CREATE TRIGGER\s+audit_append_only\b/],
    ['audit_chain_before_insert', /CREATE TRIGGER\s+audit_chain_before_insert\b/],
    ['observation_append_only', /CREATE TRIGGER\s+observation_append_only\b/],
    ['policy_bundle_ceiling', /CREATE TRIGGER\s+policy_bundle_ceiling\b/],
  ];
  const missing = required.filter(([, re]) => !re.test(schemaCode)).map(([n]) => n);
  check('triggers.present', missing.length === 0,
    missing.length ? `missing triggers: ${missing.join(', ')}` : `all 4 triggers present: ${required.map(([n]) => n).join(', ')}`);

  // BOTH BEFORE UPDATE OR DELETE and BEFORE INSERT OR UPDATE OR DELETE forms must appear for
  // the append-only guarantee, and the guard functions must actually RAISE.
  check('triggers.audit-blocks-update-and-delete',
    /CREATE TRIGGER\s+audit_append_only\b[\s\S]{0,400}?BEFORE\s+(UPDATE\s+OR\s+DELETE|INSERT\s+OR\s+UPDATE\s+OR\s+DELETE|DELETE\s+OR\s+UPDATE)/.test(schemaCode),
    'audit_append_only fires BEFORE UPDATE OR DELETE');
  check('triggers.observation-blocks-mutation',
    /CREATE TRIGGER\s+observation_append_only\b[\s\S]{0,400}?BEFORE\s+(UPDATE\s+OR\s+DELETE|INSERT\s+OR\s+UPDATE\s+OR\s+DELETE|DELETE\s+OR\s+UPDATE)/.test(schemaCode),
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
  // Compare against what the documents actually claim, rather than a constant that is itself a
  // memory of them: a constant would agree with the file and miss the drift this exists to find.
  const repoRoot = join(DB, '..');
  const ONES = { one: 1, two: 2, three: 3, four: 4, five: 5, six: 6, seven: 7, eight: 8, nine: 9 };
  const TENS = { twenty: 20, thirty: 30, forty: 40, fifty: 50, sixty: 60, seventy: 70, eighty: 80, ninety: 90 };
  const wordToNum = (w) => {
    const p = w.toLowerCase().split(/[-\s]+/).filter(Boolean);
    if (p.length === 1) return TENS[p[0]] ?? ONES[p[0]] ?? null;
    if (p.length === 2 && TENS[p[0]] != null && ONES[p[1]] != null) return TENS[p[0]] + ONES[p[1]];
    return null;
  };

  const docFiles = ['README.md', 'docs/00-architecture.md', 'docs/03-data-platform.md', '.cockpit/project.json'];
  const claims = [];
  for (const rel of docFiles) {
    const txt = readMaybe(join(repoRoot, rel.replace(/\//g, sep)));
    if (!txt) continue;
    for (const mm of txt.matchAll(/(\d+)\s+assertions|([A-Za-z]+(?:-[A-Za-z]+)?)\s+assertions/g)) {
      const n = mm[1] != null ? Number(mm[1]) : wordToNum(mm[2]);
      if (n != null) claims.push({ file: rel, n });
    }
  }

  const agree = claims.filter((c) => c.n === ids.length);
  const disagree = claims.filter((c) => c.n !== ids.length);
  warn('tests.count-matches-docs', disagree.length === 0,
    disagree.length === 0
      ? `all ${claims.length} documented claims match the file: ${ids.length} assertions (T1..T${Math.max(...ids)})`
      : `DOC DRIFT: the file contains ${ids.length} assertions (T1..T${Math.max(...ids)}), but ${disagree.map((c) => `${c.file} claims ${c.n}`).join('; ')}. ${agree.length} other claim(s) already agree. The SQL is the artifact under test; fix the prose, not the file.`);
}

// -------------------------------------------------------------------------------------
// 7. The quarantine reason-code vocabulary, checked across the SQL/Go seam
// -------------------------------------------------------------------------------------
// The ruling recorded in db/schema.sql: ingest.rejected.reason_code carries the docs/02 section 7
// wire vocabulary, spelled as device/protocol.ReasonCode spells it, plus three storage-only codes.
// Two wire codes are deliberately unrepresentable in quarantine.
//
// This checks the seam from both ends, because the failure it guards against is an ORDERING
// hazard rather than a syntax error: if the CHECK is narrowed before the Go mapping is changed,
// every quarantine for the renamed codes starts failing at runtime, and nothing static notices.
// The Go unit test proves the mapping is total over the wire enum; this proves that everything
// the mapping can emit is something the database will actually accept.

const WIRE_ONLY_EXPECTED = ['tenant_mismatch', 'duplicate_batch'];
const STORAGE_ONLY_EXPECTED = ['malformed_json', 'dedup_key_mismatch', 'internal_error'];

{
  const repoRoot = join(DB, '..');
  const readMaybe = (p) => { try { return readFileSync(p, 'utf8'); } catch { return null; } };

  const protocolSrc = readMaybe(join(repoRoot, 'device', 'protocol', 'batch.go'));
  const storeSrc = readMaybe(join(repoRoot, 'services', 'ingest-api', 'internal', 'store', 'store.go'));

  // --- the SQL side ---
  const m = schemaCode.match(/reason_code\s+text\s+NOT NULL\s+CHECK\s*\(\s*reason_code\s+IN\s*\(([\s\S]*?)\)\)/);
  check('vocab.check-found', Boolean(m),
    m ? 'ingest.rejected.reason_code CHECK located in schema.sql' : 'could not locate the reason_code CHECK');
  const sqlCodes = m ? [...m[1].matchAll(/'([a-z_]+)'/g)].map((x) => x[1]) : [];
  const sqlSet = new Set(sqlCodes);
  check('vocab.check-no-duplicates', sqlSet.size === sqlCodes.length,
    sqlSet.size === sqlCodes.length ? `${sqlCodes.length} codes, no duplicates` : 'duplicate codes in the CHECK');

  // --- the Go side ---
  if (!protocolSrc || !storeSrc) {
    warn('vocab.seam-checkable', false,
      `cannot check the SQL/Go seam: ${!protocolSrc ? 'device/protocol/batch.go ' : ''}${!storeSrc ? 'services/ingest-api/internal/store/store.go ' : ''}not readable. The vocabulary check below is INCOMPLETE.`);
  } else {
    const wireEnum = [...strip(protocolSrc).matchAll(/(Reason\w+)\s+ReasonCode\s*=\s*"([a-z_]+)"/g)]
      .map((x) => ({ name: x[1], code: x[2] }));
    check('vocab.wire-enum-found', wireEnum.length > 0,
      `${wireEnum.length} wire codes in device/protocol.ReasonCode`);

    const fnStart = storeSrc.indexOf('func QuarantineReason(');
    const fnBody = fnStart >= 0 ? storeSrc.slice(fnStart, storeSrc.indexOf('\n}', fnStart)) : '';
    check('vocab.mapping-found', fnStart >= 0 && fnBody.length > 0,
      fnStart >= 0 ? 'QuarantineReason located' : 'QuarantineReason not found in store.go');

    // Each case body is exactly one `return "code", true|false`, so match the pair directly.
    // (A lazy `[\s\S]*?` between case and return would let one case span into later ones and
    // report a mapped code as unmapped.)
    const cases = [...fnBody.matchAll(/case\s+protocol\.(Reason\w+)\s*:\s*return\s+"([a-z_]*)"\s*,\s*(true|false)/g)]
      .map((x) => ({ name: x[1], code: x[2], mapped: x[3] === 'true' }));
    const cased = new Set(cases.map((c) => c.name));
    const emitted = cases.filter((c) => c.mapped).map((c) => c.code);
    const unmapped = cases.filter((c) => !c.mapped).map((c) => c.name);

    check('vocab.mapping-cases-parsed', cases.length > 0,
      `${cases.length} case arms parsed from QuarantineReason (${emitted.length} mapped, ${unmapped.length} deliberately unmapped)`);

    // Every wire code must be handled by name. A code that falls through to the trailing
    // `return "", false` would silently lose its quarantine row instead of failing loudly.
    const unhandled = wireEnum.filter((w) => !cased.has(w.name));
    check('vocab.mapping-is-exhaustive-over-wire-enum', unhandled.length === 0,
      unhandled.length
        ? `wire codes with no case in QuarantineReason (they fall through to the default and lose their quarantine row): ${unhandled.map((u) => u.name).join(', ')}`
        : `all ${wireEnum.length} wire codes are handled explicitly`);

    // THE key check: everything the mapping can emit must be accepted by the CHECK.
    const notAccepted = emitted.filter((c) => !sqlSet.has(c));
    check('vocab.every-emitted-code-is-accepted-by-the-check', notAccepted.length === 0,
      notAccepted.length
        ? `QuarantineReason can emit codes the CHECK rejects, so those quarantines fail at runtime: ${notAccepted.join(', ')}`
        : `all ${emitted.length} emitted codes are accepted by the CHECK`);

    // The deliberately unmapped set must be exactly the documented pair, by wire code.
    const unmappedCodes = unmapped
      .map((n) => (wireEnum.find((w) => w.name === n) || {}).code)
      .filter(Boolean)
      .sort();
    check('vocab.unmapped-are-exactly-the-documented-wire-only-pair',
      JSON.stringify(unmappedCodes) === JSON.stringify([...WIRE_ONLY_EXPECTED].sort()),
      JSON.stringify(unmappedCodes) === JSON.stringify([...WIRE_ONLY_EXPECTED].sort())
        ? `unrepresentable in quarantine, as documented: ${unmappedCodes.join(', ')}`
        : `unmapped wire codes are ${unmappedCodes.join(', ') || '(none)'}, expected exactly ${[...WIRE_ONLY_EXPECTED].sort().join(', ')}`);

    // The CHECK's set must be exactly: wire codes minus the wire-only pair, plus the storage-only
    // codes. This catches both a removal and a silent addition.
    const wireOnlySet = new Set(WIRE_ONLY_EXPECTED);
    const expectedSql = new Set([
      ...wireEnum.map((w) => w.code).filter((c) => !wireOnlySet.has(c)),
      ...STORAGE_ONLY_EXPECTED,
    ]);
    const missing = [...expectedSql].filter((c) => !sqlSet.has(c));
    const extra = [...sqlSet].filter((c) => !expectedSql.has(c));
    check('vocab.check-set-is-exact', missing.length === 0 && extra.length === 0,
      missing.length || extra.length
        ? `CHECK vocabulary differs from (wire codes minus ${WIRE_ONLY_EXPECTED.join('/')}) plus ${STORAGE_ONLY_EXPECTED.join('/')}: missing [${missing.join(', ')}], unexpected [${extra.join(', ')}]`
        : `CHECK accepts exactly ${sqlSet.size} codes: 8 wire codes plus ${STORAGE_ONLY_EXPECTED.join(', ')}`);

    // The three renamed spellings must be gone from the SQL, so the rename cannot half-revert.
    const oldSpellings = ['device_revoked', 'batch_oversize', 'schema_version_unsupported'];
    const resurrected = oldSpellings.filter((c) => sqlSet.has(c));
    check('vocab.pre-alignment-spellings-absent', resurrected.length === 0,
      resurrected.length ? `pre-alignment spellings are back in the CHECK: ${resurrected.join(', ')}` : `none of the three pre-alignment spellings remain: ${oldSpellings.join(', ')}`);
  }
}

// -------------------------------------------------------------------------------------
// 8. The store's kind and mode boundaries, compared against the contract mechanically
// -------------------------------------------------------------------------------------
// The defect this exists to catch: the store's per-kind CHECK constraints constrained only SOME
// of the fields the contract forbids for that kind, so a record arriving by any path other than
// the ingest gate was accepted carrying a field its kind is defined not to have. Reading the two
// side by side is what a reviewer does once; comparing them mechanically is what catches the next
// drift. Same independent-artefact comparison that caught the reason-code regression.
//
// contracts/event-envelope.schema.json is authoritative. A contract field with no column here is
// excused EXPLICITLY and named in the output, so a field cannot be dropped from the store's list
// silently: either it has no column and is listed below, or this check fails.

// Contract fields with no column on ingest.observation, so no CHECK on that table can mention them.
const NO_COLUMN_ON_OBSERVATION = new Set(['attachments']);

{
  const repoDir = join(DB, '..');
  const contractPath = join(repoDir, 'contracts', 'event-envelope.schema.json');
  const contractText = readMaybe(contractPath);

  if (!contractText) {
    warn('kinds.contract-readable', false, `cannot read ${contractPath}; the contract-vs-store comparison is INCOMPLETE`);
  } else {
    let contract = null;
    try {
      contract = JSON.parse(contractText);
      check('kinds.contract-parses', true, 'contracts/event-envelope.schema.json parses');
    } catch (err) {
      check('kinds.contract-parses', false, `contract is not valid JSON: ${err.message}`);
    }

    if (contract) {
      // Every if/then branch keyed on `kind` (optionally narrowed by `collection_mode`).
      const branches = [];
      const walk = (node) => {
        if (Array.isArray(node)) { node.forEach(walk); return; }
        if (node && typeof node === 'object') {
          const k = node.if && node.if.properties && node.if.properties.kind && node.if.properties.kind.const;
          if (k && node.then) {
            const modeConst = node.if.properties.collection_mode && node.if.properties.collection_mode.const;
            const modeEnum = node.if.properties.collection_mode && node.if.properties.collection_mode.enum;
            branches.push({ key: k + (modeConst ? `:${modeConst}` : modeEnum ? ':m1+' : ''), then: node.then });
          }
          Object.values(node).forEach(walk);
        }
      };
      walk(contract);
      check('kinds.branches-found', branches.length >= 6,
        `${branches.length} contract kind/mode branches: ${branches.map((b) => b.key).join(', ')}`);

      // One CHECK constraint's body out of schema.sql: from its CONSTRAINT keyword to the next
      // CONSTRAINT (or the end of the table definition).
      const constraintBody = (name) => {
        const i = schemaCode.indexOf(`CONSTRAINT ${name}`);
        if (i < 0) return null;
        let j = schemaCode.indexOf('CONSTRAINT ', i + 1);
        const k = schemaCode.indexOf('\n);', i);
        if (j < 0 || (k >= 0 && k < j)) j = k;
        if (j < 0) j = i + 1500;
        return schemaCode.slice(i, j);
      };
      const sqlFields = (body, negated) => body
        ? [...body.matchAll(negated ? /(\w+)\s+IS\s+NULL/gi : /(\w+)\s+IS\s+NOT\s+NULL/gi)].map((m) => m[1])
        : [];

      // The four branches that FORBID a field set: contract not.anyOf vs the CHECK's IS NULL list.
      const forbidMap = [
        ['usage_rollup', 'observation_rollup_shape'],
        ['model_detection', 'observation_detection_shape'],
        ['prompt', 'observation_prompt_shape'],
        ['prompt:m0', 'observation_m0_carries_no_content'],
      ];

      for (const [branchKey, constraintName] of forbidMap) {
        const branch = branches.find((b) => b.key === branchKey);
        const body = constraintBody(constraintName);
        if (!branch) { check(`kinds.${constraintName}.contract-branch`, false, `no contract branch for ${branchKey}`); continue; }
        if (!body) { check(`kinds.${constraintName}.exists`, false, `CONSTRAINT ${constraintName} not found in schema.sql`); continue; }

        const contractForbids = (branch.then.not && branch.then.not.anyOf ? branch.then.not.anyOf : [])
          .map((r) => (r.required || [])[0]).filter(Boolean).sort();
        const excused = contractForbids.filter((f) => NO_COLUMN_ON_OBSERVATION.has(f));
        const expected = contractForbids.filter((f) => !NO_COLUMN_ON_OBSERVATION.has(f)).sort();
        const actual = [...new Set(sqlFields(body, true))].sort();

        const missing = expected.filter((f) => !actual.includes(f));   // store does not forbid it
        const surplus = actual.filter((f) => !expected.includes(f));   // store forbids something extra

        check(`kinds.${constraintName}.matches-contract`, missing.length === 0 && surplus.length === 0,
          missing.length || surplus.length
            ? `${constraintName} differs from the contract's ${branchKey} branch: the store does NOT forbid [${missing.join(', ')}]${surplus.length ? `, and forbids extra [${surplus.join(', ')}]` : ''}`
            : `${constraintName} forbids exactly the contract's ${expected.length} fields${excused.length ? ` (${excused.join(', ')} excused: no column on this table)` : ''}`);
      }

      // The M1+ branch REQUIRES a field set: contract then.required vs the CHECK's IS NOT NULL list.
      {
        const branch = branches.find((b) => b.key === 'prompt:m1+');
        const body = constraintBody('observation_m1_plus_carries_labels');
        if (branch && body) {
          const expected = [...(branch.then.required || [])].sort();
          const actual = [...new Set(sqlFields(body, false))].sort();
          const same = JSON.stringify(expected) === JSON.stringify(actual);
          check('kinds.observation_m1_plus_carries_labels.matches-contract', same,
            same
              ? `requires exactly the contract's ${expected.length} fields: ${expected.join(', ')}`
              : `M1+ requires [${expected.join(', ')}] but the store requires [${actual.join(', ')}]`);
        }
      }

      // The M2 branch REQUIRES an excerpt; the store only PERMITS one there. That difference is a
      // recorded decision, not an open question: the requirement is a wire-level statement
      // enforced at ingest (mode_violation + quarantine), and "M2 without an excerpt" is a less
      // informative record rather than a dangerous one, so the store deliberately does not add a
      // requirement that could invalidate a previously-valid row.
      //
      // The exemption is ASSERTED, not assumed: this passes while the world matches the recorded
      // decision and fails the moment it stops matching, so the exemption cannot rot into a hole.
      // Same treatment `attachments` gets, and for the same reason.
      {
        const branch = branches.find((b) => b.key === 'prompt:m2');
        const requiresExcerpt = Boolean(branch && branch.then.required && branch.then.required.includes('content_excerpt'));
        const body = constraintBody('observation_excerpt_only_at_m2');
        const storeRequiresIt = body ? /content_excerpt\s+IS\s+NOT\s+NULL/i.test(body) : false;

        check('kinds.m2-excerpt-is-an-excused-difference', Boolean(body) && requiresExcerpt && !storeRequiresIt,
          !body
            ? 'CONSTRAINT observation_excerpt_only_at_m2 not found in schema.sql, so the recorded exemption cannot be checked'
            : !requiresExcerpt
              ? 'the contract no longer requires content_excerpt at M2, so the exemption recorded at observation_excerpt_only_at_m2 is STALE and should be removed'
              : storeRequiresIt
                ? 'the store now requires content_excerpt at M2, so the exemption recorded at observation_excerpt_only_at_m2 is STALE'
                : 'EXCUSED BY DECISION: the contract requires content_excerpt at M2, the store only permits it. Enforced at ingest as mode_violation + quarantine; a less informative record, not a dangerous one. Recorded at the constraint.');
      }
    }
  }
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
