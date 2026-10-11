// rules.js — Rules: the enforcement rules and the custom labels they name, the state of the page
// and every action on it.
//
// The rules are laid out as a firewall's rulebase: one numbered list, read top to bottom, where the
// first rule that matches a prompt decides and a prompt no rule matches is logged. A rule matches on
// who sent the prompt (people, provisioned groups, departments, units and teams), the tool, the
// content's labels and the collection route, and can be switched off while it keeps its place.
// Changes are a draft until the whole list is saved, which control-api writes in one replacement and
// audits; devices get it on their next policy poll. Each rule's prompts over the last 30 days come
// from the query API.
//
// DOM-free, like settings.js: a test drives it without a browser.

import { buildPeopleDocument, buildRuleMatchesDocument, windowFor } from './dsl.js';

/** What an enforcement rule does to a submission it matches. */
export const RULE_ACTIONS = Object.freeze(['allow', 'warn', 'block']);
export const RULE_ACTION_LABELS = Object.freeze({ allow: 'Allow', warn: 'Warn', block: 'Block' });

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

/** A rule's match lists, in the order the editor shows them. */
export const RULE_MATCH_FIELDS = Object.freeze(['labels', 'tools', 'categories', 'sanction', 'routes']);
export const RULE_MATCH_TITLES = Object.freeze({ labels: 'Labels', tools: 'Tools', categories: 'Categories', sanction: 'Sanction', routes: 'Routes' });

/** What a rule's users may name, by the prefix of each value, and what the page calls each. */
export const USER_KINDS = Object.freeze(['user', 'group', 'department', 'org_unit', 'team']);
export const USER_KIND_LABELS = Object.freeze({ user: 'Person', group: 'Group', department: 'Department', org_unit: 'Unit', team: 'Team' });

/** The picker's filter: everything, or one kind. */
export const PICK_FILTERS = Object.freeze({ all: 'All', user: 'People', group: 'Groups', department: 'Departments', org_unit: 'Units', team: 'Teams' });

export const MAX_RULES = 100;
export const MAX_RULE_MESSAGE = 280;
export const MAX_RULE_USERS = 200;

/** The window the Hits column counts over. */
export const HITS_WINDOW = 'd30';

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
  if (!RULE_ID.test(rule.rule_id)) return problem('The name must start with a lower-case letter and use only lower-case letters, digits, "_", "." or "-" (at most 128).');
  if (others.some((r) => r.rule_id === rule.rule_id)) return problem(`Another rule is already called ${rule.rule_id}.`);
  if (!RULE_ACTIONS.includes(rule.action)) return problem('Choose allow, warn or block.');
  const length = [...rule.message].length;
  if (rule.action !== 'allow' && rule.message.trim() === '') return problem('A warn or block rule needs a message for the person it stops.');
  if (length > MAX_RULE_MESSAGE) return problem(`The message is ${length} characters; at most ${MAX_RULE_MESSAGE}.`);
  if (rule.link !== '' && !httpsLink(rule.link)) return problem('The link must be an https:// address.');
  if (rule.users.length > MAX_RULE_USERS) return problem(`A rule names at most ${MAX_RULE_USERS} people, groups, departments, units and teams.`);
  return null;
}

const BLANK_RULE = Object.freeze({
  rule_id: '', action: 'block', message: '', link: '', users: Object.freeze([]), enabled: true,
  match: Object.freeze(Object.fromEntries(RULE_MATCH_FIELDS.map((f) => [f, Object.freeze([])]))),
});

/** A rule as the page holds it: every field a string, every list an array. */
function editable(rule) {
  return Object.freeze({
    rule_id: rule.rule_id ?? '',
    action: rule.action ?? 'block',
    message: rule.message ?? '',
    link: rule.link ?? '',
    users: Object.freeze([...(rule.users ?? [])]),
    enabled: rule.enabled !== false,
    match: Object.freeze(Object.fromEntries(RULE_MATCH_FIELDS.map((f) => [f, Object.freeze([...(rule.match?.[f] ?? [])])]))),
  });
}

/** A rule as control-api takes it: no link when there is none. */
function wire(rule) {
  const out = { rule_id: rule.rule_id, action: rule.action, match: Object.fromEntries(RULE_MATCH_FIELDS.map((f) => [f, [...rule.match[f]]])), message: rule.message, users: [...rule.users], enabled: rule.enabled };
  if (rule.link) out.link = rule.link;
  return out;
}

const sameRule = (a, b) => JSON.stringify(wire(a)) === JSON.stringify(wire(b));

/** What the draft changed about a rule against the list read: 'new', 'edited', or null. */
export function ruleChange(rule, saved) {
  const before = (saved ?? []).find((r) => r.rule_id === rule.rule_id);
  if (!before) return 'new';
  return sameRule(rule, before) ? null : 'edited';
}

