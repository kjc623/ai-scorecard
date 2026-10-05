// simulate-devices.mjs — put sample events into the auth lab the way devices do.
//
// Every event this writes goes through the device path, unchanged: a simulated device generates its
// own P-256 key, enrols at POST /v1/enrol through the edge with a single-use enrolment token, trades
// a signed assertion for a DPoP-bound access token at POST /v1/token, and then POSTs event batches to
// /v1/events with a fresh DPoP proof per request. ingest-api validates each envelope against the
// contract and writes it with ingest.record_event(), exactly as it would for a real agent. Nothing
// here inserts an event, a submission or a device row.
//
// TWO THINGS ARE SEEDED WITH SQL, because no API creates them: the tenant, and the enrolment tokens
// a device presents once. localdev/run.mjs --auth seeds the same two things the same way.
//
// WHAT THIS CANNOT PRODUCE, because the device path does not produce it:
//   * findings and every mart.* aggregate come from the aggregator; usage aggregates are written by
//     it, and so is ops.coverage_snapshot;
//   * department and population come from the directory sync into ops.user_dim;
//   * `uploaded` content needs a grant and the vault. Every event here stays `not_captured`.
//   * received_at is stamped by the server, so every event is received "now". Device clocks are
//     the only thing a device controls, and a few are set days back to look like a spool flush.
//
// WHAT IT DOES PRODUCE NOW: collector health. It posts a /v1/health report per device after the
// events, so ops.collector_state is populated and the coverage snapshot has something to observe.
//
// The content is invented. Excerpts are short redacted strings, never real data.
//
//   node localdev/tools/simulate-devices.mjs                       # 10 devices, about 300 events
//   node localdev/tools/simulate-devices.mjs --devices 4 --events 80
//
// Needs the auth lab up (node localdev/run.mjs --auth). Zero dependencies.

import { createHash, generateKeyPairSync, randomBytes, randomUUID, sign } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { request } from 'node:https';
import { spawnSync } from 'node:child_process';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..');

function arg(name, fallback) {
  const i = process.argv.indexOf(`--${name}`);
  return i > 0 && process.argv[i + 1] ? process.argv[i + 1] : fallback;
}

const EDGE = arg('edge', 'https://127.0.0.1:8443').replace(/\/+$/, '');
const CA = readFileSync(arg('ca', join(ROOT, 'localdev', '.authlab', 'dev-ca.crt')));
const TENANT = arg('tenant', '5a3c0de0-7e57-4a11-9000-0000000d3a01');
const TENANT_NAME = arg('tenant-name', 'Northwind Freight (sample)');
const DEVICE_COUNT = Number(arg('devices', '10'));
const EVENT_TARGET = Number(arg('events', '300'));
const PG_CONTAINER = arg('pg-container', 'sac-authlab-postgres-1');
const CLASSIFIER = '2026.01.0-shadow';
const SEP = '\u001f';

// ── small helpers ───────────────────────────────────────────────────────────────────────────

const b64url = (buf) => Buffer.from(buf).toString('base64url');
const sha256 = (text) => `sha256:${createHash('sha256').update(text).digest('hex')}`;
const pick = (list) => list[Math.floor(Math.random() * list.length)];
const chance = (p) => Math.random() < p;
const iso = (ms) => new Date(ms).toISOString().replace(/\.\d{3}Z$/, 'Z');

function psql(sql) {
  const r = spawnSync('docker', ['exec', '-i', PG_CONTAINER, 'psql', '-U', 'postgres', '-d', 'shadow', '-v', 'ON_ERROR_STOP=1', '-Atc', sql], { encoding: 'utf8' });
  if (r.status !== 0) throw new Error(`psql failed: ${(r.stderr || r.stdout || '').trim()}`);
  return r.stdout.trim();
}

function post(path, body, headers = {}) {
  const payload = Buffer.from(JSON.stringify(body));
  return new Promise((done, fail) => {
    const req = request(`${EDGE}${path}`, {
      method: 'POST',
      ca: CA,
      minVersion: 'TLSv1.3',
      headers: { 'content-type': 'application/json', 'content-length': payload.length, ...headers },
    }, (res) => {
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => {
        const text = Buffer.concat(chunks).toString('utf8');
        let json = null;
        try { json = JSON.parse(text); } catch { /* a non-JSON body is reported as text */ }
        done({ status: res.statusCode, json, text });
      });
    });
    req.on('error', fail);
    req.end(payload);
  });
}

