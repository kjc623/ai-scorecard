// settings-render.js — Settings → Settings, state to HTML. Pure: no DOM, no I/O, no state.
//
// The same honesty rules as every other screen: a value the server did not send is "not reported",
// never a zero; every string from the API is escaped; a device's applied mode is shown as it
// reported it, not as the tenant requested it. The one confirmation on the page is a mode increase
// that starts capturing content.

import { escapeHtml } from './render.js';
import { formatInstant } from './format.js';
import {
  COLLECTION_MODES, SEARCH_TIERS, SANCTION_STATES, ENDPOINT_COLLECTORS, ENDPOINT_TOOLS, effectiveMode, modeIncreaseNeedsConfirmation, searchTierAllowed,
  RULE_ACTIONS, RULE_CATEGORIES, RULE_SANCTIONS, RULE_ROUTES, RULE_MATCH_FIELDS, MAX_RULES, MAX_RULE_MESSAGE, KILL_SWITCH_ROUTES,
} from './settings.js';

const MODE_LABELS = Object.freeze({ m0: 'M0 · metadata', m1: 'M1 · digest & labels', m2: 'M2 · excerpt', m3: 'M3 · prompt' });
const SEARCH_LABELS = Object.freeze({ disabled: 'Off', attachment_names: 'Attachment names', full_text: 'Full text' });

function stInstant(iso, absent = 'not reported') {
  return iso ? `<span class="v-time" title="${escapeHtml(iso)}">${escapeHtml(formatInstant(iso))}</span>` : `<span class="v-absent">${escapeHtml(absent)}</span>`;
}

function stChip(value) {
  const text = String(value ?? 'unknown');
  return `<span class="v-vocab v-vocab-${escapeHtml(text.replace(/[^a-z_]/gi, ''))}">${escapeHtml(text)}</span>`;
}

function stProblem(problem, lead = 'Not changed.') {
  if (!problem) return '';
  return `<p class="dp-problem" role="alert"><strong>${escapeHtml(lead)}</strong> ${escapeHtml(problem.message ?? '')}`
    + (problem.code ? ` <code>${escapeHtml(problem.code)}</code>` : '') + '</p>';
}

function stButton(label, data, { kind = '', disabled = false, small = false } = {}) {
  const attrs = Object.entries(data).map(([k, v]) => ` data-${k}="${escapeHtml(v)}"`).join('');
  const cls = ['btn', kind ? `btn-${kind}` : '', small ? 'btn-small' : ''].filter(Boolean).join(' ');
  return `<button type="button" class="${cls}"${attrs}${disabled ? ' disabled' : ''}>${escapeHtml(label)}</button>`;
}

function stCard(title, body, { extra = '', wide = false } = {}) {
  return `<section class="card dp-card${wide ? ' dp-wide' : ''}"><div class="card-core">`
    + `<header class="block-head"><h3>${escapeHtml(title)}</h3>${extra}</header>`
    + `<div class="dp-body">${body}</div></div></section>`;
}

function stHeader(eyebrow) {
  return '<header class="screen-head"><div class="screen-title">'
    + (eyebrow ? `<span class="eyebrow">${escapeHtml(eyebrow)}</span>` : '')
    + '<h2>Settings</h2>'
    + '<p class="question">What your organisation collects and keeps, and what search can reach. Every change is written to the audit trail.</p>'
    + '</div></header>';
}

function stSkeleton() {
  const lines = (widths) => widths.map((w) => `<span class="dp-skel" style="width:${w}%"></span>`).join('');
  const card = (widths) => `<section class="card dp-card"><div class="card-core"><div class="dp-body dp-skels">${lines(widths)}</div></div></section>`;
  return `<p class="sr" role="status">Loading the settings…</p><div class="dp-grid" aria-hidden="true">${card([36, 78, 64])}${card([44, 70, 58])}</div>`
    + `<div aria-hidden="true">${card([30, 92, 84, 61])}</div>`;
}

