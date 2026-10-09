import { test } from 'node:test';
import assert from 'node:assert/strict';

import { checkTenantFile, createLog, evaluateInstall, parseDsreg, parseEnvFile, selectRun } from './deploy.mjs';

const SHA = '4f1c2a9e8b7d6c5b4a39281706f5e4d3c2b1a090';
const OTHER = '0a1b2c3d4e5f60718293a4b5c6d7e8f901234567';

// `gh run list --workflow deploy.yml --branch main --json databaseId,headSha,status,conclusion,url,number,event`
const runs = [
  { databaseId: 9003, number: 203, headSha: OTHER, status: 'in_progress', conclusion: '', event: 'push', url: 'https://github.com/example/ai-scorecard/actions/runs/9003' },
  { databaseId: 9002, number: 202, headSha: SHA, status: 'completed', conclusion: 'success', event: 'workflow_dispatch', url: 'https://github.com/example/ai-scorecard/actions/runs/9002' },
  { databaseId: 9001, number: 201, headSha: SHA, status: 'completed', conclusion: 'success', event: 'push', url: 'https://github.com/example/ai-scorecard/actions/runs/9001' },
  { databaseId: 9000, number: 200, headSha: SHA, status: 'completed', conclusion: 'failure', event: 'push', url: 'https://github.com/example/ai-scorecard/actions/runs/9000' },
];

test('the run for a commit is its newest push run', () => {
  const pick = selectRun(runs, SHA);
  assert.equal(pick.state, 'success');
  assert.equal(pick.run.databaseId, 9001, 'a manual run (another environment) is never the release');
  assert.equal(selectRun(runs, SHA.slice(0, 7).toUpperCase()).run.databaseId, 9001, 'an abbreviated sha selects it too');
});

test('a running, failed or missing run is reported as such', () => {
  assert.deepEqual(selectRun(runs, OTHER), { state: 'running', run: runs[0] });
  const failed = selectRun(runs.filter((r) => r.number !== 201), SHA);
  assert.equal(failed.state, 'failed');
  assert.equal(failed.run.url, 'https://github.com/example/ai-scorecard/actions/runs/9000');
  assert.equal(selectRun([{ ...runs[2], status: 'queued', conclusion: '' }], SHA).state, 'running');
  assert.equal(selectRun([{ ...runs[2], conclusion: 'cancelled' }], SHA).state, 'failed');
  assert.deepEqual(selectRun(runs, 'abcdef1'), { state: 'missing' });
  assert.deepEqual(selectRun([], SHA), { state: 'missing' });
  assert.throws(() => selectRun(runs, 'main'), /not a commit sha/);
});

const KEY = 'sac_dk_7Qm3vX9pL2rT8wZ4yB6nC1dF5gH0jK';
const tenantFile = `# Shadow AI Capture tenant settings.\r\nSAC_TENANT_ID=0f0e0d0c-0000-4000-8000-000000000001\r\nSAC_DEVICE_ENDPOINT=https://device.preprod.example.com\r\nSAC_DEPLOYMENT_KEY=${KEY}\r\n`;
const config = { testTenantId: '0f0e0d0c-0000-4000-8000-000000000001', deviceHostname: 'device.preprod.example.com' };

test('no log line contains the deployment key', () => {
  const written = [];
  const log = createLog((s) => written.push(s));
  log.secret(checkTenantFile(parseEnvFile(tenantFile), config));

  log.line(`tenant file read: SAC_DEPLOYMENT_KEY=${KEY}`);
  log.line(log.redact(`gh failed: ${KEY} ${KEY}`));
  const relay = log.relay('  ');
  // A child's output arrives in chunks that split the key and its lines anywhere.
  const output = `publish: start\r\nmsiexec /i ShadowAICapture.msi SAC_DEPLOYMENT_KEY=${KEY} /qn\r\nlast line ${KEY}`;
  for (let i = 0; i < output.length; i += 5) relay.push(output.slice(i, i + 5));
  relay.end();

  const text = written.join('');
  assert.ok(!text.includes(KEY), text);
  for (const part of [KEY.slice(0, 12), KEY.slice(-12)]) assert.ok(!text.includes(part), `a fragment of the key leaked: ${text}`);
  assert.equal(written.length, 5);
  assert.ok(written.every((l) => l.endsWith('\n')));
  assert.match(text, /msiexec \/i ShadowAICapture\.msi SAC_DEPLOYMENT_KEY=\[redacted\] \/qn/);
});

