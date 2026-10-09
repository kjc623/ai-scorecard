/**
 * test/enforce.test.mjs — inline warn/block: the bundle's rules matched as capture-core matches
 * them, local decisions, `blocked` cancels, `warned` waits for an explicit answer, and a decision
 * that fails or exceeds its budget fails open as degraded rather than blocking.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import {
  CONFIRMATION_WINDOW_MS,
  DECISION_BUDGET_MS,
  DECISION,
  DEFAULT_RULE_ID,
  REASON,
  createBudget,
  decideSync,
  decideWithConfirmation,
  degradedDetail,
  evaluate,
} from '../src/enforce.js';

/** The evaluator's cases, shared with capture-core/enforce's Go tests. */
const SHARED_CASES = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..', 'integration', 'testdata', 'enforce', 'cases.json');

test('the shared evaluator cases: the extension matches rules exactly as capture-core does', async (t) => {
  const { cases } = JSON.parse(readFileSync(SHARED_CASES, 'utf8'));
  assert.ok(cases.length > 0, 'the shared case file holds cases');
  for (const c of cases) {
    await t.test(c.name, () => {
      const want = { rule_id: c.want.rule_id, action: c.want.action, message: c.want.message || '', link: c.want.link || '' };
      assert.deepEqual(evaluate(c.bundle, c.input), want);
    });
  }
});

test('a null bundle has no rules', () => {
  assert.deepEqual(evaluate(null, { route: 'ext.web_request' }), { rule_id: DEFAULT_RULE_ID, action: 'allow', message: '', link: '' });
});

const rule = (rule_id, action, match = {}) => ({ rule_id, action, match, message: `msg ${rule_id}`, link: `https://intranet.example/${rule_id}` });
const BLOCK_RULE = rule('block_tool', 'block', { tools: ['tf1:blocked'] });
const WARN_RULE = rule('warn_tool', 'warn', { tools: ['tf1:warned'] });
const ALLOW_RULE = rule('allow_tool', 'allow', { tools: ['tf1:allowed'] });

function base(overrides = {}) {
  return {
    bundle: { rules: [BLOCK_RULE, WARN_RULE, ALLOW_RULE], sanctioned_tools: [] },
    route: 'ext.web_request',
    tool_fingerprint: 'tf1:other',
    labels: [],
    labels_known: false,
    ...overrides,
  };
}

test('no matching rule is `logged` under policy.default with decided_locally true', () => {
  const r = decideSync(base());
  assert.equal(r.decision.action, DECISION.LOGGED);
  assert.equal(r.decision.rule_id, 'policy.default');
  assert.equal(r.decision.decided_locally, true);
  assert.equal(r.reason, REASON.NO_RULE);
  assert.equal(r.cancel, false);
  assert.equal(r.degraded, false);
});

test('a matching block rule cancels the request and carries the rule\'s message and link', () => {
  const r = decideSync(base({ tool_fingerprint: 'tf1:blocked' }));
  assert.equal(r.decision.action, DECISION.BLOCKED);
  assert.equal(r.decision.rule_id, 'block_tool');
  assert.equal(r.decision.decided_locally, true);
  assert.equal(r.cancel, true, 'blocked cancels through webRequestBlocking');
  assert.equal(r.message, 'msg block_tool');
  assert.equal(r.link, 'https://intranet.example/block_tool');
  assert.equal(r.degraded, false);
});

test('a matching warn rule requires confirmation and does not cancel on its own', () => {
  const r = decideSync(base({ tool_fingerprint: 'tf1:warned' }));
  assert.equal(r.decision.action, DECISION.WARNED);
  assert.equal(r.needs_confirmation, true);
  assert.equal(r.cancel, false, 'the answer decides, not the rule');
  assert.equal(r.message, 'msg warn_tool');
});

test('a matching allow rule records logged under its own id and cancels nothing', () => {
  const r = decideSync(base({ tool_fingerprint: 'tf1:allowed' }));
  assert.equal(r.decision.action, DECISION.LOGGED);
  assert.equal(r.decision.rule_id, 'allow_tool');
  assert.equal(r.reason, REASON.RULE_ALLOW);
  assert.equal(r.cancel, false);
});

test('rules are always enforcing: there is no release state that keeps a block from acting', () => {
  const r = decideSync(base({ tool_fingerprint: 'tf1:blocked', release_state: 'shadow' }));
  assert.equal(r.decision.action, DECISION.BLOCKED);
  assert.equal(r.cancel, true);
});

