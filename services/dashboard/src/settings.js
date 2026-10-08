// settings.js — Settings → Settings: the state of the page and every action on it.
//
// The page an admin uses to change what the tenant collects and keeps: the collection mode (within
// the ceiling) with an optional narrower per-tool override, the event and content retention
// periods, the sanction decision per tool, the content search tier, which endpoint collectors
// run on the devices, whether devices inspect TLS, the ordered enforcement rules, and the kill
// switches that stop decryption and enforcement on an interception route. Every read and write goes
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

/** What an enforcement rule does to a submission it matches. */
export const RULE_ACTIONS = Object.freeze(['allow', 'warn', 'block']);

/** The app catalog's categories by display name, in the order the editor offers those the catalog's apps fall in. */
export const RULE_CATEGORIES = Object.freeze([
  Object.freeze({ key: 'chat_assistant', label: 'Chat assistant' }),
  Object.freeze({ key: 'coding_agent', label: 'Coding agent' }),
  Object.freeze({ key: 'ide_assistant', label: 'IDE assistant' }),
  Object.freeze({ key: 'ide', label: 'IDE' }),
  Object.freeze({ key: 'local_runtime', label: 'Local model runtime' }),
  Object.freeze({ key: 'inference_api', label: 'Inference API' }),
  Object.freeze({ key: 'ai_feature', label: 'AI feature' }),
]);

export const RULE_SANCTIONS = Object.freeze(['sanctioned', 'unsanctioned']);

/** The collection routes, in fidelity order, by the name the page shows. */
export const RULE_ROUTES = Object.freeze([
  Object.freeze({ key: 'tool.hook', label: 'Tool hooks' }),
  Object.freeze({ key: 'ext.page_context', label: 'Browser extension (page)' }),
  Object.freeze({ key: 'tool.otel', label: 'Tool telemetry' }),
  Object.freeze({ key: 'cli.shim', label: 'Command-line shim' }),
  Object.freeze({ key: 'proxy.loopback', label: 'Local model broker' }),
  Object.freeze({ key: 'ext.web_request', label: 'Browser extension (request)' }),
  Object.freeze({ key: 'proxy.tls', label: 'TLS proxy' }),
  Object.freeze({ key: 'ext.dom', label: 'Browser extension (page text)' }),
  Object.freeze({ key: 'proc.detect', label: 'Process detection' }),
  Object.freeze({ key: 'inv.scan', label: 'Installed-app scan' }),
  Object.freeze({ key: 'net.flow', label: 'Network connections' }),
]);

/** The interception routes that have a kill switch, in the order the card shows them. */
export const KILL_SWITCH_ROUTES = Object.freeze([
  Object.freeze({ key: 'proxy.tls', label: 'TLS proxy' }),
  Object.freeze({ key: 'proxy.loopback', label: 'Local model broker' }),
]);

/** A kill switch's reason code, spelled as control-api accepts it. */
const REASON_CODE = /^[a-z][a-z0-9_.-]{0,63}$/;

/** A rule's match lists, in the order the editor shows them. */
export const RULE_MATCH_FIELDS = Object.freeze(['labels', 'tools', 'categories', 'sanction', 'routes']);

export const MAX_RULES = 100;
export const MAX_RULE_MESSAGE = 280;

const RULE_ID = /^[a-z][a-z0-9_.-]{0,127}$/;

/** An https URL with a host, spelled as control-api accepts it. */
function httpsLink(link) {
  if (!link.startsWith('https://')) return false;
  try {
    return new URL(link).hostname !== '';
  } catch {
    return false;
  }
}

/**
 * Why a rule cannot be kept as written, or null. `others` is the rest of the list, whose ids it must
 * not repeat. A warn or block shows its message to the person, so it needs one; an allow may have
 * none.
 */