function stRefused(problem) {
  const status = problem?.status;
  const title = status === 401 ? 'Your session has ended'
    : status === 403 ? 'Only an admin can open the settings'
      : status === 0 ? 'The admin API could not be reached'
        : 'The settings could not be read';
  const text = status === 401 ? 'Sign in again to continue.'
    : status === 403 ? 'Your role does not include settings. Ask an admin of your organisation.'
      : problem?.message ?? 'Nothing was read.';
  const action = status === 401 ? '<a class="btn" href="/signin">Sign in again</a>' : stButton('Try again', { action: 'retry' });
  return '<section class="card dp-card"><div class="card-core dp-state" role="alert">'
    + `<h3>${escapeHtml(title)}</h3><p>${escapeHtml(text)}</p>`
    + (problem?.code ? `<p class="dp-code">${escapeHtml(problem.code)}${status ? ` · ${escapeHtml(String(status))}` : ''}</p>` : '')
    + (action ? `<div class="dp-actions">${action}</div>` : '')
    + '</div></section>';
}

function segItem(label, value, current, data, { disabled = false } = {}) {
  return `<button type="button" class="seg-item" aria-pressed="${current === value}"${Object.entries(data).map(([k, v]) => ` data-${k}="${escapeHtml(v)}"`).join('')}${disabled ? ' disabled' : ''}>${escapeHtml(label)}</button>`;
}

function modeSegment(current, disabled, extraData) {
  const data = { action: 'mode', ...extraData };
  return COLLECTION_MODES.map((m) => segItem(MODE_LABELS[m], m, current, { ...data, value: m }, { disabled })).join('');
}

function stCollectionMode(state) {
  const data = state.data;
  const current = effectiveMode(data);
  const ceiling = data.ceiling_mode ?? 'm0';
  const followingCeiling = !data.collection_mode;
  const pending = state.mode.pending;
  const sentence = followingCeiling
    ? `Following the ceiling (${ceiling}). Choose a mode to request less than the ceiling allows.`
    : `Requested for every device unless a tool override below narrows it further. The ceiling is ${ceiling}.`;
  const confirm = state.mode.confirm
    ? `<div class="dp-confirm">This starts capturing content. ${escapeHtml(MODE_LABELS[state.mode.confirm])} reads the prompt; confirm to proceed? `
      + stButton('Confirm', { action: 'mode-confirm' }, { kind: 'primary', small: true })
      + stButton('Cancel', { action: 'mode-cancel' }, { kind: 'quiet', small: true }) + '</div>'
    : '';
  return stCard('Collection mode', `<div class="seg dp-seg" role="group" aria-label="Collection mode">${modeSegment(current, Boolean(pending) || Boolean(state.mode.confirm))}</div>`
    + `<p class="dp-note"${pending ? ' role="status"' : ''}>${pending ? 'Saving…' : escapeHtml(sentence)}</p>`
    + confirm
    + stProblem(state.mode.problem));
}

function stOverrideSelect(tool, state) {
  const current = state.data.scope_overrides[tool.tool_key] ?? '';
  const disabled = Boolean(state.override.pending);
  const options = ['<option value="">Default</option>']
    .concat(COLLECTION_MODES.map((m) => `<option value="${m}"${m === current ? ' selected' : ''}>${escapeHtml(MODE_LABELS[m])}</option>`))
    .join('');
  return `<select class="dp-input dp-select" data-action="override" data-tool="${escapeHtml(tool.tool_key)}" aria-label="Mode for ${escapeHtml(tool.display_name ?? tool.tool_key)}"${disabled ? ' disabled' : ''}>${options}</select>`;
}