test('the relay hands each whole line to its reader before redacting the print', () => {
  const seen = [];
  const log = createLog(() => {});
  const relay = log.relay('', (l) => seen.push(l));
  relay.push('publish: created app 9a8b7c6d\r\napp-id 9a8b7c6d-0000-4000-8000-0000');
  relay.push('0000000a\r\n');
  relay.end();
  assert.deepEqual(seen, ['publish: created app 9a8b7c6d', 'app-id 9a8b7c6d-0000-4000-8000-00000000000a']);
});

test('only the test tenant file on the device edge is used', () => {
  const env = parseEnvFile(tenantFile);
  assert.throws(() => checkTenantFile({ ...env, SAC_TENANT_ID: '11111111-1111-1111-1111-111111111111' }, config), /not the test tenant/);
  assert.throws(() => checkTenantFile({ ...env, SAC_DEVICE_ENDPOINT: 'https://localhost:8443' }, config), /not https:\/\/device\.preprod\.example\.com/);
  assert.throws(() => checkTenantFile({ SAC_TENANT_ID: config.testTenantId }, config), /lacks SAC_DEVICE_ENDPOINT, SAC_DEPLOYMENT_KEY/);
});

test('dsregcmd /status gives the Entra device id and tenant', () => {
  const out = [
    '+----------------------------------------------------------------------+',
    '| Device State                                                         |',
    '+----------------------------------------------------------------------+',
    '',
    '             AzureAdJoined : YES',
    '          EnterpriseJoined : NO',
    '',
    '                  DeviceId : 00AA00AA-BB11-CC22-DD33-44EE44EE44EE',
    '                  TenantId : aaaabbbb-0000-cccc-1111-dddd2222eeee',
  ].join('\r\n');
  assert.deepEqual(parseDsreg(out), { joined: true, deviceId: '00aa00aa-bb11-cc22-dd33-44ee44ee44ee', tenantId: 'aaaabbbb-0000-cccc-1111-dddd2222eeee' });
  assert.equal(parseDsreg('AzureAdJoined : NO').joined, false);
});

test('the VM runs a release once it is installed, running, enrolled and reporting since it started', () => {
  const release = { version: '1.0.201', product_code: '{6C1A2B3D-0000-4000-8000-0000000000AA}' };
  const done = {
    products: [{ product_code: '{6C1A2B3D-0000-4000-8000-0000000000AA}', version: '1.0.201' }],
    service: 'Running',
    service_started_at: '2026-10-09T10:00:00.0000000Z',
    health: { agent_version: '1.0.201', enrolled: true, device_id: true, last_heartbeat_at: '2026-10-09T10:01:00Z' },
  };
  assert.deepEqual(evaluateInstall(done, release), { done: true, waiting: [] });
  const older = { ...done, products: [{ product_code: '{0D0D0D0D-0000-4000-8000-000000000001}', version: '1.0.200' }] };
  assert.match(evaluateInstall(older, release).waiting[0], /install of 1\.0\.201 \(installed: 1\.0\.200\)/);
  const stale = { ...done, health: { ...done.health, last_heartbeat_at: '2026-10-09T09:59:00Z' } };
  assert.deepEqual(evaluateInstall(stale, release).waiting, ['a health report acknowledged by pre-prod']);
  const unenrolled = { ...done, health: { ...done.health, enrolled: false, device_id: false } };
  assert.deepEqual(evaluateInstall(unenrolled, release).waiting, ['a device credential']);
});
