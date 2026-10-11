// team-editor.test.mjs — Teams → "+ Add a Team" and "Edit team": the pop-up's controller over a fake
// admin API and people list, the requests it makes, what it says, and how boot() places it.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createTeamEditor, unitName, visibleItems } from '../src/team-editor.js';
import { renderTeamAdd, renderTeamDialog, renderTeamEdit } from '../src/team-editor-render.js';
import { boot } from '../src/app.js';
import { createQueryApi } from '../src/transport.js';
import { validate } from '../../query-api/src/validate.js';
import { fixtureTransport } from './fixtures.mjs';
import { fakeDocument } from './helpers.mjs';

const ADA = `u_${'a'.repeat(32)}`;
const BOB = `u_${'b'.repeat(32)}`;
const CAT = `u_${'c'.repeat(32)}`;
const TEAM = '7d0c1a52-58f4-4a51-9b1e-1f6d2f0a0001';
const SALES = '00000000-0000-4000-8000-000000000900';
const SALES_OID = '11111111-2222-4333-8444-555555555555';
const EMEA_OID = '26118915-6090-4610-87a4-49d0767e9fde';

/** An admin API that answers from memory and records what was asked of it. */
function fakeAdmin({ sync = { enabled_at: '2026-10-01T10:00:00Z', last_status: 'ok' }, teams = [], members = [], fail = {} } = {}) {
  const calls = [];
  const refused = (code, message, status = 409) => ({ state: 'refused', status, error: { code, message } });
  return {
    calls,
    async directory() {
      calls.push(['directory']);
      return { state: 'available', data: { entra_connected: true, graph_available: true, people: 3, groups: 1, sync } };
    },
    async teams() {
      calls.push(['teams']);
      return { state: 'available', teams };
    },
    async directoryGroups(q) {
      calls.push(['groups', q]);
      return {
        state: 'available',
        provisioned: [{ id: SALES, object_id: SALES_OID, name: 'Sales', members: 4, directory: 'provisioned' }],
        directory: q ? [
          { object_id: SALES_OID, name: 'Sales', directory: 'entra', imported: true },
          { object_id: EMEA_OID, name: 'Sales EMEA', directory: 'entra', imported: false },
        ] : [],
      };
    },
    async directoryValues(kind) {
      calls.push(['values', kind]);
      return { state: 'available', values: kind === 'org_unit' ? [{ value: 'OU=Research,DC=contoso,DC=com', people: 3 }] : [{ value: 'Engineering', people: 7 }, { value: 'Sales', people: 2 }] };
    },
    async createTeam(body) {
      calls.push(['create', body]);
      if (fail.create) return refused(fail.create, 'A team already has this name.');
      return { state: 'done', team: { team_id: TEAM, name: body.name, source: body.source } };
    },
    async renameTeam(id, name) {
      calls.push(['rename', id, name]);
      return { state: 'done' };
    },
    async teamMembers(id) {
      calls.push(['members', id]);
      return { state: 'available', members, limit: 500 };
    },
    async changeTeamMembers(id, change) {
      calls.push(['change', id, change]);
      return { state: 'done' };
    },
    async deleteTeam(id) {
      calls.push(['delete', id]);
      return { state: 'done' };
    },
  };
}

const PEOPLE = [
  { subject: ADA, name: 'Ada Lovelace', department: 'Engineering', directory_status: 'active' },
  { subject: BOB, name: 'Bob Jones', department: 'Sales', directory_status: 'active' },
  { subject: CAT, name: 'Cat Ng', department: null, directory_status: null },
];

/** The people list, answering a name prefix and the synced filter as query-api would. */
function fakeQuery(rows = PEOPLE) {
  const sent = [];
  return {
    sent,
    async run(body) {
      sent.push(body);
      let data = rows;
      for (const f of body.filters) {
        if (f.field === 'name_key') data = data.filter((r) => r.name.toLowerCase().startsWith(f.value));
        if (f.field === 'directory_status') data = data.filter((r) => f.value.includes(r.directory_status));
      }
      return { result_state: 'ok', data, page: { returned: data.length, next_cursor: null } };
    },
  };
}

function editor({ admin = fakeAdmin(), query = fakeQuery() } = {}) {
  const saved = [];
  const page = createTeamEditor({ admin, query, onSaved: (change) => saved.push(change) });
  return { page, admin, query, saved };
}

