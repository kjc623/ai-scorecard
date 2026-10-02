/**
 * native.js — the extension end of the native-messaging channel to `capture-core` (§3.4, A2).
 *
 * Responsibilities, and nothing else:
 *   - own the port's lifetime, connect lazily, and translate a failed connect or a disconnect
 *     into `ExtError('native_unavailable')`. It never swallows one;
 *   - correlate request/response pairs by the `id` field native.go carries for exactly that;
 *   - enforce the `MaxNativeMessageBytes` ceiling on the *framed* message before posting,
 *     because Chromium's own ceiling is the thing the attachment chunking exists for;
 *   - drain the bounded queue in order, removing an entry only after `capture-core` has
 *     accepted it. An `ack` means accepted, never "ingested" — the spool is what makes that
 *     distinction (Lead, task-6) — so nothing here increments `emitted` on an ack.
 *
 * The one thing it deliberately does not do is retry into a busy loop: a failed send leaves the
 * entry in the queue (or drops it oldest-first if the queue is full), counts the error, and
 * reports the channel `absent`.
 */

import { ExtError } from './adapter.js';
import {
  CORE_TYPE,
  MAX_NATIVE_MESSAGE_BYTES,
  REFUSAL,
  TYPE,
  framedByteLength,
  frame,
  unframe,
} from './messages.js';

/** The registered native messaging host application name. Deployment installs the manifest for it (§14). */
export const NATIVE_APP = 'com.shadowaicapture.capture_core';

export const DEFAULT_TIMEOUT_MS = 10_000;

/**
 * After a disconnect, refuse to open a new port for this long.
 *
 * Without it, a missing or broken native host produces a **tight loop**, measured in Edge 154 at
 * roughly 6,000 connect attempts per second: `connectNative` returns a port, the browser delivers
 * the disconnect for the absent host, the port is dropped, and the next send opens another. Anything
 * that triggers a send on each connect (a policy sync, a queue drain) closes the cycle and spins.
 * That is a real defect on a user's machine — sustained CPU burn for a channel that is simply not
 * there — and §3.5's "a crash loops stops retrying into a state where the user's machine is
 * intermittently broken" is the same rule applied one process down.
 *
 * The queue is what makes the wait harmless: an observation emitted during the cool-down is held,
 * counted, and delivered by the next drain once the channel is real (§3.4).
 */
export const DEFAULT_CONNECT_COOLDOWN_MS = 5_000;

/** How a refused-while-cooling-down connect is reported, so it is never mistaken for a new failure. */
export const COOLDOWN_ERROR_CODE = 'native_cooling_down';

/**
 * @param {object} opts
 * @param {import('./adapter.js').Adapter} opts.adapter
 * @param {string} [opts.application]
 * @param {number} [opts.timeoutMs]
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
  /** True once a message has actually been answered on the current port. */
  let answered = false;
  /** Wall-clock gate: no new port is opened before this, so a dead host cannot be hammered. */
  let nextConnectAllowedAt = 0;

  function connect() {
    if (port) return port;
    const remaining = nextConnectAllowedAt - Date.now();
    if (remaining > 0) {
      // Cooling down after a disconnect. Refused without opening a port and **without emitting an
      // event**, because this is not a new failure — it is the same one, still standing. Emitting
      // here is what would rebuild the loop the cool-down exists to break.
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
      // **`port_opened`, not `connected`.** A port object is not a working channel: for a host that
      // does not exist the browser hands one out and only later delivers the disconnect, so a
      // health report that called this "connected" would claim a coverage path that does not work —
      // the §15.2/INV-6 failure, and the one the browser gate caught. `connected` is emitted below,
      // when a message actually round-trips. Nothing may hang work off this event either: doing so
      // is what closed the retry loop.
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
    // Start the cool-down *here*, at the moment the channel is known bad, so the very next send in
    // the same tick cannot open another port.
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
      // frames.go: a version mismatch is a degraded handshake, never a crash.
      onEvent({ kind: 'version_mismatch', version: msg.version });
      return;
    }
    if (msg.id && pending.has(msg.id)) {
      const entry = pending.get(msg.id);
      pending.delete(msg.id);
      clearTimeout(entry.timer);
      if (msg.type === CORE_TYPE.REFUSAL) {
        const body = msg.body || {};
        // A refusal is still an answer: the channel works, the core declined this message. It proves
        // the round trip, so it counts as a working channel even though the request failed.
        markAnswered();
        entry.reject(new ExtError('core_refused', body.message || 'refused', { reason: body.reason }));
      } else {
        markAnswered();
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
      // Refused locally: the ceiling is native.go's, and a message that large would be
      // rejected by Chromium without a diagnosable error.
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

  /** Fire-and-forget: the caller keeps the queue entry and removes it on ack or refusal. */
  function sendRequest(type, body) {
    const { id } = post(type, body);
    return new Promise((resolve, reject) => {
      // NOT unref'd on purpose. In a service worker the port keeps the worker alive anyway, so
      // unref'ing would buy nothing; in a test it would let the event loop drain before the
      // timeout fires, turning "we time out and report it" into "the promise never settles",
      // which is precisely the failure mode this timer exists to prevent (C21: "no answer" is
      // `degraded`, never "nothing happened").
      const timer = setTimeout(() => {
        pending.delete(id);
        onEvent({ kind: 'timeout', type });
        reject(new ExtError('native_timeout', `no answer to ${type} within ${timeoutMs}ms`));
      }, timeoutMs);
      pending.set(id, { resolve, reject, timer, type });
    });
  }

  /**
   * A message came back: the channel is real. Emitted once per port, so `connected` means "a
   * round trip succeeded" rather than "a port object was handed out".
   */
  function markAnswered() {
    if (answered) return;
    answered = true;
    onEvent({ kind: 'connected' });
  }

  /**
   * Fire-and-forget, used where a refusal is not actionable (health).
   */
  function sendOneWay(type, body) {
    return post(type, body);
  }

  /**
   * Drain the queue oldest-first. Each entry is removed only after the ack, so a mid-drain
   * failure loses nothing that was not already dropped by the bound.
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
        // A refusal is the core's decision about this entry and it will refuse it again:
        // drop it, counted, rather than blocking the queue behind it. A transport failure
        // leaves everything in place for the next drain.
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

  function requiresAttention() {
    return pending.size > 0;
  }

  return {
    connect,
    sendRequest,
    sendOneWay,
    drain,
    isConnected,
    requiresAttention,
    get lastError() {
      return lastError;
    },
    close() {
      if (port) {
        try {
          port.disconnect();
        } catch {
          /* the host may already be gone */
        }
      }
      port = null;
      connected = false;
    },
    get application() {
      return application;
    },
  };
}

/** §3.4 policy sync: the extension holds no durable state, so this is how it gets policy after a restart. */
export function policySyncRequest(knownVersion) {
  return { known_version: knownVersion || '' };
}

export function modeQuery({ tool_fingerprint, host, media_type, size_bytes }) {
  const body = { tool_fingerprint };
  if (host) body.host = host;
  if (media_type) body.media_type = media_type;
  if (Number.isFinite(size_bytes)) body.size_bytes = size_bytes;
  return body;
}

/** The refusal-reason vocabulary, re-exported so callers never hard-code a string. */
export const REFUSALS = REFUSAL;
export { TYPE };
