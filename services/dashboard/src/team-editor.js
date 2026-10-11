// team-editor.js — Teams → "+ Add a Team" and a team's "Edit team": the pop-up's state and every
// action in it.
//
// A team is any mix of the people an admin ticks and the groups, departments, organisational units
// and other teams it includes, whose members the directory decides. The pop-up lists all of them,
// whichever way the directory was filled (SCIM or the read from Microsoft Entra ID), with a type
// filter and a search; with the directory read on, the search also asks Entra for groups not synced
// yet, which are imported when the team is saved. Editing a team ticks what it holds.
//
// A team cannot include a team that already includes it, so those are listed but cannot be ticked.
//
// Every write goes through the admin API, which control-api answers for an admin only and audits;
// listing people goes through the query API's people list, which is audited as a subject-level
// read. DOM-free, like directory.js: a test drives it without a browser.

import { buildPeopleDocument } from './dsl.js';

/** The kinds of object the filter offers: what the directory holds, and the other teams. */
export const OBJECT_TYPES = Object.freeze({
  all: 'All',
  user: 'Users',
  group: 'Groups',
  department: 'Departments',
  org_unit: 'Units',
  team: 'Teams',
});

/** The types the list shows, in the order "All" shows them. */
const LISTED = Object.freeze(['group', 'department', 'org_unit', 'team', 'user']);

const PAGE = 200;
/** control-api takes at most this many people in one create or change. */
const BATCH = 500;

const LIST = Object.freeze({ pending: false, problem: null, rows: Object.freeze([]) });
const PEOPLE = Object.freeze({ ...LIST, next: null });
const GROUPS = Object.freeze({ ...LIST, entra: Object.freeze([]) });
const TEAMS = Object.freeze({ ...LIST, includes: Object.freeze({}) });

const CLOSED = Object.freeze({
  open: false,
  mode: null,
  team: null,
  read: Object.freeze({ pending: false, problem: null }),
  name: '',
  query: '',
  term: '',
  filter: 'all',
  directory: null,
  people: PEOPLE,
  groups: GROUPS,
  departments: LIST,
  units: LIST,
  teams: TEAMS,
  members: Object.freeze({ pending: false, problem: null, rows: null }),
  picked: Object.freeze({}),
  pending: null,
  problem: null,
  confirm: false,
});

/** A team name suggested from what it includes: a unit by the first part of its distinguished name. */
export function unitName(value) {
  const first = String(value ?? '').split(/(?<!\\),/)[0] ?? '';
  return first.replace(/^\s*(OU|CN|DC)=/i, '').replace(/\\,/g, ',').trim() || String(value ?? '');
}

function personItem(row) {
  const ref = String(row.subject ?? row.user_ref ?? '');
  return Object.freeze({ key: `user:${ref}`, type: 'user', ref, name: String(row.name || ref), department: row.department ?? null });
}

function heldGroup(g) {
  return Object.freeze({ key: `group:${g.id}`, type: 'group', id: g.id, object_id: g.object_id ?? null, name: String(g.name ?? ''), members: g.members ?? null, entra: false });
}

function entraGroup(g) {
  return Object.freeze({ key: `entra:${g.object_id}`, type: 'group', id: null, object_id: g.object_id, name: String(g.name ?? ''), members: null, entra: true });
}

/** A department or unit, keyed case-insensitively as the server compares them. */
function valueItem(type, v) {
  return Object.freeze({ key: `${type}:${String(v.value).toLowerCase()}`, type, value: String(v.value), name: type === 'org_unit' ? unitName(v.value) : String(v.value), people: v.people ?? null });
}

function teamItem(t) {
  return Object.freeze({ key: `team:${t.team_id}`, type: 'team', id: t.team_id, name: String(t.name ?? ''), members: t.members ?? null });
}

/** One thing a team includes, as the item the list ticks for it. */
function includedItem(include) {
  switch (include.kind) {
    case 'group': return heldGroup({ id: include.group_id, object_id: include.group_object_id, name: include.name ?? include.group_object_id ?? '' });
    case 'team': return teamItem({ team_id: include.team_id, name: include.name });
    case 'department':
    case 'org_unit': return valueItem(include.kind, { value: include.value });
    default: return null;
  }
}

