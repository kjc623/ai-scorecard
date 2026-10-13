// integrations.js — Settings → Integrations: the state of the page and every action on it.
//
// Where an admin connects the AI providers whose admin APIs Sundial reads: each provider's own
// record of the prompts the tenant's people sent, with no device in between. The page lists
// whatever the admin api lists, so a provider added to the catalogue appears here as it is. Every
// read and write goes through the admin api, which control-api answers for an admin only and audits.
//
// DOM-free, like directory.js: it holds the state, makes the calls and tells a listener when the
// state changed, so a test can drive the page without a browser.
//
// A key is held only between the admin typing it and the one request that sends it. The key field
// is never rendered with a value, so every repaint empties it, and the held key is emptied with it.

/** What a failed pull means, for the codes the provider runner records. */
export const PULL_ERRORS = Object.freeze({
  not_available: 'This deployment cannot read this provider.',
  entra_not_connected: 'This provider is read through your organisation\'s Microsoft Entra connection, and there is no active one.',
  consent_missing: 'Microsoft refused the read. An Entra administrator must grant the Sundial application the AiEnterpriseInteraction.Read.All application permission, and each person needs a Microsoft 365 Copilot licence.',
  credential_refused: 'The provider refused the key: it has expired, been revoked, or lacks the permission to read compliance records and conversation messages. Replace the key.',
  rate_limited: 'The provider asked Sundial to slow down. The next pull resumes where this one stopped.',
  provider_unavailable: 'The provider\'s API could not be reached or answered with an error. The next pull tries again.',
  internal: 'Sundial could not record what it read. The next pull tries again.',
});

/** Is a provider connected now? A disconnected connection is kept, disabled, with its history. */
export function isConnected(provider) {
  return provider?.connection?.status === 'active';
}

/**
 * What a provider's card offers: the key form (an account and a key), one button (read with the
 * tenant's Entra consent), or nothing (not available here).
 */
export function connectKind(provider) {
  if (!provider?.available) return null;
  return provider.credential === 'entra_app' ? 'consent' : 'key';
}

const IDLE = Object.freeze({ provider: null, form: false, confirm: false, pending: null, problem: null, done: null, queued: null });
const NO_DRAFTS = Object.freeze({ account: '', key: '' });
const DRAFT_LIMITS = Object.freeze({ account: 200, key: 4096 });

/**
 * The page's behaviour over one admin api.
 *
 * @param {object} input
 * @param {ReturnType<import('./transport.js').createAdminApi>} input.admin
 * @param {(state: object) => void} [input.onChange]
 */
