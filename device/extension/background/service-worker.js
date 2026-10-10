/**
 * service-worker.js — the MV3 background entry point. Wiring only; every decision lives in a
 * module `node --test` loads without a browser.
 *
 * An MV3 service worker is killed aggressively, so everything here is memory: the queue, the
 * counters and the policy cache are rebuilt on start. The only durable store on the device is
 * capture-core's spool.
 *
 * The content script holds the `File` object and drives an attachment transfer; this worker owns
 * the native channel, relays each frame and returns capture-core's own reply.
 */

import { ExtError } from '../src/adapter.js';
import { createAttachmentCollector } from '../src/attachments/collect.js';
import { createChromeAdapter } from '../src/chrome-adapter.js';
import { createHealthReporter } from '../src/health.js';
import { CORE_TYPE, DETAIL, TYPE } from '../src/messages.js';
import { createNativeClient } from '../src/native.js';
import { createPipeline } from '../src/pipeline.js';
import { createPolicyCache } from '../src/mode-policy.js';
import { createQueue } from '../src/queue.js';
import { installLanes } from '../src/registration.js';
import { createSelfUpdate } from '../src/self-update.js';

export const HEALTH_ALARM = 'capture-health';
export const HEALTH_PERIOD_MINUTES = 1;
export const POLICY_SYNC_ALARM = 'capture-policy-sync';
export const POLICY_SYNC_PERIOD_MINUTES = 15;

/** Build the running extension. Exported so a test can drive the whole thing with a fake adapter. */
export function bootstrap(adapter, { deviceId = null, version = '0.1.0', capacity = 200, nativeTimeoutMs = undefined, connectCooldownMs = undefined } = {}) {
  const policy = createPolicyCache();
  const health = createHealthReporter({
    device_id: deviceId || 'unbound',
    version,
    queueStats: () => queue.stats(),
    policySnapshot: () => policy.snapshot(),
  });
  // The extension's `dropped` counter is separate from the spool's own drop counter; the queue is
  // the one place that knows a drop happened here.
  const queue = createQueue({
    capacity,
    onDrop: () => health.counters.inc('dropped', 1),
  });

  const native = createNativeClient({
    adapter,
    ...(Number.isFinite(nativeTimeoutMs) ? { timeoutMs: nativeTimeoutMs } : {}),
    ...(Number.isFinite(connectCooldownMs) ? { connectCooldownMs } : {}),
    onEvent: (ev) => {
      if (ev.kind === 'port_opened') {
        // A port object is not a working channel, and nothing may hang off this event: a send
        // started here would fail, drop the port, open another and fire this again.
        return;
      }
      if (ev.kind === 'connected') {
        // A message round-tripped: the channel is real. Fetch policy and deliver the queue.
        health.onChannelConnected();
        requestPolicySync();
        void pipeline.drainQueue(20).then(() => reportHealth());
      } else if (ev.kind === 'connect_failed' || ev.kind === 'disconnected') {
        // capture-core `absent` plus extension-side `degraded`, never "no observations".
        health.onChannelAbsent(DETAIL.CLASSIFIER_UNAVAILABLE);
        health.counters.countError('native_unavailable');
      } else if (ev.kind === 'version_mismatch') {
        health.counters.countError('native_protocol_error');
      } else if (ev.kind === 'timeout') {
        health.counters.countError('native_timeout');
      }
    },
  });

  const pipeline = createPipeline({
    adapter,
    policy,
    native,
    queue,
    health,
    attachments: createAttachmentCollector({ adapter, counters: health.counters }),
  });

  const selfUpdate = createSelfUpdate({
    adapter,
    version,
    beforeReload: () => pipeline.drainQueue(capacity).then(() => reportHealth()),
  });

  /**
   * Whether this install holds `webRequestBlocking`, asked before any lane is registered: a
   * blocking registration without the grant is accepted silently and never invoked, so the lanes
   * register blocking only when it can work.
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
      onResponse: (detail) => pipeline.onResponse({ detail }),
    });
    // Until the first policy arrives every destination resolves to M0, so no body lane exists.
    lanes.refresh();
    return lanes;
  }

  async function requestPolicySync() {
    try {
      const answer = await native.sendRequest(TYPE.POLICY_SYNC, { known_version: policy.snapshot().policy_version || '' });
      void selfUpdate.consider(answer.body && answer.body.agent_version);
      return applyPolicy(answer.body);
    } catch (e) {
      // With no bundle the device resolves M0 and reports why.
      health.onChannelAbsent(DETAIL.CLASSIFIER_UNAVAILABLE);
      return { applied: false, reason: 'channel_down' };
    }
  }

  /** `body` is a `policy_bundle` answer: the decoded bundle capture-core verified and holds in force. */
  function applyPolicy(body) {
    if (!body || body.unchanged) return { applied: false, reason: 'unchanged' };
    const applied = policy.applyBundle(body.bundle);
    if (!applied.applied) {
      health.counters.countError('evaluation_error');
      return applied;
    }
    installLanesNow().refresh();
    return applied;
  }

  function reportHealth() {
    const report = health.report();
    try {
      native.sendOneWay(TYPE.HEALTH, report);
      health.counters.rollWindow();
    } catch (e) {
      // Not queued: the next report carries the same counters, cumulatively.
      health.onChannelAbsent(DETAIL.CLASSIFIER_UNAVAILABLE);
    }
    return report;
  }

  // The content script sends each attachment frame (manifest, chunks, completion) here; the code
  // that holds the bytes therefore enforces "manifest before bytes".
  adapter.messages.onMessage(async (msg) => {
    if (!msg || typeof msg !== 'object') return { ok: false, error: 'empty message' };
    if (msg.type === 'capture_attachment_frame') {
      try {
        return { ok: true, answer: await native.sendRequest(msg.frame_type, msg.body) };
      } catch (e) {
        // A refusal is capture-core's answer to this frame: the content script acts on it.
        if (e instanceof ExtError && e.code === 'core_refused') {
          return { ok: true, answer: { type: CORE_TYPE.REFUSAL, body: { reason: e.detail.reason, message: e.message } } };
        }
        throw e;
      }
    }
    return { ok: false, error: `unknown message type ${msg.type}` };
  });

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
    try {
      blockingAvailable = await adapter.permissions.hasWebRequestBlocking();
    } catch {
      blockingAvailable = true;
    }
    if (blockingAvailable) health.onEnforcementAvailable();
    else health.onEnforcementUnavailable();
    installAlarms();
    selfUpdate.install();
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
    selfUpdate,
    reportHealth,
    requestPolicySync,
    applyPolicy,
    get blockingAvailable() {
      return blockingAvailable;
    },
    get lanes() {
      return lanes;
    },
  };
}

// MV3 entry: runs in a browser, never under `node --test`. `__captureApp` is the handle the
// real-browser check reads health and taps the queue through.
if (typeof chrome !== 'undefined' && chrome.runtime && typeof chrome.runtime.connectNative === 'function') {
  const adapter = createChromeAdapter(globalThis);
  const app = bootstrap(adapter, { version: adapter.runtime.version() });
  app.start();
  globalThis.__captureApp = app;
}
