// directory.test.mjs — Settings → Directory & teams: the controller over a fake admin API, the
// requests the admin API makes, and what the page says.

import test from 'node:test';
import assert from 'node:assert/strict';
import { createDirectory } from '../src/directory.js';
import { renderDirectory } from '../src/directory-render.js';
import { createAdminApi } from '../src/transport.js';
import { personView } from '../src/views.js';
import { renderScreen } from '../src/render.js';
import { readState } from '../src/states.js';
import { envelope, FRESH, COMPLETE } from './helpers.mjs';

const ADA = `u_${'a'.repeat(32)}`;
const TEAM = '7d0c1a52-58f4-4a51-9b1e-1f6d2f0a0001';

/** An admin API that answers from memory and records what was asked of it. */
function fakeAdmin({ sync = null, teams = [], fail = {} } = {}) {
  const calls = [];
  const members = new Map();
  const state = { sync, teams: [...teams] };
  const refused = (code, message, status = 409) => ({ state: 'refused', status, error: { code, message } });
  return {
    calls,
    async directory() {
      calls.push(['directory']);
      if (fail.directory) return refused(fail.directory, 'refused', 403);
      return { state: 'available', data: { entra_connected: true, graph_available: true, people: 12, groups: 1, sync: state.sync, teams: state.teams.map((t) => ({ ...t, members: (members.get(t.team_id) ?? []).length })) } };
    },
    async setDirectorySync(enabled) {
      calls.push(['sync', enabled]);
      state.sync = enabled ? { enabled_by: 'admin@contoso.com', enabled_at: '2026-10-01T10:00:00Z', last_status: null } : null;
      return { state: 'done' };
    },
    async runDirectorySync() {
      calls.push(['run']);
      return { state: 'done', queued: true };
    },
    async directoryGroups(q) {
      calls.push(['groups', q]);
      return {
        state: 'available',
        provisioned: [{ id: '00000000-0000-4000-8000-000000000900', object_id: '11111111-2222-4333-8444-555555555555', name: 'Sales', members: 4, directory: 'provisioned' }],
        directory: q ? [
          { object_id: '11111111-2222-4333-8444-555555555555', name: 'Sales', directory: 'entra', imported: true },
          { object_id: '26118915-6090-4610-87a4-49d0767e9fde', name: 'Sales EMEA', directory: 'entra', imported: false },
        ] : [],
      };
    },
    async directoryValues(kind) {
      calls.push(['values', kind]);
      return { state: 'available', values: kind === 'org_unit' ? [{ value: 'OU=Research,DC=contoso,DC=com', people: 3 }] : [{ value: 'Engineering', people: 7 }] };
    },
    async createTeam(body) {
      calls.push(['create', body]);
      if (fail.create) return refused(fail.create, 'A team with that name exists.');
      const team = { team_id: TEAM, name: body.name, source: body.source, created_at: '2026-10-01T10:00:00Z' };
      state.teams.push(team);
      return { state: 'done', team };
    },
    async deleteTeam(id) {
      calls.push(['delete', id]);
      state.teams = state.teams.filter((t) => t.team_id !== id);
      return { state: 'done' };
    },
    async teamMembers(id) {
      calls.push(['members', id]);
      return { state: 'available', members: members.get(id) ?? [], limit: 500 };
    },
    async changeTeamMembers(id, change) {
      calls.push(['change', id, change]);
      const now = members.get(id) ?? [];
      members.set(id, [...now.filter((m) => !(change.remove ?? []).includes(m.user_ref)), ...(change.add ?? []).map((ref) => ({ user_ref: ref, name: 'Ada Lovelace' }))]);
      return { state: 'done' };
    },
  };
}

function fakeQuery(rows) {
  const sent = [];
  return { sent, async run(body) { sent.push(body); return { result_state: 'ok', data: rows }; } };
}

test('turning the directory read on and running it go to the admin API, and the page says what happened', async () => {
  const admin = fakeAdmin();
  const page = createDirectory({ admin });
  await page.load();
  assert.match(renderDirectory(page.state), /User\.Read\.All/, 'the permission an Entra administrator must grant is named before turning it on');
  await page.act({ dir: 'sync-on' });
  await page.act({ dir: 'sync-run' });
  assert.deepEqual(admin.calls.filter((c) => c[0] !== 'directory'), [['sync', true], ['run']]);
  const html = renderDirectory(page.state);
  assert.match(html, /A read has been started/);
  assert.match(html, /Stop reading/);
});

test('a failed read names its cause and who can fix it', async () => {
  const page = createDirectory({ admin: fakeAdmin({ sync: { enabled_at: '2026-10-01T10:00:00Z', last_status: 'failed', last_error: 'graph_consent_missing', last_completed_at: '2026-10-01T11:00:00Z' } }) });
  await page.load();
  const html = renderDirectory(page.state);
  assert.match(html, /The last read failed/);
  assert.match(html, /GroupMember\.Read\.All/);
  assert.match(html, /graph_consent_missing/);
});

