// team-editor-render.js — the Teams page's "+ Add a Team" menu, a team's "Edit team" button, and the
// pop-up they open, state to HTML. Pure: no DOM, no I/O, no state.
//
// Every string from an API is escaped, and a list that could not be read says so rather than
// showing as empty.

import { escapeHtml } from './render.js';
import { formatCount } from './format.js';
import { depButton, depProblem } from './deployment-render.js';
import { OBJECT_TYPES, TEAM_KINDS, blockedReason, countsOf, entraSearchable, visibleItems } from './team-editor.js';

const KIND_HINTS = Object.freeze({
  idp: 'A group, department or unit from your directory',
  scim: 'Users and groups your identity provider syncs',
  manual: 'Tick the people yourself',
});

const KIND_LEADS = Object.freeze({
  idp: 'Choose a group for the team to follow, or a department, a unit or people, from what your identity provider has synced to Sundial.',
  scim: 'Choose from the users and groups synced to Sundial, and the departments and units they carry.',
  manual: 'Tick the people in the team. They stay in it until you change it.',
});

const TYPE_LABELS = Object.freeze({ user: 'User', group: 'Group', department: 'Department', org_unit: 'Unit' });

const plural = (n, one, many) => `${formatCount(n)} ${n === 1 ? one : many}`;

/** The "+ Add a Team" button and the menu of ways to add one. */
export function renderTeamAdd() {
  const items = Object.values(TEAM_KINDS).map((k) => `<button type="button" role="menuitem" class="add-menu-item" data-team="open" data-kind="${k.id}">`
    + `<span class="add-menu-label">${escapeHtml(k.label)}</span><span class="add-menu-hint">${escapeHtml(KIND_HINTS[k.id])}</span></button>`).join('');
  return '<details class="add-menu"><summary class="btn btn-primary">+ Add a Team</summary>'
    + `<div class="add-menu-list" role="menu" aria-label="Add a team from">${items}</div></details>`;
}

/** The button that opens one team in the pop-up. */
export function renderTeamEdit(teamId) {
  return depButton('Edit team', { team: 'edit', id: teamId });
}

/** How a team gets its members, in a sentence. */
export function teamSourceText(team) {
  switch (team?.source) {
    case 'console': return 'Manual: the people chosen for it.';
    case 'group': return `Follows the group ${team.group_name ?? team.group_object_id ?? ''}: its members, as your directory changes them.`;
    case 'department': return `Follows everyone in the department ${team.match_value ?? ''}.`;
    case 'org_unit': return `Follows everyone in the unit ${team.match_value ?? ''} and the units below it.`;
    default: return '';
  }
}

function detailOf(item) {
  switch (item.type) {
    case 'user': return item.department ?? '';
    case 'group': return item.entra ? 'In Microsoft Entra ID, not synced yet' : item.members === null ? '' : plural(item.members, 'member', 'members');
    case 'department': return item.people === null ? '' : plural(item.people, 'person', 'people');
    case 'org_unit': return [item.people === null ? '' : plural(item.people, 'person', 'people'), item.value].filter(Boolean).join(' · ');
    default: return '';
  }
}

function row(state, item) {
  const picked = Boolean(state.picked[item.key]);
  const blocked = blockedReason(state, item);
  const disabled = Boolean(blocked) || Boolean(state.pending);
  const type = state.kind === 'manual' ? '' : `<span class="pick-type">${escapeHtml(TYPE_LABELS[item.type] ?? item.type)}</span>`;
  const detail = detailOf(item);
  return `<li class="pick-row${blocked ? ' is-blocked' : ''}"><label class="pick-label"${blocked ? ` title="${escapeHtml(blocked)}"` : ''}>`
    + `<input type="checkbox" class="pick-box" data-team-pick="${escapeHtml(item.key)}"${picked ? ' checked' : ''}${disabled ? ' disabled' : ''}>`
    + `<span class="pick-name">${escapeHtml(item.name)}</span>${type}`
    + (detail ? `<span class="pick-detail">${escapeHtml(detail)}</span>` : '')
    + '</label></li>';
}