/** One row per tool: the override covers every fingerprint the catalogue knows for it. */
function stOverrides(state) {
  const tools = state.data.tools;
  const rows = tools.map((tool) => '<tr>'
    + `<td>${toolCell(tool)}</td>`
    + `<td class="dp-action">${stOverrideSelect(tool, state)}</td></tr>`).join('');
  const empty = tools.length === 0 ? '<tr class="row-empty"><td colspan="2">No tools are catalogued yet.</td></tr>' : '';
  const body = '<p>Narrow collection for one tool below the tenant\'s mode. An override can only reduce what is collected, never raise it, and covers every fingerprint of the tool.</p>'
    + '<div class="table-scroll dp-flush"><table><thead><tr><th scope="col">Tool</th><th scope="col">Mode</th></tr></thead>'
    + `<tbody>${rows}${empty}</tbody></table></div>`
    + stProblem(state.override.problem);
  return stCard('Per-tool overrides', body, { wide: true });
}

function stRetention(state) {
  const data = state.data;
  const field = (appliesTo, current, fallback, label) => {
    const draft = state.retention.drafts[appliesTo] ?? '';
    const shown = draft !== '' ? draft : (current ?? '');
    return '<form class="dp-form dp-inline" data-settings-form="retention" autocomplete="off">'
      + `<label class="dp-label" for="st-${appliesTo}">${escapeHtml(label)}</label>`
      + '<div class="dp-inline-row">'
      + `<input class="dp-input dp-num" id="st-${appliesTo}" inputmode="numeric" maxlength="4" data-settings-draft="${appliesTo}" placeholder="${escapeHtml(String(current ?? fallback))}" value="${escapeHtml(shown)}">`
      + '<span class="dp-sub">days</span>'
      + stButton(state.retention.pending === appliesTo ? 'Saving…' : 'Save', { action: 'save-retention', appliesTo }, { disabled: Boolean(state.retention.pending) })
      + '</div></form>';
  };
  const eventNote = `Default ${data.retention_defaults?.event_days ?? '—'} days when not set.`;
  const contentNote = `Default ${data.retention_defaults?.content_days ?? '—'} days when not set.`;
  const body = field('event', data.event_retention_days, data.retention_defaults?.event_days, 'Event retention')
    + `<p class="dp-note">${escapeHtml(eventNote)}</p>`
    + field('content', data.content_retention_days, data.retention_defaults?.content_days, 'Content retention')
    + `<p class="dp-note">${escapeHtml(contentNote)}</p>`
    + stProblem(state.retention.problem);
  return stCard('Retention', body);
}

/** A tool's name with how many fingerprints the catalogue knows for it: each way a device recognises it. */
function toolCell(tool) {
  const n = Array.isArray(tool.fingerprints) ? tool.fingerprints.length : 0;
  return `<span class="v-text">${escapeHtml(tool.display_name ?? tool.tool_key)}</span> <span class="dp-sub">${n} ${n === 1 ? 'fingerprint' : 'fingerprints'}</span>`;
}

/** One row per tool: the decision covers every fingerprint the catalogue knows for it. */
function stTools(state) {
  const tools = state.data.tools;
  const rows = tools.map((tool) => {
    const current = tool.sanctioned_state;
    const name = tool.display_name ?? tool.tool_key;
    const seg = SANCTION_STATES.map((s) => segItem(s, s, current, { action: 'sanction', tool: tool.tool_key, value: s }, { disabled: Boolean(state.sanction.pending) })).join('');
    return '<tr>'
      + `<td>${toolCell(tool)}</td>`
      + `<td class="dp-action"><div class="seg dp-seg" role="group" aria-label="Sanction for ${escapeHtml(name)}">${seg}</div></td></tr>`;
  }).join('');
  const empty = tools.length === 0 ? '<tr class="row-empty"><td colspan="2">No tools are catalogued yet.</td></tr>' : '';
  const body = '<p>Mark a tool sanctioned or unsanctioned. A decision covers every fingerprint of the tool: each way a device recognises it. A tool with no decision is <em>unknown</em>, never unsanctioned.</p>'
    + '<div class="table-scroll dp-flush"><table><thead><tr><th scope="col">Tool</th><th scope="col">Decision</th></tr></thead>'
    + `<tbody>${rows}${empty}</tbody></table></div>`
    + stProblem(state.sanction.problem);
  return stCard('Tool sanction', body, { wide: true });
}

