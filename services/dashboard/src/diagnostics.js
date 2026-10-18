// diagnostics.js — a device's diagnostics on request: their state and every action on them.
//
// An admin asks a device for its diagnostics on the device's page. The device learns of the request
// at its next health report and sends a zip of its health snapshot, configuration summary, service
// log and update logs, which the admin downloads here. Every call goes through the admin api
// (transport.js), which control-api answers for an admin only and audits with the real actor.
//
// DOM-free like the other controllers: it holds the state, makes the calls and tells a listener
// when the state changed. Saving a file is handed in by boot(). While a request waits for the
// device, its requests are read again every half minute until the bundle arrives or the page is
// left, so the page follows the request without being refreshed.

const IDLE = Object.freeze({
  request: Object.freeze({ pending: false, problem: null }),
  download: Object.freeze({ pending: null, problem: null }),
});

/** How often a waiting request is read again, in milliseconds. */
export const WATCH_EVERY = 30_000;

/**
 * @param {object} input
 * @param {ReturnType<import('./transport.js').createAdminApi>} input.admin
 * @param {(state: object) => void} [input.onChange]
 * @param {(data: any, filename: string) => void} [input.save]   hands a downloaded bundle to the browser
 * @param {{ setTimeout: Function, clearTimeout: Function }} [input.timers]   the clock a waiting request is read again by
 */
export function createDiagnostics({ admin, onChange = () => {}, save = () => {}, timers = globalThis }) {
  let state = Object.freeze({ device: null, status: 'idle', data: null, problem: null, refreshProblem: null, ...IDLE });
  let loadSeq = 0;
  let timer = null;

  function set(patch) {
    state = Object.freeze({ ...state, ...patch });
    onChange(state);
  }

  function unwatch() {
    if (timer !== null) timers.clearTimeout(timer);
    timer = null;
  }

  /** Read the device's requests again later while one of them waits for the device. */
  function watch() {
    unwatch();
    if (state.status !== 'ready' || !state.data?.some((d) => d.state === 'waiting')) return;
    const device = state.device;
    timer = timers.setTimeout(() => {
      timer = null;
      return device === state.device ? load(device, { quiet: true }) : state;
    }, WATCH_EVERY);
  }

  /**
   * Read one device's requests. Opening another device starts from the loading state; a read of
   * the same device keeps what is on screen, repaints only what changed, and says so if it failed.
   */
  async function load(device, { quiet = false } = {}) {
    unwatch();
    const seq = ++loadSeq;
    const same = device === state.device;
    const keep = quiet && same && state.data !== null;
    if (!same) set({ device, status: 'loading', data: null, problem: null, refreshProblem: null, ...IDLE });
    else if (!keep) set({ status: 'loading', problem: null, refreshProblem: null });
    const answer = await admin.diagnostics(device);
    if (seq !== loadSeq) return state;
    if (answer.state === 'available') {
      const unchanged = keep && state.status === 'ready' && !state.refreshProblem && JSON.stringify(answer.data) === JSON.stringify(state.data);
      if (!unchanged) set({ status: 'ready', data: answer.data, problem: null, refreshProblem: null });
    } else if (keep) {
      set({ refreshProblem: answer.error });
    } else {
      set({ status: 'refused', data: null, problem: Object.freeze({ ...answer.error, status: answer.status }) });
    }
    watch();
    return state;
  }

  /** The device's page was left: a waiting request is no longer read again, and a read in flight is dropped. */
  function leave() {
    unwatch();
    loadSeq += 1;
  }

  /** Ask the device for its diagnostics. A request already waiting is the answer, not a second one. */
  async function request() {
    if (!state.device || state.status !== 'ready' || state.request.pending) return state;
    const device = state.device;
    set({ request: Object.freeze({ pending: true, problem: null }) });
    const answer = await admin.requestDiagnostics(device);
    if (device !== state.device) return state;
    if (answer.state !== 'done') {
      set({ request: Object.freeze({ pending: false, problem: answer.error }) });
      return state;
    }
    // Busy until the request is listed, so the button never returns before the request's row.
    await load(device, { quiet: true });
    if (device === state.device) set({ request: IDLE.request });
    return state;
  }

  async function download(requestId) {
    if (!state.device || state.download.pending) return state;
    const device = state.device;
    set({ download: Object.freeze({ pending: requestId, problem: null }) });
    const answer = await admin.downloadDiagnostics(device, requestId);
    if (answer.state !== 'available') {
      set({ download: Object.freeze({ pending: null, problem: answer.error }) });
      return state;
    }
    const filename = answer.file.filename || `diagnostics-${String(device).slice(0, 8)}.zip`;
    try {
      save(answer.file.data, filename);
    } catch (error) {
      set({ download: Object.freeze({ pending: null, problem: Object.freeze({ code: 'save_failed', message: `The browser did not save it: ${error?.message ?? error}` }) }) });
      return state;
    }
    set({ download: Object.freeze({ pending: null, problem: null }) });
    return state;
  }

  /** One entry point for the section's buttons: `data-diag` names the action, `data-id` its request. */
  function act(dataset = {}) {
    switch (dataset.diag) {
      case 'retry': return state.device ? load(state.device) : Promise.resolve(state);
      case 'request': return request();
      case 'download': return download(dataset.id);
      default: return Promise.resolve(state);
    }
  }

  return Object.freeze({
    get state() { return state; },
    load, leave, request, download, act,
  });
}
