// rules-render.js — Rules, state to HTML. Pure: no DOM, no I/O, no state.
//
// The list reads like a firewall's rulebase: numbered rows read top to bottom, Any where a rule
// checks nothing, the default as a fixed last row, each rule's prompts over the last 30 days, and an
// unsaved change marked on its row until the list is saved. Every string from an API is escaped, a
// list that could not be read says so rather than showing as empty, and a tool is listed once, by
// its catalogue tool, however many fingerprints it has.

import { escapeHtml, renderPageHead } from './render.js';
import { formatCount, plural } from './format.js';
import { depButton, depProblem } from './deployment-render.js';
import {
  CUSTOM_LABEL_FIELDS, MAX_CUSTOM_LABELS, MAX_RULE_MESSAGE, MAX_RULES, PICK_FILTERS, RULE_ACTIONS, RULE_ACTION_LABELS,
  RULE_CATEGORIES, RULE_ROUTES, RULE_SANCTIONS, USER_KIND_LABELS,
  changeCount, movedRules, pickerItems, ruleChange, ruleMatchesQuery, shadowedBy, userOf,
} from './rules.js';

const CATEGORY_LABELS = Object.freeze(Object.fromEntries(RULE_CATEGORIES.map((c) => [c.key, c.label])));
const ROUTE_LABELS = Object.freeze(Object.fromEntries(RULE_ROUTES.map((r) => [r.key, r.label])));
const SANCTION_LABELS = Object.freeze({ sanctioned: 'Sanctioned', unsanctioned: 'Unsanctioned' });

/** "a", "a or b", "a, b or c". */
const either = (parts) => (parts.length < 2 ? parts.join('') : `${parts.slice(0, -1).join(', ')} or ${parts.at(-1)}`);

/** The catalogue's tools by key, each once, in the catalogue's order. */
function catalogueTools(data) {
  const tools = new Map();
  for (const t of data.tools ?? []) {
    if (t.tool_key && !tools.has(t.tool_key)) tools.set(t.tool_key, t.display_name ?? t.tool_key);
  }
  return tools;
}

/** What the page calls the values a rule's match lists hold. */
function vocabulary(data) {
  const tools = catalogueTools(data);
  const labels = new Map((data.custom_labels ?? []).map((l) => [l.label, l.display_name]));
  return {
    tools,
    name(field, value) {
      switch (field) {
        case 'tools': return tools.get(value) ?? value;
        case 'labels': return labels.get(value) ?? value;
        case 'categories': return CATEGORY_LABELS[value] ?? value;
        case 'sanction': return SANCTION_LABELS[value] ?? value;
        case 'routes': return ROUTE_LABELS[value] ?? value;
        default: return value;
      }
    },
  };
}

/** Every name a rule shows, for the filter: its people and groups, tools, labels, routes and action. */
function describe(rule, vocab, names) {
  return [
    ...rule.users.map((v) => userOf(v, names).name),
    ...['tools', 'categories', 'sanction', 'labels', 'routes'].flatMap((f) => rule.match[f].map((v) => vocab.name(f, v))),
    RULE_ACTION_LABELS[rule.action] ?? rule.action,
  ];
}

/** The rule read back as a sentence, so it can be checked before it is kept. */
export function ruleSentence(rule, data, names = {}) {
  const vocab = vocabulary(data);
  const named = (field) => rule.match[field].map((v) => vocab.name(field, v));
  const clauses = [];
  if (rule.users.length) clauses.push(`from ${either(rule.users.map((v) => userOf(v, names).name || v))}`);
  if (rule.match.labels.length) clauses.push(`holding ${either(named('labels'))}`);
  if (rule.match.tools.length) clauses.push(`to ${either(named('tools'))}`);
  if (rule.match.categories.length) clauses.push(`to a tool in ${either(named('categories'))}`);
  if (rule.match.sanction.length) clauses.push(`to ${rule.match.sanction.length === 1 ? (rule.match.sanction[0] === 'unsanctioned' ? 'an unsanctioned' : 'a sanctioned') : 'a sanctioned or unsanctioned'} tool`);
  if (rule.match.routes.length) clauses.push(`seen by ${either(named('routes'))}`);
  const verb = { allow: 'Allow', warn: 'Warn before sending', block: 'Block', redirect: 'Redirect' }[rule.action] ?? 'Match';
  const noun = rule.action === 'redirect' ? ['visits and prompts', 'visit and prompt'] : ['prompts', 'prompt'];
  const sentence = clauses.length ? `${verb} ${noun[0]} ${clauses.join(', ')}.` : `${verb} every ${noun[1]}.`;
  return rule.enabled ? sentence : `${sentence} Off: devices are not sent it.`;
}

// ---------------------------------------------------------------------------------------------
// Pieces
// ---------------------------------------------------------------------------------------------

const ANY = '<span class="rl-any">Any</span>';

