// team-editor.js — Teams → "+ Add a Team" and a team's "Edit team": the pop-up's state and every
// action in it.
//
// A team is either the people an admin ticks, or follows one group, department or organisational
// unit, whose members the directory decides. The three ways to add one differ in what they list:
// Manual lists everyone Sundial knows; IdP Group and SCIM list what the identity provider has
// synced (people, groups, departments, units), with a type filter and a search. Sundial keeps one
// directory whichever way it was filled, so the two list the same objects; IdP Group's search
// also asks Microsoft Entra ID for groups not synced yet, which are imported when the team is made.
//
// The selection follows what a team can be: once a group, department or unit is ticked nothing
// else can be, and once a person is ticked none of those can be.
//
// Every write goes through the admin API, which control-api answers for an admin only and audits;
// listing people goes through the query API's people list, which is audited as a subject-level
// read. DOM-free, like directory.js: a test drives it without a browser.

import { buildPeopleDocument } from './dsl.js';

/** The ways to add a team, in the order the menu offers them, and the list each opens on. */
export const TEAM_KINDS = Object.freeze({
  idp: Object.freeze({ id: 'idp', label: 'IdP Group', filter: 'group' }),
  scim: Object.freeze({ id: 'scim', label: 'SCIM', filter: 'all' }),
  manual: Object.freeze({ id: 'manual', label: 'Manual', filter: 'user' }),
});

/** The kinds of synced object the filter offers: what the directory holds and a team can use. */
export const OBJECT_TYPES = Object.freeze({
  all: 'All',
  user: 'Users',
  group: 'Groups',
  department: 'Departments',
  org_unit: 'Units',
});

/** Objects a team follows; a team follows exactly one of them. */
const FOLLOWED = Object.freeze(['group', 'department', 'org_unit']);

const PAGE = 200;
/** control-api takes at most this many people in one create or change. */
const BATCH = 500;

const LIST = Object.freeze({ pending: false, problem: null, rows: Object.freeze([]) });
const PEOPLE = Object.freeze({ ...LIST, next: null });
const GROUPS = Object.freeze({ ...LIST, entra: Object.freeze([]) });

const CLOSED = Object.freeze({
  open: false,
  mode: null,
  kind: null,
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
  members: Object.freeze({ pending: false, problem: null, rows: null }),
  picked: Object.freeze({}),
  pending: null,
  problem: null,
  confirm: false,
});

/** A team name suggested from what it follows: a unit by the first part of its distinguished name. */
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

function valueItem(type, v) {
  return Object.freeze({ key: `${type}:${v.value}`, type, value: String(v.value), name: type === 'org_unit' ? unitName(v.value) : String(v.value), people: v.people ?? null });
}

const byName = (a, b) => a.name.localeCompare(b.name, undefined, { sensitivity: 'base' });

/** The objects of one type that match the search. In edit, the team's people come before everyone else. */
function itemsOf(state, type) {
  const term = state.term.toLowerCase();
  const match = (item) => term === '' || item.name.toLowerCase().includes(term) || String(item.value ?? '').toLowerCase().includes(term);
  switch (type) {
    case 'group': return [...state.groups.rows.filter(match), ...state.groups.entra];
    case 'department': return state.departments.rows.filter(match);
    case 'org_unit': return state.units.rows.filter(match);
    case 'user': {
      const members = state.members.rows ?? [];
      const known = new Set(members.map((m) => m.key));
      // The people read decides who matches a name, so a member is listed when it returned them.
      const found = new Set(state.people.rows.map((p) => p.key));
      return [...members.filter((m) => term === '' || found.has(m.key)), ...state.people.rows.filter((p) => !known.has(p.key))];
    }
    default: return [];
  }
}

/** What the list shows for a state: the synced groups, departments and units, then the people. */
export function visibleItems(state) {
  const types = state.kind === 'manual' ? ['user'] : state.filter === 'all' ? ['group', 'department', 'org_unit', 'user'] : [state.filter];
  return types.flatMap((type) => itemsOf(state, type));
}

/** How many of each type match the search, for the filter; null while that type is being read. */
export function countsOf(state) {
  const pending = { user: state.people.pending, group: state.groups.pending, department: state.departments.pending, org_unit: state.units.pending };
  return Object.freeze(Object.fromEntries(Object.keys(pending).map((type) => [type, pending[type] ? null : itemsOf(state, type).length])));
}

/** Why an item cannot be ticked now, or null when it can. */
export function blockedReason(state, item) {
  if (state.picked[item.key]) return null;
  const picked = Object.values(state.picked);
  if (picked.length === 0) return null;
  const following = picked.find((p) => FOLLOWED.includes(p.type));
  if (following) return `The team follows ${following.name}. Untick it to choose something else.`;
  if (FOLLOWED.includes(item.type)) return 'A team follows one group, department or unit on its own. Untick the people to choose it.';
  return null;
}