// ── the device's own JOSE (RFC 7515 ES256, RFC 9449 DPoP), as localdev/authlab/jose.go does it ──

function signES256(header, claims, privateKey) {
  const input = `${b64url(JSON.stringify(header))}.${b64url(JSON.stringify(claims))}`;
  // JOSE wants the fixed-width R||S form, not the ASN.1 one.
  return `${input}.${b64url(sign('sha256', Buffer.from(input), { key: privateKey, dsaEncoding: 'ieee-p1363' }))}`;
}

function dpopProof(device, path, accessToken) {
  const claims = { htm: 'POST', htu: `${EDGE}${path}`, iat: Math.floor(Date.now() / 1000), jti: randomUUID() };
  if (accessToken) claims.ath = createHash('sha256').update(accessToken).digest('base64url');
  return signES256({ typ: 'dpop+jwt', alg: 'ES256', jwk: device.jwk }, claims, device.privateKey);
}

// ── a device: enrol, hold a token, send batches ─────────────────────────────────────────────

async function enrol(index, token) {
  const { privateKey, publicKey } = generateKeyPairSync('ec', { namedCurve: 'P-256' });
  const { kty, crv, x, y } = publicKey.export({ format: 'jwk' });
  const os = index % 3 === 0 ? 'macos' : 'windows';
  // The clear identity the agent now reports (ADR 0021). The tenant's device_identity is 'clear',
  // so the hostname and managed state are stored; a 'hashed' tenant would drop the hostname.
  const hostname = `SIM-${os === 'macos' ? 'MAC' : 'WIN'}-${String(index + 1).padStart(2, '0')}`;
  const device = { index, privateKey, jwk: { kty, crv, x, y }, os, hostname, managedState: index === 1 ? 'managed' : 'managed', monotonic: 1000, accessToken: null, tokenExpires: 0, subjectNames: {} };
  const res = await post('/v1/enrol', {
    schema_version: '1.0',
    enrolment_token: token,
    mode: 'dpop',
    jwk: device.jwk,
    device: { os, agent_version: 'simulate-devices/1', hostname, managed_state: device.managedState, hardware_identity_hash: sha256(`sample-hardware-${TENANT}-${index}`) },
  }, { DPoP: dpopProof(device, '/v1/enrol') });
  if (res.status !== 200 || !res.json?.device_id) throw new Error(`enrol ${index}: ${res.status} ${res.text.slice(0, 300)}`);
  device.id = res.json.device_id;
  return device;
}

async function accessToken(device) {
  if (device.accessToken && Date.now() < device.tokenExpires - 20_000) return device.accessToken;
  const now = Math.floor(Date.now() / 1000);
  const assertion = signES256({ alg: 'ES256', typ: 'JWT' }, {
    iss: 'simulate-devices', sub: device.id, tenant_id: TENANT, iat: now, exp: now + 300, jti: randomUUID(),
  }, device.privateKey);
  const res = await post('/v1/token', {
    grant_type: 'urn:ietf:params:oauth:grant-type:jwt-bearer', assertion, device_id: device.id,
  }, { DPoP: dpopProof(device, '/v1/token') });
  if (res.status !== 200 || !res.json?.access_token) throw new Error(`token ${device.index}: ${res.status} ${res.text.slice(0, 300)}`);
  device.accessToken = res.json.access_token;
  device.tokenExpires = Date.now() + (res.json.expires_in ?? 60) * 1000;
  return device.accessToken;
}

async function sendBatch(device, events) {
  const token = await accessToken(device);
  return post('/v1/events', {
    schema_version: '1.0',
    batch_id: `sim-${randomUUID()}`,
    device_sent_at: iso(Date.now()),
    event_count: events.length,
    events,
  }, { Authorization: `DPoP ${token}`, DPoP: dpopProof(device, '/v1/events', token) });
}

// The health channel: POST /v1/health, one keyed upsert per device per collector (docs/02 §5.4).
// It carries the collector names from ref.collector, not the route names, because the coverage
// tables key on the collector and the server refuses a name the vocabulary does not hold. One
// device is deliberately degraded so the dashboard has a degraded collector to show.
const COLLECTORS = ['capture_extension', 'egress_proxy', 'loopback_broker', 'cli_shim', 'process_detector', 'classifier_host'];

