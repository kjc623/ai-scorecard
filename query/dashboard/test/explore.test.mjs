// explore.test.mjs — the Explore page: search, filters, results, and one result's detail.
//
// The page adds no read path, so most of what is asserted here is that it cannot do what the rest
// of the dashboard is forbidden from doing: drop a filter it does not recognise, turn a list into
// a ranking, show content, or lose the coverage strip. The controller is DOM-free, so a whole
// search is driven here against the sample transport without a browser.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import {
  EXPLORE_DATASETS, EXPLORE_DATASET_IDS, parseExploreQuery, formatExploreQuery, suggestExploreTerms,
  applyExploreSuggestion, buildExploreRequest, encodeExploreHash, decodeExploreHash, exploreFieldsOutsideTemplate,
} from '../src/explore-model.js';
import { createExploreStub, buildExploreSample } from '../src/explore-stub.js';
import { createExplorer } from '../src/explore-app.js';
import {
  renderExploreResults, renderExploreDetail, renderExploreRail, renderExploreProblems, renderExploreSummary,
} from '../src/explore-render.js';
import { createQueryApi } from '../src/transport.js';
import { TEMPLATES } from '../src/vocab.js';
import { buildExploreHtml, EXPLORE_PATH } from '../tools/build-index.mjs';
import { ROOT } from './helpers.mjs';

const NOW = new Date('2026-10-01T12:00:00Z');
const now = () => NOW;

function explorerFor(scenario = 'realistic') {
  const stub = createExploreStub({ now, scenario });
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
    assert.deepEqual(exploreFieldsOutsideTemplate(EXPLORE_DATASETS[id]), [], `${id} offers a filter its template would refuse`);
    assert.ok(TEMPLATES[EXPLORE_DATASETS[id].questionId], `${id} reads a real template`);
  }
});

test('only list templates are searchable: no dataset reads an aggregate or a per-person series', () => {
  const used = EXPLORE_DATASET_IDS.map((id) => EXPLORE_DATASETS[id].questionId).sort();
  assert.deepEqual(used, ['q10_audit_trail', 'q5_findings', 'q7_devices', 'q8_activity']);
});

// ── the query bar ────────────────────────────────────────────────────────────────────────────

test('a query parses into closed filters, including a quoted value', () => {
  const parsed = parseExploreQuery('tool:claude_web action:blocked department:"Customer Success"', EXPLORE_DATASETS.events);
  assert.deepEqual(parsed.problems, []);
  assert.deepEqual({ ...parsed.filters }, { tool: 'claude_web', action: 'blocked', department: 'Customer Success' });
  assert.equal(formatExploreQuery(parsed.filters, EXPLORE_DATASETS.events), 'tool:claude_web action:blocked department:"Customer Success"');
});

test('§14.15 free text is refused with the reason, never dropped', () => {
  const parsed = parseExploreQuery('salary tool:claude_web', EXPLORE_DATASETS.events);
  assert.equal(parsed.problems.length, 1);
  assert.equal(parsed.problems[0].code, 'free_text');
  assert.match(parsed.problems[0].message, /no text search/);
  assert.match(parsed.problems[0].fix, /field:value/);
});

