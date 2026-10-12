// directory.test.mjs — Settings → Identity Providers: the controller over a fake admin API, the
// requests the admin API makes for the directory and its teams, and what the page says.

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
function fakeAdmin({ sync = null, fail = {} } = {}) {
  const calls = [];
  const state = { sync };
  return {
    calls,
    async directory() {
      calls.push(['directory']);
      if (fail.directory) return { state: 'refused', status: 403, error: { code: fail.directory, message: 'refused' } };
      return { state: 'available', data: { entra_connected: true, graph_available: true, people: 12, groups: 1, sync: state.sync } };
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
  };
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
  assert.match(html, /<h2>Identity Providers<\/h2>/);
  assert.ok(!/Create a team|Create team/.test(html), 'teams are added on the Teams page');
  assert.match(renderDirectory(page.state, { setup: { status: 'loading' } }), /Loading sign-in and provisioning/, 'sign-in and SCIM provisioning are set up on the same page');
  assert.match(renderDirectory({ status: 'loading' }, { setup: { status: 'loading' } }), /Loading sign-in and provisioning/, 'and are shown while the directory is still being read');
});

test('a role that is not an admin is told the page is an admin\'s', async () => {
  const page = createDirectory({ admin: fakeAdmin({ fail: { directory: 'forbidden' } }) });
  await page.load();
  assert.match(renderDirectory(page.state), /Only an admin can manage identity providers/);
});

test('a failed read names its cause and who can fix it', async () => {
  const page = createDirectory({ admin: fakeAdmin({ sync: { enabled_at: '2026-10-01T10:00:00Z', last_status: 'failed', last_error: 'graph_consent_missing', last_completed_at: '2026-10-01T11:00:00Z' } }) });
  await page.load();
  const html = renderDirectory(page.state);
  assert.match(html, /The last read failed/);
  assert.match(html, /GroupMember\.Read\.All/);
  assert.match(html, /graph_consent_missing/);
});

test('the admin API reads the directory and its teams, and names each team route', async () => {
  const seen = [];
  const transport = {
    async request(spec) {
      seen.push(`${spec.method} ${spec.path}${spec.body ? ` ${JSON.stringify(spec.body)}` : ''}`);
      if (spec.path === '/admin/v1/directory') return { status: 200, body: { entra_connected: true, graph_available: true, people: 3, groups: null, sync: null } };
      if (spec.path === '/admin/v1/teams' && spec.method === 'GET') return { status: 200, body: { teams: [{ team_id: TEAM, name: 'Pilot', includes: [{ kind: 'team', team_id: 'x', name: 'Ops' }, { kind: 'person' }], people: 2, members: 2 }, { name: 'no id' }] } };
      return { status: 204, body: null };
    },
  };
  const admin = createAdminApi({ transport });
  const read = await admin.directory();
  assert.equal(read.state, 'available');
  assert.equal(read.data.groups, null, 'a count not sent stays unknown');
  const teams = await admin.teams();
  assert.deepEqual(teams.teams.map((t) => t.name), ['Pilot'], 'a team with no id is dropped');
  assert.deepEqual(teams.teams[0].includes.map((i) => [i.kind, i.team_id, i.name]), [['team', 'x', 'Ops']], 'an include of an unknown kind is dropped');
  await admin.directoryGroups('sales east');
  await admin.renameTeam(TEAM, 'Pilot crew');
  await admin.setTeamIncludes(TEAM, [{ kind: 'department', value: 'Sales' }]);
  await admin.changeTeamMembers(TEAM, { add: [ADA] });
  await admin.deleteTeam(TEAM);
  assert.deepEqual(seen, [
    'GET /admin/v1/directory',
    'GET /admin/v1/teams',
    'GET /admin/v1/directory/groups?q=sales%20east',
    `PATCH /admin/v1/teams/${TEAM} {"name":"Pilot crew"}`,
    `PUT /admin/v1/teams/${TEAM}/includes {"includes":[{"kind":"department","value":"Sales"}]}`,
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

test('a person\'s Tools used counts each tool once across the window, not the sum of the days', () => {
  const rows = [
    { bucket: '2026-09-29T00:00:00Z', subject: ADA, submissions: 4, tools_used: 2, block_events: 0, bytes_total: 10 },
    { bucket: '2026-09-30T00:00:00Z', subject: ADA, submissions: 6, tools_used: 2, block_events: 0, bytes_total: 10 },
  ];
  const counted = readState(envelope('ok', {
    data: rows, freshness: FRESH, coverage: COMPLETE,
    meta: { source: 'mart.agg_user_period', extras: { subject_tools: [{ tools: 3 }] } },
  }));
  const tools = (view) => view.tiles.find((t) => t.label === 'Tools used').value;
  assert.deepEqual(tools(personView(counted, { subject: ADA })), { kind: 'number', text: '3' });
  const uncounted = readState(envelope('ok', { data: rows, freshness: FRESH, coverage: COMPLETE, meta: { source: 'mart.agg_user_period' } }));
  assert.deepEqual(tools(personView(uncounted, { subject: ADA })), { kind: 'absent', text: '—' }, 'four is not a count of tools');
});
