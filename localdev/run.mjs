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
//   node localdev/run.mjs --auth     # the opt-in device-auth lab (needs build.mjs --auth):
//                                    # real SQL stores, both production modes, and the edge
//   node localdev/run.mjs --auth --down
//
// Addresses come from localdev/docker-compose.yml, not from this file:
//   * on the host, the published ports are asked of `docker compose port`, so LAB_*_PORT
//     overrides are followed without a second edit;
//   * inside the lab's network, set LAB_INGEST_URL / LAB_VAULT_URL / LAB_QUERY_URL to the service
//     addresses (http://ingest-api:8080 and so on) and use --no-up.
// The two database checks run psql inside the postgres container, so they need the docker CLI on
// either side.

import { spawnSync } from 'node:child_process';
import { createHash, randomBytes } from 'node:crypto';
import { chmodSync, copyFileSync, existsSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const ARGS = process.argv.slice(2);
const DOWN = ARGS.includes('--down');
const NO_UP = ARGS.includes('--no-up');
// --auth selects the opt-in device-authentication lab: the real SQL stores, the real x509 and DPoP
// authenticators, and the simulated Application Gateway. See localdev/authlab.compose.yaml.
const AUTH = ARGS.includes('--auth');

const COMPOSE = ['compose', '-f', join(ROOT, 'localdev', 'docker-compose.yml')];
const COMPOSE_AUTH = ['compose', '-f', join(ROOT, 'localdev', 'authlab.compose.yaml')];
const AUTH_DIR = join(ROOT, 'localdev', '.authlab');
const AUTH_BIN = join(ROOT, 'localdev', 'authlab', 'bin', 'authlab') + (process.platform === 'win32' ? '.exe' : '');
const TMP = join(ROOT, '.testtmp');
const ENV = { ...process.env, TMP, TEMP: TMP, TMPDIR: TMP };

const TENANT = '11111111-1111-1111-1111-111111111111';
const DEVICE = '22222222-2222-2222-2222-222222222222';
const DIGEST = 'sha256:' + 'a'.repeat(64);
const KEY = 'sha256:' + 'b'.repeat(64);

// Where the three services answer. Resolved after the lab is up (see baseUrl): there is no port
// number in this file, because the compose file is the one place that decides them.
let INGEST;
let VAULT;
let QUERY;

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
  return { code: res.status, out: `${res.stdout ?? ''}${res.stderr ?? ''}`, stdout: res.stdout ?? '' };
}

function composeAuth(args, opts = {}) {
  const res = spawnSync('docker', [...COMPOSE_AUTH, ...args], { encoding: 'utf8', env: ENV, cwd: ROOT, ...opts });
  return { code: res.status, out: `${res.stdout ?? ''}${res.stderr ?? ''}`, stdout: res.stdout ?? '' };
}

/** A plain docker invocation (not compose), for staging the PKI volume. */
function dockerRun(args) {
  const res = spawnSync('docker', args, { encoding: 'utf8', env: ENV, cwd: ROOT });
  return { code: res.status, out: `${res.stdout ?? ''}${res.stderr ?? ''}` };
}

/**
 * The opt-in device-authentication lab, in order:
 *
 *   1. generate fresh PKI material (dev CA, edge server leaf, token key) into .authlab/;
 *   2. bring up the SQL stores and the edge behind the gateway simulation;
 *   3. seed a tenant and two single-use enrolment tokens;
 *   4. run the Go client, which proves x509 enrol -> events, dpop enrol -> token -> events, and the
 *      negatives (no credential, a replayed DPoP jti, a certificate from another CA).
 *
 * It is self-contained: its own PostgreSQL, its own schema apply, its own network. The default
 * memory lab is untouched and can run at the same time.
 */