function stSearch(state) {
  const data = state.data;
  const current = data.content_search ?? 'disabled';
  const ceiling = data.ceiling_mode ?? 'm0';
  const pending = state.search.pending;
  const seg = SEARCH_TIERS.map((t) => {
    const allowed = searchTierAllowed(t, ceiling);
    return segItem(SEARCH_LABELS[t], t, current, { action: 'search', value: t }, { disabled: Boolean(pending) || !allowed });
  }).join('');
  const reason = ceiling !== 'm3' && ceiling !== 'm0' ? 'Full text needs an M3 ceiling.' : ceiling === 'm0' ? 'Search over names and text needs content the M0 ceiling never collects.' : '';
  return stCard('Content search', `<div class="seg dp-seg" role="group" aria-label="Content search tier">${seg}</div>`
    + `<p class="dp-note"${pending ? ' role="status"' : ''}>${pending ? 'Saving…' : escapeHtml(reason)}</p>`
    + stProblem(state.search.problem));
}

const ENDPOINT_LABELS = Object.freeze({
  inventory: 'Inventory',
  processes: 'Processes',
  flows: 'Network flows',
  otel: 'OpenTelemetry',
  hooks: 'Hooks',
  hooks_managed_only: 'Only managed hooks',
});

/** An On/Off pair for one switch; a value the server did not send selects neither. */
function onOffSegment(label, current, data, disabled) {
  const on = current === true ? 'on' : current === false ? 'off' : null;
  return `<div class="seg dp-seg" role="group" aria-label="${escapeHtml(label)}">`
    + segItem('On', 'on', on, { ...data, value: 'on' }, { disabled })
    + segItem('Off', 'off', on, { ...data, value: 'off' }, { disabled })
    + '</div>';
}

function stEndpoint(state) {
  const endpoint = state.data.endpoint;
  if (!endpoint) {
    return stCard('Endpoint collectors', '<p><span class="v-absent">not reported</span> The server did not send the endpoint settings.</p>', { wide: true });
  }
  const pending = state.endpoint.pending;
  const busy = Boolean(pending);
  const collectorRows = ENDPOINT_COLLECTORS.map((name) => {
    const label = ENDPOINT_LABELS[name];
    const current = endpoint[name];
    return '<tr>'
      + `<td><span class="v-text">${escapeHtml(label)}</span></td>`
      + `<td class="dp-action">${current === null ? '<span class="v-absent">not reported</span>' : onOffSegment(label, current, { action: 'endpoint', collector: name }, busy)}</td></tr>`;
  }).join('');
  const cell = (tool, collector, label) => {
    if (!tool[collector]) return '<td><span class="v-absent">unavailable</span></td>';
    const current = endpoint.tools[tool.key]?.[collector] ?? null;
    if (current === null) return '<td><span class="v-absent">not reported</span></td>';
    return `<td class="dp-action">${onOffSegment(`${label} for ${tool.label}`, current, { action: 'endpoint-tool', tool: tool.key, collector }, busy)}</td>`;
  };
  const toolRows = ENDPOINT_TOOLS.map((tool) => '<tr>'
    + `<td><span class="v-text">${escapeHtml(tool.label)}</span></td>`
    + cell(tool, 'otel', 'OpenTelemetry') + cell(tool, 'hooks', 'Hooks') + cell(tool, 'loopback', 'Local model capture') + '</tr>').join('');
  const body = '<p>Which collectors run on the devices. A change reaches each device on its next policy poll, without a restart. With <em>Only managed hooks</em> on, tools run only the hooks the agent manages, not a user\'s own.</p>'
    + '<div class="table-scroll dp-flush"><table><thead><tr><th scope="col">Collector</th><th scope="col">State</th></tr></thead>'
    + `<tbody>${collectorRows}</tbody></table></div>`
    + '<p>Per tool. A tool\'s switch takes effect only while the collector above is on; <em>unavailable</em> is a collector the tool does not have.</p>'
    + '<div class="table-scroll dp-flush"><table><thead><tr><th scope="col">Tool</th><th scope="col">OpenTelemetry</th><th scope="col">Hooks</th><th scope="col">Local model capture</th></tr></thead>'
    + `<tbody>${toolRows}</tbody></table></div>`
    + '<p>While <em>Local model capture</em> is on, the device moves Ollama to another port and takes Ollama\'s own port, so it records the prompts sent to Ollama.</p>'
    + (busy ? '<p class="dp-note" role="status">Saving…</p>' : '')
    + stProblem(state.endpoint.problem);
  return stCard('Endpoint collectors', body, { wide: true });
}

