/**
 * adapter.js — the ONLY module in this extension that calls `chrome.*`.
 *
 * Every chrome API the extension touches is described here as an interface, and the
 * browser implementation of that interface is built from a `chrome` object passed in.
 * Nothing else in `src/` may reference the global `chrome` (asserted by a test that greps
 * the tree), so the entire decision path is exercised by `node --test` against
 * `test-support/fake-chrome.mjs`.
 *
 * Assumed chrome.* surface — the complete list, so a reviewer can check it against a
 * real browser without reading the rest of the tree:
 *
 *   chrome.webRequest.onBeforeRequest.addListener(fn, filter, ["blocking", "requestBody"])
 *   chrome.webRequest.onBeforeRequest.addListener(fn, filter, ["blocking"])
 *   chrome.webRequest.onCompleted.addListener(fn, filter)
 *   chrome.webRequest.OnBeforeRequestOptions.DISABLE_OPTIMIZATION   (constants, not logic)
 *   chrome.runtime.connectNative(application)
 *   chrome.runtime.onMessage.addListener(fn) / sendMessage(msg)
 *   chrome.runtime.getURL(path)
 *   chrome.runtime.lastError                                       (read inside callbacks)
 *   chrome.tabs.sendMessage(tabId, msg, options) / query({active:true, currentWindow:true})
 *   chrome.alarms.create(name, {periodInMinutes}) / onAlarm.addListener(fn)
 *   chrome.storage.session.get(key) / set(obj) / remove(key)
 *   (content script world) globalThis.crypto.subtle
 *
 * Deliberately NOT used: chrome.declarativeNetRequest (cannot block inline with a
 * decision that depends on the body), chrome.storage.local (nothing durable), any
 * analytics or remote code path.
 */

/** @typedef {'m0'|'m1'|'m2'|'m3'} CollectionMode */

/**
 * @typedef {object} RequestRecordInput
 * A normalised request. The two requestBody shapes Chrome exposes:
 *   - form encodings  -> `formData` as parsed key/value pairs (E2)
 *   - everything else -> `raw` as bytes (E2); Chrome's ArrayBuffer, copied here.
 * `raw` is an ArrayBuffer and stays binary: this module must not base64 or hash a body
 * at M0, because at M0 the bytes passed here are the defect, not the data.
 * @property {string} requestId
 * @property {string} url
 * @property {string} method
 * @property {number} tabId
 * @property {number} frameId
 * @property {Record<string,string>} requestHeaders   lower-cased names
 * @property {'main_frame'|'sub_frame'|'xmlhttprequest'|'websocket'|string} type
 * @property {number} timeStamp
 * @property {ArrayBuffer|Uint8Array|null} raw
 * @property {Record<string, Array<string|{value?:string}>>|null} formData
 * @property {boolean} fromCache
 * @property {string} initiator
 */

/**
 * @typedef {object} ResponseRecordInput
 * @property {string} requestId
 * @property {string} url
 * @property {string} method
 * @property {number} statusCode
 * @property {Record<string,string>} responseHeaders  lower-cased names
 * @property {string} type
 * @property {number} timeStamp
 */

/**
 * @typedef {object} WebRequestAdapter
 * @property {(handler: (d: RequestRecordInput) => any, opts: {urls: string[], types?: string[]}) => void} onBeforeRequest
 *   Metadata-only observation lane. The extension does NOT pass `requestBody` here.
 * @property {(handler: (d: RequestRecordInput) => any, opts: {urls: string[], types?: string[]}) => void} onBeforeRequestWithBody
 *   Body-bearing lane. `requestBody` is requested here and only here, which is what makes
 *   "an M0 destination's body is never read" a property of registration rather than of
 *   discipline inside a handler.
 * @property {(handler: (d: ResponseRecordInput) => void, opts: {urls: string[], types?: string[]}) => void} onCompleted
 * @property {(pattern: string) => boolean} isBodyBearingUrl  the host gate, injected by the caller
 */

/**
 * @typedef {object} NativePort
 * @property {(message: any) => void} postMessage  throws when the channel is gone
 * @property {(fn: (message: any) => void) => void} onMessage
 * @property {(fn: (error?: {message?: string}) => void) => void} onDisconnect
 * @property {() => void} disconnect
 */

/**
 * @typedef {object} Adapter
 * @property {WebRequestAdapter} webRequest
 * @property {{connectNative: (application: string) => NativePort}} runtime
 * @property {{onMessage: (fn: (message: any, sender: any) => any) => void, sendMessage: (message: any) => Promise<any>}} messages
 * @property {{sendMessage: (tabId: number, message: any, options?: any) => Promise<any>, query: (q: any) => Promise<any[]>}} tabs
 * @property {{create: (name: string, info: {periodInMinutes: number}) => void, onAlarm: (fn: (alarm: {name: string}) => void) => void}} alarms
 * @property {{get: (key: string) => Promise<any>, set: (items: any) => Promise<void>}} session
 * @property {Crypto} crypto                             WebCrypto, for SHA-256 and UUIDs
 * @property {() => number} now                          milliseconds, monotonic where the host allows
 * @property {() => string} getURL
 * @property {(n: number) => string[]} randomUUIDs       `n` v4 UUIDs
 * @property {(spec: WarnSpec) => Promise<WarnAnswer>} warnUser  renders the §7.4 confirmation
 */

/**
 * @typedef {object} WarnSpec
 * @property {string} url
 * @property {string} host
 * @property {string} path
 * @property {string} rule_id
 * @property {string} message
 * @property {number} tab_id
 * @property {number} timeout_ms
 * @property {string} [request_id]
 */

/**
 * @typedef {object} WarnAnswer
 * @property {boolean} proceeded     true when the user chose to send anyway
 * @property {boolean} answered      false when nothing answered (no tab, script absent, timeout)
 * @property {string} reason         'user_proceeded' | 'user_cancelled' | 'timeout' | 'no_receiver'
 */

export class ExtError extends Error {
  /** @param {string} code @param {string} message @param {object} [detail] */
  constructor(code, message, detail = {}) {
    super(message);
    this.name = 'ExtError';
    this.code = code;
    this.detail = detail;
  }
}

/** Error codes are a closed set: a health report counts them, so they cannot be prose. */
export const ERROR_CODES = Object.freeze([
  'native_unavailable',
  'native_timeout',
  'native_protocol_error',
  'core_refused',
  'oversize_refused',
  'attachment_read_failed',
  'attachment_no_input',
  'evaluation_error',
  'budget_exceeded',
  'warn_unavailable',
  /** §7.4's capability is absent on this install: observation works, a `blocked` rule cannot act. */
  'enforcement_unavailable',
  'internal_error',
]);

export function isExtError(e) {
  return e instanceof ExtError;
}

export function errorCode(e) {
  return isExtError(e) && typeof e.code === 'string' ? e.code : 'internal_error';
}

/** Chrome MV3 "unsupported request body type" arrives as a thrown Error, not a code. */
export function isUnsupportedBodyError(e) {
  const m = String((e && e.message) || e || '');
  return /request ?body/i.test(m) && /(unsupported|not supported|cannot|invalid)/i.test(m);
}

/** The single place that knows the global object is called `chrome`. */
export function chromeApi(scope = globalThis) {
  const api = scope.chrome;
  if (!api) throw new ExtError('internal_error', 'chrome.* is not available in this scope');
  return api;
}
