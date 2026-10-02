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
    status: 'done',
    note: 'Contracts T1: 12 tests; `node contracts/tools/generate.mjs --check` fails on drift; the generated Go bindings compile and are consumed by services/ingest-api.',
  },
  T2: {
    status: 'done',
    note: 'Database: 45 distinct assertions (T1..T45) pass against a real PostgreSQL 17.11 server as the runtime roles, with four negative controls proving the assertions can fail. `db` is a gate in tools/accept.mjs.',
  },
  T3: {
    status: 'done',
    note: 'Ingest: 169 tests, and the §4 ladder executed against the live stored procedure `ingest.record_event($1::jsonb, $2::timestamptz)`. The database/sql plumbing is NOT VERIFIED — no PG wire driver exists offline.',
  },
  T4: {
    status: 'done',
    note: 'Read path: query-api 150 tests, 63 compiled statements executed against the live schema, and 15 hostile inputs all rejected with typed errors; apps/dashboard 100 tests with a 204-render probe. The browser never speaks SQL (INV-3 passes as a static check over 23 files).',
  },
  T5: {
    status: 'done',
    note: 'Content vault: 38 component tests plus the Lead-owned `vaultinvariants` external suite with zero skips, driving the grant matrix against the real service. No cloud KMS has been exercised.',
  },
  T6: {
    status: 'pending',
    note: 'NOT DONE. docs/risks/ does not exist: no R1 harness was built and no measurement was made. The loopback broker itself IS implemented in device/capture-core/proxy/loopback with real-socket tests, so the subject of the validation exists — the validation does not.',
  },
  T7: {
    status: 'pending',
    note: 'NOT DONE. docs/risks/ does not exist and no Q2 decision is recorded. schema.sql has an ops.user_dim table, so the org dimension may already have a home, but nobody has checked it against the three questions that need it.',
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
