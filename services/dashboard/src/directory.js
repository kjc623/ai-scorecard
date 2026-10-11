// directory.js — Settings → Identity Providers: the state of the page and every action on it.
//
// Where an admin decides how Sundial learns who people are: whether people and groups are read from
// Microsoft Entra ID, beside what an identity provider pushes over SCIM. Teams are made from these
// people and groups on the Teams page (team-editor.js). Every read and write goes through the admin
// api, which control-api answers for an admin only and audits.
//
// DOM-free, like deployment.js: it holds the state, makes the calls and tells a listener when the
// state changed, so a test can drive the page without a browser.

/** What a failed directory read means, for the codes graphsync records. */
export const SYNC_ERRORS = Object.freeze({
  entra_not_connected: 'This tenant has no active Microsoft Entra connection, so there is no directory to read.',
  graph_consent_missing: 'Microsoft Graph refused the read. An Entra administrator must grant the Sundial application the User.Read.All and GroupMember.Read.All permissions.',
  graph_token_refused: 'Microsoft Entra would not issue a token for this tenant. The application may have been removed from it, or its consent withdrawn.',
  graph_unavailable: 'Microsoft Graph could not be reached or failed. The next pass tries again.',
  people_not_synced: 'Some people could not be written. The rest were; the next pass tries again.',
  internal: 'The read failed inside Sundial. The next pass tries again.',
});

const IDLE_SYNC = Object.freeze({ pending: null, problem: null, queued: null });

/**
 * The page's behaviour over one admin api.
 *
 * @param {object} input
 * @param {ReturnType<import('./transport.js').createAdminApi>} input.admin
 * @param {(state: object) => void} [input.onChange]
 */
export function createDirectory({ admin, onChange = () => {} }) {
  let state = Object.freeze({ status: 'idle', data: null, problem: null, refreshProblem: null, sync: IDLE_SYNC });
  let loadSeq = 0;

  function set(patch) {
    state = Object.freeze({ ...state, ...patch });
    onChange(state);
  }

  async function load({ quiet = false } = {}) {
    const seq = ++loadSeq;
    const keep = quiet && state.data !== null;
    if (!keep) set({ status: 'loading', problem: null, refreshProblem: null });
    const answer = await admin.directory();
    if (seq !== loadSeq) return state;
    if (answer.state === 'available') set({ status: 'ready', data: answer.data, problem: null, refreshProblem: null });
    else if (keep) set({ refreshProblem: answer.error });
    else set({ status: 'refused', problem: Object.freeze({ ...answer.error, status: answer.status }), data: null });
    return state;
  }

  async function setSync(enabled) {
    if (state.status !== 'ready' || state.sync.pending) return state;
    set({ sync: Object.freeze({ pending: enabled ? 'on' : 'off', problem: null, queued: null }) });
    const answer = await admin.setDirectorySync(enabled);
    set({ sync: Object.freeze({ pending: null, problem: answer.state === 'done' ? null : answer.error, queued: null }) });
    return answer.state === 'done' ? load({ quiet: true }) : state;
  }

  async function runSync() {
    if (state.status !== 'ready' || state.sync.pending) return state;
    set({ sync: Object.freeze({ pending: 'run', problem: null, queued: null }) });
    const answer = await admin.runDirectorySync();
    set({ sync: Object.freeze({ pending: null, problem: answer.state === 'done' ? null : answer.error, queued: answer.state === 'done' ? answer.queued : null }) });
    return answer.state === 'done' ? load({ quiet: true }) : state;
  }

  /** One entry point for the page's controls: `data-dir` names the action. */
  function act(dataset = {}) {
    switch (dataset.dir) {
      case 'retry': return load();
      case 'sync-on': return setSync(true);
      case 'sync-off': return setSync(false);
      case 'sync-run': return runSync();
      default: return Promise.resolve(state);
    }
  }

  return Object.freeze({
    get state() { return state; },
    load, setSync, runSync, act,
  });
}