/** What the admin API takes for one ticked item that is not a person. */
function includeOf(item) {
  switch (item.type) {
    case 'group': return item.id ? { kind: 'group', group_id: item.id } : { kind: 'group', group_object_id: item.object_id };
    case 'team': return { kind: 'team', team_id: item.id };
    default: return { kind: item.type, value: item.value };
  }
}

/** Whether team `from` reaches team `to` through the teams it includes, at any depth. */
function reaches(includes, from, to) {
  const seen = new Set();
  const next = [from];
  while (next.length) {
    const id = next.pop();
    if (id === to) return true;
    if (seen.has(id)) continue;
    seen.add(id);
    next.push(...(includes[id] ?? []));
  }
  return false;
}

const byName = (a, b) => a.name.localeCompare(b.name, undefined, { sensitivity: 'base' });

/**
 * The objects of one type that match the search. What is ticked comes first, so a team's own
 * people, and anything it includes that the directory no longer lists, are always shown.
 */
function itemsOf(state, type) {
  const term = state.term.toLowerCase();
  const match = (item) => term === '' || item.name.toLowerCase().includes(term) || String(item.value ?? '').toLowerCase().includes(term);
  if (type === 'user') {
    const members = state.members.rows ?? [];
    const known = new Set(members.map((m) => m.key));
    // The people read decides who matches a name, so a member is listed when it returned them.
    const found = new Set(state.people.rows.map((p) => p.key));
    return [...members.filter((m) => term === '' || found.has(m.key)), ...state.people.rows.filter((p) => !known.has(p.key))];
  }
  const rows = {
    group: [...state.groups.rows.filter(match), ...state.groups.entra],
    department: state.departments.rows.filter(match),
    org_unit: state.units.rows.filter(match),
    team: state.teams.rows.filter((t) => t.id !== state.team?.team_id && match(t)),
  }[type] ?? [];
  const listed = new Set(rows.map((item) => item.key));
  const unlisted = Object.values(state.picked).filter((p) => p.type === type && !listed.has(p.key) && match(p));
  return [...unlisted, ...rows];
}

/** What the list shows for a state: the synced groups, departments and units, the teams, then the people. */
export function visibleItems(state) {
  const types = state.filter === 'all' ? LISTED : [state.filter];
  return types.flatMap((type) => itemsOf(state, type));
}

/** How many of each type match the search, for the filter; null while that type is being read. */
export function countsOf(state) {
  const pending = { user: state.people.pending, group: state.groups.pending, department: state.departments.pending, org_unit: state.units.pending, team: state.teams.pending };
  return Object.freeze(Object.fromEntries(Object.keys(pending).map((type) => [type, pending[type] ? null : itemsOf(state, type).length])));
}

/** Why an item cannot be ticked now, or null when it can. */
export function blockedReason(state, item) {
  if (state.picked[item.key] || item.type !== 'team' || !state.team) return null;
  return reaches(state.teams.includes, item.id, state.team.team_id) ? `${item.name} already includes this team.` : null;
}

/** May the search ask Entra? Only with the directory read on: a group found there is imported by it. */
export function entraSearchable(state) {
  return Boolean(state.directory?.graph_available && state.directory?.entra_connected && state.directory?.sync);
}

/**
 * The pop-up's behaviour over one admin api and one query api.
 *
 * @param {object} input
 * @param {ReturnType<import('./transport.js').createAdminApi>} input.admin
 * @param {{run: (body: object) => Promise<object>}} input.query
 * @param {(state: object) => void} [input.onChange]
 * @param {(change: {team: object|null, deleted: boolean}) => void} [input.onSaved] a team was made, changed or deleted
 */
