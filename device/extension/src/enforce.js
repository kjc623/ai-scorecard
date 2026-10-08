/**
 * enforce.js — inline warn/block, decided locally against the bundle's enforcement rules.
 *
 *   - The rules and their matching are capture-core's (`capture-core/enforce`): the first rule whose
 *     match matches decides, and no match is `allow` under the rule id `policy.default`. Both
 *     evaluators pass the same cases (device/integration/testdata/enforce/cases.json).
 *   - Every decision carries `decided_locally: true`. `block` records `blocked` and cancels the
 *     request through webRequestBlocking (the caller turns `{cancel: true}` into the blocking
 *     response); `warn` records `warned` once the user confirms; `allow` records `logged`.
 *   - Decision failure fails open: an evaluation error or an exhausted budget lets the request
 *     proceed as `logged`, marked degraded, and the failure is counted. A broken evaluator must
 *     not become a broken browser.
 *
 * A blocked request is still an event: the caller emits its decision.
 */

import { DETAIL } from './messages.js';

/** The budget for reaching a verdict. */
export const DECISION_BUDGET_MS = 300;

/**
 * How long a `warned` request waits for its human, kept separate from the decision budget: a
 * person does not answer inside 300 ms, so one number would make `warned` behave like fail-open.
 */
export const CONFIRMATION_WINDOW_MS = 20_000;

/** The rule id of the decision when no rule matches. */
export const DEFAULT_RULE_ID = 'policy.default';

/** policy.RuleAction: what a rule asks for. */
export const RULE_ACTION = Object.freeze({ ALLOW: 'allow', WARN: 'warn', BLOCK: 'block' });

/** The values a rule's sanction list holds. */
export const SANCTION = Object.freeze({ SANCTIONED: 'sanctioned', UNSANCTIONED: 'unsanctioned' });

/** The closed decision vocabulary (protocol.Action): what the envelope records. */
export const DECISION = Object.freeze({ BLOCKED: 'blocked', WARNED: 'warned', LOGGED: 'logged' });

/** The closed reason vocabulary for this module. Every outcome names itself. */
export const REASON = Object.freeze({
  RULE_BLOCK: 'rule_block',
  RULE_WARN: 'rule_warn',
  RULE_ALLOW: 'rule_allow',
  NO_RULE: 'no_rule',
  WARN_PROCEEDED: 'warn_user_proceeded',
  WARN_CANCELLED: 'warn_user_cancelled',
  WARN_UNANSWERED: 'warn_unanswered',
  FAILED_OPEN: 'failed_open',
  BUDGET_EXCEEDED: 'budget_exceeded',
});

export function nowMs() {
  const p = globalThis.performance;
  return p && typeof p.now === 'function' ? p.now() : Date.now();
}

/**
 * A deadline over an injected clock. `sync(work)` and `run(work)` report whether the work finished
 * inside the budget; the work itself may check `expired()`.
 */
export function createBudget({ budgetMs = DECISION_BUDGET_MS, now = nowMs } = {}) {
  const startedAt = now();
  const expired = () => now() - startedAt >= budgetMs;
  const elapsed = () => now() - startedAt;
  return {
    budgetMs,
    startedAt,
    elapsed,
    expired,
    start: () => startedAt,
    async run(work) {
      const value = await work({ elapsed, expired });
      return { value, elapsed_ms: round(elapsed()), expired: expired() };
    },
    sync(work) {
      const value = work({ elapsed, expired });
      return { value, elapsed_ms: round(elapsed()), expired: expired() };
    },
  };
}

/**
 * The first rule in `bundle` that matches `input`, or the default allow. A null bundle has no rules.
 *
 * @param {{rules?: object[], sanctioned_tools?: string[]}|null} bundle
 * @param {{route?: string, tool_fingerprint?: string, labels?: string[], labels_known?: boolean}} input
 *   `labels_known` is false when no classification completed: the absence of a label then says
 *   nothing, so a rule that lists labels does not match.
 * @returns {{rule_id: string, action: string, message: string, link: string}}
 */
export function evaluate(bundle, input) {
  const rules = bundle && bundle.rules;
  if (rules !== undefined && rules !== null && !Array.isArray(rules)) throw new TypeError('rules is not a list');
  for (const rule of rules || []) {
    if (matches(bundle, (rule && rule.match) || {}, input || {})) {
      return { rule_id: rule.rule_id, action: rule.action, message: rule.message || '', link: rule.link || '' };
    }
  }
  return { rule_id: DEFAULT_RULE_ID, action: RULE_ACTION.ALLOW, message: '', link: '' };
}