/**
 * The ids of the rules the draft moves: the fewest kept rules that, taken out and put back, turn the
 * order read into the draft's. A rule pushed down by one moved above it has not moved.
 */
export function movedRules(draft, saved) {
  const drafted = new Set((draft ?? []).map((r) => r.rule_id));
  const before = (saved ?? []).map((r) => r.rule_id).filter((id) => drafted.has(id));
  const read = new Set(before);
  const after = (draft ?? []).map((r) => r.rule_id).filter((id) => read.has(id));
  // The longest run of rules both orders share; every kept rule outside it moved.
  const longest = Array.from({ length: before.length + 1 }, () => new Array(after.length + 1).fill(0));
  for (let i = before.length - 1; i >= 0; i--) {
    for (let j = after.length - 1; j >= 0; j--) {
      longest[i][j] = before[i] === after[j] ? longest[i + 1][j + 1] + 1 : Math.max(longest[i + 1][j], longest[i][j + 1]);
    }
  }
  const stayed = new Set();
  for (let i = 0, j = 0; i < before.length && j < after.length;) {
    if (before[i] === after[j]) {
      stayed.add(before[i]);
      i += 1;
      j += 1;
    } else if (longest[i + 1][j] >= longest[i][j + 1]) i += 1;
    else j += 1;
  }
  return new Set(after.filter((id) => !stayed.has(id)));
}

/** How many rules the draft adds, removes, changes or moves against the list read; a rule changed and moved counts once. */
export function changeCount(draft, saved) {
  if (!draft) return 0;
  const list = saved ?? [];
  const ids = new Set(draft.map((r) => r.rule_id));
  const moved = movedRules(draft, list);
  const removed = list.filter((r) => !ids.has(r.rule_id)).length;
  return removed + draft.filter((r) => ruleChange(r, list) !== null || moved.has(r.rule_id)).length;
}

const covers = (wide, narrow) => wide.length === 0 || (narrow.length > 0 && narrow.every((v) => wide.includes(v)));

/**
 * The enabled rule above `index` that matches every prompt rule `index` matches, so rule `index` can
 * never decide one, or null. Group membership is not compared: a rule is shadowed only by one that
 * names everyone, or every value it names.
 */
export function shadowedBy(list, index) {
  const rule = list[index];
  if (!rule || !rule.enabled) return null;
  for (let i = 0; i < index; i++) {
    const above = list[i];
    if (!above.enabled || !covers(above.users, rule.users)) continue;
    if (RULE_MATCH_FIELDS.every((f) => covers(above.match[f], rule.match[f]))) return i;
  }
  return null;
}

/** A user value's kind and what follows it: "group:abc" is ['group', 'abc']. */
export function userKind(value) {
  const at = String(value).indexOf(':');
  return at < 0 ? ['', String(value)] : [value.slice(0, at), value.slice(at + 1)];
}

/** The last part of a unit's distinguished name, as the Teams pop-up names a unit. */
function unitName(value) {
  const first = String(value ?? '').split(/(?<!\\),/)[0] ?? '';
  return first.replace(/^\s*(OU|CN|DC)=/i, '').replace(/\\,/g, ',').trim() || String(value ?? '');
}

/**
 * Who a user value is, for the page: its name, kind, how many people it reaches when known, and
 * whether it still exists. `names` is what the rules read named, and what the picker has listed.
 */
export function userOf(value, names = {}) {
  const [kind, rest] = userKind(value);
  const known = names[value];
  const fallback = kind === 'org_unit' ? unitName(rest) : kind === 'department' ? rest : kind === 'user' ? rest : '';
  const name = known?.name || fallback;
  return Object.freeze({
    value, kind, name: kind === 'org_unit' && known?.name === rest ? unitName(rest) : name,
    members: known?.members ?? null, found: known ? known.found !== false : true,
  });
}

/** A custom label's lists, in the order the editor shows them. */
export const CUSTOM_LABEL_FIELDS = Object.freeze(['words', 'customer_names', 'file_extensions', 'file_names']);

export const MAX_CUSTOM_LABELS = 50;
export const MAX_CUSTOM_LABEL_TERMS = 200;
const MAX_LABEL_NAME = 80;
const MAX_TERM = 100;
const FILE_EXTENSION = /^[a-z0-9]+(\.[a-z0-9]+)*$/;

/**
 * The terms a typed list holds: one per line, or for file extensions also separated by commas or
 * spaces, trimmed, extensions without their leading dot and lower-cased, with repeats dropped
 * whatever their case.
 */
