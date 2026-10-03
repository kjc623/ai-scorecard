#!/usr/bin/env node
// Bring the lab up and prove it works.
//
// `docker compose up` reporting "Started" is not evidence that anything serves. This script waits for
// the services to be healthy and then exercises the paths a person would: the two probes the
// container platform uses, a real POST /v1/events with two routes observing one submission, a replay
// of the same batch, the M0 boundary, and the database the compose file applied the schema to.
//
// Every check prints what it saw. A failure exits non-zero, so this is usable as a gate.
//
// Usage:
//   node localdev/run.mjs            # up (if needed) + smoke + report, leaves the lab running
//   node localdev/run.mjs --down     # tear the lab down and remove the volume
//   node localdev/run.mjs --no-up    # smoke against something already running

import { spawnSync } from 'node:child_process';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const ARGS = process.argv.slice(2);
const DOWN = ARGS.includes('--down');
const NO_UP = ARGS.includes('--no-up');

const COMPOSE = ['compose', '-f', join(ROOT, 'localdev', 'docker-compose.yml')];
const TMP = join(ROOT, '.testtmp');
const ENV = { ...process.env, TMP, TEMP: TMP, TMPDIR: TMP };

const TENANT = '11111111-1111-1111-1111-111111111111';
const DEVICE = '22222222-2222-2222-2222-222222222222';
const DIGEST = 'sha256:' + 'a'.repeat(64);
const KEY = 'sha256:' + 'b'.repeat(64);

const INGEST = 'http://127.0.0.1:8080';
const VAULT = 'http://127.0.0.1:8081';
const QUERY = 'http://127.0.0.1:8082';

/**
 * One well-formed DSL document for the read path.
 *
 * It is deliberately the simplest thing the closed DSL admits — a day-bucketed count over a mart
 * view with no dimensions — because what the lab proves is that the pipeline runs end to end against
 * a real schema, not that any particular question is answered. The ten questions have their own
 * coverage in query/query-api's suite.
 */
function baseQuery() {
  return {
    query_version: '1',
    source: 'mart.v_tool_usage',
    bucket: 'day',
    dimensions: ['tool'],
    measures: ['submissions'],
    filters: [],
    window: { from: '2026-09-01T00:00:00Z', to: '2026-09-08T00:00:00Z' },
    limit: 50,
  };
}

let failures = 0;
function check(name, ok, detail) {
  console.log(`${ok ? '  ok  ' : ' FAIL '} ${name}${detail ? ` — ${detail}` : ''}`);
  if (!ok) failures++;
}

function compose(args, opts = {}) {
  const res = spawnSync('docker', [...COMPOSE, ...args], { encoding: 'utf8', env: ENV, cwd: ROOT, ...opts });
  return { code: res.status, out: `${res.stdout ?? ''}${res.stderr ?? ''}` };
}

function psql(sql) {
  const res = spawnSync('docker', ['exec', 'sac-lab-postgres-1', 'psql', '-U', 'postgres', '-d', 'shadow', '-Atc', sql], {
    encoding: 'utf8',
    env: ENV,
  });
  return (res.stdout ?? '').trim();
}

/** One prompt envelope, shaped exactly as device/protocol and the contract require. */
function prompt({ eventId, source, mode, digest = DIGEST, key = KEY, occurredAt = '2026-10-02T14:00:00Z' }) {
  const base = {
    schema_version: '1.0',
    event_id: eventId,
    tenant_id: TENANT,
    device_id: DEVICE,
    user_ref: 'user-1',
    tool_fingerprint: 'chat-tool',
    direction: 'egress',
    kind: 'prompt',
    occurred_at: occurredAt,
    monotonic_offset_ms: 5,
    source,
    collection_mode: mode,
    size_bytes: 120,
    policy_decision: { rule_id: 'RULE_1', action: 'logged', decided_locally: true },
    dedup_key: key,
  };
  if (mode !== 'm0') {
    base.confidence = 'high';
    base.content_digest = digest;
    base.labels = [];
    base.classifier_version = '2026.01.0-shadow';
  }
  return base;
}

async function post(batch) {
  const res = await fetch(`${INGEST}/v1/events`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-Dev-Tenant-Id': TENANT, 'X-Dev-Device-Id': DEVICE },
    body: JSON.stringify(batch),
  });
  let body;
  try {
    body = await res.json();
  } catch {
    body = null;
  }
  return { status: res.status, body };
}

