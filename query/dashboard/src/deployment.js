// deployment.js — Settings → Deployment: the state of the page and every action on it.
//
// The page an admin uses to get the capture agent onto their devices: which identity
// provider is linked, whether a device must also be managed by Intune, a tenant package to download
// (which mints a deployment key), the keys minted so far, and the SCIM endpoint and tokens that keep
// people in sync. Every read and write goes through the admin api (transport.js), which control-api
// answers for an admin only and audits with the real actor.
//
// Split the way the Explore page is: `createDeployment` is DOM-free — it holds the state, makes the
// calls, and tells a listener when the state changed — so a test can drive the page without a
// browser. Saving a file and writing to the clipboard are handed in by boot(), the only DOM code.
//
// The SCIM token is the one secret this page ever holds. It lives in this controller's memory from
// the moment control-api returns it until the admin dismisses it or leaves the page; it is never
// stored, never re-read (the server keeps only its hash), and never rendered again.

/** The two package formats, as the admin API names them, and the file name used if the server sends none. */
export const DEPLOYMENT_FORMATS = Object.freeze({
  intunewin: Object.freeze({ id: 'intunewin', label: 'Intune package (.intunewin)', fallbackName: 'ShadowAICapture.intunewin' }),
  zip: Object.freeze({ id: 'zip', label: 'Package (.zip)', fallbackName: 'ShadowAICapture.zip' }),
});

const DEP_IDLE = Object.freeze({
  verification: Object.freeze({ pending: null, problem: null }),
  download: Object.freeze({ pending: null, problem: null, last: null }),
  keys: Object.freeze({ confirm: null, pending: null, problem: null }),
  scim: Object.freeze({ pending: false, problem: null, created: null }),
  tokens: Object.freeze({ confirm: null, pending: null, problem: null }),
  copied: null,
});

/** Intune verification is offered only when an Entra connection is active: the check runs in that tenant. */
export function verificationAvailable(data) {
  return data?.connection?.provider === 'entra' && data.connection.status === 'active';
}

/** A key's state, from its timestamps. Revoked is not expired, and neither is merged into the other. */
export function deploymentKeyState(key, now = new Date()) {
  if (key.revoked_at) return 'revoked';
  if (key.expires_at && Date.parse(key.expires_at) <= now.getTime()) return 'expired';
  return 'active';
}

/**
 * The page's behaviour over one admin api.
 *
 * @param {object} input
 * @param {ReturnType<import('./transport.js').createAdminApi>} input.admin
 * @param {(state: object) => void} [input.onChange]
 * @param {(data: any, filename: string) => void} [input.save]   hands a downloaded package to the browser
 * @param {(text: string) => Promise<void>} [input.copy]         writes to the clipboard
 * @param {() => Date} [input.now]
 */
