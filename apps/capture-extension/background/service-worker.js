/**
 * service-worker.js — the MV3 background entry point. Wiring only; every decision lives in a
 * module `node --test` can load without a browser.
 *
 * Lifecycle note that shapes the design: an MV3 service worker is killed aggressively, so
 * everything held here is memory and dies with it. That is what §7.1 requires ("the extension
 * holds nothing durable"): the queue, the counters and the policy cache are all rebuildable, and
 * the only durable store on the device is the spool, which the extension cannot reach (§3.4).
 *
 * Division of labour with the content script, stated once so it is not re-litigated per call:
 *
 *   - the **content script** holds the `File` object, so it drives an attachment transfer and
 *     calls this worker for each frame;
 *   - this **worker** owns the native channel, so attachment frames are relayed here and
 *     answered with `capture-core`'s own reply, so a refusal is the core's, not the worker's.
 */

import { createChromeAdapter } from '../src/chrome-adapter.js';
import { createHealthReporter } from '../src/health.js';
import { COLLECTOR_NAME, DETAIL, TYPE } from '../src/messages.js';
import { createNativeClient } from '../src/native.js';
import { createPipeline } from '../src/pipeline.js';
import { createPolicyCache } from '../src/mode-policy.js';
import { createQueue } from '../src/queue.js';
import { installLanes } from '../src/registration.js';

export const HEALTH_ALARM = 'capture-health';
export const HEALTH_PERIOD_MINUTES = 1;
export const POLICY_SYNC_ALARM = 'capture-policy-sync';
export const POLICY_SYNC_PERIOD_MINUTES = 15;