function stTLSInspection(state) {
  const current = state.data.tls_inspection;
  const busy = Boolean(state.tls.pending);
  const control = current === null
    ? '<p><span class="v-absent">not reported</span> The server did not send the TLS inspection setting.</p>'
    : onOffSegment('TLS inspection', current, { action: 'tls-inspection' }, busy);
  const body = control
    + '<p class="dp-note">Turning it on installs each device\'s own root certificate in its trust store, runs a local proxy that decrypts traffic to the catalogued AI services, points command-line tools at it and, on Windows, routes desktop apps through it for every signed-in user.</p>'
    + (busy ? '<p class="dp-note" role="status">Saving…</p>' : '')
    + stProblem(state.tls.problem);
  return stCard('TLS inspection', body);
}

function stKillSwitch(state) {
  const switches = state.data.kill_switches;
  if (switches === null) {
    return stCard('Kill switch', '<p><span class="v-absent">not reported</span> The server did not send the kill switches.</p>'
      + stProblem(state.killSwitch.problem));
  }
  const busy = Boolean(state.killSwitch.pending);
  const rows = KILL_SWITCH_ROUTES.map((route) => {
    const tripped = switches.find((k) => k.route === route.key);
    const id = `st-kill-${route.key.replace(/[^a-z]/g, '-')}`;
    const reason = state.killSwitch.reasons[route.key] ?? '';
    const status = tripped
      ? `${stChip('tripped')}<span class="dp-sub">since ${stInstant(tripped.effective_at)}</span>`
        + `<span class="dp-sub"><code>${escapeHtml(tripped.reason_code ?? '')}</code>${tripped.set_by ? ` by ${escapeHtml(tripped.set_by)}` : ''}</span>`
      : '<span class="v-text">Off</span>';
    const control = `<label class="dp-label sr" for="${id}">Reason code for ${escapeHtml(route.label)}</label>`
      + `<input class="dp-input" id="${id}" data-kill-switch-reason="${escapeHtml(route.key)}" value="${escapeHtml(reason)}" placeholder="${tripped ? 'new reason code' : 'reason code, e.g. app_breakage'}" maxlength="64" autocomplete="off" spellcheck="false"${busy ? ' disabled' : ''}>`
      + stButton(state.killSwitch.pending === route.key ? 'Saving…' : tripped ? 'Change reason' : 'Trip', { action: 'kill-switch', route: route.key, value: 'on' }, { kind: 'danger', small: true, disabled: busy })
      + (tripped ? stButton('Clear', { action: 'kill-switch', route: route.key, value: 'off' }, { kind: 'primary', small: true, disabled: busy }) : '');
    return '<tr>'
      + `<td><span class="v-text">${escapeHtml(route.label)}</span><span class="dp-sub"><code>${escapeHtml(route.key)}</code></span></td>`
      + `<td>${status}</td>`
      + `<td class="dp-action">${control}</td></tr>`;
  }).join('');
  const body = '<p>Tripping a route\'s kill switch stops decryption and enforcement on it within one policy poll: devices keep carrying the traffic, unread and unenforced, and report the route as killed. Clearing it resumes them. A reason code is required and goes into the audit trail.</p>'
    + '<div class="table-scroll dp-flush"><table><thead><tr><th scope="col">Route</th><th scope="col">State</th><th scope="col"><span class="sr">Reason and action</span></th></tr></thead>'
    + `<tbody>${rows}</tbody></table></div>`
    + (busy ? '<p class="dp-note" role="status">Saving…</p>' : '')
    + stProblem(state.killSwitch.problem);
  return stCard('Kill switch', body);
}

