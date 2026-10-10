/**
 * fake-chrome.mjs — the fake `chrome` the suite runs against.
 *
 * It records the two facts observable at the API boundary: which listener each lane was registered
 * on and with which `extraInfoSpec` (where the M0 guarantee lives: `requestBody` present or absent),
 * and which `urls` filter each lane was given. It implements the surface `src/adapter.js` documents
 * and nothing more, because anything Chrome would not do is a false pass.
 */

const textEncoder = new TextEncoder();

export function createFakeChrome() {
  const listeners = {
    /** @type {Array<{handler: Function, filter: object, extra: string[], lane: 'metadata'|'body'}>} */
    onBeforeRequest: [],
    /** @type {Array<{handler: Function, filter: object}>} */
    onCompleted: [],
    /**
     * @type {Array<{handler: Function, filter: object, extra: string[]}>}
     * Chrome allows exactly one listener per extension per event for our purposes; a second
     * registration replaces the first, which is how `registration.refresh()` behaves.
     */
  };

  const state = {
    nativePorts: [],
    alarms: [],
    alarmHandlers: [],
    messages: [],
    messageHandlers: [],
    tabAnswers: new Map(),
    tabQueries: [],
    lastError: undefined,
    /** Every `connectNative` attempt, whether or not it succeeded. */
    connectAttempts: 0,
    failConnect: false,
    failPostAfter: Infinity,
    posted: [],
    /** Whether this install holds `webRequestBlocking`. False models an install not forced by policy. */
    blockingGranted: true,
    /** The installed manifest's version, and what `requestUpdateCheck` answers. */
    manifestVersion: '1.0.0',
    updateCheckStatus: 'no_update',
    updateChecks: 0,
    updateAvailableHandlers: [],
    reloads: 0,
  };

  const onMessageAdd = [];
  /** Every add/remove on the webRequest listener, in order, so a test can assert on the sequence. */
  const registrations = [];

  const chrome = {
    webRequest: {
      onBeforeRequest: {
        addListener(handler, filter, extra) {
          // The body lane is the one asking for `requestBody`. Exactly one listener per lane, as
          // in Chrome: a later registration replaces the earlier one.
          const lane = Array.isArray(extra) && extra.includes('requestBody') ? 'body' : 'metadata';
          const idx = listeners.onBeforeRequest.findIndex((l) => l.lane === lane);
          const entry = { handler, filter, extra: extra || [], lane };
          if (idx >= 0) listeners.onBeforeRequest[idx] = entry;
          else listeners.onBeforeRequest.push(entry);
          registrations.push({ action: 'add', lane, extra: (extra || []).slice(), urls: (filter.urls || []).slice() });
        },
        removeListener() {
          const idx = listeners.onBeforeRequest.findIndex((l) => l.lane === 'body');
          if (idx >= 0) {
            listeners.onBeforeRequest.splice(idx, 1);
            registrations.push({ action: 'remove', lane: 'body' });
          }
        },
      },
      onCompleted: {
        addListener(handler, filter) {
          listeners.onCompleted[0] = { handler, filter };
        },
      },
    },

    /**
     * The blocking capability probe. `blockingGranted` is settable so a test can model an install
     * whose manifest declares `webRequestBlocking` without the grant.
     */
    permissions: {
      async contains({ permissions }) {
        if (!Array.isArray(permissions) || !permissions.includes('webRequestBlocking')) return true;
        return state.blockingGranted;
      },
    },

    runtime: {
      id: 'fake-extension-id',
      get lastError() {
        return state.lastError;
      },
      getURL: (p) => `chrome-extension://fake-extension-id/${p}`,
      getManifest: () => ({ manifest_version: 3, version: state.manifestVersion }),
      async requestUpdateCheck() {
        state.updateChecks += 1;
        return { status: state.updateCheckStatus };
      },
      onUpdateAvailable: {
        addListener: (fn) => state.updateAvailableHandlers.push(fn),
      },
      reload() {
        state.reloads += 1;
      },
      connectNative(application) {
        state.connectAttempts += 1;
        if (state.failConnect) {
          state.lastError = { message: `Specified native messaging host not found: ${application}` };
          throw new Error(state.lastError.message);
        }
        const port = createFakePort(application, state);
        state.nativePorts.push(port);
        return port;
      },
      onMessage: {
        addListener(handler) {
          state.messageHandlers.push(handler);
          onMessageAdd.push(handler);
        },
      },
      sendMessage(message) {
        state.messages.push(message);
        // The content script's `bridge` and the worker's relay both land here.
        for (const handler of state.messageHandlers) {
          const result = handler(message, { tab: { id: 1 } }, () => {});
          if (result && typeof result.then === 'function') return result;
        }
        return Promise.resolve(undefined);
      },
    },

    tabs: {
      async sendMessage(tabId, message) {
        state.messages.push({ tabId, message });
        if (state.tabAnswers.has(tabId)) {
          const answer = state.tabAnswers.get(tabId);
          return typeof answer === 'function' ? answer(message) : answer;
        }
        const err = new Error('Could not establish connection. Receiving end does not exist.');
        state.lastError = { message: err.message };
        throw err;
      },
      async query(q) {
        state.tabQueries.push(q);
        return [{ id: 1, url: 'https://example-ai.invalid/' }];
      },
    },

    alarms: {
      create(name, info) {
        state.alarms.push({ name, info });
      },
      onAlarm: {
        addListener(fn) {
          state.alarmHandlers.push(fn);
        },
      },
    },
  };

  /** Drive one request through a lane. Returns the blocking response Chrome would honour. */
  async function drive(lane, detail) {
    const entry = listeners.onBeforeRequest.find((l) => l.lane === lane);
    if (!entry) throw new Error(`no ${lane} listener registered`);
    return await entry.handler(detail);
  }

  return {
    chrome,
    state,
    listeners,
    registrations,
    drive,
    /** Which lanes are registered, and with what extraInfoSpec — the M0 assertion surface. */
    registration(lane) {
      const entry = listeners.onBeforeRequest.find((l) => l.lane === lane);
      return entry ? { extra: entry.extra.slice(), urls: (entry.filter.urls || []).slice(), types: (entry.filter.types || []).slice() } : null;
    },
    /** Chrome's message-passing surface used by the content script tests. */
    deliverToContentScript(handler, message) {
      return handler(message, { tab: { id: 1 } }, () => {});
    },
    completeListener() {
      return listeners.onCompleted[0] ? listeners.onCompleted[0].handler : null;
    },
  };
}

