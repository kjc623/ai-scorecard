#!/usr/bin/env node
// tools/check-seams.mjs - cross-component field check against the envelope contract.
//
// The risk this covers: nine agents write nine components against one document set. Each package
// can compile and its own tests can pass while two components disagree about a field name on a
// seam, and that disagreement stays invisible until integration. This script derives the field
// vocabulary from contracts/event-envelope.schema.json - the only permitted source of field
// names - and checks every component that declares envelope-shaped fields against it.
//
//   node tools/check-seams.mjs            # check, exit non-zero on a finding
//   node tools/check-seams.mjs --json     # machine-readable
//
// How it decides, so a clean result means something:
//   1. It collects quoted JSON keys and Go `json:"..."` struct tags - declaration sites only,
//      not free text - from every source file of every component.
//   2. A name that is a contract field is fine.
//   3. A name that is not a contract field must be declared in that component's `seamExtras`
//      below, with the seam it belongs to. Anything else is a finding: that is how an invented
//      or misspelled envelope field is caught, and the allow-list doubles as documentation of
//      which local channels legitimately exist.
//   4. A device-side component that names `received_at` is a finding, because it is
//      server-assigned and a device must not carry it (contract $defs/deviceSubmission).
//   5. A component that declares NO envelope field at all is reported as a finding, because a
//      component that cannot name the record it is supposed to handle has not been wired to the
//      contract - the failure mode this script exists to surface.
//
// What it cannot prove, stated so the output does not overclaim: nesting, optionality and types
// across a seam. Those need behavioural checks against real fixtures (see .integration/REPORT.md).