export function createDeployment({ admin, onChange = () => {}, save = () => {}, copy = null, now = () => new Date() }) {
  let state = Object.freeze({
    status: 'idle', data: null, problem: null, refreshProblem: null,
    ...DEP_IDLE,
    drafts: Object.freeze({ packageLabel: '', scimLabel: '' }),
  });
  let loadSeq = 0;

  function set(patch) {
    state = Object.freeze({ ...state, ...patch });
    onChange(state);
  }

  /**
   * Read the page. The first read shows the loading state; a read after an action keeps what is on
   * screen and only says so if it failed, so a refresh never blanks the page an admin is using.
   */
  async function load({ quiet = false } = {}) {
    const seq = ++loadSeq;
    const keep = quiet && state.data !== null;
    if (!keep) set({ status: 'loading', problem: null, refreshProblem: null });
    const answer = await admin.deployment();
    if (seq !== loadSeq) return state;
    if (answer.state === 'available') {
      set({ status: 'ready', data: answer.data, problem: null, refreshProblem: null });
    } else if (keep) {
      set({ refreshProblem: answer.error });
    } else {
      set({ status: 'refused', problem: Object.freeze({ ...answer.error, status: answer.status }), data: null });
    }
    return state;
  }

  async function setVerification(value) {
    if (!['none', 'intune'].includes(value) || state.status !== 'ready' || state.verification.pending) return state;
    if (state.data.device_verification === value) return state;
    if (value === 'intune' && !verificationAvailable(state.data)) return state;
    set({ verification: Object.freeze({ pending: value, problem: null }) });
    const answer = await admin.setVerification(value);
    if (answer.state !== 'done') {
      set({ verification: Object.freeze({ pending: null, problem: answer.error }) });
      return state;
    }
    set({ verification: DEP_IDLE.verification });
    return load({ quiet: true });
  }

  /**
   * Download a tenant package. Each download mints a new deployment key inside the package, so the
   * page says which key this one carries: the server's x-sac-deployment-key-* header when it sends
   * one, else the one key that was not in the list before the download.
   */
  async function download(format) {
    const spec = DEPLOYMENT_FORMATS[format];
    if (!spec || state.status !== 'ready' || state.download.pending) return state;
    const label = state.drafts.packageLabel.trim();
    const before = new Set(state.data.deployment_keys.map((k) => k.key_id));
    set({ download: Object.freeze({ pending: format, problem: null, last: null }) });
    const answer = await admin.downloadPackage({ format, label: label || undefined });
    if (answer.state !== 'available') {
      set({ download: Object.freeze({ pending: null, problem: answer.error, last: null }) });
      return state;
    }
    const filename = answer.file.filename || spec.fallbackName;
    let saveProblem = null;
    try {
      save(answer.file.data, filename);
    } catch (error) {
      saveProblem = Object.freeze({ code: 'save_failed', message: `The package was built but the browser did not save it: ${error?.message ?? error}` });
    }
    set({ drafts: Object.freeze({ ...state.drafts, packageLabel: '' }) });
    await load({ quiet: true });
    const keys = state.data?.deployment_keys ?? [];
    let key = answer.file.keyId ? keys.find((k) => k.key_id === answer.file.keyId) ?? null : null;
    if (!key) {
      const fresh = keys.filter((k) => !before.has(k.key_id));
      if (fresh.length === 1) key = fresh[0];
    }
    const keyLabel = answer.file.keyLabel || key?.label || label || null;
    set({
      download: Object.freeze({
        pending: null, problem: saveProblem,
        last: Object.freeze({ format, filename, keyLabel, keyId: key?.key_id ?? answer.file.keyId ?? null }),
      }),
    });
    return state;
  }

  function askRevokeKey(keyId) {
    set({ keys: Object.freeze({ confirm: keyId, pending: null, problem: null }) });
    return state;
  }

  async function revokeKey(keyId) {
    if (state.keys.pending) return state;
    set({ keys: Object.freeze({ confirm: null, pending: keyId, problem: null }) });
    const answer = await admin.revokeKey(keyId);
    if (answer.state !== 'done') {
      set({ keys: Object.freeze({ confirm: null, pending: null, problem: answer.error }) });
      return state;
    }
    set({ keys: DEP_IDLE.keys });
    return load({ quiet: true });
  }

  /** Create a SCIM token. Its value is shown once, here, and nowhere else. */
  async function createScimToken() {
    if (state.status !== 'ready' || state.scim.pending) return state;
    const label = state.drafts.scimLabel.trim();
    if (label === '') {
      set({ scim: Object.freeze({ pending: false, problem: Object.freeze({ code: 'label_required', message: 'Give the token a label, such as the identity provider that will use it.' }), created: state.scim.created }) });
      return state;
    }
    set({ scim: Object.freeze({ pending: true, problem: null, created: null }), copied: null });
    const answer = await admin.createScimToken(label);
    if (answer.state !== 'available') {
      set({ scim: Object.freeze({ pending: false, problem: answer.error, created: null }) });
      return state;
    }
    set({
      scim: Object.freeze({ pending: false, problem: null, created: Object.freeze({ token_id: answer.token_id, token: answer.token, label, base_url: answer.base_url }) }),
      drafts: Object.freeze({ ...state.drafts, scimLabel: '' }),
    });
    return load({ quiet: true });
  }

  /** Put the one-time token away. After this the page cannot show it again. */
  function dismissToken() {
    set({ scim: Object.freeze({ ...state.scim, created: null }), copied: null });
    return state;
  }

  function askRevokeToken(tokenId) {
    set({ tokens: Object.freeze({ confirm: tokenId, pending: null, problem: null }) });
    return state;
  }

  async function revokeToken(tokenId) {
    if (state.tokens.pending) return state;
    set({ tokens: Object.freeze({ confirm: null, pending: tokenId, problem: null }) });
    const answer = await admin.revokeScimToken(tokenId);
    if (answer.state !== 'done') {
      set({ tokens: Object.freeze({ confirm: null, pending: null, problem: answer.error }) });
      return state;
    }
    set({ tokens: DEP_IDLE.tokens });
    return load({ quiet: true });
  }

  /** Copy the one-time token or the SCIM base URL. A browser that refuses says so, and the text stays selectable. */
  async function copyValue(what) {
    const text = what === 'token' ? state.scim.created?.token : what === 'base_url' ? (state.data?.scim.base_url ?? state.scim.created?.base_url) : null;
    if (!text) return state;
    try {
      if (!copy) throw new Error('no clipboard');
      await copy(text);
      set({ copied: Object.freeze({ what, ok: true }) });
    } catch {
      set({ copied: Object.freeze({ what, ok: false }) });
    }
    return state;
  }

  /** What is being typed into a label field. Kept without a repaint, so typing never loses focus. */
  function setDraft(name, value) {
    if (!['packageLabel', 'scimLabel'].includes(name)) return;
    state = Object.freeze({ ...state, drafts: Object.freeze({ ...state.drafts, [name]: String(value ?? '').slice(0, 120) }) });
  }

  /** Leaving the page: the one-time token and any half-made choice go with it. */
  function leave() {
    if (state.scim.created || state.keys.confirm || state.tokens.confirm || state.copied) {
      state = Object.freeze({ ...state, scim: Object.freeze({ ...state.scim, created: null }), keys: DEP_IDLE.keys, tokens: DEP_IDLE.tokens, copied: null });
    }
  }

  /** One entry point for the page's buttons: `data-dep` names the action, the other data-* its argument. */
  function act(dataset = {}) {
    switch (dataset.dep) {
      case 'retry': return load();
      case 'verification': return setVerification(dataset.value);
      case 'download': return download(dataset.format);
      case 'revoke-key': return Promise.resolve(askRevokeKey(dataset.id));
      case 'confirm-revoke-key': return revokeKey(dataset.id);
      case 'cancel-revoke-key': return Promise.resolve(askRevokeKey(null));
      case 'create-token': return createScimToken();
      case 'dismiss-token': return Promise.resolve(dismissToken());
      case 'copy': return copyValue(dataset.what);
      case 'revoke-token': return Promise.resolve(askRevokeToken(dataset.id));
      case 'confirm-revoke-token': return revokeToken(dataset.id);
      case 'cancel-revoke-token': return Promise.resolve(askRevokeToken(null));
      default: return Promise.resolve(state);
    }
  }

  return Object.freeze({
    get state() { return state; },
    load, setVerification, download, askRevokeKey, revokeKey, createScimToken, dismissToken,
    askRevokeToken, revokeToken, copyValue, setDraft, leave, act, now,
  });
}
