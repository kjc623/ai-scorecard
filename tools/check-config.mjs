#!/usr/bin/env node
// check-config.mjs — the deployment and the code agree on configuration.
//
// For every app and job in azure/main.bicep, each environment variable the deployment sets must be
// one the component's code reads. A name set but never read is a typo or a leftover, and the
// setting it was meant to carry silently does not apply. Variables a component reads but the
// deployment does not set are listed for information: they have defaults or serve the local lab.
//
// Exit 1 when the deployment sets a name its component does not read.

import { existsSync, readFileSync, readdirSync, statSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');

// Where each deployed workload's code lives. Go services also read the shared platform module.
const COMPONENTS = {
  ingestApp: ['ingestion/ingest-api', 'platform'],
  controlApp: ['control/control-api', 'platform'],
  vaultApp: ['vault/content-vault', 'platform'],
  queryApp: ['query/query-api'],
  dashboardApp: ['query/dashboard'],
  aggregate: ['jobs', 'platform'],
  expire: ['jobs', 'platform'],
  migrate: ['database', 'platform'],
  'tenant-admin': ['control/control-api', 'platform'],
};

// Injected by the container-app and container-app-job modules for every workload; a component
// that never calls Azure simply ignores it.
const INJECTED = new Set(['AZURE_CLIENT_ID']);

function walk(dir, out = []) {
  if (!existsSync(dir)) return out;
  for (const name of readdirSync(dir)) {
    if (['node_modules', 'test', 'testdata', '.tools'].includes(name)) continue;
    const p = join(dir, name);
    if (statSync(p).isDirectory()) walk(p, out);
    else if (/\.(go|mjs|js)$/.test(name) && !/(_test\.go|\.test\.mjs)$/.test(name)) out.push(p);
  }
  return out;
}

/** Every SAC_* (and Azure identity) variable name a component's source mentions as a string. */
export function namesRead(dirs) {
  const names = new Set();
  for (const file of dirs.flatMap((d) => walk(join(ROOT, d)))) {
    for (const m of readFileSync(file, 'utf8').matchAll(/["'`](SAC_[A-Z0-9_]+|AZURE_CLIENT_ID)["'`]|process\.env\.(SAC_[A-Z0-9_]+)/g)) {
      names.add(m[1] ?? m[2]);
    }
  }
  return names;
}

/** The text of `module <symbol> …` up to its matching brace. */
function block(text, start) {
  let depth = 0;
  for (let i = text.indexOf('{', start); i < text.length; i++) {
    if (text[i] === '{') depth++;
    else if (text[i] === '}' && --depth === 0) return text.slice(start, i + 1);
  }
  return '';
}

const envNames = (text) => [...text.matchAll(/\{\s*name:\s*'([A-Z0-9_]+)'\s*,?\s*(?:value|secret):/g)].map((m) => m[1]);

/** The variables main.bicep sets for each workload, keyed like COMPONENTS. */
export function namesSet(bicep) {
  const pgAt = bicep.indexOf('var pg = [');
  const pg = pgAt < 0 ? [] : envNames(bicep.slice(pgAt, bicep.indexOf('\n]', pgAt)));
  const out = {};
  for (const symbol of Object.keys(COMPONENTS)) {
    const moduleAt = bicep.search(new RegExp(`^module ${symbol} `, 'm'));
    const jobsAt = bicep.indexOf('var jobs = {');
    const jobOffset = jobsAt < 0 ? -1 : bicep.slice(jobsAt).search(new RegExp(`^  '?${symbol}'?: \\{`, 'm'));
    const jobAt = jobOffset < 0 ? -1 : jobsAt + jobOffset;
    const text = moduleAt >= 0 ? block(bicep, moduleAt) : jobAt >= 0 ? block(bicep, jobAt) : null;
    if (text === null) continue;
    out[symbol] = [...new Set([...envNames(text), ...(/concat\(pg,/.test(text) ? pg : [])])];
  }
  return out;
}

export function check(bicep = readFileSync(join(ROOT, 'azure', 'main.bicep'), 'utf8')) {
  const set = namesSet(bicep);
  const problems = [];
  const notes = [];
  for (const [workload, dirs] of Object.entries(COMPONENTS)) {
    if (!set[workload]) {
      problems.push(`${workload}: not found in azure/main.bicep`);
      continue;
    }
    const read = namesRead(dirs);
    for (const name of set[workload]) {
      if (!read.has(name) && !INJECTED.has(name)) problems.push(`${workload}: main.bicep sets ${name}, which ${dirs[0]} never reads`);
    }
    const unset = [...read].filter((n) => !set[workload].includes(n) && !INJECTED.has(n) && !n.startsWith('SAC_TEST_')).sort();
    if (unset.length) notes.push(`${workload}: read but not set by the deployment: ${unset.join(', ')}`);
  }
  return { problems, notes };
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  const { problems, notes } = check();
  for (const n of notes) console.log(`note ${n}`);
  for (const p of problems) console.log(`FAIL ${p}`);
  console.log(problems.length ? `${problems.length} disagreement(s)` : 'ok: every variable the deployment sets is read by its component');
  process.exit(problems.length ? 1 : 0);
}
