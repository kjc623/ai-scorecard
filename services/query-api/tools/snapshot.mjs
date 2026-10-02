// snapshot.mjs — generate (or check) the compiled-SQL snapshots.
//
//   node tools/snapshot.mjs           regenerate test/snapshots/compiled-sql.json
//   node tools/snapshot.mjs --check   exit 1 if the file differs from what the code produces
//
// The snapshots exist so the integration verifier can diff the compiled SQL against a reviewed
// artefact rather than re-deriving it, and so a change to the compiler shows up as a diff in a
// review rather than as a silent change of question.

import { readFileSync, writeFileSync, mkdirSync, existsSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { plan } from '../src/plan.js';
import { TEMPLATE_NAMES } from '../src/templates.js';
import { SOURCE_IDS, SOURCES } from '../src/registry.js';

const HERE = dirname(fileURLToPath(import.meta.url));
export const SNAPSHOT_PATH = join(HERE, '..', 'test', 'snapshots', 'compiled-sql.json');

const TENANT = '00000000-0000-4000-8000-0000000000aa';
const WINDOW = { from: '2026-09-01T00:00:00Z', to: '2026-09-08T00:00:00Z' };
const NOW = new Date('2026-10-02T12:00:00Z');

const TEMPLATE_PARAMS = Object.freeze({
  q1_tools_ranked: { window: WINDOW, limit: 100 },
  q2_unsanctioned_users: { window: WINDOW, tool: 'claude_web', limit: 100 },
  q3_team_growth: { window: WINDOW, limit: 100 },
  q4_class_mix: { window: WINDOW, limit: 100 },
  q5_findings: { window: WINDOW, limit: 50 },
  q6_subject_series: { window: WINDOW, subject: 'user_ref_0001' },
  q7_devices: { limit: 50 },
  q8_activity: { window: WINDOW, subject: 'user_ref_0001', limit: 50 },
  q9_event_detail: { submission_id: '11111111-2222-4333-8444-555555555555' },
  q10_audit_trail: { window: WINDOW, limit: 50 },
});

/** Every shape the snapshots pin, in a stable order. */
export function shapes() {
  const out = [];
  for (const name of TEMPLATE_NAMES) {
    out.push({ name: `template:${name}`, request: { query_version: '1', template: name, params: TEMPLATE_PARAMS[name] } });
  }
  for (const id of SOURCE_IDS) {
    const source = SOURCES[id];
    if (source.kind === 'aggregate') {
      const measures = Object.keys(source.measures).slice(0, 3);
      const dims = Object.keys(source.dimensions).filter((d) => d !== 'bucket' && d !== 'subject').slice(0, 2);
      const filters = [];
      if (source.dimensions.tool) filters.push({ field: 'tool', op: 'starts_with', value: 'claude' });
      if (source.dimensions.device) filters.push({ field: 'device', op: 'eq', value: TENANT });
      if (source.requiresSubjectScope) filters.push({ field: 'subject', op: 'eq', value: 'user_ref_0001' });
      out.push({
        name: `source:${id}:day`,
        request: { query_version: '1', source: id, bucket: 'day', dimensions: dims, measures, filters, window: WINDOW, limit: 20 },
      });
      out.push({
        name: `source:${id}:week+rollup`,
        request: { query_version: '1', source: id, bucket: 'week', dimensions: dims, measures, filters, window: WINDOW, rollup: true },
      });
    } else {
      out.push({
        name: `source:${id}:list`,
        request: {
          query_version: '1',
          source: id,
          filters: [],
          ...(source.time ? { window: WINDOW } : {}),
          limit: 20,
        },
      });
    }
  }
  return out;
}

/** @returns {Record<string, {statements: Array<{id:string,text:string,params:unknown[]}>, meta: object}>} */
export function buildSnapshot() {
  const snapshot = {};
  for (const shape of shapes()) {
    const p = plan(shape.request, { now: NOW, tenant: TENANT, actorId: 'snapshot' });
    snapshot[shape.name] = {
      result_state_hint: p.audit.decision.phase,
      statements: p.statements.map((s) => ({ id: s.id, text: s.text, params: s.params })),
      meta: {
        source: p.meta.source,
        applied_bucket: p.meta.applied_bucket,
        native_bucket_size: p.meta.native_bucket_size,
        measure_semantics: p.meta.measure_semantics,
        subject_count_basis: p.meta.subject_count_basis,
        order: p.meta.order,
        k: p.meta.k,
        estimated_cells: p.meta.guard?.estimated_cells ?? null,
        bounded_cells: p.meta.guard?.bounded_cells ?? null,
      },
    };
  }
  return snapshot;
}

export function serialise(snapshot) {
  return `${JSON.stringify(snapshot, null, 2)}\n`;
}

const invokedDirectly = process.argv[1]?.endsWith('snapshot.mjs') ?? false;

if (invokedDirectly) {
  const snapshot = buildSnapshot();
  const text = serialise(snapshot);
  if (process.argv.includes('--check')) {
    if (!existsSync(SNAPSHOT_PATH)) {
      console.error('snapshot file is missing; run: node tools/snapshot.mjs');
      process.exit(1);
    }
    const current = readFileSync(SNAPSHOT_PATH, 'utf8').replace(/\r\n/g, '\n');
    if (current !== text) {
      console.error('compiled-SQL snapshots differ from the current compiler output.');
      process.exit(1);
    }
    console.log(`snapshots match (${Object.keys(snapshot).length} shapes)`);
    process.exit(0);
  }
  mkdirSync(dirname(SNAPSHOT_PATH), { recursive: true });
  writeFileSync(SNAPSHOT_PATH, text);
  console.log(`wrote ${SNAPSHOT_PATH} (${Object.keys(snapshot).length} shapes)`);
}