/** AND across the non-empty lists, OR inside each. */
function matches(bundle, match, input) {
  const labels = list(match.labels);
  const tools = list(match.tools);
  const categories = list(match.categories);
  const sanction = list(match.sanction);
  const routes = list(match.routes);
  // A list that is present but not a list makes the rule not match, never match everything.
  if ([labels, tools, categories, sanction, routes].includes(null)) return false;

  if (labels.length > 0) {
    const found = Array.isArray(input.labels) ? input.labels : [];
    if (!input.labels_known || !found.some((l) => labels.includes(l))) return false;
  }
  if (tools.length > 0 && !tools.includes(input.tool_fingerprint)) return false;
  // Categories resolve through the bundle's app catalog, which the bundle does not carry yet, so no
  // tool has a known category and a rule that lists categories never matches.
  if (categories.length > 0) return false;
  if (sanction.length > 0) {
    const sanctioned = Array.isArray(bundle.sanctioned_tools) && bundle.sanctioned_tools.includes(input.tool_fingerprint);
    if (!sanction.includes(sanctioned ? SANCTION.SANCTIONED : SANCTION.UNSANCTIONED)) return false;
  }
  if (routes.length > 0 && !routes.includes(input.route)) return false;
  return true;
}

/** A match list as an array: absent and null are empty, anything else that is not a list is null. */
function list(v) {
  if (v === undefined || v === null) return [];
  return Array.isArray(v) ? v : null;
}

function decide(action, ruleId, reason, extra = {}) {
  return {
    decision: { action, rule_id: ruleId, decided_locally: true },
    reason,
    degraded: false,
    error_counted: false,
    over_budget: false,
    rule_id: ruleId,
    message: '',
    link: '',
    needs_confirmation: false,
    cancel: false,
    ...extra,
  };
}

/**
 * The synchronous half: decide without asking anyone, inside the decision budget.
 *
 * @param {object} input
 * @param {{rules?: object[], sanctioned_tools?: string[]}} input.bundle   from the bundle in force
 * @param {string} input.route
 * @param {string} input.tool_fingerprint
 * @param {string[]} [input.labels]
 * @param {boolean} [input.labels_known]
 * @param {number} [input.budgetMs]
 * @param {() => number} [input.now]
 */
export function decideSync(input) {
  const budget = createBudget({ budgetMs: input.budgetMs || DECISION_BUDGET_MS, now: input.now || nowMs });
  let outcome;
  try {
    outcome = budget.sync(() =>
      evaluate(input.bundle, {
        route: input.route,
        tool_fingerprint: input.tool_fingerprint,
        labels: input.labels,
        labels_known: Boolean(input.labels_known),
      }),
    );
  } catch (e) {
    // An evaluation error fails open.
    return decide(DECISION.LOGGED, DEFAULT_RULE_ID, REASON.FAILED_OPEN, {
      degraded: true,
      error_counted: true,
      error: String((e && e.message) || e),
      elapsed_ms: round(budget.elapsed()),
    });
  }

  const d = outcome.value;
  const elapsed = outcome.elapsed_ms;
  if (outcome.expired) {
    return decide(DECISION.LOGGED, d.rule_id, REASON.BUDGET_EXCEEDED, {
      degraded: true,
      error_counted: true,
      over_budget: true,
      elapsed_ms: elapsed,
    });
  }

  const rule = { message: d.message, link: d.link, elapsed_ms: elapsed };
  switch (d.action) {
    case RULE_ACTION.BLOCK:
      return decide(DECISION.BLOCKED, d.rule_id, REASON.RULE_BLOCK, { ...rule, cancel: true });
    case RULE_ACTION.WARN:
      return decide(DECISION.WARNED, d.rule_id, REASON.RULE_WARN, { ...rule, needs_confirmation: true });
    case RULE_ACTION.ALLOW:
      return decide(DECISION.LOGGED, d.rule_id, d.rule_id === DEFAULT_RULE_ID ? REASON.NO_RULE : REASON.RULE_ALLOW, rule);
    default:
      return decide(DECISION.LOGGED, d.rule_id, REASON.FAILED_OPEN, {
        degraded: true,
        error_counted: true,
        error: `unknown_action:${String(d.action)}`,
        elapsed_ms: elapsed,
      });
  }
}

