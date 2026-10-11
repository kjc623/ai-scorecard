/**
 * tool-fingerprint.js — the tool a request went to, named from its destination host by the app
 * catalog in the policy bundle:
 *
 *   app:<app_key>  an app's inference domain covers the host: an exact host, or with a leading dot
 *                  the domain and every host under it, the rule capture-core matches domains by;
 *   host:<host>    no app claims the host, which then names itself.
 *
 * Only an app the catalog knows by its domains alone names a browser request. A desktop app or a
 * command-line tool can share a domain with a web service (ChatGPT's desktop app and chatgpt.com),
 * and a request from the browser is never that installed program. Of the apps whose domains cover
 * the host, an exact host beats a domain and a longer domain beats a shorter one; the catalog's
 * order breaks a tie.
 *
 * The path, the method and the body never name the tool: they vary between prompts to one tool and
 * belong to the predicate, which decides whether a request is a submission.
 */

import { registeredDomain } from './predicate.js';

/** The catalog signal kind a host is matched against. */
export const INFERENCE_DOMAIN = 'inference_domain';

/** The longest tool fingerprint the event envelope accepts. */
const MAX_FINGERPRINT = 128;

/**
 * @param {Array<{app_key?: string, signals?: Array<{kind?: string, value?: string}>}>} catalog  the bundle's
 * @param {string} url  the request's URL
 * @returns {string} `app:<app_key>`, or `host:<host>` when no app claims the host
 */
export function toolFingerprint(catalog, url) {
  const host = destinationHost(url);
  const app = appForHost(catalog, host);
  if (app) return `app:${app}`;
  const own = `host:${host}`;
  return own.length <= MAX_FINGERPRINT ? own : `host:${registeredDomain(host)}`;
}

/** The request's host, lower-case, without a port or a trailing dot. */
export function destinationHost(url) {
  try {
    return new URL(url).hostname.toLowerCase().replace(/\.$/, '');
  } catch {
    return '';
  }
}

/**
 * The key of the app the host belongs to, or null.
 * @returns {string|null}
 */
export function appForHost(catalog, host) {
  let best = null;
  let bestRank = -1;
  for (const app of Array.isArray(catalog) ? catalog : []) {
    const signals = app && Array.isArray(app.signals) ? app.signals : [];
    if (!app || typeof app.app_key !== 'string' || signals.length === 0) continue;
    if (!signals.every((s) => s && s.kind === INFERENCE_DOMAIN)) continue;
    for (const s of signals) {
      const rank = matchRank(host, s.value);
      if (rank > bestRank) {
        best = app.app_key;
        bestRank = rank;
      }
    }
  }
  return best;
}

/** How closely a domain pattern covers a host: -1 when it does not, and higher is closer. */
function matchRank(host, pattern) {
  const p = String(pattern ?? '').trim().toLowerCase().replace(/^\*/, '');
  if (!host || !p) return -1;
  if (p.startsWith('.')) return host === p.slice(1) || host.endsWith(p) ? p.length : -1;
  return host === p ? Number.MAX_SAFE_INTEGER : -1;
}
