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
  postureView, toolsView, unsanctionedView, teamsView, classesView,
  personView, devicesView, eventView, auditView, refusalView,
  needsInputView,
} from './views.js';
import { renderScreen, renderNav } from './render.js';
import { shellNavItems, groupOf, readCollapsed, wireShell } from './shell.js';
import { allowedPageIds, filterNavItems, mayOpen } from './session.js';
import { createDeployment } from './deployment.js';
import { renderDeployment } from './deployment-render.js';

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
 * @param {Date} [input.now]
 */
export function createDashboard({ api, now = () => new Date() }) {
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
          // The summary reads three more questions. One that fails leaves its panel empty rather
          // than taking the page down.
          const part = async (questionId) => {
            try {
              return await ask(questionId, ctx);
            } catch {
              return null;
            }
          };
          const tools = await part('q1_tools_ranked');
          const classes = await part('q4_class_mix');
          const findings = await part('q5_findings');
          return { view: postureView({ devices, tools, classes, findings }), shell: shell() };
        }
        case 'answer': {
          if (screen.questionId === 'q6_subject_series' || screen.questionId === 'q9_event_detail') {
            return { view: needsInputView({ id: screen.id, title: screen.label, question: null, hint: 'This screen needs an identifier from another screen.' }), shell: shell() };
          }
          if (screen.id === 'tools') {
            const mode = USAGE_MODES[usageMode(params.filters)];
            return { view: mode.view(await ask(mode.questionId, ctx)), shell: shell() };
          }
          const state = await ask(screen.questionId, ctx);
          return { view: viewFor(screen.id, state, params), shell: shell() };
        }
        case 'input': {
          if (screen.id === 'person') {
            const subject = params.filters?.subject;
            if (!subject) return { view: needsInputView({ id: 'person', title: 'Users', question: null, hint: null, search: Object.freeze({ value: '' }) }), shell: shell() };
            const state = await ask('q6_subject_series', context({ preset: ctx.preset, filters: { subject }, now: now() }));
            return { view: personView(state, { subject }), shell: shell() };
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

  function viewFor(screenId, state, params) {
    switch (screenId) {
      case 'teams': return teamsView(state);
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
  if (screen.id === 'tools') {
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
  const dashboard = createDashboard({ api: active });
  // A signed-in role may see fewer pages than the shell names; the server says which, and
  // query-api and control-api refuse what lies behind the rest.
  const session = givenSession !== undefined ? givenSession : await loadSession();
  const allowed = allowedPageIds(session);
  const navItems = filterNavItems(NAV_ITEMS, allowed);
  const navIds = new Set(NAV_ITEMS.map((item) => item.id));
  const collapsed = readCollapsed(document);

  let paintedNav = null;
  let paintedScreen = null;
  let deployment = null;

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
      const controller = deploymentController();
      paintDeployment(controller.state);
      if (!sameScreen || controller.state.status !== 'ready') await controller.load();
      return;
    }
    const typing = sameScreen && document.activeElement?.name === 'subject';
    const { view, shell } = await dashboard.load(screen.id, { preset, filters });
    const { preset: _preset, ...others } = filters;
    root.innerHTML = renderScreen(view, { ...shell, eyebrow: groupOf(screen.id), switch: switchFor(screen, preset, others), presets: presetsFor(screen, preset, others) });
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
    // Deployment's forms act through their buttons; Enter in the token label creates the token.
    if (form?.dataset?.depForm !== undefined) {
      event.preventDefault();
      if (deployment && form.dataset.depForm !== 'none') deployment.act({ dep: form.dataset.depForm });
      return;
    }
    // The person screen's one input: put the reference in the address, which is what renders it.
    if (form?.dataset?.screen !== 'person') return;
    event.preventDefault();
    const subject = form.elements.subject.value.trim();
    document.location.hash = subject ? `#person?subject=${encodeURIComponent(subject)}` : '#person';
  });
  // Deployment's buttons name their action in data-dep; what is typed into a label is kept as it is typed.
  document.addEventListener('click', (event) => {
    const target = event.target?.closest?.('[data-dep]');
    if (!target || !deployment || target.disabled) return;
    event.preventDefault();
    deployment.act({ ...target.dataset });
  });
  document.addEventListener('input', (event) => {
    const field = event.target?.dataset?.depDraft;
    if (field && deployment) deployment.setDraft(field, event.target.value);
  });
  await render();

  return { render, dashboard, deployment: () => deployment };
}
