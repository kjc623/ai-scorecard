#!/usr/bin/env node
// One vocabulary, five artifacts, one check.
//
// The deployment (infra/main.bicep) and the binaries declare the same environment vocabulary in
// different files. Nothing but a test keeps them equal, and this seam was found rotted twice by hand
// before this existed — once as SAC_PG_HOST and friends being passed to a process that read no
// environment at all, and once as the two services reading CONTENT_VAULT_* while the deployment passed
// SAC_*.
//
// The sibling Go tests (services/*/cmd/*/infra_agreement_test.go) assert the same property from inside
// each module, where the acceptance gate runs them on every commit. This script is the wider view: it
// also covers the Dockerfiles and the lab compose file, and it reports every app in the Bicep that has
// no binary yet, so "the deployment declares more than the repository serves" stays visible rather
// than becoming a surprise at deploy time.
//
// Usage: node lab/tools/check-config-agreement.mjs

import { readFileSync, readdirSync, statSync, existsSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..');
const SAC = /\bSAC_[A-Z0-9_]+\b/g;

/** The services that have a binary in this repository, and where their configuration lives. */
const SERVICES = [
  { app: 'ingest-api', cmdDir: 'services/ingest-api/cmd/ingest-api', dockerfile: 'services/ingest-api/Dockerfile', composeService: 'ingest-api' },
  { app: 'content-vault', cmdDir: 'services/content-vault/cmd/content-vault', dockerfile: 'services/content-vault/Dockerfile', composeService: 'content-vault' },
];

/**
 * Names a binary reads that the deployment legitimately does not pass.
 *
 * Two kinds, and they are different things:
 *   image — the image sets it, because Container Apps configures by environment and this deployment's
 *           Bicep has no command/args parameter to pass a value in;
 *   gap   — the name belongs to the deployment and infra/main.bicep does not pass it yet. Listed so it
 *           is visible, never so it is forgotten.
 */
const EXTENSIONS = {
  'ingest-api': {
    SAC_HTTP_ADDR: 'image: a container binds 0.0.0.0, and the Bicep passes no command/args',
    SAC_STORE: 'image: the store mode this build can actually serve (memory)',
    SAC_SCHEMA: 'image: the contract schema at a path a container can read',
    SAC_ROUTES_FILE: 'image: ref.route_fidelity shipped as data (§4.4), never compiled in',
    SAC_REGION: 'gap: §12 region pinning is inert until the deployment passes the region',
  },
  'content-vault': {
    SAC_HTTP_ADDR: 'image: a container binds 0.0.0.0',
    SAC_STORE: 'image: the store mode this build can actually serve (memory)',
    SAC_KEY_BACKEND: 'image: local, the only backend this build implements',
    SAC_ALLOW_NON_LOOPBACK: 'image: the image-level spelling of the internal-ingress acknowledgement',
  },
};

function read(rel) {
  return readFileSync(join(ROOT, rel), 'utf8');
}

function find(rel, re) {
  return new Set((read(rel).match(re) ?? []));
}

/** Every SAC_* name a Go package declares, from source rather than from a hand-written list. */
function namesInGoDir(relDir) {
  const out = new Set();
  const goEnv = /Env\w*\s*=\s*"(SAC_[A-Z0-9_]+)"/g;
  for (const entry of readdirSync(join(ROOT, relDir))) {
    if (!entry.endsWith('.go') || entry.endsWith('_test.go')) continue;
    const text = read(join(relDir, entry));
    for (const m of text.matchAll(goEnv)) out.add(m[1]);
  }
  return out;
}

/** SAC_* names an image sets via ENV, read line-wise so multi-line `ENV … \` continuations and
 *  comments behave: a name mentioned in a comment is documentation, not a setting. */
function namesInDockerfileEnv(rel) {
  const out = new Set();
  let inEnv = false;
  for (const raw of read(rel).split('\n')) {
    const line = raw.trim();
    if (line.startsWith('#')) continue;
    if (/^ENV\b/.test(line)) inEnv = true;
    else if (inEnv && !line.endsWith('\\') && !/^[A-Za-z_][A-Za-z0-9_]*=/.test(line)) inEnv = false;
    if (!inEnv) continue;
    for (const m of line.matchAll(SAC)) out.add(m[0]);
    if (!line.endsWith('\\')) inEnv = false;
  }
  return out;
}

/** SAC_* names the lab compose file sets, per compose service. */
function namesInCompose(rel) {
  const text = read(rel);
  const out = new Map();
  const serviceRE = /^  ([a-z][a-z0-9-]*):$/gm;
  const marks = [...text.matchAll(serviceRE)].map((m) => ({ name: m[1], at: m.index }));
  marks.forEach((mark, i) => {
    const end = i + 1 < marks.length ? marks[i + 1].at : text.length;
    const body = text.slice(mark.at, end);
    const names = new Set((body.match(SAC) ?? []));
    if (names.size) out.set(mark.name, names);
  });
  return out;
}

/** The SAC_* names infra/main.bicep passes to each container app, keyed by appName. */
function namesInBicep(rel) {
  const text = read(rel);
  const appRE = /appName:\s*'([^']+)'/g;
  const marks = [...text.matchAll(appRE)].map((m) => ({ name: m[1], at: m.index }));
  const out = new Map();
  marks.forEach((mark, i) => {
    const end = i + 1 < marks.length ? marks[i + 1].at : text.length;
    const names = new Set(text.slice(mark.at, end).match(SAC) ?? []);
    if (names.size) out.set(mark.name, names);
  });
  return out;
}

