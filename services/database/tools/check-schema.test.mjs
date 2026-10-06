// Proves each family of check-schema.mjs can fail: `node --test services/database/tools/`.
//
// Every negative case copies the checker's inputs into a temporary tree, makes one deliberate change,
// and asserts that the checker exits 1 and names the check that should catch it. The baseline asserts
// the unchanged copy passes, so a checker that failed everything could not pass either. The shipped
// script runs as a subprocess with SAC_SCHEMA_ROOT pointing at the copy; the repository is never
// written.

import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { tmpdir } from 'node:os';

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = join(HERE, '..', '..', '..');
const CHECKER = join(HERE, 'check-schema.mjs');

const INPUTS = [
  'services/database/schema.sql',
  'services/database/invariants.test.sql',
  'contracts/event-envelope.schema.json',
  'endpoint/protocol/batch.go',
  'services/ingest-api/internal/ingest/service.go',
];

const at = (root, rel) => join(root, ...rel.split('/'));

let root;

before(() => {
  root = mkdtempSync(join(tmpdir(), 'sac-schema-fixture-'));
  for (const rel of INPUTS) {
    mkdirSync(dirname(at(root, rel)), { recursive: true });
    cpSync(at(REPO, rel), at(root, rel));
  }
});

after(() => {
  if (root) rmSync(root, { recursive: true, force: true });
});

function runChecker(env = {}) {
  const r = spawnSync(process.execPath, [CHECKER], {
    env: { ...process.env, SAC_SCHEMA_ROOT: root, ...env },
    encoding: 'utf8',
  });
  const out = `${r.stdout ?? ''}${r.stderr ?? ''}`;
  const checks = new Map();
  for (const line of out.split(/\r?\n/)) {
    const m = /^\s{2}(PASS|FAIL)\s{2}(\S+)\s+(.*)$/.exec(line);
    if (m) checks.set(m[2], { status: m[1], detail: m[3] });
  }
  return { code: r.status, out, checks };
}

/** Perturb one fixture file, expect one of `ids` to FAIL and the run to exit 1, then restore. */
function expectCaught(label, rel, edit, ids) {
  test(label, () => {
    const file = at(root, rel);
    const original = readFileSync(file, 'utf8');
    const changed = edit(original);
    assert.notEqual(changed, original, `the perturbation of ${rel} changed nothing`);
    writeFileSync(file, changed);
    try {
      const r = runChecker();
      assert.equal(r.code, 1, `expected exit 1 after perturbing ${rel}\n${r.out}`);
      const want = Array.isArray(ids) ? ids : [ids];
      assert.ok(want.some((id) => r.checks.get(id)?.status === 'FAIL'),
        `expected one of [${want.join(', ')}] to FAIL; got ${want.map((id) => `${id}=${r.checks.get(id)?.status ?? 'absent'}`).join(', ')}`);
    } finally {
      writeFileSync(file, original);
    }
  });
}

test('baseline: the unchanged tree passes a real number of checks', () => {
  const r = runChecker();
  assert.equal(r.code, 0, r.out);
  assert.ok(r.checks.size >= 55, `expected at least 55 checks, parsed ${r.checks.size}`);
  for (const [id, { status }] of r.checks) assert.equal(status, 'PASS', `${id} should pass on the real tree`);
});

test('an empty tree exits 2: the checker could not run, which is not a pass', () => {
  const empty = mkdtempSync(join(tmpdir(), 'sac-empty-'));
  try {
    const r = runChecker({ SAC_SCHEMA_ROOT: empty });
    assert.equal(r.code, 2);
    assert.match(r.out, /no services\/database\/schema\.sql/);
  } finally {
    rmSync(empty, { recursive: true, force: true });
  }
});

// Row-level security
expectCaught('rls: a table outside ref with no policy',
  'services/database/schema.sql', (s) => `${s}\nCREATE TABLE ops.orphan_probe (\n  tenant_id uuid NOT NULL\n);\n`,
  'rls.every-non-ref-table-is-covered');
expectCaught('rls: FORCE dropped from the loop',
  'services/database/schema.sql', (s) => s.replace("    EXECUTE format('ALTER TABLE %s FORCE ROW LEVEL SECURITY', t);\n", ''),
  ['rls.loop.force', 'rls.enable-force-paired']);
expectCaught('rls: a policy that stops comparing with the session tenant',
  'services/database/schema.sql', (s) => s.replace('USING (tenant_id = ops.current_tenant())', 'USING (true)'),
  'rls.loop.using-session-tenant');
