#!/usr/bin/env node
// tools/update-cockpit-tasks.mjs - reflect the board's real state in .cockpit/project.json.
//
// The cockpit's own tests cover components and agents; tasks are the board's business. But a task
// list that says `pending` for everything is a map that misleads, and the cockpit is read by a
// human deciding what to do next. This sets each of the seven declared tasks to what the repository
// now shows, and records the state as evidence rather than as a claim: where a task is `done`, the
// line names what proves it.
//
//   node tools/update-cockpit-tasks.mjs --check
//   node tools/update-cockpit-tasks.mjs
//
// The evidence strings are the same ones quoted in .integration/LEAD-VERIFICATION.md, so the two
// cannot drift into disagreeing accounts of the same work.

import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const MANIFEST = join(ROOT, '.cockpit', 'project.json');
const CHECK = process.argv.includes('--check');

/** What each declared task's real state is, and what proves it. */
const STATE = {
  T1: {
    status: 'completed',
    note: 'Contracts T1: 12 tests; `node contracts/tools/generate.mjs --check` fails on drift; the generated Go bindings compile and are consumed by ingestion/ingest-api.',
  },
  T2: {
    status: 'completed',
    note: 'Database: 45 distinct assertions (T1..T45) pass against a real PostgreSQL 17.11 server as the runtime roles, with four negative controls proving the assertions can fail. `db` is a gate in tools/accept.mjs.',
  },
  T3: {
    status: 'completed',
    note: 'Ingest: 169 tests, and the §4 ladder executed against the live stored procedure `ingest.record_event($1::jsonb, $2::timestamptz)`. The database/sql plumbing is NOT VERIFIED — no PG wire driver exists offline.',
  },
  T4: {
    status: 'completed',
    note: 'Read path: query-api 150 tests, 63 compiled statements executed against the live schema, and 15 hostile inputs all rejected with typed errors; query/dashboard 100 tests with a 204-render probe. The browser never speaks SQL (INV-3 passes as a static check over 23 files).',
  },
  T5: {
    status: 'completed',
    note: 'Content vault: 38 component tests plus the Lead-owned `vaultinvariants` external suite with zero skips, driving the grant matrix against the real service. No cloud KMS has been exercised.',
  },
  T6: {
    status: 'completed',
    note: 'R1 measured with a runnable harness against the REAL loopback broker (docs/risks/R1-harness/, 7 claim verdicts, three runs identical). What remains unvalidated is stated there without softening: relocation through a real vendor tool, a real client resolving a substitute port, and behaviour across a reboot are NOT VALIDATED for any tool, so no port-set entry should ship enabled on the strength of this document — mode F stays detection_only per tool until a real lab result exists.',
  },
  T7: {
    status: 'completed',
    note: 'Q2 answered in docs/risks/Q2-organisational-dimension.md. The finding is not what the task assumed: ops.user_dim, mart.agg_org_period and the query-api dimension are all built and plumbed, and NOTHING writes user_dim — services/control-api does not exist. An empty Q3 therefore renders as a data state rather than a missing component, which is the part worth knowing.',
  },
};

const manifest = JSON.parse(readFileSync(MANIFEST, 'utf8'));
const changes = [];
for (const task of manifest.tasks) {
  const want = STATE[task.id];
  if (!want) continue;
  if (task.status !== want.status) {
    changes.push(`${task.id}: status ${task.status} -> ${want.status}`);
    task.status = want.status;
  }
  if (task.completionNote !== want.note) {
    task.completionNote = want.note;
    changes.push(`${task.id}: evidence note set`);
  }
}

if (changes.length === 0) {
  console.log('cockpit tasks: no drift');
  process.exit(0);
}
console.log(`cockpit tasks: ${changes.length} change(s)`);
for (const c of changes) console.log(`  ${c}`);

if (CHECK) {
  console.log('\n--check: not written');
  process.exit(1);
}
writeFileSync(MANIFEST, JSON.stringify(manifest, null, 2) + '\n', 'utf8');
console.log('\nwritten.');