export function createIntegrations({ admin, onChange = () => {} }) {
  let state = Object.freeze({ status: 'idle', data: null, problem: null, refreshProblem: null, action: IDLE, drafts: NO_DRAFTS });
  let loadSeq = 0;

  // Every change repaints the page, which empties the key field; the held key goes with it.
  function set(patch) {
    const drafts = Object.freeze({ ...(patch.drafts ?? state.drafts), key: '' });
    state = Object.freeze({ ...state, ...patch, drafts });
    onChange(state);
  }

  const find = (id) => state.data?.providers.find((p) => p.provider === id) ?? null;

  async function load({ quiet = false } = {}) {
    const seq = ++loadSeq;
    const keep = quiet && state.data !== null;
    if (!keep) set({ status: 'loading', problem: null, refreshProblem: null });
    const answer = await admin.providers();
    if (seq !== loadSeq) return state;
    if (answer.state === 'available') set({ status: 'ready', data: answer.data, problem: null, refreshProblem: null });
    else if (keep) set({ refreshProblem: answer.error });
    else set({ status: 'refused', problem: Object.freeze({ ...answer.error, status: answer.status }), data: null });
    return state;
  }

  /** Open the key form, with the account the connection reads already in it. */
  function openForm(id) {
    const provider = find(id);
    if (state.action.pending || connectKind(provider) !== 'key') return state;
    set({ action: Object.freeze({ ...IDLE, provider: id, form: true }), drafts: Object.freeze({ account: provider.connection?.account ?? '', key: '' }) });
    return state;
  }

  function cancel() {
    if (state.action.pending) return state;
    set({ action: IDLE, drafts: NO_DRAFTS });
    return state;
  }

  async function connect(id) {
    const provider = find(id);
    const kind = connectKind(provider);
    if (state.status !== 'ready' || state.action.pending || !kind) return state;
    if (kind === 'consent' && !state.data.entra_connected) return state;
    const replacing = isConnected(provider);
    let body = {};
    if (kind === 'key') {
      const account = state.drafts.account.trim();
      const key = state.drafts.key.trim();
      const missing = key === '' ? `Paste the ${provider.credential_label ?? 'key'}.`
        : provider.account_label && account === '' ? `Enter the ${provider.account_label}.` : null;
      if (missing) {
        set({ action: Object.freeze({ ...IDLE, provider: id, form: true, problem: Object.freeze({ code: 'required', message: missing }) }) });
        return state;
      }
      body = provider.account_label ? { account, key } : { key };
    }
    set({ action: Object.freeze({ ...IDLE, provider: id, form: kind === 'key', pending: 'connect' }) });
    const answer = await admin.connectProvider(id, body);
    if (answer.state !== 'done') {
      set({ action: Object.freeze({ ...IDLE, provider: id, form: kind === 'key', problem: answer.error }) });
      return state;
    }
    set({ action: Object.freeze({ ...IDLE, provider: id, done: replacing ? 'replace' : 'connect', queued: answer.queued }), drafts: NO_DRAFTS });
    return load({ quiet: true });
  }

  function askDisconnect(id) {
    if (state.action.pending || !isConnected(find(id))) return state;
    set({ action: Object.freeze({ ...IDLE, provider: id, confirm: true }), drafts: NO_DRAFTS });
    return state;
  }

  async function disconnect(id) {
    if (state.status !== 'ready' || state.action.pending || !isConnected(find(id))) return state;
    set({ action: Object.freeze({ ...IDLE, provider: id, pending: 'disconnect' }) });
    const answer = await admin.disconnectProvider(id);
    set({ action: Object.freeze({ ...IDLE, provider: id, problem: answer.state === 'done' ? null : answer.error, done: answer.state === 'done' ? 'disconnect' : null }) });
    return answer.state === 'done' ? load({ quiet: true }) : state;
  }

  async function run(id) {
    if (state.status !== 'ready' || state.action.pending || !isConnected(find(id))) return state;
    set({ action: Object.freeze({ ...IDLE, provider: id, pending: 'run' }), drafts: NO_DRAFTS });
    const answer = await admin.runProvider(id);
    const ran = answer.state === 'done';
    set({ action: Object.freeze({ ...IDLE, provider: id, problem: ran ? null : answer.error, done: ran ? 'run' : null, queued: ran ? answer.queued : null }) });
    return ran ? load({ quiet: true }) : state;
  }

  /** What is being typed into the key form. Kept without a repaint, so typing never loses focus. */
  function setDraft(name, value) {
    if (!(name in DRAFT_LIMITS)) return;
    state = Object.freeze({ ...state, drafts: Object.freeze({ ...state.drafts, [name]: String(value ?? '').slice(0, DRAFT_LIMITS[name]) }) });
  }

  /** Leaving the page: a key being typed and any half-made choice go with it. */
  function leave() {
    state = Object.freeze({ ...state, action: state.action.pending ? state.action : IDLE, drafts: NO_DRAFTS });
  }

  /** One entry point for the page's controls: `data-int` names the action, `data-provider` its provider. */
  function act(dataset = {}) {
    switch (dataset.int) {
      case 'retry': return load();
      case 'open': return Promise.resolve(openForm(dataset.provider));
      case 'cancel': return Promise.resolve(cancel());
      case 'connect': return connect(dataset.provider);
      case 'disconnect': return Promise.resolve(askDisconnect(dataset.provider));
      case 'confirm-disconnect': return disconnect(dataset.provider);
      case 'run': return run(dataset.provider);
      default: return Promise.resolve(state);
    }
  }

  return Object.freeze({
    get state() { return state; },
    load, openForm, cancel, connect, askDisconnect, disconnect, run, setDraft, leave, act,
  });
}