async function sendHealth(device) {
  const token = await accessToken(device);
  const collector = (name) => ({
    collector: name,
    state: device.index === 1 && name === 'egress_proxy' ? 'degraded' : 'healthy',
    detail: device.index === 1 && name === 'egress_proxy' ? 'client_pinned' : undefined,
    version: 'simulate-devices/1',
    counters: { observed: 0, emitted: 0, skipped_not_generative: 0, blind_tunnelled: 0, not_cooperative: 0, dropped: 0, errors: 0 },
  });
  return post('/v1/health', {
    schema_version: '1.0',
    reported_at: iso(Date.now()),
    agent_version: 'simulate-devices/1',
    hostname: device.hostname,
    collection_mode: 'm2',
    managed_state: device.managedState,
    policy_bundle_version: 'sim-lab',
    signature_ok: true,
    spool: { depth_events: 0, capacity_events: 25000, dropped_total: 0, rejected_total: 0 },
    collectors: COLLECTORS.map(collector),
  }, { Authorization: `DPoP ${token}`, DPoP: dpopProof(device, '/v1/health', token) });
}

// ── what the simulated people do ────────────────────────────────────────────────────────────

const TOOLS = ['chatgpt_web', 'chatgpt_web', 'chatgpt_web', 'claude_web', 'claude_web', 'copilot_chat', 'copilot_chat', 'gemini_web', 'perplexity_web', 'cursor_ide'];

/** A data class, the rule that finds it, and an already-redacted excerpt an M2 record may carry. */
const FINDINGS = [
  { class: 'payment_card', rule: 'PCI_PAN_PATTERN', excerpt: 'refund to card [REDACTED:payment_card] exp [REDACTED]', strong: true },
  { class: 'credential', rule: 'SECRET_API_KEY', excerpt: 'export API_KEY=[REDACTED:credential] then rerun', strong: true },
  { class: 'government_id', rule: 'GOV_ID_NUMBER', excerpt: 'driver national id [REDACTED:government_id] on file', strong: true },
  { class: 'customer_pii', rule: 'PII_CUSTOMER_RECORD', excerpt: 'customer [REDACTED:name], [REDACTED:email], shipment delayed', strong: false },
  { class: 'source_code', rule: 'SRC_INTERNAL_REPO', excerpt: 'func routeManifest([REDACTED:source_code])', strong: false },
  { class: 'legal_commercial', rule: 'LEGAL_CONTRACT_TERMS', excerpt: 'clause 14.2 liability cap of [REDACTED:amount]', strong: false },
  { class: 'health', rule: 'PHI_CLINICAL_TERM', excerpt: 'fit note for [REDACTED:name]: [REDACTED:health]', strong: false },
];
const ATTACHMENTS = ['Q3-carrier-rates.xlsx', 'manifest-export.csv', 'driver-roster.pdf', 'route_planner.py', 'MSA-draft-v4.docx'];

/** The dedup key of docs/02 §4, as ingestion/ingest-api/internal/dedup builds it. */
function dedupKey(device, tool, direction, kind, occurredMs, tier, material) {
  const bucket = iso(Math.floor(occurredMs / 300_000) * 300_000);
  return sha256(['sac-dedup-1', TENANT, device.id, tool, direction, kind, bucket, tier, material].join(SEP));
}

function core(device, user, tool, kind, direction, occurredMs, source, mode) {
  device.monotonic += 50 + Math.floor(Math.random() * 4000);
  return {
    schema_version: '1.0',
    event_id: randomUUID(),
    tenant_id: TENANT,
    device_id: device.id,
    user_ref: user,
    // The clear account name at submission time (ADR 0021). The tenant is 'clear', so it is stored;
    // the read falls back to user_ref when a device could not attribute an observation.
    subject_name: device.subjectNames?.[user],
    tool_fingerprint: tool,
    direction,
    kind,
    occurred_at: iso(occurredMs),
    monotonic_offset_ms: device.monotonic,
    source,
    collection_mode: mode,
  };
}

