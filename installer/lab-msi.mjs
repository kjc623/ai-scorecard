#!/usr/bin/env node
// installer/lab-msi.mjs - build the Windows MSI for THIS host against the local Docker auth lab.
//
//   node installer/lab-msi.mjs                       # M3, intercepting api.anthropic.com
//   node installer/lab-msi.mjs --mode m1 --hosts api.anthropic.com,api.openai.com
//
// The result is installer/dist/ShadowAICapture.msi. Double-click it (or `msiexec /i`): it installs
// the agent as the ShadowAICapture service, starts it, and the device enrols, intercepts and drains
// to the lab with nothing typed. Uninstall it from Apps & features.
//
// This is the product's delivery with one substitution. In production the artefact carries code and
// the customer's MDM delivers the enrolment profile (docs/05-platform-delivery.md §6.2); a lab host
// has no MDM, so this script plays that part and the MSI carries the profile it produced:
//
//   capture-core.env        the enrolment profile: lab edge, pinned CA, a fresh single-use token
//   profile\bundle.json     the signed policy bundle (collection mode, interception scope, cli.shim)
//   profile\ca.pem|ca.key   the per-device interception CA (kept across rebuilds)
//   profile\classifier\     the signed classifier release the agent's classifier-host child loads
//   profile\edge-ca.crt     the lab edge's CA
//
// It expects the AUTH lab to be up (the device enrols):  node localdev/run.mjs --auth

