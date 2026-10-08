/**
 * mode-policy.js — what the extension may do with a request, decided before it touches the body.
 *
 * capture-core verifies the signed policy bundle and hands the extension its decoded payload over
 * the native channel (`policy_bundle`); this module is the in-memory cache of that bundle. The
 * cache fails closed: absent, stale, unparseable or unknown resolves to `m0`, and at `m0` the body
 * is not read at all.
 *
 * The mode is resolved from the bundle fields the device uses: `tenant_default_mode` and
 * `tool_modes[tool_fingerprint]`, the most restrictive applying, as capture-core's `Resolve` does
 * for those two inputs. The other inputs of that resolution (population and device scopes, the
 * notice gate, class priors) need what only capture-core knows; when the bundle carries any of them,
 * `needsCoreMode()` is true and the caller asks capture-core with `mode_query`. Every other input
 * can only lower the mode, so the tenant default bounds every request from above.
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
 * The subset of capture-core's policy bundle (device/capture-core/policy/bundle.go) this module
 * reads, in its JSON spelling.
 * @typedef {object} Bundle
 * @property {string} version
 * @property {string} tenant_default_mode
 * @property {Record<string,string>} [tool_modes]         tool fingerprint -> mode
 * @property {Record<string,string>} [population_modes]
 * @property {Record<string,string>} [device_modes]
 * @property {Record<string,string[]>} [class_priors]
 * @property {string} [required_notice_version]
 * @property {{seed_hosts?: string[]}} [interception]
 * @property {object[]} [rules]
 * @property {string[]} [sanctioned_tools]
 * @property {{app_key: string, category: string}[]} [catalog]   the fields the evaluator reads
 */

/**
 * Build the in-memory policy cache. `now` is injected so expiry is testable without a clock.
 */
export function createPolicyCache({ now = () => Date.now(), staleAfterMs = DEFAULT_STALE_AFTER_MS } = {}) {
  /** @type {{bundle: Bundle, loadedAt: number, stale: boolean}|null} */
  let current = null;

  function applyBundle(bundle, { at = now() } = {}) {
    if (!bundle || typeof bundle !== 'object' || Array.isArray(bundle) || typeof bundle.version !== 'string' || bundle.version === '') {
      // An unusable bundle is not "no policy": keep the previous one, marked stale. With none,
      // everything stays at M0.
      current = current ? { ...current, stale: true } : null;
      return { applied: false, reason: 'unusable_bundle', policy_version: current ? current.bundle.version : null };
    }
    current = { bundle, loadedAt: at, stale: false };
    return { applied: true, reason: 'ok', policy_version: bundle.version };
  }

  function usable(at) {
    return current !== null && at - current.loadedAt <= staleAfterMs;
  }

  /** The tenant default, or M0 when it is not a mode. */
  function tenantDefault() {
    const m = current.bundle.tenant_default_mode;
    return isMode(m) ? m : CONSERVATIVE_MODE;
  }

  /**
   * Resolve the mode for a request: the most restrictive of the tenant default and the tool's mode,
   * and any failure to resolve returns M0 rather than a wider mode. `override` is capture-core's
   * answer to a `mode_query`, which is authoritative.
   * @param {{tool_fingerprint?: string}} target
   * @returns {{mode: string, reason: string, policy_version: string|null, unsigned: boolean}}
   */
  function modeFor(target, { at = now(), override } = {}) {
    const version = current ? current.bundle.version : null;
    if (override !== undefined && override !== null) {
      return {
        mode: isMode(override) ? override : CONSERVATIVE_MODE,
        reason: isMode(override) ? 'core_answer' : 'core_answer_unrecognised',
        policy_version: version,
        unsigned: false,
      };
    }
    if (!current) return { mode: CONSERVATIVE_MODE, reason: 'no_bundle', policy_version: null, unsigned: true };
    if (!usable(at)) return { mode: CONSERVATIVE_MODE, reason: 'bundle_stale', policy_version: version, unsigned: true };

    let mode = tenantDefault();
    let reason = 'tenant_default';
    const tool = target && target.tool_fingerprint;
    const toolModes = current.bundle.tool_modes;
    if (tool && toolModes && typeof toolModes === 'object' && Object.hasOwn(toolModes, tool)) {
      // A tool entry that is not a mode resolves downward.
      const m = isMode(toolModes[tool]) ? toolModes[tool] : CONSERVATIVE_MODE;
      if (order(m) < order(mode)) {
        mode = m;
        reason = `tool:${tool}`;
      }
    }
    return { mode, reason, policy_version: version, unsigned: false };
  }

  /**
   * Whether the bundle carries a mode input only capture-core can resolve: population or device
   * scopes, the notice gate, or class priors.
   */
  function needsCoreMode() {
    if (!current) return false;
    const b = current.bundle;
    return (
      nonEmptyObject(b.population_modes) ||
      nonEmptyObject(b.device_modes) ||
      nonEmptyObject(b.class_priors) ||
      (typeof b.required_notice_version === 'string' && b.required_notice_version !== '')
    );
  }

  /**
   * Whether any request may have its body read: the tenant default, the upper bound every request
   * shares, reads content.
   */
  function readsAnyContent() {
    return modeReadsContent(modeFor({}).mode);
  }

  /**
   * The gate the body-bearing listener is registered behind. The tool is not known before the body
   * is read, so this is the bound every request shares.
   */
  function isBodyBearing(url) {
    try {
      new URL(url);
    } catch {
      return false;
    }
    return readsAnyContent();
  }

  function snapshot() {
    if (!current) return { policy_version: null, stale: false, present: false };
    return { policy_version: current.bundle.version, stale: current.stale, present: true };
  }

  function clear() {
    current = null;
  }

  /** The rules, the sanctioned tools and the app catalog, as the evaluator reads them; never authoritative. */
  function rules() {
    if (!current) return { rules: [], sanctioned_tools: [], catalog: [] };
    const b = current.bundle;
    return {
      rules: Array.isArray(b.rules) ? b.rules : [],
      sanctioned_tools: Array.isArray(b.sanctioned_tools) ? b.sanctioned_tools : [],
      catalog: Array.isArray(b.catalog) ? b.catalog : [],
    };
  }

  /** The bundle's destination sets: evidence for the predicate, never the decision. */
  function discoverySets() {
    const seed = current && current.bundle.interception && current.bundle.interception.seed_hosts;
    return Array.isArray(seed) ? { seed } : {};
  }

  return { applyBundle, modeFor, needsCoreMode, readsAnyContent, isBodyBearing, snapshot, clear, rules, discoverySets };
}

function isMode(m) {
  return m === MODE.M0 || m === MODE.M1 || m === MODE.M2 || m === MODE.M3;
}

function order(mode) {
  return { m0: 0, m1: 1, m2: 2, m3: 3 }[mode];
}

function nonEmptyObject(v) {
  return Boolean(v) && typeof v === 'object' && Object.keys(v).length > 0;
}
