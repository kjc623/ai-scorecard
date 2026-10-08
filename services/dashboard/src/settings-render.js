// settings-render.js — Settings → Settings, state to HTML. Pure: no DOM, no I/O, no state.
//
// The same honesty rules as every other screen: a value the server did not send is "not reported",
// never a zero; every string from the API is escaped; a device's applied mode is shown as it
// reported it, not as the tenant requested it. The one confirmation on the page is a mode increase
// that starts capturing content.

import { escapeHtml } from './render.js';
import { formatInstant } from './format.js';
import { COLLECTION_MODES, SEARCH_TIERS, SANCTION_STATES, ENDPOINT_COLLECTORS, ENDPOINT_TOOLS, effectiveMode, modeIncreaseNeedsConfirmation, searchTierAllowed } from './settings.js';

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
  const current = state.data.scope_overrides[tool.tool_fingerprint] ?? '';
  const disabled = Boolean(state.override.pending);
  const options = ['<option value="">Default</option>']
    .concat(COLLECTION_MODES.map((m) => `<option value="${m}"${m === current ? ' selected' : ''}>${escapeHtml(MODE_LABELS[m])}</option>`))
    .join('');
  return `<select class="dp-input dp-select" data-action="override" data-tool="${escapeHtml(tool.tool_fingerprint)}" aria-label="Mode for ${escapeHtml(tool.display_name ?? tool.tool_fingerprint)}"${disabled ? ' disabled' : ''}>${options}</select>`;
}

function stOverrides(state) {
  const tools = state.data.tools;
  const rows = tools.map((tool) => '<tr>'
    + `<td><span class="v-text">${escapeHtml(tool.display_name ?? tool.tool_fingerprint)}</span></td>`
    + `<td><code>${escapeHtml(tool.tool_fingerprint)}</code></td>`
    + `<td class="dp-action">${stOverrideSelect(tool, state)}</td></tr>`).join('');
  const empty = tools.length === 0 ? '<tr class="row-empty"><td colspan="3">No tools are catalogued yet.</td></tr>' : '';
  const body = '<p>Narrow collection for one tool below the tenant\'s mode. An override can only reduce what is collected, never raise it.</p>'
    + '<div class="table-scroll dp-flush"><table><thead><tr><th scope="col">Tool</th><th scope="col">Fingerprint</th><th scope="col">Mode</th></tr></thead>'
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

function stTools(state) {
  const tools = state.data.tools;
  const rows = tools.map((tool) => {
    const current = tool.sanctioned_state;
    const seg = SANCTION_STATES.map((s) => segItem(s, s, current, { action: 'sanction', tool: tool.tool_fingerprint, value: s }, { disabled: Boolean(state.sanction.pending) })).join('');
    return '<tr>'
      + `<td><span class="v-text">${escapeHtml(tool.display_name ?? tool.tool_fingerprint)}</span></td>`
      + `<td class="dp-action"><div class="seg dp-seg" role="group" aria-label="Sanction for ${escapeHtml(tool.display_name ?? tool.tool_fingerprint)}">${seg}</div></td></tr>`;
  }).join('');
  const empty = tools.length === 0 ? '<tr class="row-empty"><td colspan="2">No tools are catalogued yet.</td></tr>' : '';
  const body = '<p>Mark a tool sanctioned or unsanctioned. A tool with no decision is <em>unknown</em>, never unsanctioned.</p>'
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
    + cell(tool, 'otel', 'OpenTelemetry') + cell(tool, 'hooks', 'Hooks') + '</tr>').join('');
  const body = '<p>Which collectors run on the devices. A change reaches each device on its next policy poll, without a restart. With <em>Only managed hooks</em> on, tools run only the hooks the agent manages, not a user\'s own.</p>'
    + '<div class="table-scroll dp-flush"><table><thead><tr><th scope="col">Collector</th><th scope="col">State</th></tr></thead>'
    + `<tbody>${collectorRows}</tbody></table></div>`
    + '<p>Per tool. A tool\'s switch takes effect only while the collector above is on; <em>unavailable</em> is a collector the tool does not have.</p>'
    + '<div class="table-scroll dp-flush"><table><thead><tr><th scope="col">Tool</th><th scope="col">OpenTelemetry</th><th scope="col">Hooks</th></tr></thead>'
    + `<tbody>${toolRows}</tbody></table></div>`
    + (busy ? '<p class="dp-note" role="status">Saving…</p>' : '')
    + stProblem(state.endpoint.problem);
  return stCard('Endpoint collectors', body, { wide: true });
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
    + stEndpoint(state)
    + stDevices(state)
    + notes
    + '</article>';
}