export function createTeamEditor({ admin, query, onChange = () => {}, onSaved = () => {} }) {
  let state = CLOSED;
  // Each open is a session; an answer that arrives after its pop-up closed is dropped.
  let session = 0;

  function set(patch) {
    state = Object.freeze({ ...state, ...patch });
    onChange(state);
  }

  const problemOf = (answer, fallback) => Object.freeze({ code: answer?.error?.code ?? 'refused', message: answer?.error?.message ?? fallback });

  async function readPeople({ append = false } = {}) {
    const seq = session;
    const cursor = append ? state.people.next : null;
    set({ people: Object.freeze({ ...state.people, pending: true, problem: null }) });
    let people;
    try {
      // Everyone Sundial knows: people a directory synced, and people seen only on a device.
      const envelope = await query.run(buildPeopleDocument({ search: state.term, synced: false, cursor, limit: PAGE }));
      people = envelope?.error || !Array.isArray(envelope?.data)
        ? Object.freeze({ ...PEOPLE, rows: state.people.rows, problem: Object.freeze({ code: envelope?.error?.code ?? envelope?.result_state ?? 'refused', message: envelope?.error?.message ?? 'The people list was refused.' }) })
        : Object.freeze({
          ...PEOPLE,
          rows: Object.freeze([...(append ? state.people.rows : []), ...envelope.data.map(personItem)]),
          next: envelope.page?.next_cursor ?? null,
        });
    } catch (error) {
      const e = error?.envelope?.error;
      people = Object.freeze({ ...PEOPLE, rows: state.people.rows, problem: Object.freeze({ code: e?.code ?? 'people_unavailable', message: e?.message ?? 'The people list could not be read.' }) });
    }
    if (seq === session) set({ people });
  }

  async function readList(key, read) {
    const seq = session;
    set({ [key]: Object.freeze({ ...state[key], pending: true, problem: null }) });
    const next = await read();
    if (seq === session) set({ [key]: next });
  }

  const readGroups = () => readList('groups', async () => {
    const answer = await admin.directoryGroups('');
    if (answer.state !== 'available') return Object.freeze({ ...GROUPS, problem: problemOf(answer, 'The groups could not be read.') });
    return Object.freeze({ ...GROUPS, rows: Object.freeze(answer.provisioned.map(heldGroup).sort(byName)) });
  });

  const readValues = (key, kind) => readList(key, async () => {
    const answer = await admin.directoryValues(kind);
    if (answer.state !== 'available') return Object.freeze({ ...LIST, problem: problemOf(answer, 'The directory could not be read.') });
    return Object.freeze({ ...LIST, rows: Object.freeze(answer.values.map((v) => valueItem(kind, v)).sort(byName)) });
  });

  const teamsFrom = (teams) => Object.freeze({
    ...TEAMS,
    rows: Object.freeze(teams.map(teamItem).sort(byName)),
    includes: Object.freeze(Object.fromEntries(teams.map((t) => [t.team_id, (t.includes ?? []).filter((i) => i.kind === 'team').map((i) => i.team_id)]))),
  });

  const readTeams = () => readList('teams', async () => {
    const answer = await admin.teams();
    if (answer.state !== 'available') return Object.freeze({ ...TEAMS, problem: problemOf(answer, 'The teams could not be read.') });
    return teamsFrom(answer.teams);
  });

  async function readDirectory() {
    const seq = session;
    const answer = await admin.directory();
    if (seq === session) set({ directory: answer.state === 'available' ? answer.data : null });
  }

  /** Open the pop-up to add a team, and read everything it lists. */
  async function open() {
    session += 1;
    set({ ...CLOSED, open: true, mode: 'create', filter: 'all' });
    await Promise.all([readDirectory(), readGroups(), readValues('departments', 'department'), readValues('units', 'org_unit'), readTeams(), readPeople()]);
    return state;
  }

  /** Open the pop-up on one team: its name, and everything it could hold, with what it holds ticked. */
  async function edit(teamId) {
    if (!teamId) return state;
    session += 1;
    const seq = session;
    set({ ...CLOSED, open: true, mode: 'edit', filter: 'all', read: Object.freeze({ pending: true, problem: null }) });
    const answer = await admin.teams();
    if (seq !== session) return state;
    const team = answer.state === 'available' ? answer.teams.find((t) => t.team_id === teamId) ?? null : null;
    if (!team) {
      set({ read: Object.freeze({ pending: false, problem: answer.state === 'available' ? Object.freeze({ code: 'not_found', message: 'This team no longer exists.' }) : problemOf(answer, 'The team could not be read.') }) });
      return state;
    }
    const included = (team.includes ?? []).map(includedItem).filter(Boolean);
    set({
      team,
      name: team.name,
      read: CLOSED.read,
      teams: teamsFrom(answer.teams),
      members: Object.freeze({ ...CLOSED.members, pending: true }),
      picked: Object.freeze(Object.fromEntries(included.map((item) => [item.key, item]))),
    });
    const [members] = await Promise.all([admin.teamMembers(teamId), readDirectory(), readGroups(), readValues('departments', 'department'), readValues('units', 'org_unit'), readPeople()]);
    if (seq !== session) return state;
    if (members.state !== 'available') {
      set({ members: Object.freeze({ pending: false, problem: problemOf(members, 'The team\'s people could not be read.'), rows: null }) });
      return state;
    }
    const rows = Object.freeze(members.members.map((m) => personItem({ subject: m.user_ref, name: m.name, department: m.department })).sort(byName));
    set({ members: Object.freeze({ pending: false, problem: null, rows }), picked: Object.freeze({ ...state.picked, ...Object.fromEntries(rows.map((r) => [r.key, r])) }) });
    return state;
  }

  function close() {
    session += 1;
    if (state.open) set(CLOSED);
    return state;
  }

  function setFilter(type) {
    if (state.open && (type === 'all' || LISTED.includes(type))) set({ filter: type });
    return state;
  }

  /** Search what the list shows: people by the start of their name, everything else by any part of it. */
  async function search() {
    if (!state.open) return state;
    const seq = session;
    set({ term: state.query.trim(), groups: Object.freeze({ ...state.groups, entra: Object.freeze([]) }) });
    const reads = [readPeople()];
    if (state.term !== '' && entraSearchable(state)) {
      reads.push((async () => {
        const answer = await admin.directoryGroups(state.term);
        if (seq !== session) return;
        if (answer.state !== 'available') {
          set({ groups: Object.freeze({ ...state.groups, problem: problemOf(answer, 'Microsoft Entra ID could not be searched.') }) });
          return;
        }
        // A group already synced is listed once, as synced.
        const held = new Set(state.groups.rows.map((g) => g.object_id).filter(Boolean));
        const entra = answer.directory.filter((g) => g.object_id && !held.has(g.object_id)).map(entraGroup);
        set({ groups: Object.freeze({ ...state.groups, entra: Object.freeze(entra) }) });
      })());
    }
    await Promise.all(reads);
    return state;
  }

  function more() {
    if (!state.open || !state.people.next || state.people.pending) return Promise.resolve(state);
    return readPeople({ append: true }).then(() => state);
  }

  function itemOf(key) {
    const lists = [state.people.rows, state.members.rows ?? [], state.groups.rows, state.groups.entra, state.departments.rows, state.units.rows, state.teams.rows];
    for (const list of lists) {
      const found = list.find((item) => item.key === key);
      if (found) return found;
    }
    return null;
  }

  function toggle(key) {
    if (!state.open || state.pending) return state;
    if (state.picked[key]) {
      const { [key]: _gone, ...rest } = state.picked;
      set({ picked: Object.freeze(rest), problem: null });
      return state;
    }
    const item = itemOf(key);
    if (!item || blockedReason(state, item)) return state;
    // A new team whose first tick is a group, department, unit or team is named after it.
    const first = Object.keys(state.picked).length === 0 && state.mode === 'create';
    const name = first && item.type !== 'user' && state.name.trim() === '' ? item.name : state.name;
    set({ picked: Object.freeze({ ...state.picked, [key]: item }), name, problem: null });
    return state;
  }

  const chunks = (list) => Array.from({ length: Math.ceil(list.length / BATCH) }, (_, i) => list.slice(i * BATCH, (i + 1) * BATCH));

  function fail(message, code = 'incomplete') {
    set({ pending: null, problem: Object.freeze({ code, message }) });
    return state;
  }

  async function create() {
    const name = state.name.trim();
    const picked = Object.values(state.picked);
    if (name === '') return fail('Name your team.');
    if (picked.length === 0) return fail('Tick the people, groups, departments, units or teams in the team.');
    const [first = [], ...rest] = chunks(picked.filter((p) => p.type === 'user').map((p) => p.ref));
    const body = { name, members: first, includes: picked.filter((p) => p.type !== 'user').map(includeOf) };
    set({ pending: 'save', problem: null });
    const answer = await admin.createTeam(body);
    if (answer.state !== 'done') return fail(answer.error?.message ?? 'The team was not created.', answer.error?.code ?? 'refused');
    const teamId = answer.team?.team_id;
    for (const add of rest) {
      const more = teamId ? await admin.changeTeamMembers(teamId, { add }) : { state: 'refused', error: { code: 'no_team_id', message: 'The new team was not named in the answer.' } };
      if (more.state !== 'done') {
        // The team exists; say who is missing rather than pretend nothing happened.
        onSaved({ team: answer.team ?? null, deleted: false });
        return fail(`The team was created, but not everyone was added: ${more.error?.message ?? 'the change was refused.'} Edit the team to add the rest.`, more.error?.code ?? 'refused');
      }
    }
    onSaved({ team: answer.team ?? null, deleted: false });
    return close();
  }

  async function update() {
    const team = state.team;
    const name = state.name.trim();
    if (name === '') return fail('Name your team.');
    if (!state.members.rows) return fail('The team\'s people could not be read, so the team cannot be changed. Close the team and open it again.', 'members_unread');
    set({ pending: 'save', problem: null });
    if (name !== team.name) {
      const renamed = await admin.renameTeam(team.team_id, name);
      if (renamed.state !== 'done') return fail(renamed.error?.message ?? 'The team was not renamed.', renamed.error?.code ?? 'refused');
    }
    const picked = Object.values(state.picked);
    const includes = picked.filter((p) => p.type !== 'user');
    const before = new Set((team.includes ?? []).map(includedItem).filter(Boolean).map((item) => item.key));
    if (includes.length !== before.size || includes.some((item) => !before.has(item.key))) {
      const answer = await admin.setTeamIncludes(team.team_id, includes.map(includeOf));
      if (answer.state !== 'done') {
        onSaved({ team, deleted: false });
        return fail(answer.error?.message ?? 'The groups, departments, units and teams were not changed.', answer.error?.code ?? 'refused');
      }
    }
    const had = new Set(state.members.rows.map((m) => m.ref));
    const has = new Set(picked.filter((p) => p.type === 'user').map((p) => p.ref));
    const adds = chunks([...has].filter((ref) => !had.has(ref)));
    const removes = chunks([...had].filter((ref) => !has.has(ref)));
    for (let i = 0; i < Math.max(adds.length, removes.length); i += 1) {
      const answer = await admin.changeTeamMembers(team.team_id, { add: adds[i] ?? [], remove: removes[i] ?? [] });
      if (answer.state !== 'done') {
        onSaved({ team, deleted: false });
        return fail(answer.error?.message ?? 'The team\'s people were not changed.', answer.error?.code ?? 'refused');
      }
    }
    onSaved({ team: { ...team, name }, deleted: false });
    return close();
  }

  async function save() {
    if (!state.open || state.pending) return state;
    if (state.mode === 'edit') return state.team ? update() : state;
    return create();
  }

  async function remove() {
    const team = state.team;
    if (!team || state.pending) return state;
    set({ pending: 'delete', problem: null });
    const answer = await admin.deleteTeam(team.team_id);
    if (answer.state !== 'done') {
      set({ pending: null, confirm: false, problem: problemOf(answer, 'The team was not deleted.') });
      return state;
    }
    onSaved({ team, deleted: true });
    return close();
  }

  /** What is typed into a field. Kept without a repaint, so typing never loses focus. */
  function setDraft(field, value) {
    if (field !== 'name' && field !== 'query') return;
    state = Object.freeze({ ...state, [field]: String(value ?? '').slice(0, 120) });
  }

  /** One entry point for the pop-up's controls: `data-team` names the action, the other data-* its argument. */
  function act(dataset = {}) {
    switch (dataset.team) {
      case 'open': return open();
      case 'edit': return edit(dataset.id);
      case 'close': return Promise.resolve(close());
      case 'filter': return Promise.resolve(setFilter(dataset.value));
      case 'search': return search();
      case 'more': return more();
      case 'toggle': return Promise.resolve(toggle(dataset.key));
      case 'save': return save();
      case 'delete': if (state.team && !state.pending) set({ confirm: true }); return Promise.resolve(state);
      case 'cancel-delete': set({ confirm: false }); return Promise.resolve(state);
      case 'confirm-delete': return remove();
      default: return Promise.resolve(state);
    }
  }

  return Object.freeze({
    get state() { return state; },
    open, edit, close, setFilter, search, more, toggle, save, setDraft, act,
  });
}

/** The keys a team held when its edit opened: its people and what it includes. Null until its people are read. */
export function heldKeys(state) {
  if (state.mode !== 'edit' || !state.team || !state.members.rows) return null;
  const included = (state.team.includes ?? []).map(includedItem).filter(Boolean);
  return new Set([...state.members.rows, ...included].map((item) => item.key));
}
