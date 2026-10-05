// =====================================================================================
// database/tools/check-schema.test.mjs -- proves each family of check-schema.mjs can FAIL
// =====================================================================================
// Run: node --test database/tools/     (or: node tools/verify-all.mjs --only node)
//
// WHY THIS EXISTS
// ---------------
// A checker that is only ever run against a correct tree cannot show that it checks anything.
// Every negative case below builds a fixture tree from the real repository, makes ONE deliberate
// change, and asserts that the checker exits 1 and names the specific check that should have
// caught it. The baseline case asserts the unperturbed fixture passes, so a checker that failed
// everything would not sneak through either.
//
// The checker is run as a subprocess with SAC_SCHEMA_ROOT pointing at the fixture, so these tests
// exercise the shipped script rather than a copy of its logic. Nothing here needs a live server:
// check-schema.mjs is static, and that is exactly the gap it is labelled as covering (see the
// "Not covered here" note at the bottom).
// =====================================================================================

import { test, before, after } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { cpSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join, sep } from 'node:path';
import { fileURLToPath } from 'node:url';
import { tmpdir } from 'node:os';

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = join(HERE, '..', '..');
const CHECKER = join(HERE, 'check-schema.mjs');

// The inputs check-schema.mjs reads. Tests never touch the real repository: they perturb copies.
const INPUTS = [
  'database/schema.sql',
  'database/invariants.test.sql',
  'contracts/event-envelope.schema.json',
  'ingestion/ingest-api/internal/store/store.go',
  'endpoint/protocol/batch.go',
  'README.md',
  'docs/00-architecture.md',
  'docs/03-data-platform.md',
  '.cockpit/project.json',
];

const p = (rel) => join(REPO, ...rel.split('/'));
const q = (root, rel) => join(root, ...rel.split('/'));

let root;

before(() => {
  root = mkdtempSync(join(tmpdir(), 'sac-schema-fixture-'));
  for (const rel of INPUTS) {
    const dest = q(root, rel);
    mkdirSync(dirname(dest), { recursive: true });
    cpSync(p(rel), dest);
  }
});

after(() => {
  if (root) rmSync(root, { recursive: true, force: true });
});

/** Run the shipped checker against the fixture tree. */
function runChecker(env = {}) {
  const r = spawnSync(process.execPath, [CHECKER], {
    env: { ...process.env, SAC_SCHEMA_ROOT: root, ...env },
    encoding: 'utf8',
  });
  const out = `${r.stdout ?? ''}${r.stderr ?? ''}`;
  const checks = new Map();
  for (const line of out.split(/\r?\n/)) {
    // "  PASS  some.check.id<pad>  detail"
    const m = /^\s{2}(PASS|FAIL|WARN)\s{2,}(\S+)\s{2,}(.*)$/.exec(line);
    if (m) checks.set(m[2], { status: m[1], detail: m[3] });
  }
  return { code: r.status, out, checks };
}

/** Apply an edit to a fixture file, returning a restore function. */
function perturb(rel, edit) {
  const file = q(root, rel);
  const original = readFileSync(file, 'utf8');
  const next = edit(original);
  assert.notEqual(next, original, `the perturbation to ${rel} changed nothing -- the test would prove nothing`);
  writeFileSync(file, next);
  return () => writeFileSync(file, original);
}

/**
 * The workhorse: perturb, expect a named check to FAIL and the run to exit 1, then restore.
 * `id` may be a string or an array of acceptable ids.
 */
function expectCaught(label, rel, edit, id) {
  test(label, () => {
    const ids = Array.isArray(id) ? id : [id];
    const restore = perturb(rel, edit);
    try {
      const r = runChecker();
      assert.equal(r.code, 1, `expected exit 1 after perturbing ${rel}, got ${r.code}\n${r.out}`);
      const hit = ids.find((i) => r.checks.get(i)?.status === 'FAIL');
      assert.ok(
        hit,
        `expected one of [${ids.join(', ')}] to FAIL; statuses were: ` +
        ids.map((i) => `${i}=${r.checks.get(i)?.status ?? 'absent'}`).join(', '),
      );
    } finally {
      restore();
    }
  });
}

// -------------------------------------------------------------------------------------
// Baseline: the unperturbed fixture must pass, and must actually be checking things
// -------------------------------------------------------------------------------------

