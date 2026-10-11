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
  postureView, toolsView, toolView, unsanctionedView, teamsView, teamView, classesView,
  personView, peopleView, devicesView, eventView, auditView, refusalView,
  needsInputView,
} from './views.js';
import { buildPeopleDocument, buildTeamMembersDocument } from './dsl.js';
import { renderScreen, renderNav } from './render.js';
import { shellNavItems, groupOf, readCollapsed, wireShell } from './shell.js';
import { allowedPageIds, filterNavItems, mayOpen } from './session.js';
import { createDeployment } from './deployment.js';
import { renderDeployment } from './deployment-render.js';
import { createSettings } from './settings.js';
import { createDirectory } from './directory.js';
import { renderDirectory } from './directory-render.js';
import { renderSettings } from './settings-render.js';

/**
 * The screens. The event detail is reached from a row that names a submission; the rest are in the
 * navigation. Unsanctioned use and data classes are a switch on Usage.
 */
export const SCREENS = Object.freeze([
  Object.freeze({ id: 'posture', label: 'Overview', question: null, kind: 'posture' }),
  Object.freeze({ id: 'tools', label: 'Tools', question: 1, kind: 'answer', questionId: 'q1_tools_ranked' }),
  Object.freeze({ id: 'teams', label: 'Teams', question: 3, kind: 'answer', questionId: 'q3_team_growth' }),
  Object.freeze({ id: 'person', label: 'Users', question: 6, kind: 'input', questionId: 'q6_subject_series' }),
  Object.freeze({ id: 'devices', label: 'Devices', question: 7, kind: 'answer', questionId: 'q7_devices' }),
  Object.freeze({ id: 'audit', label: 'Audit', question: 10, kind: 'answer', questionId: 'q10_audit_trail' }),
  // Settings → Deployment reads and writes control-api's admin API, not the query API: it has no
  // question and no envelope, and boot() hands it to its own controller (deployment.js).
  Object.freeze({ id: 'deployment', label: 'Deployment', question: null, kind: 'admin' }),
  // Settings → Settings is the other admin screen, over the same admin API (settings.js).
  Object.freeze({ id: 'settings', label: 'Settings', question: null, kind: 'admin' }),
  // Settings → Directory & teams: reading the directory and managing teams (directory.js).
  Object.freeze({ id: 'directory', label: 'Directory & teams', question: null, kind: 'admin' }),
  Object.freeze({ id: 'event', label: 'Event detail', question: 9, kind: 'input', questionId: 'q9_event_detail' }),
]);

/** What the Usage screen can show: each is its own question, switched in place. */
const USAGE_MODES = Object.freeze({
  tools: Object.freeze({ label: 'Tools', questionId: 'q1_tools_ranked', view: toolsView }),
  classes: Object.freeze({ label: 'Data classes', questionId: 'q4_class_mix', view: classesView }),
  unsanctioned: Object.freeze({ label: 'Unsanctioned', questionId: 'q2_unsanctioned_users', view: unsanctionedView }),
});

