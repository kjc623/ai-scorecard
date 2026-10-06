// config.test.mjs — the environment query-api reads, and its refusals to start.

import test from 'node:test';
import assert from 'node:assert/strict';
import { loadConfig, describeDatabase, parseAddr, ConfigError } from '../src/http/config.js';

const ENV = Object.freeze({
  SAC_PG_HOST: 'db.example',
  SAC_PG_DATABASE: 'shadow',
  SAC_PG_USER: 'query-api',
  SAC_CONTENT_VAULT_URL: 'https://content-vault.internal/',
  SAC_CURSOR_KEY: 'k'.repeat(32),
  SAC_AUTH_ISSUER: 'https://control-api.example',
  IDENTITY_ENDPOINT: 'http://169.254.169.254/msi/token',
  IDENTITY_HEADER: 'header-secret',
  AZURE_CLIENT_ID: '5d1c0de0-0000-4000-8000-000000000001',
});

test('the deployment environment resolves, with its defaults', () => {
  const cfg = loadConfig(ENV);
  assert.deepEqual(cfg.http, { host: '0.0.0.0', port: 8080 });
  assert.equal(cfg.pg.port, 5432);
  assert.equal(cfg.pg.sslMode, 'require');
  assert.equal(cfg.pg.password, '');
  assert.deepEqual({ ...cfg.pg.identity }, { endpoint: ENV.IDENTITY_ENDPOINT, header: ENV.IDENTITY_HEADER, clientId: ENV.AZURE_CLIENT_ID });
  assert.equal(cfg.contentVaultUrl, 'https://content-vault.internal');
  assert.deepEqual(cfg.cursorKey, Buffer.from(ENV.SAC_CURSOR_KEY));
  assert.equal(cfg.auth.audience, 'sac-query');
  assert.equal(cfg.auth.jwksUrl, 'https://control-api.example/.well-known/jwks.json');
  assert.equal(describeDatabase(cfg), 'postgres://query-api@db.example:5432/shadow (auth=entra, sslmode=require)');
});

test('every required variable refuses to start when missing', () => {
  for (const name of ['SAC_PG_HOST', 'SAC_PG_DATABASE', 'SAC_PG_USER', 'SAC_CONTENT_VAULT_URL', 'SAC_CURSOR_KEY', 'SAC_AUTH_ISSUER']) {
    assert.throws(() => loadConfig({ ...ENV, [name]: '' }), (e) => e instanceof ConfigError && e.message.includes(name), name);
  }
});

test('without a password the managed identity is required; with one it is not', () => {
  assert.throws(() => loadConfig({ ...ENV, IDENTITY_ENDPOINT: '' }), /managed identity/);
  assert.throws(() => loadConfig({ ...ENV, IDENTITY_HEADER: '' }), /managed identity/);
  const lab = loadConfig({ ...ENV, IDENTITY_ENDPOINT: '', IDENTITY_HEADER: '', SAC_PG_PASSWORD: 'secret', SAC_PG_SSLMODE: 'disable' });
  assert.equal(lab.pg.password, 'secret');
  assert.equal(lab.pg.sslMode, 'disable');
  assert.ok(!describeDatabase(lab).includes('secret'), 'the password is never rendered');
});

test('the cursor key must be long enough to be an HMAC key', () => {
  assert.throws(() => loadConfig({ ...ENV, SAC_CURSOR_KEY: 'short' }), /SAC_CURSOR_KEY/);
});

test('sslmode is require, verify-full or disable; there is no silent downgrade', () => {
  assert.equal(loadConfig({ ...ENV, SAC_PG_SSLMODE: 'verify-full' }).pg.sslMode, 'verify-full');
  assert.throws(() => loadConfig({ ...ENV, SAC_PG_SSLMODE: 'prefer' }), ConfigError);
  assert.throws(() => loadConfig({ ...ENV, SAC_PG_PORT: '0' }), ConfigError);
});

test('URLs must be absolute http(s) URLs', () => {
  assert.throws(() => loadConfig({ ...ENV, SAC_AUTH_ISSUER: 'control-api' }), ConfigError);
  assert.throws(() => loadConfig({ ...ENV, SAC_AUTH_ISSUER: 'ftp://control-api' }), ConfigError);
  assert.throws(() => loadConfig({ ...ENV, SAC_AUTH_JWKS_URL: 'keys' }), ConfigError);
  assert.throws(() => loadConfig({ ...ENV, SAC_CONTENT_VAULT_URL: 'vault' }), ConfigError);
  const custom = loadConfig({ ...ENV, SAC_AUTH_AUDIENCE: 'aud-x', SAC_AUTH_JWKS_URL: 'https://keys.example/jwks' });
  assert.equal(custom.auth.audience, 'aud-x');
  assert.equal(custom.auth.jwksUrl, 'https://keys.example/jwks');
});

test('the listen address takes host:port, :port or host', () => {
  assert.deepEqual(parseAddr(':9000'), { host: '0.0.0.0', port: 9000 });
  assert.deepEqual(parseAddr('127.0.0.1:8081'), { host: '127.0.0.1', port: 8081 });
  assert.deepEqual(parseAddr('[::1]:8081'), { host: '::1', port: 8081 });
  assert.throws(() => parseAddr('host:port'), ConfigError);
});
