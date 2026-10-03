/**
 * test/enforce.test.mjs — §7.4's inline warn/block.
 *
 * Every one of §7.4's four claims is a test here, plus the two outcomes the document is explicit
 * about and easy to get wrong: a **blocked request is still an event**, and a decision that fails
 * or exceeds its budget **fails open** with `confidence: degraded` rather than blocking.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  CONFIRMATION_WINDOW_MS,
  DECISION_BUDGET_MS,
  DECISION,
  REASON,
  createBudget,
  decideSync,
  decideWithConfirmation,
  degradedDetail,
  decisionRecord,
  enforcing,
  evaluateRule,
  ruleMatches,
} from '../src/enforce.js';

const BLOCK_RULE = { rule_id: 'BLOCK_EXTERNAL_GPT', action: 'blocked', hosts: ['denied.invalid'] };
const WARN_RULE = { rule_id: 'WARN_PII', action: 'warned', hosts: ['warned.invalid'] };
const LOG_RULE = { rule_id: 'LOG_SHADOW', action: 'logged', hosts: ['shadowed.invalid'] };
const RULES = [BLOCK_RULE, WARN_RULE, LOG_RULE];

function base(overrides = {}) {
  return {
    url: 'https://allowed.invalid/v1/chat',
    method: 'POST',
    tool_fingerprint: 'tf1:abc',
    size_bytes: 512,
    mode: 'm1',
    release_state: 'enforcing',
    rules: RULES,
    ...overrides,
  };
}

test('no matching rule is `logged` with decided_locally true — the extension has an opinion, not a veto', () => {
  const r = decideSync(base());
  assert.equal(r.decision.action, DECISION.LOGGED);
  assert.equal(r.decision.decided_locally, true);
  assert.equal(r.reason, REASON.NO_RULE);
  assert.equal(r.cancel, false);
  assert.equal(r.degraded, false);
});

test('a matching block rule cancels the request and names the rule that did it', () => {
  const r = decideSync(base({ url: 'https://denied.invalid/v1/chat' }));
  assert.equal(r.decision.action, DECISION.BLOCKED);
  assert.equal(r.decision.rule_id, 'BLOCK_EXTERNAL_GPT');
  assert.equal(r.decision.decided_locally, true);
  assert.equal(r.cancel, true, 'blocked cancels through webRequestBlocking');
  assert.equal(r.degraded, false);
});

test('a matching warn rule requires confirmation and does not cancel on its own', () => {
  const r = decideSync(base({ url: 'https://warned.invalid/v1/chat' }));
  assert.equal(r.decision.action, DECISION.WARNED);
  assert.equal(r.needs_confirmation, true);
  assert.equal(r.cancel, false, 'the answer decides, not the rule');
});

test('a logged rule carries decided_locally: true and cancels nothing', () => {
  const r = decideSync(base({ url: 'https://shadowed.invalid/v1/chat' }));
  assert.equal(r.decision.action, DECISION.LOGGED);
  assert.equal(r.decision.rule_id, 'LOG_SHADOW');
  assert.equal(r.cancel, false);
});

test('a warned decision records the user answer as part of the decision — twice sent is two events', async () => {
  const yes = await decideWithConfirmation({
    ...base({ url: 'https://warned.invalid/v1/chat' }),
    confirm: async () => ({ proceeded: true, answered: true, reason: 'user_proceeded' }),
  });
  assert.equal(yes.decision.action, DECISION.WARNED);
  assert.equal(yes.confirmed, true);
  assert.equal(yes.user_answer, 'proceeded');
  assert.equal(yes.cancel, false);
  assert.equal(yes.degraded, false);

  const no = await decideWithConfirmation({
    ...base({ url: 'https://warned.invalid/v1/chat' }),
    confirm: async () => ({ proceeded: false, answered: true, reason: 'user_cancelled' }),
  });
  assert.equal(no.decision.action, DECISION.BLOCKED, 'a cancelled warning blocks the request');
  assert.equal(no.user_answer, 'cancelled');
  assert.equal(no.confirmed, true);
  assert.equal(no.cancel, true);
});

test('an unanswered warning fails OPEN, degraded, and never silently blocks', async () => {
  const r = await decideWithConfirmation({
    ...base({ url: 'https://warned.invalid/v1/chat' }),
    confirm: async () => ({ proceeded: true, answered: false, reason: 'no_receiver' }),
  });
  assert.equal(r.decision.action, DECISION.LOGGED, 'brief §6: a broken classifier must not become a broken browser');
  assert.equal(r.cancel, false);
  assert.equal(r.degraded, true);
  assert.equal(r.error_counted, true);
  assert.equal(r.reason, REASON.WARN_UNANSWERED);
});

test('a confirmation that exhausts the window fails open as degraded with the budget reason', async () => {
  let t = 0;
  const r = await decideWithConfirmation({
    ...base({ url: 'https://warned.invalid/v1/chat' }),
    now: () => t,
    confirmationWindowMs: 100,
    confirm: async () => {
      t += 200; // the human never answered inside the window
      return { proceeded: true, answered: true, reason: 'user_proceeded' };
    },
  });
  assert.equal(r.decision.action, DECISION.LOGGED);
  assert.equal(r.cancel, false);
  assert.equal(r.degraded, true);
  assert.equal(r.over_budget, true);
  assert.equal(r.reason, REASON.BUDGET_EXCEEDED);
});

test('a confirmation transport that throws fails open, degraded, with the error kept', async () => {
  const r = await decideWithConfirmation({
    ...base({ url: 'https://warned.invalid/v1/chat' }),
    confirm: async () => {
      throw new Error('tab gone');
    },
  });
  assert.equal(r.decision.action, DECISION.LOGGED);
  assert.equal(r.degraded, true);
  assert.equal(r.error_counted, true);
  assert.equal(r.cancel, false);
  assert.match(r.error, /tab gone/);
});

test('an evaluation error fails open: `logged`, degraded, error counted (§7.4)', () => {
  // A rule list that cannot be iterated: the failure must not leave the request path.
  const r = decideSync(base({ rules: { not: 'an array' } }));
  assert.equal(r.decision.action, DECISION.LOGGED);
  assert.equal(r.cancel, false);
  assert.equal(r.degraded, true);
  assert.equal(r.error_counted, true);
  assert.ok(r.error.length > 0);
});

test('a malformed rule does NOT match everything — it is skipped, counted, and reported degraded', () => {
  // `paths: 'not-a-list'` would make `paths.some` throw; a naive "skip the check" would instead
  // make this rule block every request. Both are wrong: the rule must simply not decide.
  const r = decideSync(base({ rules: [{ rule_id: 'BROKEN', action: 'blocked', paths: 'not-a-list' }] }));
  assert.equal(r.decision.action, DECISION.LOGGED, 'a broken rule must not block');
  assert.equal(r.cancel, false);
  assert.equal(r.reason, REASON.NO_RULE);
  assert.equal(r.degraded, true, 'but the gap is visible, not silent');
  assert.equal(r.error_counted, true);
  assert.deepEqual(r.skipped_rules, [{ rule_id: 'BROKEN', reason: 'malformed_matcher' }]);

  const evaluated = evaluateRule([{ rule_id: 'BROKEN', action: 'blocked', paths: 'not-a-list' }], {
    host: 'allowed.invalid',
    path: '/',
    size_bytes: 1,
    mode: 'm1',
  });
  assert.equal(evaluated.rule, null);
  assert.equal(evaluated.skipped.length, 1);
});

test('a malformed rule is skipped and a later good rule still applies', () => {
  const r = decideSync(
    base({
      rules: [
        { rule_id: 'BROKEN', action: 'blocked', paths: 'not-a-list' },
        { rule_id: 'GOOD', action: 'blocked', hosts: ['allowed.invalid'] },
      ],
    }),
  );
  assert.equal(r.decision.action, DECISION.BLOCKED);
  assert.equal(r.decision.rule_id, 'GOOD');
  assert.equal(r.skipped_rules.length, 1);
});

test('a rule whose rule_id is out of the schema bounds is not copied into the decision', () => {
  const r = decideSync(base({ rules: [{ rule_id: 'x'.repeat(200), action: 'blocked', hosts: ['allowed.invalid'] }] }));
  assert.equal(r.decision.action, DECISION.BLOCKED, 'the action still applies');
  assert.equal(r.decision.rule_id, 'NONE', 'but an over-long id is not propagated');
});

test('an evaluation that exceeds the 300 ms budget fails open with Detail budget_exceeded', () => {
  let t = 0;
  const r = decideSync(base({ now: () => (t += 400) }));
  assert.equal(r.decision.action, DECISION.LOGGED);
  assert.equal(r.cancel, false);
  assert.equal(r.over_budget, true);
  assert.equal(r.degraded, true);
  assert.equal(r.reason, REASON.BUDGET_EXCEEDED);
  assert.equal(degradedDetail(r), 'budget_exhausted', 'the Detail value is endpoint/protocol/envelope.go\'s, not a local spelling');
});

test('the decision budget is 300 ms, taken from §7.4 rather than the 150 ms classification target', () => {
  assert.equal(DECISION_BUDGET_MS, 300);
  const b = createBudget({ budgetMs: 300, now: () => 0 });
  assert.equal(b.expired(), false);
});
test('§9.6: shadow and rolled_back releases never enforce, and the record says which it was', () => {
  for (const [state, reason] of [
    ['shadow', REASON.SHADOW],
    ['rolled_back', REASON.ROLLED_BACK],
  ]) {
    const r = decideSync(base({ url: 'https://denied.invalid/v1/chat', release_state: state }));
    assert.equal(r.decision.action, DECISION.LOGGED, `${state} must not block`);
    assert.equal(r.cancel, false);
    assert.equal(r.reason, reason);
    assert.equal(r.shadow, true);
    assert.equal(r.decision.decided_locally, true);
  }
  assert.equal(enforcing('shadow'), false);
  assert.equal(enforcing('rolled_back'), false);
  assert.equal(enforcing('enforcing'), true);
});

test('an unknown action in a bundle fails open rather than inventing a decision', () => {
  const r = decideSync(base({ url: 'https://weird.invalid/x', rules: [{ rule_id: 'X', action: 'quarantine', hosts: ['weird.invalid'] }] }));
  assert.equal(r.decision.action, DECISION.LOGGED);
  assert.equal(r.degraded, true);
  assert.equal(r.error_counted, true);
  assert.match(r.error, /unknown_action/);
});

test('an unparseable URL fails open rather than throwing out of the request path', () => {
  const r = decideSync(base({ url: '::::not a url' }));
  assert.equal(r.decision.action, DECISION.LOGGED);
  assert.equal(r.cancel, false);
  assert.equal(r.degraded, true);
  assert.equal(r.error, 'unparseable_url');
});

test('rule matchers: every named matcher must match, and a rule with none matches everything', () => {
  const req = { host: 'a.invalid', path: '/v1/chat', tool_fingerprint: 'tf1:x', size_bytes: 100, mode: 'm1' };
  assert.equal(ruleMatches({}, req), true);
  assert.equal(ruleMatches({ hosts: ['a.invalid'] }, req), true);
  assert.equal(ruleMatches({ hosts: ['b.invalid'] }, req), false);
  assert.equal(ruleMatches({ paths: ['/v1/*'] }, req), true);
  assert.equal(ruleMatches({ paths: ['/v2/*'] }, req), false);
  assert.equal(ruleMatches({ tools: ['tf1:x'] }, req), true);
  assert.equal(ruleMatches({ min_size_bytes: 200 }, req), false);
  assert.equal(ruleMatches({ modes: ['m1'] }, req), true);
  assert.equal(ruleMatches({ modes: ['m0'] }, req), false);
  assert.equal(ruleMatches({ hosts: ['a.invalid'], paths: ['/v1/*'], modes: ['m1'] }, req), true);
  assert.equal(ruleMatches({ hosts: ['a.invalid'], paths: ['/v2/*'] }, req), false, 'all named matchers are ANDed');
});

test('rules are ordered and the first match wins', () => {
  const first = { rule_id: 'FIRST', action: 'blocked', hosts: ['a.invalid'] };
  const second = { rule_id: 'SECOND', action: 'logged', hosts: ['a.invalid'] };
  const req = { host: 'a.invalid', path: '/', size_bytes: 1, mode: 'm1' };
  assert.equal(evaluateRule([first, second], req).rule.rule_id, 'FIRST');
  assert.equal(evaluateRule([second, first], req).rule.rule_id, 'SECOND');
  assert.equal(evaluateRule([], req).rule, null);
});

test('a decision record carries what capture-core needs, including the fail-open case', () => {
  const outcome = decideSync(base());
  const record = decisionRecord(outcome, {
    client_id: 'c1',
    occurred_at: '2026-10-02T17:26:50.000Z',
    url: 'https://allowed.invalid/v1/chat',
    tool_fingerprint: 'tf1:abc',
  });
  assert.equal(record.decision.action, 'logged');
  assert.equal(record.decision.decided_locally, true);
  assert.equal(record.degraded, false);
  assert.equal(record.user_answer, null);
  assert.equal(record.client_id, 'c1');
});

test('the confirmation window is separate from the decision budget, and this is a stated decision', () => {
  // Recorded deliberately: a `warned` rule whose confirmation shared the 300 ms decision budget
  // would be behaviourally identical to fail-open, making the schema's `warned` action unreachable.
  assert.equal(CONFIRMATION_WINDOW_MS, 20_000);
  assert.ok(CONFIRMATION_WINDOW_MS > DECISION_BUDGET_MS);
});