test('a console team is created, opened, and filled from the people list', async () => {
  const admin = fakeAdmin();
  const query = fakeQuery([{ subject: ADA, name: 'Ada Lovelace', department: 'Engineering' }]);
  const page = createDirectory({ admin, query });
  await page.load();
  await page.act({ dir: 'create' });
  assert.equal(page.state.create.problem.code, 'incomplete', 'a team needs a name');
  page.setDraft('name', ' Pilot ');
  await page.act({ dir: 'create' });
  assert.deepEqual(admin.calls.find((c) => c[0] === 'create')[1], { name: 'Pilot', source: 'console' });
  assert.equal(page.state.team.open, TEAM, 'a new console team opens so its members are chosen next');

  page.setDraft('peopleQuery', 'Ada L');
  await page.act({ dir: 'search-people' });
  assert.deepEqual(query.sent[0].filters, [{ field: 'name_key', op: 'starts_with', value: 'ada l' }]);
  assert.equal(query.sent[0].source, 'mart.v_person');
  await page.act({ dir: 'add-member', ref: ADA });
  assert.deepEqual(admin.calls.find((c) => c[0] === 'change'), ['change', TEAM, { add: [ADA] }]);
  const html = renderDirectory(page.state);
  assert.ok(html.includes(`href="#person?subject=${ADA}"`), 'a member opens their page');
  assert.match(html, /in the team/);

  await page.act({ dir: 'delete-team' });
  assert.match(renderDirectory(page.state), /Delete the team “Pilot”\?/);
  await page.act({ dir: 'confirm-delete' });
  assert.deepEqual(admin.calls.at(-2), ['delete', TEAM]);
  assert.equal(page.state.data.teams.length, 0);
});

test('a group team can follow a directory group not yet imported, and a unit team its distinguished name', async () => {
  const admin = fakeAdmin();
  const page = createDirectory({ admin });
  await page.load();
  await page.act({ dir: 'source', value: 'group' });
  page.setDraft('groupQuery', 'sales');
  await page.act({ dir: 'search-groups' });
  assert.deepEqual(page.state.choices.groups.map((g) => g.name), ['Sales', 'Sales EMEA'], 'a provisioned group is listed once');
  await page.act({ dir: 'pick-group', object: '26118915-6090-4610-87a4-49d0767e9fde', name: 'Sales EMEA' });
  await page.act({ dir: 'create' });
  assert.deepEqual(admin.calls.find((c) => c[0] === 'create')[1], { name: 'Sales EMEA', source: 'group', group_object_id: '26118915-6090-4610-87a4-49d0767e9fde' });

  await page.act({ dir: 'source', value: 'org_unit' });
  await page.act({ dir: 'pick-value', value: 'OU=Research,DC=contoso,DC=com' });
  assert.equal(page.state.drafts.name, 'Research', 'a unit suggests its own name');
  admin.calls.length = 0;
  await page.act({ dir: 'create' });
  assert.deepEqual(admin.calls.find((c) => c[0] === 'create')[1], { name: 'Research', source: 'org_unit', match_value: 'OU=Research,DC=contoso,DC=com' });
});

test('a refused create keeps the form and says why', async () => {
  const page = createDirectory({ admin: fakeAdmin({ fail: { create: 'team_name_taken' } }) });
  await page.load();
  page.setDraft('name', 'Pilot');
  await page.act({ dir: 'create' });
  assert.equal(page.state.drafts.name, 'Pilot');
  assert.match(renderDirectory(page.state), /No team was created\.<\/strong> A team with that name exists\. <code>team_name_taken<\/code>/);
});

test('the admin API reads the directory and its teams, and names each team route', async () => {
  const seen = [];
  const transport = {
    async request(spec) {
      seen.push(`${spec.method} ${spec.path}${spec.body ? ` ${JSON.stringify(spec.body)}` : ''}`);
      if (spec.path === '/admin/v1/directory') return { status: 200, body: { entra_connected: true, graph_available: true, people: 3, groups: null, sync: null } };
      if (spec.path === '/admin/v1/teams' && spec.method === 'GET') return { status: 200, body: { teams: [{ team_id: TEAM, name: 'Pilot', source: 'console', members: 2 }, { name: 'no id' }] } };
      return { status: 204, body: null };
    },
  };
  const admin = createAdminApi({ transport });
  const read = await admin.directory();
  assert.equal(read.state, 'available');
  assert.equal(read.data.groups, null, 'a count not sent stays unknown');
  assert.deepEqual(read.data.teams.map((t) => t.name), ['Pilot']);
  await admin.directoryGroups('sales east');
  await admin.changeTeamMembers(TEAM, { add: [ADA] });
  await admin.deleteTeam(TEAM);
  assert.deepEqual(seen.slice(2), [
    'GET /admin/v1/directory/groups?q=sales%20east',
    `POST /admin/v1/teams/${TEAM}/members {"add":["${ADA}"],"remove":[]}`,
    `DELETE /admin/v1/teams/${TEAM}`,
  ]);
});

test('a person\'s page is titled with their name and links to their prompts', () => {
  const state = readState(envelope('ok', { data: [], freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.agg_user_period' } }));
  const view = personView(state, { subject: ADA, person: { subject: ADA, name: 'Ada Lovelace', department: 'Engineering' } });
  const html = renderScreen(view, {});
  assert.match(html, /<h2>Ada Lovelace<\/h2>/);
  assert.match(html, /Engineering ·/);
  assert.ok(html.includes(`href="explore.html#events?subject=${ADA}"`));
});
