// diagnostics-render.js — a device's diagnostics, state to HTML. Pure: no DOM, no I/O, no state.
//
// The section an admin sees below a device's collectors: a button that asks the device for its
// diagnostics, and the requests made, each waiting for the device or ready to download. Every
// string from the API is escaped.

import { escapeHtml } from './render.js';
import { formatBytes, formatInstant, formatWhen } from './format.js';
import { depButton, depInstant, depProblem } from './deployment-render.js';

const INTRO = 'Collect the agent\'s logs and a health snapshot from this device. It sends them the next time it checks in, usually within 15 minutes.';
const SMALL = 'No captured prompt is included, but the logs name the device and the Windows accounts that used it. '
  + 'A request the device does not answer lapses after 7 days, and a bundle is kept for 14. Each request and download is recorded in the audit trail.';

/** An instant in a line of text: "Oct 16 14:33", with the full form on hover. */
const when = (iso) => `<span title="${escapeHtml(formatInstant(iso))}">${escapeHtml(formatWhen(iso))}</span>`;

/** A request's state: received, with when and from which agent; or waiting, with what it waits for and when it lapses. */
function stateCell(d, quietSince) {
  if (d.state === 'received') {
    const facts = [d.received_at ? when(d.received_at) : null, d.agent_version ? `agent ${escapeHtml(d.agent_version)}` : null].filter(Boolean).join(' · ');
    return `<span class="v-vocab v-vocab-healthy">Received</span>${facts ? ` <span class="dp-sub">${facts}</span>` : ''}`;
  }
  const waits = quietSince ? `The device has not checked in since ${when(quietSince)}` : 'It sends them when it next checks in';
  return '<span class="v-vocab v-vocab-degraded">Waiting for the device</span>'
    + ` <span class="dp-sub">${waits}${d.expires_at ? ` · lapses ${when(d.expires_at)}` : ''}</span>`;
}

function actionCell(d, state) {
  if (d.state !== 'received') return '';
  if (state.download.pending === d.request_id) return '<span class="dp-sub" role="status">Downloading…</span>';
  return depButton('Download', { diag: 'download', id: d.request_id }, { small: true, disabled: Boolean(state.download.pending) });
}

function table(state, quietSince) {
  const rows = state.data.map((d) => '<tr>'
    + `<td>${depInstant(d.requested_at, 'not reported')}</td>`
    + `<td>${d.requested_by ? `<span class="v-text">${escapeHtml(d.requested_by)}</span>` : '<span class="v-absent">not reported</span>'}</td>`
    + `<td>${stateCell(d, quietSince)}</td>`
    + `<td class="num">${d.size_bytes === null ? '<span class="v-absent">—</span>' : `<span class="v-number">${escapeHtml(formatBytes(d.size_bytes))}</span>`}</td>`
    + `<td class="dp-action">${actionCell(d, state)}</td>`
    + '</tr>').join('');
  const empty = state.data.length === 0 ? '<tr class="row-empty"><td colspan="5">No diagnostics have been requested from this device.</td></tr>' : '';
  return '<div class="table-scroll"><table><thead><tr><th scope="col">Requested</th><th scope="col">By</th><th scope="col">State</th>'
    + '<th scope="col" class="num">Size</th><th scope="col"><span class="sr">Action</span></th></tr></thead>'
    + `<tbody>${rows}${empty}</tbody></table></div>`;
}

function refused(problem) {
  const s = problem?.status;
  const text = s === 401 ? 'Your session has ended. Sign in again to continue.'
    : s === 403 ? 'Only an admin can request a device\'s diagnostics.'
      : s === 0 ? 'The admin API could not be reached.'
        : problem?.message ?? 'Nothing was read.';
  return `<div class="dp-state" role="alert"><p>${escapeHtml(text)}</p>`
    + (s === 401 || s === 403 ? '' : `<div class="dp-actions">${depButton('Try again', { diag: 'retry' })}</div>`)
    + '</div>';
}

/**
 * A device's diagnostics, below its collectors. A request waiting for the device says so in its
 * row for as long as it waits, and the row turns to a download when the bundle arrives.
 *
 * @param {object} state  createDiagnostics().state
 * @param {object} [options]
 * @param {string} [options.quietSince]  when a quiet device last checked in, which a request waits on
 */
export function renderDeviceDiagnostics(state, { quietSince = null } = {}) {
  const waiting = state.data?.some((d) => d.state === 'waiting') ?? false;
  const busy = state.status !== 'ready' || state.request.pending;
  const actions = '<div class="dp-actions">'
    + depButton(state.request.pending ? 'Asking the device…' : 'Collect logs', { diag: 'request' }, { kind: 'primary', disabled: busy || waiting })
    + '</div>';
  const body = state.status === 'idle' || state.status === 'loading'
    ? '<p class="dp-note" role="status">Loading the diagnostics requests…</p>'
    : state.status === 'refused' || !state.data
      ? refused(state.problem)
      : `${actions}${depProblem(state.request.problem, 'Not requested.')}${depProblem(state.download.problem, 'Not downloaded.')}`
        + (state.refreshProblem ? depProblem(state.refreshProblem, 'Not refreshed.') : '')
        + table(state, quietSince);
  return '<section class="table-block card dp" id="device-diagnostics"><div class="card-core">'
    + '<header class="block-head"><h3>Diagnostics</h3></header>'
    + `<div class="dp-body"><p>${escapeHtml(INTRO)}</p><p class="dp-note">${escapeHtml(SMALL)}</p>${body}</div>`
    + '</div></section>';
}
