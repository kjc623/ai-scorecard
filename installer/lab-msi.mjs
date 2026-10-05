#!/usr/bin/env node
// installer/lab-msi.mjs - build the Windows MSI for THIS host against the local Docker auth lab.
//
//   node installer/lab-msi.mjs                       # M3, intercepting api.anthropic.com
//   node installer/lab-msi.mjs --mode m1 --hosts api.anthropic.com,api.openai.com
//
// The result is installer/dist/ShadowAICapture.msi with installer/dist/ShadowAICapture.tenant.env
// beside it. Double-click the MSI: it installs the agent as the ShadowAICapture service, copies the
// tenant file in, starts the service, and the device enrols, intercepts and drains to the lab with
// nothing typed. Uninstall it from Apps & features.
//
// This is the product's delivery with one substitution. The MSI is the generic one release-msi.mjs
// builds (same stage, same vendor-wide capture-core.env, same classifier release); the tenant file
// beside it is what control-api's Settings -> Deployment download would put there. A lab host has no
// control-api package yet that carries everything this lab uses, so this script plays that part:
//
//   ShadowAICapture.tenant.env (beside the MSI)  the lab profile: tenant, lab edge, pinned CA, a fresh
//                                                single-use token, and the lab overrides below
//   profile\bundle.json      the signed policy bundle (collection mode, interception scope, cli.shim)
//   profile\ca.pem|ca.key    the per-device interception CA (kept across rebuilds)
//   profile\edge-ca.crt      the lab edge's CA
//
// The profile\ files are added to the MSI (ProfileExtras); the generic MSI has none. The bundle is
// signed under the vendor anchor when this checkout holds the lab's vendor key
// (localdev/.authlab-identity, which control-api signs with), so the tenant file only names it;
// otherwise the tenant file also pins the key the bundle was signed with, which wins.
//
// It expects the AUTH lab to be up (the device enrols):  docker compose -f localdev/authlab.compose.yaml up -d

