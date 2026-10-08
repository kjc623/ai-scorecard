// settings.js — Settings → Settings: the state of the page and every action on it.
//
// The page an admin uses to change what the tenant collects and keeps: the collection mode (within
// the ceiling) with an optional narrower per-tool override, the event and content retention
// periods, the sanction decision per tool, the content search tier, which endpoint collectors
// run on the devices, and whether devices inspect TLS. Every read and write goes
// through the admin api (transport.js), which control-api answers for an admin only and audits with
// the real actor, the old value and the new value.
//
// Split like the Deployment page: `createSettings` is DOM-free, so a test can drive it without a
// browser. A mode increase that starts capturing content is not sent until the admin confirms it:
// the controller holds the choice in `mode.confirm` and only then issues the write.

/** The four collection modes, and which transition counts as "starting to capture content". */
export const COLLECTION_MODES = Object.freeze(['m0', 'm1', 'm2', 'm3']);
export const SEARCH_TIERS = Object.freeze(['disabled', 'attachment_names', 'full_text']);
export const SANCTION_STATES = Object.freeze(['sanctioned', 'unsanctioned', 'unknown']);

/** The endpoint collector switches, in the order the card shows them. */
export const ENDPOINT_COLLECTORS = Object.freeze(['inventory', 'processes', 'flows', 'otel', 'hooks', 'hooks_managed_only']);

/**
 * The tools with native collectors and which of the two each has: Cursor sends no OpenTelemetry,
 * and Codex and Copilot have no hooks yet. A collector a tool lacks is shown, never offered.
 */
export const ENDPOINT_TOOLS = Object.freeze([
  Object.freeze({ key: 'claude_code', label: 'Claude Code', otel: true, hooks: true }),
  Object.freeze({ key: 'codex', label: 'Codex', otel: true, hooks: false }),
  Object.freeze({ key: 'copilot', label: 'Copilot', otel: true, hooks: false }),
  Object.freeze({ key: 'cursor', label: 'Cursor', otel: false, hooks: true }),
]);

/** The rank of each collection mode, mirrored from the database's ops.mode_rank. */
const MODE_RANK = Object.freeze({ m0: 0, m1: 1, m2: 2, m3: 3 });

/**
 * Does raising the mode from `from` to `to` begin capturing content? Reading content begins at M1,
 * and holding/uploading the prompt begins at M3; either is a change worth a confirmation.
 */
export function modeIncreaseNeedsConfirmation(from, to) {
  const a = MODE_RANK[from] ?? -1;
  const b = MODE_RANK[to] ?? -1;
  if (a < 0 || b < 0 || b <= a) return false;
  return a === 0 || to === 'm3';
}

/** May this search tier be chosen under the ceiling? The database refuses the rest. */
export function searchTierAllowed(tier, ceiling) {
  if (tier === 'full_text') return ceiling === 'm3';
  if (tier === 'attachment_names') return ceiling !== 'm0';
  return true;
}

/** The mode the tenant is actually requesting: the explicit value, else the ceiling. */
export function effectiveMode(data) {
  return data?.collection_mode || data?.ceiling_mode || 'm0';
}

const IDLE = Object.freeze({
  mode: Object.freeze({ pending: null, confirm: null, problem: null }),
  override: Object.freeze({ pending: null, problem: null }),
  retention: Object.freeze({ pending: null, problem: null, drafts: Object.freeze({}) }),
  search: Object.freeze({ pending: null, problem: null }),
  sanction: Object.freeze({ pending: null, problem: null }),
  endpoint: Object.freeze({ pending: null, problem: null }),
  tls: Object.freeze({ pending: null, problem: null }),
});

/** A switch button's data-value: 'on' or 'off', anything else is no value. */
const onOff = (value) => (value === 'on' ? true : value === 'off' ? false : null);

const NOT_READ = Object.freeze({ code: 'endpoint_not_reported', message: 'The current endpoint settings were not read; reload the page and try again.' });
const TLS_NOT_READ = Object.freeze({ code: 'tls_inspection_not_reported', message: 'The current TLS inspection setting was not read; reload the page and try again.' });

/**
 * The page's behaviour over one admin api.
 *
 * @param {object} input
 * @param {ReturnType<import('./transport.js').createAdminApi>} input.admin
 * @param {(state: object) => void} [input.onChange]
 */