import { spawnSync } from 'node:child_process';
import { copyFileSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { hostname } from 'node:os';
import { dirname, join, resolve, win32 } from 'node:path';
import { fileURLToPath } from 'node:url';

import { LAYOUT } from './manifest.mjs';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
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

if (process.platform !== 'win32') die('this builds and targets a Windows host; use installer/dev.mjs elsewhere');

const TENANT = valueOf('--tenant', '11111111-1111-1111-1111-111111111111');
const MODE = valueOf('--mode', 'm3');
const HOSTS = valueOf('--hosts', 'api.anthropic.com');
const LISTEN = valueOf('--listen', '127.0.0.1:8843');
const MDM_ID = valueOf('--mdm-id', `sac-lab-${hostname().toLowerCase()}`);
const TOKEN_HOURS = valueOf('--token-hours', '720');

const COMPOSE = join(ROOT, 'localdev', 'authlab.compose.yaml');
const EDGE_CA = join(ROOT, 'localdev', '.authlab', 'dev-ca.crt');
const STAGE = join(ROOT, 'installer', '.stage', 'windows-amd64');
const WORK = join(ROOT, 'installer', '.lab', 'msi');
const PROFILE = join(WORK, 'profile');
const KEYS = join(WORK, 'keys');
const TOOLS = join(WORK, 'tools');
const INSTALLED = win32.join(LAYOUT.windows.configdir, 'profile');

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
if (running.code !== 0 || running.out.trim() === '') die('the auth lab is not running. Start it first:\n  node localdev/run.mjs --auth');
const port = /:(\d+)\s*$/.exec(run('docker', ['compose', '-f', COMPOSE, 'port', 'edge', '8443']).out.trim());
if (!port) die('cannot find the edge’s published port (docker compose port edge 8443)');
const EDGE = `https://127.0.0.1:${port[1]}`;
if (!existsSync(EDGE_CA)) die(`lab CA not found at ${EDGE_CA}; run: node localdev/run.mjs --auth`);
step(`lab edge ${EDGE}`);

// ---------------------------------------------------------------------------------------------
// 2. The payload, and the bundle generator (a build-side tool, never installed).

step('building the windows/amd64 payload …');
must(run('node', [join(ROOT, 'installer', 'render.mjs')]), 'render');
must(run('node', [join(ROOT, 'installer', 'build.mjs'), '--os', 'windows', '--arch', 'amd64']), 'build');
mkdirSync(TOOLS, { recursive: true });
mkdirSync(KEYS, { recursive: true });
const SAC_BUNDLE = join(TOOLS, 'sac-bundle.exe');
must(run('go', ['build', '-o', SAC_BUNDLE, './cmd/sac-bundle'], { cwd: join(ROOT, 'endpoint', 'capture-core'), env: GO_ENV }), 'go build sac-bundle');

// ---------------------------------------------------------------------------------------------
// 3. The profile directory. The device CA lives in keys/ so a rebuild re-signs the bundle around the
//    same root instead of minting (and trusting) a new one per build.

rmSync(PROFILE, { recursive: true, force: true });
mkdirSync(join(PROFILE, 'classifier'), { recursive: true });

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
if (existsSync(caCert) && existsSync(caKey)) bundleArgs.push('--ca-cert', caCert, '--ca-key', caKey);
must(run(SAC_BUNDLE, bundleArgs), 'sac-bundle');
if (existsSync(caCert) && existsSync(caKey)) {
  copyFileSync(caCert, join(PROFILE, 'ca.pem'));
  copyFileSync(caKey, join(PROFILE, 'ca.key'));
} else {
  copyFileSync(join(PROFILE, 'ca.pem'), caCert);
  copyFileSync(join(PROFILE, 'ca.key'), caKey);
}
const policyKey = readFileSync(join(PROFILE, 'policy-key.pub'), 'utf8').trim();
rmSync(join(PROFILE, 'policy-key.pub')); // the profile pins it; the device needs no second copy
step(`policy bundle signed: ${MODE}, intercepting ${HOSTS}`);

const CLASSIFIER = join(STAGE, 'bin', 'classifier-host.exe');
const classifierKey = join(KEYS, 'classifier.key.hex');
must(
  run(CLASSIFIER, [
    'release', '--dir', join(PROFILE, 'classifier'), '--state', 'enforcing', '--version', 'lab-1',
    '--key', classifierKey, '--rules', join(ROOT, 'endpoint', 'classifier-host', 'testdata', 'dev-rules.json'),
  ]),
  'classifier-host release',
);
const pub = run(CLASSIFIER, ['release', '--key', classifierKey, '--print-pubkey']);
must(pub, 'classifier-host release --print-pubkey');
const classifierPubkey = (/[0-9a-f]{64}/.exec(pub.out) ?? [''])[0];
if (!classifierPubkey) die(`could not read the classifier public key from: ${pub.out.trim()}`);
step('classifier release signed: lab-1');

copyFileSync(EDGE_CA, join(PROFILE, 'edge-ca.crt'));

// ---------------------------------------------------------------------------------------------
// 4. The enrolment profile. Paths are where the MSI installs the files above.

// The tenant's ceiling follows the bundle's mode: a device collecting at a mode its tenant does not
// permit would have every content grant denied.
const seeded = spawnSync('node', [join(ROOT, 'installer', 'seed.mjs'), '--tenant', TENANT, '--hours', TOKEN_HOURS, '--ceiling', MODE], { encoding: 'utf8', cwd: ROOT });
const token = seeded.status === 0 ? (seeded.stdout ?? '').trim() : '';
if (!token) die(`could not seed an enrolment token:\n${seeded.stderr ?? ''}`);
step(`enrolment token seeded (single use, valid ${TOKEN_HOURS}h)`);

const profile = {
  SAC_TENANT_ID: TENANT,
  // A local/no-drain fallback only: the agent stamps envelopes from the credential enrolment issues.
  SAC_DEVICE_ID: '22222222-2222-4222-8222-222222222222',
  SAC_USER_REF: 'lab-user',
  SAC_POPULATION: 'lab',
  SAC_BUNDLE: win32.join(INSTALLED, 'bundle.json'),
  SAC_POLICY_KEY: policyKey,
  SAC_CLASSIFIER_RELEASE: win32.join(INSTALLED, 'classifier'),
  SAC_CLASSIFIER_PUBKEY: classifierPubkey,
  // Where an M3 device holds content until a per-event grant uploads it. Harmless below M3:
  // nothing is written there.
  SAC_CONTENT_DIR: win32.join(LAYOUT.windows.statedir, 'content'),
  SAC_CONTENT_KEY: win32.join(LAYOUT.windows.statedir, 'content.key'),
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
  SAC_DEVICE_ENDPOINT: EDGE,
  SAC_AUTH_MODE: 'x509',
  SAC_ENROLMENT_TOKEN: token,
  SAC_CA_FILE: win32.join(INSTALLED, 'edge-ca.crt'),
  SAC_MDM_ID: MDM_ID,
  SAC_BACKOFF_BASE: '250ms',
  SAC_BACKOFF_CAP: '5s',
};
const profileFile = join(WORK, 'lab.env');
writeFileSync(
  profileFile,
  ['# installer/lab-msi.mjs - the lab enrolment profile for this host. Regenerated every build.', ...Object.entries(profile).map(([k, v]) => `${k}=${v}`), ''].join('\n'),
  { mode: 0o600 },
);

// ---------------------------------------------------------------------------------------------
// 5. The MSI.

step('building the MSI …');
const rel = (p) => p.slice(ROOT.length + 1);
const msi = spawnSync(
  'powershell',
  [
    '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', join(ROOT, 'installer', 'windows', 'Build-Msi.ps1'),
    '-ConfigFile', rel(profileFile), '-ProfileDir', rel(PROFILE), '-FreshEnrolment',
  ],
  { stdio: 'inherit', cwd: ROOT },
);
if (msi.status !== 0) die('Build-Msi.ps1 failed');

const out = join(ROOT, 'installer', 'dist', 'ShadowAICapture.msi');
console.log(`
lab-msi: built ${out}

  install    double-click it, or:  msiexec /i "${out}"
  uninstall  Apps & features > Shadow AI Capture, or:  msiexec /x "${out}"

  The service starts on install and at every boot. Open a NEW terminal afterwards: the proxy and
  CA environment the agent sets reaches processes started after it.

  health     ${win32.join(LAYOUT.windows.statedir, 'health.jsonl')}
  log        ${win32.join(LAYOUT.windows.statedir, 'service.log')}
  rows       docker exec sac-authlab-postgres-1 psql -U postgres -d shadow -x -c "select * from ingest.observation order by received_at desc limit 5"
  dashboard  http://127.0.0.1:8787/explore.html?transport=live
             events, prompt-text search, and (M3) retrieval of an uploaded prompt through content-vault
`);

// =============================================================================================

function run(cmd, args, opts = {}) {
  const res = spawnSync(cmd, args, { encoding: 'utf8', cwd: ROOT, ...opts });
  return { code: res.status ?? 1, out: `${res.stdout ?? ''}${res.stderr ?? ''}` };
}

function must(res, what) {
  if (res.code !== 0) die(`${what} failed:\n${res.out.trim()}`);
}
