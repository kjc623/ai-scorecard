/**
 * enforce.js — inline warn/block, decided locally against the bundle's rules.
 *
 *   - The decision is local: rules come from the signed bundle the extension already holds, and
 *     every decision carries `decided_locally: true`.
 *   - `blocked` cancels the request through webRequestBlocking (the caller turns
 *     `{cancel: true}` into the blocking response).
 *   - `warned` requires an explicit user confirmation before the request proceeds, and the answer
 *     is recorded as part of the decision.
 *   - Decision failure fails open: an evaluation error or an exhausted budget lets the request
 *     proceed as `logged`, marked degraded, and the failure is counted. A broken classifier must
 *     not become a broken browser.
 *
 * Only an `enforcing` classifier release acts; `shadow` and `rolled_back` compute the decision and
 * record it as `logged`, marked so "we chose not to block" differs from "we may not block". A
 * blocked request is still an event: the caller emits its decision.
 */

import { DETAIL } from './messages.js';

/** The budget for reaching a verdict. */
export const DECISION_BUDGET_MS = 300;

/**
 * How long a `warned` request waits for its human, kept separate from the decision budget: a
 * person does not answer inside 300 ms, so one number would make `warned` behave like fail-open.
 * The bundle can override it (`confirmation_window_ms`).
 */
export const CONFIRMATION_WINDOW_MS = 20_000;

/** The closed decision vocabulary (protocol.Action). */
export const DECISION = Object.freeze({ BLOCKED: 'blocked', WARNED: 'warned', LOGGED: 'logged' });