function filterBar(state) {
  if (state.kind === 'manual') return '';
  const counts = countsOf(state);
  const count = (type) => {
    if (type === 'all' || counts[type] === null) return null;
    return `${formatCount(counts[type])}${type === 'user' && state.people.next ? '+' : ''}`;
  };
  const item = ([type, label]) => {
    const n = count(type);
    return `<button type="button" class="seg-item" data-team="filter" data-value="${type}" aria-pressed="${state.filter === type}"${type === 'org_unit' ? ' title="Organisational units"' : ''}>`
      + `${escapeHtml(label)}${n === null ? '' : ` <span class="seg-count">${escapeHtml(n)}</span>`}</button>`;
  };
  return `<div class="seg dp-seg pick-filter" role="group" aria-label="Show">${Object.entries(OBJECT_TYPES).map(item).join('')}</div>`;
}

function searchBar(state) {
  const placeholder = state.kind === 'manual' ? 'Search people by the start of their name' : 'Search synced users, groups, departments and units';
  return '<form class="pick-search" data-team-form="search" role="search" autocomplete="off">'
    + `<input class="dp-input" id="team-query" type="search" maxlength="120" data-team-draft="query" aria-label="Search" placeholder="${escapeHtml(placeholder)}" value="${escapeHtml(state.query)}">`
    + depButton(state.people.pending ? 'Searching…' : 'Search', { team: 'search' }, { disabled: state.people.pending })
    + '</form>';
}

/** A line under the search: what IdP Group's search reaches, and what is not synced. */
function listNote(state) {
  if (state.kind !== 'idp' || !state.directory) return '';
  if (entraSearchable(state)) return '<p class="dp-note">Searching also looks in Microsoft Entra ID for groups not synced yet; one is imported when its team is made.</p>';
  if (state.directory.graph_available && state.directory.entra_connected) {
    return '<p class="dp-note">A group is synced only once a team follows it. To search Microsoft Entra ID for one, turn on reading the directory under <a href="#directory">Identity Providers</a>.</p>';
  }
  return '';
}

function emptyText(state) {
  const term = state.term;
  if (state.filter === 'user' || state.kind === 'manual') {
    if (term) return `Nobody's name starts with “${term}”.`;
    return state.kind === 'manual'
      ? 'Sundial knows nobody yet. People appear once a device reports or your identity provider syncs them.'
      : 'No people are synced yet.';
  }
  if (term) return `Nothing synced matches “${term}”.`;
  return 'Nothing is synced from your identity provider yet.';
}

function pickList(state) {
  const loading = state.people.pending || state.groups.pending || state.departments.pending || state.units.pending || state.members.pending;
  const items = visibleItems(state);
  const rows = items.map((item) => row(state, item)).join('');
  const empty = items.length === 0
    ? `<li class="pick-empty">${loading ? 'Reading…' : escapeHtml(emptyText(state))}`
      + (!loading && !state.term && state.kind !== 'manual' ? ' Turn on reading from Microsoft Entra ID under <a href="#directory">Identity Providers</a>, or set up SCIM under <a href="#deployment">Deployment</a>.' : '')
      + '</li>'
    : '';
  const wantsPeople = state.filter === 'all' || state.filter === 'user';
  const more = wantsPeople && state.people.next
    ? `<div class="pick-more">${depButton(state.people.pending ? 'Reading…' : 'Show more people', { team: 'more' }, { small: true, disabled: state.people.pending })}</div>`
    : '';
  const problems = [state.people.problem, state.groups.problem, state.departments.problem, state.units.problem, state.members.problem]
    .filter(Boolean).map((p) => depProblem(p, 'Not all of the list could be read.')).join('');
  return `<ul class="pick-list" id="team-pick-list" aria-label="${state.kind === 'manual' ? 'People' : 'Synced objects'}" aria-busy="${loading ? 'true' : 'false'}">${rows}${empty}</ul>${more}${problems}`;
}

function summary(state) {
  const picked = Object.values(state.picked);
  if (state.mode === 'edit') {
    if (state.team?.source !== 'console' || !state.members.rows) return '';
    const before = new Set(state.members.rows.map((m) => m.key));
    const adding = picked.filter((p) => !before.has(p.key)).length;
    const removing = [...before].filter((key) => !state.picked[key]).length;
    const changes = [adding ? `${formatCount(adding)} to add` : '', removing ? `${formatCount(removing)} to remove` : ''].filter(Boolean).join(', ');
    return `${plural(picked.length, 'person', 'people')} in the team${changes ? ` (${changes})` : ''}.`;
  }
  if (picked.length === 0) return state.kind === 'manual' ? 'Tick the people in the team.' : 'Tick people, or one group, department or unit for the team to follow.';
  const followed = picked.find((p) => p.type !== 'user');
  if (!followed) return `${plural(picked.length, 'person', 'people')} ticked.${state.kind === 'manual' ? '' : ' The team holds exactly them.'}`;
  if (followed.type === 'group') {
    return `The team follows the group ${followed.name}: its members, as your directory changes them.${followed.entra ? ' It is imported from Microsoft Entra ID when the team is made.' : ''}`;
  }
  if (followed.type === 'department') return `The team follows everyone in the department ${followed.name}.`;
  return `The team follows everyone in the unit ${followed.name} and the units below it.`;
}