export function parseTerms(field, text) {
  const raw = String(text ?? '').split(field === 'file_extensions' ? /[\n,\s]+/ : /\n/);
  const out = [];
  for (let term of raw) {
    term = term.trim();
    if (field === 'file_extensions') term = term.replace(/^\./, '').toLowerCase();
    if (term !== '' && !out.some((t) => t.toLowerCase() === term.toLowerCase())) out.push(term);
  }
  return out;
}

/**
 * The code a new label is saved under: custom_ and its name in lower-case letters, digits and "_",
 * with a number added when another label has it. The code never changes once saved, so a rename
 * keeps what rules and recorded events name.
 */
export function customLabelCode(name, taken = []) {
  const slug = String(name ?? '').normalize('NFKD').replace(/[̀-ͯ]/g, '').toLowerCase()
    .replace(/[^a-z0-9]+/g, '_').replace(/^_+|_+$/g, '') || 'label';
  for (let n = 1; ; n++) {
    const suffix = n === 1 ? '' : `_${n}`;
    const code = `custom_${slug.slice(0, 56 - suffix.length).replace(/_+$/, '')}${suffix}`;
    if (!taken.includes(code)) return code;
  }
}

/**
 * Why a custom label cannot be saved as written, or null. `others` is the rest of the list, whose
 * names it must not repeat.
 */
export function customLabelProblem(label, others = []) {
  const problem = (message) => Object.freeze({ code: 'invalid_custom_label', message });
  const name = label.display_name.trim();
  if (name === '' || [...name].length > MAX_LABEL_NAME) return problem(`Give the label a name of at most ${MAX_LABEL_NAME} characters.`);
  if (others.some((l) => l.display_name.trim().toLowerCase() === name.toLowerCase())) return problem(`Another label is already called ${name}.`);
  const terms = CUSTOM_LABEL_FIELDS.reduce((n, f) => n + label[f].length, 0);
  if (terms === 0) return problem('Add at least one word, customer name, file type or file name.');
  if (terms > MAX_CUSTOM_LABEL_TERMS) return problem(`A label holds at most ${MAX_CUSTOM_LABEL_TERMS} terms; this one has ${terms}.`);
  for (const term of [...label.words, ...label.customer_names]) {
    if ([...term].length > MAX_TERM) return problem(`"${term.slice(0, 24)}…" is longer than ${MAX_TERM} characters.`);
    if (!/[\p{L}\p{N}]/u.test(term)) return problem(`"${term}" needs a letter or a digit.`);
  }
  for (const ext of label.file_extensions) {
    if (ext.length > 16 || !FILE_EXTENSION.test(ext)) return problem(`"${ext}" is not a file type such as pdf or tar.gz.`);
  }
  for (const file of label.file_names) {
    if (/[\/\\[\]]/.test(file) || [...file].length > 255) return problem(`"${file}" must be a file name without / \\ [ or ].`);
    if (file.replace(/[*?]/g, '') === '') return problem(`"${file}" needs more than wildcards.`);
  }
  return null;
}

const BLANK_LABEL = Object.freeze({
  label: null, display_name: '',
  ...Object.fromEntries(CUSTOM_LABEL_FIELDS.map((f) => [f, ''])),
});

const LIST = Object.freeze({ pending: false, problem: null, rows: Object.freeze([]) });

const PICKER = Object.freeze({
  open: false,
  filter: 'all',
  query: '',
  term: '',
  people: Object.freeze({ ...LIST, next: null }),
  groups: LIST,
  departments: LIST,
  units: LIST,
  teams: LIST,
});

const IDLE = Object.freeze({
  // query is the filter as typed; filter is the one the list is narrowed by, set when it is applied.
  query: '',
  filter: '',
  hits: Object.freeze({ pending: false, problem: null, counts: null }),
  // draft is the list being edited, null while it is the one read.
  draft: null,
  pending: null,
  listProblem: null,
  // panel is the rule open in the side panel: its index in the shown list (null for a new rule),
  // the rule as edited, where a new rule goes ('bottom', or the index it is put at), the names of
  // what its users hold, and the picker.
  panel: null,
  // editor is the custom label open for editing: its code (null for a new label), its name and
  // each list as typed. Every save writes the whole list.
  labels: Object.freeze({ editor: null, pending: null, problem: null }),
});

const RULES_NOT_READ = Object.freeze({ code: 'rules_not_reported', message: 'The current rules were not read; reload the page and try again.' });
const LABELS_NOT_READ = Object.freeze({ code: 'custom_labels_not_reported', message: 'The current custom labels were not read; reload the page and try again.' });

