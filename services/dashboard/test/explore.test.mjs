// explore.test.mjs — the Explore page: search, filters, results, and one result's detail.
//
// The page adds no read path, so most of what is asserted here is that it cannot do what the rest
// of the dashboard is forbidden from doing: drop a filter it does not recognise, turn a list into
// a ranking, show content, or lose the coverage strip. The controller is DOM-free, so a whole
// search is driven here against a fake query-api without a browser.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import {
  EXPLORE_DATASETS, EXPLORE_DATASET_IDS, parseExploreQuery, formatExploreQuery,
  buildExploreRequest, encodeExploreHash, decodeExploreHash,
} from '../src/explore-model.js';
import { createExploreFake, buildExploreSample } from './explore-fake.mjs';
import { createExplorer } from '../src/explore-app.js';
import {
  renderExploreResults, renderExploreDetail, renderExploreRail, renderExploreProblems, renderExploreSummary,
} from '../src/explore-render.js';
import { createQueryApi } from '../src/transport.js';
import { COLLECTOR_DETAILS, TEMPLATES } from '../src/vocab.js';
import { ROOT } from './helpers.mjs';

const NOW = new Date('2026-10-01T12:00:00Z');
const now = () => NOW;

function explorerFor(scenario = 'realistic') {
  const stub = createExploreFake({ now, scenario });
  const sent = [];
  const api = createQueryApi({
    transport: {
      async send(body) {
        sent.push(body);
        return stub.send(body);
      },
    },
  });
  return { explorer: createExplorer({ api, now }), stub, sent };
}

// ── the vocabulary ───────────────────────────────────────────────────────────────────────────

test('every filter a dataset offers is a parameter its template admits', () => {
  for (const id of EXPLORE_DATASET_IDS) {
    const admitted = TEMPLATES[EXPLORE_DATASETS[id].questionId]?.params ?? [];
    const outside = EXPLORE_DATASETS[id].fields.map((f) => f.name).filter((name) => !admitted.includes(name));
    assert.deepEqual(outside, [], `${id} offers a filter its template would refuse`);
    assert.ok(TEMPLATES[EXPLORE_DATASETS[id].questionId], `${id} reads a real template`);
  }
});

test('only list templates are searchable: no dataset reads an aggregate or a per-person series', () => {
  const used = EXPLORE_DATASET_IDS.map((id) => EXPLORE_DATASETS[id].questionId).sort();
  assert.deepEqual(used, ['q10_audit_trail', 'q5_findings', 'q7_devices', 'q8_activity']);
});

// ── the query bar ────────────────────────────────────────────────────────────────────────────

test('a query parses into closed filters, including a quoted value', () => {
  const parsed = parseExploreQuery('tool:tls_b6681b043244c43f action:blocked department:"Customer Success"', EXPLORE_DATASETS.events);
  assert.deepEqual(parsed.problems, []);
  assert.deepEqual({ ...parsed.filters }, { tool: 'tls_b6681b043244c43f', action: 'blocked', department: 'Customer Success' });
  assert.equal(formatExploreQuery(parsed.filters, EXPLORE_DATASETS.events), 'tool:tls_b6681b043244c43f action:blocked department:"Customer Success"');
});

test('free text is refused with the reason, never dropped', () => {
  const parsed = parseExploreQuery('salary tool:tls_b6681b043244c43f', EXPLORE_DATASETS.events);
  assert.equal(parsed.problems.length, 1);
  assert.equal(parsed.problems[0].code, 'free_text');
  assert.match(parsed.problems[0].message, /no text search/);
  assert.match(parsed.problems[0].fix, /field:value/);
});

test('an unknown field, an unknown value, a repeat and an empty value are each a problem', () => {
  const events = EXPLORE_DATASETS.events;
  assert.equal(parseExploreQuery('severity:high', events).problems[0].code, 'unknown_field');
  assert.match(parseExploreQuery('severity:high', events).problems[0].fix, /Findings/, 'the refusal says where the field does exist');
  assert.equal(parseExploreQuery('action:deleted', events).problems[0].code, 'unknown_value');
  assert.equal(parseExploreQuery('tool:a tool:b', events).problems[0].code, 'duplicate_field');
  assert.equal(parseExploreQuery('tool:', events).problems[0].code, 'missing_value');
  assert.equal(parseExploreQuery(`tool:${'x'.repeat(300)}`, events).problems[0].code, 'value_too_long');
});

