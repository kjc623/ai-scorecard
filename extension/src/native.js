/**
 * native.js — the extension end of the native-messaging channel to capture-core.
 *
 *   - owns the port's lifetime: connects lazily, and turns a failed connect or a disconnect into
 *     `ExtError('native_unavailable')`;
 *   - correlates request/response pairs by the message `id`;
 *   - enforces the 1 MiB ceiling on the framed message before posting;
 *   - drains the bounded queue in order, removing an entry only once capture-core has accepted it.
 *
 * It never retries into a busy loop: after a disconnect it refuses new connects for a cool-down,
 * and a failed send leaves the entry queued.
 */

import { ExtError } from './adapter.js';
import { CORE_TYPE, MAX_NATIVE_MESSAGE_BYTES, framedByteLength, frame, unframe } from './messages.js';

/** The native messaging host name the installers register. */
export const NATIVE_APP = 'com.shadowaicapture.capture_core';

export const DEFAULT_TIMEOUT_MS = 10_000;

/**
 * After a disconnect, no new port is opened for this long. Without it a missing host is a tight
 * loop: `connectNative` returns a port, the browser delivers the disconnect, the next send opens
 * another. The queue holds what is emitted meanwhile.
 */
export const DEFAULT_CONNECT_COOLDOWN_MS = 5_000;

/** How a connect refused during the cool-down is reported, so it is never counted as a new failure. */
export const COOLDOWN_ERROR_CODE = 'native_cooling_down';

/**
 * @param {object} opts
 * @param {import('./adapter.js').Adapter} opts.adapter
 * @param {string} [opts.application]
 * @param {number} [opts.timeoutMs]
 * @param {number} [opts.connectCooldownMs]
 * @param {(event: object) => void} [opts.onEvent]   channel events for the health reporter
 */