async function waitFor(url, timeoutMs = 40000) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    try {
      const res = await fetch(url);
      if (res.ok) return true;
    } catch {
      /* not up yet */
    }
    if (Date.now() > deadline) return false;
    await new Promise((r) => setTimeout(r, 500));
  }
}

if (DOWN) {
  console.log('lab: tearing down (volume included) …');
  const r = compose(['down', '-v']);
  console.log(r.out.trim());
  process.exit(r.code ?? 0);
}

console.log('lab: starting …');
if (!NO_UP) {
  const r = compose(['up', '-d', '--wait']);
  if (r.code !== 0) {
    console.error(r.out.trim());
    console.error('\nlab: compose up failed. If the images are missing, run: node localdev/build.mjs');
    process.exit(1);
  }
}

console.log('\nprobes (the paths infra/modules/container-app.bicep uses):');
const ingestUp = await waitFor(`${INGEST}/healthz`);
check('ingest-api /healthz', ingestUp, ingestUp ? '200' : 'no answer within 40s');
if (ingestUp) {
  const r = await fetch(`${INGEST}/readyz`);
  check('ingest-api /readyz', r.status === 200, `${r.status} ${(await r.text()).trim()}`);
}
const vaultUp = await waitFor(`${VAULT}/healthz`);
check('content-vault /healthz', vaultUp, vaultUp ? '200' : 'no answer within 40s');
if (vaultUp) {
  const r = await fetch(`${VAULT}/readyz`);
  const body = (await r.text()).trim();
  check('content-vault /readyz', r.status === 200 && body.includes('"status":"ready"'), `${r.status} ${body}`);
}

console.log('\nthe database the compose file built:');
const tables = psql("select count(*) from information_schema.tables where table_schema='ingest';");
check('db/schema.sql applied (ingest tables)', tables === '4', `${tables} tables`);
const routes = psql('select count(*) from ref.route_fidelity;');
check('ref.route_fidelity seeded', routes === '7', `${routes} routes`);

console.log('\nPOST /v1/events — two routes observing one submission:');
const batchId = `lab-${Date.now()}`;
const twoRoutes = {
  schema_version: '1.0',
  batch_id: batchId,
  device_sent_at: '2026-10-02T14:00:03Z',
  event_count: 2,
  events: [
    prompt({ eventId: 'aaaa0001-0000-4000-8000-000000000001', source: 'ext.web_request', mode: 'm1' }),
    prompt({ eventId: 'aaaa0001-0000-4000-8000-000000000002', source: 'ext.page_context', mode: 'm1' }),
  ],
};
const first = await post(twoRoutes);
const counts = first.body?.counts ?? {};
const subIds = new Set((first.body?.results ?? []).map((r) => r.submission_id));
const outcomes = (first.body?.results ?? []).map((r) => r.outcome);
/**
 * Repeatable on purpose, and the two admissible answers are both correct.
 *
 * On a database with no history these two observations are ACCEPTED. On a rerun they are the same
 * two `event_id`s under the same device and window, which is precisely the tie-break the two-tier
 * dedup ladder exists to judge, so they are counted `duplicate` and the submission is not
 * double-counted. The property under test — two routes, one logical submission — holds either way.
 * The check previously demanded `accepted === 2` unconditionally, so a lab that had ever been run
 * before reported FAIL on its second run; a smoke test that fails when nothing is wrong teaches
 * people to ignore it, which is worse than having no check.
 *
 * Verified against the running lab in both states: a fresh volume gives accepted=2, and three
 * consecutive reruns give duplicate=2.
 */
const healthy = first.status === 200 && (counts.accepted === 2 || counts.duplicate === 2);
check(
  'two routes are accepted, or recognised as the submission already recorded',
  healthy,
  `status ${first.status}, accepted ${counts.accepted}, duplicate ${counts.duplicate}, rejected ${counts.rejected}`,
);
check('every event has a submission_id, and they are the same one', subIds.size === 1 && !subIds.has(undefined), `${subIds.size} submission_id(s)`);
check(
  'neither observation is rejected, and both reach one logical submission',
  outcomes.length === 2 && outcomes.every((o) => o === 'accepted' || o === 'duplicate'),
  `outcomes ${outcomes.join(',') || '(none)'}`,
);