test('a query with a problem is not sent, and the page says nothing was', async () => {
  const { explorer, sent } = explorerFor();
  await explorer.setQuery('payroll export');
  assert.equal(sent.length, 0, 'no request left the page');
  assert.equal(explorer.state.status, 'blocked');
  assert.match(renderExploreResults(explorer.state), /Nothing was sent/);
  assert.match(renderExploreProblems(explorer.state), /Not searched/);
});

// ── requests ─────────────────────────────────────────────────────────────────────────────────

test('every request is a template of the closed DSL, with no tenant and no text predicate', async () => {
  const { explorer, sent } = explorerFor();
  for (const id of EXPLORE_DATASET_IDS) {
    await explorer.setDataset(id);
    await explorer.run();
  }
  await explorer.setDataset('events');
  await explorer.open(explorer.state.rows[0].submission_id);
  assert.ok(sent.length >= 5);
  for (const body of sent) {
    assert.equal(body.query_version, '1');
    assert.ok(typeof body.template === 'string');
    assert.ok(!/tenant|search|match|snippet|sql/i.test(JSON.stringify(body)), JSON.stringify(body).slice(0, 160));
  }
});

test('the devices read carries no window, and the others carry the one that was chosen', () => {
  const window = { from: '2026-09-24T12:00:00.000Z', to: '2026-10-01T12:00:00.000Z' };
  const devices = buildExploreRequest({ dataset: EXPLORE_DATASETS.devices, filters: { liveness: 'stale' }, window: null });
  assert.ok(!('window' in devices.params));
  assert.equal(devices.params.liveness, 'stale');
  const events = buildExploreRequest({ dataset: EXPLORE_DATASETS.events, filters: {}, window });
  assert.deepEqual(events.params.window, window);
});

test('the default events request hides client-generated requests, and the toggle drops the predicate', () => {
  const window = { from: '2026-09-24T12:00:00.000Z', to: '2026-10-01T12:00:00.000Z' };
  const hidden = buildExploreRequest({ dataset: EXPLORE_DATASETS.events, filters: {}, window });
  assert.equal(hidden.params.prompt_kind_not, 'client_generated', 'hidden by default');
  const shown = buildExploreRequest({ dataset: EXPLORE_DATASETS.events, filters: {}, window, includeClientGenerated: true });
  assert.ok(!('prompt_kind_not' in shown.params), 'the toggle removes the predicate');
  // A person's own prompt_kind choice governs, so the default-hide never contradicts it.
  const named = buildExploreRequest({ dataset: EXPLORE_DATASETS.events, filters: { prompt_kind: 'client_generated' }, window });
  assert.ok(!('prompt_kind_not' in named.params));
  assert.equal(named.params.prompt_kind, 'client_generated');
  // Other datasets are untouched.
  const findings = buildExploreRequest({ dataset: EXPLORE_DATASETS.findings, filters: {}, window });
  assert.ok(!('prompt_kind_not' in findings.params));
});

// ── filters and windows ──────────────────────────────────────────────────────────────────────

test('every filter and the window reach the read', async () => {
  const { explorer } = explorerFor();
  await explorer.restore('#events?window=d30&tool=tls_b6681b043244c43f&action=blocked');
  assert.equal(explorer.state.status, 'ready');
  assert.ok(explorer.state.rows.length > 0, 'the fake has blocked tls_b6681b043244c43f events');
  for (const row of explorer.state.rows) {
    assert.equal(row.tool, 'tls_b6681b043244c43f');
    assert.equal(row.action, 'blocked');
    assert.ok(Date.parse(row.received_at) >= NOW.getTime() - 30 * 86_400_000);
  }
  await explorer.setFilter('class', 'credential');
  for (const row of explorer.state.rows) assert.ok(row.labels.some((l) => l.class === 'credential'));
});

test('a narrower window returns no more rows than a wider one', async () => {
  const { explorer } = explorerFor();
  await explorer.restore('#events?window=h24');
  const day = explorer.state.rows.length;
  await explorer.setWindow('d7');
  const week = explorer.state.rows.length;
  assert.ok(day <= week && week > 0);
  const sample = buildExploreSample(NOW);
  assert.ok(sample.events.length > 100 && sample.findings.length > 10);
});

