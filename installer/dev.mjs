#!/usr/bin/env node
// installer/dev.mjs - install the endpoint on this host and drive it end to end against the lab.
//
//   node installer/dev.mjs                 # install into installer/.lab/root, bring up the auth
//                                          # lab, enrol, drain, and prove rows land in the schema
//   node installer/dev.mjs --no-lab        # install + --print-config only (no Docker)
//   node installer/dev.mjs --keep          # keep the previous lab state volume (skip re-enrol)
//   node installer/dev.mjs --auth-mode dpop
//
// This is the "check progress" command. It is not a release artefact: the release artefacts are the
// WiX source, the PKG script and the Linux installer, and they share installer/manifest.mjs. What
// this adds is the one thing none of them can show on a Linux CI host - that the installed agent
// actually enrols through the Application-Gateway stand-in and delivers its spool to the real
// ingestion schema.
//
// Two environment facts shape it:
//
//   * The device runs inside the lab network, beside the gateway, not on the host. Some Docker
//     daemons publish ports this shell cannot reach, and a device on the network is the faithful
//     placement anyway (authlab smoke does the same).
//   * The payload and its state travel by image build context and a named volume, not a host bind
//     mount: the same daemons present a host path the daemon cannot see as an empty directory. The
//     context is streamed by the client, so it always arrives.
//
// It also proves the identity fix: control-api mints device_id server-side and capture-core adopts
// the issued credential as its envelope identity, so the installer no longer has to read the minted
// device_id back out of ops.device and align --device-id. The placeholder --device-id is left as a
// no-drain/local fallback only; the drain's first start enrols and stamps the issued identity.

