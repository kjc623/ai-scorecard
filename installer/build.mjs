#!/usr/bin/env node
// installer/build.mjs - compile the device payload and stage it for one target platform.
//
//   node installer/build.mjs --os linux                 # host-arch Linux payload
//   node installer/build.mjs --os windows --arch amd64  # cross-compiled Windows payload
//   node installer/build.mjs --os darwin  --arch arm64
//   node installer/build.mjs --os windows --src DIR    # compile from an exported tree (e.g. git archive of a tag)
//
// The stage is the input every packager reads: the Windows WiX source, the macOS pkgbuild script
// and the Linux install.sh all point at installer/.stage/<os>-<arch>/, so the binary a platform
// tests is the binary the other two would install. It runs with GOPROXY=off and CGO_ENABLED=0, like
// the rest of the repository's gates, so a build never reaches the network and the result is a
// static binary that runs in the same places the deployment runs.
//
// The stage carries a JSON manifest with a SHA-256 per file, so a packager can assert it built from
// the bytes it meant to and a verifier can report what is actually in the artefact.

import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { copyFileSync, existsSync, mkdirSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { GO_BINARIES, LAYOUT, PRODUCT, configFor, expandDefault } from './manifest.mjs';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const ARGS = process.argv.slice(2);

function valueOf(flag, fallback) {
  const i = ARGS.indexOf(flag);
  return i === -1 ? fallback : ARGS[i + 1];
}

const os = valueOf('--os', process.platform === 'win32' ? 'windows' : process.platform === 'darwin' ? 'darwin' : 'linux');
const arch = valueOf('--arch', process.arch === 'arm64' ? 'arm64' : 'amd64');
const OUT = resolve(ROOT, valueOf('--out', join('installer', '.stage', `${os}-${arch}`)));
const KEEP = ARGS.includes('--keep');
// The tree the Go modules are compiled from: the working tree, or an export of a tagged release so a
// release is built from exactly the source it names rather than from whatever is mid-edit.
const SRC = resolve(ROOT, valueOf('--src', '.'));

if (!LAYOUT[os]) {
  console.error(`installer/build.mjs: --os must be one of ${Object.keys(LAYOUT).join(', ')} (got ${os})`);
  process.exit(2);
}
if (!['amd64', 'arm64'].includes(arch)) {
  console.error(`installer/build.mjs: --arch must be amd64 or arm64 (got ${arch})`);
  process.exit(2);
}

const EXE = os === 'windows' ? '.exe' : '';
const TMP = join(ROOT, '.testtmp');
mkdirSync(TMP, { recursive: true });

const GO_ENV = {
  ...process.env,
  GOCACHE: join(ROOT, '.tools', 'gocache'),
  GOPATH: join(ROOT, '.tools', 'gopath'),
  GOMODCACHE: join(ROOT, '.tools', 'gopath', 'pkg', 'mod'),
  GOTMPDIR: join(ROOT, '.tools', 'tmp', 'go'),
  GOPROXY: 'off',
  GOFLAGS: '-mod=mod',
  GOTOOLCHAIN: 'local',
  GOOS: os,
  GOARCH: arch,
  CGO_ENABLED: '0',
  TMP,
  TEMP: TMP,
  TMPDIR: TMP,
};

// ---------------------------------------------------------------------------------------------
// 1. Compile

if (existsSync(OUT) && !KEEP) rmrf(OUT);
mkdirSync(join(OUT, 'bin'), { recursive: true });
mkdirSync(join(OUT, 'etc'), { recursive: true });

const built = [];
for (const b of GO_BINARIES) {
  const out = join(OUT, 'bin', `${b.name}${EXE}`);
  process.stdout.write(`compile ${b.name} (${os}/${arch}) … `);
  const res = run('go', ['build', '-trimpath', '-ldflags=-s -w', '-o', out, b.pkg], { cwd: join(SRC, b.dir) });
  if (res !== 0) process.exit(res);
  console.log('ok');
  built.push(rel(out));
}

// ---------------------------------------------------------------------------------------------
// 2. Stage the wrapper and the configuration template

const generated = join(ROOT, 'installer', 'generated');
if (!existsSync(generated)) {
  console.error('installer/build.mjs: installer/generated is missing. Run: node installer/render.mjs');
  process.exit(1);
}

const wrapperSrc = os === 'windows' ? join(generated, 'windows', 'capture-core-run.cmd') : join(generated, os, 'capture-core-run');
const wrapperDest = join(OUT, 'bin', `capture-core-run${os === 'windows' ? '.cmd' : ''}`);
copyFileSync(wrapperSrc, wrapperDest);
const envDest = join(OUT, 'etc', 'capture-core.env.example');
copyFileSync(join(generated, 'capture-core.env.example'), envDest);

// The platform's service definition travels with the stage so a packager never invents one.
const serviceSrc = {
  linux: join(generated, 'linux', 'shadow-ai-capture.service'),
  darwin: join(generated, 'macos', 'com.shadowaicapture.capture-core.plist'),
  windows: join(generated, 'windows', 'ShadowAICapture.wxs'),
}[os];
if (serviceSrc) copyFileSync(serviceSrc, join(OUT, 'etc', 'service-definition' + (os === 'darwin' ? '.plist' : os === 'windows' ? '.wxs' : '')));

const readme = [
  `${PRODUCT.displayName} endpoint - development payload (${os}/${arch})`,
  `Version: ${PRODUCT.version}`,
  '',
  'Contents:',
  '  bin/capture-core       the agent (docs/01-collectors.md §3.1)',
  '  bin/classifier-host    rules/validators/model host (ADR 0016)',
  '  bin/capture-core-run   sources etc/capture-core.env and execs the agent',
  '  etc/capture-core.env.example  the enrolment-profile template',
  '  etc/service-definition*       the systemd unit / LaunchDaemon / WiX source',
  '',
  'This is a DEVELOPMENT payload: unsigned, and the wrapper reads a plaintext env file. The',
  'production artefact is signed (docs/05-platform-delivery.md §7) and the profile is delivered',
  'by the customer\'s MDM, not the installer.',
  '',
].join('\n');
writeFileSync(join(OUT, 'README.txt'), readme);

// ---------------------------------------------------------------------------------------------
// 3. The stage manifest

const files = walk(OUT).filter((f) => !f.endsWith('install-manifest.json')).sort();
const manifest = {
  product: PRODUCT.name,
  displayName: PRODUCT.displayName,
  version: PRODUCT.version,
  os,
  arch,
  generatedBy: 'installer/build.mjs',
  source: SRC === ROOT ? 'working tree' : SRC,
  unsigned: true,
  layout: LAYOUT[os],
  files: files.map((f) => ({ path: rel(f), sha256: sha256(f), bytes: statSync(f).size })),
  config: configFor(os).map((c) => ({
    env: c.env,
    flag: c.flag,
    kind: c.kind,
    required: Boolean(c.required),
    secret: Boolean(c.secret),
    default: c.secret ? '' : expandDefault(c, LAYOUT[os]),
  })),
};
writeFileSync(join(OUT, 'install-manifest.json'), JSON.stringify(manifest, null, 2) + '\n');

console.log(`\nstage: ${rel(OUT)}`);
for (const b of built) console.log(`  ${rel(b)}`);
console.log(`  etc/capture-core.env.example`);
console.log(`  ${rel(join(OUT, 'install-manifest.json'))} (${manifest.files.length} files hashed)`);
console.log(`\nnext: node installer/build.mjs --os ${os} --arch ${arch}   (rerun)`);
console.log(`      node localdev/... or node installer/dev.mjs --prefix <dir> --lab`);

// ---------------------------------------------------------------------------------------------

function run(cmd, args, opts = {}) {
  const res = spawnSync(cmd, args, { encoding: 'utf8', ...opts, env: { ...GO_ENV, ...(opts.env ?? {}) } });
  if (res.status !== 0) {
    console.error(`\n$ ${cmd} ${args.join(' ')}\n${res.stdout ?? ''}${res.stderr ?? ''}`);
  }
  return res.status ?? 1;
}

function rel(p) {
  return p.startsWith(ROOT) ? p.slice(ROOT.length + 1) : p;
}

function sha256(p) {
  return createHash('sha256').update(readFileSync(p)).digest('hex');
}

function walk(dir) {
  const out = [];
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) out.push(...walk(p));
    else out.push(p);
  }
  return out;
}

function rmrf(dir) {
  rmSync(dir, { recursive: true, force: true });
}