test('prompt_kind and prompt_kind_not reach the read', async () => {
  const { explorer } = explorerFor();
  await explorer.restore('#events?window=d30&prompt_kind=client_generated');
  assert.equal(explorer.state.rows.length, 0, 'no event in the fake is client_generated');
  await explorer.restore('#events?window=d30&prompt_kind=unknown');
  assert.ok(explorer.state.rows.length > 0, 'events in the fake read as unknown');
});

// ── paging ───────────────────────────────────────────────────────────────────────────────────

test('paging repeats the frozen window and ends only on a null cursor', async () => {
  const { explorer, sent } = explorerFor();
  await explorer.restore('#events?window=d30');
  assert.equal(explorer.state.rows.length, 50);
  assert.ok(explorer.state.result.page.next_cursor);
  let guard = 0;
  while (explorer.state.result.page.next_cursor && guard < 10) {
    await explorer.loadMore();
    guard += 1;
  }
  const windows = new Set(sent.map((b) => JSON.stringify(b.params.window)));
  assert.equal(windows.size, 1, 'every page asked for the same window');
  const ids = explorer.state.rows.map((r) => r.submission_id);
  assert.equal(new Set(ids).size, ids.length, 'no row was served twice');
  assert.equal(explorer.state.result.page.next_cursor, null);
  assert.match(renderExploreResults(explorer.state), /End of results/);
});

test('an expired cursor keeps the rows shown and offers a restart, not an offset', async () => {
  const { explorer } = explorerFor('cursor_expired');
  await explorer.restore('#events?window=d30');
  await explorer.loadMore();
  assert.equal(explorer.state.rows.length, 50, 'the first page is still on screen');
  assert.equal(explorer.state.moreProblem.resultState, 'cursor_expired');
  const html = renderExploreResults(explorer.state);
  assert.match(html, /Restart from page one/);
});

test('rows stay in the order the server returned them: there is no client-side sort', async () => {
  const { explorer, stub } = explorerFor();
  await explorer.restore('#events?window=d30');
  const expected = stub.sample.events.slice(0, 50).map((r) => r.submission_id);
  assert.deepEqual(explorer.state.rows.map((r) => r.submission_id), expected);
  const html = renderExploreResults(explorer.state);
  assert.ok(!/data-act="sort"|aria-sort/.test(html), 'no column offers a sort');
});

// ── states ───────────────────────────────────────────────────────────────────────────────────

test('a refusal is a state with its reason, never an empty list', async () => {
  for (const [scenario, expected] of [['busy', /Busy/], ['audit_unavailable', /Read not served/], ['too_broad', /Too broad to serve/]]) {
    const { explorer } = explorerFor(scenario);
    await explorer.restore('#events');
    assert.equal(explorer.state.status, 'refused', scenario);
    assert.equal(explorer.state.rows.length, 0);
    const html = renderExploreResults(explorer.state);
    assert.match(html, expected);
    assert.match(html, /No rows were served/);
    assert.ok(!/<table/.test(html), `${scenario} renders no table`);
  }
});

test('nothing found on partial coverage is not reported as "no data"', async () => {
  const { explorer } = explorerFor();
  await explorer.restore('#events?window=h24&tool=no_such_tool');
  assert.equal(explorer.state.result.resultState, 'coverage_degraded');
  const html = renderExploreResults(explorer.state);
  assert.match(html, /partial coverage/i);
  assert.match(html, /Clear 1 filter/);
  const adequate = explorerFor('empty');
  await adequate.explorer.restore('#events');
  assert.match(renderExploreResults(adequate.explorer.state), /We looked, coverage was adequate/);
});

test('both clocks are shown and the device clock is marked', async () => {
  const { explorer } = explorerFor();
  await explorer.restore('#events?window=d30');
  const html = renderExploreResults(explorer.state);
  assert.match(html, /Received \(server\)/);
  assert.match(html, /Occurred \(device\)/);
  assert.match(html, /x-device-clock/);
  assert.match(html, /possibly skewed/);
  assert.match(html, /before receipt/, 'a late flush is named as one');
});

test('device states render as themselves, and silence is not health', async () => {
  const { explorer } = explorerFor();
  await explorer.restore('#devices');
  const html = renderExploreResults(explorer.state);
  for (const value of ['reporting', 'stale', 'never_reported', 'revoked']) {
    assert.ok(html.includes(`>${value}<`), `${value} must appear`);
  }
  assert.equal(explorer.state.rows.length, new Set(explorer.state.rows.map((r) => r.device)).size, 'one row per device');
  await explorer.setFilter('liveness', 'never_reported');
  const silent = renderExploreResults(explorer.state);
  assert.ok(!/healthy/.test(silent));
  assert.equal(explorer.state.rows.length, 1);
  assert.equal(explorer.state.rows[0].collectors_reporting, 0, 'a silent device has no collector to count');
});

