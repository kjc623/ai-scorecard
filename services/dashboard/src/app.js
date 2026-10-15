// app.js — the dashboard shell: hash routing, one data layer, and the screen table.
//
// Everything here is wiring. The decisions live in states.js (what an envelope means) and views.js
// (what a screen shows); app.js only decides which one runs and puts the result on the page.
//
// `createDashboard` is DOM-free: it takes an api and returns `load(screenId, params)`, so a test
// can drive every screen against a fake transport without a browser. `boot()` is the only function
// that touches a document.

import { createQueryApi, httpTransport, loadSession, createAdminApi, httpAdminTransport } from './transport.js';
import { readState } from './states.js';
import { QUESTIONS, context } from './questions.js';
import {
  postureView, activityView, adoptionView, discoveredView, demandView, agentsView, toolsView, toolView, teamsView, teamView,
  personView, peopleView, devicesView, deviceView, eventView, auditView, rulesView, sanctionView, refusalView,
  needsInputView,
} from './views.js';
import {
  buildAgentDocument, buildDemandDocument, buildDiscoveryDocument, buildFirstSeenDocument, buildListDocument, buildPeopleDocument, buildRuleMatchesDocument,
  buildSanctionShiftDocument, buildTeamMembersDocument, buildToolPromptsDocument,
} from './dsl.js';
import { renderScreen, renderNav } from './render.js';
import { SECTIONS, sectionOf, shellNavItems, tabsFor, wireShell } from './shell.js';
import { allowedPageIds, mayOpen } from './session.js';
import { createDeployment } from './deployment.js';
import { DEPLOYMENT_NOTES, renderAgentSetup } from './deployment-render.js';
import { createDiagnostics } from './diagnostics.js';
import { renderDeviceDiagnostics } from './diagnostics-render.js';
import { createSettings } from './settings.js';
import { createDirectory } from './directory.js';
import { renderDirectory } from './directory-render.js';
import { createIntegrations } from './integrations.js';
import { renderIntegrations } from './integrations-render.js';
import { renderSanctionDecisions, renderSettings } from './settings-render.js';
import { createRules } from './rules.js';
import { renderRuleDialog, renderRules } from './rules-render.js';
import { createTeamEditor } from './team-editor.js';
import { renderTeamAdd, renderTeamDialog, renderTeamEdit } from './team-editor-render.js';

/**
 * The screens, each a tab of a destination in shell.js. One tool, team, person, device or event is
 * the same screen opened on a row of its list.
 *
 * An `admin` screen reads and writes control-api's admin API, not the query API: it has no
 * question and no envelope, and boot() hands it to its own controller. Rules is one for an admin,
 * who edits the rules (rules.js), and a query screen for everyone else, who reads them.
 */
export const SCREENS = Object.freeze([
  Object.freeze({ id: 'posture', label: 'Overview', question: null, kind: 'posture' }),
  Object.freeze({ id: 'tools', label: 'Tools', question: 1, kind: 'answer', questionId: 'q1_tools_ranked' }),
  Object.freeze({ id: 'teams', label: 'Teams', question: 3, kind: 'answer', questionId: 'q3_team_growth' }),
  Object.freeze({ id: 'person', label: 'Users', question: 6, kind: 'input', questionId: 'q6_subject_series' }),
  Object.freeze({ id: 'rules', label: 'Rules', question: null, kind: 'answer', editor: true }),
  Object.freeze({ id: 'labels', label: 'Labels', question: null, kind: 'admin' }),
  Object.freeze({ id: 'sanction', label: 'Tools', question: null, kind: 'answer' }),
  Object.freeze({ id: 'activity', label: 'Activity', question: null, kind: 'answer' }),
  Object.freeze({ id: 'insights', label: 'Insights', question: null, kind: 'answer' }),
  Object.freeze({ id: 'settings', label: 'Collection', question: null, kind: 'admin' }),
  Object.freeze({ id: 'devices', label: 'Devices', question: 7, kind: 'answer', questionId: 'q7_devices' }),
  Object.freeze({ id: 'directory', label: 'Identity Providers', question: null, kind: 'admin' }),
  Object.freeze({ id: 'integrations', label: 'Integrations', question: null, kind: 'admin' }),
  Object.freeze({ id: 'audit', label: 'Audit trail', question: 10, kind: 'answer', questionId: 'q10_audit_trail' }),
  Object.freeze({ id: 'event', label: 'Event detail', question: 9, kind: 'input', questionId: 'q9_event_detail' }),
]);

/** The window Policy reads its figures over: the rules' matches and the tools' usage. */
const POLICY_PRESET = 'd30';

/** The fields the audit trail filters by, as q10 names them. */
const AUDIT_FILTERS = Object.freeze(['actor', 'action', 'object_type', 'case']);

/** A tool's sanction, as Usage → Tools filters by it. */
const SANCTION_FILTER = Object.freeze(['sanctioned', 'unsanctioned', 'unknown']);

export const SCREEN_IDS = Object.freeze(SCREENS.map((s) => s.id));

/** Turn any thrown error into a refusal view state, so no screen can crash into a blank page. */
export function refusalFrom(error, { title = 'Read refused' } = {}) {
  return refusalView({
    resultState: error?.resultState ?? 'unsupported_query_shape',
    error: error?.envelope?.error ?? { code: error?.reason ?? 'client_error', message: String(error?.message ?? error) },
    banners: [],
    data: [],
  }, { title });
}

/**
 * Build the dashboard's behaviour over one api.
 *
 * @param {object} input
 * @param {ReturnType<typeof createQueryApi>} input.api
 * @param {ReturnType<typeof createAdminApi>|null} [input.admin] the admin API, for the team list an admin sees on Teams
 * @param {Date} [input.now]
 */