/** May IdP Group's search ask Entra? Only with the directory read on: a group found there is imported by it. */
export function entraSearchable(state) {
  return state.kind === 'idp' && Boolean(state.directory?.graph_available && state.directory?.entra_connected && state.directory?.sync);
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
      const envelope = await query.run(buildPeopleDocument({ search: state.term, synced: state.kind !== 'manual', cursor, limit: PAGE }));
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

  async function readDirectory() {
    const seq = session;
    const answer = await admin.directory();
    if (seq === session) set({ directory: answer.state === 'available' ? answer.data : null });
  }

  /** Open the pop-up to add a team of one kind, and read what it lists. */
  async function open(kind) {
    if (!TEAM_KINDS[kind]) return state;
    session += 1;
    set({ ...CLOSED, open: true, mode: 'create', kind, filter: TEAM_KINDS[kind].filter });
    if (kind === 'manual') {
      await readPeople();
      return state;
    }
    await Promise.all([readDirectory(), readGroups(), readValues('departments', 'department'), readValues('units', 'org_unit'), readPeople()]);
    return state;
  }

  /** Open the pop-up on one team: its name, and for a team of chosen people, who is in it. */
  async function edit(teamId) {
    if (!teamId) return state;
    session += 1;
    const seq = session;
    set({ ...CLOSED, open: true, mode: 'edit', read: Object.freeze({ pending: true, problem: null }) });
    const answer = await admin.teams();
    if (seq !== session) return state;
    const team = answer.state === 'available' ? answer.teams.find((t) => t.team_id === teamId) ?? null : null;
    if (!team) {
      set({ read: Object.freeze({ pending: false, problem: answer.state === 'available' ? Object.freeze({ code: 'not_found', message: 'This team no longer exists.' }) : problemOf(answer, 'The team could not be read.') }) });
      return state;
    }
    const manual = team.source === 'console';
    set({ team, name: team.name, kind: manual ? 'manual' : null, filter: 'user', read: CLOSED.read, members: Object.freeze({ ...CLOSED.members, pending: manual }) });
    if (!manual) return state;
    const [members] = await Promise.all([admin.teamMembers(teamId), readPeople()]);
    if (seq !== session) return state;
    if (members.state !== 'available') {
      set({ members: Object.freeze({ pending: false, problem: problemOf(members, 'The team\'s people could not be read.'), rows: null }) });
      return state;
    }
    const rows = Object.freeze(members.members.map((m) => personItem({ subject: m.user_ref, name: m.name, department: m.department })).sort(byName));
    set({ members: Object.freeze({ pending: false, problem: null, rows }), picked: Object.freeze(Object.fromEntries(rows.map((r) => [r.key, r]))) });
    return state;
  }

  function close() {
    session += 1;
    if (state.open) set(CLOSED);
    return state;
  }

  function setFilter(type) {
    if (OBJECT_TYPES[type] && state.open && state.kind !== 'manual') set({ filter: type });
    return state;
  }

  /** Search what the list shows: people by the start of their name, everything else by any part of it. */
  async function search() {
    if (!state.open || !state.kind) return state;
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
    const lists = [state.people.rows, state.members.rows ?? [], state.groups.rows, state.groups.entra, state.departments.rows, state.units.rows];
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
    // Following a group, department or unit names the team after it, unless it has a name already.
    const name = FOLLOWED.includes(item.type) && state.name.trim() === '' ? item.name : state.name;
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
    if (picked.length === 0) return fail(state.kind === 'manual' ? 'Tick the people in the team.' : 'Tick the people in the team, or the group, department or unit it follows.');
    const followed = picked.find((p) => FOLLOWED.includes(p.type));
    let body;
    let rest = [];
    if (!followed) {
      const [first = [], ...later] = chunks(picked.map((p) => p.ref));
      body = { name, source: 'console', members: first };
      rest = later;
    } else if (followed.type === 'group') {
      body = followed.id ? { name, source: 'group', group_id: followed.id } : { name, source: 'group', group_object_id: followed.object_id };
    } else {
      body = { name, source: followed.type, match_value: followed.value };
    }
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
    if (team.source === 'console' && !state.members.rows) return fail('The team\'s people could not be read, so they cannot be changed. Close the team and open it again.', 'members_unread');
    set({ pending: 'save', problem: null });
    if (name !== team.name) {
      const renamed = await admin.renameTeam(team.team_id, name);
      if (renamed.state !== 'done') return fail(renamed.error?.message ?? 'The team was not renamed.', renamed.error?.code ?? 'refused');
    }
    if (team.source === 'console') {
      const before = new Set(state.members.rows.map((m) => m.ref));
      const after = new Set(Object.values(state.picked).map((p) => p.ref));
      const add = [...after].filter((ref) => !before.has(ref));
      const remove = [...before].filter((ref) => !after.has(ref));
      const adds = chunks(add);
      const removes = chunks(remove);
      for (let i = 0; i < Math.max(adds.length, removes.length); i += 1) {
        const answer = await admin.changeTeamMembers(team.team_id, { add: adds[i] ?? [], remove: removes[i] ?? [] });
        if (answer.state !== 'done') {
          onSaved({ team, deleted: false });
          return fail(answer.error?.message ?? 'The team\'s people were not changed.', answer.error?.code ?? 'refused');
        }
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
      case 'open': return open(dataset.kind);
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
