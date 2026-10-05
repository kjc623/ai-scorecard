// node --test localdev/identity/identity.test.mjs
//
// The lab seed writes a sealed column that control-api must later open, so the seal is the seam this
// file pins. The vector below was produced by control/control-api/internal/directory.Cipher itself
// (its tenantKey for this master and tenant, and its Open of a value sealed here), so a change on
// either side fails here rather than as a decryption error at the sample tenant's first sign-in.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createDecipheriv, createPrivateKey } from 'node:crypto';
import { mkdtempSync, readFileSync, rmSync, statSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import {
  SAMPLE_TENANT, STAND_IN_ISSUER, STAND_IN_CLIENT_ID, ensureLabIdentity, seal, seedSQL, tenantKey,
} from './identity.mjs';

const MASTER = Buffer.from([...Array(32).keys()]);

test('the per-tenant key is directory.Cipher\'s derivation', () => {
  assert.equal(tenantKey(MASTER, SAMPLE_TENANT).toString('hex'),
    '8889bc4bda5b8fd83c421a5fc0b4d8d2ba121e47a074e4a2426d56816fbe5a5f');
});

test('a sealed value is nonce || ciphertext || tag and opens under the tenant key only', () => {
  const sealed = seal(MASTER, SAMPLE_TENANT, 'lab-client-secret');
  assert.equal(sealed.length, 12 + 'lab-client-secret'.length + 16);
  const open = (tenant) => {
    const d = createDecipheriv('aes-256-gcm', tenantKey(MASTER, tenant), sealed.subarray(0, 12));
    d.setAuthTag(sealed.subarray(sealed.length - 16));
    return Buffer.concat([d.update(sealed.subarray(12, sealed.length - 16)), d.final()]).toString('utf8');
  };
  assert.equal(open(SAMPLE_TENANT), 'lab-client-secret');
  assert.throws(() => open('11111111-1111-1111-1111-111111111111'), 'another tenant\'s key must not open it');
});

test('the material is generated once and kept, and only the seed is rewritten', () => {
  const dir = mkdtempSync(join(tmpdir(), 'sac-identity-'));
  try {
    const first = ensureLabIdentity(dir);
    assert.deepEqual(first.created.sort(), ['directory-key.env', 'internal-token.env', 'oidc-client.env',
      'policy-signing.key', 'session-signing.key'].sort());
    const keep = ['session-signing.key', 'policy-signing.key', 'policy-signing.key.hex', 'internal-token.env',
      'directory-key.env', 'oidc-client.env'].map((f) => [f, readFileSync(join(dir, f), 'utf8')]);

    const second = ensureLabIdentity(dir);
    assert.deepEqual(second.created, [], 'a second build must not replace any key');
    for (const [f, before] of keep) assert.equal(readFileSync(join(dir, f), 'utf8'), before, `${f} changed`);

    // The forms each consumer parses.
    assert.equal(createPrivateKey(readFileSync(join(dir, 'session-signing.key'))).asymmetricKeyDetails.namedCurve, 'prime256v1');
    assert.equal(createPrivateKey(readFileSync(join(dir, 'policy-signing.key'))).asymmetricKeyType, 'ed25519');
    assert.match(readFileSync(join(dir, 'policy-signing.key.hex'), 'utf8'), /^[0-9a-f]{128}\n$/);
    assert.match(readFileSync(join(dir, 'policy-signing.pub.hex'), 'utf8'), /^[0-9a-f]{64}\n$/);
    assert.ok(readFileSync(join(dir, 'policy-signing.key.hex'), 'utf8').trim().endsWith(
      readFileSync(join(dir, 'policy-signing.pub.hex'), 'utf8').trim()), 'the 64-byte form is seed || public');
    assert.match(readFileSync(join(dir, 'directory-key.env'), 'utf8'), /^SAC_DIRECTORY_KEY=[A-Za-z0-9+/]{43}=\n$/);
    assert.match(readFileSync(join(dir, 'internal-token.env'), 'utf8'), /^SAC_INTERNAL_TOKEN=[A-Za-z0-9_-]{43}\n$/);
    if (process.platform !== 'win32') {
      assert.equal(statSync(join(dir, 'directory-key.env')).mode & 0o077, 0, 'a secret is not group/world readable');
    }
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('the seed links the sample tenant only, and converges when re-run', () => {
  const sql = seedSQL(Buffer.from('00ff', 'hex'));
  assert.match(sql, new RegExp(`'${SAMPLE_TENANT}'`));
  assert.match(sql, new RegExp(`'${STAND_IN_ISSUER}', '${STAND_IN_CLIENT_ID}'`));
  assert.match(sql, /'\\x00ff'::bytea/);
  assert.match(sql, /ON CONFLICT \(issuer\) DO UPDATE/);
  assert.match(sql, /ON CONFLICT \(domain\) DO NOTHING/);
  assert.match(sql, /ON CONFLICT \(tenant_id\) DO NOTHING/);
  // The owner's tenant is named only in the comment that says it is never written.
  const code = sql.split('\n').filter((l) => !l.startsWith('--')).join('\n');
  assert.doesNotMatch(code, /11111111-1111-1111-1111-111111111111/);
});