export function createDashboard({ api, admin = null, now = () => new Date() }) {
  /** The last coverage and freshness blocks seen are kept, for a screen that reads nothing itself. */
  const observed = { coverage: null, freshness: null, watermarks: new Map() };

  function remember(state) {
    if (state.coverage) observed.coverage = state.coverage;
    if (state.freshness) {
      observed.freshness = state.freshness;
      const key = `${state.freshness.aggregate ?? 'unknown'}`;
      observed.watermarks.set(key, state.freshness);
    }
  }

  function shell() {
    return { coverage: observed.coverage, freshness: observed.freshness };
  }

  async function ask(questionId, ctx) {
    const question = QUESTIONS[questionId];
    const body = question.request(ctx);
    const envelope = await api.run(body);
    const state = readState(envelope);
    remember(state);
    return state;
  }

  /** A read beside the main one. One that fails leaves its panel empty rather than taking the page down. */
  async function part(run) {
    try {
      return await run();
    } catch {
      return null;
    }
  }

  /** A read whose refusal is shown in place: the error becomes a refusal state, with its reason. */
  async function refusable(run) {
    try {
      return await run();
    } catch (error) {
      return readState(error?.envelope ?? {
        result_state: error?.resultState ?? 'unsupported_query_shape',
        error: { code: error?.reason ?? 'client_error', message: String(error?.message ?? error) },
      });
    }
  }

  /**
   * Load one screen. Returns `{view, shell}`; never throws for an API-level problem, because a
   * refusal is a state to render rather than a crash.
   */
  async function load(screenId, given = {}) {
    const params = { ...given, filters: given.filters ?? {} };
    const screen = SCREENS.find((s) => s.id === screenId) ?? SCREENS[0];
    const ctx = context({ preset: params.preset ?? 'd7', filters: params.filters ?? {}, now: now() });
    try {
      switch (screen.kind) {
        case 'posture': {
          const devices = await ask('q7_devices', context({ preset: ctx.preset, limit: 500, filters: params.filters ?? {}, now: now() }));
          // The summary reads the window's usage, and the rules with what they matched in it.
          const tools = await part(() => ask('q1_tools_ranked', ctx));
          const rules = await part(async () => readState(await api.run(buildListDocument('ops.enforcement_rule', {}, 100))));
          const ruleMatches = await part(async () => readState(await api.run(buildRuleMatchesDocument({ window: ctx.window }))));
          return { view: postureView({ devices, tools, rules, ruleMatches }, { preset: ctx.preset, activity: params.activity !== false }), shell: shell() };
        }
        case 'answer': {
          if (screen.id === 'tools') {
            if (params.filters.tool) return { view: await toolScreen(String(params.filters.tool), ctx), shell: shell() };
            const sanction = SANCTION_FILTER.includes(params.filters.sanction) ? params.filters.sanction : null;
            return { view: toolsView(await ask('q1_tools_ranked', ctx), { preset: ctx.preset, sanction }), shell: shell() };
          }
          if (screen.id === 'teams') return { view: await teamsScreen(params, ctx), shell: shell() };
          if (screen.id === 'rules') return { view: await rulesScreen(), shell: shell() };
          if (screen.id === 'sanction') {
            const usage = await ask('q1_tools_ranked', context({ preset: POLICY_PRESET, now: now() }));
            return { view: sanctionView(usage), shell: shell() };
          }
          if (screen.id === 'activity') return { view: await activityScreen(ctx), shell: shell() };
          if (screen.id === 'insights') {
            const tab = INSIGHTS_SCREENS[params.filters.view] ?? adoptionScreen;
            return { view: await tab(ctx), shell: shell() };
          }
          if (screen.id === 'devices' && params.filters.device) return { view: await deviceScreen(String(params.filters.device)), shell: shell() };
          if (screen.id === 'audit') {
            const state = await ask('q10_audit_trail', context({ preset: ctx.preset, cursor: params.filters.cursor ?? null, filters: params.filters, now: now() }));
            return { view: state.resultState === 'audit_chain_broken' ? refusalView(state, { title: 'Audit trail' }) : auditView(state, { filters: params.filters, preset: params.preset }), shell: shell() };
          }
          return { view: devicesView(await ask('q7_devices', ctx), { filters: params.filters ?? {}, now: now() }), shell: shell() };
        }
        case 'input': {
          if (screen.id === 'person') {
            const subject = params.filters?.subject;
            if (!subject) {
              const search = String(params.filters?.q ?? '').trim();
              const cursor = params.filters?.cursor ?? null;
              const people = readState(await api.run(buildPeopleDocument({ search, cursor })));
              // Each person's teams and prompts in the window come from the teams' rosters.
              const members = await part(async () => teamMembers(await ask('q3_team_growth', ctx), ctx));
              return { view: peopleView(people, { search, members }), shell: shell() };
            }
            const state = await ask('q6_subject_series', context({ preset: ctx.preset, filters: { subject }, now: now() }));
            // The person's name and place come from the people list; without them the page still
            // shows the series under the reference.
            let person = null;
            try {
              person = readState(await api.run(buildPeopleDocument({ subject, limit: 1 }))).data[0] ?? null;
            } catch {
              person = null;
            }
            return { view: personView(state, { subject, person, preset: ctx.preset }), shell: shell() };
          }
          const submissionId = params.filters?.submission_id;
          if (!submissionId) return { view: needsInputView({ id: 'event', title: 'Event detail', question: null, hint: 'Reached from a row that names an event.' }), shell: shell() };
          const state = await ask('q9_event_detail', context({ filters: { submission_id: submissionId, received_at_hint: params.filters?.received_at_hint }, now: now() }));
          return { view: eventView(state), shell: shell() };
        }
        case 'admin':
        default:
          return { view: null, shell: shell(), admin: true };
      }
    } catch (error) {
      return { view: refusalFrom(error, { title: screen.label }), shell: shell() };
    }
  }

  /**
   * One tool's screen: its usage, who is using it (a subject-bearing read, refused for a role
   * that may not see people, and shown as such), and the findings it produced.
   */
  async function toolScreen(tool, ctx) {
    const usage = await ask('q1_tools_ranked', context({ preset: ctx.preset, filters: { tool }, now: now() }));
    // The people read names the tool, so it lists its people whatever its sanction; a tool with
    // no cell in the window has nobody to list. Both reads match every fingerprint of the tool.
    const people = usage.data.length > 0
      ? await refusable(() => ask('q2_unsanctioned_users', context({ preset: ctx.preset, filters: { tool }, now: now() })))
      : null;
    const findings = await refusable(() => ask('q5_findings', context({ preset: ctx.preset, filters: { tool }, now: now() })));
    return toolView({ usage, people, findings, names: await namesFor(people) }, { tool, preset: ctx.preset });
  }

  /**
   * The names behind the references a roster carries, from the people list: the directory's name
   * when a sync supplied one, else the one the device reported. A reference with no name is
   * absent from the map and is listed by reference; a lookup that fails leaves the roster as it is.
   */
  async function namesFor(roster) {
    const subjects = [...new Set((roster && !roster.isRefusal ? roster.data : []).map((row) => row.subject).filter((s) => typeof s === 'string' && s !== ''))];
    if (subjects.length === 0) return {};
    const people = await part(async () => readState(await api.run(buildPeopleDocument({ subjects, limit: subjects.length }))));
    const names = {};
    for (const row of people?.data ?? []) {
      const name = row.directory_name ?? row.name ?? row.subject_name ?? null;
      if (name && name !== row.subject) names[row.subject] = String(name);
    }
    return names;
  }

  /**
   * Policy → Rules as everyone but an admin reads it: the rules as they stand, each with the
   * prompts it decided over the last 30 days. Without the rules the page says why; without the
   * matches the rules are listed with their counts absent.
   */
  async function rulesScreen() {
    const ctx = context({ preset: POLICY_PRESET, now: now() });
    const ruleMatches = await part(async () => readState(await api.run(buildRuleMatchesDocument({ window: ctx.window }))));
    if (ruleMatches) remember(ruleMatches);
    const rules = await refusable(async () => readState(await api.run(buildListDocument('ops.enforcement_rule', {}, 100))));
    return rulesView({ rules, ruleMatches });
  }

  /**
   * Policy → Activity: the window's tool usage and class mix, the enforcement rules and the prompts
   * each matched, and the tool usage of the period of the same length just before it, which the
   * changes are measured against. Only the window's usage is needed; without the others their
   * panels say what is missing. The previous period is read first, so the coverage the page reports
   * is the window's own.
   */
  async function activityScreen(ctx) {
    const previous = await part(() => ask('q1_tools_ranked', context({ preset: ctx.preset, now: new Date(Date.parse(ctx.window.from)) })));
    const usage = await ask('q1_tools_ranked', ctx);
    const classes = await part(() => ask('q4_class_mix', ctx));
    const rules = await part(async () => readState(await api.run(buildListDocument('ops.enforcement_rule', {}, 100))));
    const ruleMatches = await part(async () => readState(await api.run(buildRuleMatchesDocument({ window: ctx.window }))));
    return activityView({ usage, previous, classes, rules, ruleMatches }, { preset: ctx.preset });
  }

  /**
   * One device: its row in the fleet list and every collector it reported. A device the list does
   * not hold is said to be unknown; collectors that could not be read say why in place.
   */
  async function deviceScreen(device) {
    const collectors = await refusable(async () => readState(await api.run(buildListDocument('ops.collector_state', { device }, 100))));
    const row = readState(await api.run(buildListDocument('mart.v_device_liveness', { device }, 1)));
    remember(row);
    return deviceView({ device, row, collectors }, { now: now() });
  }

  /**
   * Insights, Adoption: the window's tool usage, which carries the people who used AI; the same
   * read of the period before it, for the change; the applications first seen in the window; and
   * the prompts per bucket by sanction. Only the window's usage is needed; without the others their
   * panels say what is missing. The previous period is read first, as on Activity.
   */
  async function adoptionScreen(ctx) {
    const previous = await part(() => ask('q1_tools_ranked', context({ preset: ctx.preset, now: new Date(Date.parse(ctx.window.from)) })));
    const usage = await ask('q1_tools_ranked', ctx);
    const firstSeen = await part(async () => readState(await api.run(buildFirstSeenDocument({ window: ctx.window }))));
    const shift = await part(async () => readState(await api.run(buildSanctionShiftDocument({ window: ctx.window }))));
    return adoptionView({ usage, previous, firstSeen, shift }, { preset: ctx.preset });
  }

  /**
   * Insights, Discovered: what device discovery found in the window, with where the inventory scan
   * stands beside it, and the prompts each tool sent in the window, which tell the tools in use from
   * the ones only present. Only what was found is needed; without the prompts their column says so.
   * The prompts are read first, so the freshness the page reports is discovery's own.
   */
  async function discoveredScreen(ctx) {
    const prompts = await part(async () => readState(await api.run(buildToolPromptsDocument({ window: ctx.window }))));
    const found = readState(await api.run(buildDiscoveryDocument({ window: ctx.window })));
    remember(found);
    return discoveredView({ found, prompts }, { preset: ctx.preset });
  }

  /** Insights, Demand: teams crossed with tools, with the demand for each tool not sanctioned beside them. */
  async function demandScreen(ctx) {
    const state = readState(await api.run(buildDemandDocument({ window: ctx.window })));
    remember(state);
    return demandView(state, { preset: ctx.preset });
  }

  /**
   * Insights, Coding agents: what coding agents reported in the window, and the same read of the
   * period before it, for the change. Only the window's read is needed; the previous period is read
   * first, so the freshness the page reports is the window's own.
   */
  async function agentsScreen(ctx) {
    const earlier = context({ preset: ctx.preset, now: new Date(Date.parse(ctx.window.from)) }).window;
    const previous = await part(async () => readState(await api.run(buildAgentDocument({ window: earlier }))));
    const agents = readState(await api.run(buildAgentDocument({ window: ctx.window })));
    remember(agents);
    return agentsView({ agents, previous }, { preset: ctx.preset });
  }

  /** The tabs of Insights, by the `view` that names them. */
  const INSIGHTS_SCREENS = Object.freeze({ adoption: adoptionScreen, discovered: discoveredScreen, demand: demandScreen, agents: agentsScreen });

  /** The Teams screen: every team, or one team when the address names it. */
  async function teamsScreen(params, ctx) {
    const team = params.filters.team ? String(params.filters.team) : null;
    // An admin's team list names the teams with no usage, so a quiet team is listed rather than
    // absent, and one team's screen is named even when it has no cell in the window.
    const teams = params.mayManage && admin
      ? await part(async () => {
        const answer = await admin.teams();
        return answer.state === 'available' ? answer.teams : null;
      })
      : null;
    if (team) {
      const state = await ask('q3_team_growth', context({ preset: ctx.preset, filters: { team }, now: now() }));
      return teamView(state, { team, members: await teamMembers(state, ctx, [team]), teams, preset: ctx.preset });
    }
    const state = await ask('q3_team_growth', ctx);
    return teamsView(state, { mayManage: Boolean(params.mayManage), teams, preset: ctx.preset });
  }

  /**
   * The people behind the teams with usage. A role that may not see people, or a read that fails,
   * leaves the team totals standing and says why the people are missing.
   */
  async function teamMembers(teams, ctx, ids = teams.data.map((row) => row.team).filter(Boolean)) {
    if (teams.isRefusal || ids.length === 0) return null;
    return refusable(async () => readState(await api.run(buildTeamMembersDocument({ window: ctx.window, teams: ids }))));
  }

  return Object.freeze({ load, observed, screenIds: SCREEN_IDS });
}

