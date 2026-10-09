#!/usr/bin/env node
// check-config.mjs — the deployments and the code agree on configuration.
//
// For every app and job in azure/main.bicep, and every Fly.io app in fly/, each environment
// variable the deployment sets must be one the component's code reads. A name set but never read
// is a typo or a leftover, and the setting it was meant to carry silently does not apply.
// Variables a component reads but the deployment does not set are listed for information: they
// have defaults or serve the local lab.
//
// A Fly.io app's variables are its fly.toml [env], the -e values the deploy workflow passes to
// fly/scripts/deploy-app.sh, and the secrets fly/scripts set on it. A secret that a [[files]]
// entry writes out is a file, not a variable: its path must be a variable the app sets.
//
// Exit 1 when a deployment sets a name its component does not read.

import { existsSync, readFileSync, readdirSync, statSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');

// Where each deployed workload's code lives. Go services also read the shared platform module.
const COMPONENTS = {
  ingestApp: ['services/ingest-api', 'services/platform'],
  controlApp: ['services/control-api', 'services/platform'],
  vaultApp: ['services/content-vault', 'services/platform'],
  queryApp: ['services/query-api'],
  dashboardApp: ['services/dashboard'],
  aggregate: ['services/jobs', 'services/platform'],
  expire: ['services/jobs', 'services/platform'],
  migrate: ['services/database', 'services/platform'],
  'tenant-admin': ['services/control-api', 'services/platform'],
};

// Where each Fly.io app's code lives, by its directory under fly/.
const FLY_COMPONENTS = {
  edge: ['services/edge'],
  'ingest-api': ['services/ingest-api', 'services/platform'],
  'control-api': ['services/control-api', 'services/platform'],
  'content-vault': ['services/content-vault', 'services/platform'],
  'query-api': ['services/query-api'],
  dashboard: ['services/dashboard'],
  jobs: ['services/jobs', 'services/platform'],
  migrate: ['services/database', 'services/platform'],
};

// Injected by the container-app and container-app-job modules for every workload; a component
// that never calls Azure simply ignores it.
const INJECTED = new Set(['AZURE_CLIENT_ID']);

// Read by a Fly.io app's runtime rather than by its code.
const RUNTIME = { 'query-api': new Set(['NODE_EXTRA_CA_CERTS']) };

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

/** Every SAC_* and EDGE_* (and Azure identity) variable name a component's source mentions as a string. */
export function namesRead(dirs) {
  const names = new Set();
  for (const file of dirs.flatMap((d) => walk(join(ROOT, d)))) {
    for (const m of readFileSync(file, 'utf8').matchAll(/["'`]((?:SAC|EDGE)_[A-Z0-9_]+|AZURE_CLIENT_ID)["'`]|\benv\.((?:SAC|EDGE)_[A-Z0-9_]+)/g)) {
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

// --- Fly.io ------------------------------------------------------------------------------------

/**
 * The keys of a fly.toml that this check needs: `app`, the [env] table, and each [[files]]
 * entry. A small reader of the TOML the files use: tables, arrays of tables, and key = value with
 * basic, literal and multi-line strings.
 */
export function readFlyToml(text) {
  const out = { app: null, env: {}, files: [] };
  let table = '';
  let entry = null;
  let i = 0;
  const value = () => {
    for (const q of ["'''", '"""']) {
      if (text.startsWith(q, i)) {
        const end = text.indexOf(q, i + 3);
        let v = text.slice(i + 3, end);
        if (v.startsWith('\n')) v = v.slice(1);
        i = end + 3;
        return v;
      }
    }
    if (text[i] === "'") {
      const end = text.indexOf("'", i + 1);
      const v = text.slice(i + 1, end);
      i = end + 1;
      return v;
    }
    if (text[i] === '"') {
      let v = '';
      for (i++; text[i] !== '"'; i++) v += text[i] === '\\' ? text[++i] : text[i];
      i++;
      return v;
    }
    const end = text.indexOf('\n', i) < 0 ? text.length : text.indexOf('\n', i);
    const v = text.slice(i, end).replace(/\s+#.*$/, '').trim();
    i = end;
    return v;
  };
  while (i < text.length) {
    const lineEnd = text.indexOf('\n', i) < 0 ? text.length : text.indexOf('\n', i);
    const line = text.slice(i, lineEnd).trim();
    let m;
    if (line === '' || line.startsWith('#')) {
      i = lineEnd + 1;
    } else if ((m = /^\[\[\s*([\w.-]+)\s*\]\]/.exec(line))) {
      table = m[1];
      entry = {};
      if (table === 'files') out.files.push(entry);
      i = lineEnd + 1;
    } else if ((m = /^\[\s*([\w.-]+)\s*\]/.exec(line))) {
      table = m[1];
      entry = null;
      i = lineEnd + 1;
    } else if ((m = /^([\w.-]+|"[^"]*")\s*=\s*/.exec(line))) {
      const key = m[1].replace(/^"|"$/g, '');
      i = text.indexOf(m[0], i) + m[0].length;
      const v = value();
      if (table === '' && key === 'app') out.app = v;
      else if (table === 'env') out.env[key] = v;
      else if (table === 'files') entry[key] = v;
      const next = text.indexOf('\n', i);
      i = next < 0 ? text.length : next + 1;
    } else {
      throw new Error(`fly.toml: cannot read the line ${JSON.stringify(line)}`);
    }
  }
  return out;
}

/** The -e names the deploy workflow passes to fly/scripts/deploy-app.sh, by component. */
export function workflowEnv(workflow) {
  const out = {};
  const joined = workflow.replace(/\\\r?\n\s*/g, ' ');
  for (const m of joined.matchAll(/fly\/scripts\/deploy-app\.sh\s+([a-z][a-z0-9-]*)([^\n]*)/g)) {
    const names = [...m[2].matchAll(/-e\s+"?([A-Z][A-Z0-9_]*)=/g)].map((e) => e[1]);
    out[m[1]] = [...new Set([...(out[m[1]] ?? []), ...names])];
  }
  return out;
}

/** The secrets fly/scripts set, by component: their `put`, `put_file`, `create` and `stage` lines. */
export function scriptSecrets(scripts) {
  const out = {};
  for (const text of Object.values(scripts)) {
    for (const m of text.matchAll(/^\s*(?:put|put_file|create|stage)\s+([A-Z][A-Z0-9_]*)\s+"[^"\n]*"((?:[ \t]+[a-z][a-z0-9-]*)+)/gm)) {
      for (const component of m[2].trim().split(/\s+/)) out[component] = [...new Set([...(out[component] ?? []), m[1]])];
    }
  }
  return out;
}

/** fly/: each app's fly.toml, the deploy workflow, and the scripts. */
export function readFly() {
  const dir = join(ROOT, 'fly');
  const tomls = {};
  for (const name of readdirSync(dir)) {
    if (existsSync(join(dir, name, 'fly.toml'))) tomls[name] = readFileSync(join(dir, name, 'fly.toml'), 'utf8');
  }
  const scripts = {};
  for (const name of readdirSync(join(dir, 'scripts')).filter((n) => n.endsWith('.sh'))) {
    scripts[name] = readFileSync(join(dir, 'scripts', name), 'utf8');
  }
  return { tomls, scripts, workflow: readFileSync(join(ROOT, '.github', 'workflows', 'deploy.yml'), 'utf8') };
}

export function checkFly({ tomls, scripts, workflow } = readFly()) {
  const problems = [];
  const notes = [];
  const apps = Object.fromEntries(Object.entries(tomls).map(([c, t]) => [c, readFlyToml(t)]));
  const passed = workflowEnv(workflow);
  const secrets = scriptSecrets(scripts);
  const prefixes = new Set();
  for (const [component, app] of Object.entries(apps)) {
    if (!app.app?.endsWith(`-${component}`)) problems.push(`fly/${component}: its app ${app.app} is not <prefix>-${component}`);
    else prefixes.add(app.app.slice(0, -component.length - 1));
  }
  if (prefixes.size > 1) problems.push(`fly/: the apps use more than one prefix (${[...prefixes].join(', ')})`);
  const appNames = new Set(Object.values(apps).map((a) => a.app));
  for (const component of new Set([...Object.keys(passed), ...Object.keys(secrets)])) {
    if (!apps[component]) problems.push(`fly/${component}: the workflow or a script configures it, and it has no fly.toml`);
  }
  for (const [component, app] of Object.entries(apps)) {
    const dirs = FLY_COMPONENTS[component];
    if (!dirs) {
      problems.push(`fly/${component}: no component is known for it in tools/check-config.mjs`);
      continue;
    }
    const read = namesRead(dirs);
    const runtime = RUNTIME[component] ?? new Set();
    const fileSecrets = new Set(app.files.map((f) => f.secret_name).filter(Boolean));
    const envValues = new Set(Object.values(app.env));
    for (const f of app.files) {
      if (f.secret_name && !(secrets[component] ?? []).includes(f.secret_name)) {
        problems.push(`fly/${component}: [[files]] reads the secret ${f.secret_name}, which no fly/scripts script sets on it`);
      }
      if (!envValues.has(f.guest_path)) problems.push(`fly/${component}: [[files]] writes ${f.guest_path}, which no [env] setting names`);
    }
    for (const value of Object.values(app.env)) {
      for (const m of value.matchAll(/\/\/([a-z0-9-]+)\.internal\b/g)) {
        if (!appNames.has(m[1])) problems.push(`fly/${component}: ${m[1]}.internal is not an app in fly/`);
      }
    }
    const sources = [
      ['fly.toml sets', Object.keys(app.env)],
      ['the deploy workflow passes', passed[component] ?? []],
      ['fly/scripts set the secret', (secrets[component] ?? []).filter((n) => !fileSecrets.has(n))],
    ];
    const set = new Set();
    for (const [how, names] of sources) {
      for (const name of names) {
        set.add(name);
        if (!read.has(name) && !runtime.has(name)) problems.push(`fly/${component}: ${how} ${name}, which ${dirs[0]} never reads`);
      }
    }
    const unset = [...read].filter((n) => !set.has(n) && !INJECTED.has(n) && !n.startsWith('SAC_TEST_')).sort();
    if (unset.length) notes.push(`fly/${component}: read but not set by the deployment: ${unset.join(', ')}`);
  }
  return { problems, notes };
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  const azure = check();
  const fly = checkFly();
  const problems = [...azure.problems, ...fly.problems];
  for (const n of [...azure.notes, ...fly.notes]) console.log(`note ${n}`);
  for (const p of problems) console.log(`FAIL ${p}`);
  console.log(problems.length ? `${problems.length} disagreement(s)` : 'ok: every variable the deployments set is read by its component');
  process.exit(problems.length ? 1 : 0);
}
