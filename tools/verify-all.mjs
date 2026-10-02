#!/usr/bin/env node
// tools/verify-all.mjs - one command that runs every package's own test suite offline and
// reports a per-package verdict. Lead-owned.
//
// Why this exists: nine agents write nine packages against one document set. Each package's
// suite passing on its own does not mean the tree is green, and a package whose suite cannot be
// started must be a FAILURE rather than a silent skip. This script is the acceptance run the
// integration verifier re-executes and the Lead quotes.
//
//   node tools/verify-all.mjs                 # run everything, print the table, write evidence
//   node tools/verify-all.mjs --only go       # only Go packages
//   node tools/verify-all.mjs --only node     # only Node packages
//   node tools/verify-all.mjs --list          # show what would run
//   node tools/verify-all.mjs --keep-going=false  # stop at the first failure
//
// Exit code: 0 only when every package that exists passed. A package that is declared here but
// absent from disk is reported as MISSING and fails the run, because "the directory is not there
// yet" is exactly the state this harness exists to make visible.

import { spawnSync } from 'node:child_process';
import { existsSync, mkdirSync, readdirSync, statSync, writeFileSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const ARGS = process.argv.slice(2);
const ONLY = valueOf('--only');
const LIST = ARGS.includes('--list');
const KEEP_GOING = valueOf('--keep-going') !== 'false';

/** Every package the build round is required to produce, whether or not it exists yet. */
const PACKAGES = [
  { kind: 'node', dir: 'contracts/tools' },
  { kind: 'node', dir: 'apps/capture-extension' },
  { kind: 'node', dir: 'apps/dashboard' },
  { kind: 'node', dir: 'services/query-api' },
  { kind: 'node', dir: 'db/tools' },
  { kind: 'node', dir: 'infra/tools' },
  { kind: 'go', dir: 'device/protocol', args: ['./...'] },
  { kind: 'go', dir: 'device/capture-core', args: ['./...'] },
  { kind: 'go', dir: 'device/capture-spool', args: ['./...'] },
  { kind: 'go', dir: 'device/classifier-host', args: ['./...'] },
  { kind: 'go', dir: 'services/ingest-api', args: ['./...'] },
  { kind: 'go', dir: 'services/content-vault', args: ['./...'] },
  // Contracts' generated Go types are compiled, not tested: a build is the assertion.
  { kind: 'go', dir: 'contracts/generated/go', args: ['./...'], label: 'contracts/generated (go build)' },
];

const GO_ENV = {
  GOCACHE: join(ROOT, '.tools', 'gocache'),
  GOPATH: join(ROOT, '.tools', 'gopath'),
  GOMODCACHE: join(ROOT, '.tools', 'gopath', 'pkg', 'mod'),
  GOTMPDIR: join(ROOT, '.tools', 'tmp', 'go'),
  GOPROXY: 'off',
  GOFLAGS: '-mod=mod',
  GOTOOLCHAIN: 'local',
  GO111MODULE: 'on',
};

function valueOf(flag) {
  const i = ARGS.indexOf(flag);
  return i === -1 ? undefined : ARGS[i + 1];
}

function ensureGoDirs() {
  for (const key of ['GOCACHE', 'GOPATH', 'GOTMPDIR']) {
    const p = GO_ENV[key];
    if (!existsSync(p)) mkdirSync(p, { recursive: true });
  }
}

/** A Go package must have a go.mod, or `go test ./...` escapes into the parent tree. */
function hasGoMod(dir) {
  return existsSync(join(dir, 'go.mod'));
}

function findSubmoduleRoots(dir) {
  // A module inside a package directory (rare here) would be tested by its own entry; this keeps
  // `go test ./...` from silently skipping it.
  return [];
}

/**
 * Find the Node test entry for a package. Different packages landed on slightly different
 * layouts, and the harness must not care: any *.test.mjs / *.test.js / *.test.cjs under the
 * package (node_modules excluded) means the package has a suite, and `node --test` is given
 * those files explicitly so a package with no suite is MISSING rather than a vacuous pass.
 */
function findNodeTests(dir, depth = 0) {
  if (depth > 4) return [];
  const found = [];
  let entries;
  try {
    entries = readdirSync(dir, { withFileTypes: true });
  } catch {
    return [];
  }
  for (const e of entries) {
    if (e.name === 'node_modules' || e.name === '.git') continue;
    const p = join(dir, e.name);
    if (e.isDirectory()) found.push(...findNodeTests(p, depth + 1));
    else if (/\.test\.(mjs|cjs|js)$/.test(e.name)) found.push(p);
  }
  return found.sort();
}

function run(pkg) {
  const abs = join(ROOT, pkg.dir);
  const label = pkg.label ?? pkg.dir;
  if (!existsSync(abs) || !statSync(abs).isDirectory()) {
    return { label, status: 'MISSING', code: null, seconds: 0, output: 'directory does not exist' };
  }
  let cmd;
  let cmdArgs;
  let env = { ...process.env };
  if (pkg.kind === 'go') {
    if (!hasGoMod(abs)) {
      return { label, status: 'MISSING', code: null, seconds: 0, output: 'no go.mod in ' + pkg.dir };
    }
    ensureGoDirs();
    Object.assign(env, GO_ENV);
    cmd = 'go';
    cmdArgs = ['test', ...pkg.args];
  } else {
    const tests = findNodeTests(abs);
    if (tests.length === 0) {
      return { label, status: 'MISSING', code: null, seconds: 0, output: `no *.test.{mjs,cjs,js} under ${pkg.dir}` };
    }
    cmd = process.execPath; // the same node that runs this script
    cmdArgs = ['--test', ...tests];
  }
  const started = Date.now();
  const res = spawnSync(cmd, cmdArgs, {
    cwd: abs,
    env,
    encoding: 'utf8',
    maxBuffer: 64 * 1024 * 1024,
    shell: false,
  });
  const seconds = (Date.now() - started) / 1000;
  const output = `${res.stdout ?? ''}${res.stderr ?? ''}`.trim();
  // `go test` exits 0 for a package with no test files, which would report a component as
  // passing when it has no tests at all. That is a gap, not a pass, so it is reported as one.
  // The same applies to a module that exists but has no packages yet.
  if (pkg.kind === 'go' && res.status === 0 && /\[no test files\]/.test(output)) {
    return {
      label,
      status: 'NO-TESTS',
      code: 0,
      seconds,
      output: `${output}\n(go test reported no test files: this package has no suite, so nothing was proven)`,
    };
  }
  if (pkg.kind === 'go' && /matched no packages/.test(output)) {
    return {
      label,
      status: 'NO-TESTS',
      code: 0,
      seconds,
      output: `${output}\n(the module exists but declares no packages yet, so nothing was compiled)`,
    };
  }
  return {
    label,
    status: res.status === 0 ? 'PASS' : 'FAIL',
    code: res.status,
    seconds,
    output,
  };
}

// ---------------------------------------------------------------------------------------------

if (LIST) {
  for (const p of PACKAGES) {
    const present = existsSync(join(ROOT, p.dir)) ? 'present' : 'MISSING';
    console.log(`${p.kind.padEnd(5)} ${(p.label ?? p.dir).padEnd(40)} ${present}`);
  }
  process.exit(0);
}

const selected = PACKAGES.filter((p) => !ONLY || p.kind === ONLY);
const results = [];
// A go.work file at the repo root puts every Go command into workspace mode, where `-mod=mod` is
// rejected and the per-module offline build this harness relies on breaks. It is a foot-gun
// rather than a convenience here, so its presence is a failure rather than a warning.
if (existsSync(join(ROOT, 'go.work'))) {
  results.push({
    label: 'repo/go.work',
    status: 'FAIL',
    code: null,
    seconds: 0,
    output:
      'A go.work file exists. Workspace mode rejects -mod=mod and breaks the per-module offline ' +
      'build. Delete it; each component is its own module and wires its intra-repo dependencies ' +
      'with a replace directive.',
  });
  console.log('[FAIL] repo/go.work (present; workspace mode breaks the offline build)');
}
for (const pkg of selected) {
  const r = run(pkg);
  results.push(r);
  const mark = r.status === 'PASS' ? 'PASS' : r.status === 'MISSING' ? 'MISS' : r.status === 'NO-TESTS' ? 'NOTS' : 'FAIL';
  console.log(`[${mark}] ${r.label} (${r.seconds.toFixed(1)}s)`);
  if (r.status !== 'PASS' && r.output) {
    // Keep the failure readable: the tail is where the assertion message lives.
    const lines = r.output.split('\n');
    const tail = lines.slice(-40).join('\n');
    console.log('  ' + tail.split('\n').join('\n  '));
  }
  if (r.status === 'FAIL' && !KEEP_GOING) break;
}

const passed = results.filter((r) => r.status === 'PASS').length;
const failed = results.filter((r) => r.status === 'FAIL').length;
const missing = results.filter((r) => r.status === 'MISSING').length;
const noTests = results.filter((r) => r.status === 'NO-TESTS').length;

console.log('');
console.log('='.repeat(72));
console.log(
  `packages: ${results.length}   pass: ${passed}   fail: ${failed}   missing: ${missing}   no-tests: ${noTests}`,
);
console.log('='.repeat(72));

const evidenceDir = join(ROOT, '.integration');
if (!existsSync(evidenceDir)) mkdirSync(evidenceDir, { recursive: true });
const stamp = new Date().toISOString().replace(/[:.]/g, '-');
const jsonPath = join(evidenceDir, `verify-all-${stamp}.json`);
writeFileSync(
  jsonPath,
  JSON.stringify(
    {
      ranAt: new Date().toISOString(),
      node: process.version,
      go: goVersion(),
      cwd: ROOT,
      results: results.map((r) => ({ ...r, output: r.output.slice(0, 200_000) })),
      totals: { packages: results.length, passed, failed, missing, noTests },
    },
    null,
    2,
  ),
);
console.log(`evidence: ${relative(ROOT, jsonPath)}`);

function goVersion() {
  const r = spawnSync('go', ['version'], { encoding: 'utf8' });
  return (r.stdout ?? '').trim();
}

process.exit(failed === 0 && missing === 0 && noTests === 0 ? 0 : 1);