function tag(text, { kind = '', title = '', gone = false, mono = false } = {}) {
  const cls = ['rl-tag', gone ? 'rl-tag-gone' : '', mono ? 'v-mono' : ''].filter(Boolean).join(' ');
  return `<span class="${cls}"${title ? ` title="${escapeHtml(title)}"` : ''}>${escapeHtml(text)}`
    + (kind ? `<span class="rl-kind">${escapeHtml(kind)}</span>` : '') + '</span>';
}

function actionPill(action) {
  const known = RULE_ACTIONS.includes(action);
  return `<span class="rl-action rl-action-${known ? action : 'unknown'}">${escapeHtml(RULE_ACTION_LABELS[action] ?? String(action || 'unknown'))}</span>`;
}

/** Does a user value reach nobody now: a group or team since removed, or one with no members? */
const reachesNobody = (who) => !who.found || who.members === 0;

/**
 * A user value as a tag: its name and kind (a person has none), with its name whole and how many
 * people it reaches in its title. A person the directory has no name for is shown by their
 * reference; a group or team since removed has no name left, so it is shown as not found.
 */
function userTag(value, names) {
  const who = userOf(value, names);
  const kind = USER_KIND_LABELS[who.kind] ?? who.kind;
  if (!who.found) {
    const title = 'No longer exists, so it reaches nobody';
    return who.name
      ? tag(who.name, { kind: `${kind} · not found`, title: `${who.name} · ${title}`, gone: true })
      : tag(`${kind} not found`, { title, gone: true });
  }
  const name = who.name || value;
  const title = [name, kind, who.members === null ? '' : plural(who.members, 'person', 'people')].filter(Boolean).join(' · ');
  return tag(name, { kind: who.kind === 'user' ? '' : kind, title, gone: who.members === 0, mono: /^u_[0-9a-f]{32}$/.test(who.name) });
}

function usersCell(rule, names) {
  return rule.users.length ? rule.users.map((v) => userTag(v, names)).join('') : ANY;
}

function toolCell(rule, vocab) {
  const parts = [
    ...rule.match.tools.map((v) => tag(vocab.name('tools', v))),
    ...rule.match.categories.map((v) => tag(vocab.name('categories', v), { kind: 'Category' })),
    ...rule.match.sanction.map((v) => tag(vocab.name('sanction', v))),
  ];
  return parts.length ? parts.join('') : ANY;
}

function contentCell(rule, vocab) {
  return rule.match.labels.length ? rule.match.labels.map((v) => tag(vocab.name('labels', v))).join('') : ANY;
}

function hitsCell(rule, hits, change) {
  if (change === 'new') return '<span class="v-absent" title="Not saved yet">—</span>';
  if (!hits.counts) return hits.pending ? '<span class="v-absent">…</span>' : '<span class="v-absent">not reported</span>';
  return `<span class="v-number">${formatCount(hits.counts[rule.rule_id] ?? 0)}</span>`;
}

function iconButton(symbol, data, label, disabled) {
  const attrs = Object.entries(data).map(([k, v]) => ` data-${k}="${escapeHtml(v)}"`).join('');
  return `<button type="button" class="rl-icon"${attrs} aria-label="${escapeHtml(label)}" title="${escapeHtml(label)}"${disabled ? ' disabled' : ''}>${symbol}</button>`;
}

/** The row's ⋯ menu: everything that is not a click away on the row itself. */
function rowMenu(rule, i, n, { locked, reorder, last }) {
  const item = (label, rl, disabled, danger = false) => `<button type="button" class="rl-menu-item${danger ? ' rl-menu-danger' : ''}" data-rl="${rl}" data-index="${i}"${disabled ? ' disabled' : ''}>${escapeHtml(label)}</button>`;
  return `<details class="rl-menu"><summary class="btn btn-quiet btn-small" aria-label="More for rule ${n}">⋯</summary><div class="rl-menu-items">`
    + item('Edit', 'edit', locked)
    + item('Clone', 'clone', locked)
    + item('Move to top', 'top', reorder || i === 0)
    + item('Move to bottom', 'bottom', reorder || last)
    + item(rule.enabled ? 'Switch off' : 'Switch on', 'toggle', locked)
    + item('Delete', 'delete', locked, true)
    + '</div></details>';
}