/** Parse `#screen?preset=d30&subject=u_1` into a screen id and params. */
export function parseHash(hash) {
  const raw = String(hash ?? '').replace(/^#/, '');
  const [path, queryString] = raw.split('?');
  const parts = path.split('/').filter(Boolean);
  const id = parts[0] || 'posture';
  const filters = {};
  const params = new URLSearchParams(queryString ?? '');
  for (const [key, value] of params.entries()) filters[key] = value;
  return { id, preset: filters.preset, filters };
}

/**
 * An address from an earlier layout, as the address that now opens the same thing; null for an
 * address that is current. Links and bookmarks made before keep working.
 */
export function redirectFor({ id, filters }, allowed = null) {
  const { view, ...rest } = filters;
  const at = (screenId, params) => {
    const query = new URLSearchParams(params).toString();
    return `#${screenId}${query ? `?${query}` : ''}`;
  };
  const window = filters.preset ? { preset: filters.preset } : {};
  if (id === 'deployment') return '#devices';
  if (id === 'rules' && view === 'labels') return '#labels';
  if (id === 'tools' && view === 'unsanctioned') return at('tools', { ...rest, sanction: 'unsanctioned' });
  if (id === 'tools' && view === 'classes') return mayOpen(allowed, 'activity') ? at('activity', window) : at('tools', window);
  if (id === 'insights' && view === 'exposure') return at('activity', rest);
  return null;
}

/** Screens whose question takes a time window, and the windows offered. */
const WINDOWED = Object.freeze(['posture', 'tools', 'teams', 'person', 'activity', 'insights', 'audit']);
const PRESET_LABELS = Object.freeze({ h24: '24h', d7: '7d', d30: '30d', d90: '90d' });

/** The window switcher for a screen: each preset is a link that keeps the other parameters. */
function presetsFor(screen, preset, filters) {
  if (!WINDOWED.includes(screen.id)) return null;
  const current = PRESET_LABELS[preset] ? preset : 'd7';
  const items = Object.entries(PRESET_LABELS).map(([id, label]) => {
    // A later page of a list belongs to the window it was read in.
    const { cursor: _cursor, ...kept } = filters;
    const params = new URLSearchParams({ ...kept, preset: id });
    return { id, label, href: `#${screen.id}?${params.toString()}` };
  });
  return { current, items };
}

/** The tabs of Insights, each a question a customer asks; Adoption is the default. */
const INSIGHTS_TABS = Object.freeze({ adoption: 'Adoption', discovered: 'Discovered', demand: 'Demand', agents: 'Coding agents' });

/** The parameters that open one record of a list rather than the list. */
const RECORD_PARAMS = Object.freeze({ tools: 'tool', teams: 'team', person: 'subject', devices: 'device' });

/**
 * What a screen's head shows: on a list, its destination's name with the destination's pages as
 * tabs; on one record, the record's own title under its destination's name. Insights' tabs are its
 * own questions. A tab that takes a window keeps the one chosen.
 */
function headFor(screen, preset, filters, allowed) {
  const section = sectionOf(screen.id);
  const keep = (id) => (PRESET_LABELS[preset] && WINDOWED.includes(id) ? `?preset=${preset}` : '');
  if (screen.id === 'event' || filters[RECORD_PARAMS[screen.id]]) return { eyebrow: section?.label ?? null };
  if (screen.id === 'insights') {
    const link = (extra) => {
      const query = new URLSearchParams({ ...(PRESET_LABELS[preset] ? { preset } : {}), ...extra }).toString();
      return `#insights${query ? `?${query}` : ''}`;
    };
    return {
      eyebrow: section?.label ?? null,
      title: screen.label,
      tabs: {
        label: screen.label,
        current: INSIGHTS_TABS[filters.view] ? filters.view : 'adoption',
        items: Object.entries(INSIGHTS_TABS).map(([id, label]) => ({ id, label, href: link(id === 'adoption' ? {} : { view: id }) })),
      },
    };
  }
  const tabs = tabsFor(screen.id, { allowed, hrefOf: (id) => `#${id}${keep(id)}` });
  if (tabs) return { title: section.label, tabs };
  // A destination the session opens one page of names that page, under the destination.
  const page = section?.pages.find((p) => p.id === screen.id);
  return section && page && section.pages.length > 1 ? { eyebrow: section.label, title: page.label } : {};
}

/** Every page the navigation can offer; the server says which of them a session may open. */
const PAGE_IDS = new Set(SECTIONS.flatMap((s) => s.pages.map((p) => p.id)));

/** Put an address in place of the current one without a navigation, so Back skips the old one. */
function replaceHash(document, hash) {
  const history = document.defaultView?.history;
  if (typeof history?.replaceState === 'function') history.replaceState(null, '', hash);
  else document.location.hash = hash;
}

/**
 * A screen the server does not offer this session, said rather than attempted: one the signed-in
 * roles cannot use, which the server refuses anyway, or one still being built outside pre-production.
 */
function notPermittedView(screen) {
  return needsInputView({ id: screen.id, title: screen.label, question: null, hint: 'This page is not offered to you here. The navigation shows the pages you can use.' });
}

/**
 * Hand a downloaded package to the browser as a file. The data is a Blob from the admin API; an
 * object URL and a click on a download link is how a page saves one without a navigation.
 */
function saveDownload(document, data, filename) {
  const view = document.defaultView;
  if (!view?.URL?.createObjectURL || typeof document.createElement !== 'function') throw new Error('this browser cannot save a file from the page');
  const blob = data instanceof view.Blob ? data : new view.Blob([data]);
  const href = view.URL.createObjectURL(blob);
  const link = document.createElement('a');
  link.href = href;
  link.download = filename;
  link.hidden = true;
  document.body.appendChild(link);
  link.click();
  link.remove();
  view.setTimeout(() => view.URL.revokeObjectURL(href), 60_000);
}

/** What a control acts on, so a repaint can find the same control again. */
function controlKey(el) {
  const d = el.dataset ?? {};
  return [d.team ?? d.rl ?? d.int, d.value, d.field, d.index, d.label, d.provider].join('\u0000');
}

/**
 * Repaint a container without losing the reader's place: the list and the panel keep their scroll,
 * and the control that had the focus has it again (a checkbox by what it ticks, a field by its id, a
 * button by what it acts on), or the panel takes it, so the focus never leaves an open dialog.
 */
function repaint(document, container, html) {
  const active = document.activeElement;
  const inside = Boolean(active) && typeof container.contains === 'function' && container.contains(active);
  const was = inside ? { pick: active.dataset?.teamPick ?? active.dataset?.rlPick, id: active.id, key: controlKey(active), caret: active.selectionStart } : null;
  const scrolled = ['.pick-list', '.dialog-body'].map((selector) => [selector, container.querySelector?.(selector)?.scrollTop ?? 0]);
  container.innerHTML = html;
  for (const [selector, top] of scrolled) {
    const el = container.querySelector?.(selector);
    if (el) el.scrollTop = top;
  }
  if (!was) return;
  const all = [...(container.querySelectorAll?.('input, button, select, textarea') ?? [])];
  const target = was.pick !== undefined ? all.find((el) => (el.dataset.teamPick ?? el.dataset.rlPick) === was.pick)
    : was.id ? all.find((el) => el.id === was.id)
      : all.find((el) => controlKey(el) === was.key);
  if (target && !target.disabled) {
    target.focus({ preventScroll: true });
    if (typeof was.caret === 'number' && typeof target.setSelectionRange === 'function') target.setSelectionRange(was.caret, was.caret);
  } else {
    container.querySelector?.('.dialog-panel')?.focus({ preventScroll: true });
  }
}

function copyToClipboard(document, text) {
  const clipboard = document.defaultView?.navigator?.clipboard;
  if (!clipboard?.writeText) return Promise.reject(new Error('no clipboard'));
  return clipboard.writeText(text);
}

/**
 * Boot the dashboard into a document. The only function in this package that touches the DOM.
 *
 * Every read goes to the page's own origin: /v1/* for the query API and /admin/v1/* for
 * the admin pages, which the dashboard server forwards with the session's product token.
 *
 * @param {object} input
 * @param {Document} input.document
 * @param {object} [input.api]      a query api (createQueryApi); the page origin's otherwise
 * @param {object} [input.admin]    an admin api (createAdminApi); the page origin's otherwise
 * @param {object|null} [input.session] who is signed in, as GET /session answers; read from the server otherwise
 */
export async function boot({ document, api, admin, session: givenSession } = {}) {
  const root = document.getElementById('app');
  const nav = document.getElementById('nav');
  // One dashboard for the whole session, so the last coverage read carries across navigation.
  const active = api ?? createQueryApi({ transport: httpTransport() });
  const adminApi = admin ?? createAdminApi({ transport: httpAdminTransport() });
  const dashboard = createDashboard({ api: active, admin: adminApi });
  // A signed-in role may see fewer pages than the shell names; the server says which, and
  // query-api and control-api refuse what lies behind the rest.
  const session = givenSession !== undefined ? givenSession : await loadSession();
  const allowed = allowedPageIds(session);
  const navItems = shellNavItems({ allowed });
  // The tab is titled by the product name the page itself shows, so the two cannot disagree.
  const brand = document.querySelector?.('.brand h1')?.textContent?.trim() || 'Sundial';

  let paintedNav = null;
  let paintedScreen = null;
  // The query screen on the page, kept so the admin section it carries can be repainted alone.
  let painted = null;
  let deployment = null;
  let diagnostics = null;
  let settings = null;
  let directory = null;
  let integrations = null;
  let rules = null;
  let editor = null;
  // An admin edits Policy and Settings and adds and edits teams; the server refuses the admin API
  // to anyone else.
  const mayAdmin = mayOpen(allowed, 'settings');
  // The add and edit pop-up lives outside the screen, so a screen repaint never closes it.
  const dialog = document.getElementById('team-dialog');
  // So does the rule panel.
  const ruleDialog = document.getElementById('rule-dialog');

  /** The head of a screen at the current address. */
  function headNow(screenId) {
    const { preset, filters } = parseHash(document.location.hash);
    return headFor(SCREENS.find((s) => s.id === screenId), preset, filters, allowed);
  }

  /**
   * The admin section a query screen carries for an admin: installing the agent below the device
   * list, and the sanction decisions above the tools Policy lists.
   */
  function adminSection(screenId, filters) {
    if (!mayAdmin) return {};
    if (screenId === 'devices' && !filters.device) return { after: renderAgentSetup(deploymentController().state) };
    if (screenId === 'devices' && filters.device) return { after: renderDeviceDiagnostics(diagnosticsController().state) };
    if (screenId === 'sanction') return { before: renderSanctionDecisions(settingsController().state) };
    return {};
  }

  function paintQuery() {
    if (!painted || painted.screen !== paintedScreen) return;
    repaint(document, root, renderScreen(painted.view, { ...painted.shell, ...adminSection(painted.screen, painted.filters) }));
  }

  function paintDeployment() {
    if (paintedScreen === 'devices') paintQuery();
    if (paintedScreen === 'directory') paintDirectory();
  }

  function deploymentController() {
    deployment ??= createDeployment({
      admin: adminApi,
      onChange: paintDeployment,
      save: (data, filename) => saveDownload(document, data, filename),
      copy: (text) => copyToClipboard(document, text),
    });
    return deployment;
  }

  function paintSettings(state) {
    if (paintedScreen === 'settings') repaint(document, root, renderSettings(state, { head: headNow('settings') }));
    if (paintedScreen === 'sanction') paintQuery();
  }

  function diagnosticsController() {
    diagnostics ??= createDiagnostics({
      admin: adminApi,
      onChange: () => { if (paintedScreen === 'devices') paintQuery(); },
      save: (data, filename) => saveDownload(document, data, filename),
    });
    return diagnostics;
  }

  function settingsController() {
    settings ??= createSettings({ admin: adminApi, onChange: paintSettings });
    return settings;
  }

  /** Identity Providers: the directory, with sign-in and SCIM from the deployment settings. */
  function paintDirectory() {
    if (paintedScreen === 'directory' && directory) {
      repaint(document, root, renderDirectory(directory.state, { head: headNow('directory'), setup: deployment?.state ?? null }));
    }
  }

  function directoryController() {
    directory ??= createDirectory({ admin: adminApi, onChange: paintDirectory });
    return directory;
  }

  function paintIntegrations(state) {
    if (paintedScreen === 'integrations') repaint(document, root, renderIntegrations(state, { head: headNow('integrations') }));
  }

  function integrationsController() {
    integrations ??= createIntegrations({ admin: adminApi, onChange: paintIntegrations, copy: (text) => copyToClipboard(document, text) });
    return integrations;
  }

  /** Rules or Labels, and the rule panel. */
  function paintRules(state) {
    if (paintedScreen === 'rules' || paintedScreen === 'labels') {
      repaint(document, root, renderRules(state, { tab: paintedScreen, head: headNow(paintedScreen), activity: mayOpen(allowed, 'activity') }));
    }
    if (!ruleDialog) return;
    repaint(document, ruleDialog, renderRuleDialog(state));
    if (state.panel && !ruleDialog.open && typeof ruleDialog.showModal === 'function') ruleDialog.showModal();
    if (!state.panel && ruleDialog.open && typeof ruleDialog.close === 'function') ruleDialog.close();
  }

  function rulesController() {
    rules ??= createRules({ admin: adminApi, query: active, onChange: paintRules });
    return rules;
  }

  function paintEditor(state) {
    if (!dialog) return;
    repaint(document, dialog, renderTeamDialog(state));
    if (state.open && !dialog.open && typeof dialog.showModal === 'function') dialog.showModal();
    if (!state.open && dialog.open && typeof dialog.close === 'function') dialog.close();
  }

  /** After a team is made, changed or deleted, Teams reads again; a deleted team's own screen goes back to the list. */
  function teamSaved({ team, deleted }) {
    const { id, filters } = parseHash(document.location.hash);
    if (id !== 'teams') return;
    if (deleted && filters.team && filters.team === team?.team_id) {
      document.location.hash = filters.preset ? `#teams?preset=${encodeURIComponent(filters.preset)}` : '#teams';
      return;
    }
    render();
  }

  function editorController() {
    editor ??= createTeamEditor({ admin: adminApi, query: active, onChange: paintEditor, onSaved: teamSaved });
    return editor;
  }

  /** The control an admin has in the Teams header: add a team on the list, edit it on a team's screen. */
  function toolFor(screen, filters) {
    if (screen.id !== 'teams' || !mayAdmin) return null;
    return filters.team ? renderTeamEdit(String(filters.team)) : renderTeamAdd();
  }

  /** Read a controller's page when it is first opened, or again when its last read did not finish. */
  async function loadOnce(controller, sameScreen) {
    if (!sameScreen || controller.state.status !== 'ready') await controller.load();
  }

  async function render() {
    let route = parseHash(document.location.hash);
    const moved = redirectFor(route, allowed);
    if (moved) {
      replaceHash(document, moved);
      route = parseHash(moved);
    }
    const { id, preset, filters } = route;
    const screen = SCREENS.find((s) => s.id === id) ?? SCREENS[0];
    // The navigation is repainted only when it changes.
    const navHtml = renderNav(navItems, sectionOf(screen.id)?.id ?? null);
    if (navHtml !== paintedNav) {
      paintedNav = navHtml;
      nav.innerHTML = navHtml;
    }
    // A change within a screen (a filter, a window, a search) updates it in place: no entrance
    // is replayed, and the search box keeps the focus it had.
    const sameScreen = screen.id === paintedScreen;
    // Moving to another screen swaps the content with a short fade; only the first load arrives in full.
    root.classList?.toggle('switched', !sameScreen && paintedScreen !== null);
    // Leaving Identity Providers takes the one-time SCIM token with it.
    if (!sameScreen && paintedScreen === 'directory') deployment?.leave();
    // Leaving Integrations takes a key being typed with it.
    if (!sameScreen && paintedScreen === 'integrations') integrations?.leave();
    const before = paintedScreen;
    paintedScreen = screen.id;
    root.classList?.toggle('same-screen', sameScreen);
    if (PAGE_IDS.has(screen.id) && !mayOpen(allowed, screen.id)) {
      root.innerHTML = renderScreen(notPermittedView(screen), {});
      return;
    }
    if (screen.kind === 'admin' || (screen.editor && mayAdmin)) {
      document.title = `${headNow(screen.id).title ?? screen.label} · ${brand}`;
      if (screen.id === 'rules' || screen.id === 'labels') {
        const controller = rulesController();
        paintRules(controller.state);
        // Rules and Labels are one rulebase: moving between them reads nothing again.
        await loadOnce(controller, before === 'rules' || before === 'labels');
        return;
      }
      if (screen.id === 'directory') {
        const controller = directoryController();
        const setup = deploymentController();
        paintDirectory();
        await Promise.all([loadOnce(controller, sameScreen), loadOnce(setup, sameScreen)]);
        return;
      }
      if (screen.id === 'integrations') {
        const controller = integrationsController();
        paintIntegrations(controller.state);
        await loadOnce(controller, sameScreen);
        return;
      }
      const controller = settingsController();
      paintSettings(controller.state);
      await loadOnce(controller, sameScreen);
      return;
    }
    const typing = sameScreen && document.activeElement?.name === 'subject';
    // While the next read runs the page keeps what it shows, dimmed: no blank, no jump.
    root.setAttribute?.('aria-busy', 'true');
    const { view, shell } = await dashboard.load(screen.id, { preset, filters, mayManage: mayAdmin, activity: mayOpen(allowed, 'activity') });
    root.setAttribute?.('aria-busy', 'false');
    const { preset: _preset, ...others } = filters;
    const setup = mayAdmin && screen.id === 'devices' && !filters.device;
    painted = {
      screen: screen.id,
      filters,
      view: setup ? { ...view, notes: [...(view.notes ?? []), ...DEPLOYMENT_NOTES] } : view,
      shell: { ...shell, ...headFor(screen, preset, others, allowed), presets: presetsFor(screen, preset, others), tool: toolFor(screen, filters) },
    };
    root.innerHTML = renderScreen(painted.view, { ...painted.shell, ...adminSection(screen.id, filters) });
    document.title = `${view?.title ?? screen.label} · ${brand}`;
    if (typing) root.querySelector?.('input[name="subject"]')?.focus();
    // An admin's section below or above the screen reads its own settings.
    if (setup) await loadOnce(deploymentController(), sameScreen);
    // An admin's device page reads that device's diagnostics requests.
    if (mayAdmin && screen.id === 'devices' && filters.device) {
      const device = String(filters.device);
      const controller = diagnosticsController();
      if (controller.state.device !== device || controller.state.status !== 'ready') await controller.load(device);
    }
    if (mayAdmin && screen.id === 'sanction') await loadOnce(settingsController(), sameScreen);
  }

  wireShell({ document, session });

  // The handler returns the render promise. A browser ignores a listener's return value, so this
  // costs nothing there; it means a test can await a navigation and read the finished page rather
  // than racing it.
  // A browser fires hashchange on the window, not the document. Moving anywhere closes the pop-up
  // and the rule panel; a rule kept in the unsaved list stays there.
  (document.defaultView ?? document).addEventListener('hashchange', () => {
    editor?.close();
    if (rules?.state.panel) rules.act({ rl: 'cancel' });
    return render();
  });
  // The browser closes a dialog itself on Escape; the pop-up's and the panel's state follow it.
  dialog?.addEventListener?.('close', () => {
    if (editor?.state.open) editor.close();
  });
  ruleDialog?.addEventListener?.('close', () => {
    if (rules?.state.panel) rules.act({ rl: 'cancel' });
  });
  document.addEventListener('submit', (event) => {
    const form = event.target;
    // Settings' retention form: Enter saves the value beside it.
    if (form?.dataset?.settingsForm === 'retention') {
      event.preventDefault();
      const field = form.querySelector?.('[data-settings-draft]');
      if (field && settings) settings.act({ action: 'save-retention', appliesTo: field.dataset.settingsDraft });
      return;
    }
    // Rules' forms: Enter in the filter applies it, Enter in the panel's search searches.
    if (form?.dataset?.rlForm !== undefined) {
      event.preventDefault();
      if (rules) rules.act({ rl: form.dataset.rlForm });
      return;
    }
    // The team pop-up's forms: Enter in the name saves, Enter in the search searches.
    if (form?.dataset?.teamForm !== undefined) {
      event.preventDefault();
      if (editor) editor.act({ team: form.dataset.teamForm });
      return;
    }
    // An integration's key form: Enter or its button connects.
    if (form?.dataset?.intForm !== undefined) {
      event.preventDefault();
      if (integrations) integrations.act({ int: 'connect', provider: form.dataset.intForm });
      return;
    }
    // The deployment sections' forms act through their buttons; Enter in the token label creates it.
    if (form?.dataset?.depForm !== undefined) {
      event.preventDefault();
      if (deployment && form.dataset.depForm !== 'none') deployment.act({ dep: form.dataset.depForm });
      return;
    }
    // The audit trail's filters: each field given narrows the list, in the window chosen.
    if (form?.dataset?.screen === 'audit') {
      event.preventDefault();
      const { filters } = parseHash(document.location.hash);
      const params = new URLSearchParams(filters.preset ? { preset: filters.preset } : {});
      for (const name of AUDIT_FILTERS) {
        const value = String(form.elements[name]?.value ?? '').trim();
        if (value !== '') params.set(name, value);
      }
      const query = params.toString();
      document.location.hash = `#audit${query ? `?${query}` : ''}`;
      return;
    }
    // The person screen's one input: a reference opens that person, anything else searches names.
    if (form?.dataset?.screen !== 'person') return;
    event.preventDefault();
    const value = form.elements.subject.value.trim();
    document.location.hash = /^u_[0-9a-f]{32}$/.test(value) ? `#person?subject=${encodeURIComponent(value)}`
      : value ? `#person?q=${encodeURIComponent(value)}` : '#person';
  });
  // The deployment sections' buttons name their action in data-dep; Settings' controls name
  // theirs in data-action, and Rules' in data-rl.
  document.addEventListener('click', (event) => {
    // A rule's ⋯ menu closes when anything else is clicked.
    for (const menu of document.querySelectorAll?.('details.rl-menu[open]') ?? []) {
      if (!menu.contains(event.target)) menu.open = false;
    }
    const rlTarget = event.target?.closest?.('[data-rl]');
    if (rlTarget && rules && rlTarget.tagName !== 'SELECT' && !rlTarget.disabled) {
      event.preventDefault();
      rules.act({ ...rlTarget.dataset });
      return;
    }
    const teamTarget = event.target?.closest?.('[data-team]');
    if (teamTarget && !teamTarget.disabled) {
      event.preventDefault();
      editorController().act({ ...teamTarget.dataset });
      return;
    }
    const action = event.target?.closest?.('[data-action]');
    if (action && settings && action.tagName !== 'SELECT' && !action.disabled) {
      event.preventDefault();
      const done = settings.act({ ...action.dataset });
      // A sanction decided on Policy → Tools is read again into the tools listed below it.
      if (action.dataset.action === 'sanction' && paintedScreen === 'sanction') Promise.resolve(done).then(() => render());
      return;
    }
    const dirTarget = event.target?.closest?.('[data-dir]');
    if (dirTarget && directory && !dirTarget.disabled) {
      event.preventDefault();
      directory.act({ ...dirTarget.dataset });
      return;
    }
    const intTarget = event.target?.closest?.('[data-int]');
    if (intTarget && integrations && !intTarget.disabled) {
      event.preventDefault();
      integrations.act({ ...intTarget.dataset });
      return;
    }
    const diagTarget = event.target?.closest?.('[data-diag]');
    if (diagTarget && diagnostics && !diagTarget.disabled) {
      event.preventDefault();
      diagnostics.act({ ...diagTarget.dataset });
      return;
    }
    const target = event.target?.closest?.('[data-dep]');
    if (!target || !deployment || target.disabled) return;
    event.preventDefault();
    deployment.act({ ...target.dataset });
  });
  // A per-tool override or other select names its action in data-action and fires change, not click.
  document.addEventListener('change', (event) => {
    const rlPick = event.target?.dataset?.rlPick;
    if (rlPick !== undefined && rules) {
      rules.act({ rl: 'user', value: rlPick });
      // A tick the panel refused does not stay ticked.
      event.target.checked = Boolean(rules.state.panel?.rule.users.includes(rlPick));
      return;
    }
    const rlSelect = event.target?.closest?.('select[data-rl]');
    if (rlSelect && rules) {
      rules.act({ rl: rlSelect.dataset.rl, value: rlSelect.value });
      return;
    }
    const pick = event.target?.dataset?.teamPick;
    if (pick !== undefined && editor) {
      editor.toggle(pick);
      // A tick the pop-up refused does not stay ticked.
      event.target.checked = Boolean(editor.state.picked[pick]);
      return;
    }
    const select = event.target?.closest?.('select[data-action]');
    if (!select || !settings) return;
    settings.act({ action: select.dataset.action, tool: select.dataset.tool, value: select.value });
  });
  document.addEventListener('input', (event) => {
    const draft = event.target?.dataset?.settingsDraft;
    if (draft && settings) settings.setRetentionDraft(draft, event.target.value);
    const ruleField = event.target?.dataset?.rlDraft;
    if (ruleField && rules) {
      rules.setDraft(ruleField, event.target.value);
      // Emptying the panel's search shows everything again.
      if (ruleField === 'query' && event.target.value === '' && rules.state.panel?.picker.term) rules.act({ rl: 'pick-search' });
    }
    if (event.target?.dataset?.rlQuery !== undefined && rules) {
      rules.setQuery(event.target.value);
      // Emptying the filter lists every rule again.
      if (event.target.value === '' && rules.state.filter !== '') rules.act({ rl: 'filter' });
    }
    const labelField = event.target?.dataset?.rlLabelDraft;
    if (labelField && rules) rules.setLabelDraft(labelField, event.target.value);
    const killRoute = event.target?.dataset?.killSwitchReason;
    if (killRoute && settings) settings.setKillSwitchReason(killRoute, event.target.value);
    const field = event.target?.dataset?.depDraft;
    if (field && deployment) deployment.setDraft(field, event.target.value);
    const intField = event.target?.dataset?.intDraft;
    if (intField && integrations) integrations.setDraft(intField, event.target.value);
    const teamField = event.target?.dataset?.teamDraft;
    if (teamField && editor) {
      editor.setDraft(teamField, event.target.value);
      // Emptying the search shows everything again, as clearing a search box does elsewhere.
      if (teamField === 'query' && event.target.value === '' && editor.state.term !== '') editor.search();
    }
  });
  await render();

  return { render, dashboard, deployment: () => deployment, diagnostics: () => diagnostics, settings: () => settings, directory: () => directory, integrations: () => integrations, rules: () => rules, editor: () => editor };
}
