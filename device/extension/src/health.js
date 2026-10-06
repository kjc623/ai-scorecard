/**
 * health.js — the extension's coverage row and its seven counters.
 *
 * A dead channel is reported as capture-core `absent` plus extension-side `degraded`, never as
 * "no observations": the queue depth and drop counter travel with the report, because "collected
 * and could not deliver" and "nothing happened" are different facts. The counter set is closed;
 * an unknown name is refused rather than sent.
 */

import { COLLECTOR_NAME, COUNTER, COUNTERS, DETAIL, STATE } from './messages.js';

/** The closed set, at zero, so a missing counter is never confused with a zero counter. */
export function createCounters() {
  const c = {};
  for (const k of COUNTERS) c[k] = 0;
  return c;
}

/**
 * Counters with cumulative (since start) and windowed views. `rollWindow()` starts a new window,
 * which is what a health-report interval does.
 */
export function createCounterSet({ now = () => Date.now() } = {}) {
  const total = createCounters();
  const window = createCounters();
  let windowStartedAt = now();
  /** Error causes, so an operator can group without reading prose. Bounded to the module's own vocabulary. */
  const errorsByCode = Object.create(null);

  function inc(counter, n = 1) {
    if (!COUNTERS.includes(counter)) return false; // closed set: an unknown name is refused, not sent
    total[counter] += n;
    window[counter] += n;
    return true;
  }

  function countError(code, n = 1) {
    inc(COUNTER.ERRORS, n);
    const key = typeof code === 'string' && code ? code : 'internal_error';
    errorsByCode[key] = (errorsByCode[key] || 0) + n;
  }

  function snapshot() {
    return {
      counters: { ...total },
      window: { ...window },
      window_started_at: new Date(windowStartedAt).toISOString(),
      errors_by_code: { ...errorsByCode },
    };
  }

  function rollWindow() {
    const previous = { ...window };
    for (const k of COUNTERS) window[k] = 0;
    windowStartedAt = now();
    return previous;
  }

  function reset() {
    for (const k of COUNTERS) {
      total[k] = 0;
      window[k] = 0;
    }
    for (const k of Object.keys(errorsByCode)) delete errorsByCode[k];
    windowStartedAt = now();
  }

  return { inc, countError, snapshot, rollWindow, reset };
}

/**
 * @param {object} opts
 * @param {string} opts.device_id
 * @param {() => {depth: number, capacity: number, dropped_total: number, dropped_bytes_total: number, dropped_by_kind: object, delivered_total: number}} opts.queueStats
 * @param {() => ({policy_version: string|null, present: boolean, stale: boolean})} [opts.policySnapshot]
 * @param {() => string} [opts.nowIso]
 */
export function createHealthReporter({ device_id, version = '0.1.0', queueStats, policySnapshot, nowIso = () => new Date().toISOString() }) {
  const counters = createCounterSet();
  const since = nowIso();
  let lastSuccessAt = null;
  let channelState = STATE.ABSENT; // nothing has connected yet, and "not connected" is not "nothing observed"
  let channelDetail = DETAIL.NONE;
  /** Whether this install can enforce. Answered before any lane is registered. */
  let enforcementDetail = DETAIL.NONE;

  /**
   * @param {{state?: string, detail?: string, core?: 'connected'|'absent'}} [override]
   */
  function report(override = {}) {
    const q = queueStats ? queueStats() : { depth: 0, capacity: 0, dropped_total: 0, dropped_bytes_total: 0, dropped_by_kind: {}, delivered_total: 0 };
    const snap = counters.snapshot();
    const policy = policySnapshot ? policySnapshot() : { policy_version: null, present: false, stale: false };

    // A failed connect is `absent` for capture-core plus `degraded` extension-side.
    const coreAbsent = override.core === 'absent' || channelState === STATE.ABSENT;
    const state = override.state || (coreAbsent || enforcementDetail !== DETAIL.NONE ? STATE.DEGRADED : STATE.HEALTHY);

    // `HealthReport` carries one `detail`. The enforcement capability takes precedence: it is a
    // lasting property of the install the protocol can express nowhere else, while the channel
    // state is also carried by `state` and `core`.
    const detail = override.detail
      || (enforcementDetail !== DETAIL.NONE ? enforcementDetail : coreAbsent ? DETAIL.CLASSIFIER_UNAVAILABLE : channelDetail);

    const health = {
      device_id,
      collector: COLLECTOR_NAME,
      state,
      // `detail` is from the closed Detail vocabulary.
      detail: detail === DETAIL.NONE ? undefined : detail,
      since,
      counters: snap.counters,
      version,
    };
    if (lastSuccessAt) health.last_success_at = lastSuccessAt;
    // `window`, the queue and the policy are extension-side fields that travel alongside the
    // HealthReport, so a report about a down channel is not lossy.
    health.window = snap.window;
    health.errors_by_code = snap.errors_by_code;
    health.queue = q;
    health.core = coreAbsent ? 'absent' : 'connected';
    health.policy = { version: policy.policy_version, present: policy.present, stale: policy.stale };
    // Always present, so neither fact is lost when the other takes `detail`.
    health.enforcement = enforcementDetail === DETAIL.NONE ? 'blocking' : 'observation_only';
    return health;
  }

  /** The install does not hold the blocking grant: it observes and cannot cancel. */
  function onEnforcementUnavailable() {
    enforcementDetail = DETAIL.ENFORCEMENT_UNAVAILABLE;
    counters.countError('enforcement_unavailable');
  }

  function onEnforcementAvailable() {
    enforcementDetail = DETAIL.NONE;
  }

  function onChannelConnected() {
    channelState = STATE.HEALTHY;
    channelDetail = DETAIL.NONE;
  }

  /**
   * A failed connect or a disconnect. `classifier_unavailable` is the closest member of the closed
   * vocabulary for "capture-core is not answering"; `core: 'absent'` names the component.
   */
  function onChannelAbsent(detail = DETAIL.CLASSIFIER_UNAVAILABLE) {
    channelState = STATE.ABSENT;
    channelDetail = detail;
  }

  function markSuccess() {
    lastSuccessAt = nowIso();
  }

  /** capture-core accepted `n` observations (not delivered to the server: the spool owns that). */
  function markEmitted(n = 1) {
    markSuccess();
    counters.inc(COUNTER.EMITTED, n);
  }

  return {
    counters: {
      inc: counters.inc,
      countError: counters.countError,
      snapshot: counters.snapshot,
      rollWindow: counters.rollWindow,
      reset: counters.reset,
    },
    report,
    onChannelConnected,
    onChannelAbsent,
    onEnforcementUnavailable,
    onEnforcementAvailable,
    markEmitted,
    get state() {
      return channelState;
    },
    get enforcement() {
      return enforcementDetail === DETAIL.NONE ? 'blocking' : 'observation_only';
    },
  };
}