const problems = [];
const notes = [];

const bicep = namesInBicep('infra/main.bicep');
if (bicep.size === 0) problems.push('infra/main.bicep: no container app env names found; the parser needs revisiting');

const compose = existsSync(join(ROOT, 'lab/docker-compose.yml')) ? namesInCompose('lab/docker-compose.yml') : new Map();

console.log('configuration vocabulary across artifacts\n');
console.log(`${'app'.padEnd(15)} ${'bicep'.padEnd(7)} ${'binary'.padEnd(7)} ${'image'.padEnd(7)} lab`);
console.log('-'.repeat(52));

for (const svc of SERVICES) {
  const bicepNames = bicep.get(svc.app) ?? new Set();
  const binaryNames = namesInGoDir(svc.cmdDir);
  const imageNames = namesInDockerfileEnv(svc.dockerfile);
  const labNames = compose.get(svc.composeService) ?? new Set();

  console.log(
    `${svc.app.padEnd(15)} ${String(bicepNames.size).padEnd(7)} ${String(binaryNames.size).padEnd(7)} ` +
      `${String(imageNames.size).padEnd(7)} ${labNames.size}`,
  );

  if (binaryNames.size === 0) problems.push(`${svc.app}: no SAC_* names found in ${svc.cmdDir}; the parser needs revisiting`);

  // A. Everything the deployment passes must be read by the binary. This is the F3 defect.
  for (const name of bicepNames) {
    if (!binaryNames.has(name)) {
      problems.push(`${svc.app}: infra/main.bicep passes ${name} and the binary never reads it`);
    }
  }

  // B. Everything the binary reads must be passed, an image default, or a documented gap.
  const ext = EXTENSIONS[svc.app] ?? {};
  for (const name of binaryNames) {
    if (bicepNames.has(name) || imageNames.has(name)) continue;
    if (!(name in ext)) {
      problems.push(`${svc.app}: the binary reads ${name}, which neither the deployment, the image, nor the extension list accounts for`);
      continue;
    }
    if (ext[name].startsWith('gap')) notes.push(`${svc.app}: ${name} — ${ext[name]}`);
  }

  // C. An image setting nothing reads is the same defect one layer up.
  for (const name of imageNames) {
    if (!binaryNames.has(name)) problems.push(`${svc.app}: ${svc.dockerfile} sets ${name} and the binary never reads it`);
  }

  // D. Same for the lab compose file.
  for (const name of labNames) {
    if (!binaryNames.has(name)) problems.push(`${svc.app}: lab/docker-compose.yml sets ${name} and the binary never reads it`);
  }

  // E. An extension that has since been added to the Bicep is stale, not wrong.
  for (const name of Object.keys(ext)) {
    if (bicepNames.has(name)) notes.push(`${svc.app}: ${name} is now passed by the Bicep; the extension note is stale`);
  }
}

// Apps the deployment declares that no binary in this repository serves. Not a failure here — those
// are other components' tasks — but the list belongs in front of a reader.
const served = new Set(SERVICES.map((s) => s.app));
const unserved = [...bicep.keys()].filter((app) => !served.has(app));
if (unserved.length) {
  notes.push(`declared in infra/main.bicep with no binary in this repository yet: ${unserved.join(', ')}`);
}

console.log('');
for (const note of notes) console.log(`note: ${note}`);
if (problems.length) {
  console.log('');
  for (const p of problems) console.log(`FAIL: ${p}`);
  console.log(`\n${problems.length} problem(s). The deployment and the binaries disagree about the vocabulary.`);
  process.exit(1);
}
console.log('\nok: every name the deployment passes is read by the service it is passed to, every name a');
console.log('    binary reads is accounted for, and the image and lab settings are read too.');