async function runAuth() {
  if (!existsSync(AUTH_BIN)) {
    console.error(`lab --auth: ${AUTH_BIN} is missing. Build the tagged images and the client first:`);
    console.error('  node localdev/build.mjs --auth');
    return 1;
  }

  console.log('auth lab: generating fresh development PKI …');
  let r = spawnSync(AUTH_BIN, ['pki', '-pki-dir', AUTH_DIR], { encoding: 'utf8', env: ENV, cwd: ROOT });
  process.stdout.write(r.stdout ?? '');
  process.stderr.write(r.stderr ?? '');
  if (r.status !== 0) return r.status ?? 1;

  // Stage the client binary beside the PKI so the pki image carries both. The smoke then runs
  // *inside* the lab network: some Docker daemons publish ports the host cannot reach, so a
  // host-side client sees `connection refused`. This is also the more faithful test — the device is
  // on the network with the gateway, not on the operator's machine.
  const stagedClient = join(AUTH_DIR, 'authlab');
  copyFileSync(AUTH_BIN, stagedClient);
  chmodSync(stagedClient, 0o755);

  // Deliver the generated PKI to the containers through a named volume rather than a host-path
  // bind: some Docker daemons present a bind of a path the daemon cannot see as an empty directory,
  // so the services would report `open /authlab/dev-ca.crt: no such file or directory`. The PKI is
  // baked into sac/authlab-pki:lab with a build-time COPY (which streams through the build context)
  // and copied into the volume the compose file mounts.
  console.log('\nauth lab: staging the development PKI into the authpki volume …');
  r = dockerRun(['build', '-f', join(ROOT, 'localdev', 'pki', 'Dockerfile'), '-t', 'sac/authlab-pki:lab', '.']);
  if (r.code !== 0) {
    console.error(r.out);
    return r.code ?? 1;
  }
  r = dockerRun(['run', '--rm', '-v', 'sac-authlab-authpki:/tgt', 'sac/authlab-pki:lab', '-c',
    'cp -a /pki/. /tgt/ && ls -A /tgt']);
  process.stdout.write(r.out);
  if (r.code !== 0) {
    console.error('auth lab: staging the PKI volume failed');
    return r.code ?? 1;
  }

  console.log('\nauth lab: starting PostgreSQL, the two SQL services and the edge …');
  r = composeAuth(['up', '-d', '--wait']);
  if (r.code !== 0) {
    console.error(r.out.trim());
    console.error('\nauth lab: compose up failed. Are the --auth images built? node localdev/build.mjs --auth');
    return 1;
  }

  // The schema container is a one-shot dependency. `--wait` honours service_completed_successfully,
  // but this poll is the belt to that braces, so the seed never races a half-applied schema.
  if (!(await waitForOpsSchema())) {
    console.error('auth lab: database/schema.sql did not finish applying within 60s');
    return 1;
  }

  console.log('\nauth lab: seeding a tenant and two single-use enrolment tokens …');
  const tokenX509 = `sac1.${TENANT}.${randomBytes(32).toString('base64url')}`;
  const tokenDPoP = `sac1.${TENANT}.${randomBytes(32).toString('base64url')}`;
  const seedSQL = `
    INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, ceiling_mode)
    VALUES ('${TENANT}', 'authlab', 'active', 'authlab-region', 'vendor', 'm1')
    ON CONFLICT (tenant_id) DO NOTHING;
    INSERT INTO ops.enrolment_token (tenant_id, token_hash, expires_at)
    VALUES ('${TENANT}', '${hashEnrolmentToken(tokenX509)}', now() + interval '1 day')
    ON CONFLICT DO NOTHING;
    INSERT INTO ops.enrolment_token (tenant_id, token_hash, expires_at)
    VALUES ('${TENANT}', '${hashEnrolmentToken(tokenDPoP)}', now() + interval '1 day')
    ON CONFLICT DO NOTHING;
  `;
  r = composeAuth(['exec', '-T', 'postgres', 'psql', '-U', 'postgres', '-d', 'shadow',
    '-v', 'ON_ERROR_STOP=1', '-c', seedSQL]);
  if (r.code !== 0) {
    console.error(r.out.trim());
    return r.code ?? 1;
  }
  check('a tenant and two enrolment tokens are seeded in ops.*', true, `${TENANT}`);

  const edgeURL = 'https://edge:8443';
  console.log(`\nauth lab: smoke against ${edgeURL} from inside the lab network (both modes through the edge, then the negatives)`);
  r = dockerRun([
    'run', '--rm', '--network', 'scorecard-authlab', '--entrypoint', '/pki/authlab', 'sac/authlab-pki:lab',
    'smoke',
    '-edge-url', edgeURL,
    '-pki-dir', '/pki',
    '-tenant', TENANT,
    '-enrolment-token-x509', tokenX509,
    '-enrolment-token-dpop', tokenDPoP,
  ]);
  process.stdout.write(r.out);

  const failed = r.code !== 0;
  console.log(`\n${failed ? 'auth lab: FAILED' : 'auth lab: all checks passed'}`);
  console.log('auth lab: left running. Logs: docker compose -f localdev/authlab.compose.yaml logs -f');
  console.log('auth lab: tear down with: node localdev/run.mjs --auth --down');
  return failed ? 1 : 0;
}