console.log('\nreplay of the same batch_id:');
const replay = await post(twoRoutes);
check('409 duplicate_batch', replay.status === 409 && replay.body?.error?.code === 'duplicate_batch', `${replay.status} ${replay.body?.error?.code}`);

console.log('\nan M0 prompt carrying content_digest (docs/01-collectors.md §7.1):');
const m0 = prompt({ eventId: 'bbbb0002-0000-4000-8000-000000000001', source: 'ext.dom', mode: 'm0', key: KEY });
m0.content_digest = DIGEST; // the defect: content read at M0
const bad = await post({
  schema_version: '1.0',
  batch_id: `lab-m0-${Date.now()}`,
  device_sent_at: '2026-10-02T15:00:03Z',
  event_count: 1,
  events: [m0],
});
const rej = bad.body?.results?.[0];
check('200 with a per-event rejection, not a batch failure', bad.status === 200 && bad.body?.counts?.rejected === 1, `status ${bad.status}`);
check('reason mode_violation with the offending pointer', rej?.reason === 'mode_violation' && rej?.detail?.pointer === '/events/0/content_digest', `${rej?.reason} ${rej?.detail?.pointer}`);

console.log('\nthe read path — query-api, which had no server until now:');
const queryUp = await waitFor(`${QUERY}/healthz`);
check('query-api /healthz', queryUp, queryUp ? '200' : 'no answer within 40s');
if (queryUp) {
  const r = await fetch(`${QUERY}/readyz`);
  const body = (await r.text()).trim();
  check('query-api /readyz', r.status === 200 && body.includes('"status":"ready"'), `${r.status} ${body}`);

  // Fail closed: without a tenant there is no read. This is §2.1's rule, and it is checked here
  // because a read path that answers an unauthenticated caller is the one defect the whole design
  // exists to prevent.
  const anonymous = await fetch(`${QUERY}/v1/query`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(baseQuery()),
  });
  const anonymousBody = await anonymous.json().catch(() => ({}));
  check(
    'a read with no tenant is refused, and reaches no database',
    anonymous.status === 403 && anonymousBody.result_state === 'unauthorised_role',
    `${anonymous.status} ${anonymousBody.result_state ?? ''}`,
  );

  // The real thing: a DSL document through validate -> guard -> compile -> audit -> execute, against
  // the schema the compose file applied. An empty result is a legitimate answer; an envelope that
  // cannot carry freshness and coverage is not, which is why the state is asserted rather than the
  // row count.
  const read = await fetch(`${QUERY}/v1/query`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', 'x-sac-dev-tenant': TENANT, 'x-sac-dev-actor': 'lab' },
    body: JSON.stringify(baseQuery()),
  });
  const readBody = await read.json().catch(() => ({}));
  check(
    'POST /v1/query answers with a §13 envelope',
    read.status === 200 && typeof readBody.result_state === 'string' && 'api_version' in readBody,
    `status ${read.status}, result_state ${readBody.result_state ?? readBody.error?.code ?? '?'}`,
  );
  check(
    'the answer carries freshness and coverage, so a number never travels without its state',
    readBody.freshness !== undefined && readBody.coverage !== undefined,
    `freshness=${readBody.freshness !== undefined} coverage=${readBody.coverage !== undefined}`,
  );

  // A prohibited field is refused as a validation error, and the transport repeats the pipeline's
  // own state rather than inventing one.
  const injected = await fetch(`${QUERY}/v1/query`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', 'x-sac-dev-tenant': TENANT },
    body: JSON.stringify({ ...baseQuery(), sql: 'SELECT 1' }),
  });
  const injectedBody = await injected.json().catch(() => ({}));
  check(
    'a request that tries to speak SQL is refused',
    injected.status === 400 && injectedBody.error?.code === 'prohibited_field',
    `${injected.status} ${injectedBody.error?.code ?? ''}`,
  );
}

console.log(`\n${failures === 0 ? 'lab: all checks passed' : `lab: ${failures} check(s) FAILED`}`);
console.log('lab: left running. Logs: docker compose -f localdev/docker-compose.yml logs -f');
console.log('lab: tear down with: node localdev/run.mjs --down');process.exit(failures === 0 ? 0 : 1);
