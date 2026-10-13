/**
 * self-update.js — installs a new release of the extension as soon as its update server lists one.
 *
 * The browser installs the extension, and every update of it, from the update manifest its
 * `update_url` names, but reads that manifest only every few hours. The extension reads it too, on
 * start and every few minutes, and when it lists a newer version asks the browser to fetch it now
 * (`runtime.requestUpdateCheck`). capture-core names its own release in each `policy_bundle`
 * answer, so a newer agent starts the same read at once. The browser installs a downloaded update
 * only once the service worker stops, which the open native port prevents, so a downloaded update
 * is applied by reloading, after the queue has been handed to capture-core.
 *
 * The browser refuses update checks for hours after one that does not end in an installed update,
 * so it is asked only when the manifest lists a newer version, and again on each read until it has
 * the update.
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
 * The version a gupdate update manifest lists for extension `id`, or null. Read with patterns: a
 * service worker has no DOMParser, and the manifest is a few attributes on two elements.
 */
export function listedVersion(xml, id) {
  for (const app of String(xml).split(/<app\b/).slice(1)) {
    const appid = /^[^>]*\bappid\s*=\s*["']([^"']*)["']/.exec(app);
    if (!appid || appid[1] !== id) continue;
    const check = /<updatecheck\b[^>]*\bversion\s*=\s*["']([^"']*)["']/.exec(app);
    return check ? check[1] : null;
  }
  return null;
}

/**
 * @param {object} opts
 * @param {import('./adapter.js').Adapter} opts.adapter
 * @param {string} opts.version            this extension's version
 * @param {() => Promise<unknown>} opts.beforeReload  hands what is held in memory to capture-core
 */
export function createSelfUpdate({ adapter, version, beforeReload }) {
  let lastStatus = null;
  let reading = null;
  let reloading = false;

  /** Read the update manifest and, when it lists a newer version, ask the browser for it. */
  function check() {
    // One read at a time: start, the alarm and a policy sync can coincide.
    if (!reading) reading = read().finally(() => (reading = null));
    return reading;
  }

  async function read() {
    const url = adapter.runtime.updateUrl();
    // A build loaded unpacked has no update_url, and a reload under way has its update already.
    if (!url || reloading) return lastStatus;
    let listed;
    try {
      listed = listedVersion(await adapter.net.getText(url), adapter.runtime.id());
    } catch {
      return (lastStatus = 'unreachable');
    }
    if (!versionNewer(listed, version)) return (lastStatus = 'current');
    try {
      lastStatus = await adapter.runtime.requestUpdateCheck();
    } catch {
      lastStatus = 'error';
    }
    return lastStatus;
  }

  /** Called with the agent version from each `policy_bundle` answer. */
  function consider(agentVersion) {
    return versionNewer(agentVersion, version) ? check() : Promise.resolve(lastStatus);
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
    check,
    consider,
    install,
    applyUpdate,
    get lastStatus() {
      return lastStatus;
    },
  };
}
