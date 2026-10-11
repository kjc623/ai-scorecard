#!/usr/bin/env node
// deploy-plan.mjs - what a deploy builds: the agent release and the Fly.io apps whose inputs
// changed since the last successful deploy.
//
//   node tools/deploy-plan.mjs --base <commit>   compare HEAD with the commit the last deploy shipped
//   node tools/deploy-plan.mjs --all             everything (a manual run)
//
// Prints the plan and, under GitHub Actions, writes agent=, apps= and migrate= to $GITHUB_OUTPUT.
// A base that is missing or is not an ancestor of HEAD plans everything, so a change is never
// left undeployed.

import { spawnSync } from 'node:child_process';
import { appendFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseArgs } from 'node:util';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');

// What each deployable is built from: the paths its Dockerfile copies (deploy-plan.test.mjs checks
// them) and its fly/ directory. The agent release is built from device/.
export const INPUTS = {
  agent: ['device/'],
  'ingest-api': ['services/ingest-api/', 'device/protocol/', 'contracts/generated/go/', 'services/platform/', 'fly/ingest-api/'],
  'content-vault': ['services/content-vault/', 'device/protocol/', 'services/platform/', 'fly/content-vault/'],
  'control-api': ['services/control-api/', 'device/protocol/', 'services/platform/', 'fly/control-api/'],
  'query-api': ['services/query-api/', 'fly/query-api/'],
  dashboard: ['services/dashboard/', 'fly/dashboard/'],
  jobs: ['services/jobs/', 'services/platform/', 'fly/jobs/'],
  edge: ['services/edge/', 'fly/edge/'],
  migrate: ['services/database/', 'services/platform/', 'fly/migrate/'],
};

// A change to how everything is built or deployed redeploys everything.
export const EVERYTHING = ['.github/workflows/deploy.yml', 'fly/scripts/', '.dockerignore', 'tools/deploy-plan.mjs'];

/** The plan for a list of changed paths, or for everything when `changed` is null. */
export function plan(changed) {
  const hit = (prefixes) => changed === null || changed.some((f) => prefixes.some((p) => (p.endsWith('/') ? f.startsWith(p) : f === p)));
  const all = hit(EVERYTHING);
  const agent = all || hit(INPUTS.agent);
  // control-api serves the agent release, so a new release ships in a new control-api image.
  const apps = Object.keys(INPUTS).filter((k) => k !== 'agent' && (all || hit(INPUTS[k]) || (k === 'control-api' && agent)));
  return { agent, apps, migrate: apps.includes('migrate') };
}

function git(args) {
  const r = spawnSync('git', args, { cwd: ROOT, encoding: 'utf8' });
  return { ok: r.status === 0, out: r.stdout ?? '' };
}

/** The paths changed between base and HEAD, or null when they cannot be known. */
export function changedSince(base) {
  if (!base || !/^[0-9a-f]{40}$/.test(base)) return null;
  if (!git(['merge-base', '--is-ancestor', base, 'HEAD']).ok) return null;
  const diff = git(['diff', '--name-only', base, 'HEAD']);
  return diff.ok ? diff.out.split('\n').filter(Boolean) : null;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const { values } = parseArgs({ options: { base: { type: 'string', default: '' }, all: { type: 'boolean', default: false } } });
  const changed = values.all ? null : changedSince(values.base);
  const p = plan(changed);
  console.log(changed === null
    ? `everything: ${values.all ? 'asked for' : `no usable base (${values.base || 'none'})`}`
    : `${changed.length} path(s) changed since ${values.base.slice(0, 12)}`);
  console.log(`agent release: ${p.agent ? 'build' : 'keep the one pre-prod serves'}`);
  console.log(`apps: ${p.apps.join(' ') || 'none'}`);
  if (process.env.GITHUB_OUTPUT) {
    appendFileSync(process.env.GITHUB_OUTPUT, `agent=${p.agent}\napps=${p.apps.join(' ')}\nmigrate=${p.migrate}\n`);
  }
}