export function ruleProblem(rule, others = []) {
  const problem = (message) => Object.freeze({ code: 'invalid_rule', message });
  if (!RULE_ID.test(rule.rule_id)) return problem('The rule id must start with a lower-case letter and use only lower-case letters, digits, "_", "." or "-" (at most 128).');
  if (others.some((r) => r.rule_id === rule.rule_id)) return problem(`Another rule already has the id ${rule.rule_id}.`);
  if (!RULE_ACTIONS.includes(rule.action)) return problem('Choose allow, warn or block.');
  const length = [...rule.message].length;
  if (rule.action !== 'allow' && rule.message.trim() === '') return problem('A warn or block rule needs a message for the person it stops.');
  if (length > MAX_RULE_MESSAGE) return problem(`The message is ${length} characters; at most ${MAX_RULE_MESSAGE}.`);
  if (rule.link !== '' && !httpsLink(rule.link)) return problem('The link must be an https:// address.');
  return null;
}

const BLANK_RULE = Object.freeze({
  rule_id: '', action: 'block', message: '', link: '',
  match: Object.freeze(Object.fromEntries(RULE_MATCH_FIELDS.map((f) => [f, Object.freeze([])]))),
});

/** A rule as the editor holds it: every field a string, every match list an array. */
function editable(rule) {
  return Object.freeze({
    rule_id: rule.rule_id ?? '',
    action: rule.action ?? 'block',
    message: rule.message ?? '',
    link: rule.link ?? '',
    match: Object.freeze(Object.fromEntries(RULE_MATCH_FIELDS.map((f) => [f, Object.freeze([...(rule.match?.[f] ?? [])])]))),
  });
}

/** A rule as control-api takes it: no link when there is none. */
function wire(rule) {
  const out = { rule_id: rule.rule_id, action: rule.action, match: Object.fromEntries(RULE_MATCH_FIELDS.map((f) => [f, [...rule.match[f]]])), message: rule.message };
  if (rule.link) out.link = rule.link;
  return out;
}

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
  // draft is the list being edited, null while it is the one read; editor is the rule open for
  // editing, at index (null for a new rule).
  rules: Object.freeze({ draft: null, editor: null, pending: null, problem: null }),
  // reasons holds the reason code typed for each route; pending is the route being written.
  killSwitch: Object.freeze({ pending: null, problem: null, reasons: Object.freeze({}) }),
});

/** A switch button's data-value: 'on' or 'off', anything else is no value. */
const onOff = (value) => (value === 'on' ? true : value === 'off' ? false : null);