import { spawnSync } from 'node:child_process';
import { createHash, randomBytes } from 'node:crypto';
import { copyFileSync, existsSync, mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { LAYOUT, configFor, expandDefault } from './manifest.mjs';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const ARGS = process.argv.slice(2);
function valueOf(flag, fallback) {
  const i = ARGS.indexOf(flag);
  return i === -1 ? fallback : ARGS[i + 1];
}
const NO_LAB = ARGS.includes('--no-lab');
const KEEP = ARGS.includes('--keep');
const AUTH_MODE = valueOf('--auth-mode', 'x509');
const PREFIX = resolve(ROOT, valueOf('--prefix', join('installer', '.lab', 'root')));
const STAGE = resolve(ROOT, 'installer', '.stage', 'linux-amd64');
const FRAMES = join(ROOT, 'endpoint', 'integration', 'testdata', 'native');
const AUTH_DIR = join(ROOT, 'localdev', '.authlab');
const CA_FILE = join(AUTH_DIR, 'dev-ca.crt');
const COMPOSE_AUTH = join(ROOT, 'localdev', 'authlab.compose.yaml');
const LAB_NETWORK = 'scorecard-authlab';
const LAB_IMAGE = 'sac/installer-lab:dev';
const LAB_VOLUME = 'sac-installer-lab-state';
const LAB_CTX = join(ROOT, 'installer', '.lab', 'context');
const LAB_TENANT = '11111111-1111-1111-1111-111111111111';
const PLACEHOLDER_DEVICE = '22222222-2222-4222-8222-222222222222';
const MDM_ID = 'sac-dev-installer';

const TMP = join(ROOT, '.testtmp');
mkdirSync(TMP, { recursive: true });
const ENV = { ...process.env, TMP, TEMP: TMP, TMPDIR: TMP };

let failures = 0;
let skipped = 0;
function check(name, ok, detail) {
  const mark = ok === 'SKIP' ? ' SKIP ' : ok ? '  ok  ' : ' FAIL ';
  console.log(`${mark} ${name}${detail ? ` — ${detail}` : ''}`);
  if (ok === 'SKIP') skipped++;
  else if (!ok) failures++;
}

// ---------------------------------------------------------------------------------------------
// 0. Build and stage

console.log('dev: building the Linux payload …');
if (run('node', [join(ROOT, 'installer', 'build.mjs'), '--os', 'linux']) !== 0) {
  console.error('dev: build failed');
  process.exit(1);
}
if (!existsSync(join(STAGE, 'bin', 'capture-core'))) {
  console.error(`dev: ${STAGE}/bin/capture-core is missing after the build`);
  process.exit(1);
}

// ---------------------------------------------------------------------------------------------
// 1. Bring the auth lab up and seed a tenant + enrolment token

let edgeUrl = '';
let enrolmentToken = '';
if (NO_LAB) {
  check('auth lab reachable', 'SKIP', '--no-lab');
} else {
  edgeUrl = ensureLab();
  enrolmentToken = seedToken(LAB_TENANT);
  check('auth lab ready at the edge', edgeUrl !== '', edgeUrl || 'could not bring the lab up');
  check('a single-use enrolment token is seeded in ops.enrolment_token', enrolmentToken !== '', enrolmentToken ? 'minted' : 'seed failed');
  if (edgeUrl === '') finish('lab bring-up');
}

// ---------------------------------------------------------------------------------------------
// 2. Install the payload into a prefix and prove the installed profile parses

if (!KEEP || !existsSync(PREFIX)) rmSync(PREFIX, { recursive: true, force: true });
mkdirSync(join(PREFIX, 'etc'), { recursive: true });

const hostConfig = defaultConfig();
hostConfig.SAC_DEVICE_ENDPOINT = NO_LAB ? '' : edgeUrl;
hostConfig.SAC_AUTH_MODE = NO_LAB ? '' : AUTH_MODE;
hostConfig.SAC_CA_FILE = NO_LAB ? '' : CA_FILE;
hostConfig.SAC_ENROLMENT_TOKEN = enrolmentToken;
writeConfig(hostConfig, join(PREFIX, 'etc', 'capture-core.env'));

if (run('sh', [join(ROOT, 'installer', 'linux', 'install.sh'), '--stage', STAGE, '--prefix', PREFIX, '--config', join(PREFIX, 'etc', 'capture-core.env'), '--no-service']) !== 0) {
  console.error('dev: install failed');
  process.exit(1);
}
check('payload installed', existsSync(join(PREFIX, 'bin', 'capture-core')) && existsSync(join(PREFIX, 'bin', 'capture-core-run')), PREFIX);

const printRes = runCapture('--print-config', hostConfig);
check('capture-core --print-config resolves the installed profile', printRes.status === 0, printRes.status === 0 ? '' : printRes.output.trim().split('\n').slice(-1)[0]);
check('  /identity/ is printed from the profile', /identity:\s+tenant=/.test(printRes.output));
if (!NO_LAB) {
  check('  the drain is configured', new RegExp(`device drain:\\s+endpoint=${escapeRe(hostConfig.SAC_DEVICE_ENDPOINT)}`).test(printRes.output));
} else {
  check('  the drain is disabled without a lab', /device drain:\s+disabled/.test(printRes.output));
}

if (NO_LAB) finish('install + print-config only (--no-lab)');

// ---------------------------------------------------------------------------------------------
// 3. Assemble the lab image and state volume

buildLabImage();

if (!KEEP) dockerRun(['volume', 'rm', '-f', LAB_VOLUME]);
// A fresh volume is created implicitly by the first run; removing it is what makes enrolment a
// first-time event rather than a replay of a previous credential.

const labConfig = defaultConfig();
labConfig.SAC_SPOOL_DIR = '/state/spool';
labConfig.SAC_SPOOL_KEY = '/state/spool.key';
labConfig.SAC_CREDENTIAL_FILE = '/state/credential.sealed';
labConfig.SAC_HEALTH_FILE = '/state/health.jsonl';
labConfig.SAC_CA_FILE = '/app/ca/dev-ca.crt';
labConfig.SAC_DEVICE_ENDPOINT = edgeUrl;
labConfig.SAC_AUTH_MODE = AUTH_MODE;
labConfig.SAC_ENROLMENT_TOKEN = enrolmentToken;

// ---------------------------------------------------------------------------------------------
// 4. Enrol: run the installed service, wait for the credential, stop. No observations yet.
//
// The device_id control-api mints is not the one we guessed, so enrolling before spooling keeps the
// first spooled envelope from being rejected. This is the installer working around a known agent
// gap (see the header), not the agent behaving correctly.

console.log('\ndev: enrolling against the real control-api (phase A: credential only) …');
const enrolPass = enrollInLab(labConfig, 25000);
check('the drain enrolled and stored a credential', enrolPass.enrolled, enrolPass.enrolled ? '/state/credential.sealed' : 'no credential appeared');
if (!enrolPass.enrolled) console.log(enrolPass.log.split('\n').slice(-15).map((l) => `         ${l}`).join('\n'));

const deviceRow = queryDevice();
if (!deviceRow.deviceId) {
  check('ops.device carries the enrolled device', false, 'no row for the hardware identity');
  finish('enrolment');
}
check('ops.device carries the enrolled device', true, `device_id=${deviceRow.deviceId} region=${deviceRow.region}`);
check('the device is in the seeded tenant', deviceRow.tenantId === LAB_TENANT, deviceRow.tenantId);

// No --device-id alignment here: capture-core adopts the issued credential as its envelope identity,
// so the placeholder --device-id is only a local/no-drain fallback and is never stamped on a drain run.

// ---------------------------------------------------------------------------------------------
// 5. Populate the spool from the golden frames and drain it. The shutdown drain is the flush.

console.log('\ndev: feeding golden frames and draining (phase B) …');
const framesRes = runCaptureInLab(labConfig, '--native-frames', ['/app/frames']);
check('the golden-frames run exits zero', framesRes.status === 0, `exit ${framesRes.status}`);
const acked = (framesRes.output.match(/->\s+ack\b/g) || []).length;
const refused = (framesRes.output.match(/->\s+refusal\b/g) || []).length;
check('every frame produced an ack or a typed refusal', acked + refused >= 6, `${acked} ack, ${refused} refusal`);
check('at least one observation was spooled for the drain', acked > 0, `${acked} acked`);
if (framesRes.status !== 0) console.log(framesRes.output.split('\n').slice(-15).map((l) => `         ${l}`).join('\n'));

// ---------------------------------------------------------------------------------------------
// 6. The proof: rows in the real schema, and a healthy drain

const observations = scalarInt(`select count(*) from ingest.observation where tenant_id = '${LAB_TENANT}'`);
const submissions = scalarInt(`select count(*) from ingest.submission where tenant_id = '${LAB_TENANT}'`);
const rejected = scalarInt(`select count(*) from ingest.rejected where tenant_id = '${LAB_TENANT}'`);
check('ingest.observation holds rows for the tenant', observations > 0, `${observations} observation(s)`);
check('ingest.submission groups them', submissions > 0, `${submissions} submission(s)`);
check('nothing was rejected on identity', rejected === 0, `${rejected} rejected`);

const drain = lastDrainStatus();
if (drain) {
  check('the health channel reports the drain healthy', drain.state === 'healthy', `state=${drain.state} detail=${drain.detail ?? ''} enrolled=${drain.enrolled}`);
} else {
  check('the health channel reports the drain', 'SKIP', 'no drain row in /state/health.jsonl');
}

finish('lab end-to-end');

// =============================================================================================
// Helpers

function defaultConfig() {
  const values = {};
  for (const c of configFor('linux')) values[c.env] = expandDefault(c, LAYOUT.linux);
  values.SAC_TENANT_ID = LAB_TENANT;
  values.SAC_DEVICE_ID = PLACEHOLDER_DEVICE;
  values.SAC_MDM_ID = MDM_ID;
  values.SAC_USER_REF = 'dev-user';
  values.SAC_POPULATION = 'dev';
  values.SAC_SPOOL_DIR = join(PREFIX, 'state', 'spool');
  values.SAC_SPOOL_KEY = join(PREFIX, 'state', 'spool.key');
  values.SAC_CREDENTIAL_FILE = join(PREFIX, 'state', 'credential.sealed');
  values.SAC_HEALTH_FILE = join(PREFIX, 'state', 'health.jsonl');
  values.SAC_SPOOL_BOUNDS = 'dev';
  values.SAC_RETENTION = '1h';
  values.SAC_PROXY_TLS = 'false';
  values.SAC_PROXY_LOOPBACK = 'false';
  values.SAC_PROC_DETECT = 'false';
  values.SAC_DRAIN_DEADLINE = '15s';
  values.SAC_BACKOFF_BASE = '250ms';
  values.SAC_BACKOFF_CAP = '5s';
  values.SAC_LOG_LEVEL = 'debug';
  return values;
}

function writeConfig(values, file) {
  const lines = ['# Shadow AI Capture - development installer profile (installer/dev.mjs)'];
  for (const c of configFor('linux')) lines.push(`${c.env}=${values[c.env] ?? ''}`);
  lines.push('');
  writeFileSync(file, lines.join('\n'), { mode: 0o600 });
}

/** The flag list, kept identical to installer/render.mjs --args so the two never diverge by hand. */
function argvFor(values) {
  const argv = [];
  for (const c of configFor('linux')) {
    const v = values[c.env] ?? '';
    if (v === '') continue;
    if (c.kind === 'bool') argv.push(`${c.flag}=${v}`);
    else argv.push(c.flag, v);
  }
  return argv;
}

/** Run the installed binary on the host, for --print-config. */
function runCapture(mode, values) {
  const res = spawnSync(join(PREFIX, 'bin', 'capture-core'), [...argvFor(values), mode], { encoding: 'utf8', env: ENV, cwd: ROOT });
  return { status: res.status ?? 1, output: `${res.stdout ?? ''}${res.stderr ?? ''}` };
}

/** The lab context: payload + CA + frames, streamed to the daemon by docker build. */
function buildLabImage() {
  rmSync(LAB_CTX, { recursive: true, force: true });
  for (const d of ['bin', 'ca', 'frames']) mkdirSync(join(LAB_CTX, d), { recursive: true });
  for (const name of readdirSync(join(STAGE, 'bin'))) {
    const src = join(STAGE, 'bin', name);
    if (name.startsWith('capture-core') || name.startsWith('classifier-host')) copyFileSync(src, join(LAB_CTX, 'bin', name));
  }
  copyFileSync(CA_FILE, join(LAB_CTX, 'ca', 'dev-ca.crt'));
  for (const name of readdirSync(FRAMES)) {
    if (name.endsWith('.json')) copyFileSync(join(FRAMES, name), join(LAB_CTX, 'frames', name));
  }
  writeFileSync(
    join(LAB_CTX, 'Dockerfile'),
    ['FROM alpine:latest', 'COPY bin/ /app/bin/', 'COPY ca/ /app/ca/', 'COPY frames/ /app/frames/', 'RUN chmod +x /app/bin/capture-core /app/bin/classifier-host', ''].join('\n'),
  );
  const res = dockerRun(['build', '-t', LAB_IMAGE, LAB_CTX]);
  if (res.code !== 0) {
    console.error(`dev: docker build of the lab image failed:\n${res.out}`);
    process.exit(1);
  }
}

/**
 * Phase A: run the installed service detached, wait until the credential exists inside the state
 * volume, then stop it with SIGTERM (which is what the service manager sends).
 */
function enrollInLab(config, timeoutMs) {
  const name = 'sac-installer-enrol';
  dockerRun(['rm', '-f', name]);
  const start = dockerRun(['run', '-d', '--name', name, '--network', LAB_NETWORK, '-v', `${LAB_VOLUME}:/state`, LAB_IMAGE, '/app/bin/capture-core', ...argvFor(config)]);
  let enrolled = false;
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (dockerRun(['exec', name, 'test', '-f', '/state/credential.sealed']).code === 0) {
      enrolled = true;
      break;
    }
    sleepSync(250);
  }
  dockerRun(['stop', '-t', '15', name]);
  const logs = dockerRun(['logs', name]);
  dockerRun(['rm', '-f', name]);
  if (start.code !== 0) console.error(`dev: docker run failed:\n${start.out}`);
  return { enrolled, log: logs.out };
}

