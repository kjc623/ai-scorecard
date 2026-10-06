/**
 * adapter.js — the interface between the extension and `chrome.*`.
 *
 * Every chrome API the extension touches is described here as an interface; `chrome-adapter.js`
 * builds the browser implementation from a `chrome` object. No other module references the
 * global `chrome` (a test greps the tree), so the whole decision path runs under `node --test`
 * against `test-support/fake-chrome.mjs`.
 *
 * The chrome.* surface, complete:
 *
 *   chrome.webRequest.onBeforeRequest.addListener(fn, filter, ["blocking"?, "requestBody"?])
 *   chrome.webRequest.onBeforeRequest.removeListener(fn)
 *   chrome.webRequest.onCompleted.addListener(fn, filter)
 *   chrome.runtime.connectNative(application) / lastError
 *   chrome.runtime.onMessage.addListener(fn) / sendMessage(msg) / getURL(path)
 *   chrome.tabs.sendMessage(tabId, msg) / query({active: true, currentWindow: true})
 *   chrome.alarms.create(name, {periodInMinutes}) / onAlarm.addListener(fn)
 *   chrome.permissions.contains({permissions: ["webRequestBlocking"]})
 *   (content script world) globalThis.crypto.subtle
 *
 * Not used: chrome.declarativeNetRequest (it cannot make a decision that depends on the body),
 * chrome.storage (the extension holds nothing durable), analytics, remote code.
 */

/** @typedef {'m0'|'m1'|'m2'|'m3'} CollectionMode */

/**
 * @typedef {object} RequestRecordInput
 * A normalised request. Chrome exposes two requestBody shapes: form encodings arrive as parsed
 * `formData`, everything else as `raw` bytes (copied here from Chrome's ArrayBuffer).
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
 * @property {(handler: (d: RequestRecordInput) => any, opts: {urls: string[], types?: string[], blocking?: boolean}) => void} onBeforeRequest
 *   Metadata-only lane: registered without `requestBody`.
 * @property {(handler: (d: RequestRecordInput) => any, opts: {urls: string[], types?: string[], blocking?: boolean}) => void} onBeforeRequestWithBody
 *   Body lane: the only registration that asks for `requestBody`, so "an M0 destination's body is
 *   never read" is a property of what is registered rather than of a handler's discipline.
 * @property {() => void} removeBodyLane
 * @property {(handler: (d: ResponseRecordInput) => void, opts: {urls: string[], types?: string[]}) => void} onCompleted
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
 * @property {{hasWebRequestBlocking: () => Promise<boolean>}} permissions
 * @property {Crypto} crypto                             WebCrypto, for SHA-256 and UUIDs
 * @property {() => number} now                          milliseconds, monotonic where the host allows
 * @property {(path: string) => string} getURL
 * @property {(n: number) => string[]} randomUUIDs       `n` v4 UUIDs
 * @property {(spec: WarnSpec) => Promise<WarnAnswer>} warnUser  renders the warn confirmation
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

/** An error with a code from the closed set the health report counts (never prose). */
export class ExtError extends Error {
  /** @param {string} code @param {string} message @param {object} [detail] */
  constructor(code, message, detail = {}) {
    super(message);
    this.name = 'ExtError';
    this.code = code;
    this.detail = detail;
  }
}

export function errorCode(e) {
  return e instanceof ExtError && typeof e.code === 'string' ? e.code : 'internal_error';
}

/** The single place that knows the global object is called `chrome`. */
export function chromeApi(scope = globalThis) {
  const api = scope.chrome;
  if (!api) throw new ExtError('internal_error', 'chrome.* is not available in this scope');
  return api;
}