test('baseline: the unperturbed fixture passes, and the checker is checking a real number of properties', () => {
  const r = runChecker();
  assert.equal(r.code, 0, `expected exit 0 on the unperturbed tree\n${r.out}`);
  assert.ok(r.checks.size >= 70, `expected at least 70 checks, parsed ${r.checks.size}`);
  for (const id of [
    'rls.every-non-ref-table-is-covered',
    'dedup.weak-index.exists',
    'triggers.present',
    'secdef.pinned-search-path',
    'vocab.every-emitted-code-is-accepted-by-the-check',
    'vocab.mapping-is-exhaustive-over-wire-enum',
    'kinds.observation_detection_shape.matches-contract',
    'kinds.observation_m0_carries_no_content.matches-contract',
  ]) {
    assert.equal(r.checks.get(id)?.status, 'PASS', `${id} should PASS on the real tree`);
  }
});

test('checker cannot run: an empty root exits 2 rather than reporting a vacuous pass', () => {
  const empty = mkdtempSync(join(tmpdir(), 'sac-empty-'));
  try {
    const r = runChecker({ SAC_SCHEMA_ROOT: empty });
    assert.equal(r.code, 2, 'a missing database/schema.sql must be "could not run", not "passed"');
    assert.match(r.out, /no database\/schema\.sql/, 'the error should say what is missing');
  } finally {
    rmSync(empty, { recursive: true, force: true });
  }
});

// -------------------------------------------------------------------------------------
// RLS family
// -------------------------------------------------------------------------------------

expectCaught(
  'rls: a table created outside ref with no policy is caught',
  'database/schema.sql',
  (s) => `${s}\nCREATE TABLE ops.orphan_probe (tenant_id uuid NOT NULL);\n`,
  'rls.every-non-ref-table-is-covered',
);

expectCaught(
  'rls: dropping FORCE from the loop is caught',
  'database/schema.sql',
  (s) => s.replace("    EXECUTE format('ALTER TABLE %s FORCE ROW LEVEL SECURITY', t);\n", ''),
  ['rls.loop.force', 'rls.enable-force-paired'],
);

expectCaught(
  'rls: a policy that stops comparing against the session tenant is caught',
  'database/schema.sql',
  (s) => s.replace('USING (tenant_id = ops.current_tenant())', 'USING (true)'),
  'rls.loop.using-session-tenant',
);

expectCaught(
  'rls: the pre-tenant sign-in table losing its policy is caught',
  'database/schema.sql',
  (s) => s.replace('CREATE POLICY control_plane_only ON ops.auth_signin TO sac_control',
    'CREATE POLICY control_plane_only ON ops.auth_signin_moved TO sac_control'),
  'rls.pre-tenant.ops.auth_signin.policy',
);

expectCaught(
  'rls: a pre-tenant table that grows a tenant_id is caught, because it then belongs in the loop',
  'database/schema.sql',
  (s) => s.replace('  connection_id      uuid REFERENCES ops.identity_connection(connection_id),',
    '  tenant_id          uuid,\n  connection_id      uuid REFERENCES ops.identity_connection(connection_id),'),
  'rls.pre-tenant.ops.auth_signin.has-no-tenant',
);

// -------------------------------------------------------------------------------------
// Dedup family
// -------------------------------------------------------------------------------------

expectCaught(
  'dedup: renaming the weak-tier partial unique index is caught',
  'database/schema.sql',
  (s) => s.replace('submission_weak_key_uniq', 'submission_weak_key_moved'),
  'dedup.weak-index.exists',
);

expectCaught(
  'dedup: widening the weak index predicate so the tiers overlap is caught',
  'database/schema.sql',
  (s) => s.replace(
    'CREATE UNIQUE INDEX submission_weak_key_uniq\n  ON ingest.submission (tenant_id, dedup_weak_key)\n  WHERE dedup_key IS NULL;',
    'CREATE UNIQUE INDEX submission_weak_key_uniq\n  ON ingest.submission (tenant_id, dedup_weak_key)\n  WHERE dedup_key IS NOT NULL;',
  ),
  ['dedup.weak-index.partial-on-weak-only', 'dedup.tiers-are-disjoint'],
);

// -------------------------------------------------------------------------------------
// Triggers family
// -------------------------------------------------------------------------------------

expectCaught(
  'triggers: removing the audit append-only trigger is caught',
  'database/schema.sql',
  (s) => s.replace('CREATE TRIGGER audit_append_only', 'CREATE TRIGGER audit_append_only_moved'),
  'triggers.present',
);

