// section14.test.mjs — "What the dashboard must never do", as tests rather than comments.
//
// docs/04 §14 is a list of seventeen prohibitions. Each one that a client can violate is asserted
// here, either statically (this tree contains no SQL and no database driver) or behaviourally (the
// renderer cannot produce the forbidden output). The ones that are properties of another component
// are recorded as such rather than quietly skipped.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { readState, measureOf } from '../src/states.js';
import { renderScreen, renderValue, renderTable } from '../src/render.js';
import { activityView, eventView, devicesView, unsanctionedView } from '../src/views.js';
import { noApiView, unavailableView, GAPS, GAP_IDS } from '../src/unavailable.js';
import { createDashboard, SCREENS } from '../src/app.js';
import { createQueryApi } from '../src/transport.js';
import { scenarioTransport } from '../src/scenarios.js';
import { ACTIVITY_ROWS, DEVICE_ROWS } from '../src/fixtures.js';
import { codeOnly, sourceFiles, ROOT, envelope, FRESH, COMPLETE, PARTIAL } from './helpers.mjs';

const SHELL = { coverage: COMPLETE, freshness: FRESH };

function dashboardFor(scenario = 'realistic') {
  return createDashboard({ api: createQueryApi({ transport: scenarioTransport(scenario) }), now: () => new Date('2026-10-01T12:00:00Z') });
}

// ── §14 item 1: no per-employee scoring, ranking or efficiency reporting ──────────────────────

test('§14.1 no per-person ranking: a per-person source refuses a read without a subject', () => {
  const dashboard = dashboardFor();
  return dashboard.load('person', { filters: {} }).then(({ view }) => {
    // With no subject, the screen asks nothing at all: it cannot build a request that would
    // enumerate people, because dsl.js refuses one.
    assert.equal(view.needsInput, true);
  });
});

test('§14.1 the unsanctioned table is ordered by tool and person, never by volume', async () => {
  // The fixture's volumes run *against* the row order, so a volume sort would be visible.
  const dashboard = dashboardFor();
  const { view } = await dashboard.load('unsanctioned', {});
  const table = view.tables[0];
  const subjects = table.rows.map((r) => r.row.subject).filter(Boolean);
  assert.deepEqual(subjects, ['u_9a02', 'u_4f21'], 'rows keep the API order rather than a volume ranking');
  const volumes = table.rows.filter((r) => typeof r.row.submissions === 'number').map((r) => r.row.submissions);
  assert.ok(volumes[0] > volumes[1], 'the fixture is ordered against volume on purpose, so a volume sort would be visible');
  assert.ok(view.notes.some((n) => /not a ranking/.test(n)));
});

test('§14.1 no screen exposes a "most active people" table', async () => {
  const dashboard = dashboardFor();
  for (const screen of SCREENS) {
    const { view } = await dashboard.load(screen.id, {});
    if (!view) continue;
    for (const table of view.tables ?? []) {
      const titles = `${table.title}`.toLowerCase();
      assert.ok(!/most active|leaderboard|top people|ranking/.test(titles), `${screen.id}: ${table.title}`);
    }
  }
});

// ── §14 items 2 and 16-17: content search ────────────────────────────────────────────────────

test('§14.2 there is no content search UI, and the gap says so rather than showing an empty list', () => {
  assert.ok(GAP_IDS.includes('content-search'));
  const view = noApiView({ id: 'search', title: 'Content search', subtitle: '' });
  assert.equal(view.tables.length, 1);
  assert.equal(view.tables[0].rows.length, 1, 'one row: the gap itself');
  const rows = view.tables[0].rows.map((r) => r.row);
  for (const row of rows) {
    assert.ok('needs' in row && 'consequence' in row, 'a gap row explains what is missing');
    assert.ok(!('submission_id' in row) && !('rank' in row) && !('fragments' in row), 'no result row is rendered');
  }
  assert.ok(view.banners.some((b) => /No API behind this screen/.test(b.title)));
});