import { spawnSync } from 'node:child_process';
import { copyFileSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { hostname } from 'node:os';
import { join, win32 } from 'node:path';

import { LAYOUT, PRODUCT, TENANT_PACKAGE } from './manifest.mjs';
import { BuildError, ROOT, buildMsi, rel, resolvePolicyAnchor, run, stageGeneric } from './windows/msi.mjs';

const ARGS = process.argv.slice(2);
function valueOf(flag, fallback) {
  const i = ARGS.indexOf(flag);
  return i === -1 ? fallback : ARGS[i + 1];
}
function die(msg) {
  console.error(`lab-msi: ${msg}`);
  process.exit(1);
}
function step(msg) {
  console.log(`lab-msi: ${msg}`);
}
function must(res, what) {
  if (res.code !== 0) die(`${what} failed:\n${res.out.trim()}`);
  return res;
}

if (process.platform !== 'win32') die('this builds and targets a Windows host; use installer/dev.mjs elsewhere');

const TENANT = valueOf('--tenant', '11111111-1111-1111-1111-111111111111');
const MODE = valueOf('--mode', 'm3');
const HOSTS = valueOf('--hosts', 'api.anthropic.com');
const LISTEN = valueOf('--listen', '127.0.0.1:8843');
const MDM_ID = valueOf('--mdm-id', `sac-lab-${hostname().toLowerCase()}`);
const TOKEN_HOURS = valueOf('--token-hours', '720');

const COMPOSE = join(ROOT, 'localdev', 'authlab.compose.yaml');
const EDGE_CA = join(ROOT, 'localdev', '.authlab', 'dev-ca.crt');
const WORK = join(ROOT, 'installer', '.lab', 'msi');
const PROFILE = join(WORK, 'profile');
const KEYS = join(WORK, 'keys');
const TOOLS = join(WORK, 'tools');
const INSTALLED = LAYOUT.windows.profiledir;
const OUT = join('installer', 'dist');

const GO_ENV = {
  ...process.env,
  GOCACHE: join(ROOT, '.tools', 'gocache'),
  GOPROXY: 'off',
  GOFLAGS: '-mod=mod',
  GOTOOLCHAIN: 'local',
  CGO_ENABLED: '0',
};

// ---------------------------------------------------------------------------------------------
// 1. The lab the device will enrol against.

const running = run('docker', ['compose', '-f', COMPOSE, 'ps', '--status', 'running', '-q']);
if (running.code !== 0 || running.out.trim() === '') die('the auth lab is not running. Start it first:\n  docker compose -f localdev/authlab.compose.yaml up -d');
const port = /:(\d+)\s*$/.exec(run('docker', ['compose', '-f', COMPOSE, 'port', 'edge', '8443']).out.trim());
if (!port) die('cannot find the edge’s published port (docker compose port edge 8443)');
const EDGE = `https://127.0.0.1:${port[1]}`;
if (!existsSync(EDGE_CA)) die(`lab CA not found at ${EDGE_CA}`);
step(`lab edge ${EDGE}`);

// ---------------------------------------------------------------------------------------------
// 2. The generic payload, exactly as release-msi.mjs stages it.

let anchor;
try {
  anchor = resolvePolicyAnchor({});
  step(`policy trust anchor ${anchor.policyKeyId} from ${anchor.source}`);
  stageGeneric({ version: PRODUCT.version, anchor, log: step });
} catch (err) {
  if (err instanceof BuildError) die(err.message);
  throw err;
}
mkdirSync(TOOLS, { recursive: true });
mkdirSync(KEYS, { recursive: true });
const SAC_BUNDLE = join(TOOLS, 'sac-bundle.exe');
must(run('go', ['build', '-o', SAC_BUNDLE, './cmd/sac-bundle'], { cwd: join(ROOT, 'endpoint', 'capture-core'), env: GO_ENV }), 'go build sac-bundle');

// ---------------------------------------------------------------------------------------------
// 3. The lab's profile files. The device CA lives in keys/ so a rebuild re-signs the bundle around
//    the same root instead of minting (and trusting) a new one per build.

rmSync(PROFILE, { recursive: true, force: true });
mkdirSync(PROFILE, { recursive: true });

const caCert = join(KEYS, 'ca.pem');
const caKey = join(KEYS, 'ca.key');
const firstHost = HOSTS.split(',')[0].trim();
const bundleArgs = [
  '--out', PROFILE,
  '--tenant-default', MODE,
  '--hosts', HOSTS,
  '--listen', LISTEN,
  '--canary', `${firstHost}:443`,
  '--no-proxy', 'localhost,127.0.0.1,::1',
  '--shim-managed-dir', win32.join(LAYOUT.windows.configdir, 'shim'),
  '--version', String(Math.floor(Date.now() / 1000)),
];
// Signed with the anchor's own key when this checkout holds it (the lab's vendor key), so the bundle
// verifies under the vendor file's pin exactly as one from GET /v1/policy would. Otherwise sac-bundle
// mints a key and the tenant file pins that instead.
const signsWithAnchor = Boolean(anchor.privateHexFile);
if (signsWithAnchor) bundleArgs.push('--policy-priv', readFileSync(anchor.privateHexFile, 'utf8').trim(), '--policy-key-id', anchor.policyKeyId);
if (existsSync(caCert) && existsSync(caKey)) bundleArgs.push('--ca-cert', caCert, '--ca-key', caKey);
must(run(SAC_BUNDLE, bundleArgs), 'sac-bundle');
if (existsSync(caCert) && existsSync(caKey)) {
  copyFileSync(caCert, join(PROFILE, 'ca.pem'));
  copyFileSync(caKey, join(PROFILE, 'ca.key'));
} else {
  copyFileSync(join(PROFILE, 'ca.pem'), caCert);
  copyFileSync(join(PROFILE, 'ca.key'), caKey);
}
const mintedKey = join(PROFILE, 'policy-key.pub');
const policyKey = existsSync(mintedKey) ? readFileSync(mintedKey, 'utf8').trim() : '';
rmSync(mintedKey, { force: true }); // the tenant file pins it; the device needs no second copy
step(`policy bundle signed: ${MODE}, intercepting ${HOSTS}, under ${signsWithAnchor ? 'the vendor anchor' : 'a key the tenant file pins'}`);

copyFileSync(EDGE_CA, join(PROFILE, 'edge-ca.crt'));

// ---------------------------------------------------------------------------------------------
// 4. The tenant file. Paths are where the MSI installs the files above.

// The tenant's ceiling follows the bundle's mode: a device collecting at a mode its tenant does not
// permit would have every content grant denied.
const seeded = spawnSync('node', [join(ROOT, 'installer', 'seed.mjs'), '--tenant', TENANT, '--hours', TOKEN_HOURS, '--ceiling', MODE], { encoding: 'utf8', cwd: ROOT });
const token = seeded.status === 0 ? (seeded.stdout ?? '').trim() : '';
if (!token) die(`could not seed an enrolment token:\n${seeded.stderr ?? ''}`);
step(`enrolment token seeded (single use, valid ${TOKEN_HOURS}h)`);

const profile = {
  // What a tenant package carries ...
  SAC_TENANT_ID: TENANT,
  SAC_DEVICE_ENDPOINT: EDGE,
  SAC_AUTH_MODE: 'x509',
  // ... and what this lab adds: a single-use token in place of a deployment key, a local signed
  // bundle in place of GET /v1/policy, its device CA, and the edge's private CA.
  SAC_ENROLMENT_TOKEN: token,
  SAC_DEVICE_ID: '22222222-2222-4222-8222-222222222222', // a local/no-drain fallback only
  SAC_USER_REF: 'lab-user',
  SAC_POPULATION: 'lab',
  SAC_BUNDLE: win32.join(INSTALLED, 'bundle.json'),
  ...(signsWithAnchor ? {} : { SAC_POLICY_KEY: policyKey, SAC_POLICY_KEY_ID: 'policy-key-1' }),
  SAC_PROXY_TLS: 'true',
  SAC_PROXY_TLS_LISTEN: LISTEN,
  SAC_PROXY_TLS_CANARY: `${firstHost}:443`,
  SAC_PROXY_LOOPBACK: 'false',
  SAC_PROC_DETECT: 'false',
  SAC_TRUST_INSTALL: 'true',
  SAC_TRUST_REMOVE_ON_STOP: 'true',
  SAC_CA_CERT: win32.join(INSTALLED, 'ca.pem'),
  SAC_CA_KEY: win32.join(INSTALLED, 'ca.key'),
  SAC_CLI_SHIM: 'true',
  SAC_SHIM_DIR: win32.join(LAYOUT.windows.configdir, 'shim'),
  SAC_CA_FILE: win32.join(INSTALLED, 'edge-ca.crt'),
  SAC_MDM_ID: MDM_ID,
  SAC_BACKOFF_BASE: '250ms',
  SAC_BACKOFF_CAP: '5s',
};
const tenantFile = join(WORK, TENANT_PACKAGE.fileName);
writeFileSync(
  tenantFile,
  ['# installer/lab-msi.mjs - the lab tenant file for this host. Regenerated every build.', ...Object.entries(profile).map(([k, v]) => `${k}=${v}`), ''].join('\r\n'),
  { mode: 0o600 },
);

// ---------------------------------------------------------------------------------------------
// 5. The MSI, with the tenant file beside it.

step('building the MSI …');
let msi;
try {
  msi = buildMsi({ outDir: OUT, profileDir: rel(PROFILE), tenantEnv: rel(tenantFile), freshEnrolment: true });
} catch (err) {
  if (err instanceof BuildError) die(err.message);
  throw err;
}

console.log(`
lab-msi: built ${msi}
         with   ${join(ROOT, OUT, TENANT_PACKAGE.fileName)} beside it

  install    double-click the MSI, or:  msiexec /i "${msi}"
             It copies the tenant file from its own folder, so keep the two together.
  uninstall  Apps & features > Shadow AI Capture, or:  msiexec /x "${msi}"

  The service starts on install and at every boot. Open a NEW terminal afterwards: the proxy and
  CA environment the agent sets reaches processes started after it.

  health     ${win32.join(LAYOUT.windows.statedir, 'health.jsonl')}
  log        ${win32.join(LAYOUT.windows.statedir, 'service.log')}
  rows       docker exec sac-authlab-postgres-1 psql -U postgres -d shadow -x -c "select * from ingest.observation order by received_at desc limit 5"
  dashboard  http://127.0.0.1:8787/explore.html?transport=live
`);
