// adoption.test.mjs — Insights' Adoption tab and the Overview Users tile it feeds: the people who
// used AI, each counted once, out of the people who could have; the applications new in the
// window; and whether prompts are moving to sanctioned tools.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readState } from '../src/states.js';
import { renderScreen } from '../src/render.js';
import { adoptionView, postureView } from '../src/views.js';
import { boot, createDashboard } from '../src/app.js';
import { createQueryApi } from '../src/transport.js';
import { FIRST_SEEN_ROWS, PEOPLE_ROW, SHIFT_ROWS, TOOL_ROWS, fixtureTransport } from './fixtures.mjs';
import { COMPLETE, FRESH, envelope, fakeDocument } from './helpers.mjs';

const NOW = () => new Date('2026-10-01T12:00:00Z');

const usageWith = (people, data = TOOL_ROWS) => readState(envelope('ok', {
  data, freshness: FRESH, coverage: COMPLETE,
  meta: { source: 'mart.v_tool_usage', applied_bucket: 'day', ...(people ? { extras: { people: [people] } } : {}) },
}));
const listOf = (data, source, page = null) => readState(envelope('ok', { data, page, freshness: FRESH, coverage: COMPLETE, meta: { source } }));
const shiftOf = (data, bucket = 'day') => readState(envelope('ok', { data, freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_tool_usage', applied_bucket: bucket } }));
const refused = () => readState(envelope('query_too_broad', { error: { code: 'cost_estimate_exceeded', message: 'The window is too broad for this read' } }));
const block = (view, title) => view.blocks.find((b) => b.title === title || b.title?.startsWith(title));

/** The realistic answers, with the q1 read of the period before the window answered with fewer people. */
function dashboardWith({ previous = { ...PEOPLE_ROW, people: 1900 }, previousRows = TOOL_ROWS } = {}) {
  const sent = [];
  const fixtures = fixtureTransport('realistic');
  const transport = {
    send(body) {
      sent.push(body);
      if (body.template === 'q1_tools_ranked' && body.params.window.to === '2026-09-24T12:00:00.000Z') {
        return envelope('ok', { data: previousRows, freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_tool_usage', applied_bucket: 'day', extras: { people: [previous] } } });
      }
      return fixtures.send(body);
    },
  };
  return { sent, dashboard: createDashboard({ api: createQueryApi({ transport }), now: NOW }) };
}

test('the Overview Users tile counts each person once, out of the directory, else the enrolled devices', () => {
  const directory = postureView({ devices: null, tools: usageWith(PEOPLE_ROW) }).tiles[3];
  assert.deepEqual([directory.label, directory.value.text, directory.note], ['Users', '2,050', 'of 4,210 active people in the directory']);
  const devices = postureView({ devices: null, tools: usageWith({ ...PEOPLE_ROW, directory_people: 0 }) }).tiles[3];
  assert.equal(devices.note, 'of 3,980 people signed in on an enrolled device');
  const alone = postureView({ devices: null, tools: usageWith({ ...PEOPLE_ROW, directory_people: 0, device_people: 0 }) }).tiles[3];
  assert.equal(alone.note, 'Each person counted once, however many tools they used.');
  const older = postureView({ devices: null, tools: usageWith(null) }).tiles[3];
  assert.equal(older.value.text, '1,877');
  assert.equal(older.note, 'At least this many: the most counted in any one period of the window.', 'a read that did not count people keeps the floor it had');
});

test('Adoption reads the window and the period before it, the applications first seen, and prompts by sanction', async () => {
  const { dashboard, sent } = dashboardWith();
  const { view } = await dashboard.load('insights', { filters: { view: 'adoption' } });
  assert.match(view.subtitle, /^Adoption: /);
  assert.deepEqual(sent.filter((b) => b.template === 'q1_tools_ranked').map((b) => b.params.window.to), ['2026-09-24T12:00:00.000Z', '2026-10-01T12:00:00.000Z']);
  assert.deepEqual(sent.find((b) => b.source === 'mart.v_tool_first_seen'), {
    query_version: '1', source: 'mart.v_tool_first_seen', filters: [], window: { from: '2026-09-24T12:00:00.000Z', to: '2026-10-01T12:00:00.000Z' }, limit: 200,
  });
  const shift = sent.find((b) => b.source === 'mart.v_tool_usage');
  assert.deepEqual([shift.bucket, shift.dimensions, shift.measures], ['day', ['sanctioned_state'], ['submissions']]);

  for (const [preset, bucket] of [['d30', 'week'], ['d90', 'week'], ['h24', 'hour']]) {
    sent.length = 0;
    await dashboard.load('insights', { preset, filters: { view: 'adoption' } });
    assert.equal(sent.find((b) => b.source === 'mart.v_tool_usage').bucket, bucket, preset);
  }
  sent.length = 0;
  await dashboard.load('insights', {});
  assert.ok(sent.some((b) => b.source === 'mart.v_tool_first_seen'), 'Adoption is the default tab');
});

test('People using AI is the people out of the directory, split by the sanction of the tools they used, against the previous period', async () => {
  const { view } = await dashboardWith().dashboard.load('insights', { filters: { view: 'adoption' } });
  const people = view.lead[0];
  assert.equal(people.kind, 'share');
  assert.equal(people.title, 'People using AI');
  assert.equal(people.total, 2050);
  assert.equal(people.totalLabel, 'of 4,210 active people in the directory (49%)');
  assert.equal(people.note, '+8% on the previous 7 days');
  assert.deepEqual(people.parts.map((p) => [p.key, p.label, p.count]), [
    ['sanctioned', 'Only sanctioned tools', 1820], ['both', 'Both', 190], ['unsanctioned', 'Only tools not sanctioned', 40],
  ]);
  assert.ok(view.notes.includes('A tool not sanctioned is one marked unsanctioned or with no decision yet.'));
  assert.ok(view.notes.includes('The people are out of everyone the directory lists as active.'));
  const html = renderScreen(view, {});
  assert.match(html, /<span class="tile-value">2,050<\/span><span class="share-total-label">of 4,210 active people in the directory \(49%\)<\/span>/);
  assert.match(html, /<span class="dist-seg v-vocab-both" style="flex-grow:190"/);
});

test('without a directory the people are out of the enrolled devices, and with neither there is nothing to compare with', () => {
  const devices = adoptionView({ usage: usageWith({ ...PEOPLE_ROW, directory_people: 0 }) });
  assert.equal(devices.lead[0].totalLabel, 'of 3,980 people signed in on an enrolled device (52%)');
  assert.ok(devices.notes.includes('No directory is synced, so the people are out of everyone last signed in on an enrolled device.'));
  const none = adoptionView({ usage: usageWith({ ...PEOPLE_ROW, directory_people: 0, device_people: 0 }) });
  assert.equal(none.lead[0].totalLabel, 'people');
  assert.equal(none.lead[0].note, null, 'no previous period, no change');
  assert.ok(none.notes.includes('The previous 7 days could not be read, so no change is shown.'));
  const more = adoptionView({ usage: usageWith({ ...PEOPLE_ROW, directory_people: 1000 }) });
  assert.equal(more.lead[0].totalLabel, 'more than the 1,000 active people in the directory', 'no share above the whole');
  assert.equal(postureView({ devices: null, tools: usageWith({ ...PEOPLE_ROW, directory_people: 1000 }) }).tiles[3].note, 'more than the 1,000 active people in the directory');
  const uncounted = adoptionView({ usage: usageWith(null) });
  assert.equal(uncounted.lead[0].emptyText, 'The people could not be counted for this window.');
});

test('New this period lists the applications that sent their first prompt in the window, each opening its tool', async () => {
  const { view } = await dashboardWith().dashboard.load('insights', { preset: 'd30', filters: { view: 'adoption' } });
  const table = block(view, 'New this period');
  assert.deepEqual(table.columns.map((c) => c.label), ['Application', 'Sanction', 'First seen', 'People since']);
  assert.deepEqual(table.rows.map(({ row }) => [row.tool_cell.primary, row.tool_cell.secondary, row.sanctioned_state, row.sanctioned_state_text, row.first_seen_day, row.people, row.href]), [
    ['Unrecognised tool', 'shadow_llm_gateway', 'unknown', 'No decision yet', '2026-09-29', 2, '#tools?tool=shadow_llm_gateway&preset=d30'],
    ['Claude Code', null, 'unsanctioned', 'Unsanctioned', '2026-09-26', 230, '#tools?tool=tls_b6681b043244c43f&preset=d30'],
  ]);
  assert.equal(table.summary, '2 applications sent their first prompt in this window; 1 is unsanctioned and 1 has no decision yet.');
  const html = renderScreen(view, {});
  assert.match(html, /<p class="block-summary">2 applications sent their first prompt in this window; 1 is unsanctioned and 1 has no decision yet\.<\/p>/);
  assert.match(html, /<span class="v-vocab v-vocab-unsanctioned" title="unsanctioned">Unsanctioned<\/span>/);
});

test('the New this period line says when every application is sanctioned, when more were seen than listed, and when collection began in the window', () => {
  const sanctioned = [{ ...FIRST_SEEN_ROWS[1], sanctioned_state: 'sanctioned' }];
  const one = adoptionView({ usage: usageWith(PEOPLE_ROW), firstSeen: listOf(sanctioned, 'mart.v_tool_first_seen') });
  assert.equal(block(one, 'New this period').summary, '1 application sent its first prompt in this window; it is sanctioned.');
  const cut = adoptionView({ usage: usageWith(PEOPLE_ROW), firstSeen: listOf(sanctioned, 'mart.v_tool_first_seen', { next_cursor: 'c.s', returned: 1 }) });
  assert.equal(block(cut, 'New this period').summary, 'More than 1 application sent their first prompt in this window; all are sanctioned.');
  const first = adoptionView({ usage: usageWith(PEOPLE_ROW), previous: usageWith(PEOPLE_ROW, []), firstSeen: listOf(FIRST_SEEN_ROWS, 'mart.v_tool_first_seen') });
  assert.match(block(first, 'New this period').summary, / Nothing was recorded before this window, so every application in use is new\.$/);
});

test('New this period says why it is empty', () => {
  const none = adoptionView({ usage: usageWith(PEOPLE_ROW), firstSeen: listOf([], 'mart.v_tool_first_seen') });
  assert.equal(block(none, 'New this period').emptyText, 'No application sent its first prompt in this window.');
  assert.equal(block(none, 'New this period').summary, undefined);
  const failed = adoptionView({ usage: usageWith(PEOPLE_ROW), firstSeen: refused() });
  assert.equal(block(failed, 'New this period').emptyText, 'The applications new in this window could not be read: The window is too broad for this read.');
});

test('the shift stacks each bucket\'s prompts by sanction and says how the sanctioned share moved', async () => {
  const { view } = await dashboardWith().dashboard.load('insights', { filters: { view: 'adoption' } });
  const shift = block(view, 'Prompts per day');
  assert.equal(shift.kind, 'series');
  assert.equal(shift.title, 'Prompts per day, by sanction');
  assert.deepEqual(shift.points.map((p) => [p.bucket, p.value.value, p.parts.map((part) => [part.key, part.value])]), [
    ['2026-09-29T00:00:00Z', 3000, [['sanctioned', 2600], ['unknown', 100], ['unsanctioned', 300]]],
    ['2026-09-30T00:00:00Z', 2000, [['sanctioned', 1400], ['unknown', 100], ['unsanctioned', 500]]],
  ], 'a tool with no state has no decision');
  assert.equal(shift.summary, 'Sanctioned tools took 70% of prompts in the latest day, down from 87% in the first.');
  const html = renderScreen(view, {});
  assert.match(html, /<h3>Prompts per day, by sanction<\/h3><\/header><p class="block-summary">Sanctioned tools took 70% of prompts in the latest day, down from 87% in the first\.<\/p>/);
  assert.match(html, /<span class="bar-seg v-vocab-sanctioned" style="flex-grow:2600"><\/span>/);
});

test('one bucket says the share, and a window with no prompts or no read draws no shift', () => {
  const single = adoptionView({ usage: usageWith(PEOPLE_ROW), shift: shiftOf(SHIFT_ROWS.slice(0, 3), 'week') });
  assert.equal(block(single, 'Prompts per week').summary, 'Sanctioned tools took 87% of prompts in this window.');
  const same = adoptionView({ usage: usageWith(PEOPLE_ROW), shift: shiftOf([...SHIFT_ROWS.slice(0, 3), ...SHIFT_ROWS.slice(0, 3).map((r) => ({ ...r, bucket: '2026-09-30T00:00:00Z' }))]) });
  assert.equal(block(same, 'Prompts per day').summary, 'Sanctioned tools took 87% of prompts in the latest day, the same as 87% in the first.');
  assert.equal(block(adoptionView({ usage: usageWith(PEOPLE_ROW), shift: shiftOf([]) }), 'Prompts per'), undefined);
  assert.equal(block(adoptionView({ usage: usageWith(PEOPLE_ROW), shift: refused() }), 'Prompts per'), undefined);
});

test('Insights switches between its tabs beside its window, and each keeps the other', async () => {
  const doc = fakeDocument('#insights?view=adoption&preset=d30');
  await boot({ document: doc, api: createQueryApi({ transport: fixtureTransport() }), admin: null, session: null });
  const html = doc.html('app');
  assert.match(html, /<span class="eyebrow">Archive<\/span><h2>Insights<\/h2>/);
  assert.match(html, /<nav class="page-tabs" aria-label="Insights"><a class="page-tab" href="#insights\?preset=d30" aria-current="page">Adoption<\/a><a class="page-tab" href="#insights\?preset=d30&amp;view=discovered">Discovered<\/a><a class="page-tab" href="#insights\?preset=d30&amp;view=demand">Demand<\/a><a class="page-tab" href="#insights\?preset=d30&amp;view=agents">Coding agents<\/a><\/nav>/);
  assert.match(html, /<a class="seg-item" href="#insights\?view=adoption&amp;preset=d7">7d<\/a>/, 'a window keeps the tab');
  assert.match(html, /People using AI/);
});