export function createSettings({ admin, onChange = () => {} }) {
  let state = Object.freeze({ status: 'idle', data: null, problem: null, refreshProblem: null, ...IDLE });
  let loadSeq = 0;

  function set(patch) {
    state = Object.freeze({ ...state, ...patch });
    onChange(state);
  }

  async function load({ quiet = false } = {}) {
    const seq = ++loadSeq;
    const keep = quiet && state.data !== null;
    if (!keep) set({ status: 'loading', problem: null, refreshProblem: null });
    const answer = await admin.settings();
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

  /** Choose a mode. A content-capturing increase asks for confirmation before the write. */
  function chooseMode(value) {
    if (!COLLECTION_MODES.includes(value) || state.status !== 'ready' || state.mode.pending) return state;
    if (value === effectiveMode(state.data)) return state;
    if (modeIncreaseNeedsConfirmation(effectiveMode(state.data), value)) {
      set({ mode: Object.freeze({ pending: null, confirm: value, problem: null }) });
      return state;
    }
    return setCollectionMode(value);
  }

  function cancelMode() {
    set({ mode: Object.freeze({ ...state.mode, confirm: null }) });
    return state;
  }

  async function setCollectionMode(value) {
    if (state.mode.pending) return state;
    set({ mode: Object.freeze({ pending: value, confirm: null, problem: null }) });
    const answer = await admin.setCollectionMode(value);
    if (answer.state !== 'done') {
      set({ mode: Object.freeze({ pending: null, confirm: null, problem: answer.error }) });
      return state;
    }
    set({ mode: IDLE.mode });
    return load({ quiet: true });
  }

  /** Change or clear (value null) one tool's narrower override. */
  async function setScopeOverride(tool, value) {
    if (state.status !== 'ready' || state.override.pending) return state;
    set({ override: Object.freeze({ pending: tool, problem: null }) });
    const answer = await admin.setScopeOverride(tool, value);
    if (answer.state !== 'done') {
      set({ override: Object.freeze({ pending: null, problem: answer.error }) });
      return state;
    }
    set({ override: IDLE.override });
    return load({ quiet: true });
  }

  function setRetentionDraft(appliesTo, value) {
    state = Object.freeze({ ...state, retention: Object.freeze({ ...state.retention, drafts: Object.freeze({ ...state.retention.drafts, [appliesTo]: String(value ?? '').replace(/[^0-9]/g, '').slice(0, 4) }) }) });
  }

  async function saveRetention(appliesTo) {
    if (state.retention.pending) return state;
    const raw = (state.retention.drafts[appliesTo] ?? '').trim();
    const ttlDays = Number(raw);
    if (!Number.isInteger(ttlDays) || ttlDays <= 0) {
      set({ retention: Object.freeze({ ...state.retention, pending: null, problem: Object.freeze({ code: 'invalid_days', message: 'Enter a whole number of days greater than zero.' }) }) });
      return state;
    }
    set({ retention: Object.freeze({ ...state.retention, pending: appliesTo, problem: null }) });
    const answer = await admin.setRetention(appliesTo, ttlDays);
    if (answer.state !== 'done') {
      set({ retention: Object.freeze({ ...state.retention, pending: null, problem: answer.error }) });
      return state;
    }
    set({ retention: Object.freeze({ ...state.retention, pending: null, drafts: Object.freeze({ ...state.retention.drafts, [appliesTo]: '' }) }) });
    return load({ quiet: true });
  }

  async function setContentSearch(tier) {
    if (!SEARCH_TIERS.includes(tier) || state.status !== 'ready' || state.search.pending) return state;
    if (tier === (state.data.content_search ?? 'disabled')) return state;
    set({ search: Object.freeze({ pending: tier, problem: null }) });
    const answer = await admin.setContentSearch(tier);
    if (answer.state !== 'done') {
      set({ search: Object.freeze({ pending: null, problem: answer.error }) });
      return state;
    }
    set({ search: IDLE.search });
    return load({ quiet: true });
  }

  async function setToolSanction(tool, value) {
    if (!SANCTION_STATES.includes(value) || state.status !== 'ready' || state.sanction.pending) return state;
    set({ sanction: Object.freeze({ pending: tool, problem: null }) });
    const answer = await admin.setToolSanction(tool, value);
    if (answer.state !== 'done') {
      set({ sanction: Object.freeze({ pending: null, problem: answer.error }) });
      return state;
    }
    set({ sanction: IDLE.sanction });
    return load({ quiet: true });
  }

  async function writeEndpoint(pending, send) {
    set({ endpoint: Object.freeze({ pending, problem: null }) });
    const answer = await send();
    if (answer.state !== 'done') {
      set({ endpoint: Object.freeze({ pending: null, problem: answer.error }) });
      return state;
    }
    set({ endpoint: IDLE.endpoint });
    return load({ quiet: true });
  }

  /** Switch one endpoint collector. The write carries every switch, the others as they were read. */
  async function setEndpointCollector(name, value) {
    if (!ENDPOINT_COLLECTORS.includes(name) || typeof value !== 'boolean' || state.status !== 'ready' || state.endpoint.pending) return state;
    const current = state.data.endpoint;
    if (!current || ENDPOINT_COLLECTORS.some((c) => typeof current[c] !== 'boolean')) {
      set({ endpoint: Object.freeze({ pending: null, problem: NOT_READ }) });
      return state;
    }
    if (current[name] === value) return state;
    const next = Object.fromEntries(ENDPOINT_COLLECTORS.map((c) => [c, c === name ? value : current[c]]));
    return writeEndpoint(name, () => admin.setEndpointCollectors(next));
  }

  /** Switch one of a tool's native collectors. The write carries both, the other as it was read. */
  async function setEndpointTool(key, collector, value) {
    const tool = ENDPOINT_TOOLS.find((t) => t.key === key);
    if (!tool || !tool[collector] || typeof value !== 'boolean' || state.status !== 'ready' || state.endpoint.pending) return state;
    const current = state.data.endpoint?.tools?.[key];
    if (!current || typeof current.otel !== 'boolean' || typeof current.hooks !== 'boolean') {
      set({ endpoint: Object.freeze({ pending: null, problem: NOT_READ }) });
      return state;
    }
    if (current[collector] === value) return state;
    return writeEndpoint(`${key}.${collector}`, () => admin.setEndpointTool(key, { ...current, [collector]: value }));
  }

  /** Turn TLS inspection on or off. Nothing is sent over a setting that was not read. */
  async function setTLSInspection(value) {
    if (typeof value !== 'boolean' || state.status !== 'ready' || state.tls.pending) return state;
    const current = state.data.tls_inspection;
    if (typeof current !== 'boolean') {
      set({ tls: Object.freeze({ pending: null, problem: TLS_NOT_READ }) });
      return state;
    }
    if (current === value) return state;
    set({ tls: Object.freeze({ pending: value ? 'on' : 'off', problem: null }) });
    const answer = await admin.setTLSInspection(value);
    if (answer.state !== 'done') {
      set({ tls: Object.freeze({ pending: null, problem: answer.error }) });
      return state;
    }
    set({ tls: IDLE.tls });
    return load({ quiet: true });
  }

  /** One entry point for the page's buttons and controls, named by data-action. */
  function act(dataset = {}) {
    switch (dataset.action) {
      case 'retry': return load();
      case 'mode': return Promise.resolve(chooseMode(dataset.value));
      case 'mode-confirm': return setCollectionMode(state.mode.confirm);
      case 'mode-cancel': return Promise.resolve(cancelMode());
      case 'override': return setScopeOverride(dataset.tool, dataset.value === '' ? null : dataset.value);
      case 'save-retention': return saveRetention(dataset.appliesTo);
      case 'search': return setContentSearch(dataset.value);
      case 'sanction': return setToolSanction(dataset.tool, dataset.value);
      case 'endpoint': return setEndpointCollector(dataset.collector, onOff(dataset.value));
      case 'endpoint-tool': return setEndpointTool(dataset.tool, dataset.collector, onOff(dataset.value));
      case 'tls-inspection': return setTLSInspection(onOff(dataset.value));
      default: return Promise.resolve(state);
    }
  }

  return Object.freeze({ get state() { return state; }, load, chooseMode, setScopeOverride, setRetentionDraft, saveRetention, setContentSearch, setToolSanction, setEndpointCollector, setEndpointTool, setTLSInspection, act });
}
