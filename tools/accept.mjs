#!/usr/bin/env node
// accept.mjs — the repository's acceptance gate: one command, one exit code.
//
//   node tools/accept.mjs                  every gate that can run on this host
//   node tools/accept.mjs --only go,node   just those gates
//   node tools/accept.mjs --skip browser   everything but those
//   node tools/accept.mjs --list           the gates and what a failure means
//   node tools/accept.mjs --only go --race run each Go module's tests under -race
//
// A gate that cannot run here (no Docker, no az, not Windows, no Chromium) is reported SKIPPED with
// its reason. SKIPPED is not a pass: the summary says what was not checked.

import { spawnSync } from 'node:child_process';
import { existsSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const SKIPPED = 3; // the exit code a gate script uses to say it could not run here

const args = process.argv.slice(2);
const listArg = (flag) => (args.includes(flag) ? args[args.indexOf(flag) + 1]?.split(',') ?? [] : null);
const only = listArg('--only');
const skip = new Set(listArg('--skip') ?? []);
// --race runs each Go module's tests under the race detector. It needs cgo (a C compiler), so it
// is used on the hosted Ubuntu runner and inside a golang container, not on a plain Windows box.
const race = args.includes('--race');

function run(cmd, cmdArgs, cwd = ROOT) {
  const r = spawnSync(cmd, cmdArgs, { cwd, encoding: 'utf8', shell: process.platform === 'win32', maxBuffer: 64 << 20 });
  return { code: r.status ?? 1, output: `${r.stdout ?? ''}${r.stderr ?? ''}${r.error ? String(r.error) : ''}` };
}

const tracked = (pattern) => run('git', ['ls-files', '--cached', '--others', '--exclude-standard', '--', pattern]).output
  .split(/\r?\n/).filter((p) => p && existsSync(join(ROOT, p)));
const has = (cmd) => run(cmd, ['--version']).code === 0;

/** Runs steps in order; the first failure fails the gate, a SKIPPED exit skips it. */
function steps(list) {
  for (const [label, cmd, cmdArgs, cwd] of list) {
    const r = run(cmd, cmdArgs, cwd);
    if (r.code === SKIPPED) return { status: 'SKIPPED', detail: `${label}: ${r.output.trim().split('\n').pop()}` };
    if (r.code !== 0) return { status: 'FAIL', detail: `${label}\n${r.output.trim().split('\n').slice(-40).join('\n')}` };
  }
  return { status: 'PASS', detail: `${list.length} step(s)` };
}

const GATES = [
  {
    id: 'go',
    decides: 'A Go module fails go vet or its tests.',
    run: () => steps(tracked('*go.mod').map((mod) => dirname(mod)).flatMap((dir) => [
      [`${dir}: go vet`, 'go', ['vet', './...'], join(ROOT, dir)],
      [`${dir}: go test`, 'go', ['test', ...(race ? ['-race'] : []), './...'], join(ROOT, dir)],
    ])),
  },
  {
    id: 'node',
    decides: 'A Node package\'s test suite fails.',
    run: () => steps(tracked('*package.json').filter((p) => {
      const pkg = JSON.parse(readFileSync(join(ROOT, p), 'utf8'));
      return pkg.scripts?.test && !p.includes('node_modules');
    }).flatMap((p) => {
      const dir = join(ROOT, dirname(p));
      const install = existsSync(join(dir, 'package-lock.json')) && !existsSync(join(dir, 'node_modules'))
        ? [[`${dirname(p)}: npm ci`, 'npm', ['ci', '--no-audit', '--no-fund'], dir]] : [];
      return [...install, [`${dirname(p)}: npm test`, 'npm', ['test'], dir]];
    })),
  },
  {
    id: 'static',
    decides: 'A cross-component check fails: contract drift, vocabulary disagreement, a broken structural invariant, deployment configuration disagreement, or a schema or infrastructure property regression.',
    run: () => steps([
      ...tracked('tools/*.test.mjs').concat(tracked('azure/tools/*.test.mjs'), tracked('services/database/tools/*.test.mjs'), tracked('contracts/tools/*.test.mjs'))
        .map((t) => [t, 'node', ['--test', t]]),
      ['contracts', 'node', ['contracts/tools/verify.mjs']],
      ['vocabularies', 'node', ['tools/check-vocab.mjs']],
      ['invariants', 'node', ['tools/check-invariants.mjs']],
      ['configuration', 'node', ['tools/check-config.mjs']],
      ['schema', 'node', ['services/database/tools/check-schema.mjs']],
      ['infrastructure', 'node', ['azure/tools/check-infra.mjs']],
    ]),
  },
  {
    id: 'bicep',
    decides: 'azure/main.bicep or a parameter file does not compile cleanly.',
    run: () => {
      if (!has('az')) return { status: 'SKIPPED', detail: 'the az CLI is not installed' };
      const env = { SAC_DEVICE_FQDN: 'device.example.com', SAC_ANALYST_FQDN: 'app.example.com', SAC_ENTRA_APP_CLIENT_ID: '00000000-0000-0000-0000-000000000001', SAC_ALERT_EMAILS: 'ops@example.com' };
      Object.assign(process.env, env);
      const build = run('az', ['bicep', 'build', '--file', 'azure/main.bicep', '--stdout']);
      const warnings = build.output.split(/\r?\n/).filter((l) => /(Warning|Error) [A-Z-]/.test(l));
      if (build.code !== 0 || warnings.length) return { status: 'FAIL', detail: warnings.join('\n') || build.output.slice(-2000) };
      return steps(tracked('azure/params/*.bicepparam').map((p) => [p, 'az', ['bicep', 'build-params', '--file', p, '--stdout']]));
    },
  },
  {
    id: 'database',
    decides: 'The schema does not apply as a non-superuser on PostgreSQL 16, or its invariants fail.',
    run: () => steps([['database gate', 'node', ['services/database/tools/test-database.mjs']]]),
  },
  {
    id: 'installer',
    decides: 'The Windows installer build or its static checks are broken.',
    run: () => process.platform === 'win32'
      ? steps([['installer', 'node', ['device/installer/verify.mjs']]])
      : { status: 'SKIPPED', detail: 'the MSI is built and checked on Windows' },
  },
  {
    id: 'browser',
    decides: 'The extension does not load or observe requests in a real Chromium.',
    run: () => steps([['browser', 'node', ['device/extension/tools/in-browser-check.mjs']]]),
  },
];

if (args.includes('--list')) {
  for (const g of GATES) console.log(`${g.id.padEnd(10)} ${g.decides}`);
  process.exit(0);
}

const results = [];
for (const gate of GATES) {
  if ((only && !only.includes(gate.id)) || skip.has(gate.id)) {
    results.push({ id: gate.id, status: 'SKIPPED', detail: 'not selected' });
    continue;
  }
  const started = Date.now();
  process.stdout.write(`${gate.id} … `);
  const r = gate.run();
  results.push({ id: gate.id, ...r });
  console.log(`${r.status} (${((Date.now() - started) / 1000).toFixed(1)}s)`);
  if (r.status !== 'PASS') console.log(r.detail.replace(/^/gm, '    '));
}

const failed = results.filter((r) => r.status === 'FAIL');
const skipped = results.filter((r) => r.status === 'SKIPPED');
console.log(`\n${results.length - failed.length - skipped.length} passed, ${failed.length} failed, ${skipped.length} skipped` +
  (skipped.length ? ` (not checked: ${skipped.map((r) => r.id).join(', ')})` : ''));
process.exit(failed.length ? 1 : 0);
