#!/usr/bin/env node
// Starts the lab, seeds it and proves it works.
//
//   node localdev/run.mjs          start (or update) the lab, seed it, run the smoke
//   node localdev/run.mjs --down   stop and remove the containers; the database volume is kept
//
// The key material is created on first use and kept from then on (.authlab/, .authlab-identity/):
// an agent installed from the lab MSI depends on it. The images come from `node localdev/build.mjs`.

import { spawnSync } from 'node:child_process';
import { mkdirSync } from 'node:fs';

import { ensureLabIdentity, readLabIdentity } from './identity/identity.mjs';
import {
  COMPOSE_FILE, IDENTITY_DIR, IMAGES, MSI_DIR, PKI_DIR, ROOT, TENANTS, TENANT_IDS,
  composeEnvironment, imageTag, labAddresses, pkiState, readPKI, smokeArgs, writePKI,
} from './lab.mjs';
import { runSeed } from './seed.mjs';

function docker(args, { env = process.env, input, inherit = false } = {}) {
  const r = spawnSync('docker', args, { cwd: ROOT, env, input, encoding: 'utf8', stdio: inherit ? ['pipe', 'inherit', 'inherit'] : 'pipe', maxBuffer: 64 << 20 });
  return { code: r.error ? -1 : r.status, out: `${r.stdout ?? ''}${r.stderr ?? ''}`.trim(), stdout: r.stdout ?? '' };
}

function fail(message) {
  console.error(`lab: ${message}`);
  process.exit(1);
}

const compose = (args, opts) => docker(['compose', '-f', COMPOSE_FILE, ...args], opts);

if (process.argv.includes('--down')) {
  const r = compose(['down'], { inherit: true });
  process.exit(r.code === 0 ? 0 : 1);
}

if (docker(['version', '--format', '{{.Server.Version}}']).code !== 0) fail('Docker is not running.');
const missing = IMAGES.filter((i) => docker(['image', 'inspect', '--format', '{{.Id}}', imageTag(i.name)]).code !== 0);
if (missing.length) fail(`missing image(s) ${missing.map((i) => imageTag(i.name)).join(', ')}: run node localdev/build.mjs`);

// 1. The key material: the secrets, then the PKI, each created only if absent.
const created = ensureLabIdentity(IDENTITY_DIR, TENANT_IDS);
if (created.length) console.log(`lab: created ${created.join(', ')} in localdev/.authlab-identity`);
switch (pkiState(PKI_DIR)) {
  case 'complete':
    break;
  case 'missing': {
    const r = docker(['run', '--rm', imageTag('authlab'), 'pki']);
    if (r.code !== 0) fail(`authlab pki failed:\n${r.out}`);
    mkdirSync(PKI_DIR, { recursive: true });
    writePKI(PKI_DIR, r.stdout);
    console.log('lab: created the lab CA and the edge certificate in localdev/.authlab');
    break;
  }
  default:
    fail('localdev/.authlab holds some of dev-ca.crt, dev-ca.key, edge-server.crt and edge-server.key but not all; restore the missing ones');
}
const identity = readLabIdentity(IDENTITY_DIR, TENANT_IDS);
const env = { ...process.env, ...composeEnvironment(readPKI(PKI_DIR), identity) };
mkdirSync(MSI_DIR, { recursive: true });

// 2. Up. One-shot setup and migration run first; everything else waits for them.
console.log('lab: starting …');
let r = compose(['up', '-d', '--wait'], { env });
if (r.code !== 0) fail(`docker compose up failed:\n${r.out}`);
r = compose(['config', '--format', 'json'], { env });
if (r.code !== 0) fail(`docker compose config failed:\n${r.out}`);
const addresses = labAddresses(JSON.parse(r.stdout));

// 3. The fixtures.
r = runSeed(identity, env);
if (r.status !== 0) fail(`the seed failed:\n${`${r.stdout ?? ''}${r.stderr ?? ''}`.trim()}`);
console.log(`lab: seeded ${Object.values(TENANTS).map((t) => `${t.name} (${t.domain})`).join(', ')}`);

// 4. The smoke, from inside the lab network.
console.log('lab: smoke\n');
r = docker(smokeArgs(addresses, identity), {
  env: { ...env, LAB_DEPLOYMENT_KEY: identity.deploymentKeys.sample },
  inherit: true,
});
const passed = r.code === 0;

console.log(`
lab: ${passed ? 'all checks passed' : 'the smoke FAILED (above)'}
  dashboard   ${addresses.dashboard}   sign in as analyst@${TENANTS.lab.domain}, admin@${TENANTS.lab.domain}, … or …@${TENANTS.sample.domain}
  devices     ${addresses.deviceEndpoint}   (node localdev/lab-msi.mjs builds the lab MSI)
  postgres    ${addresses.postgres}   user postgres, password sac-lab-only, database shadow
  logs        docker compose -f localdev/compose.yaml logs -f
  stop        node localdev/run.mjs --down`);
process.exit(passed ? 0 : 1);