// ── detail ───────────────────────────────────────────────────────────────────────────────────

test('opening an event re-reads the one record and shows what its content state permits', async () => {
  const { explorer, sent } = explorerFor();
  await explorer.restore('#events?window=d30&content_state=uploaded');
  const row = explorer.state.rows[0];
  await explorer.open(row.submission_id);
  const last = sent[sent.length - 1];
  assert.equal(last.template, 'q9_event_detail');
  assert.equal(last.params.submission_id, row.submission_id);
  assert.equal(explorer.state.detail.status, 'ready');
  const html = renderExploreDetail(explorer.state);
  assert.match(html, /no content path behind it/, 'with no content path, the page says the prompt cannot be read');
  assert.match(html, /Observation routes/);
  assert.match(html, /audited read: entry/);
  assert.ok(html.includes(row.submission_id), 'the id is shown in full');
  for (const forbidden of ['content_excerpt', 'prompt_text', 'attachment_body', 'ciphertext', 'plaintext']) {
    assert.ok(!html.includes(forbidden), `detail must not render ${forbidden}`);
  }
  assert.match(renderExploreResults(explorer.state), /aria-selected="true"/);
});

test('each content state gives a different answer in the detail panel', async () => {
  const answers = new Set();
  for (const contentState of ['not_captured', 'local_only', 'uploaded', 'shredded']) {
    const { explorer } = explorerFor();
    await explorer.restore(`#events?window=d30&content_state=${contentState}`);
    assert.ok(explorer.state.rows.length > 0, `the fake has a ${contentState} event`);
    await explorer.open(explorer.state.rows[0].submission_id);
    answers.add(/<section class="x-content">([\s\S]*?)<\/section>/.exec(renderExploreDetail(explorer.state))[1]);
  }
  assert.equal(answers.size, 4);
});

test('a finding opens the submission behind it and keeps the finding facts', async () => {
  const { explorer } = explorerFor();
  await explorer.restore('#findings?window=d30&severity=critical');
  assert.ok(explorer.state.rows.every((r) => r.severity === 'critical'));
  const row = explorer.state.rows[0];
  await explorer.open(EXPLORE_DATASETS.findings.rowKey(row));
  const html = renderExploreDetail(explorer.state);
  assert.match(html, /Severity at detection/);
  assert.ok(html.includes(row.submission_id));
});

test('a destroyed record is "no longer available" with its receipt, not a blank panel', async () => {
  const { explorer } = explorerFor('destroyed');
  await explorer.restore('#events');
  await explorer.open(explorer.state.rows[0].submission_id);
  assert.equal(explorer.state.detail.status, 'refused');
  const html = renderExploreDetail(explorer.state);
  assert.match(html, /No longer available/);
  assert.match(html, /r_88/);
});

