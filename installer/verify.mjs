#!/usr/bin/env node
// installer/verify.mjs - the installer's own gate.
//
//   node installer/verify.mjs            # structural checks + a real --print-config run
//   node installer/verify.mjs --no-exec  # skip the capture-core run (no stage built yet)
//
// It decides four things that could each be wrong independently:
//   1. every configuration variable maps to a capture-core flag that actually exists (parsed from
//      cmd/capture-core/main.go, not from this repo's own manifest - a manifest that agrees with
//      itself proves nothing);
//   2. the three platform renderings carry the same flags, not three spellings of them;
//   3. the committed generated output is a function of the manifest (render --check);
//   4. the flags the installer produces are accepted by the real binary: a staged capture-core is
//      run with a representative argv and `--print-config`, and the resolved identity and drain are
//      read back from its output.
//
// A check that cannot run (no stage) is reported SKIPPED with its reason, never as a pass.

import { spawnSync } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { CONFIG, LAYOUT, PRODUCT, configFor, expandDefault } from './manifest.mjs';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const NO_EXEC = process.argv.includes('--no-exec');

let failures = 0;
let skipped = 0;
function check(name, ok, detail) {
  console.log(`${ok === 'SKIP' ? ' SKIP ' : ok ? '  ok  ' : ' FAIL '} ${name}${detail ? ` — ${detail}` : ''}`);
  if (ok === 'SKIP') skipped++;
  else if (!ok) failures++;
}

// ---------------------------------------------------------------------------------------------
// 1. The flags exist