function createFakePort(application, state) {
  const messageListeners = [];
  const disconnectListeners = [];
  let closed = false;

  const port = {
    application,
    /** `postMessage` throws before any onDisconnect in Chrome; the fake does the same. */
    postMessage(message) {
      if (closed) throw new Error('Attempting to use a disconnected port object');
      state.posted.push(message);
      if (state.posted.length > state.failPostAfter) {
        throw new Error('Attempting to use a disconnected port object');
      }
    },
    onMessage: { addListener: (fn) => messageListeners.push(fn) },
    onDisconnect: { addListener: (fn) => disconnectListeners.push(fn) },
    disconnect() {
      closed = true;
    },
    /** Test-side only: pretend capture-core answered. */
    __answer(message) {
      for (const fn of messageListeners) fn(message);
    },
    /** Test-side only: pretend the host exited. */
    __die(message = 'Native host has exited.') {
      closed = true;
      state.lastError = { message };
      for (const fn of disconnectListeners) fn({ message });
    },
    get closed() {
      return closed;
    },
    get listenerCount() {
      return messageListeners.length;
    },
  };
  return port;
}

/** A request detail object shaped the way Chrome hands one to `onBeforeRequest`. */
export function chromeRequest({
  requestId = 'r1',
  url = 'https://example-ai.invalid/v1/chat/completions',
  method = 'POST',
  type = 'xmlhttprequest',
  tabId = 1,
  headers = {},
  body = null,
  formData = null,
} = {}) {
  const requestHeaders = Object.entries(headers).map(([name, value]) => ({ name, value }));
  const detail = {
    requestId,
    url,
    method,
    type,
    tabId,
    frameId: 0,
    timeStamp: 1_700_000_000_000,
    initiator: 'https://example-ai.invalid/',
    requestHeaders,
    fromCache: false,
  };
  if (body !== null) {
    detail.requestBody = { raw: [{ bytes: toArrayBuffer(body) }] };
  }
  if (formData !== null) {
    detail.requestBody = { formData };
  }
  return detail;
}

export function toArrayBuffer(input) {
  if (input instanceof ArrayBuffer) return input;
  if (ArrayBuffer.isView(input)) return input.buffer.slice(input.byteOffset, input.byteOffset + input.byteLength);
  return textEncoder.encode(String(input)).buffer;
}

/** 0xC3 0x28 is not valid UTF-8: the decode must fail strictly rather than substituting U+FFFD. */
export const INVALID_UTF8 = new Uint8Array([0x7b, 0x22, 0x61, 0x22, 0x3a, 0xc3, 0x28, 0x7d]);

/** The most common real invalid-UTF-8 case: a latin-1 encoded body. */
export const LATIN1_TEXT = new Uint8Array([0x63, 0x61, 0x66, 0xe9]);
