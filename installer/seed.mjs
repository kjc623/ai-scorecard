#!/usr/bin/env node
// installer/seed.mjs - mint and seed one single-use enrolment token in the local auth lab.
//
//   node installer/seed.mjs                          # the lab tenant, authlab.compose.yaml
//   node installer/seed.mjs --tenant UUID --hours 24
//
// It prints ONLY the plaintext token on stdout (diagnostics go to stderr), so a caller can capture
// it:  $token = node installer/seed.mjs    |    TOKEN=$(node installer/seed.mjs)
//
// Why this exists: the device must present a single-use token to POST /v1/enrol, and
// `localdev/run.mjs --auth` mints two tokens for its own in-network smoke and then discards them.
// Nothing else seeds one for a device that runs on the host. The token's stored form is
// sha256(plaintext), the one spelling the repository uses for digests.

import { spawnSync } from 'node:child_process';
import { createHash, randomBytes } from 'node:crypto';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const ARGS = process.argv.slice(2);
function valueOf(flag, fallback) {
  const i = ARGS.indexOf(flag);
  return i === -1 ? fallback : ARGS[i + 1];
}

const tenant = valueOf('--tenant', '11111111-1111-1111-1111-111111111111');
const hours = valueOf('--hours', '24');
const compose = resolve(ROOT, valueOf('--compose', join('localdev', 'authlab.compose.yaml')));

if (!/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(tenant)) {
  console.error(`seed.mjs: --tenant must be a uuid (got ${tenant})`);
  process.exit(2);
}

// --ceiling raises (or lowers) the tenant's collection ceiling. The tenant row is otherwise left as
// it is, because the lab's own smoke seeds it at m1. At m3 the tenant must name a KEK (a schema
// constraint) and needs a content budget, without which every content grant is denied over_budget.
const ceiling = valueOf('--ceiling', '');
if (ceiling !== '' && !['m0', 'm1', 'm2', 'm3'].includes(ceiling)) {
  console.error(`seed.mjs: --ceiling must be one of m0, m1, m2, m3 (got ${ceiling})`);
  process.exit(2);
}
const CONTENT_BUDGET_BYTES_PER_DAY = 1024 * 1024 * 1024;

const token = `sac1.${tenant}.${randomBytes(32).toString('base64url')}`;
const hash = 'sha256:' + createHash('sha256').update(token).digest('hex');

const raiseCeiling =
  ceiling === 'm3'
    ? `UPDATE ops.tenant
   SET ceiling_mode = 'm3',
       kek_id = coalesce(kek_id, 'lab-kek-' || tenant_id::text),
       content_search = 'full_text',
       content_budget_bytes_per_day = greatest(content_budget_bytes_per_day, ${CONTENT_BUDGET_BYTES_PER_DAY})
 WHERE tenant_id = '${tenant}';`
    : ceiling !== ''
      ? `UPDATE ops.tenant SET ceiling_mode = '${ceiling}' WHERE tenant_id = '${tenant}';`
      : '';

const sql = `
INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, ceiling_mode)
  VALUES ('${tenant}', 'installer-dev', 'active', 'authlab-region', 'vendor', 'm1')
  ON CONFLICT (tenant_id) DO NOTHING;
${raiseCeiling}
INSERT INTO ops.enrolment_token (tenant_id, token_hash, expires_at)
  VALUES ('${tenant}', '${hash}', now() + interval '${Number(hours)} hours')
  ON CONFLICT DO NOTHING;
`;

const res = spawnSync(
  'docker',
  ['compose', '-f', compose, 'exec', '-T', 'postgres', 'psql', '-U', 'postgres', '-d', 'shadow', '-v', 'ON_ERROR_STOP=1', '-c', sql],
  { encoding: 'utf8' },
);
if (res.status !== 0) {
  console.error(`seed.mjs: seeding failed. Is the auth lab up?  node localdev/run.mjs --auth`);
  console.error(`${res.stdout ?? ''}${res.stderr ?? ''}`.trim());
  process.exit(1);
}

console.error(`seed.mjs: tenant ${tenant}, token valid ${hours}h, expires_at now()+interval '${Number(hours)} hours'`);
process.stdout.write(token + '\n');
