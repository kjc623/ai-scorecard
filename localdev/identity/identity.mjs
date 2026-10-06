// The lab's secrets, in localdev/.authlab-identity/ (git-ignored): created once when missing and
// never replaced, because what was sealed, encrypted or pinned with them depends on them. A new
// directory key orphans every sealed row in the lab database, a new content key every stored prompt,
// and a new policy key or deployment key strands the agent installed from the lab MSI.
//
//   session-signing.key      P-256 PKCS#8 PEM    control-api signs product tokens (SAC_SESSION_SIGNING_KEY_FILE)
//   policy-signing.key       Ed25519 PKCS#8 PEM  control-api signs policy bundles (SAC_POLICY_SIGNING_KEY_FILE)
//   policy-signing.pub.hex   its public half     the key the lab MSI pins (SAC_POLICY_KEY)
//   internal-token.env       SAC_INTERNAL_TOKEN  control-api and the dashboard
//   directory-key.env        SAC_DIRECTORY_KEY   control-api; the seed seals the lab IdP's client secret with it
//   content-keys.env         SAC_CONTENT_KEYS    content-vault
//   cursor-key.env           SAC_CURSOR_KEY      query-api
//   oidc-client.env          OIDC_CLIENT_SECRET  the lab identity provider's client secret for control-api
//   classifier-signing.key   hex Ed25519 seed    signs the lab MSI's classifier release
//   deployment-key.<tenant>  a deployment key    one per lab tenant; the seed stores its hash
//
// The seal is control-api's directory cipher (control/control-api/internal/directory), reproduced
// because the lab writes a sealed column with SQL: AES-256-GCM under HMAC-SHA256(directory key,
// "sac-directory-object-id\0" + tenant id), stored as nonce || ciphertext || tag. identity.test.mjs
// pins it to a vector the Go cipher produced.

import { createCipheriv, createHash, createHmac, createPrivateKey, generateKeyPairSync, randomBytes } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

const SEAL_LABEL = 'sac-directory-object-id\u0000';

/** The per-tenant key the directory cipher derives from the master key. */
export function tenantKey(master, tenantId) {
  return createHmac('sha256', master).update(SEAL_LABEL).update(tenantId).digest();
}

/** The directory cipher's Seal: nonce || ciphertext || tag, AES-256-GCM, no additional data. */
export function seal(master, tenantId, plaintext, nonce = randomBytes(12)) {
  const cipher = createCipheriv('aes-256-gcm', tenantKey(master, tenantId), nonce);
  const body = Buffer.concat([cipher.update(Buffer.from(plaintext, 'utf8')), cipher.final()]);
  return Buffer.concat([nonce, body, cipher.getAuthTag()]);
}

/** A deployment key as control-api mints one: sacdk_<tenant>.<256-bit base64url secret>. */
export function mintDeploymentKey(tenantId) {
  return `sacdk_${tenantId.toLowerCase()}.${randomBytes(32).toString('base64url')}`;
}

/** The stored form of a deployment key: sha256:<hex> of the whole key. */
export function hashDeploymentKey(key) {
  return `sha256:${createHash('sha256').update(key).digest('hex')}`;
}

/** Every file, in creation order, with how to create it. `tenants` maps a lab tenant's name to its id. */
function material(dir, tenants) {
  const files = {
    'session-signing.key': () => generateKeyPairSync('ec', { namedCurve: 'P-256' }).privateKey.export({ type: 'pkcs8', format: 'pem' }),
    'policy-signing.key': () => generateKeyPairSync('ed25519').privateKey.export({ type: 'pkcs8', format: 'pem' }),
    // Derived from the private key, which is created first when both are missing.
    'policy-signing.pub.hex': () => {
      const jwk = createPrivateKey(readFileSync(join(dir, 'policy-signing.key'))).export({ format: 'jwk' });
      return `${Buffer.from(jwk.x, 'base64url').toString('hex')}\n`;
    },
    'internal-token.env': () => `SAC_INTERNAL_TOKEN=${randomBytes(32).toString('base64url')}\n`,
    // The directory cipher's spelling: standard base64 of exactly 32 bytes.
    'directory-key.env': () => `SAC_DIRECTORY_KEY=${randomBytes(32).toString('base64')}\n`,
    'content-keys.env': () => `SAC_CONTENT_KEYS=v1:${randomBytes(32).toString('base64')}\n`,
    'cursor-key.env': () => `SAC_CURSOR_KEY=${randomBytes(32).toString('base64url')}\n`,
    'oidc-client.env': () => `OIDC_CLIENT_SECRET=${randomBytes(32).toString('base64url')}\n`,
    'classifier-signing.key': () => `${randomBytes(32).toString('hex')}\n`,
  };
  for (const [name, id] of Object.entries(tenants)) files[`deployment-key.${name}`] = () => `${mintDeploymentKey(id)}\n`;
  return files;
}

/**
 * Creates whatever is missing in `dir` and returns the names it created. An existing file is never
 * rewritten: every write fails if the file appeared in the meantime.
 */
export function ensureLabIdentity(dir, tenants) {
  mkdirSync(dir, { recursive: true });
  const created = [];
  for (const [name, make] of Object.entries(material(dir, tenants))) {
    if (existsSync(join(dir, name))) continue;
    writeFileSync(join(dir, name), make(), { mode: 0o600, flag: 'wx' });
    created.push(name);
  }
  return created;
}

function envValue(text, name, file) {
  const line = text.split(/\r?\n/).find((l) => l.startsWith(`${name}=`));
  if (!line) throw new Error(`${file} has no ${name}=`);
  return line.slice(name.length + 1).trim();
}

/** Reads the material ensureLabIdentity created. */
export function readLabIdentity(dir, tenants) {
  const read = (name) => readFileSync(join(dir, name), 'utf8');
  const env = (name, key) => envValue(read(name), key, join(dir, name));
  const directoryKey = Buffer.from(env('directory-key.env', 'SAC_DIRECTORY_KEY'), 'base64');
  if (directoryKey.length !== 32) throw new Error(`${join(dir, 'directory-key.env')}: SAC_DIRECTORY_KEY is ${directoryKey.length} bytes, want 32`);
  const policyPublicKey = read('policy-signing.pub.hex').trim();
  if (!/^[0-9a-f]{64}$/.test(policyPublicKey)) throw new Error(`${join(dir, 'policy-signing.pub.hex')} is not a hex Ed25519 public key`);
  const deploymentKeys = {};
  for (const [name, id] of Object.entries(tenants)) {
    const key = read(`deployment-key.${name}`).trim();
    if (!key.startsWith(`sacdk_${id}.`)) throw new Error(`${join(dir, `deployment-key.${name}`)} is not a deployment key of tenant ${id}`);
    deploymentKeys[name] = key;
  }
  return {
    sessionSigningKey: read('session-signing.key'),
    policySigningKey: read('policy-signing.key'),
    policyPublicKey,
    directoryKey,
    oidcClientSecret: env('oidc-client.env', 'OIDC_CLIENT_SECRET'),
    deploymentKeys,
    paths: {
      policyPublicKey: join(dir, 'policy-signing.pub.hex'),
      classifierKey: join(dir, 'classifier-signing.key'),
    },
  };
}
