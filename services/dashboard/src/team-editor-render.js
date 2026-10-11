// team-editor-render.js — the Teams page's "+ Add a Team" menu, a team's "Edit team" button, and the
// pop-up they open, state to HTML. Pure: no DOM, no I/O, no state.
//
// Every string from an API is escaped, and a list that could not be read says so rather than
// showing as empty.

import { escapeHtml } from './render.js';
import { formatCount } from './format.js';
import { depButton, depProblem } from './deployment-render.js';
import { OBJECT_TYPES, TEAM_KINDS, blockedReason, countsOf, entraSearchable, heldKeys, offeredTypes, visibleItems } from './team-editor.js';

const KIND_HINTS = Object.freeze({
  idp: 'Groups from your directory, with anyone or anything else',
  scim: 'Users and groups your identity provider syncs',
  manual: 'People and teams you pick',
});

const FOLLOWS = 'Groups, departments, units and teams bring their members as your directory changes them.';

const KIND_LEADS = Object.freeze({
  idp: `Tick any mix of groups, departments, units, people and teams from what your identity provider has synced to Sundial. ${FOLLOWS}`,
  scim: `Tick any mix of the users and groups synced to Sundial, the departments and units they carry, and other teams. ${FOLLOWS}`,
  manual: 'Tick the people and teams in the team. People stay in it until you change it; a team brings its members as they change.',
});

const TYPE_LABELS = Object.freeze({ user: 'User', group: 'Group', department: 'Department', org_unit: 'Unit', team: 'Team' });

/** What each type is called when counted, one and many. */
const TYPE_NOUNS = Object.freeze({
  user: ['person', 'people'],
  group: ['group', 'groups'],
  department: ['department', 'departments'],
  org_unit: ['unit', 'units'],
  team: ['team', 'teams'],
});

const plural = (n, one, many) => `${formatCount(n)} ${n === 1 ? one : many}`;

/** "a", "a and b", "a, b and c". */
const listed = (parts) => (parts.length < 2 ? parts.join('') : `${parts.slice(0, -1).join(', ')} and ${parts.at(-1)}`);

/** How many of each type the items hold, in words: "2 people, 1 group and 1 team". */
function tally(items) {
  return listed(Object.keys(TYPE_NOUNS).map((type) => {
    const n = items.filter((item) => item.type === type).length;
    return n ? plural(n, ...TYPE_NOUNS[type]) : '';
  }).filter(Boolean));
}

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

function detailOf(item) {
  switch (item.type) {
    case 'user': return item.department ?? '';
    case 'group': return item.entra ? 'In Microsoft Entra ID, not synced yet' : item.members === null ? '' : plural(item.members, 'member', 'members');
    case 'department': return item.people === null ? '' : plural(item.people, 'person', 'people');
    case 'org_unit': return [item.people === null ? '' : plural(item.people, 'person', 'people'), item.value].filter(Boolean).join(' · ');
    case 'team': return item.members === null ? '' : plural(item.members, 'member', 'members');
    default: return '';
  }
}

function row(state, item) {
  const picked = Boolean(state.picked[item.key]);
  const blocked = blockedReason(state, item);
  const disabled = Boolean(blocked) || Boolean(state.pending);
  const type = `<span class="pick-type">${escapeHtml(TYPE_LABELS[item.type] ?? item.type)}</span>`;
  const detail = detailOf(item);
  return `<li class="pick-row${blocked ? ' is-blocked' : ''}"><label class="pick-label"${blocked ? ` title="${escapeHtml(blocked)}"` : ''}>`
    + `<input type="checkbox" class="pick-box" data-team-pick="${escapeHtml(item.key)}"${picked ? ' checked' : ''}${disabled ? ' disabled' : ''}>`
    + `<span class="pick-name">${escapeHtml(item.name)}</span>${type}`
    + (detail ? `<span class="pick-detail">${escapeHtml(detail)}</span>` : '')
    + '</label></li>';
}

function filterBar(state) {
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
  const types = ['all', ...['user', 'group', 'department', 'org_unit', 'team'].filter((type) => offeredTypes(state).includes(type))];
  return `<div class="seg dp-seg pick-filter" role="group" aria-label="Show">${types.map((type) => item([type, OBJECT_TYPES[type]])).join('')}</div>`;
}

