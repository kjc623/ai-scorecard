// directory-render.js — Settings → Directory & teams, state to HTML. Pure: no DOM, no I/O, no state.
//
// The same rules as Deployment: a count the server did not send is "not reported", never 0, a
// failed read says which failure it was and who can fix it, and every string from an API is
// escaped.

import { escapeHtml, renderTile } from './render.js';
import { formatCount, formatInstant } from './format.js';
import { depButton, depCard, depChip, depInstant, depProblem } from './deployment-render.js';
import { SYNC_ERRORS, TEAM_SOURCES } from './directory.js';

function dirHeader(eyebrow) {
  return '<header class="screen-head"><div class="screen-title">'
    + (eyebrow ? `<span class="eyebrow">${escapeHtml(eyebrow)}</span>` : '')
    + '<h2>Directory &amp; teams</h2>'
    + '<p class="question">Who Sundial knows from your directory, and the teams the Teams page reports on.</p>'
    + '</div></header>';
}

function dirRefused(problem) {
  const status = problem?.status;
  const title = status === 401 ? 'Your session has ended'
    : status === 403 ? 'Only an admin can manage the directory and teams'
      : status === 0 ? 'The admin API could not be reached'
        : 'The directory settings could not be read';
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
    { label: 'Teams', value: number(data.teams.length), note: data.teams.length === 0 ? 'none yet' : null },
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

function sourceLabel(team) {
  switch (team.source) {
    case 'group': return `Group: ${team.group_name ?? team.group_object_id ?? 'unknown'}`;
    case 'department': return `Department: ${team.match_value ?? ''}`;
    case 'org_unit': return `Unit: ${team.match_value ?? ''}`;
    case 'console': return 'Chosen here';
    default: return team.source ?? 'unknown';
  }
}

function dirMembers(state, team) {
  const t = state.team;
  if (t.pending && !t.members) return '<p class="dp-note" role="status">Reading the members…</p>';
  const members = t.members ?? [];
  const console = team.source === 'console';
  const rows = members.map((m) => '<tr>'
    + `<td><a class="cell-link" href="#person?subject=${encodeURIComponent(m.user_ref)}">${escapeHtml(m.name || m.user_ref)}</a></td>`
    + `<td>${m.department ? `<span class="v-text">${escapeHtml(m.department)}</span>` : '<span class="v-absent">—</span>'}</td>`
    + `<td class="dp-action">${console ? depButton('Remove', { dir: 'remove-member', ref: m.user_ref }, { small: true, disabled: t.pending }) : ''}</td>`
    + '</tr>').join('');
  const empty = members.length === 0 ? `<tr class="row-empty"><td colspan="3">${console ? 'Nobody yet. Find people below to add them.' : 'Nobody matches yet. Members follow the directory, so they appear after its next read.'}</td></tr>` : '';
  const capped = t.limit !== null && members.length >= t.limit ? `<p class="dp-note">Showing the first ${escapeHtml(formatCount(t.limit))} members.</p>` : '';
  let add = '';
  if (console) {
    const results = state.people.results;
    const known = new Set(members.map((m) => m.user_ref));
    const found = results === null ? ''
      : results.length === 0 ? '<p class="dp-note">Nobody\'s name starts with that.</p>'
        : `<ul class="dir-results">${results.map((p) => `<li><span class="v-text">${escapeHtml(p.name ?? p.subject)}</span>`
          + (p.department ? ` <span class="dp-sub">${escapeHtml(p.department)}</span>` : '')
          + (known.has(p.subject) ? ' <span class="dp-sub">in the team</span>' : ` ${depButton('Add', { dir: 'add-member', ref: p.subject }, { small: true, disabled: t.pending })}`)
          + '</li>').join('')}</ul>`;
    add = '<form class="dp-form" data-dir-form="search-people" autocomplete="off">'
      + '<label class="dp-label" for="dir-people">Add people</label>'
      + `<div class="dp-inline-row"><input class="dp-input" id="dir-people" maxlength="120" data-dir-draft="peopleQuery" placeholder="The start of a name" value="${escapeHtml(state.drafts.peopleQuery)}">`
      + depButton(state.people.pending ? 'Finding…' : 'Find', { dir: 'search-people' }, { disabled: state.people.pending }) + '</div></form>'
      + found + depProblem(state.people.problem, 'Nobody was found.');
  }
  const remove = t.confirm
    ? `<span class="dp-confirm">Delete the team “${escapeHtml(team.name)}”? Its usage history goes with it. ${depButton('Delete', { dir: 'confirm-delete' }, { kind: 'danger', small: true, disabled: t.pending })}${depButton('Cancel', { dir: 'cancel-delete' }, { kind: 'quiet', small: true })}</span>`
    : depButton('Delete team', { dir: 'delete-team' }, { kind: 'quiet', small: true, disabled: t.pending });
  return '<div class="dp-body dir-team">'
    + `<div class="dp-subhead"><h4>${escapeHtml(team.name)}</h4><span class="dp-sub">${escapeHtml(sourceLabel(team))}</span></div>`
    + '<div class="table-scroll"><table><thead><tr><th scope="col">Person</th><th scope="col">Department</th><th scope="col"><span class="sr">Action</span></th></tr></thead>'
    + `<tbody>${rows}${empty}</tbody></table></div>`
    + capped + add + depProblem(t.problem, 'Not changed.')
    + `<div class="dp-actions">${depButton('Close', { dir: 'close-team' }, { small: true })}${remove}</div>`
    + '</div>';
}

function dirTeams(state) {
  const teams = state.data.teams;
  const rows = teams.map((team) => '<tr>'
    + `<td><span class="v-text">${escapeHtml(team.name)}</span></td>`
    + `<td><span class="v-text">${escapeHtml(sourceLabel(team))}</span></td>`
    + `<td class="num">${team.members === null ? '<span class="v-absent">not reported</span>' : `<span class="v-number">${escapeHtml(formatCount(team.members))}</span>`}</td>`
    + `<td>${depInstant(team.created_at, 'not reported')}</td>`
    + `<td class="dp-action">${depButton(state.team.open === team.team_id ? 'Open' : 'Members', { dir: 'open-team', id: team.team_id }, { small: true, disabled: state.team.open === team.team_id })}</td>`
    + '</tr>').join('');
  const empty = teams.length === 0 ? '<tr class="row-empty"><td colspan="5">No teams yet. Create one below.</td></tr>' : '';
  const open = teams.find((t) => t.team_id === state.team.open);
  return '<section class="table-block card" id="dir-teams"><div class="card-core">'
    + `<header class="block-head"><h3>Teams</h3>${teams.length > 0 ? `<span class="block-count">${teams.length}</span>` : ''}<a class="block-link" href="#teams">Team usage</a></header>`
    + '<div class="table-scroll"><table><thead><tr><th scope="col">Team</th><th scope="col">Members from</th><th scope="col" class="num">Members</th><th scope="col">Created</th><th scope="col"><span class="sr">Action</span></th></tr></thead>'
    + `<tbody>${rows}${empty}</tbody></table></div>`
    + (open ? dirMembers(state, open) : '')
    + '</div></section>';
}

function dirChoices(state) {
  const { source } = state.drafts;
  const c = state.choices;
  if (source === 'console') return '<p class="dp-note">You choose the members after creating it.</p>';
  if (c.pending) return '<p class="dp-note" role="status">Reading the directory…</p>';
  if (c.problem) return depProblem(c.problem, 'Nothing to choose from.');
  if (source === 'group') {
    const picked = state.drafts.group;
    const search = '<form class="dp-form" data-dir-form="search-groups" autocomplete="off"><div class="dp-inline-row">'
      + `<input class="dp-input" id="dir-group-query" maxlength="120" data-dir-draft="groupQuery" placeholder="Search your directory's groups" value="${escapeHtml(state.drafts.groupQuery)}">`
      + depButton('Search', { dir: 'search-groups' }) + '</div></form>';
    const groups = c.groups ?? [];
    const list = groups.length === 0
      ? '<p class="dp-note">No group found. Search your directory by the start of a group\'s name.</p>'
      : `<ul class="dir-results">${groups.map((g) => {
        const chosen = picked && ((g.id && picked.id === g.id) || (g.object_id && picked.object_id === g.object_id));
        return `<li><span class="v-text">${escapeHtml(g.name)}</span>`
          + ` <span class="dp-sub">${g.directory === 'entra' ? (g.imported ? 'in Entra, already imported' : 'in Entra') : `provisioned${g.members === null || g.members === undefined ? '' : `, ${escapeHtml(formatCount(g.members))} members`}`}</span> `
          + (chosen ? '<span class="dp-sub">chosen</span>' : depButton('Choose', { dir: 'pick-group', id: g.id ?? '', object: g.object_id ?? '', name: g.name ?? '' }, { small: true }))
          + '</li>';
      }).join('')}</ul>`;
    return search + list;
  }
  const values = c.values ?? [];
  if (values.length === 0) {
    return `<p class="dp-note">No ${source === 'department' ? 'department' : 'organisational unit'} is known yet. They arrive with people from the directory.</p>`;
  }
  return `<ul class="dir-results">${values.map((v) => `<li><span class="v-text">${escapeHtml(v.value)}</span> <span class="dp-sub">${escapeHtml(formatCount(v.people))} people</span> `
    + (state.drafts.match === v.value ? '<span class="dp-sub">chosen</span>' : depButton('Choose', { dir: 'pick-value', value: v.value }, { small: true }))
    + '</li>').join('')}</ul>`;
}

function dirCreate(state) {
  const source = state.drafts.source;
  const option = (s) => `<button type="button" class="seg-item" data-dir="source" data-value="${s.id}" aria-pressed="${source === s.id}">${escapeHtml(s.label)}</button>`;
  const body = '<form class="dp-form" data-dir-form="create" autocomplete="off">'
    + '<label class="dp-label" for="dir-team-name">Name</label>'
    + `<input class="dp-input" id="dir-team-name" maxlength="120" data-dir-draft="name" placeholder="For example: Platform engineering" value="${escapeHtml(state.drafts.name)}">`
    + '</form>'
    + '<span class="dp-label">Members</span>'
    + `<div class="seg dp-seg" role="group" aria-label="Where the members come from">${Object.values(TEAM_SOURCES).map(option).join('')}</div>`
    + dirChoices(state)
    + `<div class="dp-actions">${depButton(state.create.pending ? 'Creating…' : 'Create team', { dir: 'create' }, { kind: 'primary', disabled: state.create.pending })}</div>`
    + depProblem(state.create.problem, 'No team was created.');
  return depCard('Create a team', body, { wide: true });
}

const DIR_NOTES = Object.freeze([
  'Every change on this page is written to the audit trail under your name.',
  'A team counts the usage of the people in it now: adding someone brings their past usage into the team on the next rollup.',
  'Finding a person to add reads the people list, which is audited like every other read that names a person.',
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
    return `<article class="screen dp" aria-busy="true">${head}<p class="sr" role="status">Loading the directory settings…</p></article>`;
  }
  if (state.status === 'refused' || !state.data) return `<article class="screen dp">${head}${dirRefused(state.problem)}</article>`;
  const refresh = state.refreshProblem
    ? `<div class="banner banner-warning" role="status"><strong>Not refreshed</strong><span>${escapeHtml(state.refreshProblem.message ?? '')} What is shown is from the last read.</span></div>`
    : '';
  const notes = `<details class="notes"><summary>About these settings<span class="notes-count">${DIR_NOTES.length}</span></summary><ul>${DIR_NOTES.map((n) => `<li>${escapeHtml(n)}</li>`).join('')}</ul></details>`;
  return `<article class="screen dp">${head}${refresh}${dirTiles(state.data)}`
    + dirSync(state)
    + dirTeams(state)
    + dirCreate(state)
    + notes
    + '</article>';
}