test('§14.15 an unknown field, an unknown value, a repeat and an empty value are each a problem', () => {
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

test('completions offer unused fields, then a closed vocabulary', () => {
  const events = EXPLORE_DATASETS.events;
  const fields = suggestExploreTerms('tool:claude_web ', 16, events);
  assert.ok(fields.items.every((i) => i.kind === 'field'));
  assert.ok(!fields.items.some((i) => i.label === 'tool:'), 'a field already used is not offered again');
  const values = suggestExploreTerms('action:b', 8, events);
  assert.deepEqual(values.items.map((i) => i.label), ['blocked']);
  const applied = applyExploreSuggestion('action:b', values, values.items[0]);
  assert.equal(applied.text, 'action:blocked ');
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

// ── the sample transport behaves like the API ────────────────────────────────────────────────

test('the sample transport applies every filter and the window', async () => {
  const { explorer } = explorerFor();
  await explorer.restore('#events?window=d30&tool=claude_web&action=blocked');
  assert.equal(explorer.state.status, 'ready');
  assert.ok(explorer.state.rows.length > 0, 'the sample has blocked claude_web events');
  for (const row of explorer.state.rows) {
    assert.equal(row.tool, 'claude_web');
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

test('the sample transport honours prompt_kind and prompt_kind_not', async () => {
  const { explorer } = explorerFor();
  await explorer.restore('#events?window=d30&prompt_kind=client_generated');
  assert.equal(explorer.state.rows.length, 0, 'no sample event is client_generated');
  await explorer.restore('#events?window=d30&prompt_kind=unknown');
  assert.ok(explorer.state.rows.length > 0, 'sample events read as unknown');
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

test('§14.10 both clocks are shown and the device clock is marked', async () => {
  const { explorer } = explorerFor();
  await explorer.restore('#events?window=d30');
  const html = renderExploreResults(explorer.state);
  assert.match(html, /Received \(server\)/);
  assert.match(html, /Occurred \(device\)/);
  assert.match(html, /x-device-clock/);
  assert.match(html, /possibly skewed/);
  assert.match(html, /before receipt/, 'a late flush is named as one');
});

test('§14.8 and §14.9 device states render as themselves, and silence is not health', async () => {
  const { explorer } = explorerFor();
  await explorer.restore('#devices');
  const html = renderExploreResults(explorer.state);
  for (const value of ['reporting', 'stale', 'never_reported', 'revoked', 'healthy', 'degraded', 'absent', 'tampered']) {
    assert.ok(html.includes(`>${value}<`), `${value} must appear`);
  }
  await explorer.setFilter('liveness', 'never_reported');
  const silent = renderExploreResults(explorer.state);
  assert.ok(!/healthy/.test(silent));
  assert.match(silent, />unknown</, 'a null collector state is unknown, not blank');
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
    assert.ok(explorer.state.rows.length > 0, `the sample has a ${contentState} event`);
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

test('a device or audit row opens without a second read', async () => {
  const { explorer, sent } = explorerFor();
  await explorer.restore('#audit?window=d30');
  const before = sent.length;
  await explorer.open(String(explorer.state.rows[0].audit_seq));
  assert.equal(sent.length, before);
  assert.match(renderExploreDetail(explorer.state), /Row hash/);
});

test('a newer search discards an older answer that arrives late', async () => {
  const stub = createExploreStub({ now });
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
  const slow = explorer.setQuery('tool:chatgpt_web');
  await explorer.setQuery('tool:claude_web');
  release();
  await slow;
  assert.ok(explorer.state.rows.length > 0);
  assert.ok(explorer.state.rows.every((r) => r.tool === 'claude_web'), 'the late answer did not overwrite the newer one');
});

// ── links ────────────────────────────────────────────────────────────────────────────────────

test('the address carries the whole search and round-trips', async () => {
  const { explorer } = explorerFor();
  await explorer.restore('#findings?window=d30&review_state=open&tool=chatgpt_web');
  assert.equal(explorer.hash(), '#findings?window=d30&review_state=open&tool=chatgpt_web');
  const decoded = decodeExploreHash(explorer.hash());
  assert.equal(decoded.dataset.id, 'findings');
  assert.deepEqual({ ...decoded.filters }, { review_state: 'open', tool: 'chatgpt_web' });
  assert.equal(encodeExploreHash({ dataset: EXPLORE_DATASETS.events, windowPreset: 'd7', filters: {}, open: null }), '#events');
});

test('§14.15 a link carrying a filter this page does not know is refused, not broadened', async () => {
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
  await explorer.restore('#events?window=d30&tool=claude_web&action=blocked');
  await explorer.setDataset('findings');
  assert.deepEqual({ ...explorer.state.filters }, { tool: 'claude_web' });
  assert.equal(explorer.state.queryText, 'tool:claude_web');
  assert.match(renderExploreRail(explorer.state), /Clear 1/);
  assert.match(renderExploreSummary(explorer.state), /Audited read, entry/);
});

// ── the generated page ───────────────────────────────────────────────────────────────────────

test('explore.html is in sync with src/ (regenerate with: node tools/build-index.mjs)', () => {
  const current = readFileSync(EXPLORE_PATH, 'utf8').replace(/\r\n/g, '\n');
  assert.equal(current, buildExploreHtml(), 'explore.html is stale; run node tools/build-index.mjs');
});

test('explore.html is self-contained and its inline module loads', async () => {
  const html = readFileSync(EXPLORE_PATH, 'utf8');
  assert.ok(!/<script[^>]+src=/.test(html), 'no external script tag');
  assert.ok(html.includes('<link rel="stylesheet" href="explore.css">'));
  assert.ok(!/https?:\/\//.test(html.replace(/https?:\/\/www\.w3\.org[^"]*/g, '')), 'no remote URL');
  const source = /<script type="module">([\s\S]*?)<\/script>/.exec(html)[1];
  assert.ok(!/^\s*import\s/m.test(source), 'the inline module imports nothing');
  const mod = await import(`data:text/javascript;base64,${Buffer.from(source, 'utf8').toString('base64')}`);
  const explorer = mod.createExplorer({ api: mod.createQueryApi({ transport: mod.createExploreStub({ now }) }), now });
  await explorer.restore('#events?window=d30&action=warned');
  assert.ok(explorer.state.rows.length > 0);
  assert.match(mod.renderExploreResults(explorer.state), /warned/);
});

test('the page offers no export, no download and no external link', () => {
  const template = readFileSync(join(ROOT, 'tools', 'explore.template.html'), 'utf8');
  const sources = ['explore-render.js', 'explore-app.js'].map((f) => readFileSync(join(ROOT, 'src', f), 'utf8')).join('\n');
  assert.ok(!/href="http/.test(template + sources));
  assert.ok(!/download|export csv/i.test(template));
  assert.ok(!/\bdownload=|\.csv\b/.test(sources));
});

test('the stylesheet follows the system colour scheme and honours reduced motion', () => {
  const css = readFileSync(join(ROOT, 'explore.css'), 'utf8');
  assert.match(css, /prefers-color-scheme: dark/);
  assert.match(css, /prefers-reduced-motion: no-preference/);
  const outside = css.replace(/@media \(prefers-reduced-motion: no-preference\) \{[\s\S]*?\n\}/, '');
  assert.ok(!/animation:/.test(outside), 'every animation is inside the reduced-motion gate');
});
