import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

import { check, namesSet } from './check-config.mjs';

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
