#!/usr/bin/env node
// Seeds the lab's fixtures into its database: the lab tenants, each signing in through its realm of
// the lab identity provider by its email domain, and each with the deployment key kept in
// .authlab-identity/. Idempotent; run.mjs runs it on every start, and so can you:
//
//   node localdev/seed.mjs
//
// It writes as the database superuser, because the product's own paths cannot set these up
// unattended: `control-api tenant create` mints a random tenant id (the lab MSI needs a fixed one),
// an identity connection is activated by an admin's first sign-in, and a deployment key is minted
// by an admin downloading a package of a built agent release. The rows are the ones those paths
// write. Only the lab tenants are written.

import { spawnSync } from 'node:child_process';
import { pathToFileURL } from 'node:url';

import { hashDeploymentKey, readLabIdentity, seal } from './identity/identity.mjs';
import { COMPOSE_FILE, IDENTITY_DIR, OIDC_CLIENT_ID, OIDC_ISSUER, REGION, TENANTS, TENANT_IDS } from './lab.mjs';

const ROLE_MAP = { viewer: 'viewer', analyst: 'analyst', content_reader: 'content_reader', admin: 'admin' };

const lit = (v) => `'${String(v).replace(/'/g, "''")}'`;

/** The seed as one transaction of idempotent statements. `nonce` fixes the seal in tests. */
export function seedSQL({ directoryKey, oidcClientSecret, deploymentKeys }, { tenants = TENANTS, region = REGION, nonce } = {}) {
  const statements = [];
  for (const [name, t] of Object.entries(tenants)) {
    const sealed = seal(directoryKey, t.id, oidcClientSecret, nonce);
    statements.push(`
INSERT INTO ops.tenant (tenant_id, name, status, residency_region, ceiling_mode, content_search)
VALUES (${lit(t.id)}, ${lit(t.name)}, 'active', ${lit(region)}, ${lit(t.ceiling)}, ${lit(t.contentSearch)})
ON CONFLICT (tenant_id) DO NOTHING;

INSERT INTO ops.identity_connection (tenant_id, provider, issuer, client_id, client_secret_enc, scopes, roles_claim, role_map,
                                     status, activated_at, activated_by)
VALUES (${lit(t.id)}, 'oidc', ${lit(`${OIDC_ISSUER}/${t.realm}`)}, ${lit(OIDC_CLIENT_ID)}, '\\x${sealed.toString('hex')}'::bytea,
        'openid profile email', 'roles', ${lit(JSON.stringify(ROLE_MAP))}::jsonb, 'active', now(), 'lab-seed')
ON CONFLICT (issuer) DO UPDATE
   SET client_id = EXCLUDED.client_id, client_secret_enc = EXCLUDED.client_secret_enc, role_map = EXCLUDED.role_map, status = 'active',
       activated_at = coalesce(ops.identity_connection.activated_at, EXCLUDED.activated_at),
       activated_by = coalesce(ops.identity_connection.activated_by, EXCLUDED.activated_by)
 WHERE ops.identity_connection.tenant_id = EXCLUDED.tenant_id;

INSERT INTO ops.tenant_email_domain (domain, tenant_id, created_by)
VALUES (${lit(t.domain)}, ${lit(t.id)}, 'lab-seed')
ON CONFLICT (domain) DO NOTHING;

INSERT INTO ops.deployment_key (tenant_id, key_hash, label, created_by)
VALUES (${lit(t.id)}, ${lit(hashDeploymentKey(deploymentKeys[name]))}, 'lab', 'lab-seed')
ON CONFLICT (key_hash) DO NOTHING;`);
  }
  return `\\set ON_ERROR_STOP on\nBEGIN;\n${statements.join('\n')}\nCOMMIT;\n`;
}

/** Runs the seed in the lab's postgres container. Returns the psql result. */
export function runSeed(identity, env = process.env) {
  return spawnSync('docker', ['compose', '-f', COMPOSE_FILE, 'exec', '-T', 'postgres', 'psql', '-U', 'postgres', '-d', 'shadow', '-q'], {
    input: seedSQL(identity), encoding: 'utf8', env,
  });
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const r = runSeed(readLabIdentity(IDENTITY_DIR, TENANT_IDS));
  if (r.status !== 0) {
    console.error(`seed: failed. Is the lab up (node localdev/run.mjs)?\n${`${r.stdout ?? ''}${r.stderr ?? ''}`.trim()}`);
    process.exit(1);
  }
  for (const t of Object.values(TENANTS)) console.log(`seed: ${t.name} ${t.id} (sign-in domain ${t.domain})`);
}