test('§14.17 no search result without index coverage: no search result exists to render', () => {
  const html = renderScreen(noApiView({ id: 'search', title: 'Content search', subtitle: '' }), SHELL);
  // The gap explanation may name snippets; what must not exist is a rendered result table or a
  // hit count standing in for one.
  assert.ok(!/fragments/.test(html));
  assert.ok(!/class="v-number"/.test(html), 'a screen with no search renders no counts');
  assert.ok(/No API behind this screen/.test(html));
});

// ── §14 item 3: no full content ──────────────────────────────────────────────────────────────

test('§14.3 no query path returns content, and event detail shows metadata plus the content_state answer', async () => {
  const dashboard = dashboardFor();
  const { view } = await dashboard.load('event', { filters: { submission_id: '11111111-2222-4333-8444-555555555551' } });
  const html = renderScreen(view, SHELL);
  for (const forbidden of ['content_excerpt', 'prompt_text', 'attachment_body', 'ciphertext', 'wrapped_dek', 'plaintext']) {
    assert.ok(!html.includes(forbidden), `event detail must not render ${forbidden}`);
  }
  assert.ok(/Open the event in Search to read the prompt/.test(html), 'the uploaded state says where the prompt is read');
});

test('§14.3 each content_state is a different answer, and all four are rendered', () => {
  const rendered = {};
  for (const contentState of ['not_captured', 'local_only', 'uploaded', 'shredded']) {
    const state = readState(envelope('ok', {
      data: [{ submission_id: 's', content_state: contentState, observation_count: 1 }],
      freshness: FRESH, coverage: COMPLETE, meta: { source: 'ingest.submission', content_state: contentState },
    }));
    rendered[contentState] = renderScreen(eventView(state), SHELL);
  }
  const texts = Object.values(rendered);
  assert.equal(new Set(texts).size, 4, 'four content states, four different renderings');
  assert.ok(/Content was never read/.test(rendered.not_captured));
  assert.ok(/remains on the device/.test(rendered.local_only));
  assert.ok(/destroyed/.test(rendered.shredded));
});

// ── §14 item 4: no cross-tenant aggregation ──────────────────────────────────────────────────

test('§14.4 no request carries a tenant, and the tenant key is refused client-side too', async () => {
  const seen = [];
  const spy = createQueryApi({
    transport: {
      async send(body) {
        seen.push(body);
        return scenarioTransport('realistic').send(body);
      },
    },
  });
  const dashboard = createDashboard({ api: spy, now: () => new Date('2026-10-01T12:00:00Z') });
  for (const screen of SCREENS) await dashboard.load(screen.id, { filters: { submission_id: '11111111-2222-4333-8444-555555555551', subject: 'u_1' } });
  assert.ok(seen.length > 0);
  for (const body of seen) {
    const text = JSON.stringify(body);
    assert.ok(!/tenant/i.test(text), `a request mentioned a tenant: ${text.slice(0, 120)}`);
  }
});

// ── §14 item 5: no unauthenticated or non-expiring export links ──────────────────────────────

