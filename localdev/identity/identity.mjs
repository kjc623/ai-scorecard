// The auth lab's identity material and the seed that links the sample tenant to the stand-in IdP.
//
// control-api is the product's identity service (backlog/11-sign-in-and-roles): it signs product
// access tokens with a session key, signs policy bundles with the vendor policy key, authenticates the
// dashboards with an internal token, and seals client secrets and other `*_enc` columns under a
// directory key. A lab needs all four, and none may be committed, so they are generated here into
// localdev/.authlab-identity/ (gitignored) by `node localdev/build.mjs --auth`.
//
// Generated once and then kept. Unlike the PKI in .authlab/, which run.mjs regenerates, these must
// survive a rebuild: a new directory key orphans every sealed row in the lab database (a tenant's
// user-reference key among them), and a new policy key strands the agent installed on the owner's
// machine, which pins the public half. Delete the directory to start over, and expect to reinstall.
//
// What each file is for:
//   session-signing.key      P-256 PKCS#8 PEM   control-api SAC_SESSION_SIGNING_KEY_FILE
//   policy-signing.key       Ed25519 PKCS#8 PEM control-api SAC_POLICY_SIGNING_KEY_FILE
//   policy-signing.key.hex   seed||public, hex  installer/lab-msi.mjs -> sac-bundle --policy-priv
//   policy-signing.pub.hex   public, hex        the device's SAC_POLICY_KEY (the trust anchor)
//   internal-token.env       SAC_INTERNAL_TOKEN  control-api and both dashboards (compose env_file)
//   directory-key.env        SAC_DIRECTORY_KEY   control-api (compose env_file)
//   oidc-client.env          OIDC_CLIENT_SECRET  the stand-in IdP's confidential client
//   seed-identity.sql        the sample tenant's identity connection and email domain, with the
//                            client secret sealed under the directory key; rewritten every build
//
// The seal is control/control-api/internal/directory.Cipher's, reproduced because the seed is SQL
// and the lab has no other way to write a sealed column without a sign-in: AES-256-GCM under
// HMAC-SHA256(directory key, "sac-directory-object-id\0" + tenant id), stored as nonce || ciphertext
// || tag. identity.test.mjs pins the derivation; if Cipher changes, the stand-in's client secret
// stops opening and the sample tenant's sign-in fails with a decryption error, not silently.

import { createCipheriv, createHmac, generateKeyPairSync, randomBytes } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

export const SAMPLE_TENANT = '5a3c0de0-7e57-4a11-9000-0000000d3a01';
export const SAMPLE_TENANT_NAME = 'Northwind Freight (sample)';
// The stand-in as an upstream OIDC provider. The issuer is the exact `iss` it signs, which is the
// mapping key control-api resolves the tenant by; the client is control-api itself.
export const STAND_IN_ISSUER = 'http://oidc:8080';
export const STAND_IN_CLIENT_ID = 'sac-control';
// The sample accounts' domain, so "Work email -> Continue" with viewer.sample@lab.test finds the
// sample tenant. The vendor sets domains, never the customer; the seed plays the vendor here.
export const SAMPLE_EMAIL_DOMAIN = 'lab.test';
// The stand-in emits product role names in `roles`, so the map is the identity map, written out so a
// reader of the seed does not have to know that an empty map means the same.
export const SAMPLE_ROLE_MAP = { viewer: 'viewer', analyst: 'analyst', content_reader: 'content_reader', admin: 'admin' };

const SEAL_LABEL = 'sac-directory-object-id\u0000';

/** The per-tenant key directory.Cipher derives from the master key. */
export function tenantKey(master, tenantId) {
  return createHmac('sha256', master).update(SEAL_LABEL).update(tenantId).digest();
}

/** directory.Cipher.Seal: nonce || ciphertext || tag, AES-256-GCM, no associated data. */
export function seal(master, tenantId, plaintext, nonce = randomBytes(12)) {
  const cipher = createCipheriv('aes-256-gcm', tenantKey(master, tenantId), nonce);
  const body = Buffer.concat([cipher.update(Buffer.from(plaintext, 'utf8')), cipher.final()]);
  return Buffer.concat([nonce, body, cipher.getAuthTag()]);
}

function readEnvValue(file, name) {
  const line = readFileSync(file, 'utf8').split(/\r?\n/).find((l) => l.startsWith(`${name}=`));
  if (!line) throw new Error(`${file} has no ${name}=`);
  return line.slice(name.length + 1).trim();
}

function writeNew(file, content, mode = 0o600) {
  writeFileSync(file, content, { mode });
}

/**
 * Create whatever is missing in `dir`, keep whatever exists, rewrite the seed, and return the paths.
 * Idempotent: a second call with the files present changes only seed-identity.sql (a fresh nonce).
 */