/** One prompt, as one or two observations of it: two routes seeing one submission is normal. */
function prompt(device, user, occurredMs) {
  const tool = pick(TOOLS);
  const roll = Math.random();
  const mode = roll < 0.15 ? 'm0' : roll < 0.65 ? 'm1' : 'm2';
  const size = 160 + Math.floor(Math.random() * 9000);
  const cli = tool === 'cursor_ide';

  if (mode === 'm0') {
    // M0: the device may not read content, so there is no digest, no label and no excerpt.
    const event = core(device, user, tool, 'prompt', 'egress', occurredMs, cli ? 'cli.shim' : 'ext.web_request', 'm0');
    event.size_bytes = size;
    event.policy_decision = { rule_id: 'DEFAULT_LOG', action: 'logged', decided_locally: true };
    event.dedup_key = dedupKey(device, tool, 'egress', 'prompt', occurredMs, 'S', String(size));
    return [event];
  }

  const found = chance(0.5) ? [pick(FINDINGS)] : [];
  if (found.length === 1 && chance(0.2)) {
    const second = pick(FINDINGS);
    if (second.class !== found[0].class) found.push(second);
  }
  const labels = found.map((f) => ({ class: f.class, score: Math.round((0.62 + Math.random() * 0.37) * 100) / 100, rule_id: f.rule }));
  const strong = found.some((f) => f.strong) && labels.some((l) => l.score >= 0.8);
  const action = found.length === 0 ? 'logged' : strong ? (chance(0.45) ? 'blocked' : 'warned') : chance(0.3) ? 'warned' : 'logged';
  const digest = sha256(['sac-canon-1', 'T', `sample prompt ${randomUUID()}`, 'END'].join(SEP));
  const key = dedupKey(device, tool, 'egress', 'prompt', occurredMs, 'T-A', digest);
  const routes = cli ? ['cli.shim'] : chance(0.3) ? ['ext.page_context', 'ext.web_request'] : [chance(0.7) ? 'ext.page_context' : 'ext.web_request'];

  return routes.map((source, i) => {
    const event = core(device, user, tool, 'prompt', 'egress', occurredMs + i * 40, source, mode);
    event.size_bytes = size;
    event.confidence = chance(0.06) ? 'degraded' : strong ? 'high' : found.length > 0 ? 'medium' : 'high';
    event.content_digest = digest;
    event.labels = labels;
    event.classifier_version = CLASSIFIER;
    event.policy_decision = { rule_id: found[0]?.rule ?? 'DEFAULT_LOG', action, decided_locally: true };
    event.dedup_key = key;
    if (mode === 'm2') {
      event.content_excerpt = found.length > 0
        ? { kind: 'redacted_window', text: found[0].excerpt, match_type: found[0].class, redaction_applied: true }
        : { kind: 'redacted_window', text: '[REDACTED: no rule matched; window withheld]', redaction_applied: true };
    }
    if (chance(0.12)) event.attachments = [{ name: pick(ATTACHMENTS), size_bytes: 2000 + Math.floor(Math.random() * 400_000) }];
    return event;
  });
}

function rollup(device, user, occurredMs) {
  const tool = pick(['copilot_chat', 'cursor_ide']);
  const end = Math.floor(occurredMs / 3_600_000) * 3_600_000;
  const event = core(device, user, tool, 'usage_rollup', 'none', occurredMs, 'cli.shim', 'm0');
  event.window_start = iso(end - 3_600_000);
  event.window_end = iso(end);
  event.submission_count = 3 + Math.floor(Math.random() * 40);
  event.bytes_total = event.submission_count * (300 + Math.floor(Math.random() * 2000));
  event.dedup_key = dedupKey(device, tool, 'none', 'usage_rollup', occurredMs, 'R', `${event.window_start}${SEP}${event.window_end}`);
  return [event];
}

function detection(device, user, occurredMs) {
  const basis = device.os === 'macos' ? 'endpoint_security' : pick(['process_scan', 'etw']);
  const event = core(device, user, 'ollama_local', 'model_detection', 'none', occurredMs, 'proc.detect', 'm0');
  event.detection_basis = basis;
  event.dedup_key = dedupKey(device, 'ollama_local', 'none', 'model_detection', occurredMs, 'D', basis);
  return [event];
}

// ── run ─────────────────────────────────────────────────────────────────────────────────────

console.log(`simulate-devices: edge ${EDGE}, tenant ${TENANT}`);

// Seeded with SQL because no API creates a tenant or mints an enrolment token (see the header).
// Ceiling m2: the tenant's devices are permitted an excerpt and never an upload.
const tokens = Array.from({ length: DEVICE_COUNT }, () => `sac1.${TENANT}.${randomBytes(32).toString('base64url')}`);
const region = psql("select coalesce((select residency_region from ops.tenant limit 1), 'authlab-region')");
psql(`
  INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, ceiling_mode)
  VALUES ('${TENANT}', '${TENANT_NAME.replace(/'/g, "''")}', 'active', '${region}', 'vendor', 'm2')
  ON CONFLICT (tenant_id) DO NOTHING;
  ${tokens.map((t) => `INSERT INTO ops.enrolment_token (tenant_id, token_hash, expires_at) VALUES ('${TENANT}', '${sha256(t)}', now() + interval '1 hour');`).join('\n  ')}
`);
console.log(`  seeded (SQL): the tenant row and ${tokens.length} single-use enrolment tokens`);

