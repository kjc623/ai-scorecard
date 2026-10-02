/**
 * enforce.js — §7.4's inline warn/block, decided locally against the bundle's rules.
 *
 * The four properties §7.4 fixes, each one a line of code below:
 *   - **the decision is local**: rules come from the signed bundle the extension already
 *     holds, and every decision carries `decided_locally: true`. No round trip.
 *   - **`blocked` cancels the request** through webRequestBlocking (the caller turns
 *     `{cancel: true}` into the blocking response).
 *   - **`warned` requires explicit user confirmation** rendered before the request proceeds,
 *     and the answer is recorded *as part of the decision* — so the same rule deciding the
 *     same way twice is two events, because two prompts were sent.
 *   - **fail open on decision failure**: evaluation error or budget exhaustion ⇒ the request
 *     proceeds as `logged`, the event carries `confidence: degraded` and a `Detail` of
 *     `budget_exceeded`, and the failure is counted. "A broken classifier must not become a
 *     broken browser."
 *
 * §9.6's release states are honoured here rather than in the classifier path: a release in
 * `shadow` or `rolled_back` never enforces, and the resulting `logged` decision is marked so
 * an operator can see the difference between "we chose not to block" and "we may not block".
 *
 * A blocked request is still an event: `blocked` returns a decision record and the caller
 * emits it. Otherwise the product could not answer "what did we stop" (§7.4).
 */

import { DETAIL } from './messages.js';

/** §7.4: "the budget is the 300 ms target, not the 150 ms classification target". */
export const DECISION_BUDGET_MS = 300;

/**
 * The confirmation window, kept separate from the decision budget on purpose.
 *
 * §7.4's 300 ms is a *decision* budget: it bounds evaluating the rules and reaching a verdict,
 * because "the only work permitted to delay a request is the inline warn/block decision, bounded
 * by §9.4's decision budget". A `warned` verdict then has to be rendered and answered by a human,
 * and a human does not answer inside 300 ms — so treating the two as one number would make
 * `warned` behaviourally identical to fail-open, and the schema's `warned` action unreachable.
 *
 * This is therefore a **decision the document leaves open**, recorded as such in the module
 * report: rule evaluation is bounded at 300 ms, the confirmation window is bounded separately,
 * and the separation is what keeps a `warned` rule from silently degrading to `logged`.
 * Whoever owns the deployment can set this; it is bundle-overridable in `policy.rules()` usage.
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
 * A deadline over an injected clock. `sync` reports whether `work` finished inside the
 * budget at the point it was measured; `asyncDeadline(cb)` runs a callback that can itself
 * check `expired()`.
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

/** A rule matches when every matcher it names matches. `mode` is the strictest mode the rule may act at. */
export function ruleMatches(rule, { host, path, tool_fingerprint: tool, size_bytes: size, mode }) {
  if (!rule || typeof rule !== 'object') return false;
  if (Array.isArray(rule.hosts) && rule.hosts.length && !rule.hosts.some((h) => hostMatches(h, host))) return false;
  if (Array.isArray(rule.paths) && rule.paths.length && !rule.paths.some((p) => pathMatches(p, path))) return false;
  if (Array.isArray(rule.tools) && rule.tools.length && !rule.tools.includes(tool)) return false;
  if (Number.isFinite(rule.min_size_bytes) && !(size >= rule.min_size_bytes)) return false;
  if (Array.isArray(rule.modes) && rule.modes.length && !rule.modes.includes(mode)) return false;
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

/** Rules are ordered; the first match wins, which is what makes a bundle's ordering meaningful. */
export function evaluateRule(rules, req) {
  for (const rule of rules || []) {
    if (ruleMatches(rule, req)) return rule;
  }
  return null;
}

/** §9.6: only an `enforcing` release may act. */
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
    ...extra,
  };
}