export function ensureLabIdentity(dir) {
  mkdirSync(dir, { recursive: true });
  const p = (name) => join(dir, name);
  const created = [];

  if (!existsSync(p('session-signing.key'))) {
    const { privateKey } = generateKeyPairSync('ec', { namedCurve: 'P-256' });
    writeNew(p('session-signing.key'), privateKey.export({ type: 'pkcs8', format: 'pem' }));
    created.push('session-signing.key');
  }

  if (!existsSync(p('policy-signing.key'))) {
    const { privateKey } = generateKeyPairSync('ed25519');
    const jwk = privateKey.export({ format: 'jwk' });
    const seed = Buffer.from(jwk.d, 'base64url');
    const pub = Buffer.from(jwk.x, 'base64url');
    writeNew(p('policy-signing.key'), privateKey.export({ type: 'pkcs8', format: 'pem' }));
    // sac-bundle takes Go's ed25519.PrivateKey form: the 32-byte seed followed by the public key.
    writeNew(p('policy-signing.key.hex'), `${Buffer.concat([seed, pub]).toString('hex')}\n`);
    writeNew(p('policy-signing.pub.hex'), `${pub.toString('hex')}\n`, 0o644);
    created.push('policy-signing.key');
  }

  if (!existsSync(p('internal-token.env'))) {
    writeNew(p('internal-token.env'), `SAC_INTERNAL_TOKEN=${randomBytes(32).toString('base64url')}\n`);
    created.push('internal-token.env');
  }

  if (!existsSync(p('directory-key.env'))) {
    // directory.DecodeKey's spelling: standard base64 of exactly 32 bytes.
    writeNew(p('directory-key.env'), `SAC_DIRECTORY_KEY=${randomBytes(32).toString('base64')}\n`);
    created.push('directory-key.env');
  }

  if (!existsSync(p('oidc-client.env'))) {
    writeNew(p('oidc-client.env'), `OIDC_CLIENT_SECRET=${randomBytes(32).toString('base64url')}\n`);
    created.push('oidc-client.env');
  }

  const master = Buffer.from(readEnvValue(p('directory-key.env'), 'SAC_DIRECTORY_KEY'), 'base64');
  if (master.length !== 32) throw new Error(`${p('directory-key.env')}: SAC_DIRECTORY_KEY is ${master.length} bytes, want 32`);
  const clientSecret = readEnvValue(p('oidc-client.env'), 'OIDC_CLIENT_SECRET');
  writeFileSync(p('seed-identity.sql'), seedSQL(seal(master, SAMPLE_TENANT, clientSecret)), { mode: 0o600 });

  return { dir, created };
}

/** The seed, as SQL. Idempotent: every statement converges on the same rows when re-run. */
export function seedSQL(sealedSecret) {
  const lit = (s) => `'${String(s).replace(/'/g, "''")}'`;
  return `-- Generated by localdev/build.mjs --auth (localdev/identity/identity.mjs). Rewritten every build;
-- do not edit. It links the SAMPLE tenant to the lab's stand-in IdP and nothing else: the owner's
-- tenant (11111111-1111-1111-1111-111111111111) is never written by the lab.
\\set ON_ERROR_STOP on
BEGIN;

-- The sample tenant, exactly as localdev/tools/simulate-devices.mjs creates it, so a fresh lab can
-- sign into it before the simulator has run. An existing row is left as it is.
INSERT INTO ops.tenant (tenant_id, name, status, residency_region, key_custody, ceiling_mode)
SELECT ${lit(SAMPLE_TENANT)}, ${lit(SAMPLE_TENANT_NAME)}, 'active',
       coalesce((SELECT residency_region FROM ops.tenant ORDER BY created_at LIMIT 1), 'authlab-region'),
       'vendor', 'm2'
ON CONFLICT (tenant_id) DO NOTHING;

-- The stand-in as the sample tenant's OIDC provider: issuer, confidential client, product roles
-- from its \`roles\` claim. Active from the start, attributed to the seed, because the lab plays the
-- vendor and the customer's first admin at once. A rebuild that re-sealed the secret updates it.
INSERT INTO ops.identity_connection (tenant_id, provider, issuer, client_id, client_secret_enc, scopes,
                                     roles_claim, role_map, status, activated_at, activated_by)
VALUES (${lit(SAMPLE_TENANT)}, 'oidc', ${lit(STAND_IN_ISSUER)}, ${lit(STAND_IN_CLIENT_ID)},
        '\\x${sealedSecret.toString('hex')}'::bytea, 'openid profile email', 'roles',
        ${lit(JSON.stringify(SAMPLE_ROLE_MAP))}::jsonb, 'active', now(), 'lab-seed')
ON CONFLICT (issuer) DO UPDATE
   SET client_id = EXCLUDED.client_id,
       client_secret_enc = EXCLUDED.client_secret_enc,
       role_map = EXCLUDED.role_map,
       status = 'active',
       activated_at = coalesce(ops.identity_connection.activated_at, EXCLUDED.activated_at),
       activated_by = coalesce(ops.identity_connection.activated_by, EXCLUDED.activated_by)
 WHERE ops.identity_connection.tenant_id = EXCLUDED.tenant_id;

-- The sign-in discovery domain of the sample accounts (viewer.sample@lab.test and the rest).
INSERT INTO ops.tenant_email_domain (domain, tenant_id, created_by)
VALUES (${lit(SAMPLE_EMAIL_DOMAIN)}, ${lit(SAMPLE_TENANT)}, 'lab-seed')
ON CONFLICT (domain) DO NOTHING;

COMMIT;
`;
}
