import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

import { check, checkFly, namesSet, readFly, readFlyToml, scriptSecrets, workflowEnv } from './check-config.mjs';

const bicep = readFileSync(fileURLToPath(new URL('../azure/main.bicep', import.meta.url)), 'utf8');

test('the deployment and the code agree', () => {
  assert.deepEqual(check(bicep).problems, []);
});

test('every workload is found with its database settings', () => {
  const set = namesSet(bicep);
  for (const workload of ['ingestApp', 'controlApp', 'vaultApp', 'queryApp', 'aggregate', 'expire', 'migrate', 'tenant-admin']) {
    assert.ok(set[workload]?.includes('SAC_PG_HOST'), `${workload} has no SAC_PG_HOST`);
    assert.ok(set[workload]?.includes('SAC_PG_USER'), `${workload} has no SAC_PG_USER`);
  }
  assert.ok(set.migrate.includes('SAC_DB_LOGINS'));
  assert.ok(set.dashboardApp.includes('SAC_INTERNAL_TOKEN'));
});

test('a variable no component reads is reported', () => {
  const mutated = bicep.replace("{ name: 'SAC_PG_USER', value: 'ingest-api' }", "{ name: 'SAC_PG_USR', value: 'ingest-api' }");
  assert.notEqual(mutated, bicep);
  assert.ok(check(mutated).problems.some((p) => p.includes('SAC_PG_USR')));
});

test('a secret wired to an unread name is reported', () => {
  const mutated = bicep.replace("{ name: 'SAC_CURSOR_KEY', secret: 'sac-cursor-key' }", "{ name: 'SAC_CURSOR_SECRET', secret: 'sac-cursor-key' }");
  assert.notEqual(mutated, bicep);
  assert.ok(check(mutated).problems.some((p) => p.includes('SAC_CURSOR_SECRET')));
});

test('the Fly.io deployment and the code agree', () => {
  assert.deepEqual(checkFly().problems, []);
});

test('every Fly.io app is read with its settings and secrets', () => {
  const fly = readFly();
  assert.deepEqual(Object.keys(fly.tomls).sort(), ['content-vault', 'control-api', 'dashboard', 'edge', 'ingest-api', 'jobs', 'migrate', 'query-api']);
  const control = readFlyToml(fly.tomls['control-api']);
  assert.equal(control.app, 'sac-preprod-control-api');
  assert.equal(control.env.SAC_HTTP_ADDR, '[::]:8080');
  assert.deepEqual(control.files.map((f) => f.secret_name), ['SESSION_SIGNING_KEY', 'POLICY_SIGNING_KEY']);
  assert.equal(JSON.parse(readFlyToml(fly.tomls.migrate).env.SAC_DB_LOGINS).length, 5);
  assert.ok(workflowEnv(fly.workflow)['control-api'].includes('SAC_PUBLIC_DEVICE_ENDPOINT'));
  const secrets = scriptSecrets(fly.scripts);
  assert.ok(secrets['control-api'].includes('SAC_CA_KEY_PEM'));
  assert.ok(secrets.migrate.includes('SAC_PG_PASSWORD'));
  assert.ok(secrets.edge.includes('EDGE_TLS_KEY_PEM'));
});

/** fly/ with one file's text changed; the change must apply. */
function mutated(kind, name, from, to) {
  const fly = readFly();
  const before = name ? fly[kind][name] : fly[kind];
  const after = before.replace(from, to);
  assert.notEqual(after, before);
  if (name) fly[kind][name] = after;
  else fly[kind] = after;
  return fly;
}

test('a Fly.io setting no component reads is reported', () => {
  const fly = mutated('tomls', 'ingest-api', 'SAC_PG_SSLMODE = ', 'SAC_PG_SSL_MODE = ');
  assert.ok(checkFly(fly).problems.some((p) => p.includes('SAC_PG_SSL_MODE')));
});

test('a value the deploy workflow passes that no component reads is reported', () => {
  const fly = mutated('workflow', null, '-e "SAC_PUBLIC_DEVICE_ENDPOINT=', '-e "SAC_DEVICE_ENDPOINT=');
  assert.ok(checkFly(fly).problems.some((p) => p.includes('SAC_DEVICE_ENDPOINT')));
});

test('a secret a script sets that no component reads is reported', () => {
  const fly = mutated('scripts', 'create-secrets.sh', 'put SAC_CURSOR_KEY ', 'put SAC_CURSOR_SECRET ');
  assert.ok(checkFly(fly).problems.some((p) => p.includes('SAC_CURSOR_SECRET')));
});

test('a file whose secret no script sets is reported', () => {
  const fly = mutated('tomls', 'control-api', 'secret_name = "POLICY_SIGNING_KEY"', 'secret_name = "POLICY_KEY"');
  assert.ok(checkFly(fly).problems.some((p) => p.includes('the secret POLICY_KEY,')));
});

test('a private address that names no app is reported', () => {
  const fly = mutated('tomls', 'dashboard', 'http://sac-preprod-query-api.internal', 'http://sac-preprod-queryapi.internal');
  assert.ok(checkFly(fly).problems.some((p) => p.includes('sac-preprod-queryapi.internal')));
});

test('an app outside the shared prefix is reported', () => {
  const fly = mutated('tomls', 'jobs', 'app = "sac-preprod-jobs"', 'app = "sac-test-jobs"');
  assert.ok(checkFly(fly).problems.some((p) => p.includes('more than one prefix')));
});