function ruleRow(state, list, rule, i, { vocab, names, locked, reorder, moved }) {
  const n = i + 1;
  const change = ruleChange(rule, state.data.rules);
  const shifted = moved.has(rule.rule_id);
  const shadow = shadowedBy(list, i);
  const marker = (change === 'new' ? '<span class="rl-mark rl-mark-new">New</span>' : change === 'edited' ? '<span class="rl-mark">Edited</span>' : '')
    + (shifted ? '<span class="rl-mark">Moved</span>' : '');
  const off = rule.enabled ? '' : '<span class="rl-mark rl-mark-off">Off</span>';
  // The name opens the rule, as a rulebase's does.
  const name = `<button type="button" class="rl-name rl-open" data-rl="edit" data-index="${i}" aria-label="Edit rule ${n}, ${escapeHtml(rule.rule_id)}" title="Edit rule ${n}"${locked ? ' disabled' : ''}>${escapeHtml(rule.rule_id)}</button>${marker}${off}`
    + (rule.message ? `<span class="dp-sub rl-message">${escapeHtml(rule.message)}</span>` : '')
    + (rule.match.routes.length ? `<span class="dp-sub">Seen by ${escapeHtml(either(rule.match.routes.map((v) => vocab.name('routes', v))))}</span>` : '')
    + (rule.enabled && rule.users.length && rule.users.every((v) => reachesNobody(userOf(v, names)))
      ? '<span class="rl-shadow">Reaches nobody now, so devices are not sent it.</span>' : '')
    + (shadow !== null ? `<span class="rl-shadow">Never decides a prompt: rule ${shadow + 1} above matches every prompt this one would.</span>` : '');
  const toggle = `<button type="button" class="rl-switch" role="switch" aria-checked="${rule.enabled}" data-rl="toggle" data-index="${i}"`
    + ` aria-label="Rule ${n}, ${escapeHtml(rule.rule_id)}: ${rule.enabled ? 'on' : 'off'}"${locked ? ' disabled' : ''}></button>`;
  const last = i === list.length - 1;
  return `<tr class="${['rl-row', rule.enabled ? '' : 'rl-off', change || shifted ? 'rl-changed' : ''].filter(Boolean).join(' ')}">`
    + `<td class="rl-num">${n}</td>`
    + `<td>${toggle}</td>`
    + `<td class="rl-name-cell">${name}</td>`
    + `<td>${usersCell(rule, names)}</td>`
    + `<td>${toolCell(rule, vocab)}</td>`
    + `<td>${contentCell(rule, vocab)}</td>`
    + `<td>${actionPill(rule.action)}</td>`
    + `<td class="num">${hitsCell(rule, state.hits, change)}</td>`
    + '<td class="rl-controls">'
    + iconButton('↑', { rl: 'up', index: String(i) }, `Move rule ${n} up`, reorder || i === 0)
    + iconButton('↓', { rl: 'down', index: String(i) }, `Move rule ${n} down`, reorder || last)
    + rowMenu(rule, i, n, { locked, reorder, last })
    + '</td></tr>';
}

/** The implicit last rule: what happens to a prompt no rule matches. */
const DEFAULT_ROW = '<tr class="rl-default">'
  + '<td class="rl-num" aria-label="Last">–</td><td></td>'
  + '<td class="rl-name-cell"><span class="rl-name">Default</span><span class="dp-sub">Every prompt no rule above matches</span></td>'
  + `<td>${ANY}</td><td>${ANY}</td><td>${ANY}</td>`
  + `<td>${actionPill('allow')}<span class="dp-sub">and logged</span></td><td></td><td></td></tr>`;

function unsavedBar(state) {
  if (state.draft === null) return '';
  const n = changeCount(state.draft, state.data.rules);
  const saving = state.pending === 'save';
  if (n === 0 && !saving) return '';
  return '<div class="rl-unsaved" role="status">'
    + `<span><strong>${saving ? 'Saving…' : `${plural(n, 'unsaved change', 'unsaved changes')}.`}</strong> Devices keep the saved rules until you save.</span>`
    + '<span class="rl-unsaved-actions">'
    + depButton('Discard', { rl: 'discard' }, { kind: 'quiet', small: true, disabled: saving })
    + depButton(saving ? 'Saving…' : 'Save changes', { rl: 'save' }, { kind: 'primary', small: true, disabled: saving || Boolean(state.panel) })
    + '</span></div>';
}

function hitsNote(state, { activity }) {
  const hits = state.hits;
  if (hits.problem) return `<p class="dp-note">The hits could not be read: ${escapeHtml(hits.problem.message ?? hits.problem.code ?? '')}</p>`;
  if (!hits.counts) return '';
  return '<p class="dp-note">Hits counts the prompts each rule decided in the last 30 days.'
    + (hits.truncated ? ' Not every rule\'s hits could be read, so some are lower than they are.' : '')
    + (activity ? ' <a href="#activity">Activity</a> splits them by what happened.' : '')
    + '</p>';
}

function listProblemLead(problem) {
  return ['too_many_rules', 'rules_not_reported'].includes(problem?.code) ? 'Not changed.' : 'Not saved.';
}

