// directory.js — Settings → Directory & teams: the state of the page and every action on it.
//
// Where an admin decides who the product knows and how they are grouped: whether people and
// groups are read from Microsoft Entra, and the teams the Teams page reports on. A team is made in
// the console with its members chosen here, or follows a directory group, a department or an
// organisational unit, whose members the directory decides. Every read and write goes through the
// admin api, which control-api answers for an admin only and audits; finding a person to add to a
// team goes through the query API's people list, which is audited as a subject-level read.
//
// DOM-free, like deployment.js: it holds the state, makes the calls and tells a listener when the
// state changed, so a test can drive the page without a browser.

import { buildPeopleDocument } from './dsl.js';

/** Where a team's members come from, in the order the page offers them. */
export const TEAM_SOURCES = Object.freeze({
  console: Object.freeze({ id: 'console', label: 'Chosen here' }),
  group: Object.freeze({ id: 'group', label: 'A directory group' }),
  department: Object.freeze({ id: 'department', label: 'A department' }),
  org_unit: Object.freeze({ id: 'org_unit', label: 'An organisational unit' }),
});

/** What a failed directory read means, for the codes graphsync records. */
export const SYNC_ERRORS = Object.freeze({
  entra_not_connected: 'This tenant has no active Microsoft Entra connection, so there is no directory to read.',
  graph_consent_missing: 'Microsoft Graph refused the read. An Entra administrator must grant the Sundial application the User.Read.All and GroupMember.Read.All permissions.',
  graph_token_refused: 'Microsoft Entra would not issue a token for this tenant. The application may have been removed from it, or its consent withdrawn.',
  graph_unavailable: 'Microsoft Graph could not be reached or failed. The next pass tries again.',
  people_not_synced: 'Some people could not be written. The rest were; the next pass tries again.',
  internal: 'The read failed inside Sundial. The next pass tries again.',
});

const IDLE = Object.freeze({
  sync: Object.freeze({ pending: null, problem: null, queued: null }),
  create: Object.freeze({ pending: false, problem: null }),
  choices: Object.freeze({ pending: false, problem: null, groups: null, values: null }),
  team: Object.freeze({ open: null, pending: false, problem: null, members: null, limit: null, confirm: false }),
  people: Object.freeze({ pending: false, problem: null, results: null }),
});

const EMPTY_DRAFTS = Object.freeze({ name: '', source: 'console', groupQuery: '', match: '', group: null, peopleQuery: '' });

/**
 * The page's behaviour over one admin api and, for finding people, one query api.
 *
 * @param {object} input
 * @param {ReturnType<import('./transport.js').createAdminApi>} input.admin
 * @param {{run: (body: object) => Promise<object>}} [input.query]
 * @param {(state: object) => void} [input.onChange]
 */