function footer(state) {
  const saving = state.pending === 'save';
  const save = depButton(saving ? (state.mode === 'edit' ? 'Saving…' : 'Creating…') : (state.mode === 'edit' ? 'Save' : 'Create team'), { team: 'save' }, { kind: 'primary', disabled: Boolean(state.pending) || state.read.pending || Boolean(state.read.problem) });
  const cancel = depButton('Cancel', { team: 'close' }, { kind: 'quiet' });
  let remove = '';
  if (state.mode === 'edit' && state.team) {
    remove = state.confirm
      ? `<span class="dp-confirm">Delete “${escapeHtml(state.team.name)}”? Its usage history goes with it. ${depButton(state.pending === 'delete' ? 'Deleting…' : 'Delete', { team: 'confirm-delete' }, { kind: 'danger', small: true, disabled: Boolean(state.pending) })}${depButton('Keep it', { team: 'cancel-delete' }, { kind: 'quiet', small: true, disabled: Boolean(state.pending) })}</span>`
      : depButton('Delete team', { team: 'delete' }, { kind: 'quiet', disabled: Boolean(state.pending) });
  }
  const said = summary(state);
  return '<footer class="dialog-foot">'
    + (said ? `<p class="dialog-summary" role="status">${escapeHtml(said)}</p>` : '')
    + depProblem(state.problem, state.mode === 'edit' ? 'Not saved.' : 'No team was created.')
    + `<div class="dialog-actions"><span class="dialog-start">${remove}</span>${cancel}${save}</div>`
    + '</footer>';
}

function body(state) {
  const name = '<form class="dp-form" data-team-form="save" autocomplete="off">'
    + '<label class="dp-label" for="team-name">Name your Team</label>'
    + `<input class="dp-input" id="team-name" maxlength="120" data-team-draft="name" placeholder="For example: Platform engineering" value="${escapeHtml(state.name)}" autofocus>`
    + '</form>';
  if (state.mode === 'edit') {
    if (state.read.pending) return `${name}<p class="dp-note" role="status">Reading the team…</p>`;
    if (state.read.problem) return name + depProblem(state.read.problem, 'The team could not be opened.');
    if (state.team.source !== 'console') {
      const count = state.team.members === null ? '' : ` ${plural(state.team.members, 'person is', 'people are')} in it now.`;
      return `${name}<p class="dp-note">${escapeHtml(teamSourceText(state.team))}${escapeHtml(count)} To follow something else, add a new team and delete this one.</p>`;
    }
  }
  return `${name}<div class="pick">${filterBar(state)}${searchBar(state)}${listNote(state)}${pickList(state)}</div>`;
}

/** The pop-up for one state, as the contents of its dialog element. Empty while it is closed. */
export function renderTeamDialog(state) {
  if (!state.open) return '';
  const edit = state.mode === 'edit';
  const eyebrow = edit ? 'Team' : TEAM_KINDS[state.kind]?.label ?? '';
  const lead = edit ? (state.team?.source === 'console' ? 'Tick or untick people to change who is in the team.' : '') : KIND_LEADS[state.kind] ?? '';
  return '<div class="dialog-panel" tabindex="-1">'
    + '<header class="dialog-head">'
    + `<div><span class="eyebrow">${escapeHtml(eyebrow)}</span><h3 id="team-dialog-title">${edit ? 'Edit team' : 'Add a Team'}</h3>`
    + (lead ? `<p class="dialog-lead">${escapeHtml(lead)}</p>` : '') + '</div>'
    + '<button type="button" class="dialog-x" data-team="close" aria-label="Close">×</button>'
    + '</header>'
    + `<div class="dialog-body">${body(state)}</div>`
    + footer(state)
    + '</div>';
}