/** The edge's published base URL, asked of compose so LAB_EDGE_PORT is followed without a second edit. */
function authEdgeURL() {
  const explicit = process.env.LAB_EDGE_URL;
  if (explicit) return explicit.replace(/\/+$/, '');
  const r = composeAuth(['port', 'edge', '8443']);
  const m = /:(\d+)\s*$/.exec(r.stdout.trim());
  const port = m ? m[1] : (process.env.LAB_EDGE_PORT ?? '8443');
  return `https://127.0.0.1:${port}`;
}

/** Wait until the seeded schema is queryable, so the seed cannot race the schema container. */
async function waitForOpsSchema() {
  for (let i = 0; i < 60; i++) {
    const r = composeAuth(['exec', '-T', 'postgres', 'psql', '-U', 'postgres', '-d', 'shadow', '-Atc',
      "select 1 from information_schema.schemata where schema_name='ops'"]);
    if (r.stdout.trim() === '1') return true;
    await new Promise((res) => setTimeout(res, 1000));
  }
  return false;
}

/** The enrolment token's stored form: sha256 over the plaintext, in the one spelling the repo uses. */
function hashEnrolmentToken(plaintext) {
  return 'sha256:' + createHash('sha256').update(plaintext).digest('hex');
}

/**
 * The base URL of one service, from whichever side of the container boundary this runs on.
 *
 * An explicit URL wins: a process inside the lab's network (a container attached to `scorecard`)
 * reaches a service by name on its container port — LAB_INGEST_URL=http://ingest-api:8080 — and the
 * published host port means nothing there. Otherwise the answer comes from `docker compose port`,
 * which reports the host port actually bound, so an override of LAB_*_PORT needs no second edit.
 */
function baseUrl(envName, service) {
  const explicit = process.env[envName];
  if (explicit) return explicit.replace(/\/+$/, '');
  const r = compose(['port', service, '8080']);
  const m = /:(\d+)\s*$/.exec(r.stdout.trim());
  if (r.code !== 0 || !m) {
    console.error(`lab: cannot find the published port of ${service} (${r.out.trim() || 'no output'}).`);
    console.error(`lab: is the lab up? Or set ${envName} to the service's base URL.`);
    process.exit(1);
  }
  return `http://127.0.0.1:${m[1]}`;
}

function psql(sql) {
  // By service, not by container name: compose owns the name it gives the container.
  const r = compose(['exec', '-T', 'postgres', 'psql', '-U', 'postgres', '-d', 'shadow', '-Atc', sql]);
  // A check that could not run says so, instead of failing on an empty string that reads like a count.
  if (r.code !== 0) return `(psql did not run: ${r.code === null ? 'no docker CLI here' : r.out.trim()})`;
  return r.stdout.trim();
}

/** One prompt envelope, shaped exactly as endpoint/protocol and the contract require. */
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

if (AUTH) {
  if (DOWN) {
    console.log('auth lab: tearing down (volume included) …');
    const r = composeAuth(['down', '-v']);
    console.log(r.out.trim());
    process.exit(r.code ?? 0);
  }
  process.exit(await runAuth());
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

INGEST = baseUrl('LAB_INGEST_URL', 'ingest-api');
VAULT = baseUrl('LAB_VAULT_URL', 'content-vault');
QUERY = baseUrl('LAB_QUERY_URL', 'query-api');
console.log(`lab: ingest-api ${INGEST}, content-vault ${VAULT}, query-api ${QUERY}`);

console.log('\nprobes (the paths azure/modules/container-app.bicep uses):');
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
check('database/schema.sql applied (ingest tables)', tables === '4', `${tables} tables`);
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
console.log('lab: tear down with: node localdev/run.mjs --down');
process.exit(failures === 0 ? 0 : 1);