/**
 * The asynchronous half: a `warned` decision, rendered before the request proceeds, with the
 * user's answer recorded as part of the decision.
 *
 * Rule evaluation is bounded by the decision budget; the wait for a human by the confirmation
 * window. Exceeding either, or an unanswered prompt, fails open as a degraded `logged`, never a
 * silent block and never an indefinitely held request.
 *
 * @param {object} input                 as for decideSync, plus:
 * @param {(decision: object) => Promise<{proceeded: boolean, answered: boolean, reason: string}>} input.confirm
 *   shows the rule's message and link and waits for the answer
 * @param {number} [input.confirmationWindowMs]
 * @returns {Promise<object>}
 */
export async function decideWithConfirmation(input) {
  const evalBudget = createBudget({ budgetMs: input.budgetMs || DECISION_BUDGET_MS, now: input.now || nowMs });
  // decideSync takes its own budget reading; the outer budget measures the whole evaluation and
  // is what decides whether the 300 ms was respected.
  const decision = decideSync({ ...input });

  if (decision.over_budget || evalBudget.expired()) {
    return {
      ...decision,
      decision: { action: DECISION.LOGGED, rule_id: decision.rule_id, decided_locally: true },
      reason: REASON.BUDGET_EXCEEDED,
      degraded: true,
      error_counted: true,
      over_budget: true,
      confirmed: null,
      user_answer: null,
      needs_confirmation: false,
      cancel: false,
      elapsed_ms: round(evalBudget.elapsed()),
    };
  }

  if (!decision.needs_confirmation) {
    return { ...decision, confirmed: null, user_answer: null };
  }

  const windowMs = input.confirmationWindowMs || CONFIRMATION_WINDOW_MS;
  const confirmBudget = createBudget({ budgetMs: windowMs, now: input.now || nowMs });

  let answer;
  try {
    answer = await confirmBudget.run(() => input.confirm(decision));
  } catch (e) {
    return {
      ...decision,
      decision: { action: DECISION.LOGGED, rule_id: decision.rule_id, decided_locally: true },
      reason: REASON.FAILED_OPEN,
      degraded: true,
      error_counted: true,
      confirmed: null,
      user_answer: null,
      needs_confirmation: false,
      cancel: false,
      error: String((e && e.message) || e),
      elapsed_ms: round(evalBudget.elapsed()),
      confirmation_elapsed_ms: round(confirmBudget.elapsed()),
    };
  }

  const a = answer.value || { proceeded: true, answered: false, reason: 'no_receiver' };
  const unanswered = answer.expired || !a.answered;

  if (unanswered) {
    return {
      ...decision,
      decision: { action: DECISION.LOGGED, rule_id: decision.rule_id, decided_locally: true },
      reason: answer.expired ? REASON.BUDGET_EXCEEDED : REASON.WARN_UNANSWERED,
      degraded: true,
      error_counted: true,
      over_budget: true,
      confirmed: false,
      user_answer: a.reason,
      needs_confirmation: false,
      cancel: false,
      elapsed_ms: round(evalBudget.elapsed()),
      confirmation_elapsed_ms: round(confirmBudget.elapsed()),
    };
  }

  if (a.proceeded) {
    return {
      ...decision,
      decision: { action: DECISION.WARNED, rule_id: decision.rule_id, decided_locally: true },
      reason: REASON.WARN_PROCEEDED,
      confirmed: true,
      user_answer: 'proceeded',
      needs_confirmation: false,
      cancel: false,
      elapsed_ms: round(evalBudget.elapsed()),
      confirmation_elapsed_ms: round(confirmBudget.elapsed()),
    };
  }

  return {
    ...decision,
    decision: { action: DECISION.BLOCKED, rule_id: decision.rule_id, decided_locally: true },
    reason: REASON.WARN_CANCELLED,
    confirmed: true,
    user_answer: 'cancelled',
    needs_confirmation: false,
    cancel: true,
    elapsed_ms: round(evalBudget.elapsed()),
    confirmation_elapsed_ms: round(confirmBudget.elapsed()),
  };
}

/**
 * `Detail` for a degraded decision: an exhausted budget is `budget_exhausted`; anything else (a
 * prompt nobody answered, an evaluation error) is `classifier_unavailable`.
 */
export function degradedDetail(outcome) {
  return outcome.reason === REASON.BUDGET_EXCEEDED ? DETAIL.BUDGET_EXHAUSTED : DETAIL.CLASSIFIER_UNAVAILABLE;
}

function round(n) {
  return Math.round(n * 1000) / 1000;
}