function searchBar(state) {
  const placeholder = state.kind === 'manual' ? 'Search people by the start of their name, and teams' : 'Search people, groups, departments, units and teams';
  return '<form class="pick-search" data-team-form="search" role="search" autocomplete="off">'
    + `<input class="dp-input" id="team-query" type="search" maxlength="120" data-team-draft="query" aria-label="Search" placeholder="${escapeHtml(placeholder)}" value="${escapeHtml(state.query)}">`
    + depButton(state.people.pending ? 'Searching…' : 'Search', { team: 'search' }, { disabled: state.people.pending })
    + '</form>';
}

/** A line under the search: what the search reaches in Entra, and what is not synced. */
function listNote(state) {
  if ((state.kind !== 'idp' && state.mode !== 'edit') || !state.directory) return '';
  if (entraSearchable(state)) return '<p class="dp-note">Searching also looks in Microsoft Entra ID for groups not synced yet; one is imported when the team is saved.</p>';
  if (state.kind === 'idp' && state.directory.graph_available && state.directory.entra_connected) {
    return '<p class="dp-note">A group is synced only once a team includes it. To search Microsoft Entra ID for one, turn on reading the directory under <a href="#directory">Identity Providers</a>.</p>';
  }
  return '';
}

function emptyText(state) {
  const term = state.term;
  if (state.filter === 'team') return term ? `No team's name has “${term}” in it.` : 'There are no other teams yet.';
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
  const loading = state.people.pending || state.groups.pending || state.departments.pending || state.units.pending || state.teams.pending || state.members.pending;
  const items = visibleItems(state);
  const rows = items.map((item) => row(state, item)).join('');
  const empty = items.length === 0
    ? `<li class="pick-empty">${loading ? 'Reading…' : escapeHtml(emptyText(state))}`
      + (!loading && !state.term && state.kind !== 'manual' && state.filter !== 'team' ? ' Turn on reading from Microsoft Entra ID under <a href="#directory">Identity Providers</a>, or set up SCIM under <a href="#deployment">Deployment</a>.' : '')
      + '</li>'
    : '';
  const wantsPeople = state.filter === 'all' || state.filter === 'user';
  const more = wantsPeople && state.people.next
    ? `<div class="pick-more">${depButton(state.people.pending ? 'Reading…' : 'Show more people', { team: 'more' }, { small: true, disabled: state.people.pending })}</div>`
    : '';
  const problems = [state.people.problem, state.groups.problem, state.departments.problem, state.units.problem, state.teams.problem, state.members.problem]
    .filter(Boolean).map((p) => depProblem(p, 'Not all of the list could be read.')).join('');
  return `<ul class="pick-list" id="team-pick-list" aria-label="${state.kind === 'manual' ? 'People and teams' : 'People, groups and teams'}" aria-busy="${loading ? 'true' : 'false'}">${rows}${empty}</ul>${more}${problems}`;
}

function summary(state) {
  const picked = Object.values(state.picked);
  if (state.mode === 'edit') {
    const held = heldKeys(state);
    if (!held) return '';
    const adding = picked.filter((p) => !held.has(p.key)).length;
    const removing = [...held].filter((key) => !state.picked[key]).length;
    const changes = [adding ? `${formatCount(adding)} to add` : '', removing ? `${formatCount(removing)} to remove` : ''].filter(Boolean).join(', ');
    return picked.length === 0
      ? `The team will be empty${changes ? ` (${changes})` : ''}.`
      : `The team holds ${tally(picked)}${changes ? ` (${changes})` : ''}.`;
  }
  if (picked.length === 0) return state.kind === 'manual' ? 'Tick the people and teams in the team.' : 'Tick any mix of people, groups, departments, units and teams.';
  const follows = picked.some((p) => p.type !== 'user') ? ' Groups, departments, units and teams bring their members as they change.' : '';
  const imported = picked.some((p) => p.entra) ? ' A group from Microsoft Entra ID is imported when the team is made.' : '';
  return `${tally(picked)} ticked.${follows}${imported}`;
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
  }
  return `${name}<div class="pick">${filterBar(state)}${searchBar(state)}${listNote(state)}${pickList(state)}</div>`;
}

/** The pop-up for one state, as the contents of its dialog element. Empty while it is closed. */
export function renderTeamDialog(state) {
  if (!state.open) return '';
  const edit = state.mode === 'edit';
  const eyebrow = edit ? 'Team' : TEAM_KINDS[state.kind]?.label ?? '';
  const lead = edit
    ? `Tick or untick people, groups, departments, units and teams to change who is in the team.${state.team?.members === null || state.team?.members === undefined ? '' : ` ${plural(state.team.members, 'person is', 'people are')} in it now.`}`
    : KIND_LEADS[state.kind] ?? '';
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