function runCaptureInLab(config, mode, extra = []) {
  const res = dockerRun(['run', '--rm', '--network', LAB_NETWORK, '-v', `${LAB_VOLUME}:/state`, LAB_IMAGE, '/app/bin/capture-core', ...argvFor(config), mode, ...extra]);
  return { status: res.code, output: res.out };
}

function volumeCat(path) {
  return dockerRun(['run', '--rm', '-v', `${LAB_VOLUME}:/state`, LAB_IMAGE, 'cat', path]).out;
}

function dockerRun(args) {
  const res = spawnSync('docker', args, { encoding: 'utf8', env: ENV, cwd: ROOT });
  return { code: res.status ?? 1, out: `${res.stdout ?? ''}${res.stderr ?? ''}` };
}

function ensureLab() {
  const up = composeAuth(['ps', '--status', 'running', '-q']).out.trim() !== '';
  if (existsSync(CA_FILE) && up) {
    console.log('dev: reusing the running auth lab (its PKI is the one in localdev/.authlab)');
  } else {
    console.log('dev: bringing the auth lab up (fresh PKI) …');
    composeAuth(['down', '-v']);
    if (run('node', [join(ROOT, 'localdev', 'run.mjs'), '--auth']) !== 0) return '';
  }
  return 'https://edge:8443';
}

