// guarantees.test.mjs — what the dashboard must never do, as tests rather than comments.
//
// Each guarantee a client can break is asserted here, either statically (this tree contains no SQL
// and no database driver) or behaviourally (the renderer cannot produce the forbidden output).

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { readState, measureOf } from '../src/states.js';
import { renderScreen, renderValue, renderTable } from '../src/render.js';
import { eventView, devicesView, seriesFrom, refusalView } from '../src/views.js';
import { createDashboard, SCREENS } from '../src/app.js';
import { createQueryApi, createAdminApi } from '../src/transport.js';
import { ADMIN_DEPLOYMENT_ENDPOINT } from '../src/vocab.js';
import { ACTIVITY_ROWS, DEVICE_ROWS, fixtureTransport } from './fixtures.mjs';
import { codeOnly, sourceFiles, ROOT, envelope, FRESH, COMPLETE, PARTIAL } from './helpers.mjs';

const SHELL = { coverage: COMPLETE, freshness: FRESH };

function dashboardFor(scenario = 'realistic') {
  return createDashboard({ api: createQueryApi({ transport: fixtureTransport(scenario) }), now: () => new Date('2026-10-01T12:00:00Z') });
}

// ── no per-employee scoring, ranking or efficiency reporting ─────────────────────────────────

test('no per-person ranking: a per-person source refuses a read without a subject', () => {
  const dashboard = dashboardFor();
  return dashboard.load('person', { filters: {} }).then(({ view }) => {
    // With no subject, the screen asks nothing at all: it cannot build a request that would
    // enumerate people, because dsl.js refuses one.
    assert.equal(view.needsInput, true);
  });
});

test('the unsanctioned table is ordered by tool and person, never by volume', async () => {
  // The fixture's volumes run *against* the row order, so a volume sort would be visible.
  const dashboard = dashboardFor();
  const { view } = await dashboard.load('tools', { filters: { view: 'unsanctioned' } });
  const table = view.tables[0];
  const subjects = table.rows.map((r) => r.row.subject).filter(Boolean);
  assert.deepEqual(subjects, ['u_9a02', 'u_4f21', 'u_1b77'], 'rows keep the API order rather than a volume ranking');
  const volumes = table.rows.filter((r) => typeof r.row.submissions === 'number').map((r) => r.row.submissions);
  assert.ok(volumes[0] > volumes[1], 'the fixture is ordered against volume on purpose, so a volume sort would be visible');
  assert.ok(view.notes.some((n) => /not a ranking/.test(n)));
});

