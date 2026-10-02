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
//   node lab/run.mjs            # up (if needed) + smoke + report, leaves the lab running
//   node lab/run.mjs --down     # tear the lab down and remove the volume
//   node lab/run.mjs --no-up    # smoke against something already running

import { spawnSync } from 'node:child_process';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const ARGS = process.argv.slice(2);
const DOWN = ARGS.includes('--down');
const NO_UP = ARGS.includes('--no-up');

const COMPOSE = ['compose', '-f', join(ROOT, 'lab', 'docker-compose.yml')];
const TMP = join(ROOT, '.testtmp');
const ENV = { ...process.env, TMP, TEMP: TMP, TMPDIR: TMP };

const TENANT = '11111111-1111-1111-1111-111111111111';
const DEVICE = '22222222-2222-2222-2222-222222222222';
const DIGEST = 'sha256:' + 'a'.repeat(64);
const KEY = 'sha256:' + 'b'.repeat(64);

const INGEST = 'http://127.0.0.1:8080';
const VAULT = 'http://127.0.0.1:8081';

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
    console.error('\nlab: compose up failed. If the images are missing, run: node lab/build.mjs');
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
const accepted = first.body?.counts?.accepted;
const subIds = new Set((first.body?.results ?? []).map((r) => r.submission_id));
check('200 with two accepted results', first.status === 200 && accepted === 2, `status ${first.status}, accepted ${accepted}`);
check('one logical submission for two observations', subIds.size === 1 && !subIds.has(undefined), `${subIds.size} submission_id(s)`);
check('dedup_tier recomputed at ingest', (first.body?.results ?? []).every((r) => r.dedup_tier === 'T'), `tiers ${(first.body?.results ?? []).map((r) => r.dedup_tier).join(',')}`);

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

console.log(`\n${failures === 0 ? 'lab: all checks passed' : `lab: ${failures} check(s) FAILED`}`);
console.log('lab: left running. Logs: docker compose -f lab/docker-compose.yml logs -f');
console.log('lab: tear down with: node lab/run.mjs --down');
process.exit(failures === 0 ? 0 : 1);
