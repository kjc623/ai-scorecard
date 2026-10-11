// discovered.test.mjs — Insights' Discovered tab: every AI application, CLI, IDE extension, local
// model and inference endpoint device discovery found, whether or not it sent prompts, and the
// devices whose inventory scan is not reporting.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readState } from '../src/states.js';
import { renderScreen } from '../src/render.js';
import { discoveredView } from '../src/views.js';
import { boot, createDashboard } from '../src/app.js';
import { createQueryApi } from '../src/transport.js';
import { DISCOVERY_ROWS, INVENTORY_SCAN, fixtureTransport } from './fixtures.mjs';
import { COMPLETE, FRESH, envelope, fakeDocument } from './helpers.mjs';

const NOW = () => new Date('2026-10-01T12:00:00Z');

/** The prompts per tool in the window: Claude Code under two fingerprints, Ollama under one. */
const PROMPT_ROWS = Object.freeze([
  { bucket: '2026-09-01T00:00:00Z', tool: 'tls_b6681b043244c43f', tool_key: 'claude_code', tool_name: 'Claude Code', submissions: 800 },
  { bucket: '2026-09-01T00:00:00Z', tool: 'app:claude_code', tool_key: 'claude_code', tool_name: 'Claude Code', submissions: 12 },
  { bucket: '2026-09-01T00:00:00Z', tool: 'app:ollama', tool_key: 'ollama', tool_name: 'Ollama', submissions: 30 },
]);

