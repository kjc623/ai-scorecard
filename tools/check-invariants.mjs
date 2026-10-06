#!/usr/bin/env node
// check-invariants.mjs — structural properties the system depends on, checked across components.
//
// Each is a property one component's own tests cannot see, because it is about what the other
// components do not contain:
//
//   collectors-hold-no-database-credential  device and browser code has no database driver or DSN
//   dashboard-never-speaks-sql              the dashboard holds no SQL and no database driver
//   query-api-binds-values                  hostile values never reach query-api's SQL text
//   spool-is-append-only                    the device spool has no way to rewrite an entry
//   only-content-vault-decrypts             stored content and its keys are touched by content-vault alone
//   coverage-states-are-closed              collector health keeps its four states and its drop counter
//
// Exit 1 on a violation.

import { existsSync, readFileSync, readdirSync, statSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { dirname, join, relative } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const SKIP = new Set(['node_modules', '.git', '.tools', 'testdata', 'dist', 'bin']);

function walk(dir, out = []) {
  if (!existsSync(dir)) return out;
  for (const name of readdirSync(dir)) {
    if (SKIP.has(name)) continue;
    const p = join(dir, name);
    if (statSync(p).isDirectory()) walk(p, out);
    else if (/\.(go|mjs|cjs|js|ts|mod)$/.test(name)) out.push(p);
  }
  return out;
}

const files = (...dirs) => dirs.flatMap((d) => walk(join(ROOT, d)));
const isTest = (f) => /(_test\.go|\.test\.mjs)$/.test(f) || /[\\/](test|test-support)[\\/]/.test(f);

/** Comments blanked; strings kept, because SQL lives in strings. */
function uncommented(text) {
  return text.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '').replace(/\s\/\/\s.*$/gm, '');
}

/** Comments and string literals blanked, so prose and fixtures are not violations. */
function codeOnly(text) {
  return uncommented(text)
    .replace(/`(?:[^`\\]|\\.)*`/g, '``')
    .replace(/"(?:[^"\\\n]|\\.)*"/g, '""')
    .replace(/'(?:[^'\\\n]|\\.)*'/g, "''");
}

function scan(list, pattern, view = codeOnly) {
  const hits = [];
  for (const f of list) {
    const raw = readFileSync(f, 'utf8');
    const text = f.endsWith('.mod') ? raw : view(raw);
    text.split(/\r?\n/).forEach((line, i) => {
      if (pattern.test(line)) hits.push(`${relative(ROOT, f)}:${i + 1}: ${line.trim().slice(0, 120)}`);
    });
  }
  return hits;
}

const checks = {
  'collectors-hold-no-database-credential': () => {
    const device = files('device');
    return [
      ...scan(device, /\b(jackc\/pgx|lib\/pq|database\/sql|sql\.Open)\b/),
      ...scan(device.filter((f) => f.endsWith('go.mod')), /(pgx|lib\/pq)/),
      ...scan(device, /postgres(ql)?:\/\/|PGPASSWORD|SAC_PG_/, uncommented),
    ];
  },

  'dashboard-never-speaks-sql': () => {
    const dash = files('services/dashboard').filter((f) => !isTest(f));
    return [
      ...scan(dash, /\b(SELECT\s+[\w*]+\s+FROM|INSERT\s+INTO|DELETE\s+FROM|UPDATE\s+\w+\s+SET|FROM\s+(ops|mart|ingest|ref)\.)/, uncommented),
      ...scan(dash, /from\s+['"](pg|postgres|pg-native)['"]|require\(['"]pg['"]\)/, uncommented),
    ];
  },

  'query-api-binds-values': () => {
    const entry = join(ROOT, 'services', 'query-api', 'src', 'compile.js');
    if (!existsSync(entry)) return ['services/query-api/src/compile.js is missing'];
    const probe = `
      import { compile } from ${JSON.stringify(pathToFileURL(entry).href)};
      const hostile = ["x; DROP TABLE ops.tenant--", "tool_fingerprint) UNION SELECT 1--", "1;SELECT", '"; --'];
      let reached = 0;
      for (const v of hostile) for (const q of [
        { group_by: [v], order: [{ by: v, dir: 'desc' }], bucket: 'day' },
        { order: [{ by: v }] },
        { filters: [{ field: v, op: 'eq', value: v }] },
      ]) {
        try {
          const out = JSON.stringify(compile(q));
          if (out.includes(v) || /DROP TABLE|UNION SELECT/.test(out)) reached++;
        } catch {}
      }
      console.log(reached);`;
    const r = spawnSync(process.execPath, ['--input-type=module', '-e', probe], { encoding: 'utf8' });
    const reached = Number((r.stdout ?? '').trim().split('\n').pop());
    if (r.status !== 0 || Number.isNaN(reached)) return [`the probe could not run: ${(r.stderr ?? '').trim().slice(0, 300)}`];
    return reached ? [`${reached} hostile input(s) reached SQL text`] : [];
  },

  'spool-is-append-only': () => scan(files('device/capture-spool').filter((f) => f.endsWith('.go') && !isTest(f)),
    /\bfunc\s*\([^)]*\)\s*(Update|SetPayload|Rewrite|ReplaceEntry)\s*\(/),

  'only-content-vault-decrypts': () => {
    // The jobs delete expired content without reading it; the schema and the deployment declare it.
    const elsewhere = files('services/control-api', 'services/ingest-api', 'services/query-api', 'services/dashboard', 'device/capture-core', 'device/capture-spool', 'device/classifier-host', 'device/protocol', 'device/integration', 'device/extension', 'services/platform').filter((f) => !isTest(f));
    return [
      ...scan(elsewhere, /\bops\.content\b(?!_)/, uncommented),
      ...scan(elsewhere.concat(files('services/jobs')), /SAC_CONTENT_KEYS/, uncommented),
    ];
  },

  'coverage-states-are-closed': () => {
    const envelope = join(ROOT, 'device', 'protocol', 'envelope.go');
    if (!existsSync(envelope)) return ['device/protocol/envelope.go is missing'];
    const body = readFileSync(envelope, 'utf8');
    return ['StateHealthy', 'StateDegraded', 'StateAbsent', 'StateTampered', 'CounterDropped']
      .filter((name) => !new RegExp(`\\b${name}\\b`).test(body))
      .map((name) => `${name} is no longer defined`);
  },
};

let failed = 0;
for (const [name, check] of Object.entries(checks)) {
  const problems = check();
  console.log(`${problems.length ? 'FAIL' : 'ok  '} ${name}`);
  for (const p of problems) console.log(`     ${p}`);
  if (problems.length) failed++;
}
process.exit(failed ? 1 : 0);