function seedToken(tenant) {
  const token = `sac1.${tenant}.${randomBytes(32).toString('base64url')}`;
  const hash = 'sha256:' + createHash('sha256').update(token).digest('hex');
  const sql = `
    INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, ceiling_mode)
      VALUES ('${tenant}', 'installer-dev', 'active', 'authlab-region', 'vendor', 'm1')
      ON CONFLICT (tenant_id) DO NOTHING;
    INSERT INTO ops.enrolment_token (tenant_id, token_hash, expires_at)
      VALUES ('${tenant}', '${hash}', now() + interval '1 day')
      ON CONFLICT DO NOTHING;
  `;
  const r = composeAuth(['exec', '-T', 'postgres', 'psql', '-U', 'postgres', '-d', 'shadow', '-v', 'ON_ERROR_STOP=1', '-c', sql]);
  return r.code === 0 ? token : '';
}

function queryDevice() {
  const hwid = hardwareIdentityHash(LAB_TENANT, MDM_ID);
  const row = composeAuth(['exec', '-T', 'postgres', 'psql', '-U', 'postgres', '-d', 'shadow', '-Atc',
    `select tenant_id || '|' || device_id || '|' || coalesce(residency_region,'') from ops.device where tenant_id='${LAB_TENANT}' and hardware_identity_hash='${hwid}' limit 1`]).out.trim();
  const [tenantId, deviceId, region] = row.split('|');
  return { tenantId, deviceId, region };
}

