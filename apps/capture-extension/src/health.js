/**
 * health.js — the extension's coverage row and its seven counters (§4.3), plus the §3.4 rule
 * for a dead channel.
 *
 * Two things this module refuses to do.
 *
 *   - **It never reports `absent` as "no observations".** §3.4: "A failed connect is reported
 *     as `capture-core` `absent` **plus** extension-side `degraded`, never as 'no
 *     observations'." So the state is a function of the channel, and the queue depth and drop
 *     counter travel with it in `detail`, because "we collected and could not deliver" and
 *     "nothing happened" are different facts.
 *   - **It never invents a counter outside the closed seven.** §4.3 makes the set closed
 *     precisely so a provider cannot add a high-cardinality stream one level down; an unknown
 *     name is dropped from the report rather than sent.
 *
 * The collector name comes from `ref.collector` (`capture_extension`) — §4.3: "the collector
 * name must come from ref.collector, so a provider cannot invent a name for a coverage path
 * the reporting layer does not know."
 */

import { COLLECTOR_NAME, COUNTERS, DETAIL, STATE } from './messages.js';

/** The closed set, at zero, so a missing counter is never confused with a zero counter. */
export function createCounters() {
  const c = {};
  for (const k of COUNTERS) c[k] = 0;
  return c;
}

export const COUNTER = Object.freeze({
  OBSERVED: 'observed',
  EMITTED: 'emitted',
  SKIPPED_NOT_GENERATIVE: 'skipped_not_generative',
  BLIND_TUNNELLED: 'blind_tunnelled',
  NOT_COOPERATIVE: 'not_cooperative',
  DROPPED: 'dropped',
  ERRORS: 'errors',
});

/**
 * Counters with cumulative and windowed views (§4.3: "cumulative since process start plus a
 * windowed delta"). `since` is the origin of the current window; `rollWindow()` moves it,
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
  let since = nowIso();
  let lastSuccessAt = null;
  let channelState = STATE.ABSENT; // nothing has connected yet, and "not connected" is not "nothing observed"
  let channelDetail = DETAIL.NONE;

  /**
   * @param {{state?: string, detail?: string, core?: 'connected'|'absent'}} [override]
   */
  function report(override = {}) {
    const q = queueStats ? queueStats() : { depth: 0, capacity: 0, dropped_total: 0, dropped_bytes_total: 0, dropped_by_kind: {}, delivered_total: 0 };
    const snap = counters.snapshot();
    const policy = policySnapshot ? policySnapshot() : { policy_version: null, present: false, stale: false };

    // §3.4: a failed connect is `absent` for capture-core *plus* `degraded` extension-side.
    const coreAbsent = override.core === 'absent' || channelState === STATE.ABSENT;
    const state = override.state || (coreAbsent ? STATE.DEGRADED : STATE.HEALTHY);
    const detail = override.detail || (coreAbsent ? DETAIL.CLASSIFIER_UNAVAILABLE : channelDetail);

    const health = {
      device_id,
      collector: COLLECTOR_NAME,
      state,
      // protocol.HealthReport: `detail` is the closed Detail vocabulary, carried as error_code.
      detail: detail === DETAIL.NONE ? undefined : detail,
      since,
      counters: snap.counters,
      version,
    };
    if (lastSuccessAt) health.last_success_at = lastSuccessAt;
    // `window` and the extension's own queue state are not HealthReport fields: HealthReport is
    // the device-side shape and capture-core forwards it. They travel alongside so the report
    // is not lossy about a channel that is down, which is the case they exist for.
    health.window = snap.window;
    health.errors_by_code = snap.errors_by_code;
    health.queue = q;
    health.core = coreAbsent ? 'absent' : 'connected';
    health.policy = { version: policy.policy_version, present: policy.present, stale: policy.stale };
    return health;
  }

  function onChannelConnected() {
    channelState = STATE.HEALTHY;
    channelDetail = DETAIL.NONE;
  }

  /**
   * A failed connect or a disconnect. `detail` comes from the closed vocabulary —
   * `classifier_unavailable` is the closest honest member for "capture-core is not answering",
   * and the `core: 'absent'` field above is what records which component it was.
   */
  function onChannelAbsent(detail = DETAIL.CLASSIFIER_UNAVAILABLE) {
    channelState = STATE.ABSENT;
    channelDetail = detail;
  }

  function markSuccess() {
    lastSuccessAt = nowIso();
  }

  /** An observation reached `capture-core` (not the server — the spool owns that distinction). */
  function markEmitted(n = 1) {
    markSuccess();
    counters.inc(COUNTER.EMITTED, n);
  }

  function resetSince() {
    since = nowIso();
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
    markSuccess,
    markEmitted,
    resetSince,
    get state() {
      return channelState;
    },
  };
}