const names = (state) => visibleItems(state).map((item) => item.name);

test('"+ Add a Team" offers IdP Group, SCIM and Manual, in that order', () => {
  const html = renderTeamAdd();
  assert.match(html, /<summary class="btn btn-primary">\+ Add a Team<\/summary>/);
  assert.deepEqual([...html.matchAll(/data-kind="([a-z]+)"/g)].map((m) => m[1]), ['idp', 'scim', 'manual']);
  assert.deepEqual([...html.matchAll(/class="add-menu-label">([^<]+)</g)].map((m) => m[1]), ['IdP Group', 'SCIM', 'Manual']);
  assert.match(renderTeamEdit(TEAM), new RegExp(`data-team="edit" data-id="${TEAM}"`));
});

test('Manual lists everyone Sundial knows with a checkbox each, and creates the team with the people ticked', async () => {
  const { page, admin, query, saved } = editor();
  await page.open('manual');
  assert.deepEqual(query.sent[0].filters, [], 'everyone, not only the people a directory synced');
  assert.deepEqual(names(page.state), ['Ada Lovelace', 'Bob Jones', 'Cat Ng']);
  const html = renderTeamDialog(page.state);
  assert.match(html, /<label class="dp-label" for="team-name">Name your Team<\/label>/);
  assert.equal((html.match(/type="checkbox"/g) ?? []).length, 3);
  assert.ok(!/data-team="filter"/.test(html), 'Manual lists people only, so it has no type filter');

  await page.save();
  assert.match(page.state.problem.message, /Name your team/);
  page.setDraft('name', '  Pilot ');
  await page.save();
  assert.match(page.state.problem.message, /Tick the people/);
  page.toggle(`user:${ADA}`);
  page.toggle(`user:${CAT}`);
  assert.match(renderTeamDialog(page.state), /2 people ticked\./);
  await page.save();
  assert.deepEqual(admin.calls.find((c) => c[0] === 'create')[1], { name: 'Pilot', source: 'console', members: [ADA, CAT] });
  assert.equal(page.state.open, false, 'the pop-up closes once the team exists');
  assert.equal(saved.length, 1);
  assert.equal(renderTeamDialog(page.state), '');
});

test('IdP Group lists what the directory synced, opens on groups, filters by type and searches every synced object', async () => {
  const { page, admin, query } = editor();
  await page.open('idp');
  assert.deepEqual(query.sent[0].filters, [{ field: 'directory_status', op: 'in', value: ['active', 'unknown'] }], 'people the directory holds, not the ones it marked gone');
  assert.equal(page.state.filter, 'group');
  assert.deepEqual(names(page.state), ['Sales']);
  let html = renderTeamDialog(page.state);
  assert.deepEqual([...html.matchAll(/data-team="filter" data-value="([a-z_]+)"/g)].map((m) => m[1]), ['all', 'user', 'group', 'department', 'org_unit']);
  assert.match(html, /Searching also looks in Microsoft Entra ID/);

  page.setFilter('all');
  assert.deepEqual(names(page.state), ['Sales', 'Engineering', 'Sales', 'Research', 'Ada Lovelace', 'Bob Jones'], 'groups, departments, units, then people; a person seen only on a device is not synced');
  html = renderTeamDialog(page.state);
  assert.match(html, /<span class="pick-type">Department<\/span>/);
  assert.match(html, /4 members/);

  page.setDraft('query', 'sal');
  await page.search();
  assert.deepEqual(admin.calls.filter((c) => c[0] === 'groups').map((c) => c[1]), ['', 'sal'], 'the search asks Entra too');
  assert.deepEqual(names(page.state), ['Sales', 'Sales EMEA', 'Sales'], 'a group already synced is listed once; nobody\'s name starts with "sal"');
  assert.deepEqual(query.sent.at(-1).filters.map((f) => f.field), ['name_key', 'directory_status']);
  assert.match(renderTeamDialog(page.state), /In Microsoft Entra ID, not synced yet/);
});