function rulesCard(state, options) {
  const data = state.data;
  const list = state.draft ?? data.rules;
  if (list === null) {
    return '<section class="card dp-card"><div class="card-core"><div class="dp-body dp-skels">'
      + '<p><span class="v-absent">not reported</span> The server did not send the rules, so they cannot be shown or changed. Reload the page to try again.</p>'
      + depProblem(state.listProblem, listProblemLead(state.listProblem))
      + '</div></div></section>';
  }
  const vocab = vocabulary(data);
  const names = data.names ?? {};
  const busy = Boolean(state.pending);
  const locked = busy || Boolean(state.panel);
  const filtering = state.filter !== '';
  const reorder = locked || filtering;
  const moved = movedRules(state.draft, data.rules);
  const shown = list.map((rule, i) => [rule, i]).filter(([rule]) => ruleMatchesQuery(rule, state.filter, (r) => describe(r, vocab, names)));
  const rows = shown.map(([rule, i]) => ruleRow(state, list, rule, i, { vocab, names, locked, reorder, moved })).join('');
  const empty = list.length === 0
    ? '<tr class="row-empty"><td colspan="9">No rules yet. Every prompt goes to the default below.</td></tr>'
    : shown.length === 0 ? `<tr class="row-empty"><td colspan="9">No rule matches “${escapeHtml(state.filter)}”.</td></tr>` : '';
  const filter = '<form class="rl-filter" data-rl-form="filter" role="search" autocomplete="off">'
    + `<input class="dp-input" id="rl-filter" type="search" maxlength="120" data-rl-query aria-label="Filter the rules" placeholder="Filter by name, person, group, tool or label" value="${escapeHtml(state.query)}">`
    + depButton('Filter', { rl: 'filter' }, { small: true })
    + '</form>';
  const count = filtering
    ? `<span class="table-note">${formatCount(shown.length)} of ${plural(list.length, 'rule', 'rules')}${shown.length < list.length ? ' · reordering is off while filtered' : ''}</span>`
    : `<span class="block-count">${formatCount(list.length)}</span>`;
  const head = '<thead><tr>'
    + '<th scope="col">#</th><th scope="col">On</th><th scope="col">Name</th><th scope="col">Users and groups</th>'
    + '<th scope="col">Tool</th><th scope="col">Content</th><th scope="col">Action</th><th scope="col" class="num" title="The prompts each rule decided in the last 30 days">Hits</th>'
    + '<th scope="col"><span class="sr">Order and edit</span></th></tr></thead>';
  return '<section class="card dp-card rl-card"><div class="card-core">'
    + `<header class="block-head"><h3>Rules</h3>${count}</header>`
    + '<div class="dp-body">'
    + `<div class="rl-toolbar">${filter}${depButton('+ Add rule', { rl: 'add' }, { kind: 'primary', small: true, disabled: locked || list.length >= MAX_RULES })}</div>`
    + `<div class="table-scroll dp-flush"><table class="rl-table">${head}<tbody>${rows}${empty}${filtering ? '' : DEFAULT_ROW}</tbody></table></div>`
    + hitsNote(state, options)
    + depProblem(state.listProblem, listProblemLead(state.listProblem))
    + '</div></div></section>';
}

const RULE_NOTES = Object.freeze([
  'The first rule that matches a prompt decides what happens to it; a prompt no rule matches is allowed and logged.',
  'A rule matches when every criterion it sets matches: one of the people, groups, departments, units or teams it names, one of its tools, and one of its labels. Any means the criterion is not checked.',
  'Groups, departments, units and teams are expanded to their members when devices fetch the rules, so a change in your directory reaches devices on their next policy poll. A prompt from someone the device cannot name never matches a rule that names people.',
  'A rule switched off keeps its place but is not sent to devices.',
  'Saved rules reach each device on its next policy poll. Every save is written to the audit trail with the old and the new list.',
]);

// ---------------------------------------------------------------------------------------------
// Custom labels
// ---------------------------------------------------------------------------------------------

const LABEL_FIELD_TITLES = Object.freeze({
  words: 'Words', customer_names: 'Customer names', file_extensions: 'File types', file_names: 'File names',
});
const LABEL_FIELD_HINTS = Object.freeze({
  words: 'One word or phrase per line. Matched as whole words, whatever their case.',
  customer_names: 'One name per line. Matched as whole words, whatever their case.',
  file_extensions: 'Separated by commas, e.g. pdf, dwg, tar.gz.',
  file_names: 'One per line; * stands for any characters and ? for one, e.g. Q3 forecast*.xlsx.',
});

function labelEditor(state) {
  const editor = state.labels.editor;
  const busy = Boolean(state.labels.pending);
  const off = busy ? ' disabled' : '';
  const lists = CUSTOM_LABEL_FIELDS.map((f) => `<label class="dp-label" for="rl-label-${f}">${escapeHtml(LABEL_FIELD_TITLES[f])}</label>`
    + `<textarea class="dp-input" id="rl-label-${f}" data-rl-label-draft="${f}" rows="${f === 'file_extensions' ? 1 : 3}" spellcheck="false"${off}>${escapeHtml(editor[f])}</textarea>`
    + `<span class="dp-sub">${escapeHtml(LABEL_FIELD_HINTS[f])}</span>`).join('');
  return `<div class="dp-form" data-label-editor="${escapeHtml(editor.label ?? 'new')}">`
    + `<p class="dp-note"><strong>${editor.label === null ? 'New label' : escapeHtml(editor.display_name || editor.label)}</strong></p>`
    + '<label class="dp-label" for="rl-label-display_name">Name</label>'
    + `<input class="dp-input" id="rl-label-display_name" data-rl-label-draft="display_name" value="${escapeHtml(editor.display_name)}" maxlength="80" placeholder="Project Falcon" autocomplete="off"${off}>`
    + lists
    + '<div class="dp-actions">'
    + depButton(busy ? 'Saving…' : 'Save label', { rl: 'label-save' }, { kind: 'primary', small: true, disabled: busy })
    + depButton('Cancel', { rl: 'label-cancel' }, { kind: 'quiet', small: true, disabled: busy })
    + '</div>'
    + depProblem(editor.problem, 'Not saved.')
    + '</div>';
}

