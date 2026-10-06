import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createDecipheriv, createPrivateKey, createPublicKey } from 'node:crypto';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { ensureLabIdentity, hashDeploymentKey, mintDeploymentKey, readLabIdentity, seal, tenantKey } from './identity.mjs';

const MASTER = Buffer.from([...Array(32).keys()]);
const SAMPLE = '5a3c0de0-7e57-4a11-9000-0000000d3a01';
const TENANTS = { lab: '10ca1ab0-0000-4000-8000-000000000001', sample: SAMPLE };

function scratch(t) {
  const dir = mkdtempSync(join(tmpdir(), 'sac-identity-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  return dir;
}

test('the per-tenant key is the directory cipher\'s derivation', () => {
  // Produced by control-api's directory cipher for this master key and tenant.
  assert.equal(tenantKey(MASTER, SAMPLE).toString('hex'), '8889bc4bda5b8fd83c421a5fc0b4d8d2ba121e47a074e4a2426d56816fbe5a5f');
});

test('a sealed value is nonce || ciphertext || tag and opens under its tenant\'s key only', () => {
  const sealed = seal(MASTER, SAMPLE, 'lab-client-secret');
  assert.equal(sealed.length, 12 + 'lab-client-secret'.length + 16);
  const open = (tenant) => {
    const d = createDecipheriv('aes-256-gcm', tenantKey(MASTER, tenant), sealed.subarray(0, 12));
    d.setAuthTag(sealed.subarray(sealed.length - 16));
    return Buffer.concat([d.update(sealed.subarray(12, sealed.length - 16)), d.final()]).toString('utf8');
  };
  assert.equal(open(SAMPLE), 'lab-client-secret');
  assert.throws(() => open(TENANTS.lab));
});

test('a deployment key has control-api\'s shape, and its stored form is its sha256', () => {
  const key = mintDeploymentKey(TENANTS.lab.toUpperCase());
  assert.match(key, /^sacdk_10ca1ab0-0000-4000-8000-000000000001\.[A-Za-z0-9_-]{43}$/);
  assert.match(hashDeploymentKey(key), /^sha256:[0-9a-f]{64}$/);
  assert.notEqual(mintDeploymentKey(SAMPLE), mintDeploymentKey(SAMPLE));
});

test('the material is created once and never rewritten', (t) => {
  const dir = scratch(t);
  const created = ensureLabIdentity(dir, TENANTS);
  assert.deepEqual(created.sort(), ['classifier-signing.key', 'content-keys.env', 'cursor-key.env', 'deployment-key.lab',
    'deployment-key.sample', 'directory-key.env', 'internal-token.env', 'oidc-client.env', 'policy-signing.key',
    'policy-signing.pub.hex', 'session-signing.key'].sort());
  const before = Object.fromEntries(created.map((f) => [f, readFileSync(join(dir, f), 'utf8')]));
  assert.deepEqual(ensureLabIdentity(dir, TENANTS), []);
  for (const [f, text] of Object.entries(before)) assert.equal(readFileSync(join(dir, f), 'utf8'), text, `${f} changed`);

  // The forms each consumer parses.
  assert.equal(createPrivateKey(before['session-signing.key']).asymmetricKeyDetails.namedCurve, 'prime256v1');
  const policy = createPrivateKey(before['policy-signing.key']);
  assert.equal(policy.asymmetricKeyType, 'ed25519');
  assert.equal(before['policy-signing.pub.hex'].trim(),
    Buffer.from(createPublicKey(policy).export({ format: 'jwk' }).x, 'base64url').toString('hex'));
  assert.match(before['directory-key.env'], /^SAC_DIRECTORY_KEY=[A-Za-z0-9+/]{43}=\n$/);
  assert.match(before['content-keys.env'], /^SAC_CONTENT_KEYS=v1:[A-Za-z0-9+/]{43}=\n$/);
  assert.match(before['cursor-key.env'], /^SAC_CURSOR_KEY=[A-Za-z0-9_-]{43}\n$/);
  assert.match(before['classifier-signing.key'], /^[0-9a-f]{64}\n$/);

  const read = readLabIdentity(dir, TENANTS);
  assert.equal(read.directoryKey.length, 32);
  assert.equal(read.policyPublicKey, before['policy-signing.pub.hex'].trim());
  assert.ok(read.deploymentKeys.sample.startsWith(`sacdk_${SAMPLE}.`));
  assert.equal(read.oidcClientSecret, before['oidc-client.env'].trim().slice('OIDC_CLIENT_SECRET='.length));
});

test('existing material is kept and only what is missing is added', (t) => {
  const dir = scratch(t);
  writeFileSync(join(dir, 'directory-key.env'), `SAC_DIRECTORY_KEY=${MASTER.toString('base64')}\n`);
  const created = ensureLabIdentity(dir, TENANTS);
  assert.ok(!created.includes('directory-key.env'));
  assert.deepEqual(readLabIdentity(dir, TENANTS).directoryKey, MASTER);
});

test('a deployment key of another tenant is refused when read', (t) => {
  const dir = scratch(t);
  ensureLabIdentity(dir, TENANTS);
  writeFileSync(join(dir, 'deployment-key.lab'), `${mintDeploymentKey(SAMPLE)}\n`);
  assert.throws(() => readLabIdentity(dir, TENANTS), /deployment-key\.lab/);
});