const devices = [];
for (let i = 0; i < DEVICE_COUNT; i += 1) devices.push(await enrol(i, tokens[i]));
console.log(`  enrolled ${devices.length} devices through ${EDGE}/v1/enrol (dpop)`);

// One or two people per device. A user_ref is a pseudonym; the clear account name rides alongside
// it (ADR 0021), and the tenant's device_identity is 'clear' so it is stored.
const people = devices.flatMap((device, i) => {
  const refs = [{ ref: `u_${createHash('sha256').update(`${TENANT}-person-${i}-a`).digest('hex').slice(0, 4)}`, name: `sim.user.${i}.a@northwind.example` }];
  if (i % 4 === 0) refs.push({ ref: `u_${createHash('sha256').update(`${TENANT}-person-${i}-b`).digest('hex').slice(0, 4)}`, name: `sim.user.${i}.b@northwind.example` });
  return refs.map(({ ref, name }) => {
    device.subjectNames[ref] = name;
    return { device, user: ref };
  });
});

const now = Date.now();
const queues = new Map(devices.map((d) => [d, []]));
let made = 0;
while (made < EVENT_TARGET) {
  const { device, user } = pick(people);
  // Most events are minutes old. A few are days old: a laptop that was offline and then flushed.
  const occurredMs = chance(0.07) ? now - Math.floor((1 + Math.random() * 6) * 86_400_000) : now - Math.floor(Math.random() * 40 * 60_000);
  const roll = Math.random();
  const events = roll < 0.04 ? rollup(device, user, occurredMs) : roll < 0.07 ? detection(device, user, occurredMs) : prompt(device, user, occurredMs);
  queues.get(device).push(...events);
  made += events.length;
}

const totals = { accepted: 0, duplicate: 0, rejected: 0, batches: 0 };
const reasons = new Map();
for (const [device, queue] of queues) {
  queue.sort((a, b) => a.monotonic_offset_ms - b.monotonic_offset_ms);
  for (let i = 0; i < queue.length; i += 20) {
    const res = await sendBatch(device, queue.slice(i, i + 20));
    if (res.status !== 200) throw new Error(`events from device ${device.index}: ${res.status} ${res.text.slice(0, 400)}`);
    totals.batches += 1;
    for (const k of ['accepted', 'duplicate', 'rejected']) totals[k] += res.json.counts?.[k] ?? 0;
    for (const r of res.json.results ?? []) {
      if (r.outcome === 'rejected') reasons.set(`${r.reason} ${r.detail?.pointer ?? ''}`.trim(), (reasons.get(`${r.reason} ${r.detail?.pointer ?? ''}`.trim()) ?? 0) + 1);
    }
  }
}

console.log(`  sent ${made} observations in ${totals.batches} batches through ${EDGE}/v1/events`);
console.log(`  server said: accepted ${totals.accepted}, duplicate ${totals.duplicate}, rejected ${totals.rejected}`);
for (const [reason, count] of reasons) console.log(`    rejected ${count}: ${reason}`);

// Heartbeat every device, so an idle device is reporting and the coverage snapshot has something to
// observe (docs/02 §5.4, docs/04 §3.7).
let healthOk = 0;
for (const device of devices) {
  const res = await sendHealth(device);
  if (res.status !== 200) throw new Error(`health from device ${device.index}: ${res.status} ${res.text.slice(0, 300)}`);
  healthOk += 1;
}
console.log(`  posted ${healthOk} health reports through ${EDGE}/v1/health (${COLLECTORS.length} collectors each)`);
console.log(`  in the database for this tenant: ${psql(`select count(*) from ingest.submission where tenant_id = '${TENANT}'`)} submissions, ${psql(`select count(*) from ingest.observation where tenant_id = '${TENANT}'`)} observations, ${psql(`select count(*) from ops.device where tenant_id = '${TENANT}'`)} devices, ${psql(`select count(*) from ops.collector_state where tenant_id = '${TENANT}'`)} collector-state rows`);
if (totals.rejected > 0) process.exitCode = 1;