function labelTerms(values) {
  return values.length ? values.map((v) => `<span class="v-text">${escapeHtml(v)}</span>`).join(', ') : '<span class="v-absent">none</span>';
}

/** The rules that name a label, saved or not, by number. */
function labelUsers(code, rules) {
  const using = (rules ?? []).map((r, i) => [r, i]).filter(([r]) => r.match.labels.includes(code));
  return using.length ? using.map(([r, i]) => `<span class="v-text" title="${escapeHtml(r.rule_id)}">Rule ${i + 1}</span>`).join(', ') : '<span class="v-absent">none</span>';
}

function labelsCard(state) {
  const list = state.data.custom_labels;
  if (list === null) {
    return '<section class="card dp-card"><div class="card-core"><div class="dp-body dp-skels">'
      + '<p><span class="v-absent">not reported</span> The server did not send the custom labels.</p>'
      + depProblem(state.labels.problem, 'Not changed.')
      + '</div></div></section>';
  }
  const busy = Boolean(state.labels.pending);
  const locked = busy || Boolean(state.labels.editor);
  const rules = state.draft ?? state.data.rules;
  const rows = list.map((l) => '<tr>'
    + `<td><span class="v-text">${escapeHtml(l.display_name)}</span><span class="dp-sub"><code>${escapeHtml(l.label)}</code></span></td>`
    + CUSTOM_LABEL_FIELDS.map((f) => `<td>${labelTerms(l[f])}</td>`).join('')
    + `<td>${labelUsers(l.label, rules)}</td>`
    + '<td class="dp-action">'
    + depButton('Edit', { rl: 'label-edit', label: l.label }, { kind: 'quiet', small: true, disabled: locked })
    + depButton(state.labels.pending === l.label ? 'Deleting…' : 'Delete', { rl: 'label-delete', label: l.label }, { kind: 'quiet', small: true, disabled: locked })
    + '</td></tr>').join('');
  const empty = list.length === 0 ? '<tr class="row-empty"><td colspan="7">No custom labels.</td></tr>' : '';
  const body = '<p>Your own labels, for the words, customer names, file types and file names that matter to your organisation. A device labels a prompt with one when the prompt\'s text holds one of its words or customer names, or an attached file has one of its types or names; a rule can then name it under Content like any built-in label. A saved label reaches each device on its next policy poll.</p>'
    + '<div class="table-scroll dp-flush"><table><thead><tr><th scope="col">Label</th>'
    + CUSTOM_LABEL_FIELDS.map((f) => `<th scope="col">${escapeHtml(LABEL_FIELD_TITLES[f])}</th>`).join('')
    + '<th scope="col">Used by</th><th scope="col"><span class="sr">Edit</span></th></tr></thead>'
    + `<tbody>${rows}${empty}</tbody></table></div>`
    + (state.labels.editor ? labelEditor(state) : '')
    + '<div class="dp-actions">'
    + depButton('Add label', { rl: 'label-add' }, { small: true, disabled: locked || list.length >= MAX_CUSTOM_LABELS })
    + '</div>'
    + (busy && !state.labels.editor ? '<p class="dp-note" role="status">Saving…</p>' : '')
    + depProblem(state.labels.problem, 'Not changed.');
  return '<section class="card dp-card"><div class="card-core">'
    + `<header class="block-head"><h3>Custom labels</h3><span class="block-count">${formatCount(list.length)}</span></header>`
    + `<div class="dp-body">${body}</div></div></section>`;
}

// ---------------------------------------------------------------------------------------------
// The page
// ---------------------------------------------------------------------------------------------

/** What each page says under its tabs. */
const LEDES = Object.freeze({
  rules: 'What happens to a prompt, by who sends it, the tool it goes to and what it holds. The first rule that matches decides.',
  labels: 'The labels of your own that a rule can match on, beside the built-in data classes.',
});

function skeleton() {
  const lines = (widths) => widths.map((w) => `<span class="dp-skel" style="width:${w}%"></span>`).join('');
  return '<p class="sr" role="status">Loading the rules…</p>'
    + `<div aria-hidden="true"><section class="card dp-card"><div class="card-core"><div class="dp-body dp-skels">${lines([30, 92, 88, 84, 61])}</div></div></section></div>`;
}

