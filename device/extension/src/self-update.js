/**
 * self-update.js — installs a new release of the extension as soon as the agent has one.
 *
 * The agent and the extension are built from one release under one version, and the agent updates
 * itself within minutes; the browser's own update check runs only every few hours. capture-core
 * names its version in every `policy_bundle` answer, so when it is newer than this extension the
 * browser is asked to check the `update_url` now. The browser installs a downloaded update only
 * once the service worker stops, which the open native port prevents, so a downloaded update is
 * applied by reloading, after the queue has been handed to capture-core.
 */

/** Parse a dotted numeric version; null for anything else (a development build). */
export function parseVersion(v) {
  if (typeof v !== 'string' || !/^\d{1,9}(\.\d{1,9}){0,3}$/.test(v.trim())) return null;
  return v.trim().split('.').map(Number);
}

/** Whether `candidate` is a later version than `current`; false when either is not a release version. */
export function versionNewer(candidate, current) {
  const a = parseVersion(candidate);
  const b = parseVersion(current);
  if (!a || !b) return false;
  for (let i = 0; i < Math.max(a.length, b.length); i++) {
    const x = a[i] || 0;
    const y = b[i] || 0;
    if (x !== y) return x > y;
  }
  return false;
}

/**
 * @param {object} opts
 * @param {import('./adapter.js').Adapter} opts.adapter
 * @param {string} opts.version            this extension's version
 * @param {() => Promise<unknown>} opts.beforeReload  hands what is held in memory to capture-core
 */
export function createSelfUpdate({ adapter, version, beforeReload }) {
  /** The agent version a check was last asked for, so each release is asked for once per worker. */
  let askedFor = null;
  let lastStatus = null;
  let reloading = false;

  /** Called with the agent version from each `policy_bundle` answer. */
  async function consider(agentVersion) {
    if (!versionNewer(agentVersion, version) || askedFor === agentVersion) return lastStatus;
    askedFor = agentVersion;
    try {
      lastStatus = await adapter.runtime.requestUpdateCheck();
    } catch {
      lastStatus = 'error';
    }
    return lastStatus;
  }

  async function applyUpdate() {
    if (reloading) return;
    reloading = true;
    try {
      await beforeReload();
    } catch {
      /* the reload goes ahead: an update must not wait on a channel that may not come back */
    }
    adapter.runtime.reload();
  }

  function install() {
    adapter.runtime.onUpdateAvailable(() => void applyUpdate());
  }

  return {
    consider,
    install,
    applyUpdate,
    get lastStatus() {
      return lastStatus;
    },
  };
}