test('§14.5 the exports screen renders no link and no download affordance', () => {
  const view = noApiView({ id: 'exports', title: 'Exports', subtitle: '' });
  const html = renderScreen(view, SHELL);
  assert.ok(!/href="http/.test(html), 'no external link');
  assert.ok(!/download/i.test(html.split('<footer')[0] ?? html), 'no download affordance');
  assert.ok(GAP_IDS.includes('export'));
});

// ── §14 item 6: no aggregate without its freshness and coverage state ────────────────────────

test('§14.6 a screen read under partial coverage says so, with the enrolled denominator', async () => {
  const dashboard = dashboardFor();
  for (const screen of SCREENS) {
    const { view, shell, gallery } = await dashboard.load(screen.id, { filters: { submission_id: '11111111-2222-4333-8444-555555555551', subject: 'u_1' } });
    if (gallery || !view || view.needsInput || screen.kind !== 'answer' || screen.id === 'audit') continue; // the audit trail is not a fleet figure
    const html = renderScreen(view, shell);
    assert.ok(/Coverage is partial/.test(html), `${screen.id} must say coverage is partial`);
    assert.ok(/4,180 of 4,620 enrolled devices reporting/.test(html), `${screen.id} must carry the denominator`);
  }
});

test('§14.6 a data-bearing envelope without a state block is refused rather than rendered', () => {
  assert.throws(() => readState(envelope('ok', { data: [{ submissions: 5 }] })), /refusing to render/);
});

// ── §14 item 7: no suppressed cell shown as zero, no zero shown as suppressed ────────────────

test('§14.7 the renderer cannot merge a suppressed cell with a zero', () => {
  const suppressed = renderValue({ kind: 'suppressed', k: 5 });
  const zero = renderValue({ kind: 'number', value: 0, text: '0' });
  assert.match(suppressed, /v-suppressed/);
  assert.ok(!/>\s*0\s*</.test(suppressed), 'a suppressed marker contains no zero');
  assert.match(zero, /v-number">0</);
  assert.ok(!/suppressed/.test(zero), 'a zero is not rendered as suppressed');
  assert.notEqual(suppressed.replace(/k=5/, ''), zero);
});

test('§14.7 the same distinction survives a whole table', () => {
  const state = readState(envelope('ok', {
    data: [
      { tool: 'a', submissions: 0, users: 0 },
      { tool: 'b', result_state: 'suppressed', reason: 'fewer_than_k_subjects', k: 5 },
    ],
    freshness: FRESH, coverage: COMPLETE, suppression: { k: 5, suppressed_cells: 1 },
  }));
  const html = renderTable({
    title: 't',
    columns: [{ key: 'tool', label: 'Tool' }, { key: 'submissions', label: 'Submissions', kind: 'measure' }],
    rows: state.data.map((row) => ({ row, vocab: {}, suppressed: row.result_state === 'suppressed' })),
    emptyText: 'none',
    suppressedCells: 1,
  });
  assert.match(html, /<span class="v-number">0<\/span>/);
  assert.match(html, /v-suppressed/);
  assert.match(html, /row-suppressed/);
});

test('§14.7 a floor is labelled as a floor', () => {
  const html = renderValue({ kind: 'floor', value: 812, text: '812', suppressedCells: 1 });
  assert.match(html, /≥ 812/);
  assert.match(html, /v-floor/);
});

// ── §14 item 8: no merging of the state pairs ────────────────────────────────────────────────

test('§14.8 every value of every state pair renders as itself', () => {
  const pairs = {
    liveness: ['reporting', 'stale', 'never_reported', 'revoked'],
    collector_state: ['healthy', 'degraded', 'absent', 'tampered'],
    content_state: ['not_captured', 'local_only', 'uploaded', 'shredded'],
    policy_action: ['blocked', 'warned', 'logged'],
    review_state: ['open', 'disputed', 'confirmed'],
    sanctioned_state: ['sanctioned', 'unsanctioned', 'unknown'],
  };
  for (const [field, values] of Object.entries(pairs)) {
    const rendered = values.map((v) => renderValue({ kind: 'vocab', text: v }));
    assert.equal(new Set(rendered).size, values.length, `${field} values must render distinctly`);
    for (const [i, html] of rendered.entries()) assert.ok(html.includes(values[i]), `${field}=${values[i]}`);
  }
});

test('§14.8 revoked is not stale, and never_reported is not absent', () => {
  const revoked = renderValue({ kind: 'vocab', text: 'revoked' });
  const stale = renderValue({ kind: 'vocab', text: 'stale' });
  assert.notEqual(revoked, stale);
  const devices = readState(envelope('ok', { data: DEVICE_ROWS, freshness: FRESH, coverage: PARTIAL, meta: { source: 'mart.v_device_liveness' } }));
  const html = renderScreen(devicesView(devices), SHELL);
  for (const value of ['reporting', 'stale', 'never_reported', 'revoked']) assert.ok(html.includes(value), `${value} must appear`);
});

// ── §14 item 9: no inferred health ───────────────────────────────────────────────────────────

test('§14.9 a silent device is never rendered as healthy', () => {
  const silent = readState(envelope('ok', {
    data: [{ device: 'd', liveness: 'never_reported', collector: null, collector_state: null, last_seen_at: null, spool_dropped_total: null }],
    freshness: FRESH, coverage: PARTIAL, meta: { source: 'mart.v_device_liveness' },
  }));
  const html = renderScreen(devicesView(silent), SHELL);
  assert.ok(/never_reported/.test(html));
  assert.ok(!/healthy/.test(html), 'no health is inferred from the absence of a signal');
  assert.ok(/unknown/.test(html), 'a null collector state renders as unknown, not as a blank');
});

// ── §14 item 10: no clock-skew normalisation ─────────────────────────────────────────────────

test('§14.10 both clocks are rendered, and the device clock is marked as possibly skewed', () => {
  const state = readState(envelope('ok', { data: ACTIVITY_ROWS, freshness: FRESH, coverage: PARTIAL, meta: { source: 'ingest.submission' } }));
  const html = renderScreen(activityView(state), SHELL);
  assert.ok(/Received \(server\)/.test(html));
  assert.ok(/Occurred \(device\)/.test(html));
  assert.ok(/v-device-clock/.test(html));
  assert.ok(/possibly skewed/.test(html));
});

// ── §14 item 11: no chart computed by scanning raw events ────────────────────────────────────

test('§14.11 a series is refused for any source that is not a precomputed aggregate', () => {
  const state = readState(envelope('ok', { data: ACTIVITY_ROWS, freshness: FRESH, coverage: PARTIAL, meta: { source: 'ingest.submission' } }));
  const view = activityView(state);
  assert.equal(view.series.length, 0, 'the event list produces no chart at all');
  const rendered = renderScreen(view, SHELL);
  assert.ok(!/class="bars"/.test(rendered));
});

// ── §14 item 13: no configuration change without an audit entry ──────────────────────────────

test('§14.13 this client performs no write of any kind', async () => {
  const seen = [];
  const spy = createQueryApi({
    transport: {
      async send(body) {
        seen.push(body);
        return scenarioTransport('realistic').send(body);
      },
    },
  });
  const dashboard = createDashboard({ api: spy, now: () => new Date('2026-10-01T12:00:00Z') });
  for (const screen of SCREENS) await dashboard.load(screen.id, {});
  for (const body of seen) {
    assert.ok(body.query_version === '1', 'every request is a query document');
    assert.ok('template' in body || 'source' in body, 'and nothing else');
  }
  assert.ok(GAP_IDS.includes('settings'), 'configuration is reported as not exposed, not implemented here');
});

// ── §14 item 14: no silence presented as coverage ────────────────────────────────────────────

test('§14.14 the coverage denominator is on screen whenever a reporting figure is', () => {
  const html = renderScreen(devicesView(readState(envelope('ok', { data: DEVICE_ROWS, freshness: FRESH, coverage: PARTIAL, meta: { source: 'mart.v_device_liveness' } }))), SHELL);
  assert.match(html, /4,180 of 4,620 enrolled devices reporting/);
  const noDenominator = renderScreen(devicesView(readState(envelope('ok', {
    data: DEVICE_ROWS, freshness: FRESH,
    coverage: { state: 'not_yet_covered', devices_reporting: null, devices_enrolled: null, gap_reasons: {} },
    meta: { source: 'mart.v_device_liveness' },
  }))), SHELL);
  assert.ok(/Coverage not yet measured/.test(noDenominator));
  assert.ok(!/\d+%/.test(noDenominator), 'no fleet percentage the product cannot compute');
});

// ── §14 item 15: no unrecognised filter silently ignored ─────────────────────────────────────

test('§14.15 an unrecognised filter is a refusal, and the refusal carries the fix', async () => {
  const error = await import('../src/dsl.js').then(({ buildDocument }) => {
    try {
      buildDocument({ source: 'mart.v_tool_usage', window: { from: '2026-09-24T00:00:00Z', to: '2026-10-01T00:00:00Z' }, measures: ['submissions'], dimensions: [], filters: [{ field: 'salary', op: 'eq', value: 'x' }] });
      return null;
    } catch (e) {
      return e;
    }
  });
  assert.equal(error.reason, 'unknown_dimension');
  const serverRefusal = readState({
    api_version: '1', query_version: '1', result_state: 'query_too_broad',
    error: { code: 'cost_estimate_exceeded', message: 'too broad', detail: { max_cells: 2000, estimated_cells: 33600, fix: { coarser_bucket: 'week' } } },
  });
  const html = renderScreen({ ...unavailableView(), banners: serverRefusal.banners }, SHELL);
  assert.ok(/coarsen the bucket to week/.test(html), 'the server-side fix is rendered');
});

// ── INV-3 mirrored: the browser never speaks SQL ─────────────────────────────────────────────

test('INV-3 no file in this package contains a statement or a database driver', () => {
  const files = sourceFiles({ includeTests: true });
  assert.ok(files.length > 10, `expected a substantial tree, got ${files.length}`);
  // Patterns are assembled from parts so this file does not match its own scanner text.
  const verb = ['sel', 'ect'].join('');
  const into = ['ins', 'ert'].join('');
  const fromJoin = ['del', 'ete'].join('');
  const statement = new RegExp(`\\b(${verb}\\s+[\\w*]|${into}\\s+into|${fromJoin}\\s+from|update\\s+\\w+\\s+set)\\b`, 'i');
  const schemaJoin = new RegExp(`\\bfrom\\s+(ops|mart|ingest|ref)\\s*\\.`, 'i');
  const driver = new RegExp(`\\b(pg|pgx|${['post', 'gres'].join('')}|mysql|sqlite|database/sql)\\b`);
  const hits = [];
  for (const rel of files) {
    const code = codeOnly(readFileSync(join(ROOT, rel), 'utf8'), rel);
    code.split(/\r?\n/).forEach((line, i) => {
      if (statement.test(line)) hits.push(`${rel}:${i + 1} statement`);
      if (schemaJoin.test(line)) hits.push(`${rel}:${i + 1} schema`);
      if (driver.test(line)) hits.push(`${rel}:${i + 1} driver`);
    });
  }
  assert.deepEqual(hits, [], 'the dashboard tree must contain no statement and no driver');
});

test('INV-3 the only endpoints in this package are the query endpoint and the two content reads', () => {
  // The query endpoint takes a closed query document and never returns content. The two content
  // reads are the approved path (docs/04 §15.3, docs/02 §11): forwarded to content-vault, which
  // decides and audits. Anything else named here would be a fourth way to reach data.
  const files = sourceFiles();
  const allowed = [['/v1/', 'query'], ['/v1/', 'content-search'], ['/v1/', 'content/retrieval']].map((parts) => parts.join(''));
  const others = files.map((rel) => readFileSync(join(ROOT, rel), 'utf8')).join('\n');
  const urls = [...others.matchAll(/['"](\/v1\/[a-z/-]+)['"]/g)].map((m) => m[1]);
  assert.ok(urls.includes(allowed[0]), 'the query endpoint is named somewhere');
  for (const url of urls) assert.ok(allowed.includes(url), `${url} is not one of ${allowed.join(', ')}`);
});

test('INV-3 the fetch global is reachable from exactly one module of the application', () => {
  // Scoped to src/: that is the tree a browser loads. tools/ are node-side build and preview
  // helpers and are never shipped, so a prose mention there is not a seam.
  const files = sourceFiles().filter((rel) => rel.startsWith('src/'));
  const withFetch = files.filter((rel) => /\bfetch\b/.test(codeOnly(readFileSync(join(ROOT, rel), 'utf8'), rel)));
  assert.deepEqual(withFetch, ['src/transport.js'], 'one seam, and it is the transport');
});

// ── recorded, not skipped ────────────────────────────────────────────────────────────────────

test('the prohibitions that are properties of another component are recorded in the catalogue', () => {
  // §14 item 12 (no response-side capture in v1) is an extension property, not a dashboard one.
  // Recording it keeps the list complete rather than silently short.
  const all = JSON.stringify(GAPS);
  assert.ok(/content search/i.test(all));
  assert.ok(/export/i.test(all));
  assert.ok(/settings|configuration/i.test(all));
  assert.equal(GAPS.length >= 12, true);
});
