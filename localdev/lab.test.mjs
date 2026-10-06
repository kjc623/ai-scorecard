import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createDecipheriv } from 'node:crypto';
import { existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { tenantKey } from './identity/identity.mjs';
import {
  COMPOSE_FILE, IMAGES, NETWORK, OIDC_CLIENT_ID, OIDC_ISSUER, REGION, ROOT, SMOKE_ANALYST, TENANTS,
  buildArgs, composeEnvironment, hostPort, imageTag, labAddresses, pkiState, readPKI, smokeArgs, writePKI,
} from './lab.mjs';
import { labVersion, tenantFile } from './lab-msi.mjs';
import { seedSQL } from './seed.mjs';

const OWNER_TENANT = '11111111-1111-1111-1111-111111111111';
const compose = readFileSync(COMPOSE_FILE, 'utf8');

function scratch(t) {
  const dir = mkdtempSync(join(tmpdir(), 'sac-lab-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  return dir;
}

const identity = {
  directoryKey: Buffer.alloc(32, 7),
  oidcClientSecret: 'client-secret',
  deploymentKeys: { lab: `sacdk_${TENANTS.lab.id}.lab-key`, sample: `sacdk_${TENANTS.sample.id}.sample-key` },
  policyPublicKey: 'ab'.repeat(32),
  sessionSigningKey: 'session-pem',
  policySigningKey: 'policy-pem',
};

test('every image is built from a Dockerfile in the repository, as sac/<name>:lab', () => {
  for (const image of IMAGES) {
    assert.ok(existsSync(join(ROOT, image.dockerfile)), image.dockerfile);
    assert.deepEqual(buildArgs(image), ['build', '-f', image.dockerfile, '-t', `sac/${image.name}:lab`, '.']);
  }
  assert.ok(IMAGES.findIndex((i) => i.name === 'jobs') < IMAGES.findIndex((i) => i.name === 'lab-jobs'), 'lab-jobs is built from jobs');
  // Every image compose.yaml runs is one build.mjs builds.
  for (const [, name] of compose.matchAll(/image: sac\/([a-z-]+):lab/g)) assert.ok(IMAGES.some((i) => i.name === name), name);
});

test('compose.yaml agrees with the facts the scripts use', () => {
  assert.match(compose, new RegExp(`^networks:\\n  default:\\n    name: ${NETWORK}$`, 'm'));
  assert.equal([...compose.matchAll(/SAC_REGION: (\S+)/g)].map((m) => m[1]).join(), `${REGION},${REGION}`);
  assert.match(compose, new RegExp(`OIDC_ISSUER: ${OIDC_ISSUER}\\n`));
  assert.match(compose, new RegExp(`OIDC_CLIENT_ID: ${OIDC_CLIENT_ID}\\n`));
  for (const t of Object.values(TENANTS)) {
    for (const role of ['viewer', 'analyst', 'content_reader', 'admin']) {
      assert.match(compose, new RegExp(`"email":"[a-z]+@${t.domain.replace('.', '\\.')}","role":"${role}","realm":"${t.realm}"`), `${t.realm} ${role}`);
    }
  }
  assert.match(compose, new RegExp(`"email":"${SMOKE_ANALYST}","role":"analyst"`));
  for (const name of Object.keys(composeEnvironment({}, identity))) {
    assert.match(compose, new RegExp(`\\$\\{?${name}\\b|environment: ${name}\\n`), `compose.yaml reads ${name}`);
  }
  assert.doesNotMatch(compose, new RegExp(OWNER_TENANT));
});

test('the key material reaches compose through the environment', () => {
  const pki = { caCert: 'ca', caKey: 'cakey', edgeCert: 'edge', edgeKey: 'edgekey' };
  assert.deepEqual(composeEnvironment(pki, identity), {
    LAB_CA_CERT_PEM: 'ca', LAB_CA_KEY_PEM: 'cakey', LAB_EDGE_CERT_PEM: 'edge', LAB_EDGE_KEY_PEM: 'edgekey',
    LAB_SESSION_SIGNING_KEY: 'session-pem', LAB_POLICY_SIGNING_KEY: 'policy-pem',
  });
});

test('the PKI is stored only where none exists, and never replaced', (t) => {
  const dir = scratch(t);
  assert.equal(pkiState(dir), 'missing');
  const json = JSON.stringify({ ca_cert: '-----BEGIN CERTIFICATE-----ca', ca_key: '-----BEGIN EC PRIVATE KEY-----k',
    edge_cert: '-----BEGIN CERTIFICATE-----e', edge_key: '-----BEGIN EC PRIVATE KEY-----ek' });
  writePKI(dir, json);
  assert.equal(pkiState(dir), 'complete');
  assert.equal(readPKI(dir).edgeCert, '-----BEGIN CERTIFICATE-----e');
  assert.throws(() => writePKI(dir, json), /EEXIST/);
  assert.equal(readPKI(dir).caCert, '-----BEGIN CERTIFICATE-----ca');
  rmSync(join(dir, 'edge-server.key'));
  assert.equal(pkiState(dir), 'partial');
  assert.throws(() => writePKI(scratch(t), JSON.stringify({ ca_cert: 'x' })), /no caCert/);
});

test('the addresses come from the rendered compose configuration', () => {
  const config = {
    services: {
      dashboard: { environment: { SAC_PUBLIC_URL: 'http://127.0.0.1:9787' }, ports: [{ host_ip: '127.0.0.1', target: 8080, published: '9787' }] },
      oidc: { environment: { OIDC_PUBLIC_URL: 'http://127.0.0.1:9790' } },
      'control-api': { environment: { SAC_PUBLIC_DEVICE_ENDPOINT: 'https://127.0.0.1:9443' } },
      postgres: { ports: [{ host_ip: '127.0.0.1', target: 5432, published: '55499' }] },
    },
  };
  const a = labAddresses(config);
  assert.deepEqual(a, { dashboard: 'http://127.0.0.1:9787', oidc: 'http://127.0.0.1:9790', deviceEndpoint: 'https://127.0.0.1:9443', postgres: '127.0.0.1:55499' });
  assert.equal(hostPort('http://localhost'), 'localhost:80');
  assert.equal(hostPort('https://edge'), 'edge:443');

  const args = smokeArgs(a, identity);
  assert.deepEqual(args.slice(0, 10), ['run', '--rm', '--network', NETWORK, '-e', 'LAB_CA_CERT_PEM', '-e', 'LAB_DEPLOYMENT_KEY', imageTag('authlab'), 'smoke']);
  const flag = (name) => args[args.indexOf(name) + 1];
  assert.equal(flag('-tenant'), TENANTS.sample.id);
  assert.equal(flag('-analyst'), SMOKE_ANALYST);
  assert.equal(flag('-policy-key'), identity.policyPublicKey);
  assert.equal(flag('-dashboard'), 'http://127.0.0.1:9787');
  assert.equal(flag('-resolve'), '127.0.0.1:9787=dashboard:8080,127.0.0.1:9790=oidc:8080');
  assert.ok(!args.some((a) => a.includes('sacdk_')), 'the deployment key travels in the environment, not on the command line');
});

test('the seed writes the lab tenants only, idempotently', () => {
  const nonce = Buffer.alloc(12, 1);
  const sql = seedSQL(identity, { nonce });
  assert.match(sql, /^\\set ON_ERROR_STOP on\nBEGIN;\n/);
  assert.match(sql, /COMMIT;\n$/);
  assert.doesNotMatch(sql, new RegExp(OWNER_TENANT));
  for (const t of Object.values(TENANTS)) {
    assert.match(sql, new RegExp(`'${t.id}', '${t.name.replace(/[()]/g, '\\$&')}', 'active', '${REGION}', '${t.ceiling}', '${t.contentSearch}'`));
    assert.match(sql, new RegExp(`'${OIDC_ISSUER}/${t.realm}', '${OIDC_CLIENT_ID}'`));
    assert.match(sql, new RegExp(`VALUES \\('${t.domain.replace('.', '\\.')}', '${t.id}'`));
  }
  for (const clause of ['ON CONFLICT (tenant_id) DO NOTHING', 'ON CONFLICT (issuer) DO UPDATE', 'ON CONFLICT (domain) DO NOTHING', 'ON CONFLICT (key_hash) DO NOTHING']) {
    assert.equal(sql.split(clause).length - 1, Object.keys(TENANTS).length, clause);
  }
  // The stored key is the hash, never the key itself.
  assert.doesNotMatch(sql, /sacdk_/);
  assert.match(sql, /'sha256:[0-9a-f]{64}', 'lab', 'lab-seed'/);

  // The client secret is sealed for its own tenant.
  const sealed = Buffer.from(new RegExp(`'${TENANTS.lab.id}', 'oidc', '[^']+', '[^']+', '\\\\x([0-9a-f]+)'`).exec(sql)[1], 'hex');
  const d = createDecipheriv('aes-256-gcm', tenantKey(identity.directoryKey, TENANTS.lab.id), sealed.subarray(0, 12));
  d.setAuthTag(sealed.subarray(sealed.length - 16));
  assert.equal(Buffer.concat([d.update(sealed.subarray(12, sealed.length - 16)), d.final()]).toString(), 'client-secret');
});

test('the lab MSI carries a rising version and the lab tenant file', () => {
  assert.equal(labVersion(new Date(Date.UTC(2026, 0, 1))), '1.0.0');
  assert.equal(labVersion(new Date(Date.UTC(2026, 9, 6, 12))), `1.0.${278 * 24 + 12}`);
  assert.equal(labVersion(new Date(Date.UTC(2040, 0, 1))), '1.0.65535');
  const file = tenantFile({ tenantId: TENANTS.lab.id, deviceEndpoint: 'https://127.0.0.1:8443', deploymentKey: 'sacdk_x', caFile: 'C:\\lab\\dev-ca.crt' });
  assert.deepEqual(file.split('\r\n').filter((l) => l && !l.startsWith('#')), [
    `SAC_TENANT_ID=${TENANTS.lab.id}`, 'SAC_DEVICE_ENDPOINT=https://127.0.0.1:8443', 'SAC_DEPLOYMENT_KEY=sacdk_x', 'SAC_CA_FILE=C:\\lab\\dev-ca.crt',
  ]);
});

test('nothing in the lab names a removed column, mode or rule', () => {
  for (const file of ['compose.yaml', 'seed.mjs', 'run.mjs', 'lab.mjs', 'lab-msi.mjs', 'tools/simulate-devices.mjs', 'identity/identity.mjs']) {
    const text = readFileSync(join(ROOT, 'localdev', file), 'utf8');
    for (const gone of ['key_custody', 'kek_id', 'enrolment_token', 'device_ca_pem', 'dpop', 'DPoP', 'SAC_STORE', 'SAC_ROLE', 'ollama_local', 'backlog/', 'sac-bundle']) {
      assert.ok(!text.includes(gone), `${file} names ${gone}`);
    }
  }
});