test('a device row lists every collector the device reported, with its state, cause and last report', async () => {
  const { explorer, sent, stub } = explorerFor();
  const tampered = stub.sample.collectors.find((r) => r.error_code === 'config_tampered');
  await explorer.restore('#devices');
  const row = explorer.state.rows.find((r) => r.device === tampered.device);
  await explorer.open(row.device);

  const last = sent[sent.length - 1];
  assert.deepEqual(last, {
    query_version: '1',
    source: 'ops.collector_state',
    filters: [{ field: 'device', op: 'eq', value: tampered.device }],
    limit: 100,
  });
  assert.equal(explorer.state.detail.collectors.status, 'ready');
  const html = renderExploreDetail(explorer.state);
  const table = html.slice(html.indexOf('<h3>Collectors</h3>'));
  for (const r of stub.sample.collectors.filter((c) => c.device === tampered.device)) {
    assert.ok(table.includes(`>${r.collector}<`), `${r.collector} has a row`);
  }
  assert.match(table, /<th scope="col">State<\/th><th scope="col">Cause<\/th><th scope="col">Last report<\/th>/);
  assert.ok(table.includes(COLLECTOR_DETAILS.config_tampered.replace(/'/g, '&#39;')), 'the cause is in words');
  assert.ok(table.includes('title="config_tampered"'), 'the detail code stays beside its words');
  assert.match(table, /<span class="x-chip x-chip-warn">degraded<\/span>/);
  // Switched off by policy is not a fault: it says so, untinted, and gives no cause.
  const off = table.slice(table.indexOf('>tool_config_cursor<'));
  assert.match(off, /^>tool_config_cursor<\/span><\/td><td><span class="x-chip">Off in policy<\/span><\/td><td><span class="x-absent">none<\/span>/);
  assert.ok(!/x-chip-bad">absent/.test(off.slice(0, off.indexOf('</tr>'))), 'a collector off in policy is not shown as absent');
  assert.ok(!/audited read/.test(html), 'the collectors read is not audited');
});

test('a device whose collectors cannot be read says so beside the row it opened', async () => {
  const { explorer, stub } = explorerFor();
  await explorer.restore('#devices');
  const row = explorer.state.rows[0];
  stub.setScenario('busy');
  await explorer.open(row.device);
  assert.equal(explorer.state.detail.collectors.status, 'refused');
  const html = renderExploreDetail(explorer.state);
  assert.match(html, /<h3>Collectors<\/h3><div class="x-state x-state-refusal" role="alert">/);
  assert.ok(html.includes(row.device), 'the row itself is still shown');
});

test('an audit row opens without a second read', async () => {
  const { explorer, sent } = explorerFor();
  await explorer.restore('#audit?window=d30');
  const before = sent.length;
  await explorer.open(String(explorer.state.rows[0].audit_seq));
  assert.equal(sent.length, before);
  assert.match(renderExploreDetail(explorer.state), /Row hash/);
});

test('a newer search discards an older answer that arrives late', async () => {
  const stub = createExploreFake({ now });
  let release;
  const gate = new Promise((resolve) => { release = resolve; });
  let calls = 0;
  const api = createQueryApi({
    transport: {
      async send(body) {
        calls += 1;
        if (calls === 1) await gate;
        return stub.send(body);
      },
    },
  });
  const explorer = createExplorer({ api, now });
  const slow = explorer.setQuery('tool:tls_11574658dafb8805');
  await explorer.setQuery('tool:tls_b6681b043244c43f');
  release();
  await slow;
  assert.ok(explorer.state.rows.length > 0);
  assert.ok(explorer.state.rows.every((r) => r.tool === 'tls_b6681b043244c43f'), 'the late answer did not overwrite the newer one');
});

// ── links ────────────────────────────────────────────────────────────────────────────────────

test('the address carries the whole search and round-trips', async () => {
  const { explorer } = explorerFor();
  await explorer.restore('#findings?window=d30&review_state=open&tool=tls_11574658dafb8805');
  assert.equal(explorer.hash(), '#findings?window=d30&review_state=open&tool=tls_11574658dafb8805');
  const decoded = decodeExploreHash(explorer.hash());
  assert.equal(decoded.dataset.id, 'findings');
  assert.deepEqual({ ...decoded.filters }, { review_state: 'open', tool: 'tls_11574658dafb8805' });
  assert.equal(encodeExploreHash({ dataset: EXPLORE_DATASETS.events, windowPreset: 'd7', filters: {}, open: null }), '#events');
});

test('a link carrying a filter this page does not know is refused, not broadened', async () => {
  const { explorer, sent } = explorerFor();
  await explorer.restore('#events?salary=high');
  assert.equal(sent.length, 0);
  assert.equal(explorer.state.status, 'blocked');
  assert.equal(explorer.state.problems[0].code, 'unknown_field');
});

test('the include toggle round-trips in the address without being read as a filter', async () => {
  const { explorer } = explorerFor();
  await explorer.restore('#events?window=d30');
  assert.equal(explorer.hash(), '#events?window=d30');
  await explorer.setIncludeClientGenerated(true);
  assert.equal(explorer.hash(), '#events?window=d30&include=1');
  const decoded = decodeExploreHash(explorer.hash());
  assert.equal(decoded.includeClientGenerated, true);
  assert.deepEqual(decoded.problems, [], 'include is reserved page state, not an unknown filter');
  await explorer.setIncludeClientGenerated(false);
  assert.equal(explorer.hash(), '#events?window=d30');
});

test('the rail renders the include toggle for events, with the pressed state it is in', async () => {
  const { explorer } = explorerFor();
  await explorer.restore('#events?window=d30');
  let html = renderExploreRail(explorer.state);
  assert.match(html, /data-act="include"/);
  assert.match(html, /Include client-generated requests/);
  assert.match(html, /aria-pressed="false"/);
  await explorer.setIncludeClientGenerated(true);
  html = renderExploreRail(explorer.state);
  assert.match(html, /aria-pressed="true"/);
  await explorer.setDataset('findings');
  assert.ok(!renderExploreRail(explorer.state).includes('data-act="include"'), 'only events offers the toggle');
});

test('switching dataset carries the filters both have and drops the rest by name', async () => {
  const { explorer } = explorerFor();
  await explorer.restore('#events?window=d30&tool=tls_b6681b043244c43f&action=blocked');
  await explorer.setDataset('findings');
  assert.deepEqual({ ...explorer.state.filters }, { tool: 'tls_b6681b043244c43f' });
  assert.equal(explorer.state.queryText, 'tool:tls_b6681b043244c43f');
  assert.match(renderExploreRail(explorer.state), /Clear 1/);
  assert.match(renderExploreSummary(explorer.state), /Audited read, entry/);
});

test('the page offers the export only for events and findings, and no external link', async () => {
  const template = readFileSync(join(ROOT, 'explore.html'), 'utf8');
  const sources = ['explore-render.js', 'explore-app.js', 'explore-model.js'].map((f) => readFileSync(join(ROOT, 'src', f), 'utf8')).join('\n');
  // The download is a server-minted link: the client names no .csv file and no download attribute,
  // and loads nothing from another host.
  assert.ok(!/href="http/.test(template + sources));
  assert.ok(!/\bdownload=|\.csv\b/.test(sources));

  const ready = { dataset: 'events', status: 'ready', rows: [], result: { page: {} }, filters: {}, includeClientGenerated: false, windowPreset: 'd7', export: { status: 'idle' } };
  assert.match(renderExploreSummary(ready), /Export CSV/, 'events offer the export');
  assert.match(renderExploreSummary({ ...ready, dataset: 'findings' }), /Export CSV/, 'findings offer the export');
  assert.ok(!/Export CSV/.test(renderExploreSummary({ ...ready, dataset: 'devices' })), 'devices do not export');
  assert.ok(!/Export CSV/.test(renderExploreSummary({ ...ready, dataset: 'audit' })), 'the audit trail does not export');
});

test('exporting the list follows the server link and reports the refusal otherwise', async () => {
  const stub = createExploreFake({ now, scenario: 'realistic' });
  const api = createQueryApi({ transport: { async send(body) { return stub.send(body); } } });

  const saved = [];
  const ex = createExplorer({
    api,
    export: { async list() { return { state: 'available', download_url: '/v1/export/x', row_count: 7 }; } },
    save: (url) => saved.push(url),
    now,
  });
  await ex.run();
  await ex.exportList();
  assert.deepEqual(saved, ['/v1/export/x'], 'the link was handed to the browser');
  assert.equal(ex.state.export.status, 'saved');

  const refused = createExplorer({
    api,
    export: { async list() { return { state: 'refused', error: { code: 'query_too_broad', message: 'too large' } }; } },
    save: () => {},
    now,
  });
  await refused.run();
  await refused.exportList();
  assert.equal(refused.state.export.status, 'refused');
  assert.equal(refused.state.export.problem.code, 'query_too_broad');
});

test('the stylesheet follows the system colour scheme and honours reduced motion', () => {
  const css = readFileSync(join(ROOT, 'explore.css'), 'utf8');
  assert.match(css, /prefers-color-scheme: dark/);
  assert.match(css, /prefers-reduced-motion: no-preference/);
  const outside = css.replace(/@media \(prefers-reduced-motion: no-preference\) \{[\s\S]*?\n\}/, '');
  assert.ok(!/animation:/.test(outside), 'every animation is inside the reduced-motion gate');
});

test('explore.html loads its modules from src/ and fetches nothing from another host', () => {
  const html = readFileSync(join(ROOT, 'explore.html'), 'utf8');
  assert.match(html, /import \{ bootExplore \} from '\.\/src\/explore-app\.js'/);
  assert.ok(html.includes('<link rel="stylesheet" href="explore.css">'));
  assert.ok(!/https?:\/\//.test(html.replace(/https?:\/\/www\.w3\.org[^"]*/g, '')), 'no remote URL');
});
