/**
 * registration.js — where "the extension does not read a body it may not read" stops being a
 * promise and becomes a property of the listener filter.
 *
 * §11.2's last paragraph is the load-bearing sentence: "**the browser route is the one place
 * where content is genuinely not read at M0**: the extension's `requestBody` read *is* the
 * content read, so where the effective mode for a destination is already known to be M0 the
 * extension does not register the body-bearing listener for it and works from request metadata
 * only."
 *
 * So there are two lanes, and the difference between them is one option in the filter:
 *
 *   lane A (metadata)  addListener(handler, {urls}, ['blocking'])                   — always installed
 *   lane B (body)      addListener(handler, {urls: bodyFilter}, ['blocking','requestBody']) — gated
 *
 * Three guards, in order, all of which must pass before any byte is handed to the logic:
 *
 *   1. `computeBodyFilter(policy)` — destinations the policy cache resolves to M0 are not in the
 *      filter at all. Chrome therefore never produces `requestBody` for them.
 *   2. `isBodyBearing(url)` inside the handler — belt and braces for the window between a policy
 *      change and the filter being rebuilt.
 *   3. `pipeline.readBodyForMode()` — the mode gate at the point of use.
 *
 * A test asserts on the registered filter, which is the only place the guarantee can be checked
 * from outside a browser. The cost is stated rather than hidden: §11.2 says "any resulting
 * weakness in the shape predicate [is] recorded as a coverage property of M0", so a
 * metadata-lane observation carries `unread_reason: 'mode_forbids_read'` and no content.
 */

import { normaliseBody } from './request-body.js';

export const ALL_URLS = ['<all_urls>'];

/**
 * Everything the extension observes. `webRequest` with `<all_urls>` is §7.1's broad-observation
 * layer — a real privacy surface, declared in the manifest and justified in PERMISSIONS.md
 * rather than discovered in a review.
 */
export const OBSERVED_TYPES = ['main_frame', 'sub_frame', 'xmlhttprequest', 'websocket', 'other'];

/**
 * Destinations that are never body-bearing regardless of what a bundle says: §11.2's
 * conservative direction is downward, and a loopback or link-local destination inside a SaaS
 * page is not the submission the predicate is looking for.
 */
export const NEVER_BODY_BEARING = ['http://localhost/*', 'http://127.0.0.1/*', 'http://[::1]/*', 'http://169.254.0.0/16/*'];

export function installLanes({ adapter, policy, onBodyLane, onMetadataLane, onResponse = null, types = OBSERVED_TYPES }) {
  /** @type {string[]} what lane B is currently registered for — the thing the test asserts on. */
  let bodyFilter = computeBodyFilter(policy);

  async function bodyHandler(detail) {
    // Guard 2: the filter is rebuilt on policy change, but a request already in flight must not
    // slip through the window. Asking the cache directly is cheap and conservative.
    if (!policy.isBodyBearing(detail.url)) {
      return toBlockingResponse(await onMetadataLane({ detail, tab_context: tabContextOf(detail) }));
    }
    const body = normaliseBody(detail.requestBody, { capBytes: capFor(policy) });
    const result = await onBodyLane({ detail, body, body_capable: true, tab_context: tabContextOf(detail) });
    return toBlockingResponse(result);
  }

  async function metadataHandler(detail) {
    return toBlockingResponse(await onMetadataLane({ detail, tab_context: tabContextOf(detail) }));
  }

  const responseHandler = onResponse
    ? (detail) => {
        Promise.resolve(onResponse({ detail })).catch(() => {
          /* a response record corroborates an observation; it is never the observation itself */
        });
      }
    : null;

  adapter.webRequest.onBeforeRequest(guard(metadataHandler), { urls: ALL_URLS, types });
  if (bodyFilter.length > 0) {
    adapter.webRequest.onBeforeRequestWithBody(guard(bodyHandler), { urls: bodyFilter.slice(), types });
  }
  if (responseHandler) adapter.webRequest.onCompleted(responseHandler, { urls: ALL_URLS, types });

  /**
   * Re-derive lane B's filter and re-register it. Called on every policy sync, which is what
   * makes a mode change take effect without a browser restart (§11.3: "the device stores the
   * bundle it is enforcing, and the health report carries that version").
   *
   * Chrome cannot remove one URL pattern from a live listener, so a change re-installs the
   * listener. The old listener is replaced rather than duplicated: `removeListener` is not
   * available to us here, so the adapter's fake and the real adapter both key the listener by
   * lane name (`onBeforeRequestWithBody` is the body lane and there is exactly one).
   */
  function refresh() {
    const next = computeBodyFilter(policy);
    const changed = next.join('\n') !== bodyFilter.join('\n');
    bodyFilter = next;
    adapter.webRequest.onBeforeRequestWithBody(guard(bodyHandler), { urls: bodyFilter.slice(), types });
    return changed;
  }

  return {
    get bodyLaneInstalled() {
      return bodyFilter.length > 0;
    },
    get metadataFilter() {
      return ALL_URLS.slice();
    },
    get bodyFilter() {
      return bodyFilter.slice();
    },
    refresh,
  };
}

/**
 * The URL patterns Chrome is given for lane B. It is not `<all_urls>` when the bundle names M0
 * scope entries or a body-lane include list: the whole point is that those destinations are
 * excluded by pattern, so their bytes never arrive.
 *
 * `policy.bodyLanePatterns()` is the bundle's include list (a deployment's decided observation
 * set). Where the bundle gives neither an include list nor any M0 entry, the default is
 * `<all_urls>` — broad observation, narrow emission (§7.1).
 */
export function computeBodyFilter(policy) {
  const includes = typeof policy.bodyLanePatterns === 'function' ? policy.bodyLanePatterns() : [];
  const never = NEVER_BODY_BEARING.slice();
  if (includes && includes.length > 0) {
    // An explicit include list is authoritative and already excludes what the deployment decided
    // not to read. The static never-list is unioned in only as an assertion of intent.
    return includes.filter((p) => !never.includes(p));
  }
  // No include list and no M0 entries: observe broadly. With M0 entries, the exclusion is
  // applied by the handler guard, because Chrome match patterns cannot express "everything
  // except" — and a bundle that needs the strict version supplies the include list above.
  const m0 = typeof policy.m0Patterns === 'function' ? policy.m0Patterns() : [];
  if (m0.length === 0) return ALL_URLS.slice();
  const strict = typeof policy.bodyLanePatternsStrict === 'function' && policy.bodyLanePatternsStrict();
  return strict ? [] : ALL_URLS.slice();
}

function guard(handler) {
  return (detail) =>
    handler(detail).catch((e) => {
      // Fail open: a broken classifier must not become a broken browser (brief §6, §7.4).
      if (typeof console !== 'undefined' && console.warn) console.warn('[capture] observation failed; failing open', e);
      return undefined;
    });
}

function toBlockingResponse(result) {
  if (!result) return undefined;
  // §7.4: `blocked` cancels through webRequestBlocking. Everything else releases the request.
  return result.cancel ? { cancel: true } : undefined;
}

function capFor(policy) {
  const caps = typeof policy.caps === 'function' ? policy.caps() : {};
  return Number.isFinite(caps.body_bytes) && caps.body_bytes > 0 ? caps.body_bytes : undefined;
}

/** §7.5 Mode C's automation marker is read from request headers, never from a page global. */
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