expectCaught(
  'triggers: a guard that fires on the wrong event is caught',
  'database/schema.sql',
  (s) => s.replace(
    'CREATE TRIGGER observation_append_only\n  BEFORE UPDATE OR DELETE ON ingest.observation',
    'CREATE TRIGGER observation_append_only\n  BEFORE INSERT ON ingest.observation',
  ),
  'triggers.observation-blocks-mutation',
);

// -------------------------------------------------------------------------------------
// Roles and least privilege
// -------------------------------------------------------------------------------------

expectCaught(
  'grants: giving a runtime role UPDATE on the audit log is caught',
  'database/schema.sql',
  (s) => `${s}\nGRANT UPDATE ON ops.audit TO sac_query;\n`,
  'grants.audit-is-insert-and-select-only',
);

expectCaught(
  'grants: letting query-api reach wrapped content keys is caught',
  'database/schema.sql',
  (s) => `${s}\nGRANT SELECT ON ops.content_object TO sac_query;\n`,
  'grants.content-object-restricted',
);

expectCaught(
  'roles: a role that can log in or bypass RLS is caught',
  'database/schema.sql',
  (s) => s.replace('CREATE ROLE sac_ingest    NOLOGIN;', 'CREATE ROLE sac_ingest    LOGIN;'),
  'roles.no-login-no-superuser',
);

// -------------------------------------------------------------------------------------
// SECURITY DEFINER
// -------------------------------------------------------------------------------------

expectCaught(
  'secdef: a SECURITY DEFINER function without a pinned search_path is caught',
  'database/schema.sql',
  (s) => `${s}\nCREATE FUNCTION ops.probe_definer() RETURNS void\nLANGUAGE plpgsql SECURITY DEFINER AS $$ BEGIN RETURN; END $$;\n`,
  'secdef.pinned-search-path',
);

expectCaught(
  'secdef: a SECURITY DEFINER function left executable by PUBLIC is caught',
  'database/schema.sql',
  (s) => `${s}\nCREATE FUNCTION ops.probe_definer() RETURNS void\nLANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$ BEGIN RETURN; END $$;\nALTER FUNCTION ops.probe_definer() OWNER TO sac_resolver;\n`,
  'secdef.execute-revoked-from-public',
);

expectCaught(
  'secdef: a SECURITY DEFINER function owned by the BYPASSRLS migrator is caught',
  'database/schema.sql',
  (s) => s.replace('ALTER FUNCTION ops.tenant_for_scim_token(text)          OWNER TO sac_resolver;',
    'ALTER FUNCTION ops.tenant_for_scim_token(text)          OWNER TO sac_migrator;'),
  'secdef.owned-by-narrow-role',
);

expectCaught(
  'secdef: a SECURITY DEFINER function left with the role that applied the schema is caught',
  'database/schema.sql',
  (s) => s.replace('ALTER FUNCTION ops.auth_session_by_hash(bytea)          OWNER TO sac_resolver;\n', ''),
  'secdef.owned-by-narrow-role',
);

// -------------------------------------------------------------------------------------
// The contract-vs-store seam (the family that caught the M0 `confidence` gap)
// -------------------------------------------------------------------------------------

expectCaught(
  'kinds: a store constraint that stops forbidding a contract-forbidden field is caught',
  'database/schema.sql',
  (s) => s.replace(
    '      AND window_start IS NULL AND window_end IS NULL AND submission_count IS NULL\n      AND bytes_total IS NULL AND prompt_kind IS NULL))',
    '      AND window_end IS NULL AND submission_count IS NULL\n      AND bytes_total IS NULL AND prompt_kind IS NULL))',
  ),
  'kinds.observation_detection_shape.matches-contract',
);

expectCaught(
  'kinds: the M0 constraint losing `confidence` again is caught',
  'database/schema.sql',
  (s) => s.replace(
    '      AND confidence IS NULL AND content_excerpt IS NULL AND prompt_kind IS NULL)),',
    '      AND content_excerpt IS NULL AND prompt_kind IS NULL)),',
  ),
  'kinds.observation_m0_carries_no_content.matches-contract',
);