export function createDirectory({ admin, query = null, onChange = () => {} }) {
  let state = Object.freeze({ status: 'idle', data: null, problem: null, refreshProblem: null, ...IDLE, drafts: EMPTY_DRAFTS });
  let loadSeq = 0;

  function set(patch) {
    state = Object.freeze({ ...state, ...patch });
    onChange(state);
  }

  async function load({ quiet = false } = {}) {
    const seq = ++loadSeq;
    const keep = quiet && state.data !== null;
    if (!keep) set({ status: 'loading', problem: null, refreshProblem: null });
    const answer = await admin.directory();
    if (seq !== loadSeq) return state;
    if (answer.state === 'available') set({ status: 'ready', data: answer.data, problem: null, refreshProblem: null });
    else if (keep) set({ refreshProblem: answer.error });
    else set({ status: 'refused', problem: Object.freeze({ ...answer.error, status: answer.status }), data: null });
    return state;
  }

  async function setSync(enabled) {
    if (state.status !== 'ready' || state.sync.pending) return state;
    set({ sync: Object.freeze({ pending: enabled ? 'on' : 'off', problem: null, queued: null }) });
    const answer = await admin.setDirectorySync(enabled);
    set({ sync: Object.freeze({ pending: null, problem: answer.state === 'done' ? null : answer.error, queued: null }) });
    return answer.state === 'done' ? load({ quiet: true }) : state;
  }

  async function runSync() {
    if (state.status !== 'ready' || state.sync.pending) return state;
    set({ sync: Object.freeze({ pending: 'run', problem: null, queued: null }) });
    const answer = await admin.runDirectorySync();
    set({ sync: Object.freeze({ pending: null, problem: answer.state === 'done' ? null : answer.error, queued: answer.state === 'done' ? answer.queued : null }) });
    return answer.state === 'done' ? load({ quiet: true }) : state;
  }

  /** Choosing where a new team's members come from loads what can be picked for that source. */
  async function chooseSource(source) {
    if (!TEAM_SOURCES[source]) return state;
    set({ drafts: Object.freeze({ ...state.drafts, source, match: '', group: null }), choices: IDLE.choices, create: IDLE.create });
    if (source === 'department' || source === 'org_unit') {
      set({ choices: Object.freeze({ ...IDLE.choices, pending: true }) });
      const answer = await admin.directoryValues(source);
      if (state.drafts.source !== source) return state;
      set({ choices: Object.freeze({ ...IDLE.choices, problem: answer.state === 'available' ? null : answer.error, values: answer.state === 'available' ? answer.values : null }) });
    } else if (source === 'group') {
      return searchGroups();
    }
    return state;
  }

  async function searchGroups() {
    if (state.drafts.source !== 'group') return state;
    const q = state.drafts.groupQuery.trim();
    set({ choices: Object.freeze({ ...IDLE.choices, pending: true }) });
    const answer = await admin.directoryGroups(q);
    if (state.drafts.source !== 'group') return state;
    if (answer.state !== 'available') {
      set({ choices: Object.freeze({ ...IDLE.choices, problem: answer.error }) });
      return state;
    }
    // A group already provisioned is listed once, as provisioned, even when the search finds it too.
    const provisioned = answer.provisioned.filter((g) => !q || String(g.name ?? '').toLowerCase().includes(q.toLowerCase()));
    const known = new Set(provisioned.map((g) => g.object_id).filter(Boolean));
    const groups = [...provisioned, ...answer.directory.filter((g) => !known.has(g.object_id))];
    set({ choices: Object.freeze({ ...IDLE.choices, groups: Object.freeze(groups) }) });
    return state;
  }

  function pickGroup({ id = '', object = '', name = '' }) {
    if (!id && !object) return state;
    const group = Object.freeze({ id: id || null, object_id: object || null, name });
    set({ drafts: Object.freeze({ ...state.drafts, group, name: state.drafts.name || name }) });
    return state;
  }

  function pickValue(value) {
    const draft = String(value ?? '');
    set({ drafts: Object.freeze({ ...state.drafts, match: draft, name: state.drafts.name || shortName(state.drafts.source, draft) }) });
    return state;
  }

  async function createTeam() {
    if (state.status !== 'ready' || state.create.pending) return state;
    const { name, source, group, match } = state.drafts;
    const problem = (message) => {
      set({ create: Object.freeze({ pending: false, problem: Object.freeze({ code: 'incomplete', message }) }) });
      return state;
    };
    if (name.trim() === '') return problem('Give the team a name.');
    const body = { name: name.trim(), source };
    if (source === 'group') {
      if (!group) return problem('Choose the group the team follows.');
      if (group.id) body.group_id = group.id;
      else body.group_object_id = group.object_id;
    }
    if (source === 'department' || source === 'org_unit') {
      if (match.trim() === '') return problem(source === 'department' ? 'Choose the department.' : 'Choose the organisational unit.');
      body.match_value = match.trim();
    }
    set({ create: Object.freeze({ pending: true, problem: null }) });
    const answer = await admin.createTeam(body);
    if (answer.state !== 'done') {
      set({ create: Object.freeze({ pending: false, problem: answer.error }) });
      return state;
    }
    set({ create: IDLE.create, choices: IDLE.choices, drafts: EMPTY_DRAFTS });
    await load({ quiet: true });
    // A team chosen here starts empty: open it, so its members are the next thing done.
    if (source === 'console' && answer.team?.team_id) return openTeam(answer.team.team_id);
    return state;
  }

  async function openTeam(teamId) {
    if (!teamId) return state;
    // Reopening the same team after a change keeps the search, so several people can be added in turn.
    const same = state.team.open === teamId;
    set({ team: Object.freeze({ ...IDLE.team, open: teamId, pending: true, members: same ? state.team.members : null }), people: same ? state.people : IDLE.people });
    const answer = await admin.teamMembers(teamId);
    if (state.team.open !== teamId) return state;
    set({
      team: Object.freeze({
        ...IDLE.team, open: teamId,
        problem: answer.state === 'available' ? null : answer.error,
        members: answer.state === 'available' ? Object.freeze(answer.members) : null,
        limit: answer.state === 'available' ? answer.limit : null,
      }),
    });
    return state;
  }

  function closeTeam() {
    set({ team: IDLE.team, people: IDLE.people, drafts: Object.freeze({ ...state.drafts, peopleQuery: '' }) });
    return state;
  }

  async function changeMembers(change) {
    const teamId = state.team.open;
    if (!teamId || state.team.pending) return state;
    set({ team: Object.freeze({ ...state.team, pending: true, problem: null }) });
    const answer = await admin.changeTeamMembers(teamId, change);
    if (answer.state !== 'done') {
      set({ team: Object.freeze({ ...state.team, pending: false, problem: answer.error }) });
      return state;
    }
    await load({ quiet: true });
    return openTeam(teamId);
  }

  /** Find people to add by the start of their name, through the audited people list. */
  async function searchPeople() {
    const term = state.drafts.peopleQuery.trim();
    if (!query || term === '' || !state.team.open) return state;
    set({ people: Object.freeze({ ...IDLE.people, pending: true }) });
    try {
      const envelope = await query.run(buildPeopleDocument({ search: term, limit: 20 }));
      const rows = Array.isArray(envelope?.data) ? envelope.data : [];
      const problem = envelope?.error ? Object.freeze({ code: envelope.error.code ?? envelope.result_state, message: envelope.error.message ?? 'The people list was refused.' }) : null;
      set({ people: Object.freeze({ ...IDLE.people, problem, results: problem ? null : Object.freeze(rows) }) });
    } catch (error) {
      const e = error?.envelope?.error;
      set({ people: Object.freeze({ ...IDLE.people, problem: Object.freeze({ code: e?.code ?? 'people_unavailable', message: e?.message ?? 'The people list could not be read.' }) }) });
    }
    return state;
  }

  async function deleteTeam() {
    const teamId = state.team.open;
    if (!teamId || state.team.pending) return state;
    set({ team: Object.freeze({ ...state.team, pending: true, problem: null }) });
    const answer = await admin.deleteTeam(teamId);
    if (answer.state !== 'done') {
      set({ team: Object.freeze({ ...state.team, pending: false, confirm: false, problem: answer.error }) });
      return state;
    }
    set({ team: IDLE.team, people: IDLE.people });
    return load({ quiet: true });
  }

  /** What is typed into a field. Kept without a repaint, so typing never loses focus. */
  function setDraft(name, value) {
    if (!['name', 'groupQuery', 'match', 'peopleQuery'].includes(name)) return;
    state = Object.freeze({ ...state, drafts: Object.freeze({ ...state.drafts, [name]: String(value ?? '').slice(0, name === 'match' ? 1024 : 120) }) });
  }

  /** One entry point for the page's controls: `data-dir` names the action, the other data-* its argument. */
  function act(dataset = {}) {
    switch (dataset.dir) {
      case 'retry': return load();
      case 'sync-on': return setSync(true);
      case 'sync-off': return setSync(false);
      case 'sync-run': return runSync();
      case 'source': return chooseSource(dataset.value);
      case 'search-groups': return searchGroups();
      case 'pick-group': return Promise.resolve(pickGroup(dataset));
      case 'pick-value': return Promise.resolve(pickValue(dataset.value));
      case 'create': return createTeam();
      case 'open-team': return openTeam(dataset.id);
      case 'close-team': return Promise.resolve(closeTeam());
      case 'search-people': return searchPeople();
      case 'add-member': return dataset.ref ? changeMembers({ add: [dataset.ref] }) : Promise.resolve(state);
      case 'remove-member': return dataset.ref ? changeMembers({ remove: [dataset.ref] }) : Promise.resolve(state);
      case 'delete-team': set({ team: Object.freeze({ ...state.team, confirm: true }) }); return Promise.resolve(state);
      case 'cancel-delete': set({ team: Object.freeze({ ...state.team, confirm: false }) }); return Promise.resolve(state);
      case 'confirm-delete': return deleteTeam();
      default: return Promise.resolve(state);
    }
  }

  return Object.freeze({
    get state() { return state; },
    load, setSync, runSync, chooseSource, searchGroups, pickGroup, pickValue, createTeam,
    openTeam, closeTeam, changeMembers, searchPeople, deleteTeam, setDraft, act,
  });
}

/** A team name suggested from what it matches: a department as it is, a unit by its first part. */
function shortName(source, value) {
  if (source !== 'org_unit') return value;
  const first = value.split(/(?<!\\),/)[0] ?? '';
  return first.replace(/^\s*(OU|CN|DC)=/i, '').replace(/\\,/g, ',').trim();
}