const problemOf = (answer, fallback) => Object.freeze({ code: answer?.error?.code ?? 'refused', message: answer?.error?.message ?? fallback });
const byName = (a, b) => a.name.localeCompare(b.name, undefined, { sensitivity: 'base' });

/** One thing the picker lists: its value as a rule names it, its kind, name and a detail line. */
function pickItem(value, kind, name, detail = '', members = null) {
  return Object.freeze({ value, kind, name: String(name ?? ''), detail, members });
}

/**
 * The page's behaviour over one admin api and one query api.
 *
 * @param {object} input
 * @param {ReturnType<import('./transport.js').createAdminApi>} input.admin
 * @param {{run: (body: object) => Promise<object>}} [input.query]
 * @param {(state: object) => void} [input.onChange]
 * @param {() => Date} [input.now]
 */
export function createRules({ admin, query = null, onChange = () => {}, now = () => new Date() }) {
  let state = Object.freeze({ status: 'idle', data: null, problem: null, refreshProblem: null, ...IDLE });
  let loadSeq = 0;
  // Each opening of the picker is a session; an answer that arrives after it closed is dropped.
  let pickSeq = 0;

  function set(patch) {
    state = Object.freeze({ ...state, ...patch });
    onChange(state);
    return state;
  }

  async function readHits() {
    if (!query) return;
    const seq = loadSeq;
    set({ hits: Object.freeze({ ...state.hits, pending: true, problem: null }) });
    let hits;
    try {
      const envelope = await query.run(buildRuleMatchesDocument({ window: windowFor(HITS_WINDOW, now()) }));
      if (envelope?.error || !Array.isArray(envelope?.data)) {
        hits = Object.freeze({ pending: false, problem: problemOf(envelope, 'The prompts each rule matched could not be read.'), counts: null });
      } else {
        const counts = {};
        for (const row of envelope.data) {
          const id = String(row.rule ?? '');
          if (id && typeof row.matches === 'number') counts[id] = (counts[id] ?? 0) + row.matches;
        }
        hits = Object.freeze({ pending: false, problem: null, counts: Object.freeze(counts), truncated: Boolean(envelope.meta?.truncated) });
      }
    } catch (error) {
      hits = Object.freeze({ pending: false, problem: problemOf(error?.envelope, 'The prompts each rule matched could not be read.'), counts: null });
    }
    if (seq === loadSeq) set({ hits });
  }

  async function load({ quiet = false } = {}) {
    const seq = ++loadSeq;
    const keep = quiet && state.data !== null;
    if (!keep) set({ status: 'loading', problem: null, refreshProblem: null });
    const [settings, rules] = await Promise.all([admin.settings(), admin.rules()]);
    if (seq !== loadSeq) return state;
    if (settings.state === 'available' || rules.state === 'available') {
      const s = settings.state === 'available' ? settings.data : null;
      // Rules that were not read are null, never an empty list: saving over them would delete them.
      const data = Object.freeze({
        rules: rules.state === 'available' ? rules.data : null,
        names: rules.state === 'available' ? rules.users ?? Object.freeze({}) : Object.freeze({}),
        data_classes: s?.data_classes ?? null,
        custom_labels: s?.custom_labels ?? null,
        app_categories: s?.app_categories ?? null,
        tools: s?.tools ?? null,
      });
      set({ status: 'ready', data, problem: null, refreshProblem: null });
      readHits();
    } else if (keep) {
      set({ refreshProblem: rules.error ?? settings.error });
    } else {
      const answer = rules.state !== 'available' ? rules : settings;
      set({ status: 'refused', problem: Object.freeze({ ...answer.error, status: answer.status }), data: null });
    }
    return state;
  }

  // --- The list: a draft until saved -----------------------------------------------------------

  /** The list the page shows: the unsaved draft, else the one read. */
  function shownRules() {
    return state.draft ?? state.data?.rules ?? null;
  }

  function editable_() {
    if (state.status !== 'ready' || state.pending) return false;
    if (shownRules() === null) {
      set({ listProblem: RULES_NOT_READ });
      return false;
    }
    return true;
  }

  function changeList(next) {
    return set({ draft: Object.freeze(next), listProblem: null });
  }

  function rulesAt(index) {
    if (!editable_() || state.panel) return null;
    const list = shownRules();
    return Number.isInteger(index) && list[index] ? list : null;
  }

  function toggleEnabled(index) {
    const list = rulesAt(index);
    if (!list) return state;
    return changeList(list.map((r, i) => (i === index ? Object.freeze({ ...r, enabled: !r.enabled }) : r)));
  }

  /** Move a rule by `step` places (up is negative), or to the top (-Infinity) or bottom (Infinity). */
  function move(index, step) {
    const list = rulesAt(index);
    if (!list) return state;
    const to = Math.max(0, Math.min(list.length - 1, index + step));
    if (to === index) return state;
    const next = [...list];
    const [rule] = next.splice(index, 1);
    next.splice(to, 0, rule);
    return changeList(next);
  }

  function remove(index) {
    const list = rulesAt(index);
    if (!list) return state;
    return changeList(list.filter((_, i) => i !== index));
  }

  /** A copy of a rule just below it, switched off, under a name no rule has. */
  function clone(index) {
    const list = rulesAt(index);
    if (!list) return state;
    if (list.length >= MAX_RULES) return set({ listProblem: Object.freeze({ code: 'too_many_rules', message: `A list holds at most ${MAX_RULES} rules.` }) });
    const base = list[index].rule_id.replace(/_copy(_\d+)?$/, '').slice(0, 110);
    let id = `${base}_copy`;
    for (let n = 2; list.some((r) => r.rule_id === id); n++) id = `${base}_copy_${n}`;
    const copy = Object.freeze({ ...list[index], rule_id: id, enabled: false });
    return changeList([...list.slice(0, index + 1), copy, ...list.slice(index + 1)]);
  }

  function discard() {
    if (state.pending) return state;
    return set({ draft: null, panel: null, listProblem: null });
  }

  /** Save the whole draft list. control-api replaces the tenant's list with it in one write. */
  async function save() {
    const draft = state.draft;
    if (state.status !== 'ready' || state.pending || state.panel || draft === null) return state;
    set({ pending: 'save', listProblem: null });
    const answer = await admin.setRules(draft.map(wire));
    if (answer.state !== 'done') return set({ pending: null, listProblem: answer.error });
    set({ draft: null, pending: null, listProblem: null });
    return load({ quiet: true });
  }

  /** The filter as typed. Held without a repaint, so the field keeps its focus. */
  function setQuery(value) {
    state = Object.freeze({ ...state, query: String(value ?? '') });
  }

  function applyFilter() {
    return set({ filter: state.query.trim() });
  }

  // --- The side panel: one rule, as edited ------------------------------------------------------

  function openRule(index) {
    if (!editable_() || state.panel) return state;
    const list = shownRules();
    if (index === null) {
      if (list.length >= MAX_RULES) return set({ listProblem: Object.freeze({ code: 'too_many_rules', message: `A list holds at most ${MAX_RULES} rules.` }) });
      return set({ panel: Object.freeze({ index: null, rule: BLANK_RULE, at: 'bottom', names: Object.freeze({}), picker: PICKER, problem: null }), listProblem: null });
    }
    if (!Number.isInteger(index) || !list[index]) return state;
    return set({ panel: Object.freeze({ index, rule: editable(list[index]), at: null, names: Object.freeze({}), picker: PICKER, problem: null }), listProblem: null });
  }

  function setPanel(patch) {
    if (!state.panel) return state;
    return set({ panel: Object.freeze({ ...state.panel, ...patch }) });
  }

  function editRule(change) {
    if (!state.panel) return state;
    return setPanel({ rule: Object.freeze({ ...state.panel.rule, ...change }), problem: null });
  }

  /** Where a new rule goes: at the bottom, just above the default, or at an index ('top' is 0). */
  function place(value) {
    if (!state.panel || state.panel.index !== null) return state;
    if (value === 'bottom') return setPanel({ at: 'bottom' });
    const at = value === 'top' ? 0 : Number(value);
    return Number.isInteger(at) && at >= 0 && at < shownRules().length ? setPanel({ at }) : state;
  }

  /** A typed field of the open rule. Held without a repaint, so the field keeps its focus. */
  function setDraft(field, value) {
    const panel = state.panel;
    if (!panel) return;
    if (field === 'query') {
      state = Object.freeze({ ...state, panel: Object.freeze({ ...panel, picker: Object.freeze({ ...panel.picker, query: String(value ?? '') }) }) });
      return;
    }
    if (!['rule_id', 'message', 'link'].includes(field)) return;
    const rule = Object.freeze({ ...panel.rule, [field]: String(value ?? '') });
    state = Object.freeze({ ...state, panel: Object.freeze({ ...panel, rule }) });
  }

  /** Add the value to one of the open rule's match lists, or take it out. */
  function toggleMatch(field, value) {
    const panel = state.panel;
    if (!panel || !RULE_MATCH_FIELDS.includes(field) || typeof value !== 'string' || value === '') return state;
    const current = panel.rule.match[field];
    const next = current.includes(value) ? current.filter((v) => v !== value) : [...current, value];
    return editRule({ match: Object.freeze({ ...panel.rule.match, [field]: Object.freeze(next) }) });
  }

  /** Name the value in the open rule's users, or take it out. */
  function toggleUser(value) {
    const panel = state.panel;
    if (!panel || typeof value !== 'string' || !USER_KINDS.includes(userKind(value)[0])) return state;
    const current = panel.rule.users;
    if (current.includes(value)) return editRule({ users: Object.freeze(current.filter((v) => v !== value)) });
    const item = listedItems(panel.picker).find((i) => i.value === value);
    const names = item ? Object.freeze({ ...panel.names, [value]: Object.freeze({ name: item.name, members: item.members, found: true }) }) : panel.names;
    return set({ panel: Object.freeze({ ...panel, names, rule: Object.freeze({ ...panel.rule, users: Object.freeze([...current, value]) }), problem: null }) });
  }

  /** Keep the open rule in the draft list, if it is valid; the list is not saved yet. */
  function apply() {
    const panel = state.panel;
    if (!panel || state.pending) return state;
    const list = shownRules();
    const rule = Object.freeze({ ...panel.rule, rule_id: panel.rule.rule_id.trim(), link: panel.rule.link.trim() });
    const problem = ruleProblem(rule, list.filter((_, i) => i !== panel.index));
    if (problem) return setPanel({ rule, problem });
    let draft;
    if (panel.index !== null) draft = list.map((r, i) => (i === panel.index ? rule : r));
    else if (Number.isInteger(panel.at)) draft = [...list.slice(0, panel.at), rule, ...list.slice(panel.at)];
    else draft = [...list, rule];
    // What the panel learnt about the values it named stays known to the list.
    const names = Object.freeze({ ...state.data.names, ...panel.names });
    return set({ draft: Object.freeze(draft), panel: null, listProblem: null, data: Object.freeze({ ...state.data, names }) });
  }

  function cancel() {
    pickSeq += 1;
    return set({ panel: null });
  }

  // --- The picker: people, groups, departments, units and teams ---------------------------------

  function setPicker(patch, seq = pickSeq) {
    if (!state.panel || seq !== pickSeq) return state;
    return setPanel({ picker: Object.freeze({ ...state.panel.picker, ...patch }) });
  }

  async function readList(key, read) {
    const seq = pickSeq;
    setPicker({ [key]: Object.freeze({ ...state.panel.picker[key], pending: true, problem: null }) }, seq);
    const next = await read();
    setPicker({ [key]: next }, seq);
  }

  async function readPeople({ append = false } = {}) {
    if (!query) return;
    const seq = pickSeq;
    const picker = state.panel.picker;
    const cursor = append ? picker.people.next : null;
    setPicker({ people: Object.freeze({ ...picker.people, pending: true, problem: null }) }, seq);
    let people;
    try {
      const envelope = await query.run(buildPeopleDocument({ search: picker.term, synced: false, cursor, limit: 50 }));
      const rows = state.panel?.picker.people.rows ?? [];
      people = envelope?.error || !Array.isArray(envelope?.data)
        ? Object.freeze({ ...LIST, rows, next: null, problem: problemOf(envelope, 'The people list was refused.') })
        : Object.freeze({
          ...LIST,
          rows: Object.freeze([...(append ? rows : []), ...envelope.data.map((p) => {
            const ref = String(p.subject ?? p.user_ref ?? '');
            return pickItem(`user:${ref}`, 'user', p.name || ref, p.department ?? '', 1);
          })]),
          next: envelope.page?.next_cursor ?? null,
        });
    } catch (error) {
      people = Object.freeze({ ...LIST, next: null, problem: problemOf(error?.envelope, 'The people list could not be read.') });
    }
    setPicker({ people }, seq);
  }

  const readGroups = () => readList('groups', async () => {
    const answer = await admin.directoryGroups('');
    if (answer.state !== 'available') return Object.freeze({ ...LIST, problem: problemOf(answer, 'The groups could not be read.') });
    return Object.freeze({ ...LIST, rows: Object.freeze(answer.provisioned.filter((g) => g.id).map((g) => pickItem(`group:${g.id}`, 'group', g.name, '', g.members ?? null)).sort(byName)) });
  });

  const readValues = (key, kind) => readList(key, async () => {
    const answer = await admin.directoryValues(kind);
    if (answer.state !== 'available') return Object.freeze({ ...LIST, problem: problemOf(answer, 'The directory could not be read.') });
    return Object.freeze({ ...LIST, rows: Object.freeze(answer.values.map((v) => pickItem(`${kind}:${v.value}`, kind, kind === 'org_unit' ? unitName(v.value) : v.value, kind === 'org_unit' ? v.value : '', v.people ?? null)).sort(byName)) });
  });

  const readTeams = () => readList('teams', async () => {
    const answer = await admin.teams();
    if (answer.state !== 'available') return Object.freeze({ ...LIST, problem: problemOf(answer, 'The teams could not be read.') });
    return Object.freeze({ ...LIST, rows: Object.freeze(answer.teams.map((t) => pickItem(`team:${t.team_id}`, 'team', t.name, '', t.members ?? null)).sort(byName)) });
  });

  async function openPicker() {
    if (!state.panel || state.panel.picker.open) return state;
    pickSeq += 1;
    setPanel({ picker: Object.freeze({ ...PICKER, open: true }) });
    await Promise.all([readPeople(), readGroups(), readValues('departments', 'department'), readValues('units', 'org_unit'), readTeams()]);
    return state;
  }

  function closePicker() {
    pickSeq += 1;
    return setPanel({ picker: PICKER });
  }

  function pickFilter(value) {
    if (!state.panel?.picker.open || !(value in PICK_FILTERS)) return state;
    return setPicker({ filter: value });
  }

  /** Search the picker: people by the start of their name, everything else by any part of it. */
  async function search() {
    if (!state.panel?.picker.open) return state;
    setPicker({ term: state.panel.picker.query.trim() });
    await readPeople();
    return state;
  }

  async function morePeople() {
    if (!state.panel?.picker.open || !state.panel.picker.people.next) return state;
    await readPeople({ append: true });
    return state;
  }

  // --- Custom labels: each save writes the whole list ------------------------------------------

  function setLabels(patch) {
    return set({ labels: Object.freeze({ ...state.labels, ...patch }) });
  }

  function labelsEditable() {
    if (state.status !== 'ready' || state.labels.pending) return false;
    if (state.data.custom_labels === null) {
      setLabels({ problem: LABELS_NOT_READ });
      return false;
    }
    return true;
  }

  /** Open a label for editing by its code, or a new one (null). */
  function openLabel(code) {
    if (!labelsEditable()) return state;
    const list = state.data.custom_labels;
    if (code === null) {
      if (list.length >= MAX_CUSTOM_LABELS) return setLabels({ problem: Object.freeze({ code: 'too_many_custom_labels', message: `A tenant holds at most ${MAX_CUSTOM_LABELS} custom labels.` }) });
      return setLabels({ editor: Object.freeze({ ...BLANK_LABEL, problem: null }), problem: null });
    }
    const label = list.find((l) => l.label === code);
    if (!label) return state;
    return setLabels({
      editor: Object.freeze({ label: label.label, display_name: label.display_name,
        ...Object.fromEntries(CUSTOM_LABEL_FIELDS.map((f) => [f, label[f].join(f === 'file_extensions' ? ', ' : '\n')])), problem: null }),
      problem: null,
    });
  }

  /** A typed field of the open label. Held without a repaint, so the field keeps its focus. */
  function setLabelDraft(field, value) {
    const editor = state.labels.editor;
    if (!editor || !['display_name', ...CUSTOM_LABEL_FIELDS].includes(field)) return;
    state = Object.freeze({ ...state, labels: Object.freeze({ ...state.labels, editor: Object.freeze({ ...editor, [field]: String(value ?? '') }) }) });
  }

  function cancelLabel() {
    if (state.labels.pending) return state;
    return setLabels({ editor: null, problem: null });
  }

  /** The list as control-api takes it. */
  function wireLabels(list) {
    return list.map((l) => ({ label: l.label, display_name: l.display_name, ...Object.fromEntries(CUSTOM_LABEL_FIELDS.map((f) => [f, [...l[f]]])) }));
  }

  async function writeLabels(list) {
    const answer = await admin.setCustomLabels(wireLabels(list));
    if (answer.state !== 'done') return setLabels({ pending: null, problem: answer.error });
    setLabels({ editor: null, pending: null, problem: null });
    return load({ quiet: true });
  }

  /** Save the open label, if it is valid: the list is written with it added or replaced. */
  async function saveLabel() {
    const editor = state.labels.editor;
    if (!editor || !labelsEditable()) return state;
    const list = state.data.custom_labels;
    const others = list.filter((l) => l.label !== editor.label);
    const label = Object.freeze({
      label: editor.label ?? customLabelCode(editor.display_name.trim(), list.map((l) => l.label)),
      display_name: editor.display_name.trim(),
      ...Object.fromEntries(CUSTOM_LABEL_FIELDS.map((f) => [f, parseTerms(f, editor[f])])),
    });
    const problem = customLabelProblem(label, others);
    if (problem) return setLabels({ editor: Object.freeze({ ...editor, problem }) });
    setLabels({ pending: 'save', editor: Object.freeze({ ...editor, problem: null }), problem: null });
    return writeLabels(editor.label === null ? [...list, label] : list.map((l) => (l.label === label.label ? label : l)));
  }

  /**
   * Delete a label no rule names, saved or unsaved: control-api refuses to delete one a saved rule
   * names, and an unsaved rule naming it could not be saved after.
   */
  async function deleteLabel(code) {
    if (!labelsEditable() || state.labels.editor) return state;
    const list = state.data.custom_labels;
    const label = list.find((l) => l.label === code);
    if (!label) return state;
    const naming = [...new Set([...(state.data.rules ?? []), ...(state.draft ?? [])].filter((r) => r.match.labels.includes(code)).map((r) => r.rule_id))];
    if (naming.length > 0) {
      return setLabels({ problem: Object.freeze({ code: 'custom_label_in_use', message: `Rule ${naming.join(', ')} names ${label.display_name}; take the label out of the rule first.` }) });
    }
    setLabels({ pending: code, problem: null });
    return writeLabels(list.filter((l) => l.label !== code));
  }

  /** One entry point for the page's buttons and controls, named by data-rl. */
  function act(dataset = {}) {
    const index = dataset.index === undefined ? null : Number(dataset.index);
    switch (dataset.rl) {
      case 'retry': return load();
      case 'filter': return Promise.resolve(applyFilter());
      case 'add': return Promise.resolve(openRule(null));
      case 'edit': return Promise.resolve(index === null ? state : openRule(index));
      case 'clone': return Promise.resolve(clone(index));
      case 'toggle': return Promise.resolve(toggleEnabled(index));
      case 'up': return Promise.resolve(move(index, -1));
      case 'down': return Promise.resolve(move(index, 1));
      case 'top': return Promise.resolve(move(index, -Infinity));
      case 'bottom': return Promise.resolve(move(index, Infinity));
      case 'delete': return Promise.resolve(remove(index));
      case 'discard': return Promise.resolve(discard());
      case 'save': return save();
      case 'action': return Promise.resolve(RULE_ACTIONS.includes(dataset.value) ? editRule({ action: dataset.value }) : state);
      case 'enabled': return Promise.resolve(state.panel ? editRule({ enabled: dataset.value !== 'off' }) : state);
      case 'at': return Promise.resolve(place(dataset.value));
      case 'match': return Promise.resolve(toggleMatch(dataset.field, dataset.value));
      case 'user': return Promise.resolve(toggleUser(dataset.value));
      case 'users-any': return Promise.resolve(state.panel ? editRule({ users: Object.freeze([]) }) : state);
      case 'pick-open': return openPicker();
      case 'pick-close': return Promise.resolve(closePicker());
      case 'pick-filter': return Promise.resolve(pickFilter(dataset.value));
      case 'pick-search': return search();
      case 'pick-more': return morePeople();
      case 'apply': return Promise.resolve(apply());
      case 'cancel': return Promise.resolve(cancel());
      case 'label-add': return Promise.resolve(openLabel(null));
      case 'label-edit': return Promise.resolve(dataset.label ? openLabel(dataset.label) : state);
      case 'label-cancel': return Promise.resolve(cancelLabel());
      case 'label-save': return saveLabel();
      case 'label-delete': return deleteLabel(dataset.label);
      default: return Promise.resolve(state);
    }
  }

  return Object.freeze({ get state() { return state; }, load, setDraft, setQuery, setLabelDraft, act });
}