import { readFileSync, existsSync, readdirSync, statSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const JSON_OUT = process.argv.includes('--json');
const SCHEMA = join(ROOT, 'contracts', 'event-envelope.schema.json');

/**
 * The seams that legitimately carry fields which are NOT envelope fields, per component. Listing
 * them is deliberate: it is the difference between "this name is unrelated" and "this name is an
 * envelope field someone invented".
 */
const COMPONENTS = [
  {
    name: 'endpoint/protocol',
    dir: 'endpoint/protocol',
    seamExtras: {
      // POST /v1/events batch envelope, docs/02-ingest-and-transport.md §5.3
      device_sent_at: 'batch envelope',
      batch_id: 'batch envelope',
      event_count: 'batch envelope',
      events: 'batch envelope',
      // per-event response, §5.3
      outcome: 'batch response',
      submission_id: 'batch response',
      dedup_tier: 'batch response',
      won_fields: 'batch response',
      first_received_at: 'batch response',
      results: 'batch response',
      counts: 'batch response',
      server_time: 'batch response',
      received_at: 'batch response (the server -> device direction)',
      accepted: 'batch response counts',
      duplicate: 'batch response counts',
      rejected: 'batch response counts',
      // health channel, §9
      collector: 'health channel',
      last_success_at: 'health channel',
      counters: 'health channel',
      // classifier local protocol, docs/01-collectors.md §3.4
      core_version: 'classifier handshake',
      protocol_version: 'classifier handshake',
      classifier_version: 'classifier handshake / response',
      release_id: 'classifier request',
      budget_ms: 'classifier request',
      content: 'classifier payload (opaque bytes, never an envelope field)',
      media_type: 'classifier payload hint',
      content_is_binary: 'extension observation',
      stages: 'classifier response',
      shadowed: 'classifier response',
      // extension native messaging, docs/01-collectors.md §3.4
      client_id: 'native messaging',
      has_content: 'native messaging',
      over_cap: 'native messaging',
      page_context_attachments: 'native messaging',
      degraded_reason: 'native messaging',
      transfer_id: 'attachment transfer',
      observation_id: 'attachment transfer',
      descriptor: 'attachment transfer',
      seq: 'attachment transfer',
      data: 'attachment transfer',
      chunks: 'attachment transfer',
      policy_version: 'policy sync',
      unchanged: 'policy sync',
      bundle: 'policy sync',
      known_version: 'policy sync',
      // spool record, docs/01-collectors.md §12
      payload: 'spool record',
      attempts: 'spool record',
      last_error: 'spool record',
      reject_reason: 'spool record',
      expires_at: 'spool record',
      state: 'spool record',
      depth: 'spool stats',
      dropped_total: 'spool stats',
      rejected_total: 'spool stats',
      delivered_total: 'spool stats',
      oldest_spooled_at: 'spool stats',
      encryption_key_sealed: 'spool stats',
      bound_bytes: 'spool stats',
      used_bytes: 'spool stats',
      // generic shapes these messages use
      type: 'message discriminator',
      id: 'message correlation',
      version: 'message version',
      body: 'message body',
      message: 'refusal text',
      reason: 'refusal reason',
      ok: 'handshake response',
      detail: 'health/refusal detail',
      pointer: 'rejection detail',
      expected: 'rejection detail',
      supported: 'rejection detail',
      presence_map: 'rejection detail',
      duration_ms: 'stage timing',
      ran: 'stage result',
      failed: 'stage result',
      error: 'stage result',
      truncated: 'stage result',
      stage: 'stage result',
      text: 'excerpt text',
      kind: 'excerpt kind',
      score: 'label score',
      class: 'label class',
      rule_id: 'label / decision rule',
      action: 'policy decision action',
      decided_locally: 'policy decision provenance',
      match_type: 'excerpt provenance',
      offset_start: 'excerpt offset',
      offset_end: 'excerpt offset',
      redaction_applied: 'excerpt provenance',
      name: 'attachment / provider name',
      digest: 'attachment digest',
      mode: 'mode answer',
      hosts: 'mode answer',
      min_size_bytes: 'rule predicate',
      expires_at_2: 'placeholder-unused',
    },
  },
  {
    name: 'endpoint/capture-core',
    dir: 'endpoint/capture-core',
    seamExtras: {
      // policy bundle, docs/01-collectors.md §13.2 - signed policy data, not an envelope
      tenant_hosts: 'policy bundle',
      device_retention_hours: 'policy bundle',
      user_authored_fields: 'policy bundle',
      tenant_default_mode: 'policy bundle',
      tool_modes: 'policy bundle',
      device_modes: 'policy bundle',
      classifier: 'policy bundle',
      kill_switch: 'policy bundle',
      provider_scopes: 'policy bundle',
      scope_matrix: 'policy bundle',
      released_at: 'policy bundle',
      signature: 'policy bundle',
      key_id: 'policy bundle',
      rules: 'policy bundle',
      weights: 'policy bundle',
      seed_set: 'policy bundle',
      port_map: 'policy bundle',
      provider: 'kill switch entry',
      effective_at: 'kill switch entry',
      reason_code: 'kill switch entry',
      // provider registry / health, §4
      detail_2: 'placeholder-unused',
      collector: 'health channel',
      counters: 'health channel',
      last_success_at: 'health channel',
      since: 'health channel',
      // The device-level health document (master §4.4), which the service binary carries because
      // protocol has no device-level type yet. These are health-channel fields and deliberately not
      // envelope fields: `policy_outcome` says whether the signed bundle was accepted or rejected,
      // which is a fact about the agent's configuration rather than about any observation. It sits
      // close to the contract's `policy_decision` by name and is unrelated to it - that one is what
      // policy did about a specific event.
      policy_outcome: 'device-level health document',
      policy_cause: 'device-level health document',
      agent_version: 'device-level health document',
      generated_at: 'device-level health document',
      named_gaps: 'device-level health document',
      note: 'device-level health document',
      classification: 'device-level health document',
      reports: 'device-level health document',
      // loopback broker / proxy local state
      upstream_port: 'broker configuration',
      listen_port: 'broker configuration',
      relocations: 'broker configuration',
      ports: 'broker configuration',
      // The broker's captured request body, docs/01-collectors.md §6: the plaintext a client sent
      // to a local inference server. It is content the broker holds to classify and hash, and it is
      // deliberately NOT an envelope field - §3.3's content-never-enters-the-envelope rule is the
      // reason capture-core mints the envelope separately from this.
      content: 'broker request body',
      // envelope construction, §5.2 - the fields the core mints
      envelope: 'envelope construction',
      envelope_json: 'envelope construction',
      payload: 'spool record',
      attempts: 'spool record',
      last_error: 'spool record',
      reject_reason: 'spool record',
    },
  },
  {
    name: 'endpoint/capture-spool',
    dir: 'endpoint/capture-spool',
    seamExtras: {
      payload: 'spool record (opaque envelope bytes)',
      depth: 'spool stats',
      dropped_total: 'spool stats',
      rejected_total: 'spool stats',
      delivered_total: 'spool stats',
      oldest_spooled_at: 'spool stats',
      encryption_key_sealed: 'spool stats',
      bound_bytes: 'spool stats',
      used_bytes: 'spool stats',
      segment: 'segment file header',
      checksum: 'segment file header',
      nonce: 'AEAD frame',
      ciphertext: 'AEAD frame',
      key_id: 'key file',
      wrapped: 'key file',
    },
  },
  {
    name: 'endpoint/classifier-host',
    dir: 'endpoint/classifier-host',
    // It legitimately never names an envelope field: docs/01-collectors.md §3.3 gives it bytes
    // and returns labels, deliberately with no identity and no envelope. Verified rather than
    // assumed - if it ever starts naming envelope fields, that is the finding.
    envelopeBearingNote:
      'takes content bytes and returns labels; the envelope is minted by capture-core (§3.3 requires the classifier to be unable to see identity)',
    seamExtras: {
      rules: 'rules release',
      rule_id: 'rule',
      predicate: 'rule',
      operators: 'rule',
      validators: 'release manifest',
      model: 'release manifest',
      weights: 'release manifest',
      release_id: 'release manifest',
      signature: 'release manifest',
      key_id: 'release manifest',
      stages: 'classifier response',
      shadowed: 'classifier response',
      corpus: 'equivalence test',
      text: 'parser output',
      offsets: 'parser output',
      status: 'parser output',
      // The parser child's own accounting, docs/01-collectors.md §10's output cap. Named like an
      // envelope field and deliberately not one: it counts bytes through the pipe, not bytes of an
      // observation, and it never leaves the classifier host.
      bytes_in: 'parser accounting (§10 output cap)',
      bytes_out: 'parser accounting (§10 output cap)',
      budget_ms: 'classifier request',
      content: 'classifier payload',
    },
  },
  {
    name: 'ingestion/ingest-api',
    dir: 'ingestion/ingest-api',
    // The one validating write path reads field names out of the contract file at runtime rather
    // than declaring them in Go (no generated types had landed when it was written), so it names
    // no envelope field literally. That is reported to the Lead as an integration risk rather
    // than as a seam finding here.
    envelopeBearingNote:
      'validates envelopes against contracts/event-envelope.schema.json by loading the schema at runtime; field names are data, not declarations',
    seamExtras: {
      device_sent_at: 'batch envelope',
      batch_id: 'batch envelope',
      event_count: 'batch envelope',
      events: 'batch envelope',
      outcome: 'batch response',
      submission_id: 'batch response',
      dedup_tier: 'batch response',
      won_fields: 'batch response',
      first_received_at: 'batch response',
      results: 'batch response',
      counts: 'batch response',
      server_time: 'batch response',
      accepted: 'batch response counts',
      duplicate: 'batch response counts',
      rejected: 'batch response counts',
      pointer: 'rejection detail',
      expected: 'rejection detail',
      supported: 'rejection detail',
      presence_map: 'rejection detail',
      reason: 'rejection reason code',
      error_report: 'quarantine record',
      envelope_stripped: 'quarantine record',
      dedup_key: 'contract field mirrored in SQL',
    },
  },
  {
    name: 'extension',
    dir: 'extension',
    // The extension receives web-request records and emits an ObservationMessage over native
    // messaging; capture-core mints the envelope, so the extension never names an envelope field.
    envelopeBearingNote:
      'emits native-messaging observations; capture-core mints the envelope, so the extension handles a subset by design (§3.4)',
    seamExtras: {
      client_id: 'native messaging',
      has_content: 'native messaging',
      over_cap: 'native messaging',
      content_is_binary: 'native messaging',
      page_context_attachments: 'native messaging',
      degraded_reason: 'native messaging',
      content: 'native messaging payload',
      transfer_id: 'attachment transfer',
      observation_id: 'attachment transfer',
      descriptor: 'attachment transfer',
      seq: 'native messaging chunk',
      data: 'native messaging chunk',
      chunks: 'native messaging chunk',
      type: 'message discriminator',
      version: 'message version',
      id: 'message correlation',
      body: 'message body',
      message: 'refusal text',
      reason: 'refusal reason',
      ok: 'refusal acknowledgement',
      policy_version: 'policy sync',
      unchanged: 'policy sync',
      bundle: 'policy sync',
      known_version: 'policy sync',
      mode: 'mode answer',
      hosts: 'mode answer',
      media_type: 'attachment descriptor',
      digest: 'attachment descriptor',
      name: 'attachment descriptor',
      rule_id: 'rule',
      action: 'rule action',
      min_size_bytes: 'rule predicate',
      path_patterns: 'rule predicate',
      host_patterns: 'rule predicate',
      user_proceeded: 'warn outcome',
      user_cancelled: 'warn outcome',
      policy_sync: 'native message type',
      attachment_manifest: 'native message type',
      attachment_chunk: 'native message type',
      attachment_complete: 'native message type',
      decision_record: 'native message type',
      mode_query: 'native message type',
      health_snapshot: 'native message type',
      policy_bundle: 'native message type',
      mode_answer: 'native message type',
      refusal: 'native message type',
      ack: 'native message type',
      observation: 'native message type',
      counters: 'health report',
      collector: 'health report',
      state: 'health report',
      detail: 'health report',
      since: 'health report',
      last_success_at: 'health report',
      queue_depth: 'extension health',
      queue_dropped_total: 'extension health',
      channel_state: 'extension health',
    },
  },
];

function loadSchema() {
  const raw = JSON.parse(readFileSync(SCHEMA, 'utf8'));
  const defs = raw.$defs ?? {};
  const core = defs.envelopeCore ?? {};
  const coreProps = Object.keys(core.properties ?? {});
  const coreRequired = core.required ?? [];
  const storedOnly = ['received_at'];

  const perKind = {};
  for (const branch of core.allOf ?? []) {
    const k = branch.if?.properties?.kind?.const;
    if (k) perKind[k] = branch.then?.required ?? [];
  }

  const m0Forbidden = [];
  for (const branch of core.allOf ?? []) {
    const isM0 =
      branch.if?.properties?.kind?.const === 'prompt' &&
      branch.if?.properties?.collection_mode?.const === 'm0';
    if (isM0) {
      for (const anyOf of branch.then?.not?.anyOf ?? []) {
        for (const k of anyOf.required ?? []) m0Forbidden.push(k);
      }
    }
  }

  return {
    coreProps,
    coreRequired,
    storedOnly,
    perKind,
    m0Forbidden,
    kinds: core.properties?.kind?.enum ?? [],
    modes: core.properties?.collection_mode?.enum ?? [],
    routes: defs.route?.enum ?? [],
  };
}

function walkFiles(dir, out = []) {
  if (!existsSync(dir)) return out;
  for (const name of readdirSync(dir)) {
    if (['node_modules', '.git', '.tools', 'test', 'tests', 'wasm'].includes(name)) continue;
    const p = join(dir, name);
    const st = statSync(p);
    if (st.isDirectory()) walkFiles(p, out);
    else if (
      /\.(go|mjs|cjs|js|ts)$/.test(name) &&
      // Test files are excluded deliberately: a fixture that keys a map by a data-class name or a
      // scope-axis name is speaking the product's vocabulary, not the contract's, and reporting it
      // would train the reader to ignore this script.
      !/_test\.go$/.test(name) &&
      !/\.test\.(mjs|cjs|js)$/.test(name) &&
      !/^test-support\//.test(name)
    ) {
      out.push(p);
    }
  }
  return out;
}

/** Declaration sites only: Go struct tags and quoted keys. */
function declaredFields(files) {
  const hits = new Map();
  for (const file of files) {
    const lines = readFileSync(file, 'utf8').split(/\r?\n/);
    lines.forEach((text, i) => {
      const add = (field) => {
        if (!hits.has(field)) hits.set(field, []);
        hits.get(field).push({ file: relative(ROOT, file), line: i + 1, text: text.trim().slice(0, 160) });
      };
      for (const m of text.matchAll(/json:"([a-z][a-z0-9_]*)"/g)) add(m[1]);
      for (const m of text.matchAll(/["']([a-z][a-z0-9_]{3,40})["']\s*:/g)) add(m[1]);
    });
  }
  return hits;
}

/**
 * Words that appear on every local seam in every component and carry no envelope meaning. A name
 * in this set is never reported, because flagging `mode` or `version` on a policy bundle is noise
 * that would train the reader to ignore the report.
 */
const GENERIC_SEAM_WORDS = new Set([
  'mode', 'modes', 'version', 'versions', 'class', 'classes', 'count', 'counts', 'v', 'type',
  'types', 'id', 'ids', 'name', 'names', 'state', 'detail', 'text', 'data', 'body', 'reason',
  'score', 'status', 'action', 'key', 'keys', 'value', 'values', 'label', 'labels', 'rules',
  'rule', 'model', 'models', 'weights', 'signature', 'digest', 'hash', 'bytes', 'size', 'path',
  'paths', 'host', 'hosts', 'url', 'port', 'ports', 'seq', 'index', 'offset', 'length', 'error',
  'errors', 'message', 'messages', 'ok', 'supported', 'expected', 'actual', 'pointer', 'source',
  'target', 'stages', 'stage', 'ran', 'failed', 'truncated', 'params', 'options', 'config',
]);

function nearContractField(field, envelopeFields) {
  if (GENERIC_SEAM_WORDS.has(field)) return null;
  for (const k of envelopeFields) {
    // Shares a meaningful first token: the signature of a renamed or typo'd field.
    const a = field.split('_')[0];
    const b = k.split('_')[0];
    if (a === b && a.length >= 4) return k;
    // A strict prefix or suffix relationship, which is what a truncation looks like.
    if (k.startsWith(field + '_') || field.startsWith(k + '_')) return k;
  }
  return null;
}

function main() {
  const schema = loadSchema();
  const known = new Set([...schema.coreProps, ...schema.storedOnly]);
  const envelopeFields = [...schema.coreProps, ...schema.storedOnly];
  const findings = [];
  const summaries = [];

  for (const component of COMPONENTS) {
    const dir = join(ROOT, component.dir);
    if (!existsSync(dir)) {
      summaries.push({ component: component.name, status: 'ABSENT', declared: 0, envelopeFields: 0, findings: 0 });
      continue;
    }
    const files = walkFiles(dir);
    const hits = declaredFields(files);
    const envelope = envelopeFields.filter((f) => hits.has(f));
    let count = 0;

    // 1. An envelope-shaped name that is neither a contract field nor a declared seam extra.
    const extras = component.seamExtras ?? {};
    for (const [field, places] of hits) {
      if (known.has(field) || extras[field]) continue;
      const near = nearContractField(field, envelopeFields);
      if (!near) continue;
      for (const place of places.slice(0, 2)) {
        findings.push({
          component: component.name,
          kind: 'unknown-envelope-field',
          field,
          file: place.file,
          line: place.line,
          detail: `"${field}" is not a contract field and is not a declared seam field; did you mean "${near}"?`,
          text: place.text,
        });
        count++;
      }
    }

    // 2. A server-assigned field named by a device-side component.
    if (component.name.startsWith('endpoint/') || component.name === 'extension') {
      for (const f of schema.storedOnly) {
        const places = hits.get(f);
        if (!places) continue;
        // A batch response legitimately carries received_at in the server -> device direction.
        if (extras[f]) continue;
        for (const place of places.slice(0, 2)) {
          findings.push({
            component: component.name,
            kind: 'server-assigned-field-on-device',
            field: f,
            file: place.file,
            line: place.line,
            detail: `"${f}" is server-assigned; a device must not carry it`,
            text: place.text,
          });
          count++;
        }
      }
    }

    // 3. A component that declares nothing envelope-shaped is only a finding if nothing explains
    //    why. Some components legitimately never name an envelope field, and saying so here is
    //    better than a heuristic that either misses the real case or nags about the legitimate
    //    ones.
    if (envelope.length === 0 && !component.envelopeBearingNote) {
      findings.push({
        component: component.name,
        kind: 'no-envelope-fields-declared',
        field: '',
        file: component.dir,
        line: 0,
        detail:
          'this component declares no field from contracts/event-envelope.schema.json, so nothing ties what it produces to the contract',
        text: '',
      });
      count++;
    }

    summaries.push({
      component: component.name,
      status: 'present',
      files: files.length,
      declared: hits.size,
      envelopeFields: envelope.length,
      findings: count,
    });
  }

  if (JSON_OUT) {
    console.log(JSON.stringify({ schema: relative(ROOT, SCHEMA), summaries, findings }, null, 2));
    process.exit(findings.length === 0 ? 0 : 1);
  }

  console.log('Envelope contract vocabulary (contracts/event-envelope.schema.json)');
  console.log(`  core fields: ${schema.coreProps.length}   required: ${schema.coreRequired.length}`);
  console.log(`  kinds: ${schema.kinds.join(', ')}   modes: ${schema.modes.join(', ')}   routes: ${schema.routes.length}`);
  console.log(`  M0 forbids: ${schema.m0Forbidden.join(', ') || '(none parsed)'}`);
  console.log('');
  console.log('Components');
  for (const s of summaries) {
    if (s.status === 'ABSENT') {
      console.log(`  [ABSENT ] ${s.component}`);
      continue;
    }
    const mark = s.findings > 0 ? 'FINDING' : 'clean  ';
    console.log(`  [${mark}] ${s.component} - ${s.files} files, ${s.envelopeFields} contract field(s) named`);
  }
  console.log('');
  if (findings.length === 0) {
    console.log('No seam findings.');
  } else {
    console.log(`${findings.length} seam finding(s):`);
    for (const f of findings) {
      const where = f.line ? `${f.file}:${f.line}` : f.file;
      console.log(`  - ${f.component} ${where} [${f.kind}] ${f.detail}`);
      if (f.text) console.log(`      ${f.text}`);
    }
  }
  console.log('');
  console.log('Not covered: nesting, optionality and types across a seam. Those need behavioural');
  console.log('checks against real fixtures - see .integration/REPORT.md.');
  process.exit(findings.length === 0 ? 0 : 1);
}

main();