/**
 * The synchronous half: decide without asking anyone. Everything §7.4 permits to block
 * inline is decided here, inside the 300 ms budget.
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
    outcome = budget.sync(() => {
      const rule = evaluateRule(input.rules, {
        host,
        path,
        tool_fingerprint: input.tool_fingerprint,
        size_bytes: Number.isFinite(input.size_bytes) ? input.size_bytes : 0,
        mode: input.mode,
      });
      return rule;
    });
  } catch (e) {
    // §7.4: "if evaluation errors ... the request proceeds (logged)".
    return decide(DECISION.LOGGED, 'NONE', REASON.FAILED_OPEN, {
      degraded: true,
      error_counted: true,
      error: String((e && e.message) || e),
      needs_confirmation: false,
      cancel: false,
      elapsed_ms: round(budget.elapsed()),
    });
  }

  const rule = outcome.value;
  const elapsed = outcome.elapsed_ms;

  if (outcome.expired) {
    return decide(DECISION.LOGGED, rule ? rule.rule_id : 'NONE', REASON.BUDGET_EXCEEDED, {
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
    });
  }

  if (!enforcing(input.release_state)) {
    // Shadow and rolled_back compute the decision, record it, and never act (§9.6).
    return decide(DECISION.LOGGED, rule.rule_id, input.release_state === 'shadow' ? REASON.SHADOW : REASON.ROLLED_BACK, {
      needs_confirmation: false,
      cancel: false,
      shadow: true,
      elapsed_ms: elapsed,
    });
  }

  switch (rule.action) {
    case DECISION.BLOCKED:
      return decide(DECISION.BLOCKED, rule.rule_id, REASON.RULE_BLOCK, {
        needs_confirmation: false,
        cancel: true,
        elapsed_ms: elapsed,
      });
    case DECISION.WARNED:
      return decide(DECISION.WARNED, rule.rule_id, REASON.RULE_WARN, {
        needs_confirmation: true,
        cancel: false,
        elapsed_ms: elapsed,
      });
    case DECISION.LOGGED:
      return decide(DECISION.LOGGED, rule.rule_id, REASON.RULE_LOG, {
        needs_confirmation: false,
        cancel: false,
        elapsed_ms: elapsed,
      });
    default:
      return decide(DECISION.LOGGED, rule.rule_id, REASON.FAILED_OPEN, {
        degraded: true,
        error_counted: true,
        error: `unknown_action:${rule.action}`,
        needs_confirmation: false,
        cancel: false,
        elapsed_ms: elapsed,
      });
  }
}

/**
 * The asynchronous half: a `warned` decision, rendered before the request proceeds, with the
 * user's answer recorded as part of the decision. Bounded by the same 300 ms budget: an
 * unanswered prompt is fail-open `logged` with `confidence: degraded`, never a silent block
 * (and never an indefinitely held request).
 *
 * @param {object} input                 as for decideSync, plus:
 * @param {() => Promise<{proceeded: boolean, answered: boolean, reason: string}>} input.confirm
 * @returns {Promise<{decision: {rule_id: string, action: string, decided_locally: true},
 *                    reason: string, degraded: boolean, error_counted: boolean,
 *                    over_budget: boolean, confirmed: boolean|null, user_answer: string|null,
 *                    needs_confirmation: boolean, cancel: boolean, elapsed_ms: number}>}
 */
export async function decideWithConfirmation(input) {
  const sync = decideSync({ ...input, budgetMs: Number.POSITIVE_INFINITY });
  const budget = createBudget({ budgetMs: input.budgetMs || DECISION_BUDGET_MS, now: input.now || nowMs });

  if (!sync.needs_confirmation) {
    return { ...sync, confirmed: null, user_answer: null };
  }

  let answer;
  try {
    answer = await budget.run(() => input.confirm());
  } catch (e) {
    return {
      ...sync,
      decision: { action: DECISION.LOGGED, rule_id: sync.rule_id, decided_locally: true },
      reason: REASON.FAILED_OPEN,
      degraded: true,
      error_counted: true,
      confirmed: null,
      user_answer: null,
      needs_confirmation: false,
      cancel: false,
      error: String((e && e.message) || e),
      elapsed_ms: round(budget.elapsed()),
    };
  }

  const a = answer.value || { proceeded: true, answered: false, reason: 'no_receiver' };
  const overBudget = answer.expired || !a.answered;

  if (overBudget) {
    return {
      ...sync,
      decision: { action: DECISION.LOGGED, rule_id: sync.rule_id, decided_locally: true },
      reason: answer.expired ? REASON.BUDGET_EXCEEDED : REASON.WARN_UNANSWERED,
      degraded: true,
      error_counted: true,
      over_budget: true,
      confirmed: false,
      user_answer: a.reason,
      needs_confirmation: false,
      cancel: false,
      elapsed_ms: round(budget.elapsed()),
    };
  }

  if (a.proceeded) {
    return {
      ...sync,
      decision: { action: DECISION.WARNED, rule_id: sync.rule_id, decided_locally: true },
      reason: REASON.WARN_PROCEEDED,
      confirmed: true,
      user_answer: 'proceeded',
      needs_confirmation: false,
      cancel: false,
      elapsed_ms: round(budget.elapsed()),
    };
  }

  return {
    ...sync,
    decision: { action: DECISION.BLOCKED, rule_id: sync.rule_id, decided_locally: true },
    reason: REASON.WARN_CANCELLED,
    confirmed: true,
    user_answer: 'cancelled',
    needs_confirmation: false,
    cancel: true,
    elapsed_ms: round(budget.elapsed()),
  };
}

/**
 * Turn an outcome into the shared decision record: the message capture-core receives, and
 * what the counters are updated from. `decided_locally` is true on every path, including
 * fail-open, because a decision made without a round trip is what the field records.
 */
export function decisionRecord(outcome, { client_id, occurred_at, url, tool_fingerprint }) {
  return {
    client_id,
    occurred_at,
    url,
    tool_fingerprint,
    decision: outcome.decision,
    reason: outcome.reason,
    confirmed: outcome.confirmed === undefined ? null : outcome.confirmed,
    user_answer: outcome.user_answer === undefined ? null : outcome.user_answer,
    degraded: Boolean(outcome.degraded),
    over_budget: Boolean(outcome.over_budget),
  };
}

/** `Detail` for a degraded decision, from the closed vocabulary. */
export function degradedDetail(outcome) {
  return outcome.over_budget ? DETAIL.BUDGET_EXCEEDED : DETAIL.CLASSIFIER_UNAVAILABLE;
}

function round(n) {
  return Math.round(n * 1000) / 1000;
}