test('no screen exposes a "most active people" table', async () => {
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

// ── no content from a query ──────────────────────────────────────────────────────────────────

test('no query path returns content, and event detail shows metadata plus the content_state answer', async () => {
  const dashboard = dashboardFor();
  const { view } = await dashboard.load('event', { filters: { submission_id: '11111111-2222-4333-8444-555555555551' } });
  const html = renderScreen(view, SHELL);
  for (const forbidden of ['content_excerpt', 'prompt_text', 'attachment_body', 'ciphertext', 'wrapped_dek', 'plaintext']) {
    assert.ok(!html.includes(forbidden), `event detail must not render ${forbidden}`);
  }
  assert.ok(/Open the event in Search to read the prompt/.test(html), 'the uploaded state says where the prompt is read');
});

test('each content_state is a different answer, and all four are rendered', () => {
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

// ── no cross-tenant read ─────────────────────────────────────────────────────────────────────

test('no request carries a tenant', async () => {
  const seen = [];
  const spy = createQueryApi({
    transport: {
      async send(body) {
        seen.push(body);
        return fixtureTransport('realistic').send(body);
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

// ── no aggregate without its freshness and coverage state ────────────────────────────────────

test('a screen read under partial coverage says so, with the enrolled denominator', async () => {
  const dashboard = dashboardFor();
  for (const screen of SCREENS) {
    const { view, shell } = await dashboard.load(screen.id, { filters: { submission_id: '11111111-2222-4333-8444-555555555551', subject: 'u_1' } });
    if (!view || view.needsInput || screen.kind !== 'answer' || screen.id === 'audit') continue; // the audit trail is not a fleet figure
    const html = renderScreen(view, shell);
    assert.ok(/Coverage is partial/.test(html), `${screen.id} must say coverage is partial`);
    assert.ok(/4,180 of 4,620 enrolled devices reporting/.test(html), `${screen.id} must carry the denominator`);
  }
});

test('a data-bearing envelope without a state block is refused rather than rendered', () => {
  assert.throws(() => readState(envelope('ok', { data: [{ submissions: 5 }] })), /refusing to render/);
});

// ── no absent value shown as zero, no zero shown as absent ───────────────────────────────────

test('the renderer cannot merge an absent value with a zero', () => {
  const absent = renderValue({ kind: 'absent' });
  const zero = renderValue({ kind: 'number', value: 0, text: '0' });
  assert.match(absent, /v-absent/);
  assert.ok(!/>\s*0\s*</.test(absent), 'an absent marker contains no zero');
  assert.match(zero, /v-number">0</);
  assert.ok(!/v-absent/.test(zero), 'a zero is not rendered as absent');
});

test('the same distinction survives a whole table, and a small count is a number', () => {
  const state = readState(envelope('ok', {
    data: [
      { tool: 'a', submissions: 0, users: 0 },
      { tool: 'b', submissions: 2, users: 1 },
      { tool: 'c' },
    ],
    freshness: FRESH, coverage: COMPLETE,
  }));
  const html = renderTable({
    title: 't',
    columns: [{ key: 'tool', label: 'Tool' }, { key: 'submissions', label: 'Submissions', kind: 'measure' }],
    rows: state.data.map((row) => ({ row, vocab: {} })),
    emptyText: 'none',
  });
  assert.match(html, /<span class="v-number">0<\/span>/);
  assert.match(html, /<span class="v-number">2<\/span>/, 'a small count renders its number');
  assert.match(html, /v-absent/);
});

// ── no merging of the state pairs ────────────────────────────────────────────────────────────

test('every value of every state pair renders as itself', () => {
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

test('revoked is not stale, and never_reported is not absent', () => {
  const revoked = renderValue({ kind: 'vocab', text: 'revoked' });
  const stale = renderValue({ kind: 'vocab', text: 'stale' });
  assert.notEqual(revoked, stale);
  const devices = readState(envelope('ok', { data: DEVICE_ROWS, freshness: FRESH, coverage: PARTIAL, meta: { source: 'mart.v_device_liveness' } }));
  const html = renderScreen(devicesView(devices), SHELL);
  for (const value of ['reporting', 'stale', 'never_reported', 'revoked']) assert.ok(html.includes(value), `${value} must appear`);
});

// ── no inferred health ───────────────────────────────────────────────────────────────────────

test('a silent device is never rendered as healthy', () => {
  const silent = readState(envelope('ok', {
    data: [{ device: 'd', liveness: 'never_reported', collector: null, collector_state: null, last_seen_at: null, spool_dropped_total: null }],
    freshness: FRESH, coverage: PARTIAL, meta: { source: 'mart.v_device_liveness' },
  }));
  const html = renderScreen(devicesView(silent), SHELL);
  assert.ok(/never_reported/.test(html));
  assert.ok(!/healthy/.test(html), 'no health is inferred from the absence of a signal');
  assert.ok(/unknown/.test(html), 'a null collector state renders as unknown, not as a blank');
});

// ── no clock-skew normalisation ──────────────────────────────────────────────────────────────

test('both clocks are rendered, and the device clock is marked as possibly skewed', () => {
  const html = renderTable({
    title: 'Events',
    columns: [{ key: 'received_at', label: 'Received (server)', kind: 'instant' }, { key: 'first_occurred_at', label: 'Occurred (device)', kind: 'device-clock' }],
    rows: ACTIVITY_ROWS.map((row) => ({ row, vocab: {} })),
    emptyText: 'none',
  });
  assert.ok(/Received \(server\)/.test(html));
  assert.ok(/Occurred \(device\)/.test(html));
  assert.ok(/v-device-clock/.test(html));
  assert.ok(/possibly skewed/.test(html));
});

// ── no chart computed by scanning raw events ─────────────────────────────────────────────────

test('a series is refused for any source that is not a precomputed aggregate', () => {
  const state = readState(envelope('ok', { data: ACTIVITY_ROWS, freshness: FRESH, coverage: PARTIAL, meta: { source: 'ingest.submission' } }));
  const series = seriesFrom(state, { measure: 'submissions', title: 'Events' });
  assert.equal(series.unavailable, true, 'an event list produces no chart');
  assert.deepEqual(series.points, []);
});

// ── no configuration change without an audit entry ───────────────────────────────────────────

test('the query path performs no write of any kind', async () => {
  const seen = [];
  const spy = createQueryApi({
    transport: {
      async send(body) {
        seen.push(body);
        return fixtureTransport('realistic').send(body);
      },
    },
  });
  const dashboard = createDashboard({ api: spy, now: () => new Date('2026-10-01T12:00:00Z') });
  for (const screen of SCREENS) await dashboard.load(screen.id, {});
  for (const body of seen) {
    assert.ok(body.query_version === '1', 'every request is a query document');
    assert.ok('template' in body || 'source' in body, 'and nothing else');
  }
});

test('every write this client can make goes to control-api\'s admin API, which audits it', async () => {
  // Settings → Deployment and Settings → Settings are the two places the dashboard changes
  // anything. Each write is one of the admin endpoints vocab.js names; control-api checks the admin
  // role and writes the audit row with the real actor. Nothing else in the client sends a write.
  const sent = [];
  const admin = createAdminApi({ transport: { async request(spec) { sent.push(spec); return { status: 204, body: null }; } } });
  await admin.setVerification('none');
  await admin.downloadPackage({ format: 'zip' });
  await admin.revokeKey('k');
  await admin.createScimToken('label');
  await admin.revokeScimToken('t');
  await admin.setCollectionMode('m1');
  await admin.setScopeOverride('tls_x', 'm0');
  await admin.setRetention('event', 90);
  await admin.setContentSearch('attachment_names');
  await admin.setToolSanction('tls_x', 'unsanctioned');
  await admin.setKillSwitch('proxy.tls', true, 'app_breakage');
  const writes = sent.filter((s) => s.method !== 'GET');
  assert.equal(writes.length, 11);
  for (const w of writes) assert.ok(w.path.startsWith('/admin/v1/'), `${w.method} ${w.path} is not an admin endpoint`);
});

// ── no silence presented as coverage ─────────────────────────────────────────────────────────

test('the coverage denominator is on screen whenever a reporting figure is', () => {
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

// ── no unrecognised filter silently ignored ──────────────────────────────────────────────────

test('an unrecognised filter is a refusal, and the refusal carries the fix', async () => {
  const error = await import('../src/dsl.js').then(({ buildTemplate }) => {
    try {
      buildTemplate('q1_tools_ranked', { window: { from: '2026-09-24T00:00:00Z', to: '2026-10-01T00:00:00Z' }, salary: 'x' });
      return null;
    } catch (e) {
      return e;
    }
  });
  assert.equal(error.reason, 'unknown_key');
  const serverRefusal = readState({
    api_version: '1', query_version: '1', result_state: 'query_too_broad',
    error: { code: 'cost_estimate_exceeded', message: 'too broad', detail: { max_cells: 2000, estimated_cells: 33600, fix: { coarser_bucket: 'week' } } },
  });
  const html = renderScreen(refusalView(serverRefusal, { title: 'Tools' }), SHELL);
  assert.ok(/coarsen the bucket to week/.test(html), 'the server-side fix is rendered');
});

// ── the browser never speaks SQL ─────────────────────────────────────────────────────────────

test('no file in this package contains a statement or a database driver', () => {
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

test('the only endpoints the pages call are the query endpoint, the two content reads and the deployment admin API', () => {
  // The query endpoint takes a closed query document and never returns content. The two content
  // reads are forwarded to content-vault, which decides and audits. The admin endpoints are
  // Settings → Deployment's and Settings → Settings', control-api's, admin-only and audited; they
  // read configuration and never data. Anything else named here would be another way in.
  const files = sourceFiles().filter((rel) => rel.startsWith('src/'));
  const allowed = [
    ['/v1/', 'query'], ['/v1/', 'content-search'], ['/v1/', 'content/retrieval'], ['/v1/', 'list-export'],
    ['/admin/v1/', 'deployment'], ['/admin/v1/', 'deployment/package'], ['/admin/v1/', 'deployment/verification'],
    ['/admin/v1/', 'deployment/keys'], ['/admin/v1/', 'scim/tokens'],
    ['/admin/v1/', 'settings'], ['/admin/v1/', 'settings/collection-mode'], ['/admin/v1/', 'settings/scope-override'],
    ['/admin/v1/', 'settings/retention'], ['/admin/v1/', 'settings/content-search'], ['/admin/v1/', 'settings/tools'],
    ['/admin/v1/', 'settings/endpoint'], ['/admin/v1/', 'settings/tls-inspection'], ['/admin/v1/', 'settings/rules'],
    ['/admin/v1/', 'settings/kill-switch'],
  ].map((parts) => parts.join(''));
  const others = files.map((rel) => readFileSync(join(ROOT, rel), 'utf8')).join('\n');
  const urls = [...others.matchAll(/['"](\/(?:admin\/)?v1\/[a-z/-]+)['"]/g)].map((m) => m[1]);
  assert.ok(urls.includes(allowed[0]), 'the query endpoint is named somewhere');
  assert.ok(urls.includes(ADMIN_DEPLOYMENT_ENDPOINT), 'the admin endpoints are matched by this scan too');
  for (const url of urls) assert.ok(allowed.includes(url), `${url} is not one of ${allowed.join(', ')}`);
});

test('the fetch global is reachable from exactly one module of the pages', () => {
  // Scoped to src/: that is the tree a browser loads. server/ and tools/ run in Node.
  const files = sourceFiles().filter((rel) => rel.startsWith('src/'));
  const withFetch = files.filter((rel) => /\bfetch\b/.test(codeOnly(readFileSync(join(ROOT, rel), 'utf8'), rel)));
  assert.deepEqual(withFetch, ['src/transport.js'], 'one seam, and it is the transport');
});