expectCaught('rls: ops.content left out of the loop',
  'services/database/schema.sql', (s) => s.replace("'ops.grant', 'ops.content', ", "'ops.grant', "),
  'rls.every-non-ref-table-is-covered');
expectCaught('rls: the pre-tenant sign-in table losing its policy',
  'services/database/schema.sql', (s) => s.replace('CREATE POLICY control_plane_only ON ops.auth_signin', 'CREATE POLICY control_plane_only ON ops.auth_signin_moved'),
  'rls.pre-tenant.ops.auth_signin.policy');
expectCaught('rls: a pre-tenant table that grows a tenant_id',
  'services/database/schema.sql', (s) => s.replace('  connection_id      uuid REFERENCES ops.identity_connection(connection_id),',
    '  tenant_id          uuid,\n  connection_id      uuid REFERENCES ops.identity_connection(connection_id),'),
  'rls.pre-tenant.ops.auth_signin.has-no-tenant');

// Dedup ladder
expectCaught('dedup: the weak-tier index renamed',
  'services/database/schema.sql', (s) => s.replace('submission_weak_key_uniq', 'submission_weak_key_moved'),
  'dedup.weak-index.exists');
expectCaught('dedup: the weak predicate widened so the tiers overlap',
  'services/database/schema.sql', (s) => s.replace('ON ingest.submission (tenant_id, dedup_weak_key)\n  WHERE dedup_key IS NULL;',
    'ON ingest.submission (tenant_id, dedup_weak_key)\n  WHERE dedup_key IS NOT NULL;'),
  'dedup.tiers-are-disjoint');

// Digests
expectCaught('digests: a format CHECK replaced by a weaker one',
  'services/database/schema.sql', (s) => s.replace("CHECK (raw_digest ~ '^sha256:[0-9a-f]{64}$'),\n  retention_class",
    'CHECK (length(raw_digest) > 0),\n  retention_class'),
  'digest.every-column-format-checked');
expectCaught('digests: a new digest column without a format CHECK',
  'services/database/schema.sql', (s) => s.replace('  raw_digest      text NOT NULL,', '  raw_digest      text NOT NULL,\n  proof_digest    text NOT NULL,'),
  'digest.every-column-format-checked');

// Triggers
expectCaught('triggers: the audit append-only trigger renamed',
  'services/database/schema.sql', (s) => s.replace('CREATE TRIGGER audit_append_only', 'CREATE TRIGGER audit_append_only_moved'),
  'triggers.present');
expectCaught('triggers: the observation guard firing on the wrong event',
  'services/database/schema.sql', (s) => s.replace('CREATE TRIGGER observation_append_only\n  BEFORE UPDATE OR DELETE ON ingest.observation',
    'CREATE TRIGGER observation_append_only\n  BEFORE INSERT ON ingest.observation'),
  'triggers.observation-blocks-mutation');

// Roles and grants
expectCaught('roles: a role that can log in',
  'services/database/schema.sql', (s) => s.replace('CREATE ROLE sac_ingest   NOLOGIN;', 'CREATE ROLE sac_ingest   LOGIN;'),
  'roles.no-login-no-bypass');
expectCaught('roles: a BYPASSRLS role added',
  'services/database/schema.sql', (s) => s.replace('CREATE ROLE sac_ops      NOLOGIN;', 'CREATE ROLE sac_ops      NOLOGIN;\nCREATE ROLE sac_owner NOLOGIN BYPASSRLS;'),
  ['roles.exactly-the-expected-set', 'roles.no-login-no-bypass']);
expectCaught('grants: UPDATE on the audit log',
  'services/database/schema.sql', (s) => `${s}\nGRANT UPDATE ON ops.audit TO sac_query;\n`,
  'grants.audit-is-append-only');
expectCaught('grants: query-api reading stored content',
  'services/database/schema.sql', (s) => `${s}\nGRANT SELECT ON ops.content TO sac_query;\n`,
  'grants.ops.content.only-vault-and-jobs');
expectCaught('grants: the expire job reading ciphertext',
  'services/database/schema.sql', (s) => s.replace('GRANT SELECT (tenant_id, object_id, submission_id, expires_at), DELETE ON ops.content TO sac_ops;',
    'GRANT SELECT (tenant_id, object_id, submission_id, expires_at, ciphertext), DELETE ON ops.content TO sac_ops;'),
  'grants.ops.content.jobs-delete-without-reading');
