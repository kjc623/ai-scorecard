// directory-render.js — Settings → Identity Providers, state to HTML. Pure: no DOM, no I/O, no state.
//
// The same rules as Deployment: a count the server did not send is "not reported", never 0, a
// failed read says which failure it was and who can fix it, and every string from an API is
// escaped.

import { escapeHtml, renderTile } from './render.js';
import { formatCount, formatInstant } from './format.js';
import { depButton, depCard, depChip, depInstant, depProblem } from './deployment-render.js';
import { SYNC_ERRORS } from './directory.js';

function dirHeader(eyebrow) {
  return '<header class="screen-head"><div class="screen-title">'
    + (eyebrow ? `<span class="eyebrow">${escapeHtml(eyebrow)}</span>` : '')
    + '<h2>Identity Providers</h2>'
    + '<p class="question">Where Sundial learns who your people are and which groups they are in. Teams are added from them on the Teams page.</p>'
    + '</div></header>';
}

function dirRefused(problem) {
  const status = problem?.status;
  const title = status === 401 ? 'Your session has ended'
    : status === 403 ? 'Only an admin can manage identity providers'
      : status === 0 ? 'The admin API could not be reached'
        : 'The identity provider settings could not be read';
  const text = status === 401 ? 'Sign in again to continue.' : problem?.message ?? 'Nothing was read.';
  const action = status === 401 ? '<a class="btn" href="/signin">Sign in again</a>' : status === 403 ? '' : depButton('Try again', { dir: 'retry' });
  return '<section class="card dp-card"><div class="card-core dp-state" role="alert">'
    + `<h3>${escapeHtml(title)}</h3><p>${escapeHtml(text)}</p>`
    + (problem?.code ? `<p class="dp-code">${escapeHtml(problem.code)}${status ? ` · ${escapeHtml(String(status))}` : ''}</p>` : '')
    + (action ? `<div class="dp-actions">${action}</div>` : '')
    + '</div></section>';
}

const number = (n) => (n === null || n === undefined ? { kind: 'absent' } : { kind: 'number', value: n, text: formatCount(n) });

function dirTiles(data) {
  const sync = data.sync;
  return `<div class="tiles">${[
    { label: 'People', value: number(data.people), note: data.people === null ? 'not reported by the server' : 'in the directory Sundial holds' },
    { label: 'Groups', value: number(data.groups), note: data.groups === null ? 'not reported by the server' : 'provisioned or imported' },
    {
      label: 'Last directory read', value: sync?.last_completed_at ? { kind: 'number', value: 0, text: formatInstant(sync.last_completed_at) } : { kind: 'absent' },
      note: !sync ? 'reading from Entra is off' : sync.last_status === 'failed' ? 'failed' : sync.last_completed_at ? null : 'not finished yet',
    },
  ].map(renderTile).join('')}</div>`;
}