const mainGo = readFileSync(join(ROOT, 'endpoint/capture-core/cmd/capture-core/main.go'), 'utf8');
const realFlags = new Set([...mainGo.matchAll(/fs\.[A-Za-z0-9]+Var\([^,]+,\s*"([a-z0-9-]+)"/g)].map((m) => m[1]));
check('parsed the real flag set from main.go', realFlags.size > 20, `${realFlags.size} flags`);
const missing = CONFIG.filter((c) => !realFlags.has(c.flag.replace(/^--/, '')));
check('every configuration variable maps to a real capture-core flag', missing.length === 0, missing.map((c) => `${c.env}->${c.flag}`).join(', ') || `${CONFIG.length} variables`);

// ---------------------------------------------------------------------------------------------
// 2. The manifest is internally consistent

const envs = new Set();
const flags = new Set();
let dup = 0;
for (const c of CONFIG) {
  if (envs.has(c.env) || flags.has(c.flag)) dup++;
  envs.add(c.env);
  flags.add(c.flag);
  if (!/^SAC_[A-Z0-9_]+$/.test(c.env)) check(`  ${c.env} is deployment vocabulary`, false, 'expected SAC_*');
}
check('no duplicate env names or flags', dup === 0, `${dup} duplicate(s)`);
const unresolved = CONFIG.filter((c) => expandDefault(c, LAYOUT.linux).includes('<'));
check('no unexpanded placeholder ships in a generated default', unresolved.length === 0, unresolved.map((c) => c.env).join(', ') || 'none');
const requiredEmpty = new Set(CONFIG.filter((c) => c.required && expandDefault(c, LAYOUT.linux) === '').map((c) => c.env));
const mustFill = new Set(CONFIG.filter((c) => c.mustFill).map((c) => c.env));
check(
  'every required value with no default is marked must-fill',
  [...requiredEmpty].every((e) => mustFill.has(e)),
  [...requiredEmpty].join(', ') || 'none empty',
);
check('every must-fill value has no baked default', CONFIG.filter((c) => c.mustFill).every((c) => expandDefault(c, LAYOUT.linux) === ''));
check('no secret bakes a default', CONFIG.filter((c) => c.secret).every((c) => expandDefault(c, LAYOUT.linux) === ''));
const spoolKey = CONFIG.find((c) => c.env === 'SAC_SPOOL_KEY');
const spoolDir = CONFIG.find((c) => c.env === 'SAC_SPOOL_DIR');
check(
  'the default spool key is outside the default spool directory',
  !expandDefault(spoolKey, LAYOUT.linux).startsWith(expandDefault(spoolDir, LAYOUT.linux) + '/'),
  `${expandDefault(spoolKey, LAYOUT.linux)}`,
);

// ---------------------------------------------------------------------------------------------
// 3. The three renderings agree

const linuxWrapper = readFileSync(join(ROOT, 'installer/generated/linux/capture-core-run'), 'utf8');
const cmdWrapper = readFileSync(join(ROOT, 'installer/generated/windows/capture-core-run.cmd'), 'utf8');
const wxs = readFileSync(join(ROOT, 'installer/generated/windows/ShadowAICapture.wxs'), 'utf8');
const envExample = readFileSync(join(ROOT, 'installer/generated/capture-core.env.example'), 'utf8');

function flagsIn(text) {
  return new Set([...text.matchAll(/--([a-z0-9-]+)/g)].map((m) => m[1]));
}
function sameSet(a, b) {
  return a.size === b.size && [...a].every((x) => b.has(x));
}
const linuxExpected = new Set(configFor('linux').map((c) => c.flag.replace(/^--/, '')));
const winExpected = new Set(configFor('windows').map((c) => c.flag.replace(/^--/, '')));
check('the Linux wrapper carries exactly the Linux flag set', sameSet(flagsIn(linuxWrapper), linuxExpected), `${flagsIn(linuxWrapper).size} flags`);
check('the Windows console wrapper carries exactly the Windows flag set', sameSet(flagsIn(cmdWrapper), winExpected), `${flagsIn(cmdWrapper).size} flags`);
check('the WiX source registers the service with the SAC_ARGS argv', /ServiceInstall[\s\S]*Arguments="\[SAC_ARGS\]"/.test(wxs), 'ServiceInstall Arguments="[SAC_ARGS]"');
check('the WiX default argv is a build-time variable, overridable at install', wxs.includes('$(var.SacArgs)') && wxs.includes('Id="SAC_ARGS"'));
const missingEnv = CONFIG.filter((c) => !envExample.includes(`${c.env}=`));
check('the generated env example carries every variable', missingEnv.length === 0, missingEnv.map((c) => c.env).join(', ') || `${CONFIG.length} variables`);
const hasSh = spawnSync('sh', ['-c', 'true']).status === 0;
check(
  'the Linux wrapper passes shell syntax check (sh -n)',
  hasSh ? run('sh', ['-n', join(ROOT, 'installer/generated/linux/capture-core-run')]) === 0 : 'SKIP',
  hasSh ? '' : 'no sh on PATH',
);

// The launchd plist and systemd unit must point at the generated wrapper, not a hand-typed path.
const plist = readFileSync(join(ROOT, 'installer/generated/macos/com.shadowaicapture.capture-core.plist'), 'utf8');
check('the LaunchDaemon runs the generated wrapper', plist.includes(`${LAYOUT.darwin.bindir}/capture-core-run`));
check('the systemd unit runs the generated wrapper', readFileSync(join(ROOT, 'installer/generated/linux/shadow-ai-capture.service'), 'utf8').includes(`${LAYOUT.linux.bindir}/capture-core-run`));

// ---------------------------------------------------------------------------------------------
// 4. Generated output is a function of the manifest

const renderCheck = run('node', [join(ROOT, 'installer/render.mjs'), '--check']);
check('installer/render.mjs --check reports no drift', renderCheck === 0, renderCheck === 0 ? '' : 'run: node installer/render.mjs');

// ---------------------------------------------------------------------------------------------
// 5. The real binary accepts the argv the installer produces

const stageBinary = join(ROOT, 'installer', '.stage', 'linux-amd64', 'bin', 'capture-core');
if (NO_EXEC) {
  check('capture-core --print-config accepts the generated argv', 'SKIP', '--no-exec');
} else if (!existsSync(stageBinary)) {
  check('capture-core --print-config accepts the generated argv', 'SKIP', `${stageBinary} not built; run node installer/build.mjs --os linux`);
} else {
  const work = mkdtempSync(join(tmpdir(), 'sac-installer-verify-'));
  const overrides = {
    SAC_TENANT_ID: '11111111-1111-4111-8111-111111111111',
    SAC_DEVICE_ID: '22222222-2222-4222-8222-222222222222',
    SAC_SPOOL_DIR: join(work, 'spool'),
    SAC_SPOOL_KEY: join(work, 'spool.key'),
    SAC_HEALTH_FILE: join(work, 'health.jsonl'),
    SAC_CREDENTIAL_FILE: join(work, 'credential.sealed'),
    SAC_DEVICE_ENDPOINT: 'https://ingest.example.com',
    SAC_AUTH_MODE: 'x509',
  };
  const argv = ['--print-config'];
  for (const c of configFor('linux')) {
    const v = c.env in overrides ? overrides[c.env] : expandDefault(c, LAYOUT.linux);
    if (v === '') continue;
    if (c.kind === 'bool') argv.push(`${c.flag}=${v}`);
    else argv.push(c.flag, v);
  }
  const res = spawnSync(stageBinary, argv, { encoding: 'utf8' });
  const out = `${res.stdout ?? ''}${res.stderr ?? ''}`;
  check('capture-core --print-config accepts the generated argv', res.status === 0, res.status === 0 ? '' : out.trim().split('\n').slice(-1)[0]);
  check('  the resolved identity is printed', /identity:\s+tenant=/.test(out));
  check('  the resolved drain is printed', /device drain:\s+endpoint=https:\/\/ingest\.example\.com/.test(out));
  rmSync(work, { recursive: true, force: true });
}

// ---------------------------------------------------------------------------------------------

function run(cmd, args) {
  const res = spawnSync(cmd, args, { encoding: 'utf8' });
  if (res.status !== 0) {
    const text = `${res.stdout ?? ''}${res.stderr ?? ''}`.trim();
    if (text) console.log(text.split('\n').map((l) => `         ${l}`).join('\n'));
  }
  return res.status ?? 1;
}

console.log(`\ninstaller: ${failures === 0 ? 'all checks passed' : `${failures} check(s) FAILED`}${skipped ? ` (${skipped} skipped)` : ''}`);
process.exit(failures === 0 ? 0 : 1);