expectCaught('grants: the expire job reading indexed prompt text',
  'services/database/schema.sql', (s) => s.replace('GRANT SELECT (tenant_id, submission_id, unit_kind, unit_index, expires_at), DELETE',
    'GRANT SELECT, DELETE'),
  'grants.ingest.search_text.jobs-delete-without-reading');

// SECURITY DEFINER
expectCaught('secdef: a definer without a pinned search_path',
  'services/database/schema.sql', (s) => `${s}\nCREATE FUNCTION ops.probe_definer() RETURNS void\nLANGUAGE plpgsql SECURITY DEFINER AS $$ BEGIN RETURN; END $$;\n`,
  'secdef.pinned-search-path');
expectCaught('secdef: a definer left executable by PUBLIC',
  'services/database/schema.sql', (s) => s.replace('      ops.tenant_for_scim_token(text), ops.tenant_ids()\n  FROM PUBLIC;', '      ops.tenant_for_scim_token(text)\n  FROM PUBLIC;'),
  'secdef.execute-revoked-from-public');
expectCaught('secdef: a definer left with the role that applied the schema',
  'services/database/schema.sql', (s) => s.replace('ALTER FUNCTION ops.auth_session_by_hash(bytea)          OWNER TO sac_resolver;\n', ''),
  'secdef.owned-by-resolver');
expectCaught('secdef: ownership moved before EXECUTE is revoked',
  'services/database/schema.sql', (s) => {
    const start = s.indexOf('-- Functions are executable by PUBLIC unless revoked');
    const mid = s.indexOf('-- Hand the lookups to sac_resolver.');
    const end = s.indexOf('REVOKE sac_resolver FROM CURRENT_USER;') + 'REVOKE sac_resolver FROM CURRENT_USER;\n'.length;
    return s.slice(0, start) + s.slice(mid, end) + '\n' + s.slice(start, mid) + s.slice(end);
  },
  'secdef.privileges-set-before-handover');
expectCaught('secdef: sac_resolver keeping its CREATE on ops',
  'services/database/schema.sql', (s) => s.replace('REVOKE CREATE ON SCHEMA ops FROM sac_resolver;\n', ''),
  'secdef.handover-revoked');

// Expiry indexes
expectCaught('expiry: a swept table without its expiry index',
  'services/database/schema.sql', (s) => s.replace('CREATE INDEX rejected_expiry ON ingest.rejected (tenant_id, expires_at);', ''),
  'expiry.indexes');

// The test file
expectCaught('tests: a gap in the assertion numbering',
  'services/database/invariants.test.sql', (s) => s.replaceAll('PASS T47 ', 'PASS T71 '),
  'tests.contiguous-ids');

// Quarantine reason codes against ingest-api
expectCaught('vocab: ingest-api quarantining a code the CHECK refuses',
  'services/ingest-api/internal/ingest/service.go', (s) => s.replace('return reject(protocol.ReasonUnknownKind,', 'return reject(protocol.ReasonOversize,'),
  'vocab.check-equals-quarantined-codes');
expectCaught('vocab: an extra code in the CHECK',
  'services/database/schema.sql', (s) => s.replace("'mode_violation')),", "'mode_violation','oversize')),"),
  'vocab.check-equals-quarantined-codes');

// The envelope contract
expectCaught('kinds: a constraint that stops forbidding a contract-forbidden field',
  'services/database/schema.sql', (s) => s.replace(
    '      AND window_start IS NULL AND window_end IS NULL AND submission_count IS NULL\n      AND bytes_total IS NULL AND prompt_kind IS NULL))\n);',
    '      AND window_end IS NULL AND submission_count IS NULL\n      AND bytes_total IS NULL AND prompt_kind IS NULL))\n);'),
  'kinds.observation_detection_shape.matches-contract');
expectCaught('kinds: the M0 constraint losing `confidence`',
  'services/database/schema.sql', (s) => s.replace('      AND confidence IS NULL AND content_excerpt IS NULL AND prompt_kind IS NULL)),',
    '      AND content_excerpt IS NULL AND prompt_kind IS NULL)),'),
  'kinds.observation_m0_carries_no_content.matches-contract');
expectCaught('kinds: the store starting to require the M2 excerpt',
  'services/database/schema.sql', (s) => s.replace("    CHECK (content_excerpt IS NULL OR (kind = 'prompt' AND collection_mode = 'm2')),",
    "    CHECK (kind <> 'prompt' OR collection_mode <> 'm2' OR content_excerpt IS NOT NULL),"),
  'kinds.m2-excerpt-permitted-not-required');