const foundOf = (data = DISCOVERY_ROWS, meta = {}) => readState(envelope('ok', {
  data,
  freshness: { ...FRESH, aggregate: 'mart.agg_discovery_period' },
  coverage: { ...COMPLETE, devices_reporting: 4180 },
  meta: { source: 'mart.agg_discovery_period', applied_bucket: 'month', measure_semantics: { devices: 'distinct_lower_bound' }, extras: { inventory_scan: [INVENTORY_SCAN] }, ...meta },
}));
const promptsOf = (data = PROMPT_ROWS, meta = {}) => readState(envelope('ok', { data, freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_tool_usage', applied_bucket: 'month', ...meta } }));

/** The realistic answers, with the prompts per tool answered from PROMPT_ROWS. */
function transportWith({ prompts = 'answer' } = {}) {
  const sent = [];
  const fixtures = fixtureTransport('realistic');
  return {
    sent,
    send(body) {
      sent.push(body);
      if (body.source === 'mart.v_tool_usage' && body.dimensions?.includes('tool')) {
        if (prompts === 'refused') return envelope('query_too_broad', { error: { code: 'cost_estimate_exceeded', message: 'Too broad.' } });
        return envelope('ok', { data: PROMPT_ROWS, freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.v_tool_usage', applied_bucket: 'month' } });
      }
      return fixtures.send(body);
    },
  };
}

const table = (view) => view.blocks.find((b) => b.title === 'Found on devices');
const cells = (view) => table(view).rows.map(({ row }) => [row.tool_cell.primary, row.found_as, row.devices, row.first_seen_day, row.last_seen_day, row.prompts]);

test('each application is one row, local models and inference endpoints first, then by the devices it was on', () => {
  const view = discoveredView({ found: foundOf(), prompts: promptsOf() });
  assert.deepEqual(cells(view), [
    ['Ollama', 'Local model, Desktop app', 5, '2026-09-12', '2026-09-30', 30],
    ['Unrecognised tool', 'Inference endpoint', 1, '2026-09-29', '2026-09-30', 0],
    ['Claude Code', 'CLI', 12, '2026-08-02', '2026-09-30', 812],
    ['ChatGPT (desktop)', 'Desktop app', 7, '2026-08-02', '2026-09-30', 0],
  ]);
  const [ollama, process, claude, chatgpt] = table(view).rows.map(({ row }) => row);
  assert.equal(ollama.tool_cell.secondary, 'llama3, mistral', 'the models it holds');
  assert.deepEqual(ollama.tool_cell.chip, { key: 'unknown', text: 'No decision yet' });
  assert.equal(process.tool_cell.secondary, 'api.example.test · exe:0123456789abcdef', 'the host it reached, and the fingerprint that tells it apart');
  assert.equal(claude.href, '#tools?tool=tls_b6681b043244c43f', 'opens the Tools row that sent most of its prompts');
  assert.equal(chatgpt.href, null, 'nothing to open for a tool that sent no prompt');
  assert.deepEqual(table(view).columns.map((c) => c.label), ['Application', 'Found as', 'Devices of 4,180', 'First seen', 'Last seen', 'Prompts']);
  assert.equal(table(view).summary, '4 applications were found on devices in this window; 1 holds local models, 1 reached an inference endpoint and 2 sent no prompt.');
});

test('a catalogue application found under two fingerprints is one row, with what each found', () => {
  const running = { ...DISCOVERY_ROWS[1], tool: 'exe:fedcba9876543210', discovery_type: 'app_running', devices: 15, first_seen: '2026-07-20T00:00:00Z' };
  const view = discoveredView({ found: foundOf([...DISCOVERY_ROWS, running]), prompts: promptsOf() });
  const claude = cells(view).filter((c) => c[0] === 'Claude Code');
  assert.deepEqual(claude, [['Claude Code', 'CLI, Seen running', 15, '2026-07-20', '2026-09-30', 812]]);
  assert.equal(view.tiles[0].value.text, '4');
});

test('the tiles count what was found, what sends nothing, and the devices whose scan is not reporting', () => {
  const view = discoveredView({ found: foundOf(), prompts: promptsOf() }, { preset: 'd30' });
  assert.deepEqual(view.tiles.map((t) => [t.label, t.value.text, t.note]), [
    ['Found on devices', '4', 'applications found in this window'],
    ['Not sending prompts', '2', 'found on a device but sent no prompt in this window'],
    ['Not scanned', '200', 'of 4,620 enrolled devices: the inventory scan has not reported in the last 24 hours, so what they hold is not known. 20 more have it switched off by policy.'],
  ]);
  assert.equal(view.tiles[2].href, '#devices');
  assert.equal(table(view).rows[0].row.href, '#tools?tool=app%3Aollama&preset=d30', 'a drill-down keeps the window');
  assert.match(view.notes.join(' '), /devices is a distinct-subject count served as a lower bound/);
  assert.match(view.notes.join(' '), /on its busiest day in the window/, 'a month is read over days');
  const hours = discoveredView({ found: foundOf(DISCOVERY_ROWS, { applied_bucket: 'hour' }), prompts: promptsOf() });
  assert.match(hours.notes.join(' '), /on its busiest hour in the window/);
});

test('without the prompts the column is empty and the tile says why, and a cut-short read leaves the missing ones unknown', () => {
  const none = discoveredView({ found: foundOf(), prompts: null });
  assert.deepEqual(cells(none).map((c) => c[5]), [null, null, null, null]);
  assert.equal(none.tiles[1].value.kind, 'absent');
  assert.match(none.notes.join(' '), /The prompts each application sent could not be read/);
  const cut = discoveredView({ found: foundOf(), prompts: promptsOf(PROMPT_ROWS, { truncated: true }) });
  assert.deepEqual(cells(cut).map((c) => c[5]), [30, null, 812, null], 'a tool the read left out is not known to be silent');
  assert.equal(cut.tiles[1].value.text, '0');
});

test('a window with nothing found says so, and a scan that could not be read is a dash', () => {
  const view = discoveredView({ found: foundOf([], { extras: {} }), prompts: promptsOf([]) });
  assert.equal(table(view).emptyText, 'Discovery found no AI application on a device in this window.');
  assert.equal(table(view).summary, undefined);
  assert.equal(view.tiles[0].value.kind, 'absent');
  assert.deepEqual([view.tiles[2].value.kind, view.tiles[2].note], ['absent', 'Where the inventory scan stands could not be read.']);
});

test('the Discovered tab reads what was found beside the prompts per tool, and renders under Insights', async () => {
  const transport = transportWith();
  const dashboard = createDashboard({ api: createQueryApi({ transport }), now: NOW });
  const { view } = await dashboard.load('insights', { preset: 'd7', filters: { view: 'discovered' } });
  const found = transport.sent.find((b) => b.source === 'mart.agg_discovery_period');
  assert.deepEqual([found.bucket, found.dimensions, found.measures, found.limit], ['month', ['tool', 'discovery_type', 'item'], ['devices'], 1999]);
  const prompts = transport.sent.find((b) => b.source === 'mart.v_tool_usage');
  assert.deepEqual([prompts.bucket, prompts.dimensions, prompts.measures], ['month', ['tool'], ['submissions']]);
  assert.equal(transport.sent.indexOf(prompts) < transport.sent.indexOf(found), true, 'the prompts are read first, so the freshness shown is discovery\'s');
  assert.equal(view.source, 'mart.agg_discovery_period');
  assert.deepEqual(cells(view).map((c) => [c[0], c[5]]), [['Ollama', 30], ['Unrecognised tool', 0], ['Claude Code', 812], ['ChatGPT (desktop)', 0]]);
  const html = renderScreen(view, {});
  assert.match(html, /<h3>Found on devices<\/h3>/);
  assert.match(html, /<tr class="row-link" tabindex="0" data-href="#tools\?tool=app%3Aollama">/);

  const refused = createDashboard({ api: createQueryApi({ transport: transportWith({ prompts: 'refused' }) }), now: NOW });
  const without = (await refused.load('insights', { filters: { view: 'discovered' } })).view;
  assert.equal(without.tiles[1].value.kind, 'absent', 'a refused prompts read leaves the page standing');
});

test('Insights switches to Discovered beside its window', async () => {
  const doc = fakeDocument('#insights?view=discovered&preset=d30');
  await boot({ document: doc, api: createQueryApi({ transport: transportWith() }), admin: null, session: null });
  const html = doc.html('app');
  assert.match(html, /<a class="seg-item" href="#insights\?preset=d30&amp;view=discovered" aria-current="true">Discovered<\/a>/);
  assert.match(html, /Not scanned/);
});