function usageMode(filters) {
  return USAGE_MODES[filters?.view] ? filters.view : 'tools';
}

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
          // The summary reads three more questions.
          const tools = await part(() => ask('q1_tools_ranked', ctx));
          const classes = await part(() => ask('q4_class_mix', ctx));
          const findings = await part(() => ask('q5_findings', ctx));
          return { view: postureView({ devices, tools, classes, findings }, { preset: ctx.preset }), shell: shell() };
        }
        case 'answer': {
          if (screen.questionId === 'q6_subject_series' || screen.questionId === 'q9_event_detail') {
            return { view: needsInputView({ id: screen.id, title: screen.label, question: null, hint: 'This screen needs an identifier from another screen.' }), shell: shell() };
          }
          if (screen.id === 'tools') {
            if (params.filters.tool) return { view: await toolScreen(String(params.filters.tool), ctx), shell: shell() };
            const mode = USAGE_MODES[usageMode(params.filters)];
            return { view: mode.view(await ask(mode.questionId, ctx), { preset: ctx.preset }), shell: shell() };
          }
          if (screen.id === 'teams') return { view: await teamsScreen(params, ctx), shell: shell() };
          const state = await ask(screen.questionId, ctx);
          return { view: viewFor(screen.id, state, params), shell: shell() };
        }
        case 'input': {
          if (screen.id === 'person') {
            const subject = params.filters?.subject;
            if (!subject) {
              const search = String(params.filters?.q ?? '').trim();
              const people = readState(await api.run(buildPeopleDocument({ search, cursor: params.filters?.cursor ?? null })));
              return { view: peopleView(people, { search }), shell: shell() };
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
            return { view: personView(state, { subject, person }), shell: shell() };
          }
          const submissionId = params.filters?.submission_id;
          if (!submissionId) return { view: needsInputView({ id: 'event', title: 'Event detail', question: null, hint: 'Reached from a row that names a submission.' }), shell: shell() };
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
    // no cell in the window has nobody to list.
    const people = usage.data.some((row) => String(row.tool) === tool)
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

  /** The Teams screen: every team, or one team when the address names it. */
  async function teamsScreen(params, ctx) {
    const manageHref = params.mayManage ? '#directory' : null;
    const team = params.filters.team ? String(params.filters.team) : null;
    // An admin's team list names the teams with no usage, so a quiet team is listed rather than
    // absent, and one team's screen is named even when it has no cell in the window.
    const teams = params.mayManage && admin
      ? await part(async () => {
        const answer = await admin.directory();
        return answer.state === 'available' ? answer.data.teams : null;
      })
      : null;
    if (team) {
      const state = await ask('q3_team_growth', context({ preset: ctx.preset, filters: { team }, now: now() }));
      return teamView(state, { team, members: await teamMembers(state, ctx, [team]), teams, manageHref, preset: ctx.preset });
    }
    const state = await ask('q3_team_growth', ctx);
    const members = await teamMembers(state, ctx);
    return teamsView(state, { manageHref, members, teams, preset: ctx.preset });
  }

  /**
   * The people behind the teams with usage. A role that may not see people, or a read that fails,
   * leaves the team totals standing and says why the people are missing.
   */
  async function teamMembers(teams, ctx, ids = teams.data.map((row) => row.team).filter(Boolean)) {
    if (teams.isRefusal || ids.length === 0) return null;
    return refusable(async () => readState(await api.run(buildTeamMembersDocument({ window: ctx.window, teams: ids }))));
  }

  function viewFor(screenId, state, params) {
    switch (screenId) {
      case 'devices': return devicesView(state, { filters: params.filters ?? {}, now: now() });
      case 'audit': return state.resultState === 'audit_chain_broken' ? refusalView(state, { title: 'Audit' }) : auditView(state);
      default: return refusalView(state, { title: screenId });
    }
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

/** The navigation of shell.js, with each screen's question number for its tooltip. */
const NAV_ITEMS = shellNavItems({ questionOf: (id) => SCREENS.find((s) => s.id === id)?.question ?? null });

/** Screens whose question takes a time window, and the windows offered. */
const WINDOWED_KINDS = Object.freeze(['answer', 'input']);
const UNWINDOWED_SCREENS = Object.freeze(['devices', 'event']);
const PRESET_LABELS = Object.freeze({ h24: '24h', d7: '7d', d30: '30d', d90: '90d' });

/** The window switcher for a screen: each preset is a link that keeps the other parameters. */
function presetsFor(screen, preset, filters) {
  if (!WINDOWED_KINDS.includes(screen.kind) || UNWINDOWED_SCREENS.includes(screen.id)) return null;
  const current = PRESET_LABELS[preset] ? preset : 'd7';
  const items = Object.entries(PRESET_LABELS).map(([id, label]) => {
    const params = new URLSearchParams({ ...filters, preset: id });
    return { id, label, href: `#${screen.id}?${params.toString()}` };
  });
  return { current, items };
}

/** The switch a screen carries beside its window: what Usage shows. */
function switchFor(screen, preset, filters) {
  const link = (extra) => {
    const query = new URLSearchParams({ ...(PRESET_LABELS[preset] ? { preset } : {}), ...extra }).toString();
    return `#${screen.id}${query ? `?${query}` : ''}`;
  };
  // One tool's screen is reached from the list; it has no list to switch.
  if (screen.id === 'tools' && !filters.tool) {
    return {
      label: 'Show',
      current: usageMode(filters),
      items: Object.entries(USAGE_MODES).map(([id, mode]) => ({ id, label: mode.label, href: link(id === 'tools' ? {} : { view: id }) })),
    };
  }
  return null;
}

/** A screen the signed-in roles cannot use, said rather than attempted: the server refuses it anyway. */
function notPermittedView(screen) {
  return needsInputView({ id: screen.id, title: screen.label, question: null, hint: 'Your role cannot use this page. The navigation shows the pages it can.' });
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

function copyToClipboard(document, text) {
  const clipboard = document.defaultView?.navigator?.clipboard;
  if (!clipboard?.writeText) return Promise.reject(new Error('no clipboard'));
  return clipboard.writeText(text);
}

/**
 * Boot the dashboard into a document. The only function in this package that touches the DOM.
 *
 * Every read goes to the page's own origin: /v1/* for the query API and /admin/v1/* for
 * Settings → Deployment, which the dashboard server forwards with the session's product token.
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
  const navItems = filterNavItems(NAV_ITEMS, allowed);
  const navIds = new Set(NAV_ITEMS.map((item) => item.id));
  const collapsed = readCollapsed(document);
  // The tab is titled by the product name the page itself shows, so the two cannot disagree.
  const brand = document.querySelector?.('.brand h1')?.textContent?.trim() || 'Sundial';

  let paintedNav = null;
  let paintedScreen = null;
  let deployment = null;
  let settings = null;
  let directory = null;

  function paintDeployment(state) {
    if (paintedScreen === 'deployment') root.innerHTML = renderDeployment(state, { eyebrow: groupOf('deployment') });
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
    if (paintedScreen === 'settings') root.innerHTML = renderSettings(state, { eyebrow: groupOf('settings') });
  }

  function settingsController() {
    settings ??= createSettings({ admin: adminApi, onChange: paintSettings });
    return settings;
  }

  function paintDirectory(state) {
    if (paintedScreen === 'directory') root.innerHTML = renderDirectory(state, { eyebrow: groupOf('directory') });
  }

  function directoryController() {
    directory ??= createDirectory({ admin: adminApi, query: active, onChange: paintDirectory });
    return directory;
  }

  async function render() {
    const { id, preset, filters } = parseHash(document.location.hash);
    const screen = SCREENS.find((s) => s.id === id) ?? SCREENS[0];
    // The navigation is repainted only when it changes, so its groups do not re-open on every click.
    const navHtml = renderNav(navItems, screen.id, { collapsed: [...collapsed] });
    if (navHtml !== paintedNav) {
      paintedNav = navHtml;
      nav.innerHTML = navHtml;
    }
    // A change within a screen (a filter, a window, a search) updates it in place: no entrance
    // is replayed, and the search box keeps the focus it had.
    const sameScreen = screen.id === paintedScreen;
    // Moving to another screen swaps the content with a short fade; only the first load arrives in full.
    root.classList?.toggle('switched', !sameScreen && paintedScreen !== null);
    // Leaving Deployment takes the one-time SCIM token with it.
    if (!sameScreen && paintedScreen === 'deployment') deployment?.leave();
    paintedScreen = screen.id;
    root.classList?.toggle('same-screen', sameScreen);
    if (navIds.has(screen.id) && !mayOpen(allowed, screen.id)) {
      root.innerHTML = renderScreen(notPermittedView(screen), {});
      return;
    }
    if (screen.kind === 'admin') {
      if (screen.id === 'directory') {
        const controller = directoryController();
        paintDirectory(controller.state);
        if (!sameScreen || controller.state.status !== 'ready') await controller.load();
        return;
      }
      if (screen.id === 'settings') {
        const controller = settingsController();
        paintSettings(controller.state);
        if (!sameScreen || controller.state.status !== 'ready') await controller.load();
        return;
      }
      const controller = deploymentController();
      paintDeployment(controller.state);
      if (!sameScreen || controller.state.status !== 'ready') await controller.load();
      return;
    }
    const typing = sameScreen && document.activeElement?.name === 'subject';
    // While the next read runs the page keeps what it shows, dimmed: no blank, no jump.
    root.setAttribute?.('aria-busy', 'true');
    const { view, shell } = await dashboard.load(screen.id, { preset, filters, mayManage: mayOpen(allowed, 'directory') });
    root.setAttribute?.('aria-busy', 'false');
    const { preset: _preset, ...others } = filters;
    root.innerHTML = renderScreen(view, { ...shell, eyebrow: groupOf(screen.id), switch: switchFor(screen, preset, others), presets: presetsFor(screen, preset, others) });
    document.title = `${view?.title ?? screen.label} · ${brand}`;
    if (typing) root.querySelector?.('input[name="subject"]')?.focus();
  }

  wireShell({ document, collapsed, session });

  // The handler returns the render promise. A browser ignores a listener's return value, so this
  // costs nothing there; it means a test can await a navigation and read the finished page rather
  // than racing it.
  // A browser fires hashchange on the window, not the document.
  (document.defaultView ?? document).addEventListener('hashchange', render);
  document.addEventListener('submit', (event) => {
    const form = event.target;
    // Settings' retention form: Enter saves the value beside it.
    if (form?.dataset?.settingsForm === 'retention') {
      event.preventDefault();
      const field = form.querySelector?.('[data-settings-draft]');
      if (field && settings) settings.act({ action: 'save-retention', appliesTo: field.dataset.settingsDraft });
      return;
    }
    // Directory's forms: Enter runs the search or creates the team the form is for.
    if (form?.dataset?.dirForm !== undefined) {
      event.preventDefault();
      if (directory) directory.act({ dir: form.dataset.dirForm });
      return;
    }
    // Deployment's forms act through their buttons; Enter in the token label creates the token.
    if (form?.dataset?.depForm !== undefined) {
      event.preventDefault();
      if (deployment && form.dataset.depForm !== 'none') deployment.act({ dep: form.dataset.depForm });
      return;
    }
    // The person screen's one input: a reference opens that person, anything else searches names.
    if (form?.dataset?.screen !== 'person') return;
    event.preventDefault();
    const value = form.elements.subject.value.trim();
    document.location.hash = /^u_[0-9a-f]{32}$/.test(value) ? `#person?subject=${encodeURIComponent(value)}`
      : value ? `#person?q=${encodeURIComponent(value)}` : '#person';
  });
  // Deployment's buttons name their action in data-dep; Settings' controls name theirs in data-action.
  document.addEventListener('click', (event) => {
    const action = event.target?.closest?.('[data-action]');
    if (action && settings && action.tagName !== 'SELECT' && !action.disabled) {
      event.preventDefault();
      settings.act({ ...action.dataset });
      return;
    }
    const dirTarget = event.target?.closest?.('[data-dir]');
    if (dirTarget && directory && !dirTarget.disabled) {
      event.preventDefault();
      directory.act({ ...dirTarget.dataset });
      return;
    }
    const target = event.target?.closest?.('[data-dep]');
    if (!target || !deployment || target.disabled) return;
    event.preventDefault();
    deployment.act({ ...target.dataset });
  });
  // A per-tool override or other select names its action in data-action and fires change, not click.
  document.addEventListener('change', (event) => {
    const select = event.target?.closest?.('select[data-action]');
    if (!select || !settings) return;
    settings.act({ action: select.dataset.action, tool: select.dataset.tool, value: select.value });
  });
  document.addEventListener('input', (event) => {
    const draft = event.target?.dataset?.settingsDraft;
    if (draft && settings) settings.setRetentionDraft(draft, event.target.value);
    const ruleField = event.target?.dataset?.ruleDraft;
    if (ruleField && settings) settings.setRuleDraft(ruleField, event.target.value);
    const killRoute = event.target?.dataset?.killSwitchReason;
    if (killRoute && settings) settings.setKillSwitchReason(killRoute, event.target.value);
    const field = event.target?.dataset?.depDraft;
    if (field && deployment) deployment.setDraft(field, event.target.value);
    const dirField = event.target?.dataset?.dirDraft;
    if (dirField && directory) directory.setDraft(dirField, event.target.value);
  });
  await render();

  return { render, dashboard, deployment: () => deployment, settings: () => settings, directory: () => directory };
}
