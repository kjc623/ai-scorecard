/**
 * test/mode-policy.test.mjs — §11's "the mode is applied before content is read".
 *
 * The properties that matter here are the conservative ones: with no bundle, a stale bundle or an
 * unparseable scope entry the resolution goes **downward** to M0, never upward. §11.3: "A device
 * cannot exceed a ceiling it holds no bundle entry for".
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { createPolicyCache, patternMatches } from '../src/mode-policy.js';
import { modeReadsContent } from '../src/messages.js';

function clock(start = 1_000_000) {
  let t = start;
  return { now: () => t, advance: (ms) => (t += ms) };
}

test('no bundle at all resolves to M0 and says why', () => {
  const policy = createPolicyCache();
  const r = policy.modeFor({ host: 'chat.example-ai.invalid' });
  assert.equal(r.mode, 'm0');
  assert.equal(r.reason, 'no_bundle');
  assert.equal(r.unsigned, true);
  assert.equal(modeReadsContent(r.mode), false);
});

test('a signed bundle resolves a host to its scope entry', () => {
  const policy = createPolicyCache();
  policy.applyBundle({
    policy_version: 'v1',
    default_mode: 'm1',
    scope: { 'chat.example-ai.invalid': 'm2' },
  });
  const r = policy.modeFor({ host: 'chat.example-ai.invalid' });
  assert.equal(r.mode, 'm2');
  assert.equal(r.reason, 'scope:chat.example-ai.invalid');
  assert.equal(r.policy_version, 'v1');
  assert.equal(r.unsigned, false);
});

test('a host with no scope entry resolves to the tenant default, not to a wider mode', () => {
  const policy = createPolicyCache();
  policy.applyBundle({ policy_version: 'v1', default_mode: 'm0', scope: { 'known.invalid': 'm2' } });
  const r = policy.modeFor({ host: 'unknown.invalid' });
  assert.equal(r.mode, 'm0');
  assert.equal(r.reason, 'scope:(default)');
});

test('most-restrictive wins across every applicable entry, including an m0 default (§11.1)', () => {
  const policy = createPolicyCache();
  policy.applyBundle({
    policy_version: 'v1',
    default_mode: 'm3',
    scope: { '*.example-ai.invalid': 'm1', 'chat.example-ai.invalid': 'm0' },
  });
  // The host matches both patterns and the default; the lowest value is the answer.
  assert.equal(policy.modeFor({ host: 'chat.example-ai.invalid' }).mode, 'm0');
  assert.equal(policy.modeFor({ host: 'api.example-ai.invalid' }).mode, 'm1', 'the wildcard applies');
  assert.equal(policy.modeFor({ host: 'other.invalid' }).mode, 'm3', 'the default applies');

  // The consequence stated plainly in §11.1: the device may apply a MORE restrictive mode than
  // the strictest applicable scope entry, and that over-restriction is the conservative direction.
  const strict = createPolicyCache();
  strict.applyBundle({ policy_version: 'v1', default_mode: 'm1', scope: { 'chat.example-ai.invalid': 'm0', '*.example-ai.invalid': 'm2' } });
  assert.equal(
    strict.modeFor({ host: 'chat.example-ai.invalid' }).mode,
    'm0',
    'the exact entry beats the wildcard even though the wildcard is wider',
  );
  assert.equal(strict.modeFor({ host: 'api.example-ai.invalid' }).mode, 'm2', 'the wildcard alone applies elsewhere');
});

test('the default is a fallback for unmatched hosts, not an override of matched ones', () => {
  // Passing an m0 default and an m2 scope entry must resolve the scoped host to m2. Folding the
  // default into the most-restrictive reduction would make an m0 default silently cancel every
  // scope entry, i.e. make the matrix inert.
  const policy = createPolicyCache();
  policy.applyBundle({ policy_version: 'v1', default_mode: 'm0', scope: { 'chat.example-ai.invalid': 'm2' } });
  assert.equal(policy.modeFor({ host: 'chat.example-ai.invalid' }).mode, 'm2');
  assert.equal(policy.modeFor({ host: 'chat.example-ai.invalid' }).reason, 'scope:chat.example-ai.invalid');
  assert.equal(policy.modeFor({ host: 'unlisted.invalid' }).mode, 'm0');
});

test('a stale bundle resolves downward to M0 rather than continuing at its old mode', () => {
  const c = clock();
  const policy = createPolicyCache({ now: c.now, staleAfterMs: 1000 });
  policy.applyBundle({ policy_version: 'v1', default_mode: 'm3', scope: { 'x.invalid': 'm3' } });
  assert.equal(policy.modeFor({ host: 'x.invalid' }).mode, 'm3');
  c.advance(1001);
  const r = policy.modeFor({ host: 'x.invalid' });
  assert.equal(r.mode, 'm0');
  assert.equal(r.reason, 'bundle_stale');
  assert.equal(r.unsigned, true);
});

test('an unusable bundle keeps the previous one and marks it stale, exactly as §13.3 keeps enforcing', () => {
  const c = clock();
  const policy = createPolicyCache({ now: c.now, staleAfterMs: 10_000 });
  policy.applyBundle({ policy_version: 'v1', default_mode: 'm1', scope: { 'x.invalid': 'm2' } });
  const result = policy.applyBundle({ not_a_bundle: true });
  assert.equal(result.applied, false);
  assert.equal(result.reason, 'unusable_bundle');
  assert.equal(policy.snapshot().policy_version, 'v1', 'the previous bundle is retained');
  assert.equal(policy.snapshot().stale, true, 'and is reported as stale');
  assert.equal(policy.modeFor({ host: 'x.invalid' }).mode, 'm2', 'a stale-but-present bundle keeps resolving');
  assert.equal(policy.applyBundle({ policy_version: 'v1', default_mode: 'm2' }).applied, true);
  assert.equal(policy.snapshot().stale, false, 'a good bundle clears the stale mark');
});

test('an unrecognised scope value is ignored, not defaulted upward', () => {
  const policy = createPolicyCache();
  policy.applyBundle({ policy_version: 'v1', default_mode: 'm1', scope: { 'x.invalid': 'm9' } });
  assert.equal(policy.modeFor({ host: 'x.invalid' }).mode, 'm1', 'the invalid entry is skipped, the default stands');
});

test('a core answer overrides the cache, and an unrecognised answer still resolves downward', () => {
  const policy = createPolicyCache();
  policy.applyBundle({ policy_version: 'v1', default_mode: 'm1' });
  assert.equal(policy.modeFor({ host: 'x.invalid' }, { override: 'm3' }).mode, 'm3');
  assert.equal(policy.modeFor({ host: 'x.invalid' }, { override: 'bogus' }).mode, 'm0');
  assert.equal(policy.modeFor({ host: 'x.invalid' }, { override: 'bogus' }).reason, 'core_answer_unrecognised');
});

test('isBodyBearing is false for an M0 host and false when no policy is loaded', () => {
  const policy = createPolicyCache();
  assert.equal(policy.isBodyBearing('https://anything.invalid/chat'), false, 'absent cache ⇒ do not read the body');
  policy.applyBundle({ policy_version: 'v1', default_mode: 'm1', scope: { 'blocked.invalid': 'm0' } });
  assert.equal(policy.isBodyBearing('https://allowed.invalid/chat'), true);
  assert.equal(policy.isBodyBearing('https://blocked.invalid/chat'), false);
  assert.equal(policy.isBodyBearing('not a url'), false);
});

test('a tool-scoped entry matches on fingerprint, and a wildcard matches a suffix only', () => {
  assert.equal(patternMatches('tool:tf1:abc', 'any.invalid', 'tf1:abc'), true);
  assert.equal(patternMatches('tool:tf1:abc', 'any.invalid', 'tf1:zzz'), false);
  assert.equal(patternMatches('*.example-ai.invalid', 'chat.example-ai.invalid', undefined), true);
  assert.equal(patternMatches('*.example-ai.invalid', 'example-ai.invalid', undefined), true);
  assert.equal(patternMatches('*.example-ai.invalid', 'notexample-ai.invalid', undefined), false);
  assert.equal(patternMatches('exact.invalid', 'exact.invalid', undefined), true);
  assert.equal(patternMatches('exact.invalid', 'sub.exact.invalid', undefined), false);
});

test('the cache never persists: snapshot() exposes only what the running bundle says', () => {
  const policy = createPolicyCache();
  policy.applyBundle({ policy_version: 'v7', default_mode: 'm1', scope: { 'a.invalid': 'm0' } });
  const snap = policy.snapshot();
  assert.equal(snap.policy_version, 'v7');
  assert.deepEqual(snap.m0_patterns, ['a.invalid']);
  policy.clear();
  assert.equal(policy.snapshot().present, false);
  assert.equal(policy.snapshot().policy_version, null);
});

test('§11.3 mode-change attribution: the health report carries the enforcing bundle version', () => {
  const policy = createPolicyCache();
  policy.applyBundle({ policy_version: 'bundle-42', default_mode: 'm1' });
  assert.equal(policy.snapshot().policy_version, 'bundle-42');
});

test('the body-lane include list and the confirmation window are bundle policy, with safe defaults', () => {
  const policy = createPolicyCache();
  assert.deepEqual(policy.bodyLanePatterns(), []);
  assert.equal(policy.confirmationWindowMs(), 20_000);
  policy.applyBundle({
    policy_version: 'v1',
    default_mode: 'm1',
    body_lane_patterns: ['https://chat.example-ai.invalid/*'],
    confirmation_window_ms: 5000,
  });
  assert.deepEqual(policy.bodyLanePatterns(), ['https://chat.example-ai.invalid/*']);
  assert.equal(policy.confirmationWindowMs(), 5000);
});

test('the rules and release state come from the bundle and default to non-enforcing (§9.6)', () => {
  const policy = createPolicyCache();
  assert.deepEqual(policy.rules(), []);
  assert.equal(policy.releaseState(), 'rolled_back', 'no release ⇒ no enforcement');
  policy.applyBundle({ policy_version: 'v1', default_mode: 'm1', rules: [{ rule_id: 'R1', action: 'blocked' }], classifier_release: { version: 'c1', state: 'shadow' } });
  assert.equal(policy.rules().length, 1);
  assert.equal(policy.releaseState(), 'shadow');
});

test('the body cap is bundle policy, and absent a bundle there is no cap to exceed — only M0', () => {
  const policy = createPolicyCache();
  assert.deepEqual(policy.caps(), { body_bytes: null, attachment_bytes: null });
  policy.applyBundle({ policy_version: 'v1', default_mode: 'm1', mode_caps: { body_bytes: 4096, attachment_bytes: 8192 } });
  assert.deepEqual(policy.caps(), { body_bytes: 4096, attachment_bytes: 8192 });
});