/** The closed reason vocabulary for this module. Every outcome names itself. */
export const REASON = Object.freeze({
  RULE_BLOCK: 'rule_block',
  RULE_WARN: 'rule_warn',
  RULE_LOG: 'rule_log',
  NO_RULE: 'no_rule',
  UNKNOWN_TOOL: 'unknown_tool',
  SHADOW: 'shadow_release',
  ROLLED_BACK: 'rolled_back_release',
  NO_BUNDLE: 'no_bundle',
  WARN_PROCEEDED: 'warn_user_proceeded',
  WARN_CANCELLED: 'warn_user_cancelled',
  WARN_UNANSWERED: 'warn_unanswered',
  FAILED_OPEN: 'failed_open',
  BUDGET_EXCEEDED: 'budget_exceeded',
  INITIAL: 'initial',
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
 * A rule matches when every matcher it names matches. `mode` is the strictest mode the rule may act at.
 *
 * A matcher that is present but malformed makes the rule not match, rather than matching
 * everything: a broken rule must not become an unqualified block. `evaluateRule` reports the skip
 * so it is counted.
 */
export function ruleMatches(rule, { host, path, tool_fingerprint: tool, size_bytes: size, mode }) {
  if (!rule || typeof rule !== 'object') return false;
  if (rule.hosts !== undefined) {
    if (!Array.isArray(rule.hosts) || rule.hosts.length === 0) return false;
    if (!rule.hosts.some((h) => hostMatches(h, host))) return false;
  }
  if (rule.paths !== undefined) {
    if (!Array.isArray(rule.paths) || rule.paths.length === 0) return false;
    if (!rule.paths.some((p) => pathMatches(p, path))) return false;
  }
  if (rule.tools !== undefined) {
    if (!Array.isArray(rule.tools) || rule.tools.length === 0) return false;
    if (!rule.tools.includes(tool)) return false;
  }
  if (rule.modes !== undefined) {
    if (!Array.isArray(rule.modes) || rule.modes.length === 0) return false;
    if (!rule.modes.includes(mode)) return false;
  }
  if (rule.min_size_bytes !== undefined) {
    if (!Number.isFinite(rule.min_size_bytes)) return false;
    if (!(size >= rule.min_size_bytes)) return false;
  }
  return true;
}

function hostMatches(pattern, host) {
  const p = String(pattern || '').toLowerCase();
  if (p === '*' || p === '<all_urls>') return true;
  if (p.startsWith('*.')) {
    const suffix = p.slice(2);
    return host === suffix || String(host).endsWith(`.${suffix}`);
  }
  return p === host;
}

function pathMatches(pattern, path) {
  if (pattern === '*') return true;
  const p = String(pattern);
  if (p.endsWith('*')) return String(path).startsWith(p.slice(0, -1));
  if (p.startsWith('*')) return String(path).endsWith(p.slice(1));
  return String(path).includes(p);
}

/**
 * Rules are ordered; the first match wins, which is what makes a bundle's ordering meaningful.
 *
 * @returns {{rule: object|null, skipped: Array<{rule_id: string, reason: string}>}} `skipped` is
 * non-empty when the bundle held a rule this build could not evaluate. The caller counts it —
 * a rule the device cannot apply is a coverage fact, never a silent no-op.
 */
export function evaluateRule(rules, req) {
  if (!Array.isArray(rules)) throw new TypeError('rules is not a list');
  const skipped = [];
  for (const rule of rules) {
    if (!rule || typeof rule !== 'object') {
      skipped.push({ rule_id: 'NONE', reason: 'not_an_object' });
      continue;
    }
    if (hasMalformedMatcher(rule)) {
      skipped.push({ rule_id: safeRuleId(rule), reason: 'malformed_matcher' });
      continue;
    }
    if (ruleMatches(rule, req)) return { rule, skipped };
  }
  return { rule: null, skipped };
}

function hasMalformedMatcher(rule) {
  for (const key of ['hosts', 'paths', 'tools', 'modes']) {
    if (rule[key] !== undefined && (!Array.isArray(rule[key]) || rule[key].length === 0)) return true;
  }
  return rule.min_size_bytes !== undefined && !Number.isFinite(rule.min_size_bytes);
}

/** Only an `enforcing` release may act. */
export function enforcing(releaseState) {
  return releaseState === 'enforcing';
}

function decide(action, ruleId, reason, extra = {}) {
  return {
    decision: { action, rule_id: ruleId || 'NONE', decided_locally: true },
    reason,
    degraded: false,
    error_counted: false,
    over_budget: false,
    rule_id: ruleId || 'NONE',
    skipped_rules: [],
    ...extra,
  };
}

/**
 * The synchronous half: decide without asking anyone, inside the decision budget.
 *
 * @param {object} input
 * @param {string} input.url
 * @param {string} input.method
 * @param {string} input.tool_fingerprint
 * @param {string} input.mode                 the effective mode (cache or core answer)
 * @param {string} input.release_state        shadow | enforcing | rolled_back
 * @param {Array} input.rules                 from the signed bundle
 * @param {number} [input.budgetMs]
 * @param {(started: number) => number} [input.now]
 */
export function decideSync(input) {
  const url = String(input.url || '');
  let host = '';
  let path = '';
  try {
    const u = new URL(url);
    host = u.host.toLowerCase();
    path = u.pathname;
  } catch {
    return decide(DECISION.LOGGED, 'NONE', REASON.FAILED_OPEN, {
      degraded: true,
      error_counted: true,
      error: 'unparseable_url',
      needs_confirmation: false,
      cancel: false,
      elapsed_ms: 0,
    });
  }

  const budget = createBudget({ budgetMs: input.budgetMs || DECISION_BUDGET_MS, now: input.now || nowMs });
  let outcome;
  try {
    outcome = budget.sync(() =>
      evaluateRule(input.rules, {
        host,
        path,
        tool_fingerprint: input.tool_fingerprint,
        size_bytes: Number.isFinite(input.size_bytes) ? input.size_bytes : 0,
        mode: input.mode,
      }),
    );
  } catch (e) {
    // An evaluation error fails open.
    return decide(DECISION.LOGGED, 'NONE', REASON.FAILED_OPEN, {
      degraded: true,
      error_counted: true,
      error: String((e && e.message) || e),
      needs_confirmation: false,
      cancel: false,
      elapsed_ms: round(budget.elapsed()),
    });
  }

  const rule = outcome.value.rule;
  const skippedRules = outcome.value.skipped;
  const elapsed = outcome.elapsed_ms;
  const degradedReasons = skippedRules.length ? [...skippedRules.map((s) => `skipped_rule:${s.reason}`)] : [];

  if (outcome.expired) {
    return decide(DECISION.LOGGED, rule ? safeRuleId(rule) : 'NONE', REASON.BUDGET_EXCEEDED, {
      degraded: true,
      error_counted: true,
      over_budget: true,
      needs_confirmation: false,
      cancel: false,
      elapsed_ms: elapsed,
    });
  }

  if (!rule) {
    return decide(DECISION.LOGGED, 'NONE', REASON.NO_RULE, {
      needs_confirmation: false,
      cancel: false,
      elapsed_ms: elapsed,
      skipped_rules: skippedRules,
      // A bundle rule the device could not apply is a coverage fact: the decision is still
      // `logged` (nothing here blocked anything), but it is reported as degraded so the gap is
      // visible rather than looking like "no rule matched".
      degraded: degradedReasons.length > 0,
      error_counted: degradedReasons.length > 0,
      error: degradedReasons.join(','),
    });
  }

  // Past this point the rule's own fields are read; a rule that is not shaped like a rule fails
  // open rather than throwing out of the request path.
  let ruleId;
  let action;
  try {
    ruleId = safeRuleId(rule);
    action = rule.action;
  } catch (e) {
    return decide(DECISION.LOGGED, 'NONE', REASON.FAILED_OPEN, {
      degraded: true,
      error_counted: true,
      error: String((e && e.message) || e),
      needs_confirmation: false,
      cancel: false,
      elapsed_ms: elapsed,
    });
  }

  if (!enforcing(input.release_state)) {
    // Shadow and rolled_back compute the decision, record it, and never act.
    return decide(DECISION.LOGGED, ruleId, input.release_state === 'shadow' ? REASON.SHADOW : REASON.ROLLED_BACK, {
      needs_confirmation: false,
      cancel: false,
      shadow: true,
      elapsed_ms: elapsed,
      skipped_rules: skippedRules,
      degraded: degradedReasons.length > 0,
      error_counted: degradedReasons.length > 0,
      error: degradedReasons.join(','),
    });
  }

  switch (action) {
    case DECISION.BLOCKED:
      return decide(DECISION.BLOCKED, ruleId, REASON.RULE_BLOCK, {
        needs_confirmation: false,
        cancel: true,
        elapsed_ms: elapsed,
        skipped_rules: skippedRules,
      });
    case DECISION.WARNED:
      return decide(DECISION.WARNED, ruleId, REASON.RULE_WARN, {
        needs_confirmation: true,
        cancel: false,
        elapsed_ms: elapsed,
        skipped_rules: skippedRules,
      });
    case DECISION.LOGGED:
      return decide(DECISION.LOGGED, ruleId, REASON.RULE_LOG, {
        needs_confirmation: false,
        cancel: false,
        elapsed_ms: elapsed,
        skipped_rules: skippedRules,
        degraded: degradedReasons.length > 0,
        error_counted: degradedReasons.length > 0,
        error: degradedReasons.join(','),
      });
    default:
      return decide(DECISION.LOGGED, ruleId, REASON.FAILED_OPEN, {
        degraded: true,
        error_counted: true,
        error: `unknown_action:${String(action)}`,
        needs_confirmation: false,
        cancel: false,
        elapsed_ms: elapsed,
      });
  }
}

/** The schema gives `rule_id` a 128-character bound; a bundle-sourced id is still validated. */
function safeRuleId(rule) {
  const id = rule && rule.rule_id;
  if (typeof id !== 'string' || id.length === 0 || id.length > 128) return 'NONE';
  return id;
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
 * @param {() => Promise<{proceeded: boolean, answered: boolean, reason: string}>} input.confirm
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
    answer = await confirmBudget.run(() => input.confirm());
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
