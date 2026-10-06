/**
 * mode-policy.js — what the extension may do with a request, decided before it touches the body.
 *
 * capture-core is authoritative for the effective mode; the extension holds a cache of the signed
 * policy it receives over the native channel. The cache fails closed: absent, stale, unparseable or
 * unknown resolves to `m0`, and at `m0` the body is not read at all.
 *
 * M0 is enforced at registration: the body-bearing webRequest lane is filtered by
 * `isBodyBearing(url)`, so for an M0 destination Chrome never hands the extension `requestBody`.
 *
 * Nothing here persists. The bundle lives in memory only and its version is reported on the health
 * channel.
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
      // An unusable bundle is not "no policy": keep the previous one, marked stale. With none,
      // everything stays at M0.
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
   * Resolve the mode for a request: most restrictive wins over every entry that applies, and any
   * failure to resolve returns M0 rather than a wider mode.
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

    // The tenant default applies only when no scope entry matches. Folding it into the
    // most-restrictive reduction would let an M0 default silently override every explicit scope
    // entry.
    const entries = matches.length
      ? matches
      : [{ pattern: '(default)', mode: isMode(current.bundle.default_mode) ? current.bundle.default_mode : CONSERVATIVE_MODE }];

    // most_restrictive = the lowest value in m0 < m1 < m2 < m3
    let winner = entries[0];
    for (const m of entries) if (order(m.mode) < order(winner.mode)) winner = m;
    return { mode: winner.mode, reason: `scope:${winner.pattern}`, policy_version: current.bundle.policy_version, unsigned: false };
  }

  /** The gate the body-bearing listener is registered behind. */
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

  /**
   * The bundle's body-lane include list, if the deployment supplies one. Chrome match patterns
   * cannot express "everything except", so a deployment that needs a strict observation set names
   * it here; otherwise the lane observes broadly and the per-request gate does the work.
   */
  function bodyLanePatterns() {
    const p = current && current.bundle.body_lane_patterns;
    return Array.isArray(p) ? p.slice() : [];
  }

  function releaseState() {
    const r = current && current.bundle.classifier_release;
    return r && typeof r.state === 'string' ? r.state : 'rolled_back';
  }

  /**
   * How long a `warned` request waits for its human. Bundle-overridable, because the right number
   * is a deployment's decision about its own users; the default is exported from enforce.js.
   */
  function confirmationWindowMs() {
    const v = current && current.bundle.confirmation_window_ms;
    return Number.isFinite(v) && v > 0 ? v : 20_000;
  }

  /** The caps come from the bundle; without a bundle there is no cap to exceed, only M0. */
  function caps() {
    const c = (current && current.bundle.mode_caps) || {};
    return {
      body_bytes: Number.isFinite(c.body_bytes) ? c.body_bytes : null,
      attachment_bytes: Number.isFinite(c.attachment_bytes) ? c.attachment_bytes : null,
    };
  }

  /** The bundle's destination sets: evidence for the predicate, never the decision. */
  function discoverySets() {
    if (!current) return {};
    return {
      sanctioned: current.bundle.sanctioned_hosts || [],
      denied: current.bundle.denied_hosts || [],
      seed: current.bundle.seed_hosts || [],
    };
  }

  return { applyBundle, modeFor, isBodyBearing, snapshot, clear, rules, releaseState, caps, discoverySets, confirmationWindowMs, bodyLanePatterns };
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
