#!/usr/bin/env node
// installer/dev-host.mjs - run the endpoint ON the host against the local Docker auth lab.
//
//   node installer/dev-host.mjs                 # x509, the default lab tenant
//   node installer/dev-host.mjs --auth-mode dpop
//   node installer/dev-host.mjs --print-only    # resolve config and stop (no network)
//
// This is the host-side counterpart of installer/dev.mjs. dev.mjs runs the agent *inside* the lab
// network because some Docker daemons publish ports the calling host cannot reach; on a machine
// running Docker Desktop (a Windows host, say) the published ports ARE reachable at 127.0.0.1, so the
// agent can run as a real host process - which is what "run the endpoint on the host" means.
//
// It expects the AUTH lab to be up, not the default memory lab: the endpoint enrols, so it needs
// control-api and the edge, which only authlab.compose.yaml provides.
//
//   node localdev/build.mjs --auth
//   node localdev/run.mjs --auth
//   node installer/dev-host.mjs
//
// It automates the two steps that are easy to get wrong by hand: minting a single-use enrolment
// token (installer/seed.mjs), and aligning --device-id with the device_id the server mints - the
// agent stamps envelopes from the flag while control-api mints the value, so an unaligned run is
// rejected as schema_violation on /device_id.

import { spawn, spawnSync } from 'node:child_process';
import { closeSync, existsSync, mkdirSync, openSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { LAYOUT, configFor, expandDefault } from './manifest.mjs';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const ARGS = process.argv.slice(2);
function valueOf(flag, fallback) {
  const i = ARGS.indexOf(flag);
  return i === -1 ? fallback : ARGS[i + 1];
}

const HOST_OS = process.platform === 'win32' ? 'windows' : process.platform === 'darwin' ? 'darwin' : 'linux';
const HOST_ARCH = process.arch === 'arm64' ? 'arm64' : 'amd64';
const EXE = HOST_OS === 'windows' ? '.exe' : '';
const COMPOSE = join(ROOT, 'localdev', 'authlab.compose.yaml');
const CA_FILE = join(ROOT, 'localdev', '.authlab', 'dev-ca.crt');
const FRAMES = join(ROOT, 'endpoint', 'integration', 'testdata', 'native');
const PRINT_ONLY = ARGS.includes('--print-only');

const TENANT = valueOf('--tenant', '11111111-1111-1111-1111-111111111111');
const MDM_ID = valueOf('--mdm-id', 'sac-dev-host');
const AUTH_MODE = valueOf('--auth-mode', 'x509');
const PREFIX = resolve(ROOT, valueOf('--prefix', join('installer', '.lab', 'host')));
const STAGE = join(ROOT, 'installer', '.stage', `${HOST_OS}-${HOST_ARCH}`);
const BIN = join(STAGE, 'bin', `capture-core${EXE}`);
const PLACEHOLDER_DEVICE = '22222222-2222-4222-8222-222222222222';

let failures = 0;
function check(name, ok, detail) {
  console.log(`${ok ? '  ok  ' : ' FAIL '} ${name}${detail ? ` — ${detail}` : ''}`);
  if (!ok) failures++;
}
function die(msg) {
  console.error(`dev-host: ${msg}`);
  process.exit(1);
}

if (!LAYOUT[HOST_OS]) die(`unsupported host OS ${HOST_OS}`);

// ---------------------------------------------------------------------------------------------
// 1. The auth lab must be up; resolve the host-facing edge.

if (compose(['ps', '--status', 'running', '-q']).out.trim() === '') {
  die('the auth lab is not running. Start it first:\n  node localdev/build.mjs --auth\n  node localdev/run.mjs --auth');
}
const edgeURL = resolveEdgeURL();
if (!edgeURL) die('cannot find the edge’s published port (docker compose port edge 8443)');
check('auth lab reachable from the host', true, edgeURL);
if (!existsSync(CA_FILE)) die(`dev CA not found at ${CA_FILE}; run: node localdev/run.mjs --auth`);
check('dev CA present', true, CA_FILE);

// ---------------------------------------------------------------------------------------------
// 2. Build the host payload.

console.log(`dev-host: building the ${HOST_OS}/${HOST_ARCH} payload …`);
if (spawnSync('node', [join(ROOT, 'installer', 'build.mjs'), '--os', HOST_OS, '--arch', HOST_ARCH], { stdio: 'inherit', cwd: ROOT }).status !== 0) {
  die('build failed');
}
check('host payload built', existsSync(BIN), BIN);

// ---------------------------------------------------------------------------------------------
// 3. Mint a single-use token and compose the config.

const token = PRINT_ONLY ? `sac1.${TENANT}.print-only` : seedToken();
if (!PRINT_ONLY) check('a single-use enrolment token is seeded', token !== '', token ? 'minted' : 'seed failed');

const config = hostConfig(token, edgeURL);
mkdirSync(join(PREFIX, 'state'), { recursive: true });
const configFile = join(PREFIX, 'capture-core.env');
writeConfig(config, configFile);

// ---------------------------------------------------------------------------------------------
// 4. The real binary accepts the resolved argv.

const pc = spawnSync(BIN, [...argvFor(config), '--print-config'], { encoding: 'utf8' });
check('capture-core --print-config resolves the host profile', pc.status === 0, pc.status === 0 ? '' : `${pc.stdout ?? ''}${pc.stderr ?? ''}`.trim().split('\n').slice(-1)[0]);
check('  /identity/ printed', /identity:\s+tenant=/.test(`${pc.stdout ?? ''}${pc.stderr ?? ''}`));

if (PRINT_ONLY) finish('print-only');

// ---------------------------------------------------------------------------------------------
// 5. Enrol pass: run the service, wait for the credential, stop. No observations yet.

console.log('\ndev-host: enrolling (phase A) …');
const log = enrollPass(config);
check('the drain enrolled and stored a credential', existsSync(config.SAC_CREDENTIAL_FILE), existsSync(config.SAC_CREDENTIAL_FILE) ? config.SAC_CREDENTIAL_FILE : 'no credential');
if (!existsSync(config.SAC_CREDENTIAL_FILE)) console.log(log.split('\n').slice(-15).map((l) => `         ${l}`).join('\n'));

const deviceId = queryDevice();
if (!deviceId) {
  check('ops.device carries the enrolled device', false, `no row for mdm_id=${MDM_ID}`);
  finish('enrolment');
}
check('ops.device carries the enrolled device', true, `device_id=${deviceId}`);

if (deviceId !== config.SAC_DEVICE_ID) {
  console.log(`\ndev-host: aligning --device-id ${config.SAC_DEVICE_ID} -> ${deviceId} (the agent stamps envelopes from flags)`);
  config.SAC_DEVICE_ID = deviceId;
  writeConfig(config, configFile);
}

// ---------------------------------------------------------------------------------------------
// 6. Feed the golden frames and drain them.

console.log('\ndev-host: feeding golden frames and draining (phase B) …');
const frames = spawnSync(BIN, [...argvFor(config), '--native-frames', FRAMES], { encoding: 'utf8' });
const out = `${frames.stdout ?? ''}${frames.stderr ?? ''}`;
check('the golden-frames run exits zero', frames.status === 0, `exit ${frames.status}`);
const acked = (out.match(/->\s+ack\b/g) || []).length;
check('the endpoint spooled and drained observations', acked > 0, `${acked} ack`);
if (frames.status !== 0) console.log(out.split('\n').slice(-15).map((l) => `         ${l}`).join('\n'));

// ---------------------------------------------------------------------------------------------
// 7. The proof: rows in the real schema.

const obs = int(psql(`select count(*) from ingest.observation where tenant_id='${TENANT}'`));
const rej = int(psql(`select count(*) from ingest.rejected where tenant_id='${TENANT}'`));
check('ingest.observation holds rows for the tenant', obs > 0, `${obs} observation(s)`);
check('nothing was rejected on identity', rej === 0, `${rej} rejected`);

finish('host end-to-end');

// =============================================================================================

function hostConfig(tokenValue, edge) {
  const v = {};
  for (const c of configFor(HOST_OS)) v[c.env] = expandDefault(c, LAYOUT[HOST_OS]);
  v.SAC_TENANT_ID = TENANT;
  v.SAC_DEVICE_ID = PLACEHOLDER_DEVICE;
  v.SAC_MDM_ID = MDM_ID;
  v.SAC_USER_REF = 'dev-user';
  v.SAC_POPULATION = 'dev';
  v.SAC_SPOOL_DIR = join(PREFIX, 'state', 'spool');
  v.SAC_SPOOL_KEY = join(PREFIX, 'state', 'spool.key');
  v.SAC_CREDENTIAL_FILE = join(PREFIX, 'state', 'credential.sealed');
  v.SAC_HEALTH_FILE = join(PREFIX, 'state', 'health.jsonl');
  v.SAC_SPOOL_BOUNDS = 'dev';
  v.SAC_RETENTION = '1h';
  v.SAC_PROXY_TLS = 'false';
  v.SAC_PROXY_LOOPBACK = 'false';
  v.SAC_PROC_DETECT = 'false';
  v.SAC_DEVICE_ENDPOINT = edge;
  v.SAC_AUTH_MODE = AUTH_MODE;
  v.SAC_CA_FILE = CA_FILE;
  v.SAC_ENROLMENT_TOKEN = tokenValue;
  v.SAC_BACKOFF_BASE = '250ms';
  v.SAC_BACKOFF_CAP = '5s';
  v.SAC_LOG_LEVEL = 'debug';
  return v;
}

function writeConfig(values, file) {
  const lines = ['# Shadow AI Capture - host dev profile (installer/dev-host.mjs)'];
  for (const c of configFor(HOST_OS)) lines.push(`${c.env}=${values[c.env] ?? ''}`);
  lines.push('');
  writeFileSync(file, lines.join('\n'), { mode: 0o600 });
}

/** The flag list, kept identical to installer/render.mjs --args. */
function argvFor(values) {
  const argv = [];
  for (const c of configFor(HOST_OS)) {
    const v = values[c.env] ?? '';
    if (v === '') continue;
    if (c.kind === 'bool') argv.push(`${c.flag}=${v}`);
    else argv.push(c.flag, v);
  }
  return argv;
}

/**
 * Run the service until the credential file exists, then stop it. stdout/stderr go to a file, not a
 * pipe: Node is single-threaded, so a synchronous wait never drains a pipe and a chatty debug run
 * could fill it and stall the agent.
 */
function enrollPass(cfg) {
  const logFile = join(PREFIX, 'state', 'enrol.log');
  mkdirSync(dirname(logFile), { recursive: true });
  const fd = openSync(logFile, 'a');
  const child = spawn(BIN, argvFor(cfg), { stdio: ['ignore', fd, fd] });
  const deadline = Date.now() + 25000;
  while (Date.now() < deadline && !existsSync(cfg.SAC_CREDENTIAL_FILE)) sleepSync(200);
  // The credential is written by an atomic rename, so once the file exists it is complete; a hard
  // kill is safe here (SIGTERM on POSIX runs the graceful path, TerminateProcess on Windows).
  child.kill();
  closeSync(fd);
  try {
    return readFileSync(logFile, 'utf8');
  } catch {
    return '';
  }
}

function seedToken() {
  const res = spawnSync('node', [join(ROOT, 'installer', 'seed.mjs'), '--tenant', TENANT], { encoding: 'utf8', cwd: ROOT });
  process.stderr.write(res.stderr ?? '');
  return res.status === 0 ? (res.stdout ?? '').trim() : '';
}

function queryDevice() {
  return psql(`select device_id from ops.device where tenant_id='${TENANT}' and mdm_id='${MDM_ID}' order by enrolled_at desc limit 1`).trim();
}

function resolveEdgeURL() {
  const r = spawnSync('docker', ['compose', '-f', COMPOSE, 'port', 'edge', '8443'], { encoding: 'utf8', cwd: ROOT });
  const m = /:(\d+)\s*$/.exec(`${r.stdout ?? ''}`.trim());
  return m ? `https://127.0.0.1:${m[1]}` : '';
}

function compose(args) {
  const res = spawnSync('docker', ['compose', '-f', COMPOSE, ...args], { encoding: 'utf8', cwd: ROOT });
  return { code: res.status ?? 1, out: `${res.stdout ?? ''}${res.stderr ?? ''}` };
}

function psql(sql) {
  const r = spawnSync('docker', ['compose', '-f', COMPOSE, 'exec', '-T', 'postgres', 'psql', '-U', 'postgres', '-d', 'shadow', '-Atc', sql], { encoding: 'utf8', cwd: ROOT });
  return r.status === 0 ? `${r.stdout ?? ''}` : '';
}

function int(text) {
  const n = Number.parseInt(text.trim(), 10);
  return Number.isFinite(n) ? n : -1;
}

function sleepSync(ms) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
}

function finish(label) {
  console.log(`\ndev-host: ${failures === 0 ? `${label}: all checks passed` : `${label}: ${failures} check(s) FAILED`}`);
  console.log(`dev-host: work dir ${PREFIX}`);
  console.log('dev-host: stop the lab with: node localdev/run.mjs --auth --down');
  process.exit(failures === 0 ? 0 : 1);
}