export function createNativeClient({
  adapter,
  application = NATIVE_APP,
  timeoutMs = DEFAULT_TIMEOUT_MS,
  connectCooldownMs = DEFAULT_CONNECT_COOLDOWN_MS,
  onEvent = () => {},
}) {
  /** @type {import('./adapter.js').NativePort|null} */
  let port = null;
  let nextId = 0;
  const pending = new Map();
  let connected = false;
  let lastError = null;
  /** True once a message has been answered on the current port. */
  let answered = false;
  /** No new port is opened before this time. */
  let nextConnectAllowedAt = 0;

  function connect() {
    if (port) return port;
    const remaining = nextConnectAllowedAt - Date.now();
    if (remaining > 0) {
      // The same failure, still standing: refuse without opening a port or emitting an event.
      throw new ExtError(COOLDOWN_ERROR_CODE, `native host unavailable; next attempt in ${Math.ceil(remaining / 1000)}s`);
    }
    try {
      const p = adapter.runtime.connectNative(application);
      if (!p) throw new ExtError('native_unavailable', `connectNative(${application}) returned no port`);
      port = p;
      connected = true;
      answered = false;
      lastError = null;
      p.onMessage((raw) => handleMessage(raw));
      p.onDisconnect((err) => handleDisconnect(err));
      // A port object is not a working channel: for a missing host the browser hands one out and
      // delivers the disconnect later. `connected` is emitted once a message round-trips, and no
      // work may hang off `port_opened`, or a missing host becomes a retry loop.
      onEvent({ kind: 'port_opened' });
      return port;
    } catch (e) {
      port = null;
      connected = false;
      lastError = e;
      nextConnectAllowedAt = Date.now() + connectCooldownMs;
      onEvent({ kind: 'connect_failed', error: String((e && e.message) || e) });
      throw e instanceof ExtError ? e : new ExtError('native_unavailable', String((e && e.message) || e));
    }
  }

  function handleDisconnect(err) {
    const message = (err && err.message) || (adapter.runtime && adapter.runtime.lastError && adapter.runtime.lastError.message) || 'native host disconnected';
    connected = false;
    answered = false;
    port = null;
    lastError = new ExtError('native_unavailable', message);
    // The cool-down starts here, so the very next send in the same tick cannot open another port.
    nextConnectAllowedAt = Date.now() + connectCooldownMs;
    for (const [, entry] of pending) {
      clearTimeout(entry.timer);
      entry.reject(lastError);
    }
    pending.clear();
    onEvent({ kind: 'disconnected', error: message });
  }

  function handleMessage(raw) {
    const msg = unframe(raw);
    if (!msg) {
      onEvent({ kind: 'protocol_error', error: 'message is not a NativeMessage' });
      return;
    }
    if (msg.versionMismatch) {
      // A version mismatch is a degraded handshake, never a crash.
      onEvent({ kind: 'version_mismatch', version: msg.version });
      return;
    }
    if (msg.id && pending.has(msg.id)) {
      const entry = pending.get(msg.id);
      pending.delete(msg.id);
      clearTimeout(entry.timer);
      // A refusal is still an answer: the channel works even though this request was declined.
      markAnswered();
      if (msg.type === CORE_TYPE.REFUSAL) {
        const body = msg.body || {};
        entry.reject(new ExtError('core_refused', body.message || 'refused', { reason: body.reason }));
      } else {
        entry.resolve(msg);
      }
      return;
    }
    onEvent({ kind: 'unsolicited', type: msg.type, body: msg.body });
  }

  function post(type, body, { id } = {}) {
    const p = connect();
    const correlationId = id || `m${++nextId}`;
    const size = framedByteLength(type, body, correlationId);
    if (size > MAX_NATIVE_MESSAGE_BYTES) {
      // Chromium would reject a message this large without a diagnosable error.
      throw new ExtError('core_refused', `framed message is ${size} bytes, ceiling is ${MAX_NATIVE_MESSAGE_BYTES}`);
    }
    try {
      p.postMessage(frame(type, body, correlationId));
    } catch (e) {
      handleDisconnect(e);
      throw lastError;
    }
    return { id: correlationId, bytes: size };
  }

  /** Send a message and resolve with capture-core's answer, or reject on refusal or timeout. */
  function sendRequest(type, body) {
    const { id } = post(type, body);
    return new Promise((resolve, reject) => {
      // Not unref'd: an unanswered request must settle as `native_timeout`, never hang.
      const timer = setTimeout(() => {
        pending.delete(id);
        onEvent({ kind: 'timeout', type });
        reject(new ExtError('native_timeout', `no answer to ${type} within ${timeoutMs}ms`));
      }, timeoutMs);
      pending.set(id, { resolve, reject, timer, type });
    });
  }

  /** Emitted once per port: `connected` means a round trip succeeded. */
  function markAnswered() {
    if (answered) return;
    answered = true;
    onEvent({ kind: 'connected' });
  }

  /** Fire-and-forget, where a refusal is not actionable (health). */
  function sendOneWay(type, body) {
    return post(type, body);
  }

  /**
   * Drain the queue oldest-first. Each entry is removed only after its ack, so a mid-drain failure
   * loses nothing the bound had not already dropped.
   *
   * @param {ReturnType<import('./queue.js').createQueue>} queue
   * @param {number} [max]
   */
  async function drain(queue, max = 20) {
    let sent = 0;
    const failures = [];
    while (sent < max) {
      const head = queue.peek(1)[0];
      if (!head) break;
      try {
        await sendRequest(head.kind, head.payload);
        queue.ack(head.seq, 1);
        sent++;
      } catch (e) {
        failures.push({ seq: head.seq, code: e instanceof ExtError ? e.code : 'internal_error' });
        // capture-core will refuse the same entry again: drop it, counted, rather than block the
        // queue behind it. A transport failure leaves everything in place for the next drain.
        if (e instanceof ExtError && e.code === 'core_refused') {
          queue.ack(head.seq, 1);
          continue;
        }
        break;
      }
    }
    return { sent, failures };
  }

  function isConnected() {
    return connected;
  }

  return { connect, sendRequest, sendOneWay, drain, isConnected };
}