const ACTION_LABELS = Object.freeze({ allow: 'Allow', warn: 'Warn', block: 'Block' });
const MATCH_TITLES = Object.freeze({ labels: 'Labels', tools: 'Tools', categories: 'Categories', sanction: 'Sanction', routes: 'Routes' });
const CATEGORY_LABELS = Object.freeze(Object.fromEntries(RULE_CATEGORIES.map((c) => [c.key, c.label])));
const ROUTE_LABELS = Object.freeze(Object.fromEntries(RULE_ROUTES.map((r) => [r.key, r.label])));

/** The options each condition picker offers, as [value, label]: the known set, plus any value a rule already holds. */
function matchOptions(field, data, current) {
  const known = {
    labels: () => (data.data_classes ?? []).map((c) => [c, c]),
    tools: () => data.tools.map((t) => [t.tool_key, t.display_name ?? t.tool_key]),
    categories: () => {
      const present = data.app_categories ?? [];
      return RULE_CATEGORIES.filter((c) => present.includes(c.key)).map((c) => [c.key, c.label])
        .concat(present.filter((c) => !(c in CATEGORY_LABELS)).map((c) => [c, c]));
    },
    sanction: () => RULE_SANCTIONS.map((s) => [s, s]),
    routes: () => RULE_ROUTES.map((r) => [r.key, r.label]),
  }[field]();
  const extra = current.filter((v) => !known.some(([k]) => k === v)).map((v) => [v, v]);
  return known.concat(extra);
}

/** A match value as the table shows it. */
function matchValueLabel(field, value, data) {
  if (field === 'tools') return data.tools.find((t) => t.tool_key === value)?.display_name ?? value;
  if (field === 'categories') return CATEGORY_LABELS[value] ?? value;
  if (field === 'routes') return ROUTE_LABELS[value] ?? value;
  return value;
}

function ruleConditions(rule, data) {
  const parts = RULE_MATCH_FIELDS.filter((f) => rule.match[f].length > 0).map((f) => `<span class="dp-sub">${escapeHtml(MATCH_TITLES[f])}</span>`
    + rule.match[f].map((v) => `<span class="v-text">${escapeHtml(matchValueLabel(f, v, data))}</span>`).join(' or '));
  return parts.length ? parts.join('') : '<span class="v-text">Any submission</span>';
}

