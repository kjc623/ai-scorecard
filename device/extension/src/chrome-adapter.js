/**
 * chrome-adapter.js — builds the adapter from a real `chrome` object.
 *
 * It normalises Chrome's shapes into the adapter's, maps the two `webRequest` lanes onto
 * `onBeforeRequest` with and without `requestBody`, and nothing else: no policy, no
 * classification, no storage.
 */

import { ExtError, chromeApi } from './adapter.js';

/** Upper bound on how long a blocking listener holds a request, whatever the handler does. */
const BLOCKING_TIMEOUT_MS = 25_000;

export function createChromeAdapter(scope = globalThis) {
  const api = chromeApi(scope);
  const cryptoObj = scope.crypto || globalThis.crypto;
  /** The live body-lane listener, so it can be removed when policy closes every destination. */
  let bodyLaneListener = null;

  function headerMap(list) {
    const out = {};
    for (const h of list || []) {
      if (h && typeof h.name === 'string') out[h.name.toLowerCase()] = String(h.value ?? '');
    }
    return out;
  }

  /** Chrome hands `bytes` as an ArrayBuffer; copy it, because a detached buffer is silent data loss. */
  function rawParts(raw) {
    if (!Array.isArray(raw)) return null;
    return raw.map((p) => {
      const src = p && p.bytes;
      if (!src) return { bytes: null };
      if (src instanceof ArrayBuffer) return { bytes: src.slice(0) };
      if (ArrayBuffer.isView(src)) {
        return { bytes: new Uint8Array(src.buffer.slice(src.byteOffset, src.byteOffset + src.byteLength)) };
      }
      return { bytes: null };
    });
  }

  function normaliseFormData(fd) {
    if (!fd || typeof fd !== 'object') return null;
    const out = {};
    for (const [k, values] of Object.entries(fd)) {
      out[k] = (Array.isArray(values) ? values : [values]).map((v) =>
        typeof v === 'string' ? v : v && typeof v.value === 'string' ? v.value : '',
      );
    }
    return out;
  }

  function requestRecord(d, withBody) {
    return {
      requestId: d.requestId,
      url: d.url,
      method: d.method,
      tabId: d.tabId,
      frameId: d.frameId,
      requestHeaders: headerMap(d.requestHeaders),
      type: d.type,
      timeStamp: d.timeStamp,
      initiator: d.initiator || d.documentUrl || '',
      fromCache: Boolean(d.fromCache),
      // `requestBody` is present only on the lane registered with it; on the metadata lane this
      // is null and stays null, which is what makes the M0 guarantee checkable.
      requestBody: withBody && d.requestBody
        ? { raw: rawParts(d.requestBody.raw), formData: normaliseFormData(d.requestBody.formData) }
        : null,
      requestBodyPresent: Boolean(d.requestBody),
    };
  }

  function register(handler, opts, withBody) {
    // `blocking` comes from the caller because the capability is per install: only a
    // policy-installed extension holds `webRequestBlocking`, and a blocking registration without
    // the grant is accepted silently and never invoked (see registration.js).
    const extraInfoSpec = [];
    if (opts.blocking) extraInfoSpec.push('blocking');
    if (withBody) extraInfoSpec.push('requestBody');
    const filter = { urls: opts.urls && opts.urls.length ? opts.urls : ['<all_urls>'] };
    if (opts.types) filter.types = opts.types;

    const listener = (details) => {
      let settled;
      try {
        settled = Promise.resolve(handler(requestRecord(details, withBody)));
      } catch (e) {
        settled = Promise.resolve(undefined);
      }
      return Promise.race([
        settled.catch(() => undefined),
        new Promise((resolve) => setTimeout(() => resolve(undefined), BLOCKING_TIMEOUT_MS)),
      ]);
    };
    api.webRequest.onBeforeRequest.addListener(listener, filter, extraInfoSpec);
    return listener;
  }

  return {
    webRequest: {
      onBeforeRequest: (handler, opts) => register(handler, opts, false),
      onBeforeRequestWithBody: (handler, opts) => {
        const listener = register(handler, opts, true);
        bodyLaneListener = listener;
        return listener;
      },
      /**
       * Unregister the body lane: when policy resolves every destination to M0 the extension must
       * stop asking Chrome for bodies, or the next request still carries bytes.
       */
      removeBodyLane: () => {
        if (bodyLaneListener) {
          try {
            api.webRequest.onBeforeRequest.removeListener(bodyLaneListener);
          } catch (e) {
            /* the listener may already be gone */
          }
          bodyLaneListener = null;
        }
      },
      onCompleted: (handler, opts) => {
        const filter = { urls: opts.urls && opts.urls.length ? opts.urls : ['<all_urls>'] };
        if (opts.types) filter.types = opts.types;
        api.webRequest.onCompleted.addListener(
          (d) =>
            handler({
              requestId: d.requestId,
              url: d.url,
              method: d.method,
              statusCode: d.statusCode,
              responseHeaders: headerMap(d.responseHeaders),
              // The predicate evaluates the request and its response contract as a pair.
              requestHeaders: headerMap(d.requestHeaders),
              type: d.type,
              timeStamp: d.timeStamp,
              fromCache: Boolean(d.fromCache),
            }),
          filter,
        );
      },
    },

    runtime: {
      connectNative: (application) => {
        const port = api.runtime.connectNative(application);
        if (!port) throw new ExtError('native_unavailable', `connectNative(${application}) failed`);
        return {
          postMessage: (m) => port.postMessage(m),
          onMessage: (fn) => port.onMessage.addListener(fn),
          onDisconnect: (fn) => port.onDisconnect.addListener(() => fn(api.runtime.lastError || null)),
          disconnect: () => port.disconnect(),
        };
      },
    },

    messages: {
      onMessage: (fn) =>
        api.runtime.onMessage.addListener((msg, sender, sendResponse) => {
          // Returning true keeps the response channel open for the async form.
          Promise.resolve(fn(msg, sender))
            .then((r) => sendResponse(r))
            .catch((e) => sendResponse({ ok: false, error: String((e && e.message) || e) }));
          return true;
        }),
      sendMessage: (message) => api.runtime.sendMessage(message),
    },

    tabs: {
      sendMessage: (tabId, message, options) => api.tabs.sendMessage(tabId, message, options),
      query: (q) => api.tabs.query(q),
    },

    alarms: {
      create: (name, info) => api.alarms.create(name, info),
      onAlarm: (fn) => api.alarms.onAlarm.addListener(fn),
    },

    crypto: cryptoObj,
    now: () => (scope.performance && typeof scope.performance.now === 'function' ? scope.performance.now() : Date.now()),
    getURL: (path) => api.runtime.getURL(path),

    /**
     * Whether this install holds `webRequestBlocking`. Only a policy-installed extension is granted
     * it, and the manifest lists it either way, so the grant is asked for rather than read from the
     * manifest. An unanswerable query defaults to `true`, the deployed (force-installed) case;
     * observation does not depend on the answer.
     */
    permissions: {
      hasWebRequestBlocking: async () => {
        try {
          if (!api.permissions || typeof api.permissions.contains !== 'function') return true;
          return await api.permissions.contains({ permissions: ['webRequestBlocking'] });
        } catch {
          return true;
        }
      },
    },
    randomUUIDs: (n) => {
      const out = [];
      for (let i = 0; i < n; i++) {
        if (cryptoObj && typeof cryptoObj.randomUUID === 'function') out.push(cryptoObj.randomUUID());
        else out.push(fallbackUuid(cryptoObj));
      }
      return out;
    },

    /**
     * The warn confirmation, rendered in the page. A tab with no content script, or an unanswered
     * prompt, reports `answered: false`; the caller turns that into a fail-open, degraded `logged`.
     */
    warnUser: async (spec) => {
      try {
        let tabId = spec.tab_id;
        if (tabId === undefined || tabId === null || tabId < 0) {
          const tabs = await api.tabs.query({ active: true, currentWindow: true });
          tabId = tabs && tabs[0] ? tabs[0].id : undefined;
        }
        if (tabId === undefined) return { proceeded: true, answered: false, reason: 'no_receiver' };
        const answer = await api.tabs.sendMessage(tabId, { type: 'capture_warn', spec });
        if (!answer || answer.answered !== true) return { proceeded: true, answered: false, reason: 'no_receiver' };
        return {
          proceeded: answer.proceeded === true,
          answered: true,
          reason: answer.proceeded ? 'user_proceeded' : 'user_cancelled',
        };
      } catch (e) {
        return { proceeded: true, answered: false, reason: 'no_receiver' };
      }
    },
  };
}

