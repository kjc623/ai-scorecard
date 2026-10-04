#!/usr/bin/env node
// tools/accept.mjs - the one release-acceptance command for this repository.
//
// `verify-all.mjs` answers "does every package's own suite pass". That is necessary and not
// sufficient: a package can be green while the contract it consumes has drifted, while a seam
// field name disagrees with the contract, or while the database's assertions fail. This runs the
// independent gates in one place and gives a single verdict, so "the build is green" is one
// command with one exit code rather than four results someone has to hold in their head.
//
//   node tools/accept.mjs                 # every gate that can run on this host
//   node tools/accept.mjs --skip db       # skip a gate by id
//   node tools/accept.mjs --list          # show the gates and what decides each one
//
// A gate that cannot run is reported as SKIPPED with the reason, never as a pass. On a host with
// no Docker and no PostgreSQL, the database gate is SKIPPED and the overall verdict says so -
// which is the honest outcome, and the opposite of a green tick nobody can defend.

import { spawnSync } from 'node:child_process';
import { existsSync, mkdirSync, writeFileSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const ARGS = process.argv.slice(2);

function valueOf(flag) {
  const i = ARGS.indexOf(flag);
  return i === -1 ? undefined : ARGS[i + 1];
}
const SKIP = new Set(
  ARGS.filter((_, i) => ARGS[i - 1] === '--skip').flatMap((v) => String(v).split(',')),
);
const LIST = ARGS.includes('--list');

const GO_ENV = {
  GOCACHE: join(ROOT, '.tools', 'gocache'),
  GOPATH: join(ROOT, '.tools', 'gopath'),
  GOMODCACHE: join(ROOT, '.tools', 'gopath', 'pkg', 'mod'),
  GOTMPDIR: join(ROOT, '.tools', 'tmp', 'go'),
  GOPROXY: 'off',
  GOFLAGS: '-mod=mod',
  GOTOOLCHAIN: 'local',
};

function dockerUp() {
  const r = spawnSync('docker', ['info', '--format', '{{.ServerVersion}}'], { encoding: 'utf8' });
  return r.status === 0 && /\d/.test(r.stdout ?? '');
}

/**
 * The gates. `decides` is what a failure means, in words, because a gate whose failure mode is
 * unclear is a gate people route around.
 */
const GATES = [
  {
    id: 'packages',
    title: 'Every package suite (tools/verify-all.mjs)',
    decides: 'A component is broken or has no suite at all (NO-TESTS is a failure, not a pass).',
    run: () => runNode(['tools/verify-all.mjs']),
  },
  {
    id: 'contract',
    title: 'Contract codegen drift (contracts/tools, --check)',
    decides: 'contracts/generated no longer matches contracts/event-envelope.schema.json.',
    run: () => {
      if (!existsSync(join(ROOT, 'contracts', 'tools', 'generate.mjs'))) {
        return { status: 'SKIPPED', why: 'contracts/tools/generate.mjs does not exist yet' };
      }
      const r = runNode(['contracts/tools/generate.mjs', '--check']);
      return r.status === 'PASS'
        ? { status: 'PASS', detail: r.output.trim().split('\n').slice(-3).join('\n') }
        : { status: 'FAIL', detail: r.detail || r.output };
    },
  },
  {
    id: 'seams',
    title: 'Cross-component field check (tools/check-seams.mjs)',
    decides: 'A component names an envelope field the contract does not define, or names a server-assigned field on the device side.',
    run: () => runNode(['tools/check-seams.mjs']),
  },
  {
    id: 'vocab',
    title: 'Shared vocabulary drift (tools/check-vocab.mjs)',
    decides: 'Two components spell the same closed enum differently, which compiles on both sides and fails only on the rejection path.',
    run: () => runNode(['tools/check-vocab.mjs']),
  },
  {
    id: 'installer',
    title: 'The development installer agrees with the agent, the schema and itself (installer/verify.mjs)',
    decides:
      'A configuration variable maps to a capture-core flag that does not exist, the Windows/macOS/Linux renderings disagree about the flag set, the committed generated output drifted from installer/manifest.mjs, or the flags the installer emits are rejected by the real binary. The generated output is checked for drift the way the wire contract is.',
    run: () => {
      if (!existsSync(join(ROOT, 'installer', 'manifest.mjs'))) {
        return { status: 'SKIPPED', why: 'installer/manifest.mjs does not exist yet' };
      }
      return runNode(['installer/verify.mjs']);
    },
  },
  {
    id: 'invariants',
    title: 'The six declared invariants, plus the mechanical half of INV-3 and a query-DSL runtime probe',
    decides:
      'A structural property the architecture relies on is violated, or a component that owns one no longer exists. The checker reports seven rows because INV-3 is checked twice: once as "the dashboard contains no SQL" and once as "the DSL binds every value as a parameter", which are different claims about the same invariant.',
    run: () => runNode(['tools/check-invariants.mjs']),
  },
  {
    id: 'endpoint',
    title: 'The endpoint agent runs as a process (capture-core --selftest)',
    decides:
      'The assembled endpoint does not run: the §3.5 startup/shutdown order, the native-messaging host, the spool, M0 handling or the coverage rows are broken when wired together. Every other gate tests components; this one tests the binary.',
    run: () => {
      const dir = join(ROOT, 'endpoint', 'capture-core');
      const exe = join(dir, 'bin', process.platform === 'win32' ? 'capture-core.exe' : 'capture-core');
      if (!existsSync(join(dir, 'cmd', 'capture-core'))) {
        return { status: 'SKIPPED', why: 'endpoint/capture-core/cmd/capture-core does not exist yet' };
      }
      const env = {
        ...process.env,
        ...GO_ENV,
        TMP: join(ROOT, '.testtmp'),
        TEMP: join(ROOT, '.testtmp'),
      };
      if (!existsSync(join(ROOT, '.tools', 'tmp', 'go'))) mkdirSync(join(ROOT, '.tools', 'tmp', 'go'), { recursive: true });
      if (!existsSync(join(ROOT, '.testtmp'))) mkdirSync(join(ROOT, '.testtmp'), { recursive: true });

      const build = spawnSync('go', ['build', '-o', exe, './cmd/capture-core'], {
        cwd: dir,
        env,
        encoding: 'utf8',
        maxBuffer: 32 * 1024 * 1024,
        timeout: 240_000,
      });
      if (build.status !== 0) {
        return { status: 'FAIL', detail: `the endpoint binary does not build:\n${tail(`${build.stdout ?? ''}${build.stderr ?? ''}`)}` };
      }
      // The selftest mints its own signed bundle, opens its own spool, uses ephemeral ports and
      // removes its own work directory, so it is safe to run from a gate. It is the only test in
      // this repository that exercises the assembled endpoint rather than a component.
      const run = spawnSync(exe, ['--selftest'], {
        cwd: dir,
        env,
        encoding: 'utf8',
        maxBuffer: 32 * 1024 * 1024,
        timeout: 300_000,
      });
      const out = `${run.stdout ?? ''}${run.stderr ?? ''}`;
      if (run.status !== 0) {
        return { status: 'FAIL', detail: `capture-core --selftest exited ${run.status}:\n${tail(out, 40)}` };
      }
      const passed = (out.match(/\[PASS\]/g) ?? []).length;
      const failed = (out.match(/\[FAIL\]/g) ?? []).length;
      return {
        status: failed === 0 ? 'PASS' : 'FAIL',
        detail: `the endpoint binary ran its own selftest: ${passed} assertion(s), ${failed} failure(s). Covers the §3.5 order, the native-messaging host in two child processes, the spool, M0, and the coverage rows.`,
      };
    },
  },
  {
    id: 'browser',
    title: 'The browser half in a real Chromium, in both native-channel modes (extension/tools/in-browser-check.mjs)',
    decides:
      'The extension does not load as a browser extension, its webRequest listener never fires, M0 reads content it should not, a registered native host never connects, a real user-selected File is not read, or an absent native channel breaks the page instead of degrading. This is the only gate that runs the extension inside a browser.',
    run: () => {
      const script = join(ROOT, 'extension', 'tools', 'in-browser-check.mjs');
      if (!existsSync(script)) return { status: 'SKIPPED', why: 'the in-browser check does not exist yet' };
      // The check needs a Chromium-family browser that will load an unpacked extension. Edge does;
      // Google Chrome Stable refuses --load-extension outright (verified), so this looks for either
      // and reports which one it used.
      const candidates = [
        'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
        'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
        'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
      ];
      const browser = candidates.find((p) => existsSync(p));
      if (!browser) {
        return { status: 'SKIPPED', why: 'no Chromium-family browser found at the usual Windows paths' };
      }
      const cwd = join(ROOT, 'extension');
      const runMode = (args) =>
        spawnSync(process.execPath, [script, ...args], {
          cwd,
          env: { ...process.env, SAC_BROWSER: browser },
          encoding: 'utf8',
          maxBuffer: 32 * 1024 * 1024,
          timeout: 600_000,
        });
      // Two modes, because they assert opposite halves of the same requirement and neither is
      // sufficient alone. With no host registered, §3.4/INV-6 requires `absent` + `degraded` and a
      // page that still works. With a host registered *before launch* (a host registered afterwards
      // is invisible to the running browser), the same requirement is `connected`, and only then can
      // the real §7.3 attachment path be driven end to end. The connected mode registers the host
      // itself and removes it again.
      const modes = [
        { name: 'absent host', args: [], counts: ['4', '2'] },
        { name: 'connected host', args: ['--with-native-host'], counts: ['6', '1'] },
      ];
      const results = [];
      for (const mode of modes) {
        const r = runMode(mode.args);
        const out = `${r.stdout ?? ''}${r.stderr ?? ''}`;
        if (r.status !== 0) {
          return {
            status: 'FAIL',
            detail: `the in-browser check failed in the "${mode.name}" mode (exit ${r.status}):\n${tail(out, 30)}`,
          };
        }
        // Count summary rows only: the same strings appear in the per-check detail and in the
        // "NOT VERIFIED BY THIS RUN" list, so a plain match over-counts.
        const pass = (out.match(/^\s*PASS\s+check/gm) ?? []).length;
        const notObservable = (out.match(/^\s*NOT-OBSERVABLE\s+check/gm) ?? []).length;
        const fail = (out.match(/^\s*FAIL\s+check/gm) ?? []).length;
        if (fail > 0) {
          return { status: 'FAIL', detail: `the "${mode.name}" mode reported ${fail} failing check(s):\n${tail(out, 30)}` };
        }
        // The expected shape of each mode, so a mode that silently stops running half its checks is
        // a failure rather than a smaller number nobody reads.
        const [wantPass, wantNotObservable] = mode.counts;
        if (String(pass) !== wantPass || String(notObservable) !== wantNotObservable) {
          return {
            status: 'FAIL',
            detail: `the "${mode.name}" mode reported ${pass} pass / ${notObservable} not-observable, expected ${wantPass} / ${wantNotObservable}:\n${tail(out, 30)}`,
          };
        }
        results.push({ mode: mode.name, pass, notObservable });
      }
      const pass = results.reduce((n, r) => n + r.pass, 0);
      const notObservable = results.reduce((n, r) => n + r.notObservable, 0);
      return {
        status: 'PASS',
        detail: `${pass} check(s) passed in a real browser across two modes (${results.map((r) => `${r.mode}: ${r.pass} pass`).join(', ')}), ${notObservable} honestly not observable (blocking needs a policy install). Browser: ${browser.split('\\\\').pop()}.`,
      };
    },
  },
  {
    id: 'db',
    title: 'Database invariants against a real server (database/tools/run-invariants.ps1)',
    decides: 'The schema asserts its own properties, as the runtime roles, on PostgreSQL.',
    run: () => {
      const script = join(ROOT, 'database', 'tools', 'run-invariants.ps1');
      if (!existsSync(script)) return { status: 'SKIPPED', why: 'database/tools/run-invariants.ps1 does not exist yet' };
      if (!dockerUp()) return { status: 'SKIPPED', why: 'no Docker daemon; a real server needs one on this host' };
      const r = spawnSync(
        'powershell',
        ['-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', script, '-KeepContainer'],
        { cwd: ROOT, encoding: 'utf8', maxBuffer: 32 * 1024 * 1024 },
      );
      const out = `${r.stdout ?? ''}${r.stderr ?? ''}`;
      // The runner's own exit code is not trustworthy while its error counter greps for the
      // substring ERROR (it matches ON_ERROR_STOP in the echoed command). The tally line is:
      // parse the assertion counts and judge on those, and say so rather than trusting the code.
      const tally = /TALLY pass=(\d+) fail=(\d+) .*distinct_ids=(\d+)/.exec(out);
      if (!tally) return { status: 'FAIL', detail: `no TALLY line in the runner output:\n${tail(out)}` };
      const [, pass, fail, distinct] = tally;
      if (Number(fail) > 0) {
        return { status: 'FAIL', detail: `assertions failed: pass=${pass} fail=${fail}\n${tail(out)}` };
      }
      return {
        status: 'PASS',
        detail: `${distinct} distinct assertions, ${pass} PASS notices, ${fail} failures (judged on the tally, not the runner's exit code - see .integration/LEAD-VERIFICATION.md L3)`,
      };
    },
  },
];

function runNode(args) {
  const r = spawnSync(process.execPath, args, {
    cwd: ROOT,
    encoding: 'utf8',
    maxBuffer: 64 * 1024 * 1024,
    env: { ...process.env, ...GO_ENV },
  });
  const output = `${r.stdout ?? ''}${r.stderr ?? ''}`;
  // Always hand back the output: a gate that fails must be able to show why, and an earlier
  // version of this file dropped it on the failure path, which produced a FAIL with no detail.
  return r.status === 0
    ? { status: 'PASS', output, detail: '' }
    : { status: 'FAIL', output, detail: tail(output) };
}

function tail(text, lines = 25) {
  return text.trim().split('\n').slice(-lines).join('\n');
}

if (LIST) {
  for (const g of GATES) console.log(`${g.id.padEnd(10)} ${g.title}\n           fails when: ${g.decides}`);
  process.exit(0);
}

const results = [];
for (const gate of GATES) {
  if (SKIP.has(gate.id)) {
    results.push({ id: gate.id, title: gate.title, status: 'SKIPPED', detail: 'skipped by --skip' });
    console.log(`[SKIP] ${gate.id} - ${gate.title}`);
    continue;
  }
  const started = Date.now();
  const r = gate.run();
  const seconds = ((Date.now() - started) / 1000).toFixed(1);
  results.push({ id: gate.id, title: gate.title, ...r, seconds: Number(seconds) });
  const mark = r.status === 'PASS' ? 'PASS' : r.status === 'SKIPPED' ? 'SKIP' : 'FAIL';
  console.log(`[${mark}] ${gate.id} (${seconds}s) - ${gate.title}`);
  if (r.detail) console.log(r.detail.split('\n').map((l) => '    ' + l).join('\n'));
  if (r.status === 'SKIPPED' && r.why) console.log(`    reason: ${r.why}`);
}

const failed = results.filter((r) => r.status === 'FAIL');
const skipped = results.filter((r) => r.status === 'SKIPPED');
const passed = results.filter((r) => r.status === 'PASS');

console.log('');
console.log('='.repeat(72));
console.log(`gates: ${results.length}   pass: ${passed.length}   fail: ${failed.length}   skipped: ${skipped.length}`);
console.log('='.repeat(72));
if (skipped.length > 0) {
  console.log('Skipped gates are NOT evidence:');
  for (const s of skipped) console.log(`  - ${s.id}: ${s.why ?? s.detail}`);
}
console.log(
  failed.length === 0 && skipped.length === 0
    ? 'VERDICT: every gate passed. This is the acceptance run.'
    : failed.length > 0
      ? 'VERDICT: FAILED - see the gate output above.'
      : 'VERDICT: all runnable gates passed, but this is NOT a complete acceptance: gates above were skipped.',
);

const dir = join(ROOT, '.integration');
if (!existsSync(dir)) mkdirSync(dir, { recursive: true });
const stamp = new Date().toISOString().replace(/[:.]/g, '-');
const out = join(dir, `accept-${stamp}.json`);
writeFileSync(out, JSON.stringify({ ranAt: new Date().toISOString(), results }, null, 2));
console.log(`evidence: ${relative(ROOT, out)}`);

process.exit(failed.length === 0 ? 0 : 1);