test('a warned decision shows the rule\'s message and records the user answer — twice sent is two events', async () => {
  let shown = null;
  const yes = await decideWithConfirmation({
    ...base({ tool_fingerprint: 'tf1:warned' }),
    confirm: async (decision) => {
      shown = decision;
      return { proceeded: true, answered: true, reason: 'user_proceeded' };
    },
  });
  assert.equal(shown.message, 'msg warn_tool', 'the confirmation is given the rule\'s message');
  assert.equal(shown.link, 'https://intranet.example/warn_tool', 'and its link');
  assert.equal(yes.decision.action, DECISION.WARNED);
  assert.equal(yes.confirmed, true);
  assert.equal(yes.user_answer, 'proceeded');
  assert.equal(yes.cancel, false);
  assert.equal(yes.degraded, false);

  const no = await decideWithConfirmation({
    ...base({ tool_fingerprint: 'tf1:warned' }),
    confirm: async () => ({ proceeded: false, answered: true, reason: 'user_cancelled' }),
  });
  assert.equal(no.decision.action, DECISION.BLOCKED, 'a cancelled warning blocks the request');
  assert.equal(no.user_answer, 'cancelled');
  assert.equal(no.confirmed, true);
  assert.equal(no.cancel, true);
});

test('an unanswered warning fails OPEN, degraded, and never silently blocks', async () => {
  const r = await decideWithConfirmation({
    ...base({ tool_fingerprint: 'tf1:warned' }),
    confirm: async () => ({ proceeded: true, answered: false, reason: 'no_receiver' }),
  });
  assert.equal(r.decision.action, DECISION.LOGGED, 'a broken evaluator must not become a broken browser');
  assert.equal(r.cancel, false);
  assert.equal(r.degraded, true);
  assert.equal(r.error_counted, true);
  assert.equal(r.reason, REASON.WARN_UNANSWERED);
});

test('a confirmation that exhausts the window fails open as degraded with the budget reason', async () => {
  let t = 0;
  const r = await decideWithConfirmation({
    ...base({ tool_fingerprint: 'tf1:warned' }),
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
    ...base({ tool_fingerprint: 'tf1:warned' }),
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

test('an evaluation error fails open: `logged`, degraded, error counted', () => {
  // A rule list that cannot be iterated: the failure must not leave the request path.
  const r = decideSync(base({ bundle: { rules: { not: 'an array' } } }));
  assert.equal(r.decision.action, DECISION.LOGGED);
  assert.equal(r.cancel, false);
  assert.equal(r.degraded, true);
  assert.equal(r.error_counted, true);
  assert.ok(r.error.length > 0);
});

test('a match list that is not a list makes the rule not match, never match everything', () => {
  const r = decideSync(base({ bundle: { rules: [rule('broken', 'block', { tools: 'tf1:other' }), rule('good', 'warn', { tools: ['tf1:other'] })] } }));
  assert.equal(r.decision.rule_id, 'good', 'the broken rule is passed over and a later good rule still applies');
  assert.equal(r.decision.action, DECISION.WARNED);
});

test('an evaluation that exceeds the 300 ms budget fails open with Detail budget_exhausted', () => {
  let t = 0;
  const r = decideSync(base({ tool_fingerprint: 'tf1:blocked', now: () => (t += 400) }));
  assert.equal(r.decision.action, DECISION.LOGGED);
  assert.equal(r.cancel, false);
  assert.equal(r.over_budget, true);
  assert.equal(r.degraded, true);
  assert.equal(r.reason, REASON.BUDGET_EXCEEDED);
  assert.equal(degradedDetail(r), 'budget_exhausted', 'the Detail value is device/protocol/envelope.go\'s, not a local spelling');
});

test('the decision budget is 300 ms', () => {
  assert.equal(DECISION_BUDGET_MS, 300);
  const b = createBudget({ budgetMs: 300, now: () => 0 });
  assert.equal(b.expired(), false);
});

test('an unknown action in a bundle fails open rather than inventing a decision', () => {
  const r = decideSync(base({ bundle: { rules: [rule('x', 'quarantine')] } }));
  assert.equal(r.decision.action, DECISION.LOGGED);
  assert.equal(r.degraded, true);
  assert.equal(r.error_counted, true);
  assert.match(r.error, /unknown_action/);
});

test('the confirmation window is an extension constant, separate from the decision budget', () => {
  // A confirmation sharing the 300 ms decision budget would make `warned` behave like fail-open.
  assert.equal(CONFIRMATION_WINDOW_MS, 20_000);
  assert.ok(CONFIRMATION_WINDOW_MS > DECISION_BUDGET_MS);
});
