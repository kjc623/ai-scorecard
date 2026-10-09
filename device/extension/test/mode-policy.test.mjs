/**
 * test/mode-policy.test.mjs — the mode is applied before content is read, from the bundle fields
 * the device resolves it from.
 *
 * The properties that matter are the conservative ones: with no bundle, a stale bundle or an
 * unrecognised mode the resolution goes downward to M0, never upward.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { createPolicyCache } from '../src/mode-policy.js';
import { modeReadsContent } from '../src/messages.js';

function clock(start = 1_000_000) {
  let t = start;
  return { now: () => t, advance: (ms) => (t += ms) };
}

test('no bundle at all resolves to M0 and says why', () => {
  const policy = createPolicyCache();
  const r = policy.modeFor({ tool_fingerprint: 'tf1:a' });
  assert.equal(r.mode, 'm0');
  assert.equal(r.reason, 'no_bundle');
  assert.equal(r.unsigned, true);
  assert.equal(modeReadsContent(r.mode), false);
});

test('the tenant default applies to a tool with no mode of its own', () => {
  const policy = createPolicyCache();
  policy.applyBundle({ version: 'v1', tenant_default_mode: 'm2' });
  const r = policy.modeFor({ tool_fingerprint: 'tf1:a' });
  assert.equal(r.mode, 'm2');
  assert.equal(r.reason, 'tenant_default');
  assert.equal(r.policy_version, 'v1');
  assert.equal(r.unsigned, false);
  assert.equal(policy.modeFor({}).mode, 'm2', 'and to a request whose tool is not known yet');
});

test('a tool mode applies when it is the more restrictive, as capture-core resolves it', () => {
  const policy = createPolicyCache();
  policy.applyBundle({ version: 'v1', tenant_default_mode: 'm2', tool_modes: { 'tf1:quiet': 'm0', 'tf1:low': 'm1', 'tf1:high': 'm3' } });
  assert.deepEqual(
    [policy.modeFor({ tool_fingerprint: 'tf1:quiet' }).mode, policy.modeFor({ tool_fingerprint: 'tf1:low' }).mode],
    ['m0', 'm1'],
  );
  assert.equal(policy.modeFor({ tool_fingerprint: 'tf1:low' }).reason, 'tool:tf1:low');
  // The other axes resolve to the tenant default for a request, so a tool's mode can only lower it.
  assert.equal(policy.modeFor({ tool_fingerprint: 'tf1:high' }).mode, 'm2', 'a wider tool mode does not raise the tenant default');
  assert.equal(policy.modeFor({ tool_fingerprint: 'tf1:unlisted' }).mode, 'm2');
});

test('an unrecognised mode resolves downward, never upward', () => {
  const policy = createPolicyCache();
  policy.applyBundle({ version: 'v1', tenant_default_mode: 'm2', tool_modes: { 'tf1:a': 'm9' } });
  assert.equal(policy.modeFor({ tool_fingerprint: 'tf1:a' }).mode, 'm0');
  policy.applyBundle({ version: 'v2', tenant_default_mode: 'everything' });
  assert.equal(policy.modeFor({ tool_fingerprint: 'tf1:b' }).mode, 'm0');
});

test('a stale bundle resolves downward to M0 rather than continuing at its old mode', () => {
  const c = clock();
  const policy = createPolicyCache({ now: c.now, staleAfterMs: 1000 });
  policy.applyBundle({ version: 'v1', tenant_default_mode: 'm3' });
  assert.equal(policy.modeFor({}).mode, 'm3');
  c.advance(1001);
  const r = policy.modeFor({});
  assert.equal(r.mode, 'm0');
  assert.equal(r.reason, 'bundle_stale');
  assert.equal(r.unsigned, true);
});

test('an unusable bundle keeps the previous one and marks it stale', () => {
  const c = clock();
  const policy = createPolicyCache({ now: c.now, staleAfterMs: 10_000 });
  policy.applyBundle({ version: 'v1', tenant_default_mode: 'm2' });
  for (const bad of [{ not_a_bundle: true }, { policy_version: 'v9', default_mode: 'm3' }, null, 'text', []]) {
    const result = policy.applyBundle(bad);
    assert.equal(result.applied, false);
    assert.equal(result.reason, 'unusable_bundle');
  }
  assert.equal(policy.snapshot().policy_version, 'v1', 'the previous bundle is retained');
  assert.equal(policy.snapshot().stale, true, 'and is reported as stale');
  assert.equal(policy.modeFor({}).mode, 'm2', 'a stale-but-present bundle keeps resolving');
  assert.equal(policy.applyBundle({ version: 'v2', tenant_default_mode: 'm1' }).applied, true);
  assert.equal(policy.snapshot().stale, false, 'a good bundle clears the stale mark');
});

test('a core answer overrides the cache, and an unrecognised answer still resolves downward', () => {
  const policy = createPolicyCache();
  policy.applyBundle({ version: 'v1', tenant_default_mode: 'm1' });
  assert.equal(policy.modeFor({}, { override: 'm3' }).mode, 'm3');
  assert.equal(policy.modeFor({}, { override: 'bogus' }).mode, 'm0');
  assert.equal(policy.modeFor({}, { override: '' }).reason, 'core_answer_unrecognised');
});

test('capture-core is asked when the bundle carries a mode input the extension cannot see', () => {
  const policy = createPolicyCache();
  assert.equal(policy.needsCoreMode(), false, 'no bundle: M0, nothing to ask');
  policy.applyBundle({ version: 'v1', tenant_default_mode: 'm2', tool_modes: { 'tf1:a': 'm1' }, population_modes: {}, device_modes: {} });
  assert.equal(policy.needsCoreMode(), false, 'the tenant default and tool modes are the extension\'s to resolve');
  for (const extra of [
    { population_modes: { finance: 'm1' } },
    { device_modes: { 'dev-1': 'm0' } },
    { required_notice_version: 'n1' },
    { class_priors: { 'tf1:a': ['health'] } },
  ]) {
    policy.applyBundle({ version: 'v2', tenant_default_mode: 'm2', ...extra });
    assert.equal(policy.needsCoreMode(), true, JSON.stringify(extra));
  }
});

test('isBodyBearing follows the tenant default, the bound no other input can raise', () => {
  const policy = createPolicyCache();
  assert.equal(policy.isBodyBearing('https://anything.invalid/chat'), false, 'absent cache ⇒ do not read the body');
  assert.equal(policy.readsAnyContent(), false);
  policy.applyBundle({ version: 'v1', tenant_default_mode: 'm1', tool_modes: { 'tf1:a': 'm0' } });
  assert.equal(policy.isBodyBearing('https://allowed.invalid/chat'), true);
  assert.equal(policy.readsAnyContent(), true);
  assert.equal(policy.isBodyBearing('not a url'), false);
  policy.applyBundle({ version: 'v2', tenant_default_mode: 'm0', tool_modes: { 'tf1:a': 'm3' } });
  assert.equal(policy.isBodyBearing('https://allowed.invalid/chat'), false);
  assert.equal(policy.readsAnyContent(), false);
});

test('the cache never persists: snapshot() exposes only what the running bundle says', () => {
  const policy = createPolicyCache();
  policy.applyBundle({ version: 'v7', tenant_default_mode: 'm1' });
  assert.deepEqual(policy.snapshot(), { policy_version: 'v7', stale: false, present: true });
  policy.clear();
  assert.equal(policy.snapshot().present, false);
  assert.equal(policy.snapshot().policy_version, null);
});

test('the rules, sanctioned tools, catalog and seed hosts come from the bundle, empty without one', () => {
  const policy = createPolicyCache();
  assert.deepEqual(policy.rules(), { rules: [], sanctioned_tools: [], catalog: [] });
  assert.deepEqual(policy.discoverySets(), {});
  const rules = [{ rule_id: 'block_credentials', action: 'block', match: { labels: ['credential'] }, message: 'Remove it.' }];
  const catalog = [{ app_key: 'cursor', category: 'ide', signals: [{ platform: 'windows', kind: 'windows_exe', value: 'Cursor.exe' }] }];
  policy.applyBundle({
    version: 'v1',
    tenant_default_mode: 'm1',
    interception: { enabled: false, seed_hosts: ['chat.example-ai.invalid'] },
    rules,
    sanctioned_tools: ['app:claude_code'],
    catalog,
  });
  assert.deepEqual(policy.rules(), { rules, sanctioned_tools: ['app:claude_code'], catalog });
  assert.deepEqual(policy.discoverySets(), { seed: ['chat.example-ai.invalid'] });
});
