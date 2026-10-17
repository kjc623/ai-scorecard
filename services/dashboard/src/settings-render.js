// settings-render.js — Settings → Collection, and the sanction decisions on Policy → Tools, state to
// HTML. Pure: no DOM, no I/O, no state.
//
// The same honesty rules as every other screen: a value the server did not send is "not reported",
// never a zero, and every string from the API is escaped. The one confirmation on the page is a
// mode increase that starts capturing content.

import { escapeHtml, renderPageHead } from './render.js';
import { formatInstant } from './format.js';
import { COLLECTION_MODE_LABELS, COLLECTION_MODE_CODES } from './vocab.js';
import {
  COLLECTION_MODES, ENDPOINT_COLLECTORS, ENDPOINT_TOOLS, effectiveMode, modeIncreaseNeedsConfirmation, searchTierAllowed,
  KILL_SWITCH_ROUTES, RESTART_NOTICES,
} from './settings.js';

const MODE_LABELS = COLLECTION_MODE_LABELS;

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

function stHeader(head) {
  return renderPageHead({
    title: 'Settings',
    ...head,
    subtitle: 'What your organisation collects and keeps, and what search can reach. Every change is written to the audit trail.',
  });
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
  return COLLECTION_MODES.map((m) => segItem(`${MODE_LABELS[m]} · ${COLLECTION_MODE_CODES[m]}`, m, current, { ...data, value: m }, { disabled })).join('');
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
    .concat(COLLECTION_MODES.map((m) => `<option value="${m}"${m === current ? ' selected' : ''}>${escapeHtml(`${MODE_LABELS[m]} · ${COLLECTION_MODE_CODES[m]}`)}</option>`))
    .join('');
  return `<select class="dp-input dp-select" data-action="override" data-tool="${escapeHtml(tool.tool_key)}" aria-label="Mode for ${escapeHtml(tool.display_name ?? tool.tool_key)}"${disabled ? ' disabled' : ''}>${options}</select>`;
}

/**
 * The tools with an override, one row each, and a picker that adds one: a tool picked is listed
 * with a mode to choose, and nothing is written until it is. An override covers every fingerprint
 * the catalogue knows for the tool.
 */
