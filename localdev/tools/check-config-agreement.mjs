#!/usr/bin/env node
// One vocabulary, five artifacts, one check.
//
// The deployment (azure/main.bicep) and the binaries declare the same environment vocabulary in
// different files. Nothing but a test keeps them equal, and this seam was found rotted twice by hand
// before this existed — once as SAC_PG_HOST and friends being passed to a process that read no
// environment at all, and once as the two services reading CONTENT_VAULT_* while the deployment passed
// SAC_*.
//
// The sibling Go tests ({ingestion,vault}/*/cmd/*/infra_agreement_test.go) assert the same property from inside
// each module, where the acceptance gate runs them on every commit. This script is the wider view: it
// also covers the Dockerfiles and the lab compose file, and it reports every app in the Bicep that has
// no binary yet, so "the deployment declares more than the repository serves" stays visible rather
// than becoming a surprise at deploy time.
//
// Usage: node localdev/tools/check-config-agreement.mjs

import { readFileSync, readdirSync, statSync, existsSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..');
const SAC = /\bSAC_[A-Z0-9_]+\b/g;

/** The services that have a binary in this repository, and where their configuration lives. */
const SERVICES = [
  { app: 'ingest-api', cmdDir: 'ingestion/ingest-api/cmd/ingest-api', dockerfile: 'ingestion/ingest-api/Dockerfile', composeService: 'ingest-api' },
  { app: 'content-vault', cmdDir: 'vault/content-vault/cmd/content-vault', dockerfile: 'vault/content-vault/Dockerfile', composeService: 'content-vault' },
  // control-api is the device-facing control plane ADR 0020 adds (POST /v1/enrol, POST /v1/token).
  // Listing it is what makes its vocabulary answerable to azure/main.bicep and to the auth lab's
  // compose file; its infra_agreement_test.go asserts the same property from inside the module.
  { app: 'control-api', cmdDir: 'control/control-api/cmd/control-api', dockerfile: 'control/control-api/Dockerfile', composeService: 'control-api' },
  // query-api is Node, so it has no cmd/ directory: the environment it reads lives in one module.
  // Listing it is what makes the deployment's names and the service's names answerable to each
  // other. Until it was listed, azure/main.bicep passed SAC_PG_HOST and SAC_CONTENT_VAULT_URL to a
  // container app that could not start, and this checker reported that as "declared with no binary
  // in this repository yet" — a note, not the defect it was.
  {
    app: 'query-api',
    cmdDir: 'query/query-api/src/http',
    sources: ['query/query-api/src/http/config.js'],
    dockerfile: 'query/query-api/Dockerfile',
    composeService: 'query-api',
  },
];

/**
 * Names a binary reads that the deployment legitimately does not pass.
 *
 * Two kinds, and they are different things:
 *   image — the image sets it, because Container Apps configures by environment and this deployment's
 *           Bicep has no command/args parameter to pass a value in;
 *   gap   — the name belongs to the deployment and azure/main.bicep does not pass it yet. Listed so it
 *           is visible, never so it is forgotten.
 */
const EXTENSIONS = {
  'ingest-api': {
    SAC_HTTP_ADDR: 'image: a container binds 0.0.0.0, and the Bicep passes no command/args',
    SAC_STORE: 'image: the store mode this build can actually serve (memory)',
    SAC_SCHEMA: 'image: the contract schema at a path a container can read',
    SAC_ROUTES_FILE: 'image: ref.route_fidelity shipped as data (§4.4), never compiled in',
    SAC_REGION: 'gap: §12 region pinning is inert until the deployment passes the region',
    SAC_TLS_CERT_PEM: 'gap (F5): the server certificate for the module keyVaultEnv to inject; every app in azure/main.bicep has keyVaultEnv: [] today',
    SAC_TLS_KEY_PEM: 'gap (F5): the server private key — see SAC_TLS_CERT_PEM',
    SAC_TLS_CLIENT_CA_PEM: 'gap (F5): the CA that must have signed the device certificates — see SAC_TLS_CERT_PEM',
    // The ADR 0020 device-authentication vocabulary. The binary reads all of it; a deployment that
    // serves a production mode must pass it, and azure/main.bicep passes none of it yet. The auth
    // lab passes them, but rule B deliberately does not count the lab as the deployment, so they are
    // gaps here and not silently satisfied by localdev.
    SAC_AUTH_MODES: 'gap: the production authenticator modes to serve (x509,dpop). Unset infers from the material present; a deployment should name them so a mode cannot be enabled by accident',
    SAC_TLS_CLIENT_CERT_HEADER: 'gap: the edge-forwarded certificate header. A deployment behind Application Gateway must set it to X-Client-Cert, or the forwarded x509 path is disabled',
    SAC_DPOP_TOKEN_PUBLIC_PEM: 'gap: the access-token verification key (PEM text or a readable PEM file). The deployment passes no token material yet',
    SAC_DPOP_ISSUER: 'gap: the DPoP access token `iss` the origin requires. The deployment passes none yet',
    SAC_DPOP_AUDIENCE: 'gap: the DPoP access token `aud` the origin requires. The deployment passes none yet',
  },
  'content-vault': {
    SAC_HTTP_ADDR: 'image: a container binds 0.0.0.0',
    SAC_STORE: 'image: the store mode this build can actually serve (memory)',
    SAC_KEY_BACKEND: 'image: local, the only backend this build implements',
    SAC_ALLOW_NON_LOOPBACK: 'image: the image-level spelling of the internal-ingress acknowledgement',
    SAC_BLOB_READ_CREDENTIAL: 'lab only: the shared bearer the storage stand-in checks. A deployment uses SAC_BLOB_IDENTITY=managed instead',
    SAC_RETRIEVAL_URL_BASE: 'gap: the origin a browser reaches a minted retrieval URL on; a deployment behind an ingress sets it, the lab resolves a path against the page\'s own origin',
  },
  'query-api': {
    SAC_HTTP_ADDR: 'image: a container binds 0.0.0.0; the deployment has no command/args to pass one in',
    SAC_PG_PORT: 'image: 5432 unless a deployment says otherwise; a Flexible Server always listens there',
    SAC_PG_USER: 'image: the role is the deployment\'s managed identity, mapped to sac_query. A lab overrides it',
    SAC_PG_PASSWORD: 'lab only: a deployed PostgreSQL authenticates the managed identity. Listed rather than hidden so an operator can see the lab is not the deployment',
    SAC_PG_SSLMODE: 'lab only: the lab container has no CA chain for its own server, so it says `disable` out loud. A deployment leaves this unset and gets `require`',
    SAC_DEV_TRUST_PRINCIPAL: 'lab only: the development stand-in for the authenticated session, which is NOT BUILT YET. It is the same escape hatch ingest-api uses, off unless set',
    SAC_MAX_CONCURRENCY: 'image: 8 concurrent statements per tenant is §12.3\'s number, and a container platform has no field for it. A deployment that wants a different ceiling sets it',
    SAC_MAX_QUEUE: 'image: the bounded queue of 32 from §12.3, for the same reason as the concurrency ceiling',
    /**
     * These two ARE passed by the deployment — to the migration job (azure/main.bicep ~L547, its
     * binary takes `args: ['migrate']`), where §3.4's "a DDL lock must not stall ingest" is the
     * reason. They are listed for this service because query-api reads the same NAMES for §12.3's
     * server-side statement budget: one dataset-wide vocabulary, honoured by both.
     *
     * What is a real gap, and is not absorbed by this note: the query-api container app does not
     * pass them, so in Azure its statement budget is the default rather than the deployment's. That
     * belongs in azure/main.bicep's env list for queryApp, not here.
     */
    SAC_PG_STATEMENT_TIMEOUT_MS: 'shared with the migration job, which also reads it: §12.3\'s server-side statement budget',
    SAC_PG_LOCK_TIMEOUT_MS: 'shared with the migration job, which also reads it: a blocked read must be distinguishable from an expensive one',
    // Pre-existing, unrelated to ADR 0020: query-api reads this name and nothing passed it, so the
    // checker was red before the auth lab existed. It is an image-level default, like the two
    // ceilings above: a container platform has no field to pass a value in, and a deployment that
    // wants a different global pool sets it.
    SAC_MAX_CONNECTIONS: 'image: 40 is the global statement-pool cap from §12.3 (master doc Q12); a deployment that wants a different ceiling sets it',
  },
  'control-api': {
    SAC_HTTP_ADDR: 'image: a container binds 0.0.0.0, and the Bicep passes no command/args',
    SAC_STORE: 'image: the store mode this build can actually serve (memory)',
    SAC_CREDENTIAL_TTL: 'image: the default life of an issued device credential',
    SAC_ENROLMENT_TOKEN_TTL: 'image: the default life an operator-minted enrolment token would carry; the request path only verifies tokens the MDM profile delivered',
    SAC_REGION: 'gap: §12 region pinning is inert until the deployment passes the region',
    SAC_CA_CERT_PEM: 'gap (ADR 0020 decision 3): the LocalCA certificate as PEM text, for the module keyVaultEnv to inject; every app in azure/main.bicep has keyVaultEnv: [] today',
    SAC_CA_KEY_PEM: 'gap (ADR 0020 decision 3): the LocalCA private key — see SAC_CA_CERT_PEM',
    SAC_TOKEN_ISSUER: 'gap (§5.2): the `iss` of issued access tokens; the deployment passes none',
    SAC_TOKEN_AUDIENCE: 'gap (§5.2): the `aud` of issued access tokens — see SAC_TOKEN_ISSUER',
    SAC_DPOP_TOKEN_KEY_PEM: 'gap: the access-token signing key. The binary refuses to start without it; no deployment injects it yet',
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

/**
 * Every SAC_* name a JavaScript module names, for a service that is not Go.
 *
 * The pattern is deliberately the same shape as the Go one — a name bound to a string constant —
 * rather than "any SAC_ mention": a name appearing only in a comment is documentation, and treating
 * it as read would make the checker agree with prose instead of with code.
 */
function namesInJsFiles(files) {
  const out = new Set();
  const jsEnv = /:\s*'(SAC_[A-Z0-9_]+)'/g;
  for (const rel of files) {
    const text = read(rel);
    for (const m of text.matchAll(jsEnv)) out.add(m[1]);
  }
  return out;
}

/** The names a service reads, whichever language it is written in. */
function namesInService(svc) {
  if (svc.sources) return namesInJsFiles(svc.sources);
  return namesInGoDir(svc.cmdDir);
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

/**
 * SAC_* names the lab compose files set, per compose service, merged across files.
 *
 * There are two lab compose files: the default memory lab (docker-compose.yml) and the opt-in auth
 * lab (authlab.compose.yaml). Both name their services after the app they configure — there is an
 * `ingest-api` in each — so a service's names are the union of its blocks. A service that sets no
 * SAC_* name (postgres, schema, edge, the seed) is simply absent, and the per-app lookup then finds
 * nothing, which is correct: those blocks configure no service vocabulary.
 */
function namesInComposeFiles(files) {
  const out = new Map();
  for (const rel of files) {
    if (!existsSync(join(ROOT, rel))) continue;
    for (const [name, names] of namesInCompose(rel)) {
      const merged = out.get(name) ?? new Set();
      for (const n of names) merged.add(n);
      out.set(name, merged);
    }
  }
  return out;
}

/** SAC_* names one lab compose file sets, per compose service. */
function namesInCompose(rel) {
  const text = read(rel);
  const out = new Map();
  const serviceRE = /^  ([a-z][a-z0-9-]*):$/gm;
  const marks = [...text.matchAll(serviceRE)].map((m) => ({ name: m[1], at: m.index }));
  marks.forEach((mark, i) => {
    const end = i + 1 < marks.length ? marks[i + 1].at : text.length;
    // Comment lines are documentation, not configuration. The control-api block explains its
    // neighbour's vocabulary in a comment; counting that prose as control-api's settings would
    // attribute ingest-api's names to the wrong service. The Dockerfile ENV parser above ignores
    // comments for the same reason.
    const body = text
      .slice(mark.at, end)
      .split('\n')
      .map((line) => (line.trimStart().startsWith('#') ? '' : line))
      .join('\n');
    const names = new Set((body.match(SAC) ?? []));
    if (names.size) out.set(mark.name, names);
  });
  return out;
}

/** The SAC_* names azure/main.bicep passes to each container app, keyed by appName. */
function namesInBicep(rel) {
  const text = read(rel);
  // Every app AND every job, because a job has a `jobName` and no `appName`, and splitting on
  // appName alone lets one container app's env block run straight into the next job's. That is not
  // hypothetical: query-api's block ended at the migration job, so the job's two timeout names were
  // attributed to query-api — which then looked like "the deployment already passes these", hiding
  // the real finding that the query-api app passes neither.
  const blockRE = /(?:appName|jobName):\s*'([^']+)'/g;
  const marks = [...text.matchAll(blockRE)].map((m) => ({ name: m[1], at: m.index }));
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

const bicep = namesInBicep('azure/main.bicep');
if (bicep.size === 0) problems.push('azure/main.bicep: no container app env names found; the parser needs revisiting');

const compose = namesInComposeFiles([
  'localdev/docker-compose.yml',
  'localdev/authlab.compose.yaml',
]);

console.log('configuration vocabulary across artifacts\n');
console.log(`${'app'.padEnd(15)} ${'bicep'.padEnd(7)} ${'binary'.padEnd(7)} ${'image'.padEnd(7)} lab`);
console.log('-'.repeat(52));

for (const svc of SERVICES) {
  const bicepNames = bicep.get(svc.app) ?? new Set();
  const binaryNames = namesInService(svc);
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
      problems.push(`${svc.app}: azure/main.bicep passes ${name} and the binary never reads it`);
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
    if (!binaryNames.has(name)) problems.push(`${svc.app}: localdev/docker-compose.yml sets ${name} and the binary never reads it`);
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
  notes.push(`declared in azure/main.bicep with no binary in this repository yet: ${unserved.join(', ')}`);
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