function stRuleEditor(state) {
  const { index, rule, problem } = state.rules.editor;
  const data = state.data;
  const field = (name, label, value, attrs = '') => `<label class="dp-label" for="st-rule-${name}">${escapeHtml(label)}</label>`
    + `<input class="dp-input" id="st-rule-${name}" data-rule-draft="${name}" value="${escapeHtml(value)}" autocomplete="off"${attrs}>`;
  const actions = RULE_ACTIONS.map((a) => segItem(ACTION_LABELS[a], a, rule.action, { action: 'rule-action', value: a })).join('');
  const pickers = RULE_MATCH_FIELDS.map((f) => {
    const current = rule.match[f];
    const options = matchOptions(f, data, current);
    const items = options.length === 0
      ? `<span class="v-absent">${(f === 'labels' && data.data_classes === null) || (f === 'categories' && data.app_categories === null) ? 'not reported' : 'none known yet'}</span>`
      : `<div class="seg dp-seg" style="flex-wrap:wrap;border-radius:14px" role="group" aria-label="${escapeHtml(MATCH_TITLES[f])}">`
        + options.map(([value, label]) => `<button type="button" class="seg-item" aria-pressed="${current.includes(value)}" data-action="rule-match" data-field="${f}" data-value="${escapeHtml(value)}">${escapeHtml(label)}</button>`).join('')
        + '</div>';
    return `<span class="dp-label">${escapeHtml(MATCH_TITLES[f])}</span>${items}`;
  }).join('');
  return `<div class="dp-form" data-rule-editor="${index === null ? 'new' : index}">`
    + `<p class="dp-note"><strong>${index === null ? 'New rule' : `Rule ${index + 1}`}</strong></p>`
    + field('rule_id', 'Rule id', rule.rule_id, ' placeholder="block_credentials" spellcheck="false"')
    + '<span class="dp-label">Action</span>'
    + `<div class="seg dp-seg" role="group" aria-label="Action">${actions}</div>`
    + '<p class="dp-note">Conditions: a rule matches when every list with a choice matches one of its choices. A list with no choice matches anything.</p>'
    + pickers
    + field('message', 'Message', rule.message, ` maxlength="${MAX_RULE_MESSAGE}" placeholder="Shown to the person; needed for warn and block"`)
    + field('link', 'Link (optional)', rule.link, ' placeholder="https://" spellcheck="false"')
    + '<div class="dp-actions">'
    + stButton('Keep rule', { action: 'rule-apply' }, { kind: 'primary', small: true })
    + stButton('Cancel', { action: 'rule-cancel' }, { kind: 'quiet', small: true })
    + '</div>'
    + stProblem(problem, 'Not kept.')
    + '</div>';
}

function stRules(state) {
  const data = state.data;
  const list = state.rules.draft ?? data.rules;
  if (list === null) {
    return stCard('Enforcement rules', '<p><span class="v-absent">not reported</span> The server did not send the enforcement rules.</p>'
      + stProblem(state.rules.problem), { wide: true });
  }
  const busy = Boolean(state.rules.pending);
  const locked = busy || Boolean(state.rules.editor);
  const rows = list.map((rule, i) => '<tr>'
    + `<td>${i + 1}</td>`
    + `<td>${stChip(rule.action)}</td>`
    + `<td>${ruleConditions(rule, data)}</td>`
    + `<td>${rule.message ? `<span class="v-text">${escapeHtml(rule.message)}</span>` : '<span class="v-absent">none</span>'}`
    + (rule.link ? `<span class="dp-sub">${escapeHtml(rule.link)}</span>` : '') + '</td>'
    + '<td class="dp-action">'
    + stButton('Up', { action: 'rule-up', index: String(i) }, { kind: 'quiet', small: true, disabled: locked || i === 0 })
    + stButton('Down', { action: 'rule-down', index: String(i) }, { kind: 'quiet', small: true, disabled: locked || i === list.length - 1 })
    + stButton('Edit', { action: 'rule-edit', index: String(i) }, { kind: 'quiet', small: true, disabled: locked })
    + stButton('Delete', { action: 'rule-delete', index: String(i) }, { kind: 'quiet', small: true, disabled: locked })
    + '</td></tr>').join('');
  const empty = list.length === 0 ? '<tr class="row-empty"><td colspan="5">No rules. Every submission is logged.</td></tr>' : '';
  const unsaved = state.rules.draft !== null;
  const body = '<p>The first rule that matches a submission decides what happens to it; a submission no rule matches is logged. <em>Warn</em> and <em>block</em> show the message to the person. The list reaches each device on its next policy poll once it is saved.</p>'
    + '<div class="table-scroll dp-flush"><table><thead><tr><th scope="col">#</th><th scope="col">Action</th><th scope="col">Conditions</th><th scope="col">Message</th><th scope="col"><span class="sr">Order and edit</span></th></tr></thead>'
    + `<tbody>${rows}${empty}</tbody></table></div>`
    + (state.rules.editor ? stRuleEditor(state) : '')
    + '<div class="dp-actions">'
    + stButton('Add rule', { action: 'rule-add' }, { small: true, disabled: locked || list.length >= MAX_RULES })
    + (unsaved ? stButton(busy ? 'Saving…' : 'Save rules', { action: 'rules-save' }, { kind: 'primary', small: true, disabled: locked })
      + stButton('Discard changes', { action: 'rules-discard' }, { kind: 'quiet', small: true, disabled: busy }) : '')
    + '</div>'
    + (unsaved && !busy ? '<p class="dp-note" role="status">Unsaved changes: devices keep the saved list until you save.</p>' : '')
    + (busy ? '<p class="dp-note" role="status">Saving…</p>' : '')
    + stProblem(state.rules.problem);
  return stCard('Enforcement rules', body, { wide: true });
}

