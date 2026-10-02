// =====================================================================================
// db/tools/tally-log.test.mjs -- tests for the harness's parsing rules
// =====================================================================================
// Run: node --test db/tools/
//
// WHAT THIS COVERS
// ----------------
// run-invariants.ps1 cannot be run from Node without Docker and a real server, but its *parsing*
// is the part that has been wrong twice: an unanchored `grep -ci ERROR` counted the provenance
// header's ON_ERROR_STOP and failed a healthy run, and a `fail=` that counted only FAIL markers
// let a run which aborted on a raw SQL error be judged as passing. Both rules now live in
// db/tools/tally-log.mjs and are tested here against logs shaped exactly like the real ones.
//
// The stored fixture db/evidence/08-negative-control-raw-prefix-schema.log is used when present,
// and skipped with a label when not: db/evidence/*.log is gitignored, so a test that required it
// would fail on a fresh clone. The synthetic fixtures below always run and encode the same shapes.
// =====================================================================================

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { existsSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { ANCHORED_ERROR, tallyLine, tallyLogs, stripHeader } from './tally-log.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO = join(HERE, '..', '..');

// The provenance header run-invariants.ps1 prepends. Its first line is the whole reason the tally
// must anchor: ON_ERROR_STOP contains the substring ERROR.
const HEADER = [
  '# command : psql -v ON_ERROR_STOP=1 -f db/schema.sql && psql -v ON_ERROR_STOP=1 -f db/invariants.test.sql',
  '# schema  : db/schema.sql sha256 abc',
  '# tests   : db/invariants.test.sql sha256 def',
  '# server  : PostgreSQL 17.11',
  '# database: shadow   captured: 2026-10-02T17:37:12Z',
  '#',
].join('\n');

const HEALTHY = [
  'psql:/db/invariants.test.sql:59: NOTICE:  fixtures loaded',
  'psql:/db/invariants.test.sql:76: NOTICE:  PASS T1 bundle within ceiling accepted',
  'psql:/db/invariants.test.sql:91: NOTICE:  PASS T2 bundle exceeding ceiling rejected (policy bundle 1 ...)',
  'psql:/db/invariants.test.sql:106: NOTICE:  PASS T32 full_text search without an M3 ceiling refused',
  'psql:/db/invariants.test.sql:120: NOTICE:  PASS T32 attachment_names search on an M0 ceiling refused',
  'psql:/db/invariants.test.sql:907: NOTICE:  summary: 0 submissions, 0 observations',
  'psql:/db/invariants.test.sql:907: NOTICE:  all invariant tests completed',
].join('\n');

// A run that aborted on a raw SQL error rather than on an assertion's own RAISE: the case that
// used to be reported as fail=0, because `fail` counted only FAIL markers.
const ABORTED = [
  'psql:/db/invariants.test.sql:985: NOTICE:  PASS T1 something',
  'psql:/db/invariants.test.sql:985: ERROR:  new row for relation "submission" violates check constraint "submission_exact_key_implies_digest"',
  'CONTEXT:  SQL statement "UPDATE ingest.submission s SET ..."',
].join('\n');

const MARKED_FAILURE = [
  'psql:/db/invariants.test.sql:985: NOTICE:  PASS T1 something',
  "psql:/db/invariants.test.sql:1309: ERROR:  FAIL T42 an M0 record accepted confidence",
].join('\n');

// -------------------------------------------------------------------------------------

test('a healthy log with the provenance header has no false positives', () => {
  const t = tallyLogs({ tests: `${HEADER}\n${HEALTHY}` });
  assert.equal(t.errorLines, 0, 'the header quotes ON_ERROR_STOP; it must not be counted');
  assert.equal(t.fail, 0);
  assert.equal(t.aborted, false);
  assert.equal(t.pass, 4);
});

test('the header alone is never an error, however many times the command is quoted', () => {
  const t = tallyLogs({ tests: `${HEADER}\n${HEADER}\n${HEALTHY}` });
  assert.equal(t.errorLines, 0);
  assert.equal(t.pass, 4, 'a duplicated header must not create phantom assertions either');
});

test('the unanchored form really would have counted the header -- the bug this guards', () => {
  const text = `${HEADER}\n${HEALTHY}`;
  const unanchored = text.split(/\r?\n/).filter((l) => /ERROR/i.test(l)).length;
  assert.equal(unanchored, 1, 'the header does contain the substring ERROR (ON_ERROR_STOP)');
  assert.equal(tallyLogs({ tests: text }).errorLines, 0, 'the anchored rule does not');
});

test('an aborted run counts as a failure even with no FAIL marker', () => {
  const t = tallyLogs({ tests: ABORTED });
  assert.equal(t.failMarkers, 0, 'no assertion raised a FAIL marker');
  assert.equal(t.errorLines, 1);
  assert.equal(t.fail, 1, 'accept.mjs judges on `fail`, so a raw abort must set it');
  assert.equal(t.aborted, true);
});

test('a marked assertion failure is counted as both a marker and a failure', () => {
  const t = tallyLogs({ tests: MARKED_FAILURE });
  assert.equal(t.failMarkers, 1);
  assert.equal(t.errorLines, 1);
  assert.equal(t.fail, 1);
});

test('schema-log errors are counted separately from the test log', () => {
  const t = tallyLogs({
    tests: `${HEADER}\n${HEALTHY}`,
    schema: 'psql:/db/schema.sql:77: ERROR:  role "sac_owner" already exists',
  });
  assert.equal(t.errorLines, 0, 'the invariants log itself is clean');
  assert.equal(t.schemaErrors, 1);
  assert.equal(t.fail, 0);
});

test('distinct assertions are counted apart from PASS notices (T32 and T33 repeat)', () => {
  const t = tallyLogs({ tests: HEALTHY });
  assert.equal(t.pass, 4, 'four PASS notices');
  assert.equal(t.distinct, 3, 'T32 appears twice');
  assert.deepEqual(t.ids, ['T1', 'T2', 'T32']);
});

test('a NOTICE that merely mentions an error is not counted', () => {
  const t = tallyLogs({
    tests: 'psql:x:1: NOTICE:  PASS T1 handled an error gracefully\npsql:x:2: NOTICE:  CONTEXT: not an error line',
  });
  assert.equal(t.errorLines, 0);
  assert.equal(t.pass, 1);
});

test('stripHeader removes only header lines', () => {
  assert.equal(stripHeader(`${HEADER}\npsql:x:1: NOTICE:  hi`).trim(), 'psql:x:1: NOTICE:  hi');
});

test('ANCHORED_ERROR matches real psql shapes and not prose', () => {
  assert.ok(ANCHORED_ERROR.test('psql:/db/x.sql:12: ERROR:  boom'));
  assert.ok(ANCHORED_ERROR.test('ERROR:  boom'));
  assert.ok(ANCHORED_ERROR.test('psql:/db/x.sql:12: FATAL:  boom'));
  assert.ok(ANCHORED_ERROR.test('psql:/db/x.sql:12: PANIC:  boom'));
  assert.ok(!ANCHORED_ERROR.test('# command : psql -v ON_ERROR_STOP=1 -f db/schema.sql'));
  assert.ok(!ANCHORED_ERROR.test('psql:x:1: NOTICE:  FAIL T1 wrapped up'));
});

test('the TALLY line keeps the shape tools/accept.mjs parses', () => {
  const t = tallyLogs({ tests: `${HEADER}\n${HEALTHY}` });
  const line = tallyLine(t);
  const m = /TALLY pass=(\d+) fail=(\d+) .*distinct_ids=(\d+)/.exec(line);
  assert.ok(m, `accept.mjs would not match this line: ${line}`);
  assert.equal(m[1], '4');
  assert.equal(m[2], '0');
  assert.equal(m[3], '3');
});

test('the TALLY line reports a failure for an aborted run, so the gate fails it', () => {
  const line = tallyLine(tallyLogs({ tests: ABORTED }));
  const m = /TALLY pass=(\d+) fail=(\d+) .*distinct_ids=(\d+)/.exec(line);
  assert.ok(m);
  assert.equal(m[2], '1', 'tools/accept.mjs judges on this field alone');
});

// -------------------------------------------------------------------------------------
// The captured fixture, when it is on disk
// -------------------------------------------------------------------------------------

const FIXTURE = join(REPO, 'db', 'evidence', '08-negative-control-raw-prefix-schema.log');

test('captured fixture: the pre-fix run that aborted is judged a failure', { skip: !existsSync(FIXTURE) ? 'db/evidence/08-... is gitignored and not on disk' : false }, () => {
  const t = tallyLogs({ tests: readFileSync(FIXTURE, 'utf8') });
  assert.ok(t.pass > 0, 'it got through T35 before the constraint violation');
  assert.equal(t.failMarkers, 0, 'the abort was a raw CHECK violation, not a FAIL marker');
  assert.equal(t.fail, 1, 'and it must still be judged a failure');
  assert.equal(t.aborted, true);
});