test('a team follows one group, department or unit, or holds the people ticked, never both', async () => {
  const { page } = editor();
  await page.open('scim');
  page.toggle(`user:${ADA}`);
  page.toggle(`group:${SALES}`);
  assert.deepEqual(Object.keys(page.state.picked), [`user:${ADA}`], 'a group cannot join people');
  let html = renderTeamDialog(page.state);
  assert.match(html, /data-team-pick="group:[^"]+" disabled/);
  assert.match(html, /1 person ticked\. The team holds exactly them\./);

  page.toggle(`user:${ADA}`);
  page.toggle(`department:Engineering`);
  page.toggle(`user:${BOB}`);
  page.toggle(`department:Sales`);
  assert.deepEqual(Object.keys(page.state.picked), ['department:Engineering'], 'nothing else joins what the team follows');
  assert.equal(page.state.name, 'Engineering', 'the team is named after what it follows');
  html = renderTeamDialog(page.state);
  assert.match(html, /The team follows everyone in the department Engineering\./);
  assert.match(html, /title="The team follows Engineering\. Untick it to choose something else\."/);
  await page.save();
  assert.deepEqual(page.state.open, false);
});

test('each followed object is created as the team the admin API knows', async () => {
  const cases = [
    { kind: 'idp', search: 'sales', key: `entra:${EMEA_OID}`, body: { name: 'Sales EMEA', source: 'group', group_object_id: EMEA_OID } },
    { kind: 'scim', key: `group:${SALES}`, body: { name: 'Sales', source: 'group', group_id: SALES } },
    { kind: 'scim', key: 'org_unit:OU=Research,DC=contoso,DC=com', body: { name: 'Research', source: 'org_unit', match_value: 'OU=Research,DC=contoso,DC=com' } },
  ];
  for (const c of cases) {
    const { page, admin } = editor();
    await page.open(c.kind);
    if (c.search) {
      page.setDraft('query', c.search);
      await page.search();
    }
    page.toggle(c.key);
    if (c.key.startsWith('entra:')) assert.match(renderTeamDialog(page.state), /imported from Microsoft Entra ID when the team is made/);
    await page.save();
    assert.deepEqual(admin.calls.find((call) => call[0] === 'create')[1], c.body, c.key);
  }
  assert.equal(unitName('OU=R\\, and D,OU=Labs,DC=contoso,DC=com'), 'R, and D');
});

test('with the directory read off, IdP Group searches only what is synced and says how to reach Entra', async () => {
  const { page, admin } = editor({ admin: fakeAdmin({ sync: null }) });
  await page.open('idp');
  page.setDraft('query', 'sales');
  await page.search();
  assert.deepEqual(admin.calls.filter((c) => c[0] === 'groups').map((c) => c[1]), [''], 'Entra is not asked');
  const html = renderTeamDialog(page.state);
  assert.match(html, /A group is synced only once a team follows it/);
  assert.ok(html.includes('href="#directory"'));
});

test('a refused create keeps the pop-up open and says why', async () => {
  const { page, saved } = editor({ admin: fakeAdmin({ fail: { create: 'team_name_taken' } }) });
  await page.open('manual');
  page.setDraft('name', 'Pilot');
  page.toggle(`user:${ADA}`);
  await page.save();
  assert.equal(page.state.open, true);
  assert.equal(saved.length, 0);
  assert.match(renderTeamDialog(page.state), /No team was created\.<\/strong> A team already has this name\. <code>team_name_taken<\/code>/);
});

test('editing a manual team shows its people ticked first, and saves a rename and the people added and removed', async () => {
  const admin = fakeAdmin({
    teams: [{ team_id: TEAM, name: 'Pilot', source: 'console', members: 2 }],
    members: [{ user_ref: BOB, name: 'Bob Jones' }, { user_ref: ADA, name: 'Ada Lovelace' }],
  });
  const { page, saved } = editor({ admin });
  await page.edit(TEAM);
  assert.equal(page.state.name, 'Pilot');
  assert.deepEqual(names(page.state), ['Ada Lovelace', 'Bob Jones', 'Cat Ng'], 'the team\'s people, then everyone else');
  assert.deepEqual(Object.keys(page.state.picked).sort(), [`user:${ADA}`, `user:${BOB}`]);
  page.toggle(`user:${BOB}`);
  page.toggle(`user:${CAT}`);
  assert.match(renderTeamDialog(page.state), /2 people in the team \(1 to add, 1 to remove\)\./);
  page.setDraft('name', 'Pilot crew');
  await page.save();
  assert.deepEqual(admin.calls.filter((c) => c[0] === 'rename' || c[0] === 'change'), [
    ['rename', TEAM, 'Pilot crew'],
    ['change', TEAM, { add: [CAT], remove: [BOB] }],
  ]);
  assert.equal(page.state.open, false);
  assert.equal(saved[0].team.name, 'Pilot crew');
});

