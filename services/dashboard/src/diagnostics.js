// diagnostics.js — a device's diagnostics on request: their state and every action on them.
//
// An admin asks a device for its diagnostics on the device's page. The device learns of the request
// at its next health report and sends a zip of its health snapshot, configuration summary, service
// log and update logs, which the admin downloads here. Every call goes through the admin api
// (transport.js), which control-api answers for an admin only and audits with the real actor.
//
// DOM-free like the other controllers: it holds the state, makes the calls and tells a listener
// when the state changed. Saving a file is handed in by boot().

const IDLE = Object.freeze({
  request: Object.freeze({ pending: false, problem: null, last: null }),
  download: Object.freeze({ pending: null, problem: null, last: null }),
});

/**
 * @param {object} input
 * @param {ReturnType<import('./transport.js').createAdminApi>} input.admin
 * @param {(state: object) => void} [input.onChange]
 * @param {(data: any, filename: string) => void} [input.save]   hands a downloaded bundle to the browser
 */
export function createDiagnostics({ admin, onChange = () => {}, save = () => {} }) {
  let state = Object.freeze({ device: null, status: 'idle', data: null, problem: null, refreshProblem: null, ...IDLE });
  let loadSeq = 0;

  function set(patch) {
    state = Object.freeze({ ...state, ...patch });
    onChange(state);
  }

  /**
   * Read one device's requests. Opening another device starts from the loading state; a read of
   * the same device keeps what is on screen and only says so if it failed.
   */
  async function load(device, { quiet = false } = {}) {
    const seq = ++loadSeq;
    const same = device === state.device;
    const keep = quiet && same && state.data !== null;
    if (!same) set({ device, status: 'loading', data: null, problem: null, refreshProblem: null, ...IDLE });
    else if (!keep) set({ status: 'loading', problem: null, refreshProblem: null });
    const answer = await admin.diagnostics(device);
    if (seq !== loadSeq) return state;
    if (answer.state === 'available') {
      set({ status: 'ready', data: answer.data, problem: null, refreshProblem: null });
    } else if (keep) {
      set({ refreshProblem: answer.error });
    } else {
      set({ status: 'refused', data: null, problem: Object.freeze({ ...answer.error, status: answer.status }) });
    }
    return state;
  }

  /** Ask the device for its diagnostics. A request already waiting is the answer, not a second one. */
  async function request() {
    if (!state.device || state.status !== 'ready' || state.request.pending) return state;
    const device = state.device;
    set({ request: Object.freeze({ pending: true, problem: null, last: null }) });
    const answer = await admin.requestDiagnostics(device);
    if (device !== state.device) return state;
    if (answer.state !== 'done') {
      set({ request: Object.freeze({ pending: false, problem: answer.error, last: null }) });
      return state;
    }
    set({ request: Object.freeze({ pending: false, problem: null, last: Object.freeze({ created: answer.created }) }) });
    return load(device, { quiet: true });
  }

  async function download(requestId) {
    if (!state.device || state.download.pending) return state;
    const device = state.device;
    set({ download: Object.freeze({ pending: requestId, problem: null, last: null }) });
    const answer = await admin.downloadDiagnostics(device, requestId);
    if (answer.state !== 'available') {
      set({ download: Object.freeze({ pending: null, problem: answer.error, last: null }) });
      return state;
    }
    const filename = answer.file.filename || `diagnostics-${String(device).slice(0, 8)}.zip`;
    try {
      save(answer.file.data, filename);
    } catch (error) {
      set({ download: Object.freeze({ pending: null, problem: Object.freeze({ code: 'save_failed', message: `The browser did not save it: ${error?.message ?? error}` }), last: null }) });
      return state;
    }
    set({ download: Object.freeze({ pending: null, problem: null, last: Object.freeze({ requestId, filename }) }) });
    return state;
  }

  /** One entry point for the section's buttons: `data-diag` names the action, `data-id` its request. */
  function act(dataset = {}) {
    switch (dataset.diag) {
      case 'retry':
      case 'refresh': return state.device ? load(state.device, { quiet: dataset.diag === 'refresh' }) : Promise.resolve(state);
      case 'request': return request();
      case 'download': return download(dataset.id);
      default: return Promise.resolve(state);
    }
  }

  return Object.freeze({
    get state() { return state; },
    load, request, download, act,
  });
}
