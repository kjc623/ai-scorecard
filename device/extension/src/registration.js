/**
 * registration.js — the two webRequest lanes. "The extension does not read a body it may not read"
 * is a property of the listener filter, not a promise inside a handler:
 *
 *   metadata lane  addListener(handler, {urls}, [...])                          always installed
 *   body lane      addListener(handler, {urls: bodyFilter}, [..., 'requestBody']) gated by policy
 *
 * While the policy resolves every destination to M0 the body lane is not registered, so Chrome
 * never produces `requestBody`. Inside the handler `isBodyBearing(url)` covers the window between a
 * policy change and the filter rebuild, and the pipeline's mode gate, which knows the tool, is the
 * last check.
 *
 * Blocking: only a force-installed extension is granted `webRequestBlocking`, and a blocking
 * registration without the grant is accepted silently and never invoked. So the lanes register
 * with 'blocking' only when the grant is held; without it observation is unchanged and the health
 * report says enforcement is unavailable.
 */

import { normaliseBody } from './request-body.js';

export const ALL_URLS = ['<all_urls>'];

/** The request types the extension observes, on every host. */
export const OBSERVED_TYPES = ['main_frame', 'sub_frame', 'xmlhttprequest', 'websocket', 'other'];

export function installLanes({
  adapter,
  policy,
  onBodyLane,
  onMetadataLane,
  onResponse = null,
  types = OBSERVED_TYPES,
  /** False means observation still works and enforcement is reported unavailable. */
  blockingAvailable = true,
}) {
  /** @type {string[]} what the body lane is currently registered for. */
  let bodyFilter = [];
  let installed = false;
  let bodyInstalled = false;

  async function bodyHandler(detail) {
    // The filter is rebuilt on policy change; a request already in flight is checked again here.
    if (!policy.isBodyBearing(detail.url)) {
      return toBlockingResponse(await onMetadataLane({ detail, tab_context: tabContextOf(detail) }));
    }
    const body = normaliseBody(detail.requestBody);
    const result = await onBodyLane({ detail, body, body_capable: true, tab_context: tabContextOf(detail) });
    return toBlockingResponse(result);
  }

  async function metadataHandler(detail) {
    return toBlockingResponse(await onMetadataLane({ detail, tab_context: tabContextOf(detail) }));
  }

  function install() {
    adapter.webRequest.onBeforeRequest(guard(metadataHandler), {
      urls: ALL_URLS,
      types,
      blocking: blockingAvailable,
    });
    if (onResponse) {
      // The response lane is not blocking: a response record can only corroborate an observation
      // that has already been released, so it has no business delaying anything.
      adapter.webRequest.onCompleted(guard(onResponse), { urls: ALL_URLS, types });
    }
    // The body lane is registered only when its filter is non-empty. Until the first policy
    // arrives every destination resolves to M0, so no body listener exists at all.
    if (bodyFilter.length > 0) {
      adapter.webRequest.onBeforeRequestWithBody(guard(bodyHandler), {
        urls: bodyFilter.slice(),
        types,
        blocking: blockingAvailable,
      });
      bodyInstalled = true;
    }
  }

  /**
   * Re-derive the body lane's filter and re-register it, or remove it when policy now resolves
   * every destination to M0. Called on every policy sync, so a mode change takes effect without a
   * browser restart.
   */
  function refresh() {
    const next = computeBodyFilter(policy);
    const changed = next.join('\n') !== bodyFilter.join('\n');
    bodyFilter = next;
    if (!installed) {
      installed = true;
      install();
      return true;
    }
    if (!changed) return false;
    if (bodyFilter.length > 0) {
      adapter.webRequest.onBeforeRequestWithBody(guard(bodyHandler), {
        urls: bodyFilter.slice(),
        types,
        blocking: blockingAvailable,
      });
      bodyInstalled = true;
    } else {
      adapter.webRequest.removeBodyLane();
      bodyInstalled = false;
    }
    return true;
  }

  return {
    get bodyLaneInstalled() {
      return bodyInstalled;
    },
    get enforcement() {
      return blockingAvailable ? 'blocking' : 'observation_only';
    },
    get bodyFilter() {
      return bodyFilter.slice();
    },
    refresh,
  };
}

/**
 * The URL patterns Chrome is given for the body lane: `[]` while no content may be read anywhere
 * (no bundle, or a tenant default that reads nothing, which no other input can raise), so the body
 * lane is not installed and no body is requested; otherwise `<all_urls>`, with the handler guard
 * and the pipeline's mode gate deciding per request.
 */
export function computeBodyFilter(policy) {
  return policy.readsAnyContent() ? ALL_URLS.slice() : [];
}

function guard(handler) {
  return (detail) =>
    Promise.resolve()
      .then(() => handler(detail))
      .catch((e) => {
        // Fail open: a broken classifier must not become a broken browser.
        if (typeof console !== 'undefined' && console.warn) console.warn('[capture] observation failed; failing open', e);
        return undefined;
      });
}

function toBlockingResponse(result) {
  if (!result) return undefined;
  // `blocked` cancels through webRequestBlocking; everything else releases the request.
  return result.cancel ? { cancel: true } : undefined;
}

/** The automation marker is read from request headers, never from a page global. */
export function tabContextOf(detail) {
  const ctx = [];
  const headers = (detail && detail.requestHeaders) || {};
  if (
    headers['x-automation'] !== undefined ||
    headers['x-agent'] !== undefined ||
    /headless|automation/i.test(String(headers['user-agent'] || ''))
  ) {
    ctx.push('automation_marker');
  }
  return ctx;
}