test('a team that follows the directory is renamed or deleted, never given people', async () => {
  const admin = fakeAdmin({ teams: [{ team_id: TEAM, name: 'Sales', source: 'group', group_name: 'Sales', members: 4 }] });
  const { page, saved } = editor({ admin });
  await page.edit(TEAM);
  const html = renderTeamDialog(page.state);
  assert.match(html, /Follows the group Sales: its members, as your directory changes them\. 4 people are in it now\./);
  assert.ok(!/type="checkbox"/.test(html), 'its members are the directory\'s');
  assert.ok(!admin.calls.some((c) => c[0] === 'members'));
  await page.act({ team: 'delete' });
  assert.match(renderTeamDialog(page.state), /Delete “Sales”\? Its usage history goes with it\./);
  await page.act({ team: 'confirm-delete' });
  assert.deepEqual(admin.calls.at(-1), ['delete', TEAM]);
  assert.equal(saved.length, 1);
  assert.equal(saved[0].deleted, true);
  assert.equal(saved[0].team.team_id, TEAM);
  assert.equal(page.state.open, false);
});

test('an answer that arrives after the pop-up closed is dropped', async () => {
  let release;
  const slow = { run: () => new Promise((resolve) => { release = () => resolve({ result_state: 'ok', data: PEOPLE }); }) };
  const { page } = editor({ query: slow });
  const opening = page.open('manual');
  page.close();
  release();
  await opening;
  assert.equal(page.state.open, false);
  assert.deepEqual(page.state.people.rows, []);
});

test('every people read the pop-up makes is one query-api accepts', async () => {
  const { page, query } = editor();
  await page.open('idp');
  page.setDraft('query', 'Ada L');
  await page.search();
  await page.open('manual');
  for (const body of query.sent) assert.doesNotThrow(() => validate(body), JSON.stringify(body));
  assert.equal(query.sent.length, 3);
});

test('on Teams an admin gets "+ Add a Team", on a team "Edit team", and the pop-up opens outside the screen', async () => {
  const admin = fakeAdmin({ teams: [{ team_id: TEAM, name: 'Engineering', source: 'console', members: 1 }] });
  const api = createQueryApi({ transport: { send: (body) => (body.source === 'mart.v_person' ? fakeQuery().run(body) : fixtureTransport('realistic').send(body)) } });
  const doc = fakeDocument('#teams');
  const listeners = [];
  const add = doc.addEventListener;
  doc.addEventListener = (type, fn) => { listeners.push({ type, fn }); add(type, fn); };
  const app = await boot({ document: doc, api, admin, session: { actor: 'admin@lab.test', roles: ['admin'], pages: ['posture', 'tools', 'teams', 'person', 'settings', 'deployment', 'directory'] } });
  assert.match(doc.html('app'), /\+ Add a Team/);
  const option = { tagName: 'BUTTON', disabled: false, dataset: { team: 'open', kind: 'manual' } };
  await Promise.all(listeners.filter((l) => l.type === 'click').map((l) => l.fn({ target: { closest: (sel) => (sel === '[data-team]' ? option : null) }, preventDefault() {} })));
  for (let i = 0; i < 100 && app.editor().state.people.pending; i++) await new Promise((resolve) => setTimeout(resolve, 5));
  assert.match(doc.html('team-dialog'), /Name your Team/);
  assert.match(doc.html('team-dialog'), /Ada Lovelace/);
  await doc.navigate(`#teams?team=${TEAM}`);
  assert.equal(app.editor().state.open, false, 'moving to another screen closes the pop-up');
  assert.match(doc.html('app'), /data-team="edit"/);
  assert.ok(!/\+ Add a Team/.test(doc.html('app')));

  const viewer = fakeDocument('#teams');
  await boot({ document: viewer, api, admin, session: { actor: 'v@lab.test', roles: ['viewer'], pages: ['posture', 'tools', 'teams'] } });
  assert.ok(!/Add a Team/.test(viewer.html('app')), 'only an admin adds teams');
});
