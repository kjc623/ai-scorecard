/**
 * mode-policy.js — what the extension is allowed to do with a request, decided before it
 * touches the body (§11.2: "the mode is applied before content is read").
 *
 * Two facts shape this module.
 *
 * 1. **capture-core is authoritative for the effective mode** (Lead, task-6; §3.4's
 *    bidirectional native channel exists so the extension "can be told ... a destination's
 *    effective mode"). What the extension holds is a *cache of signed policy*. §11.3:
 *    "A device cannot exceed a ceiling it holds no bundle entry for" and "a matrix that fails
 *    to resolve resolves *downward*". The cache therefore fails closed: absent, stale,
 *    unparseable or unknown ⇒ `m0`, and `m0` means the body is not read at all.
 *
 * 2. **M0 is enforced at registration, not inside the handler.** The pipeline asks
 *    `isBodyBearing(url)` before it registers the body-bearing webRequest lane, so for an M0
 *    destination Chrome never hands the extension `requestBody`. That makes §7.1's "the body
 *    is not retained, hashed or sent" a property of the listener filter — one a test can
 *    assert on — rather than a promise about what a handler does after the bytes arrive.
 *
 * Nothing here persists. §11.3's mode-change attribution and the notice gate are policy the
 * bundle carries; the extension stores the bundle it is enforcing in memory only, and reports
 * its version on the health channel.
 */

import { MODE, modeReadsContent } from './messages.js';

/** The conservative resolution, used for every uncertainty in this module. */
export const CONSERVATIVE_MODE = MODE.M0;

/** How long a cached bundle stays usable without a refresh before everything resolves downward. */
export const DEFAULT_STALE_AFTER_MS = 6 * 60 * 60 * 1000;

/**
 * @typedef {object} Bundle
 * @property {string} policy_version
 * @property {number} [effective_at_ms]
 * @property {Record<string,string>} [scope]      pattern -> mode; keys may be `host`, `host:port`, `*.suffix`, `tool:<fp>`
 * @property {object} [mode_caps]                 { attachment_bytes, body_bytes }
 * @property {object} [classifier_release]        { version, state } — shadow / enforcing / rolled_back
 * @property {string} [default_mode]
 */

/**
 * Build the in-memory policy cache. `now` is injected so expiry is testable without a clock.
 * @returns {{applyBundle: Function, modeFor: Function, isBodyBearing: Function, snapshot: Function, clear: Function}}
 */