function refused(problem) {
  const status = problem?.status;
  const title = status === 401 ? 'Your session has ended'
    : status === 403 ? 'Only an admin can open the rules'
      : status === 0 ? 'The admin API could not be reached'
        : 'The rules could not be read';
  const text = status === 401 ? 'Sign in again to continue.'
    : status === 403 ? 'Your role does not include the rules. Ask an admin of your organisation.'
      : problem?.message ?? 'Nothing was read.';
  const action = status === 401 ? '<a class="btn" href="/signin">Sign in again</a>' : depButton('Try again', { rl: 'retry' });
  return '<section class="card dp-card"><div class="card-core dp-state" role="alert">'
    + `<h3>${escapeHtml(title)}</h3><p>${escapeHtml(text)}</p>`
    + (problem?.code ? `<p class="dp-code">${escapeHtml(problem.code)}${status ? ` · ${escapeHtml(String(status))}` : ''}</p>` : '')
    + `<div class="dp-actions">${action}</div>`
    + '</div></section>';
}

/**
 * The whole page for one state.
 *
 * @param {object} state  createRules().state
 * @param {object} [options]
 * @param {'rules'|'labels'} [options.tab]
 * @param {object} [options.head]       the page's title, eyebrow and tabs
 * @param {boolean} [options.activity]  whether Activity is offered, for the link that breaks the hits down
 */
export function renderRules(state, { tab = 'rules', head = {}, activity = false } = {}) {
  const shown = LEDES[tab] ? tab : 'rules';
  const top = renderPageHead({ title: 'Policy', ...head, subtitle: LEDES[shown] });
  if (state.status === 'idle' || state.status === 'loading') return `<article class="screen dp rl" aria-busy="true">${top}${skeleton()}</article>`;
  if (state.status === 'refused' || !state.data) return `<article class="screen dp rl">${top}${refused(state.problem)}</article>`;
  const refresh = state.refreshProblem
    ? `<div class="banner banner-warning" role="status"><strong>Not refreshed</strong><span>${escapeHtml(state.refreshProblem.message ?? '')} What is shown is from the last read.</span></div>`
    : '';
  if (shown === 'labels') return `<article class="screen dp rl">${top}${refresh}${unsavedBar(state)}${labelsCard(state)}</article>`;
  const notes = `<details class="notes"><summary>How rules work<span class="notes-count">${RULE_NOTES.length}</span></summary><ul>${RULE_NOTES.map((n) => `<li>${escapeHtml(n)}</li>`).join('')}</ul></details>`;
  return `<article class="screen dp rl">${top}${refresh}${unsavedBar(state)}${rulesCard(state, { activity })}${notes}</article>`;
}

// ---------------------------------------------------------------------------------------------
// The side panel: one rule
// ---------------------------------------------------------------------------------------------

/** The options a match list offers, as [value, label]: the known set, each once, plus any value the rule already holds. */
function matchOptions(field, data, current) {
  const known = {
    labels: () => [...(data.data_classes ?? []).map((c) => [c, c]), ...(data.custom_labels ?? []).map((l) => [l.label, l.display_name])],
    tools: () => [...catalogueTools(data)],
    categories: () => {
      const present = data.app_categories ?? [];
      return [...RULE_CATEGORIES.filter((c) => present.includes(c.key)).map((c) => [c.key, c.label]),
        ...present.filter((c) => !(c in CATEGORY_LABELS)).map((c) => [c, c])];
    },
    sanction: () => RULE_SANCTIONS.map((s) => [s, SANCTION_LABELS[s]]),
    routes: () => RULE_ROUTES.map((r) => [r.key, r.label]),
  }[field]();
  const out = new Map();
  for (const [value, label] of known) if (!out.has(value)) out.set(value, label);
  for (const value of current) if (!out.has(value)) out.set(value, value);
  return [...out];
}

/** Why a match list offers nothing: the server did not send its choices, or there are none yet. */
function noOptions(field, data) {
  const unread = { labels: data.data_classes === null && data.custom_labels === null, tools: data.tools === null, categories: data.app_categories === null }[field];
  return `<span class="v-absent">${unread ? 'not reported' : 'none known yet'}</span>`;
}

function chips(field, title, data, rule) {
  const current = rule.match[field];
  const options = matchOptions(field, data, current);
  const body = options.length === 0 ? noOptions(field, data)
    : `<div class="rl-chips" role="group" aria-label="${escapeHtml(title)}">`
      + options.map(([value, label]) => `<button type="button" class="rl-chip" aria-pressed="${current.includes(value)}" data-rl="match" data-field="${field}" data-value="${escapeHtml(value)}">${escapeHtml(label)}</button>`).join('')
      + '</div>';
  return `<div class="rl-field"><span class="dp-label">${escapeHtml(title)}${current.length ? '' : ' <span class="dp-optional">Any</span>'}</span>${body}</div>`;
}

function section(title, body, hint = '') {
  return `<section class="rl-sec"><h4 class="rl-sec-title">${escapeHtml(title)}</h4>${hint ? `<p class="dp-note">${escapeHtml(hint)}</p>` : ''}${body}</section>`;
}