/** Everything the picker has read, whatever its filter and search. */
function listedItems(picker) {
  return [...picker.people.rows, ...picker.groups.rows, ...picker.departments.rows, ...picker.units.rows, ...picker.teams.rows];
}

/**
 * What the picker shows: the kinds its filter allows, each matching the search. The people read
 * already searched by name; the rest match any part of their name.
 */
export function pickerItems(picker) {
  const term = picker.term.toLowerCase();
  const match = (item) => term === '' || item.name.toLowerCase().includes(term) || item.detail.toLowerCase().includes(term);
  const byKind = {
    group: picker.groups.rows.filter(match),
    department: picker.departments.rows.filter(match),
    org_unit: picker.units.rows.filter(match),
    team: picker.teams.rows.filter(match),
    user: picker.people.rows,
  };
  const kinds = picker.filter === 'all' ? ['group', 'department', 'org_unit', 'team', 'user'] : [picker.filter];
  return kinds.flatMap((k) => byKind[k] ?? []);
}

/** Does the rule match on `text`: its name, message, or anything it names? */
export function ruleMatchesQuery(rule, text, describe) {
  const term = String(text ?? '').trim().toLowerCase();
  if (term === '') return true;
  return [rule.rule_id, rule.message, ...describe(rule)].some((s) => String(s).toLowerCase().includes(term));
}
