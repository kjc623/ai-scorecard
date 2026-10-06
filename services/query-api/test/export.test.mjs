// export.test.mjs — the list-export planner and the CSV serialiser, without a database.
import test from 'node:test';
import assert from 'node:assert/strict';

import { planListExport, listExportCsv, rowsToCsv, exportTooLarge, EXPORT_MAX_ROWS } from '../src/export.js';
import { rejection } from './helpers.mjs';

const WINDOW = { from: '2026-09-01T00:00:00Z', to: '2026-09-08T00:00:00Z' };

test('a findings list plans an export, dropping the cursor and capping the limit', () => {
  const planned = planListExport(
    { query_version: '1', template: 'q5_findings', params: { window: WINDOW, cursor: 'ignored', severity: 'critical' } },
    { actorId: 'analyst@lab.test' },
  );
  assert.equal(planned.source.id, 'mart.v_finding');
  assert.equal(planned.query.limit, EXPORT_MAX_ROWS);
  assert.equal(planned.query.cursor, null);
  assert.equal(planned.compiled.meta.source, 'mart.v_finding');
  assert.equal(planned.audit.params[2], 'export.list'); // action
});

test('only events and findings can be exported; an aggregate is refused', () => {
  const err = rejection(() => planListExport(
    { query_version: '1', template: 'q1_tools_ranked', params: { window: WINDOW } },
    { actorId: 'analyst@lab.test' },
  ));
  assert.equal(err.resultState, 'unsupported_query_shape');
  assert.match(err.message, /events or findings/);
});

test('a single-record template is not a list and is refused', () => {
  const err = rejection(() => planListExport(
    { query_version: '1', template: 'q9_event_detail', params: { submission_id: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa' } },
    { actorId: 'analyst@lab.test' },
  ));
  assert.equal(err.resultState, 'unsupported_query_shape');
});

test('the CSV carries metadata columns and no prompt text', () => {
  const csv = listExportCsv('ingest.submission', [{
    submission_id: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
    received_at: new Date('2026-10-01T00:00:00Z'),
    subject: 'u_4f21',
    tool: 'tls_b6681b043244c43f',
    tool_name: 'Claude Code',
    labels: [{ class: 'payment_card', score: 0.97 }],
    observed_routes: ['ext.page_context'],
    size_bytes: 4096,
  }]);
  const lines = csv.trim().split('\r\n');
  assert.equal(lines.length, 2); // header + one row
  assert.match(lines[0], /^submission_id,received_at,/);
  assert.match(lines[1], /u_4f21/);
  // labels is a JSON object (commas and quotes), so it is quoted and its quotes doubled.
  assert.match(lines[1], /""class"":""payment_card""/);
  // The export is metadata only: no content column exists to carry a prompt.
  assert.ok(!/prompt_text|content_excerpt|content_bytes/.test(lines[0]));
});

test('rowsToCsv quotes values that contain commas, quotes or newlines', () => {
  const csv = rowsToCsv(['a', 'b'], [{ a: 'x,y', b: 'he said "hi"' }, { a: 'line\nbreak', b: null }]);
  const lines = csv.trim().split('\r\n');
  assert.equal(lines[1], '"x,y","he said ""hi"""');
  assert.equal(lines[2], '"line\nbreak",');
});

test('an over-large export is refused with the fix named', () => {
  const err = exportTooLarge(new Array(EXPORT_MAX_ROWS + 1));
  assert.equal(err.resultState, 'query_too_broad');
  assert.equal(err.reason, 'cost_estimate_exceeded');
  assert.ok(err.detail.fix.narrow_window);
});
