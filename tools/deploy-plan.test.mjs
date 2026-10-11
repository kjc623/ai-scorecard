import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

import { EVERYTHING, INPUTS, plan } from './deploy-plan.mjs';

const read = (rel) => readFileSync(fileURLToPath(new URL(`../${rel}`, import.meta.url)), 'utf8');
const workflow = read('.github/workflows/deploy.yml');
const APPS = Object.keys(INPUTS).filter((k) => k !== 'agent');

/** The Dockerfiles the workflow builds each app from: its `build <app> <file>` line, and the base image tagged sac-<app>. */
function dockerfiles() {
  const out = {};
  for (const m of workflow.matchAll(/^\s*build\s+([a-z-]+)\s+(\S+)/gm)) (out[m[1]] ??= []).push(m[2]);
  for (const m of workflow.matchAll(/docker build --tag "sac-([a-z-]+):\S+ --file (\S+)/g)) (out[m[1]] ??= []).push(m[2]);
  return out;
}

/** The repository paths a Dockerfile copies into an image. */
function copied(dockerfile) {
  const lines = read(dockerfile).replace(/\\\r?\n/g, ' ').split(/\r?\n/);
  return lines.filter((l) => /^(COPY|ADD)\s/.test(l)).flatMap((l) => {
    const args = l.trim().split(/\s+/).slice(1);
    if (args.some((a) => a.startsWith('--from='))) return [];
    return args.filter((a) => !a.startsWith('--')).slice(0, -1).filter((a) => !/^https?:/.test(a));
  });
}

test('the workflow builds and deploys exactly the planned apps', () => {
  assert.deepEqual(Object.keys(dockerfiles()).sort(), [...APPS].sort());
  const deployed = [...workflow.matchAll(/fly\/scripts\/deploy-app\.sh\s+([a-z-]+)/g)].map((m) => m[1]);
  assert.deepEqual([...new Set(deployed)].sort(), [...APPS].sort());
});

test("every path an app's Dockerfile copies is one of that app's inputs", () => {
  for (const [app, files] of Object.entries(dockerfiles())) {
    for (const file of files) {
      for (const path of copied(file)) {
        assert.ok(INPUTS[app].some((p) => path.startsWith(p) || `${path}/` === p), `${file} copies ${path}, which is not among ${app}'s inputs`);
      }
      assert.ok(INPUTS[app].some((p) => file.startsWith(p)), `${file} is not among ${app}'s inputs`);
    }
  }
});

test('a dashboard change deploys only the dashboard', () => {
  assert.deepEqual(plan(['services/dashboard/src/views.js']), { agent: false, apps: ['dashboard'], migrate: false });
});

test('documentation deploys nothing', () => {
  assert.deepEqual(plan(['docs/architecture.md', 'backlog/AGENTS.md', 'azure/main.bicep']), { agent: false, apps: [], migrate: false });
});

test('a device change builds the agent release and the control-api image that serves it', () => {
  assert.deepEqual(plan(['device/capture-core/main.go']), { agent: true, apps: ['control-api'], migrate: false });
});

test('device/protocol reaches every service built on it, and the agent', () => {
  const p = plan(['device/protocol/envelope.go']);
  assert.equal(p.agent, true);
  assert.deepEqual(p.apps, ['ingest-api', 'content-vault', 'control-api']);
});

test('services/platform reaches every service built on it, and migrates', () => {
  assert.deepEqual(plan(['services/platform/postgres/postgres.go']).apps, ['ingest-api', 'content-vault', 'control-api', 'jobs', 'migrate']);
  assert.equal(plan(['services/database/schema.sql']).migrate, true);
});

test('a change to how deploys work, or an unknown base, deploys everything', () => {
  for (const path of EVERYTHING) {
    assert.deepEqual(plan([path.endsWith('/') ? `${path}x.sh` : path]), { agent: true, apps: APPS, migrate: true });
  }
  assert.deepEqual(plan(null), { agent: true, apps: APPS, migrate: true });
});
