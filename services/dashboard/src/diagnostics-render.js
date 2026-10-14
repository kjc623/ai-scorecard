// diagnostics-render.js — a device's diagnostics, state to HTML. Pure: no DOM, no I/O, no state.
//
// The section an admin sees below a device's collectors: a button that asks the device for its
// diagnostics, and the requests made, each waiting for the device or ready to download. Every
// string from the API is escaped.

import { escapeHtml } from './render.js';
import { formatBytes, formatInstant } from './format.js';
import { depButton, depInstant, depProblem } from './deployment-render.js';

const INTRO = 'The device sends its health snapshot, a configuration summary, its service log and its update logs '
  + 'the next time it checks in, usually within 15 minutes. No prompt it captured is included, but the logs name '
  + 'the device and the Windows accounts that used it. '
  + 'A request the device does not answer lapses after 7 days; a bundle is kept for 14. '
  + 'Each request and download is recorded in the audit trail.';

function stateCell(d) {
  if (d.state === 'received') {
    return '<span class="v-vocab v-vocab-healthy">Received</span>'
      + (d.received_at ? ` <span class="dp-sub">${escapeHtml(formatInstant(d.received_at))}</span>` : '');
  }
  return '<span class="v-vocab v-vocab-degraded">Waiting for the device</span>'
    + (d.expires_at ? ` <span class="dp-sub">lapses ${escapeHtml(formatInstant(d.expires_at))}</span>` : '');
}

function actionCell(d, state) {
  if (d.state !== 'received') return '';
  if (state.download.pending === d.request_id) return '<span class="dp-sub" role="status">Downloading…</span>';
  return depButton('Download', { diag: 'download', id: d.request_id }, { small: true, disabled: Boolean(state.download.pending) });
}

function table(state) {
  const rows = state.data.map((d) => '<tr>'
    + `<td>${depInstant(d.requested_at, 'not reported')}</td>`
    + `<td>${d.requested_by ? `<span class="v-text">${escapeHtml(d.requested_by)}</span>` : '<span class="v-absent">not reported</span>'}</td>`
    + `<td>${stateCell(d)}</td>`
    + `<td class="num">${d.size_bytes === null ? '<span class="v-absent">—</span>' : `<span class="v-number">${escapeHtml(formatBytes(d.size_bytes))}</span>`}</td>`
    + `<td>${d.agent_version ? `<span class="v-text">${escapeHtml(d.agent_version)}</span>` : '<span class="v-absent">—</span>'}</td>`
    + `<td class="dp-action">${actionCell(d, state)}</td>`
    + '</tr>').join('');
  const empty = state.data.length === 0 ? '<tr class="row-empty"><td colspan="6">No diagnostics have been requested from this device.</td></tr>' : '';
  return '<div class="table-scroll"><table><thead><tr><th scope="col">Requested</th><th scope="col">By</th><th scope="col">State</th>'
    + '<th scope="col" class="num">Size</th><th scope="col">Agent</th><th scope="col"><span class="sr">Action</span></th></tr></thead>'
    + `<tbody>${rows}${empty}</tbody></table></div>`;
}

function status(state) {
  const r = state.request;
  if (r.pending) return '<p class="dp-note" role="status">Asking the device…</p>';
  if (r.last) {
    return `<p class="dp-done" role="status">${r.last.created
      ? 'Requested. The device sends its diagnostics the next time it checks in; refresh to see them arrive.'
      : 'A request is already waiting for this device; it answers that one the next time it checks in.'}</p>`;
  }
  const d = state.download;
  if (d.last) return `<p class="dp-done" role="status">Saved <strong>${escapeHtml(d.last.filename)}</strong>.</p>`;
  return '';
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
 * A device's diagnostics, below its collectors.
 *
 * @param {object} state  createDiagnostics().state
 */
export function renderDeviceDiagnostics(state) {
  const waiting = state.data?.some((d) => d.state === 'waiting') ?? false;
  const busy = state.status !== 'ready' || state.request.pending;
  const actions = '<div class="dp-actions">'
    + depButton('Collect logs', { diag: 'request' }, { kind: 'primary', disabled: busy || waiting })
    + depButton('Refresh', { diag: 'refresh' }, { disabled: state.status !== 'ready' })
    + '</div>';
  const body = state.status === 'idle' || state.status === 'loading'
    ? '<p class="dp-note" role="status">Loading the diagnostics requests…</p>'
    : state.status === 'refused' || !state.data
      ? refused(state.problem)
      : `${actions}${status(state)}${depProblem(state.request.problem, 'Not requested.')}${depProblem(state.download.problem, 'Not downloaded.')}`
        + (state.refreshProblem ? depProblem(state.refreshProblem, 'Not refreshed.') : '')
        + table(state);
  return '<section class="table-block card dp" id="device-diagnostics"><div class="card-core">'
    + '<header class="block-head"><h3>Diagnostics</h3></header>'
    + `<div class="dp-body"><p>${escapeHtml(INTRO)}</p>${body}</div>`
    + '</div></section>';
}