function fallbackUuid(cryptoObj) {
  const b = new Uint8Array(16);
  cryptoObj.getRandomValues(b);
  b[6] = (b[6] & 0x0f) | 0x40;
  b[8] = (b[8] & 0x3f) | 0x80;
  const h = [...b].map((x) => x.toString(16).padStart(2, '0')).join('');
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

/** The content-script half of the adapter: no webRequest, no native port, no alarms. */
export function createContentScriptAdapter(scope = globalThis) {
  const api = chromeApi(scope);
  const cryptoObj = scope.crypto || globalThis.crypto;

  return {
    messages: {
      onMessage: (fn) =>
        api.runtime.onMessage.addListener((msg, sender, sendResponse) => {
          Promise.resolve(fn(msg, sender))
            .then((r) => sendResponse(r))
            .catch((e) => sendResponse({ ok: false, error: String((e && e.message) || e) }));
          return true;
        }),
      /** Relays one attachment frame to the service worker, which owns the native channel. */
      sendMessage: (message) => api.runtime.sendMessage(message),
    },
    crypto: cryptoObj,
    now: () => (scope.performance && typeof scope.performance.now === 'function' ? scope.performance.now() : Date.now()),
    randomUUIDs: (n) => {
      const out = [];
      for (let i = 0; i < n; i++) {
        if (cryptoObj && typeof cryptoObj.randomUUID === 'function') out.push(cryptoObj.randomUUID());
        else out.push(fallbackUuid(cryptoObj));
      }
      return out;
    },
    document: scope.document,
  };
}
