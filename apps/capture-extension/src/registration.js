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
  let bodyFilter = [];
  let installed = false;
  let reinstallCount = 0;

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

  function install() {
    adapter.webRequest.onBeforeRequest(guard(metadataHandler), { urls: ALL_URLS, types });
    if (onResponse) {
      adapter.webRequest.onCompleted(responseHandler, { urls: ALL_URLS, types });
    }
    // Lane B is registered only when its filter is non-empty. Until the first policy arrives
    // `computeBodyFilter` returns [] — the extension has no bundle entry for any destination, so
    // §11.3's "cannot exceed a ceiling it holds no bundle entry for" resolves everything to M0 and
    // the body-bearing listener is simply not installed. Chrome then never produces `requestBody`
    // for this extension at all, which is the strongest form of §11.2's guarantee.
    if (bodyFilter.length > 0) {
      adapter.webRequest.onBeforeRequestWithBody(guard(bodyHandler), { urls: bodyFilter.slice(), types });
      bodyInstalled = true;
    }
  }

  let bodyInstalled = false;

  const responseHandler = (detail) => {
    Promise.resolve(onResponse({ detail })).catch(() => {
      /* a response record corroborates an observation; it is never the observation itself */
    });
  };

  /**
   * Re-derive lane B's filter and (re-)register it, or remove it when policy now says every
   * destination is M0. Called on every policy sync, which is what makes a mode change take effect
   * without a browser restart (§11.3: "the device stores the bundle it is enforcing").
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
    reinstallCount += 1;
    if (bodyFilter.length > 0) {
      adapter.webRequest.onBeforeRequestWithBody(guard(bodyHandler), { urls: bodyFilter.slice(), types });
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
    get metadataFilter() {
      return installed ? ALL_URLS.slice() : [];
    },
    get bodyFilter() {
      return bodyFilter.slice();
    },
    get reinstallCount() {
      return reinstallCount;
    },
    refresh,
  };
}

/**
 * The URL patterns Chrome is given for lane B.
 *
 * Three cases, in order of how much the bundle has decided:
 *
 *   1. **No bundle at all** ⇒ `[]`: lane B is not installed, so no destination is body-bearing.
 *      §11.3's "a device cannot exceed a ceiling it holds no bundle entry for" makes this the
 *      correct starting state, and it also makes §11.2's M0 guarantee structural: the extension
 *      has not merely promised not to read those bodies, it has not asked Chrome for them.
 *   2. **A bundle with a body-lane include list** ⇒ exactly that list. Chrome match patterns
 *      cannot express "everything except", so a deployment that wants a strict observation set
 *      names it, and the exclusion of M0 destinations is then by construction.
 *   3. **A bundle with no include list** ⇒ `<all_urls>`. Broad observation, narrow emission
 *      (§7.1); the M0 destinations are excluded per-request by the handler guard instead.
 */
export function computeBodyFilter(policy) {
  const snapshot = typeof policy.snapshot === 'function' ? policy.snapshot() : null;
  const includes = typeof policy.bodyLanePatterns === 'function' ? policy.bodyLanePatterns() : [];
  const never = NEVER_BODY_BEARING.slice();
  if (includes && includes.length > 0) {
    return includes.filter((p) => !never.includes(p));
  }
  if (!snapshot || !snapshot.present) return [];
  return ALL_URLS.slice();
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
