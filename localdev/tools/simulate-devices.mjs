#!/usr/bin/env node
// Puts sample activity into the lab's sample tenant the way devices do. Each simulated device makes
// its own key and CSR, enrols through the edge with the sample tenant's deployment key, then sends
// event batches and a health report with the certificate it was issued. ingest-api validates and
// records every event exactly as it would a real agent's; nothing is written to the database here.
//
//   node localdev/tools/simulate-devices.mjs                       # 10 devices, about 300 events
//   node localdev/tools/simulate-devices.mjs --devices 4 --events 80
//   node localdev/tools/simulate-devices.mjs --client-generated 4  # also 4 client-generated requests
//
// Sign in to the dashboard as analyst@sample.test to see them. Findings and usage aggregates appear
// after the next `jobs aggregate` (every minute in the lab). The excerpts are invented.

import { createHash, randomUUID } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { IDENTITY_DIR, PKI_DIR, TENANTS } from '../lab.mjs';
import { edgeRequest, enrol } from './device.mjs';

function arg(name, fallback) {
  const i = process.argv.indexOf(`--${name}`);
  return i > 0 && process.argv[i + 1] ? process.argv[i + 1] : fallback;
}

const EDGE = arg('edge', `https://127.0.0.1:${process.env.LAB_EDGE_PORT || '8443'}`).replace(/\/+$/, '');
const CA = readFileSync(join(PKI_DIR, 'dev-ca.crt'));
const TENANT = TENANTS.sample.id;
const DEPLOYMENT_KEY = readFileSync(join(IDENTITY_DIR, 'deployment-key.sample'), 'utf8').trim();
const DEVICE_COUNT = Number(arg('devices', '10'));
const EVENT_TARGET = Number(arg('events', '300'));
const CLIENT_GENERATED = Number(arg('client-generated', '0'));
const CLASSIFIER = '1.0.0';
const AGENT = 'simulate-devices';
const SEP = '\u001f';

const sha256 = (text) => `sha256:${createHash('sha256').update(text).digest('hex')}`;
const pick = (list) => list[Math.floor(Math.random() * list.length)];
const chance = (p) => Math.random() < p;
const iso = (ms) => new Date(ms).toISOString().replace(/\.\d{3}Z$/, 'Z');

// Catalogued fingerprints (ref.tool_catalogue) and the routes that observe each kind of tool.
const WEB = ['ext.page_context', 'ext.web_request'];
const TOOLS = [
  { fp: 'tls_11574658dafb8805', routes: WEB }, // ChatGPT (web)
  { fp: 'tls_11574658dafb8805', routes: WEB },
  { fp: 'tls_7659f7a5e64cd885', routes: WEB }, // Gemini (web)
  { fp: 'tls_a40a04ef6e27de9f', routes: WEB }, // Perplexity (web)
  { fp: 'tls_b6681b043244c43f', routes: ['proxy.tls'] }, // Claude Code
  { fp: 'tls_b6681b043244c43f', routes: ['proxy.tls'] },
  { fp: 'tls_f412811be7ac6539', routes: ['cli.shim'] }, // GitHub Copilot
  { fp: 'tls_9230903190dcf0dc', routes: ['cli.shim'] }, // Cursor
  { fp: 'tls_f32477ff734d70d1', routes: ['proxy.tls'] }, // OpenAI API
];
const LOCAL_MODELS = ['proc_ollama', 'proc_lmstudio'];

// The classifier's shipped rules (ref.rule), each with the class it reports and an invented,
// already-redacted excerpt an M2 record may carry.
const FINDINGS = [
  { class: 'payment_card', rule: 'PAYMENT_CARD_PAN', excerpt: 'refund to card [REDACTED:payment_card] exp [REDACTED]', strong: true },
  { class: 'government_id', rule: 'US_SSN_STRUCTURE', excerpt: 'employee SSN [REDACTED:government_id] on file', strong: true },
  { class: 'government_id', rule: 'UK_NINO', excerpt: 'NI number [REDACTED:government_id] for payroll', strong: true },
  { class: 'credential', rule: 'PRIVATE_KEY_BLOCK', excerpt: '[REDACTED:credential] pasted to debug the deploy', strong: true },
  { class: 'credential', rule: 'AWS_ACCESS_KEY_ID', excerpt: 'export AWS_ACCESS_KEY_ID=[REDACTED:credential]', strong: true },
  { class: 'customer_pii', rule: 'CUSTOMER_PII_LABEL', excerpt: 'date of birth [REDACTED], home address [REDACTED]', strong: false },
  { class: 'health', rule: 'HEALTH_CLINICAL_VOCABULARY', excerpt: 'diagnosis and prescription for [REDACTED:name]', strong: false },
  { class: 'legal_commercial', rule: 'LEGAL_CLAUSE_VOCABULARY', excerpt: 'governing law and indemnify wording of [REDACTED]', strong: false },
  { class: 'source_code', rule: 'SOURCE_DECLARATION', excerpt: 'func routeManifest([REDACTED:source_code])', strong: false },
];
const ATTACHMENTS = ['Q3-carrier-rates.xlsx', 'manifest-export.csv', 'driver-roster.pdf', 'route_planner.py', 'MSA-draft-v4.docx'];
const COLLECTORS = ['capture_extension', 'egress_proxy', 'loopback_broker', 'cli_shim', 'process_detector', 'classifier_host'];
const DEFAULT_DECISION = 'policy.default';