function pickDetail(item) {
  if (item.kind === 'user') return item.detail;
  const count = item.members === null ? '' : item.kind === 'group' || item.kind === 'team' ? plural(item.members, 'member', 'members') : plural(item.members, 'person', 'people');
  return [count, item.detail].filter(Boolean).join(' · ');
}

function pickEmpty(picker) {
  if (picker.term) return picker.filter === 'user' ? `Nobody's name starts with “${picker.term}”.` : `Nothing matches “${picker.term}”.`;
  return 'Nothing to list yet. People appear once a device reports or your identity provider syncs them.';
}

function pickerHtml(panel) {
  const picker = panel.picker;
  const lists = [picker.people, picker.groups, picker.departments, picker.units, picker.teams];
  const loading = lists.some((l) => l.pending);
  const chosen = new Set(panel.rule.users);
  const items = pickerItems(picker);
  const filter = '<div class="seg dp-seg pick-filter" role="group" aria-label="Show">'
    + Object.entries(PICK_FILTERS).map(([key, label]) => `<button type="button" class="seg-item" data-rl="pick-filter" data-value="${key}" aria-pressed="${picker.filter === key}">${escapeHtml(label)}</button>`).join('')
    + '</div>';
  const search = '<form class="pick-search" data-rl-form="pick-search" role="search" autocomplete="off">'
    + `<input class="dp-input" id="rl-pick-query" type="search" maxlength="120" data-rl-draft="query" aria-label="Search" placeholder="Search people, groups, departments, units and teams" value="${escapeHtml(picker.query)}">`
    + depButton(picker.people.pending ? 'Searching…' : 'Search', { rl: 'pick-search' }, { disabled: picker.people.pending })
    + '</form>';
  const rows = items.map((item) => {
    const detail = pickDetail(item);
    return `<li class="pick-row"><label class="pick-label">`
      + `<input type="checkbox" class="pick-box" data-rl-pick="${escapeHtml(item.value)}"${chosen.has(item.value) ? ' checked' : ''}>`
      + `<span class="pick-name">${escapeHtml(item.name)}</span><span class="pick-type">${escapeHtml(USER_KIND_LABELS[item.kind] ?? item.kind)}</span>`
      + (detail ? `<span class="pick-detail">${escapeHtml(detail)}</span>` : '')
      + '</label></li>';
  }).join('');
  const empty = items.length === 0 ? `<li class="pick-empty">${loading ? 'Reading…' : escapeHtml(pickEmpty(picker))}</li>` : '';
  const more = (picker.filter === 'all' || picker.filter === 'user') && picker.people.next
    ? `<div class="pick-more">${depButton(picker.people.pending ? 'Reading…' : 'Show more people', { rl: 'pick-more' }, { small: true, disabled: picker.people.pending })}</div>`
    : '';
  const problems = lists.map((l) => l.problem).filter(Boolean).map((p) => depProblem(p, 'Not all of the list could be read.')).join('');
  return `<div class="pick rl-pick">${filter}${search}`
    + `<ul class="pick-list" id="rl-pick-list" aria-label="People, groups, departments, units and teams" aria-busy="${loading}">${rows}${empty}</ul>`
    + more + problems
    + `<div class="dp-actions">${depButton('Done', { rl: 'pick-close' }, { small: true })}</div></div>`;
}

function usersSection(panel, names) {
  const users = panel.rule.users;
  const chosen = users.length === 0 ? `<p class="rl-chosen">${ANY}<span class="dp-note">Everyone, including a prompt the device cannot name a person for.</span></p>`
    : '<ul class="rl-chosen-list">' + users.map((value) => {
      const who = userOf(value, names);
      return `<li>${userTag(value, names)}<button type="button" class="rl-remove" data-rl="user" data-value="${escapeHtml(value)}" aria-label="Take out ${escapeHtml(who.name || value)}">×</button></li>`;
    }).join('') + '</ul>';
  const actions = panel.picker.open ? pickerHtml(panel)
    : '<div class="dp-actions">'
      + depButton(users.length ? 'Add people and groups' : 'Choose people and groups', { rl: 'pick-open' }, { small: true })
      + (users.length ? depButton('Any', { rl: 'users-any' }, { kind: 'quiet', small: true }) : '')
      + '</div>';
  return section('Users and groups', chosen + actions, 'People, identity provider groups, departments, units and teams. Groups and teams bring their members as your directory changes them.');
}

/** What the person sees when the rule decides their prompt, as the agent shows it. */
function seenNote(action) {
  switch (action) {
    case 'block': return 'The request is cancelled, and the person sees “Request blocked” with your message, the link and Dismiss.';
    case 'redirect': return 'A person opening the web app in the browser sees the Sundial page with your message and a button to your link. A desktop app or command-line tool has no page to show, so there the prompt is blocked with your message and link.';
    case 'warn': return 'The request is held, and the person sees “Request held for confirmation” with your message, the link, Send anyway and Cancel request.';
    default: return 'The prompt is sent and recorded under this rule\'s name; the person sees nothing.';
  }
}