function dirSync(state) {
  const data = state.data;
  const sync = data.sync;
  const pending = state.sync.pending;
  let body;
  if (!data.graph_available) {
    body = '<p class="dp-empty">This deployment has no Microsoft Entra application, so it cannot read a directory. People and groups arrive only over SCIM (Settings → Deployment).</p>';
  } else if (!data.entra_connected) {
    body = '<p class="dp-empty">Reading the directory needs an active Microsoft Entra ID connection for your organisation. Sign-in is linked from the onboarding link your vendor sent.</p>';
  } else if (!sync) {
    body = '<p>Sundial can read the people and groups in your Microsoft Entra ID itself, every hour, with no provisioning to set up. It reads each person\'s name, sign-in name, department, organisational unit and whether their account is enabled, and the members of the groups your teams follow.</p>'
      + '<p class="dp-note">An Entra administrator must first grant the Sundial application the <b>User.Read.All</b> and <b>GroupMember.Read.All</b> application permissions.</p>'
      + `<div class="dp-actions">${depButton(pending === 'on' ? 'Turning on…' : 'Read the directory from Entra', { dir: 'sync-on' }, { kind: 'primary', disabled: Boolean(pending) })}</div>`;
  } else {
    const result = sync.last_status === 'failed'
      ? `<p class="dp-problem" role="alert"><strong>The last read failed.</strong> ${escapeHtml(SYNC_ERRORS[sync.last_error] ?? 'The directory could not be read.')} <code>${escapeHtml(sync.last_error ?? '')}</code></p>`
      : sync.last_status === 'ok'
        ? `<p class="dp-done">Read ${escapeHtml(formatCount(sync.users_synced ?? 0))} people and ${escapeHtml(formatCount(sync.groups_synced ?? 0))} team group${sync.groups_synced === 1 ? '' : 's'} ${escapeHtml(formatInstant(sync.last_completed_at))}.</p>`
        : '<p class="dp-note" role="status">The first read has not finished yet.</p>';
    const queued = state.sync.queued === true ? '<p class="dp-note" role="status">A read has been started.</p>'
      : state.sync.queued === false ? '<p class="dp-note" role="status">A read is already running.</p>' : '';
    body = '<dl class="dp-kv">'
      + `<div><dt>Turned on</dt><dd>${depInstant(sync.enabled_at, 'not reported')}${sync.enabled_by ? ` <span class="dp-sub">by ${escapeHtml(sync.enabled_by)}</span>` : ''}</dd></div>`
      + `<div><dt>Last started</dt><dd>${depInstant(sync.last_started_at)}</dd></div>`
      + `<div><dt>Last finished</dt><dd>${depInstant(sync.last_completed_at)} ${sync.last_status ? depChip(sync.last_status) : ''}</dd></div>`
      + '</dl>'
      + result + queued
      + '<div class="dp-actions">'
      + depButton(pending === 'run' ? 'Starting…' : 'Read now', { dir: 'sync-run' }, { kind: 'primary', disabled: Boolean(pending) })
      + depButton(pending === 'off' ? 'Turning off…' : 'Stop reading', { dir: 'sync-off' }, { kind: 'quiet', disabled: Boolean(pending) })
      + '</div>'
      + '<p class="dp-note">Stopping keeps the people already read. Someone who leaves the directory is marked inactive on the next complete read, never deleted.</p>';
  }
  return depCard('Read people and groups from Microsoft Entra ID', body + depProblem(state.sync.problem, 'Not changed.'), { wide: true });
}

function dirScim() {
  const body = '<p>An identity provider such as Microsoft Entra ID or Okta can instead push people and groups to Sundial over SCIM 2.0. Sundial keeps one directory either way: a person who arrives both ways is one person.</p>'
    + '<p class="dp-note">The SCIM base URL and its tokens are under <a href="#deployment">Settings → Deployment</a>.</p>';
  return depCard('Provision people and groups over SCIM', body, { wide: true });
}

const DIR_NOTES = Object.freeze([
  'Every change on this page is written to the audit trail under your name.',
  'Teams are added and edited on Usage → Teams, from the people, groups, departments and units held here.',
]);

/**
 * The whole page for one state.
 *
 * @param {object} state  createDirectory().state
 * @param {object} [options]
 * @param {string|null} [options.eyebrow]
 */
export function renderDirectory(state, { eyebrow = 'Settings' } = {}) {
  const head = dirHeader(eyebrow);
  if (state.status === 'idle' || state.status === 'loading') {
    return `<article class="screen dp" aria-busy="true">${head}<p class="sr" role="status">Loading the identity provider settings…</p></article>`;
  }
  if (state.status === 'refused' || !state.data) return `<article class="screen dp">${head}${dirRefused(state.problem)}</article>`;
  const refresh = state.refreshProblem
    ? `<div class="banner banner-warning" role="status"><strong>Not refreshed</strong><span>${escapeHtml(state.refreshProblem.message ?? '')} What is shown is from the last read.</span></div>`
    : '';
  const notes = `<details class="notes"><summary>About these settings<span class="notes-count">${DIR_NOTES.length}</span></summary><ul>${DIR_NOTES.map((n) => `<li>${escapeHtml(n)}</li>`).join('')}</ul></details>`;
  return `<article class="screen dp">${head}${refresh}${dirTiles(state.data)}`
    + dirSync(state)
    + dirScim()
    + notes
    + '</article>';
}