function hardwareIdentityHash(tenantID, mdmID) {
  return 'sha256:' + createHash('sha256').update(Buffer.from(`sac-hwid\u001f${tenantID}\u001f${mdmID}`, 'utf8')).digest('hex');
}

function scalarInt(sql) {
  const out = composeAuth(['exec', '-T', 'postgres', 'psql', '-U', 'postgres', '-d', 'shadow', '-Atc', sql]).out.trim();
  const n = Number.parseInt(out, 10);
  return Number.isFinite(n) ? n : -1;
}

function lastDrainStatus() {
  try {
    const lines = volumeCat('/state/health.jsonl').trim().split('\n');
    for (let i = lines.length - 1; i >= 0; i--) {
      const snap = JSON.parse(lines[i]);
      if (snap.drain) return snap.drain;
    }
  } catch {
    /* no health file */
  }
  return null;
}

function composeAuth(args) {
  const res = spawnSync('docker', ['compose', '-f', COMPOSE_AUTH, ...args], { encoding: 'utf8', env: ENV, cwd: ROOT });
  return { code: res.status ?? 1, out: `${res.stdout ?? ''}${res.stderr ?? ''}` };
}

function run(cmd, args) {
  const res = spawnSync(cmd, args, { encoding: 'utf8', env: ENV, cwd: ROOT, stdio: 'inherit' });
  return res.status ?? 1;
}

function sleepSync(ms) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
}

function escapeRe(s) {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

function finish(label) {
  console.log(`\ndev: ${failures === 0 ? `${label}: all checks passed` : `${label}: ${failures} check(s) FAILED`}${skipped ? ` (${skipped} skipped)` : ''}`);
  console.log(`dev: prefix ${PREFIX}`);
  console.log('dev: the lab is left running; stop it with: node localdev/run.mjs --auth --down');
  process.exit(failures === 0 ? 0 : 1);
}