function positionField(state, panel) {
  if (panel.index !== null) return '';
  const list = state.draft ?? state.data.rules ?? [];
  const at = panel.at;
  const options = [['bottom', 'At the bottom, above the default']]
    .concat(list.map((r, i) => [String(i), i === 0 ? `At the top, above rule 1 (${r.rule_id})` : `Above rule ${i + 1} (${r.rule_id})`]));
  const current = Number.isInteger(at) ? String(at) : 'bottom';
  return section('Position', '<label class="dp-label sr" for="rl-at">Position</label>'
    + `<select class="dp-input dp-select" id="rl-at" data-rl="at">${options.map(([v, label]) => `<option value="${v}"${v === current ? ' selected' : ''}>${escapeHtml(label)}</option>`).join('')}</select>`,
  'The first rule that matches decides, so a narrower rule goes above a broader one: an Allow for one team above a Block for everyone.');
}

/** The side panel for one state, as the contents of its dialog element. Empty while no rule is open. */
export function renderRuleDialog(state) {
  const panel = state.panel;
  if (!panel || !state.data) return '';
  const { rule } = panel;
  const data = state.data;
  const names = { ...(data.names ?? {}), ...panel.names };
  const field = (name, label, value, attrs = '', { hidden = false } = {}) => `<label class="dp-label${hidden ? ' sr' : ''}" for="rl-${name}">${escapeHtml(label)}</label>`
    + `<input class="dp-input" id="rl-${name}" data-rl-draft="${name}" value="${escapeHtml(value)}" autocomplete="off"${attrs}>`;
  const onOff = ['on', 'off'].map((v) => `<button type="button" class="seg-item" data-rl="enabled" data-value="${v}" aria-pressed="${(v === 'on') === rule.enabled}">${v === 'on' ? 'On' : 'Off'}</button>`).join('');
  const name = section('Name', field('rule_id', 'Name', rule.rule_id, ' placeholder="block_credentials" maxlength="128" spellcheck="false"', { hidden: true })
    + '<span class="dp-sub">Recorded on every prompt the rule decides: lower-case letters, digits, "_", "." and "-".</span>'
    + `<div class="seg dp-seg" role="group" aria-label="On or off">${onOff}</div>`);
  const tool = section('Tool', chips('tools', 'Tools', data, rule) + chips('categories', 'Categories', data, rule) + chips('sanction', 'Sanction', data, rule));
  const content = section('Content', chips('labels', 'Labels', data, rule), 'Built-in data classes and your custom labels, which are made on the Labels tab.');
  const routes = `<details class="rl-advanced"${rule.match.routes.length ? ' open' : ''}><summary>Advanced: collection routes</summary>`
    + chips('routes', 'Routes', data, rule) + '</details>';
  const actions = RULE_ACTIONS.map((a) => `<button type="button" class="seg-item" data-rl="action" data-value="${a}" aria-pressed="${rule.action === a}">${escapeHtml(RULE_ACTION_LABELS[a])}</button>`).join('');
  const action = section('Action', `<div class="seg dp-seg" role="group" aria-label="Action">${actions}</div>`
    + `<p class="dp-note">${escapeHtml(seenNote(rule.action))}</p>`
    + field('message', rule.action === 'allow' ? 'Message (optional)' : 'Message', rule.message, ` maxlength="${MAX_RULE_MESSAGE}" placeholder="Shown to the person; needed for warn, block and redirect"`)
    + field('link', rule.action === 'redirect' ? 'Link' : 'Link (optional)', rule.link, ' placeholder="https://" spellcheck="false"'));
  const n = panel.index === null ? null : panel.index + 1;
  return '<div class="dialog-panel" tabindex="-1">'
    + '<header class="dialog-head"><div>'
    + `<span class="eyebrow">${n === null ? 'New rule' : `Rule ${n}`}</span>`
    + `<h3 id="rule-dialog-title">${n === null ? 'Add a rule' : 'Edit rule'}</h3>`
    + `<p class="dialog-lead rl-sentence">${escapeHtml(ruleSentence(rule, data, names))}</p>`
    + '</div><button type="button" class="dialog-x" data-rl="cancel" aria-label="Close">×</button></header>'
    + `<div class="dialog-body">${name}${usersSection(panel, names)}${tool}${content}${routes}${action}${positionField(state, panel)}</div>`
    + '<footer class="dialog-foot">'
    + '<p class="dialog-summary">A kept rule is saved with the rest of the list by Save changes; devices get nothing until then.</p>'
    + depProblem(panel.problem, 'Not kept.')
    + `<div class="dialog-actions"><span class="dialog-start"></span>${depButton('Cancel', { rl: 'cancel' }, { kind: 'quiet' })}${depButton(n === null ? 'Add rule' : 'Keep changes', { rl: 'apply' }, { kind: 'primary' })}</div>`
    + '</footer></div>';
}
