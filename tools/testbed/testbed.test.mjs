import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

import { APP_ID_ROW, FIELDS, OWNER_TENANT, TESTBED_FILE, checkAppId, parseTestbed, recordAppId, tableRows } from './testbed.mjs';

const real = readFileSync(TESTBED_FILE, 'utf8');
const env = { USERPROFILE: 'C:\\Users\\owner' };

const FILLED = {
  'Device hostname': 'device.preprod.example.com',
  'Test tenant id': '`0F0E0D0C-0000-4000-8000-000000000001`',
  'GitHub repository': 'example/ai-scorecard',
  'VM name': 'sac-ref-win11',
  'Console user': 'console.tester@contoso.example',
  'Second user': 'second.tester@contoso.example',
  'Entra tenant id': '0f0e0d0c-0000-4000-8000-000000000002',
  'Intune app registration': '0f0e0d0c-0000-4000-8000-000000000003',
  'Publishing certificate': 'A1B2C3D4E5F60718293A4B5C6D7E8F9012345678',
  'Test device group': '0f0e0d0c-0000-4000-8000-000000000004',
  'Tenant file': '`%USERPROFILE%\\.sac-testbed\\ShadowAICapture.tenant.env`',
  'VM admin credential': '`%USERPROFILE%\\.sac-testbed\\vm-admin.xml`',
  'Clean checkpoint': '`clean-enrolled`',
  [APP_ID_ROW]: '(task 59 fills this in)',
};

// The real TESTBED.md with every value cell replaced: by values[name], or emptied.
function withValues(text, values) {
  const lines = text.split('\n');
  for (const row of tableRows(text)) {
    const cells = lines[row.line].split(/(?<!\\)\|/);
    cells[2] = ` ${values[row.name] ?? ''} `;
    lines[row.line] = cells.join('|');
  }
  return lines.join('\n');
}

// The real file's table, filled in as the owner would.
function filled(overrides = {}) {
  return withValues(real, { ...FILLED, ...overrides });
}

test('the real TESTBED.md table has every row the tools read', () => {
  const names = tableRows(real).map((r) => r.name);
  for (const [name] of FIELDS) assert.ok(names.includes(name), `TESTBED.md has no "${name}" row`);
});

test('a filled table parses into the configuration', () => {
  const config = parseTestbed(filled(), env);
  assert.equal(config.vmName, 'sac-ref-win11');
  assert.equal(config.testTenantId, '0f0e0d0c-0000-4000-8000-000000000001');
  assert.equal(config.tenantFile, 'C:\\Users\\owner\\.sac-testbed\\ShadowAICapture.tenant.env');
  assert.equal(config.vmAdminCredential, 'C:\\Users\\owner\\.sac-testbed\\vm-admin.xml');
  assert.equal(config.cleanCheckpoint, 'clean-enrolled');
  assert.equal(config.intuneAppId, '', 'the placeholder in parentheses is no app id');
});

test('an empty required value is refused, by name', () => {
  assert.throws(() => parseTestbed(withValues(real, {}), env), (err) => {
    for (const [name, , , required] of FIELDS) {
      if (required) assert.match(err.message, new RegExp(`"${name}"`));
    }
    assert.doesNotMatch(err.message, new RegExp(`"${APP_ID_ROW}"`));
    return true;
  });
  // One signed-in user is enough: the second user is optional.
  assert.equal(parseTestbed(filled({ 'Second user': '' }), env).secondUser, '');
});

test('a malformed value is refused, by name', () => {
  assert.throws(() => parseTestbed(filled({ 'Publishing certificate': 'not-a-thumbprint' }), env), /"Publishing certificate" must be a 40-character certificate thumbprint/);
  assert.throws(() => parseTestbed(filled({ 'Test device group': 'Test devices' }), env), /"Test device group" must be a GUID/);
  assert.throws(() => parseTestbed(filled(), {}), /"Tenant file" uses %USERPROFILE%, which is not set/);
});

test("the owner's tenant is refused as the test tenant", () => {
  assert.throws(() => parseTestbed(filled({ 'Test tenant id': OWNER_TENANT }), env), /owner's tenant/);
});

test('the Intune app id is recorded once, in its row only', () => {
  const id = '9a8b7c6d-0000-4000-8000-00000000000a';
  const text = filled();
  const next = recordAppId(text, id.toUpperCase());
  assert.equal(parseTestbed(next, env).intuneAppId, id);
  const changed = next.split('\n').filter((l, i) => l !== text.split('\n')[i]);
  assert.equal(changed.length, 1);
  assert.match(changed[0], new RegExp(`^\\| ${APP_ID_ROW} \\| \`${id}\` \\|`));
  assert.equal(recordAppId(next, id), next, 'recording the same id again changes nothing');
});

test('a foreign app id is refused', () => {
  const recorded = '9a8b7c6d-0000-4000-8000-00000000000a';
  const foreign = '1b2c3d4e-0000-4000-8000-00000000000b';
  assert.throws(() => checkAppId(recorded, foreign), /refusing Intune app 1b2c3d4e-.*TESTBED\.md records 9a8b7c6d-/);
  assert.throws(() => checkAppId(recorded, 'not-an-id'), /not a GUID/);
  assert.doesNotThrow(() => checkAppId(recorded, recorded.toUpperCase()));
  assert.doesNotThrow(() => checkAppId('', foreign), 'with none recorded, the first app is the one');
  assert.throws(() => recordAppId(recordAppId(filled(), recorded), foreign), /refusing Intune app/);
});