expectCaught(
  'kinds: the store suddenly REQUIRING the M2 excerpt makes the recorded exemption stale',
  'database/schema.sql',
  (s) => s.replace(
    "  CONSTRAINT observation_excerpt_only_at_m2\n    CHECK (content_excerpt IS NULL OR (kind = 'prompt' AND collection_mode = 'm2')),",
    "  CONSTRAINT observation_excerpt_only_at_m2\n    CHECK (kind <> 'prompt' OR collection_mode <> 'm2' OR content_excerpt IS NOT NULL),",
  ),
  'kinds.m2-excerpt-is-an-excused-difference',
);

// -------------------------------------------------------------------------------------
// The reason-code seam (the family that caught the revoked_device regression)
// -------------------------------------------------------------------------------------

expectCaught(
  'vocab: a mapping that emits a code the CHECK rejects is caught',
  'ingestion/ingest-api/internal/store/store.go',
  (s) => s.replace('return "oversize", true', 'return "batch_oversize", true'),
  'vocab.every-emitted-code-is-accepted-by-the-check',
);

expectCaught(
  'vocab: a wire code with no case arm is caught before it silently loses its quarantine row',
  'ingestion/ingest-api/internal/store/store.go',
  (s) => s.replace('case protocol.ReasonOversize:', 'case protocol.ReasonOversize_UNUSED:'),
  'vocab.mapping-is-exhaustive-over-wire-enum',
);

expectCaught(
  'vocab: mapping a wire-only code instead of refusing it is caught',
  'ingestion/ingest-api/internal/store/store.go',
  (s) => s.replace(
    '	case protocol.ReasonDuplicateBatch:\n		return "", false',
    '	case protocol.ReasonDuplicateBatch:\n		return "schema_violation", true',
  ),
  'vocab.unmapped-are-exactly-the-documented-wire-only-pair',
);

expectCaught(
  'vocab: an extra code added to the quarantine CHECK is caught',
  'database/schema.sql',
  (s) => s.replace("'internal_error')),", "'internal_error','extra_code')),"),
  'vocab.check-set-is-exact',
);

expectCaught(
  'vocab: a pre-alignment spelling creeping back into the CHECK is caught',
  'database/schema.sql',
  (s) => s.replace("'oversize',", "'batch_oversize',"),
  ['vocab.check-set-is-exact', 'vocab.pre-alignment-spellings-absent'],
);

// -------------------------------------------------------------------------------------
// Digest-shaped columns must carry a format CHECK, or be a recorded exemption
// -------------------------------------------------------------------------------------

expectCaught(
  'digests: a digest column whose format CHECK is replaced by a weaker one is caught',
  'database/schema.sql',
  (s) => s.replace(
    "  CONSTRAINT retrieval_grant_raw_digest_is_sha256\n    CHECK (raw_digest ~ '^sha256:[0-9a-f]{64}$'),",
    '  CONSTRAINT retrieval_grant_raw_digest_is_sha256\n    CHECK (length(raw_digest) > 0),',
  ),
  'digest.every-column-format-checked-or-excused',
);

expectCaught(
  'digests: a newly added digest column with no format CHECK is caught',
  'database/schema.sql',
  (s) => s.replace(
    '  raw_digest      text NOT NULL,',
    '  raw_digest      text NOT NULL,\n  proof_digest    text NOT NULL,',
  ),
  'digest.every-column-format-checked-or-excused',
);

expectCaught(
  'digests: the policy-bundle digest CHECK cannot be dropped now that the table has a writer',
  'database/schema.sql',
  (s) => s.replace(
    "  CONSTRAINT policy_bundle_digest_is_sha256\n    CHECK (signed_digest ~ '^sha256:[0-9a-f]{64}$'),",
    '  CONSTRAINT policy_bundle_digest_is_sha256\n    CHECK (length(signed_digest) > 0),',
  ),
  'digest.every-column-format-checked-or-excused',
);

expectCaught(
  'digests: the classifier-release digest CHECK cannot be dropped either',
  'database/schema.sql',
  (s) => s.replace(
    "  CONSTRAINT classifier_release_digest_is_sha256\n    CHECK (artifact_digest ~ '^sha256:[0-9a-f]{64}$')",
    '  CONSTRAINT classifier_release_digest_is_sha256\n    CHECK (length(artifact_digest) > 0)',
  ),
  'digest.every-column-format-checked-or-excused',
);

// -------------------------------------------------------------------------------------
// Documentation drift: a WARNING, and it must clear when the prose is fixed
// -------------------------------------------------------------------------------------