const NOT_READ = Object.freeze({ code: 'endpoint_not_reported', message: 'The current endpoint settings were not read; reload the page and try again.' });
const TLS_NOT_READ = Object.freeze({ code: 'tls_inspection_not_reported', message: 'The current TLS inspection setting was not read; reload the page and try again.' });
const RULES_NOT_READ = Object.freeze({ code: 'rules_not_reported', message: 'The current rules were not read; reload the page and try again.' });
const KILL_SWITCHES_NOT_READ = Object.freeze({ code: 'kill_switches_not_reported', message: 'The current kill switches were not read; reload the page and try again.' });
const REASON_REQUIRED = Object.freeze({ code: 'invalid_reason_code', message: 'Give a reason code: a lower-case letter, then lower-case letters, digits, "_", "." or "-" (at most 64).' });

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
    const [answer, rules] = await Promise.all([admin.settings(), admin.rules()]);
    if (seq !== loadSeq) return state;
    if (answer.state === 'available') {
      // Rules that were not read are null, never an empty list: saving over them would delete them.
      const data = Object.freeze({ ...answer.data, rules: rules.state === 'available' ? rules.data : null });
      set({ status: 'ready', data, problem: null, refreshProblem: null });
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

  // --- Kill switches: one per interception route, tripped with a reason code --------------------

  /** A typed reason code for a route. Held without a repaint, so the field keeps its focus. */
  function setKillSwitchReason(route, value) {
    if (!KILL_SWITCH_ROUTES.some((r) => r.key === route)) return;
    const reasons = Object.freeze({ ...state.killSwitch.reasons, [route]: String(value ?? '').trim() });
    state = Object.freeze({ ...state, killSwitch: Object.freeze({ ...state.killSwitch, reasons }) });
  }

  /**
   * Trip (on) or clear a route's kill switch. Tripping needs a reason code, and tripping a tripped
   * switch gives it the new one. Nothing is sent over switches that were not read, or to clear a
   * switch that is not tripped.
   */
  async function setKillSwitch(route, on) {
    if (!KILL_SWITCH_ROUTES.some((r) => r.key === route) || typeof on !== 'boolean' || state.status !== 'ready' || state.killSwitch.pending) return state;
    const current = state.data.kill_switches;
    if (!Array.isArray(current)) {
      set({ killSwitch: Object.freeze({ ...state.killSwitch, problem: KILL_SWITCHES_NOT_READ }) });
      return state;
    }
    if (!on && !current.some((k) => k.route === route)) return state;
    const reason = state.killSwitch.reasons[route] ?? '';
    if (on && !REASON_CODE.test(reason)) {
      set({ killSwitch: Object.freeze({ ...state.killSwitch, problem: REASON_REQUIRED }) });
      return state;
    }
    set({ killSwitch: Object.freeze({ ...state.killSwitch, pending: route, problem: null }) });
    const answer = await admin.setKillSwitch(route, on, on ? reason : '');
    if (answer.state !== 'done') {
      set({ killSwitch: Object.freeze({ ...state.killSwitch, pending: null, problem: answer.error }) });
      return state;
    }
    set({ killSwitch: Object.freeze({ pending: null, problem: null, reasons: Object.freeze({ ...state.killSwitch.reasons, [route]: '' }) }) });
    return load({ quiet: true });
  }

  // --- Enforcement rules: edited as a draft list, saved as a whole ------------------------------

  /** The list the card shows: the unsaved draft, else the one read. */
  function shownRules() {
    return state.rules.draft ?? state.data?.rules ?? null;
  }

  function setRules(patch) {
    set({ rules: Object.freeze({ ...state.rules, ...patch }) });
    return state;
  }

  function rulesEditable() {
    if (state.status !== 'ready' || state.rules.pending) return false;
    if (shownRules() === null) {
      setRules({ problem: RULES_NOT_READ });
      return false;
    }
    return true;
  }

  function openRule(index) {
    if (!rulesEditable()) return state;
    const list = shownRules();
    if (index === null) {
      if (list.length >= MAX_RULES) return setRules({ problem: Object.freeze({ code: 'too_many_rules', message: `A list holds at most ${MAX_RULES} rules.` }) });
      return setRules({ editor: Object.freeze({ index: null, rule: BLANK_RULE, problem: null }), problem: null });
    }
    if (!Number.isInteger(index) || !list[index]) return state;
    return setRules({ editor: Object.freeze({ index, rule: editable(list[index]), problem: null }), problem: null });
  }

  function editRule(change) {
    const editor = state.rules.editor;
    if (!editor || state.rules.pending) return state;
    return setRules({ editor: Object.freeze({ ...editor, rule: Object.freeze({ ...editor.rule, ...change }), problem: null }) });
  }

  /** A typed field of the open rule. Held without a repaint, so the field keeps its focus. */
  function setRuleDraft(field, value) {
    const editor = state.rules.editor;
    if (!editor || !['rule_id', 'message', 'link'].includes(field)) return;
    const rule = Object.freeze({ ...editor.rule, [field]: String(value ?? '') });
    state = Object.freeze({ ...state, rules: Object.freeze({ ...state.rules, editor: Object.freeze({ ...editor, rule }) }) });
  }

  function setRuleAction(value) {
    if (!RULE_ACTIONS.includes(value)) return state;
    return editRule({ action: value });
  }

  /** Add the value to one of the open rule's match lists, or take it out. */
  function toggleRuleMatch(field, value) {
    const editor = state.rules.editor;
    if (!editor || !RULE_MATCH_FIELDS.includes(field) || typeof value !== 'string' || value === '') return state;
    const current = editor.rule.match[field];
    const next = current.includes(value) ? current.filter((v) => v !== value) : [...current, value];
    return editRule({ match: Object.freeze({ ...editor.rule.match, [field]: Object.freeze(next) }) });
  }

  /** Keep the open rule in the draft list, if it is valid; the list is not saved yet. */
  function applyRule() {
    const editor = state.rules.editor;
    if (!editor || state.rules.pending) return state;
    const list = shownRules();
    const rule = Object.freeze({ ...editor.rule, rule_id: editor.rule.rule_id.trim(), link: editor.rule.link.trim() });
    const problem = ruleProblem(rule, list.filter((_, i) => i !== editor.index));
    if (problem) return setRules({ editor: Object.freeze({ ...editor, rule, problem }) });
    const draft = editor.index === null ? [...list, rule] : list.map((r, i) => (i === editor.index ? rule : r));
    return setRules({ draft: Object.freeze(draft), editor: null, problem: null });
  }

  function cancelRule() {
    return setRules({ editor: null });
  }

  function deleteRule(index) {
    if (!rulesEditable() || state.rules.editor) return state;
    const list = shownRules();
    if (!Number.isInteger(index) || !list[index]) return state;
    return setRules({ draft: Object.freeze(list.filter((_, i) => i !== index)), problem: null });
  }

  /** Move a rule one place up (-1) or down (+1): the first rule that matches decides. */
  function moveRule(index, step) {
    if (!rulesEditable() || state.rules.editor) return state;
    const list = shownRules();
    const to = index + step;
    if (!Number.isInteger(index) || !list[index] || !list[to]) return state;
    const draft = [...list];
    [draft[index], draft[to]] = [draft[to], draft[index]];
    return setRules({ draft: Object.freeze(draft), problem: null });
  }

  function discardRules() {
    if (state.rules.pending) return state;
    return setRules({ draft: null, editor: null, problem: null });
  }

  /** Save the whole draft list. control-api replaces the tenant's list with it in one write. */
  async function saveRules() {
    const draft = state.rules.draft;
    if (state.status !== 'ready' || state.rules.pending || state.rules.editor || draft === null) return state;
    setRules({ pending: 'save', problem: null });
    const answer = await admin.setRules(draft.map(wire));
    if (answer.state !== 'done') return setRules({ pending: null, problem: answer.error });
    setRules({ draft: null, pending: null, problem: null });
    return load({ quiet: true });
  }

  /** One entry point for the page's buttons and controls, named by data-action. */
  function act(dataset = {}) {
    const index = dataset.index === undefined ? null : Number(dataset.index);
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
      case 'kill-switch': return setKillSwitch(dataset.route, onOff(dataset.value));
      case 'rule-add': return Promise.resolve(openRule(null));
      case 'rule-edit': return Promise.resolve(index === null ? state : openRule(index));
      case 'rule-delete': return Promise.resolve(deleteRule(index));
      case 'rule-up': return Promise.resolve(moveRule(index, -1));
      case 'rule-down': return Promise.resolve(moveRule(index, 1));
      case 'rule-action': return Promise.resolve(setRuleAction(dataset.value));
      case 'rule-match': return Promise.resolve(toggleRuleMatch(dataset.field, dataset.value));
      case 'rule-apply': return Promise.resolve(applyRule());
      case 'rule-cancel': return Promise.resolve(cancelRule());
      case 'rules-discard': return Promise.resolve(discardRules());
      case 'rules-save': return saveRules();
      default: return Promise.resolve(state);
    }
  }

  return Object.freeze({ get state() { return state; }, load, chooseMode, setScopeOverride, setRetentionDraft, saveRetention, setContentSearch, setToolSanction, setEndpointCollector, setEndpointTool, setTLSInspection, setKillSwitchReason, setKillSwitch, setRuleDraft, act });
}
