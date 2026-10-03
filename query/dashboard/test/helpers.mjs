// helpers.mjs — test scaffolding. Not a test file itself.
//
// The dashboard has no DOM dependency in src/ except app.js's boot(), so most tests drive view
// models and HTML strings directly. The few that need a document get the smallest one that works.

import { readFileSync, readdirSync, statSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

export const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
export const REPO = resolve(ROOT, '..', '..');

/** Every .js/.mjs file in this package, tests excluded unless asked for. */
export function sourceFiles({ includeTests = false } = {}) {
  const out = [];
  const walk = (dir) => {
    for (const name of readdirSync(dir)) {
      if (name === 'node_modules' || name === '.git') continue;
      const path = join(dir, name);
      const info = statSync(path);
      if (info.isDirectory()) walk(path);
      else if (/\.(mjs|js)$/.test(name)) {
        const rel = relative(ROOT, path).replace(/\\/g, '/');
        if (!includeTests && rel.startsWith('test/')) continue;
        out.push(rel);
      }
    }
  };
  walk(ROOT);
  return out.sort();
}

/**
 * Strip comments and string literals, the way tools/check-invariants.mjs does, so a mention in
 * prose is not a violation and a scanner does not match its own pattern text.
 */
export function codeOnly(text, file = 'x.js') {
  let t = text;
  t = t.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '');
  t = t.replace(/`(?:[^`\\]|\\.)*`/g, '``');
  t = t.replace(/'(?:[^'\\]|\\.)*'/g, "''");
  t = t.replace(/"(?:[^"\\]|\\.)*"/g, '""');
  void file;
  return t;
}

/** The smallest document `boot()` needs, plus a way to navigate. */
export function fakeDocument(hash = '#posture') {
  const elements = new Map();
  const listeners = [];
  const doc = {
    readyState: 'complete',
    location: { hash },
    getElementById(id) {
      if (!elements.has(id)) elements.set(id, { id, innerHTML: '' });
      return elements.get(id);
    },
    addEventListener(type, fn) {
      listeners.push({ type, fn });
    },
    /** Change the hash and fire the listener, as a real navigation would. */
    async navigate(next) {
      doc.location.hash = next;
      for (const l of listeners) if (l.type === 'hashchange') await l.fn();
    },
    html(id) {
      return doc.getElementById(id).innerHTML;
    },
    elements,
  };
  return doc;
}

/** Import the inline module that index.html carries, so a test drives exactly what a browser runs. */
export async function importInlineModule() {
  const html = readFileSync(join(ROOT, 'index.html'), 'utf8');
  const match = /<script type="module">([\s\S]*?)<\/script>/.exec(html);
  if (!match) throw new Error('index.html has no inline module block');
  const source = match[1];
  const url = `data:text/javascript;base64,${Buffer.from(source, 'utf8').toString('base64')}`;
  return import(url);
}

/** Extract the inline module source without evaluating it. */
export function inlineModuleSource() {
  const html = readFileSync(join(ROOT, 'index.html'), 'utf8');
  const match = /<script type="module">([\s\S]*?)<\/script>/.exec(html);
  if (!match) throw new Error('index.html has no inline module block');
  return match[1];
}

/** A envelope builder for tests that need one specific state. */
export function envelope(resultState, body = {}) {
  return { api_version: '1', query_version: '1', result_state: resultState, ...body };
}

export const FRESH = Object.freeze({
  aggregate: 'mart.agg_tool_period',
  bucket_size: 'day',
  last_run_at: '2026-10-01T11:57:00Z',
  last_complete_bucket: '2026-10-01T11:00:00Z',
  lag_seconds: 180,
  state: 'fresh',
});

export const COMPLETE = Object.freeze({
  window: ['2026-09-24', '2026-10-01'],
  devices_reporting: 4620,
  devices_enrolled: 4620,
  expected_collector_days: 27720,
  observed_collector_days: 27720,
  gap_reasons: {},
  gaps_total: 0,
  state: 'complete',
});

export const PARTIAL = Object.freeze({
  ...COMPLETE,
  devices_reporting: 4180,
  observed_collector_days: 25080,
  gap_reasons: { not_enrolled: 440, unknown: 12 },
  gaps_total: 452,
  state: 'partial',
});