export function createPolicyCache({ now = () => Date.now(), staleAfterMs = DEFAULT_STALE_AFTER_MS } = {}) {
  /** @type {{bundle: Bundle, loadedAt: number, stale: boolean}|null} */
  let current = null;
  /** Hosts known to resolve to a non-reading mode, so the body lane can be excluded per URL pattern. */
  let m0Hosts = new Set();

  function applyBundle(bundle, { at = now() } = {}) {
    if (!bundle || typeof bundle !== 'object' || typeof bundle.policy_version !== 'string' || bundle.policy_version === '') {
      // An unusable bundle is not "no policy": keep the previous one, exactly as §13.3 does
      // for a signature failure, and report it. If there is none, we stay at M0.
      current = current ? { ...current, stale: true } : null;
      rebuildM0();
      return { applied: false, reason: 'unusable_bundle', policy_version: current ? current.bundle.policy_version : null };
    }
    current = { bundle, loadedAt: at, stale: false };
    rebuildM0();
    return { applied: true, reason: 'ok', policy_version: bundle.policy_version };
  }

  function rebuildM0() {
    m0Hosts = new Set();
    if (!current) return;
    for (const [pattern, mode] of Object.entries(current.bundle.scope || {})) {
      if (!modeReadsContent(mode)) m0Hosts.add(pattern);
    }
  }

  function expired(at) {
    return !current || at - current.loadedAt > staleAfterMs;
  }

  /**
   * Resolve the mode for a request. Order is most-restrictive-wins over every entry that
   * applies (§11.1), and any failure to resolve returns M0 rather than a wider mode.
   * @param {{host: string, tool_fingerprint?: string, media_type?: string, size_bytes?: number, url?: string}} target
   * @returns {{mode: string, reason: string, policy_version: string|null, unsigned: boolean}}
   */
  function modeFor(target, { at = now(), override } = {}) {
    if (override !== undefined && override !== null) {
      return {
        mode: isMode(override) ? override : CONSERVATIVE_MODE,
        reason: isMode(override) ? 'core_answer' : 'core_answer_unrecognised',
        policy_version: current ? current.bundle.policy_version : null,
        unsigned: false,
      };
    }
    if (!current) return { mode: CONSERVATIVE_MODE, reason: 'no_bundle', policy_version: null, unsigned: true };
    if (expired(at)) return { mode: CONSERVATIVE_MODE, reason: 'bundle_stale', policy_version: current.bundle.policy_version, unsigned: true };

    const host = String(target.host || '').toLowerCase();
    const matches = [];
    for (const [pattern, mode] of Object.entries(current.bundle.scope || {})) {
      if (!isMode(mode)) continue;
      if (patternMatches(pattern, host, target.tool_fingerprint)) matches.push({ pattern, mode });
    }
    const defaultMode = isMode(current.bundle.default_mode) ? current.bundle.default_mode : CONSERVATIVE_MODE;
    matches.push({ pattern: '(default)', mode: defaultMode });

    // most_restrictive = the lowest value in m0 < m1 < m2 < m3
    let winner = matches[0];
    for (const m of matches) if (order(m.mode) < order(winner.mode)) winner = m;
    return { mode: winner.mode, reason: `scope:${winner.pattern}`, policy_version: current.bundle.policy_version, unsigned: false };
  }

  /** §11.2/§7.1: the gate the pipeline registers the body-bearing listener behind. */
  function isBodyBearing(url) {
    let host = '';
    try {
      host = new URL(url).host.toLowerCase();
    } catch {
      return false;
    }
    const resolved = modeFor({ host });
    if (!modeReadsContent(resolved.mode)) return false;
    // Belt and braces: even if the scope matrix says otherwise, a pattern that this device
    // knows to be M0 keeps the body lane off.
    for (const pattern of m0Hosts) {
      if (patternMatches(pattern, host, undefined)) return false;
    }
    return true;
  }

  function snapshot() {
    if (!current) return { policy_version: null, stale: false, present: false, m0_patterns: [] };
    return {
      policy_version: current.bundle.policy_version,
      stale: current.stale,
      present: true,
      classifier_release: current.bundle.classifier_release || null,
      m0_patterns: [...m0Hosts],
    };
  }

  function clear() {
    current = null;
    m0Hosts = new Set();
  }

  /** Policy data the enforcement path needs; never authoritative, always a cache. */
  function rules() {
    return current ? current.bundle.rules || [] : [];
  }

  function releaseState() {
    const r = current && current.bundle.classifier_release;
    return r && typeof r.state === 'string' ? r.state : 'rolled_back';
  }

  /** §11.3: the cap comes from the bundle; absent a bundle there is no cap to exceed, only M0. */
  function caps() {
    const c = (current && current.bundle.mode_caps) || {};
    return {
      body_bytes: Number.isFinite(c.body_bytes) ? c.body_bytes : null,
      attachment_bytes: Number.isFinite(c.attachment_bytes) ? c.attachment_bytes : null,
    };
  }

  /**
   * The §8.2 destination sets, for the predicate's "evidence, never sufficient" row. These are
   * *discovery* hints (A9/C8: "a destination the tenant already sanctioned or denied may shift a
   * prior"), never the decision — §2.1 forbids deciding by hostname.
   */
  function discoverySets() {
    if (!current) return {};
    return {
      sanctioned: current.bundle.sanctioned_hosts || [],
      denied: current.bundle.denied_hosts || [],
      seed: current.bundle.seed_hosts || [],
    };
  }

  return { applyBundle, modeFor, isBodyBearing, snapshot, clear, rules, releaseState, caps, discoverySets };
}

function isMode(m) {
  return m === MODE.M0 || m === MODE.M1 || m === MODE.M2 || m === MODE.M3;
}

function order(mode) {
  return { m0: 0, m1: 1, m2: 2, m3: 3 }[mode];
}

/** Scope keys: exact host, host:port, `*.suffix`, or `tool:<fingerprint>`. */
export function patternMatches(pattern, host, toolFingerprint) {
  const p = String(pattern || '').toLowerCase();
  if (!p) return false;
  if (p.startsWith('tool:')) return Boolean(toolFingerprint) && p.slice(5) === String(toolFingerprint).toLowerCase();
  if (p === '*' || p === '<all_urls>') return true;
  if (p.startsWith('*.')) {
    const suffix = p.slice(2);
    return host === suffix || host.endsWith(`.${suffix}`);
  }
  return p === host;
}