/** Build the running extension. Exported so a test can drive the whole thing with a fake adapter. */
export function bootstrap(adapter, { deviceId = null, version = '0.1.0', capacity = 200, nativeTimeoutMs = undefined } = {}) {
  const policy = createPolicyCache();
  const health = createHealthReporter({
    device_id: deviceId || 'unbound',
    version,
    queueStats: () => queue.stats(),
    policySnapshot: () => policy.snapshot(),
  });
  // §4.3 + C22: the provider's `dropped` counter and the spool's `spool_dropped_total` are
  // separate counters, summed for the operator. This is the provider half, and the queue is the
  // one place that knows a drop happened.
  const queue = createQueue({
    capacity,
    onDrop: () => health.counters.inc('dropped', 1),
  });

  const native = createNativeClient({
    adapter,
    ...(Number.isFinite(nativeTimeoutMs) ? { timeoutMs: nativeTimeoutMs } : {}),
    onEvent: (ev) => {
      if (ev.kind === 'connected') {
        health.onChannelConnected();
        void pipeline.drainQueue(20).then(() => reportHealth());
        requestPolicySync();
      } else if (ev.kind === 'connect_failed' || ev.kind === 'disconnected') {
        // §3.4: a failed connect is capture-core `absent` plus extension-side `degraded`,
        // never "no observations".
        health.onChannelAbsent(DETAIL.CLASSIFIER_UNAVAILABLE);
        health.counters.countError('native_unavailable');
      } else if (ev.kind === 'version_mismatch') {
        health.counters.countError('native_protocol_error');
      } else if (ev.kind === 'timeout') {
        health.counters.countError('native_timeout');
      }
    },
  });

  const pipeline = createPipeline({ adapter, policy, native, queue, health });

  /**
   * §7.4's capability, asked before registration rather than assumed.
   *
   * A refused blocking registration is accepted silently and then never invoked, so registering the
   * observation lanes as blocking on an install that lacks the grant would collect **nothing at
   * all** — the failure §15.2 forbids. The lanes therefore adapt: with the grant a `blocked` rule can
   * cancel; without it observation is unchanged and the health report says enforcement is
   * unavailable instead of leaving it silently inert.
   */
  let blockingAvailable = true;
  let lanes = null;

  function installLanesNow() {
    if (lanes) return lanes;
    lanes = installLanes({
      adapter,
      policy,
      blockingAvailable,
      onBodyLane: (input) => pipeline.captureWithBody(input),
      onMetadataLane: (input) => pipeline.captureMetadata(input),
      // The response lane hands over a bare response record; the pipeline takes it wrapped, the
      // same shape the two request lanes use.
      onResponse: (detail) => pipeline.onResponse({ detail }),
    });
    // Nothing is registered until the first policy arrives: with no bundle every destination
    // resolves to M0, so §11.2's guarantee holds by construction rather than by a handler check.
    lanes.refresh();
    return lanes;
  }

  // ── policy (§3.4 bidirectional channel, §11.3's device-side bundle) ───────────────────────
  async function requestPolicySync() {
    try {
      const answer = await native.sendRequest(TYPE.POLICY_SYNC, { known_version: policy.snapshot().policy_version || '' });
      return applyPolicy(answer.body);
    } catch (e) {
      // §13.3's device-side half: with no bundle the device resolves M0 and reports why.
      health.onChannelAbsent(DETAIL.CLASSIFIER_UNAVAILABLE);
      return { applied: false, reason: 'channel_down' };
    }
  }

  function applyPolicy(body) {
    if (!body || body.unchanged) return { applied: false, reason: 'unchanged' };
    const bundle = body.bundle && typeof body.bundle === 'object' ? { ...body.bundle, policy_version: body.policy_version || body.bundle.policy_version } : null;
    const applied = policy.applyBundle(bundle);
    if (!applied.applied) {
      health.counters.countError('evaluation_error');
      return applied;
    }
    if (bundle.device_id) pipeline.setDeviceId(bundle.device_id);
    installLanesNow().refresh();
    return applied;
  }

  // ── health ────────────────────────────────────────────────────────────────────────────────
  function reportHealth() {
    // `detail` and `enforcement` are set by the health reporter from the capability it was told
    // about at startup, so the wire report and the internal state cannot disagree.
    const report = health.report();
    try {
      native.sendOneWay(TYPE.HEALTH, report);
      health.counters.rollWindow();
    } catch (e) {
      // The report is not queued: the next one carries the same counters, cumulatively.
      health.onChannelAbsent(DETAIL.CLASSIFIER_UNAVAILABLE);
    }
    return report;
  }

  // ── attachment relay ──────────────────────────────────────────────────────────────────────
  /**
   * One attachment frame, relayed. The content script calls this for the manifest, each chunk and
   * the completion, so the ordering guarantee of §7.3 ("manifest before bytes") is enforced by
   * the code that holds the bytes.
   */
  async function relayFrame(type, body) {
    return await native.sendRequest(type, body);
  }

  // ── content-script routing ────────────────────────────────────────────────────────────────
  adapter.messages.onMessage(async (msg, sender) => {
    if (!msg || typeof msg !== 'object') return { ok: false, error: 'empty message' };
    switch (msg.type) {
      case 'capture_attachment_frame':
        return { ok: true, answer: await relayFrame(msg.frame_type, msg.body) };
      case 'capture_page_context': {
        // §7.3: `content_no_attachments` is the honest record when no File handle was reachable.
        // It is counted here and reported with the observation, never guessed at.
        if (msg.candidates && msg.candidates.length) {
          health.counters.inc('observed', 0);
        } else {
          health.counters.inc('observed', 0);
          health.pageContextNoAttachments = (health.pageContextNoAttachments || 0) + 1;
        }
        return { ok: true };
      }
      default:
        return { ok: false, error: `unknown message type ${msg.type}` };
    }
  });

  // ── alarms ────────────────────────────────────────────────────────────────────────────────
  function installAlarms() {
    adapter.alarms.create(HEALTH_ALARM, { periodInMinutes: HEALTH_PERIOD_MINUTES });
    adapter.alarms.create(POLICY_SYNC_ALARM, { periodInMinutes: POLICY_SYNC_PERIOD_MINUTES });
    adapter.alarms.onAlarm((alarm) => {
      if (alarm.name === HEALTH_ALARM) {
        void pipeline.drainQueue(20).then(() => reportHealth());
      } else if (alarm.name === POLICY_SYNC_ALARM) {
        requestPolicySync();
      }
    });
  }

  async function start() {
    // Ask for the capability before anything is registered, because the answer changes what the
    // lanes ask for. `hasWebRequestBlocking` defaults to true when it cannot be answered, so this
    // can only ever turn a silently-inert lane into a working observation lane.
    try {
      blockingAvailable = await adapter.permissions.hasWebRequestBlocking();
    } catch {
      blockingAvailable = true;
    }
    if (!blockingAvailable) {
      // §7.4/§15.2's coverage state, carried in the protocol's own `detail` vocabulary as
      // `enforcement_unavailable`. Reported rather than silent: a path that cannot enforce must say
      // so instead of reporting healthy while inspection is wider than enforcement.
      health.onEnforcementUnavailable();
    } else {
      health.onEnforcementAvailable();
    }
    installAlarms();
    const installed = installLanesNow();
    await requestPolicySync();
    return { policy, queue, health, native, pipeline, lanes: installed, blockingAvailable };
  }

  return {
    start,
    policy,
    queue,
    health,
    native,
    pipeline,
    reportHealth,
    requestPolicySync,
    applyPolicy,
    relayFrame,
    collector: COLLECTOR_NAME,
    /** §7.4's capability as this install holds it, and the lanes once they are registered. */
    get blockingAvailable() {
      return blockingAvailable;
    },
    get lanes() {
      return lanes;
    },
  };
}

// ── MV3 entry: only runs in a browser, never under `node --test` ─────────────────────────────
if (typeof chrome !== 'undefined' && chrome.runtime && typeof chrome.runtime.connectNative === 'function') {
  const adapter = createChromeAdapter(globalThis);
  const app = bootstrap(adapter);
  app.start();
  globalThis.__captureApp = app;
}