/** A dedup key in the shape the agent derives it: tenant, device, tool, kind, 5-minute bucket, material. */
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
    subject_name: device.subjectNames[user],
    tool_fingerprint: tool,
    direction,
    kind,
    occurred_at: iso(occurredMs),
    monotonic_offset_ms: device.monotonic,
    source,
    collection_mode: mode,
  };
}

/** One prompt, as one or two observations: two routes seeing one submission is normal. */
function prompt(device, user, occurredMs) {
  const tool = pick(TOOLS);
  const roll = Math.random();
  const mode = roll < 0.15 ? 'm0' : roll < 0.65 ? 'm1' : 'm2';
  const size = 160 + Math.floor(Math.random() * 9000);

  if (mode === 'm0') {
    // M0 reads no content: no digest, no label, no excerpt.
    const event = core(device, user, tool.fp, 'prompt', 'egress', occurredMs, tool.routes[0], 'm0');
    event.size_bytes = size;
    event.policy_decision = { rule_id: DEFAULT_DECISION, action: 'logged', decided_locally: true };
    event.dedup_key = dedupKey(device, tool.fp, 'egress', 'prompt', occurredMs, 'S', String(size));
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
  const key = dedupKey(device, tool.fp, 'egress', 'prompt', occurredMs, 'T-A', digest);
  const routes = tool.routes.length > 1 && chance(0.3) ? tool.routes : [pick(tool.routes)];

  return routes.map((source, i) => {
    const event = core(device, user, tool.fp, 'prompt', 'egress', occurredMs + i * 40, source, mode);
    event.size_bytes = size;
    event.confidence = chance(0.06) ? 'degraded' : strong ? 'high' : found.length > 0 ? 'medium' : 'high';
    event.content_digest = digest;
    event.labels = labels;
    event.classifier_version = CLASSIFIER;
    event.policy_decision = { rule_id: found[0]?.rule ?? DEFAULT_DECISION, action, decided_locally: true };
    event.dedup_key = key;
    event.prompt_kind = 'user';
    if (mode === 'm2') {
      event.content_excerpt = found.length > 0
        ? { kind: 'redacted_window', text: found[0].excerpt, match_type: found[0].class, redaction_applied: true }
        : { kind: 'redacted_window', text: '[REDACTED: no rule matched; window withheld]', redaction_applied: true };
    }
    if (source.startsWith('ext.') && chance(0.12)) {
      event.attachments = [{ name: pick(ATTACHMENTS), size_bytes: 2000 + Math.floor(Math.random() * 400_000) }];
    }
    return event;
  });
}

/** A request the client made for itself (a title or a summary): no authored text, never indexed. */
function clientGenerated(device, user, occurredMs) {
  const tool = pick(TOOLS.filter((t) => t.routes[0] === 'proxy.tls'));
  const digest = sha256(['sac-canon-1', 'T', `client generated ${randomUUID()}`, 'END'].join(SEP));
  const event = core(device, user, tool.fp, 'prompt', 'egress', occurredMs, 'proxy.tls', 'm1');
  event.size_bytes = 300 + Math.floor(Math.random() * 4000);
  event.confidence = 'high';
  event.content_digest = digest;
  event.labels = [];
  event.classifier_version = CLASSIFIER;
  event.policy_decision = { rule_id: DEFAULT_DECISION, action: 'logged', decided_locally: true };
  event.dedup_key = dedupKey(device, tool.fp, 'egress', 'prompt', occurredMs, 'T-A', digest);
  event.prompt_kind = 'client_generated';
  return [event];
}

function rollup(device, user, occurredMs) {
  const tool = pick(TOOLS.filter((t) => t.routes[0] === 'cli.shim')).fp;
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
  const tool = pick(LOCAL_MODELS);
  const basis = device.os === 'macos' ? 'endpoint_security' : pick(['process_scan', 'etw']);
  const event = core(device, user, tool, 'model_detection', 'none', occurredMs, 'proc.detect', 'm0');
  event.detection_basis = basis;
  event.dedup_key = dedupKey(device, tool, 'none', 'model_detection', occurredMs, 'D', basis);
  return [event];
}

function send(device, path, body) {
  return edgeRequest(EDGE, CA, { path, body, cert: device.cert, key: device.key });
}

function health(device) {
  const now = iso(Date.now());
  const degraded = device.index === 1;
  return send(device, '/v1/health', {
    schema_version: '1.0',
    reported_at: now,
    agent_version: AGENT,
    hostname: device.hostname,
    collection_mode: 'm2',
    managed_state: 'managed',
    policy_bundle_version: 'sim',
    signature_ok: true,
    spool: { depth_events: 0, capacity_events: 25000, dropped_total: 0, rejected_total: 0 },
    collectors: COLLECTORS.map((collector) => ({
      collector,
      state: degraded && collector === 'egress_proxy' ? 'degraded' : 'healthy',
      detail: degraded && collector === 'egress_proxy' ? 'client_pinned' : undefined,
      since: now,
      version: AGENT,
      counters: { observed: 0, emitted: 0, skipped_not_generative: 0, blind_tunnelled: 0, not_cooperative: 0, dropped: 0, errors: 0 },
    })),
  });
}

// --- run ---------------------------------------------------------------------------------------

console.log(`simulate-devices: edge ${EDGE}, tenant ${TENANTS.sample.name} (${TENANT})`);

const devices = [];
for (let index = 0; index < DEVICE_COUNT; index += 1) {
  const os = index % 3 === 0 ? 'macos' : 'windows';
  const hostname = `SIM-${os === 'macos' ? 'MAC' : 'WIN'}-${String(index + 1).padStart(2, '0')}`;
  // A fixed hardware identity per index, so a rerun re-enrols the same devices.
  const enrolled = await enrol(EDGE, CA, DEPLOYMENT_KEY, {
    os, agent_version: AGENT, hostname, managed_state: 'managed', hardware_identity_hash: sha256(`sim-hardware-${TENANT}-${index}`),
  });
  devices.push({ index, os, hostname, id: enrolled.deviceId, cert: enrolled.cert, key: enrolled.key, monotonic: 1000, subjectNames: {} });
}
console.log(`  enrolled ${devices.length} devices with the deployment key`);

// One or two people per device: a pseudonymous user_ref, and the account name beside it.
const people = devices.flatMap((device, i) => {
  const refs = [`${i}-a`, ...(i % 4 === 0 ? [`${i}-b`] : [])];
  return refs.map((r) => {
    const ref = `u_${createHash('sha256').update(`${TENANT}-person-${r}`).digest('hex').slice(0, 8)}`;
    device.subjectNames[ref] = `sim.user.${r}@northwind.example`;
    return { device, user: ref };
  });
});

const now = Date.now();
const queues = new Map(devices.map((d) => [d, []]));
let made = 0;
while (made < EVENT_TARGET) {
  const { device, user } = pick(people);
  // Most events are minutes old; a few are days old, as a laptop that was offline flushes its spool.
  const occurredMs = chance(0.07) ? now - Math.floor((1 + Math.random() * 6) * 86_400_000) : now - Math.floor(Math.random() * 40 * 60_000);
  const roll = Math.random();
  const events = roll < 0.04 ? rollup(device, user, occurredMs) : roll < 0.07 ? detection(device, user, occurredMs) : prompt(device, user, occurredMs);
  queues.get(device).push(...events);
  made += events.length;
}
for (let i = 0; i < CLIENT_GENERATED; i += 1) {
  const { device } = pick(people);
  queues.get(device).push(...clientGenerated(device, 'u_clientgen', now - i * 1000));
  made += 1;
}

const totals = { accepted: 0, duplicate: 0, rejected: 0, batches: 0 };
const reasons = new Map();
for (const [device, queue] of queues) {
  queue.sort((a, b) => a.monotonic_offset_ms - b.monotonic_offset_ms);
  for (let i = 0; i < queue.length; i += 20) {
    const events = queue.slice(i, i + 20);
    const res = await send(device, '/v1/events', {
      schema_version: '1.0', batch_id: `sim-${randomUUID()}`, device_sent_at: iso(Date.now()), event_count: events.length, events,
    });
    if (res.status !== 200) throw new Error(`events from ${device.hostname}: ${res.status} ${res.text.slice(0, 400)}`);
    totals.batches += 1;
    for (const k of ['accepted', 'duplicate', 'rejected']) totals[k] += res.json.counts?.[k] ?? 0;
    for (const r of res.json.results ?? []) {
      if (r.outcome !== 'rejected') continue;
      const reason = `${r.reason} ${r.detail?.pointer ?? ''}`.trim();
      reasons.set(reason, (reasons.get(reason) ?? 0) + 1);
    }
  }
}
console.log(`  sent ${made} observations in ${totals.batches} batches: accepted ${totals.accepted}, duplicate ${totals.duplicate}, rejected ${totals.rejected}`);
for (const [reason, count] of reasons) console.log(`    rejected ${count}: ${reason}`);

for (const device of devices) {
  const res = await health(device);
  if (res.status !== 200) throw new Error(`health from ${device.hostname}: ${res.status} ${res.text.slice(0, 300)}`);
}
console.log(`  posted ${devices.length} health reports`);
if (totals.rejected > 0) process.exitCode = 1;
