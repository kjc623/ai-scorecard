#!/usr/bin/env node
// check-vocab.mjs — the device components spell every shared closed vocabulary the same way.
//
// device/protocol (Go) is the source of truth; the extension transcribes its vocabularies in
// device/extension/src/messages.js. A one-letter difference compiles on both sides and fails only at
// runtime, so this parses both and compares them by value, with the names aligned. A value only
// capture-core uses is listed as deviceOnly; the extension's own contract test reads those lists.
//
//   node tools/check-vocab.mjs            # exit 1 on drift
//   node tools/check-vocab.mjs --json

import { readFileSync, existsSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const JSON_OUT = process.argv.includes('--json');

/** A vocabulary: a Go const block, a JS consumer, and the names on each side. */
export const VOCABULARIES = [
  {
    id: 'native-message-type',
    title: 'Native-messaging message types (protocol/native.go Type*)',
    go: { file: 'device/protocol/native.go', prefix: 'Type' },
    // The extension splits the two directions into separate frozen objects (TYPE for extension ->
    // core, CORE_TYPE for core -> extension). That is a clearer structure than one flat set, and it
    // is not drift, so the comparison is against the union of the two.
    js: { file: 'device/extension/src/messages.js', groups: ['TYPE', 'CORE_TYPE'] },
    // capture-core, its user-session helper and the tools' hook command speak these on the same
    // endpoint; the extension never sends or receives them.
    deviceOnly: ['helper_hello', 'notify', 'notify_result', 'hook_evaluate', 'hook_decision'],
  },
  {
    id: 'refusal-reason',
    title: 'Refusal reasons (protocol RefusalReason)',
    go: { file: 'device/protocol/native.go', prefix: 'Refusal', pascalValues: true },
    js: { file: 'device/extension/src/messages.js', groups: ['REFUSAL'] },
  },
  {
    id: 'route',
    title: 'Collection routes (protocol Route*)',
    go: { file: 'device/protocol/envelope.go', prefix: 'Route' },
    js: { file: 'device/extension/src/messages.js', groups: ['ROUTE'] },
    // Routes only capture-core's own collectors emit. The extension never sees them, so it does
    // not transcribe them.
    deviceOnly: ['tool.hook', 'tool.otel', 'inv.scan', 'net.flow'],
  },
  {
    id: 'collection-mode',
    title: 'Collection modes (protocol Mode*)',
    go: { file: 'device/protocol/envelope.go', prefix: 'Mode', values: ['m0', 'm1', 'm2', 'm3'] },
    js: { file: 'device/extension/src/messages.js', groups: ['MODE'] },
  },
  {
    id: 'policy-action',
    title: 'Policy actions (protocol Action*)',
    go: { file: 'device/protocol/envelope.go', prefix: 'Action' },
    js: { file: 'device/extension/src/messages.js', groups: ['ACTION'] },
  },
  {
    id: 'counter',
    title: 'Health counters (protocol Counter*)',
    go: { file: 'device/protocol/envelope.go', prefix: 'Counter' },
    js: { file: 'device/extension/src/messages.js', groups: ['COUNTER'] },
  },
  {
    id: 'collector-state',
    title: 'Collector states (protocol State*)',
    go: { file: 'device/protocol/envelope.go', prefix: 'State' },
    js: { file: 'device/extension/src/messages.js', groups: ['STATE'] },
  },
  {
    id: 'detail',
    title: 'Health detail vocabulary (protocol Detail*)',
    go: { file: 'device/protocol/envelope.go', prefix: 'Detail' },
    js: { file: 'device/extension/src/messages.js', groups: ['DETAIL'] },
    // Details only capture-core's own collectors report.
    deviceOnly: ['helper_unavailable', 'component_crash_loop', 'tool_not_installed', 'tool_version_unsupported', 'config_write_failed', 'config_tampered', 'no_recent_events', 'etw_session_failed'],
  },
];

/** Extract `Name ... = "value"` pairs from a Go file, filtered by a name prefix. */
function goConstants(file, prefix) {
  const source = readFileSync(join(ROOT, file), 'utf8');
  const out = new Map();
  const re = new RegExp(`\\b(${prefix}[A-Za-z0-9_]*)\\s*(?:[A-Za-z0-9_.\\[\\]]+\\s*)?=\\s*"([^"]*)"`, 'g');
  for (const m of source.matchAll(re)) out.set(m[1], m[2]);
  return out;
}

/** Extract a frozen object literal's string values from a JS file, e.g. `export const TYPE = {...}`. */
function jsGroup(file, group) {
  const source = readFileSync(join(ROOT, file), 'utf8');
  const start = source.search(new RegExp(`export const ${group}\\s*=\\s*(?:Object\\.freeze\\()?\\{`));
  if (start === -1) return null;
  // Walk braces so a nested object does not truncate the block.
  let i = source.indexOf('{', start);
  let depth = 0;
  let end = -1;
  for (let j = i; j < source.length; j++) {
    if (source[j] === '{') depth++;
    else if (source[j] === '}') {
      depth--;
      if (depth === 0) {
        end = j;
        break;
      }
    }
  }
  if (end === -1) return null;
  const body = source.slice(i + 1, end);
  const out = new Map();
  for (const m of body.matchAll(/([A-Za-z0-9_$]+)\s*:\s*'([^']*)'/g)) out.set(m[1], m[2]);
  return out;
}

function main() {
  const findings = [];
  const summaries = [];

  for (const vocab of VOCABULARIES) {
    const goPath = join(ROOT, vocab.go.file);
    const jsPath = join(ROOT, vocab.js.file);
    if (!existsSync(goPath) || !existsSync(jsPath)) {
      // ABSENT is not agreement: a component that is not there cannot be compared, and reporting
      // it as a clean result would let a deleted consumer pass the drift check.
      findings.push({
        id: vocab.id,
        kind: 'consumer-absent',
        detail: `${vocab.go.file} or ${vocab.js.file} does not exist, so this vocabulary cannot be compared`,
      });
      summaries.push({ id: vocab.id, status: 'ABSENT', go: 0, js: 0, findings: 1 });
      continue;
    }
    const go = goConstants(vocab.go.file, vocab.go.prefix);
    const groups = vocab.js.groups ?? [vocab.js.group];
    const js = new Map();
    const missingGroups = [];
    for (const g of groups) {
      const part = jsGroup(vocab.js.file, g);
      if (!part) {
        missingGroups.push(g);
        continue;
      }
      for (const [k, v] of part) js.set(`${g}.${k}`, v);
    }
    if (js.size === 0) {
      findings.push({
        id: vocab.id,
        kind: 'consumer-group-missing',
        detail: `${vocab.js.file} has no exported ${groups.join(' or ')} object`,
      });
      summaries.push({ id: vocab.id, status: 'FINDINGS', go: go.size, js: 0, findings: 1 });
      continue;
    }
    if (missingGroups.length > 0) {
      findings.push({
        id: vocab.id,
        kind: 'consumer-group-missing',
        detail: `${vocab.js.file} is missing the expected ${missingGroups.join(', ')} object`,
      });
    }

    const goValues = new Set(go.values());
    const jsValues = new Set(js.values());
    const deviceOnly = new Set(vocab.deviceOnly ?? []);
    let count = 0;

    for (const value of deviceOnly) {
      if (!goValues.has(value)) {
        findings.push({
          id: vocab.id,
          kind: 'stale-device-only',
          value,
          detail: `the device-only list names "${value}", which device/protocol does not define`,
        });
        count++;
      }
    }
    for (const [name, value] of go) {
      if (deviceOnly.has(value)) continue;
      if (!jsValues.has(value)) {
        findings.push({
          id: vocab.id,
          kind: 'missing-in-consumer',
          name,
          value,
          detail: `protocol defines ${name} = "${value}"; ${vocab.js.file} (${vocab.js.group}) does not carry that value`,
        });
        count++;
      }
    }
    for (const [name, value] of js) {
      if (!goValues.has(value)) {
        // An empty-string detail is legitimate on the JS side (DetailNone) and has no Go constant
        // with a value, so it is not drift.
        if (value === '') continue;
        findings.push({
          id: vocab.id,
          kind: 'not-in-protocol',
          name,
          value,
          detail: `${vocab.js.file} (${vocab.js.group}) defines ${name} = "${value}"; device/protocol has no such value`,
        });
        count++;
      }
    }

    summaries.push({ id: vocab.id, status: count ? 'FINDINGS' : 'agree', go: go.size, js: js.size, findings: count });
  }

  if (JSON_OUT) {
    console.log(JSON.stringify({ summaries, findings }, null, 2));
    process.exit(findings.length === 0 ? 0 : 1);
  }

  console.log('Shared vocabularies: device/protocol (source of truth) vs consumers');
  console.log('');
  for (const s of summaries) {
    const mark = s.status === 'ABSENT' ? 'ABSENT ' : s.status === 'agree' ? 'agree  ' : 'DRIFT  ';
    console.log(`  [${mark}] ${s.id.padEnd(20)} protocol ${String(s.go).padStart(3)} value(s), consumer ${String(s.js).padStart(3)}`);
  }
  console.log('');
  if (findings.length === 0) {
    console.log('No vocabulary drift.');
  } else {
    console.log(`${findings.length} drift finding(s):`);
    for (const f of findings) console.log(`  - ${f.id} [${f.kind}] ${f.detail}`);
  }
  console.log('');
  console.log('Compared: message types, refusal reasons, routes, modes, policy actions, the seven');
  console.log('counters, collector states, and the detail vocabulary. NOT compared: whether either');
  console.log('side uses a value correctly, and any vocabulary a component re-derives at runtime.');
  process.exit(findings.length === 0 ? 0 : 1);
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) main();