// The two doc-drift cases are deliberately self-contained: they CREATE the drift they test, rather
// than assuming the repository is currently drifted. An earlier version asserted "the fixture docs
// do not match, so this must WARN", which meant that correcting the documentation - the very thing
// the check exists to encourage - turned the test red. A test that fails when the code gets better
// is worse than no test, so the drift is now part of the fixture.

test('docs: drift is reported as a WARN and does not fail the structural run', () => {
  const tests = readFileSync(q(root, 'database/invariants.test.sql'), 'utf8');
  const n = new Set([...tests.matchAll(/PASS (T\d+)/g)].map((m) => m[1])).size;

  // Move every claim OFF the true count by one, so the mismatch is the test's own doing.
  const docFiles = ['README.md', 'docs/00-architecture.md', 'docs/03-data-platform.md', '.cockpit/project.json'];
  const restores = docFiles.map((rel) =>
    perturb(rel, (s) =>
      s
        .replace(/\d+\s+assertions/g, `${n + 1} assertions`)
        .replace(/\b[A-Za-z]+(?:-[A-Za-z]+)?\s+assertions/g, `${n + 1} assertions`),
    ),
  );
  try {
    const perturbed = docFiles.map((rel) => readFileSync(q(root, rel), 'utf8')).join('\n');
    assert.match(perturbed, new RegExp(`${n + 1}\\s+assertions`), 'the perturbation changed nothing -- the test would prove nothing');
    const r = runChecker();
    assert.equal(r.checks.get('tests.count-matches-docs')?.status, 'WARN', 'drift must WARN, not fail');
    assert.equal(r.code, 0, 'documentation drift must not fail the structural run -- the SQL is the artifact under test');
  } finally {
    restores.forEach((restore) => restore());
  }
});

test('docs: correcting every claim clears the warning', () => {
  const tests = readFileSync(q(root, 'database/invariants.test.sql'), 'utf8');
  const n = new Set([...tests.matchAll(/PASS (T\d+)/g)].map((m) => m[1])).size;

  const docFiles = ['README.md', 'docs/00-architecture.md', 'docs/03-data-platform.md', '.cockpit/project.json'];
  const originals = new Map(docFiles.map((rel) => [rel, readFileSync(q(root, rel), 'utf8')]));

  // Rewrites both the numeric and the spelled-out forms the checker reads.
  const setClaims = (value) => {
    for (const rel of docFiles) {
      writeFileSync(
        q(root, rel),
        originals.get(rel)
          .replace(/\d+\s+assertions/g, `${value} assertions`)
          .replace(/\b[A-Za-z]+(?:-[A-Za-z]+)?\s+assertions/g, `${value} assertions`),
      );
    }
  };

  try {
    // Create the drift first, then remove it. Asserting only the "clears" half was idempotent on a
    // correct tree, which meant that keeping the documentation right -- the thing this check exists
    // to encourage -- made the test unrunnable. Both halves are now the test's own doing.
    setClaims(n + 1);
    const drifted = runChecker();
    assert.equal(drifted.checks.get('tests.count-matches-docs')?.status, 'WARN',
      'a deliberately wrong claim must WARN before it is corrected');

    setClaims(n);
    const corrected = runChecker();
    assert.equal(corrected.checks.get('tests.count-matches-docs')?.status, 'PASS',
      `with every claim set to ${n} the drift check should clear. Detail: ${corrected.checks.get('tests.count-matches-docs')?.detail}`);
    assert.equal(corrected.code, 0);
  } finally {
    for (const [rel, original] of originals) writeFileSync(q(root, rel), original);
  }
});

// -------------------------------------------------------------------------------------
// Not covered here, and labelled rather than left implicit
// -------------------------------------------------------------------------------------
//
// check-schema.mjs is a STATIC check. It cannot show that a constraint the schema declares is
// actually enforced by a running server, that RLS really hides another tenant's rows, or that the
// single-use claim loses a race. Those are runtime properties and they live in
// database/invariants.test.sql, which needs a real PostgreSQL server and is driven by
// database/tools/run-invariants.ps1 -- not runnable from `node --test`.
//
// The split is deliberate: the static half proves each forbid-list is COMPLETE (nothing missing),
// the runtime half proves the list is ENFORCED (nothing decorative). database/tools/tally-log.test.mjs
// covers the harness's parsing, which is the part of that path that can be tested without a server.