function stOverrides(state) {
  const tools = state.data.tools;
  const overridden = tools.filter((tool) => state.data.scope_overrides[tool.tool_key] || state.override.adding === tool.tool_key);
  const rows = overridden.map((tool) => '<tr>'
    + `<td>${toolCell(tool)}</td>`
    + `<td class="dp-action">${stOverrideSelect(tool, state)}</td></tr>`).join('');
  const rest = tools.filter((tool) => !overridden.includes(tool));
  const empty = overridden.length === 0 ? `<tr class="row-empty"><td colspan="2">${tools.length === 0 ? 'No tools are catalogued yet.' : 'No tool has an override: every tool follows the collection mode.'}</td></tr>` : '';
  const picker = rest.length > 0
    ? `<p class="dp-inline-row"><label class="dp-label sr" for="st-override-add">Add an override for</label><select class="dp-input dp-select" id="st-override-add" data-action="override-add"${state.override.pending ? ' disabled' : ''}><option value="">Add an override…</option>${rest.map((tool) => `<option value="${escapeHtml(tool.tool_key)}">${escapeHtml(tool.display_name ?? tool.tool_key)}</option>`).join('')}</select></p>`
    : '';
  const body = '<p>Narrow collection for one tool below the tenant\'s mode. An override can only reduce what is collected, never raise it, and covers every fingerprint of the tool.</p>'
    + '<div class="table-scroll dp-flush"><table><thead><tr><th scope="col">Tool</th><th scope="col">Mode</th></tr></thead>'
    + `<tbody>${rows}${empty}</tbody></table></div>`
    + picker
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

function stSearch(state) {
  const data = state.data;
  const current = data.content_search ?? 'disabled';
  const ceiling = data.ceiling_mode ?? 'm0';
  const pending = state.search.pending;
  const seg = segItem('On', 'full_text', current, { action: 'search', value: 'full_text' }, { disabled: Boolean(pending) || !searchTierAllowed('full_text', ceiling) })
    + segItem('Off', 'disabled', current, { action: 'search', value: 'disabled' }, { disabled: Boolean(pending) });
  const note = ceiling !== 'm3'
    ? 'Needs an M3 ceiling: below it, devices never send prompt text.'
    : current === 'disabled'
      ? 'Prompts stay on the devices, so the Search page finds none.'
      : 'Prompts are kept for the content retention period, and can be searched and read on the Search page.';
  return stCard('Store and search prompts', `<div class="seg dp-seg" role="group" aria-label="Store and search prompts">${seg}</div>`
    + `<p class="dp-note"${pending ? ' role="status"' : ''}>${pending ? 'Saving…' : escapeHtml(note)}</p>`
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

function stRestartNotice(state) {
  const current = state.data.restart_notice;
  const busy = Boolean(state.restartNotice.pending);
  const control = current === null
    ? '<p><span class="v-absent">not reported</span> The server did not send this setting.</p>'
    : `<div class="seg dp-seg" role="group" aria-label="Apps that need a restart">${RESTART_NOTICES.map((n) => segItem(n.label, n.key, current, { action: 'restart-notice', value: n.key }, { disabled: busy })).join('')}</div>`;
  const body = control
    + '<p class="dp-note">Some apps read the agent\'s settings only when they start, so one that is already running keeps running without them until it restarts. Choose whether such a device shows on the dashboard, whether the person using the app is notified to restart it, or both.</p>'
    + (busy ? '<p class="dp-note" role="status">Saving…</p>' : '')
    + stProblem(state.restartNotice.problem);
  return stCard('Apps that need a restart', body);
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
    const control = `<label class="dp-label sr" for="${id}">Reason for ${escapeHtml(route.label)}</label>`
      + `<input class="dp-input" id="${id}" data-kill-switch-reason="${escapeHtml(route.key)}" value="${escapeHtml(reason)}" placeholder="${tripped ? 'New reason' : 'Reason, such as app_breakage'}" maxlength="64" autocomplete="off" spellcheck="false"${busy ? ' disabled' : ''}>`
      + stButton(state.killSwitch.pending === route.key ? 'Saving…' : tripped ? 'Change reason' : 'Trip', { action: 'kill-switch', route: route.key, value: 'on' }, { kind: 'danger', small: true, disabled: busy })
      + (tripped ? stButton('Clear', { action: 'kill-switch', route: route.key, value: 'off' }, { kind: 'primary', small: true, disabled: busy }) : '');
    return '<tr>'
      + `<td><span class="v-text">${escapeHtml(route.label)}</span><span class="dp-sub"><code>${escapeHtml(route.key)}</code></span></td>`
      + `<td>${status}</td>`
      + `<td class="dp-action">${control}</td></tr>`;
  }).join('');
  const body = '<p>Tripping a route\'s kill switch stops decryption and enforcement on it within one policy poll: devices keep carrying the traffic, unread and unenforced, and report the route as killed. Clearing it resumes them. A reason is required and goes into the audit trail as a short code: lower-case letters, digits, dots, dashes or underscores.</p>'
    + '<div class="table-scroll dp-flush"><table><thead><tr><th scope="col">Route</th><th scope="col">State</th><th scope="col"><span class="sr">Reason and action</span></th></tr></thead>'
    + `<tbody>${rows}</tbody></table></div>`
    + (busy ? '<p class="dp-note" role="status">Saving…</p>' : '')
    + stProblem(state.killSwitch.problem);
  return stCard('Kill switch', body);
}

const ST_NOTES = Object.freeze([
  'Every change on this page is written to the audit trail under your name, with the old and the new value.',
  'A change reaches devices through the signed policy bundle, without reinstalling the agent.',
  'A mode increase that starts capturing content asks you to confirm before it is sent.',
]);

/**
 * Settings → Collection for one state.
 *
 * @param {object} state  createSettings().state
 * @param {object} [options]
 * @param {object} [options.head]  the page's title, eyebrow and tabs
 */
export function renderSettings(state, { head = {} } = {}) {
  const top = stHeader(head);
  if (state.status === 'idle' || state.status === 'loading') return `<article class="screen dp" aria-busy="true">${top}${stSkeleton()}</article>`;
  if (state.status === 'refused' || !state.data) return `<article class="screen dp">${top}${stRefused(state.problem)}</article>`;
  const refresh = state.refreshProblem
    ? `<div class="banner banner-warning" role="status"><strong>Not refreshed</strong><span>${escapeHtml(state.refreshProblem.message ?? '')} What is shown is from the last read.</span></div>`
    : '';
  const notes = `<details class="notes"><summary>About these settings<span class="notes-count">${ST_NOTES.length}</span></summary><ul>${ST_NOTES.map((n) => `<li>${escapeHtml(n)}</li>`).join('')}</ul></details>`;
  // What most admins set is above; what the agent watches, how it inspects traffic, and the switch
  // that stops it are under Advanced, closed until needed.
  const advanced = '<details class="dp-advanced"><summary>Advanced<span class="dp-sub">TLS inspection, the kill switch, and what the agent watches. Open it to change how collection works, not what is collected.</span></summary>'
    + `<div class="dp-advanced-body"><div class="dp-grid">${stTLSInspection(state)}${stKillSwitch(state)}</div>${stEndpoint(state)}</div></details>`;
  return `<article class="screen dp">${top}${refresh}`
    + `<div class="dp-grid">${stCollectionMode(state)}${stSearch(state)}</div>`
    + stRetention(state)
    + stOverrides(state)
    + stRestartNotice(state)
    + advanced
    + notes
    + '</article>';
}