function stDevices(state) {
  const devices = state.data.devices;
  const rows = devices.map((d) => '<tr>'
    + `<td>${d.hostname ? `<span class="v-text">${escapeHtml(d.hostname)}</span>` : '<span class="v-absent">not reported</span>'}</td>`
    + `<td>${d.collection_mode ? stChip(d.collection_mode) : '<span class="v-absent">not reported</span>'}</td>`
    + `<td>${stInstant(d.last_seen_at)}</td></tr>`).join('');
  const empty = devices.length === 0 ? '<tr class="row-empty"><td colspan="3">No devices have enrolled.</td></tr>' : '';
  const body = '<p>The mode each device has actually applied, as it reported it — not only the mode requested above.</p>'
    + '<div class="table-scroll dp-flush"><table><thead><tr><th scope="col">Device</th><th scope="col">Applied mode</th><th scope="col">Last seen</th></tr></thead>'
    + `<tbody>${rows}${empty}</tbody></table></div>`;
  return stCard('Devices', body, { wide: true });
}

const ST_NOTES = Object.freeze([
  'Every change on this page is written to the audit trail under your name, with the old and the new value.',
  'A change reaches devices through the signed policy bundle, without reinstalling the agent.',
  'A mode increase that starts capturing content asks you to confirm before it is sent.',
]);

/**
 * The whole page for one state.
 *
 * @param {object} state  createSettings().state
 * @param {object} [options]
 * @param {string|null} [options.eyebrow]
 */
export function renderSettings(state, { eyebrow = 'Settings' } = {}) {
  const head = stHeader(eyebrow);
  if (state.status === 'idle' || state.status === 'loading') return `<article class="screen dp" aria-busy="true">${head}${stSkeleton()}</article>`;
  if (state.status === 'refused' || !state.data) return `<article class="screen dp">${head}${stRefused(state.problem)}</article>`;
  const refresh = state.refreshProblem
    ? `<div class="banner banner-warning" role="status"><strong>Not refreshed</strong><span>${escapeHtml(state.refreshProblem.message ?? '')} What is shown is from the last read.</span></div>`
    : '';
  const notes = `<details class="notes"><summary>About these settings<span class="notes-count">${ST_NOTES.length}</span></summary><ul>${ST_NOTES.map((n) => `<li>${escapeHtml(n)}</li>`).join('')}</ul></details>`;
  return `<article class="screen dp">${head}${refresh}`
    + `<div class="dp-grid">${stCollectionMode(state)}${stSearch(state)}</div>`
    + stRetention(state)
    + stOverrides(state)
    + stTools(state)
    + `<div class="dp-grid">${stTLSInspection(state)}${stKillSwitch(state)}</div>`
    + stEndpoint(state)
    + stRules(state)
    + stDevices(state)
    + notes
    + '</article>';
}
