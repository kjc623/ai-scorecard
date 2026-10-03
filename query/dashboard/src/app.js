// app.js — the shell: hash routing, one data layer, and the screen table of §11.2.
//
// Everything here is wiring. The decisions live in states.js (what an envelope means), views.js
// (what a screen shows) and unavailable.js (what cannot be shown); app.js only decides which one
// runs and puts the result on the page.
//
// `createDashboard` is deliberately DOM-free: it takes an api and returns `load(screenId, params)`,
// so a test can drive every screen against the stub transport without a browser. `boot()` is the
// only function that touches a document.

import { createQueryApi } from './transport.js';
import { scenarioTransport } from './scenarios.js';
import { readState } from './states.js';
import { QUESTIONS, context } from './questions.js';
import {
  postureView, toolsView, unsanctionedView, teamsView, classesView, findingsView,
  personView, devicesView, activityView, eventView, auditView, refusalView,
  needsInputView,
} from './views.js';
import { degradedCollectionView, noApiView, unavailableView } from './unavailable.js';
import { renderScreen, renderNav, renderGallery } from './render.js';
import { SCENARIOS, SCENARIO_NAMES } from './fixtures.js';

/** The information architecture of docs/04 §11.2, in the order a reader moves through it. */
export const SCREENS = Object.freeze([
  Object.freeze({ id: 'posture', label: 'Posture', question: null, kind: 'posture' }),
  Object.freeze({ id: 'tools', label: 'Tools', question: 1, kind: 'answer', questionId: 'q1_tools_ranked' }),
  Object.freeze({ id: 'unsanctioned', label: 'Unsanctioned', question: 2, kind: 'answer', questionId: 'q2_unsanctioned_users' }),
  Object.freeze({ id: 'classes', label: 'Classes', question: 4, kind: 'answer', questionId: 'q4_class_mix' }),
  Object.freeze({ id: 'teams', label: 'Teams', question: 3, kind: 'answer', questionId: 'q3_team_growth' }),
  Object.freeze({ id: 'person', label: 'Person', question: 6, kind: 'input', questionId: 'q6_subject_series' }),
  Object.freeze({ id: 'findings', label: 'Findings', question: 5, kind: 'answer', questionId: 'q5_findings' }),
  Object.freeze({ id: 'activity', label: 'Activity', question: 8, kind: 'answer', questionId: 'q8_activity' }),
  Object.freeze({ id: 'event', label: 'Event detail', question: 9, kind: 'input', questionId: 'q9_event_detail' }),
  Object.freeze({ id: 'search', label: 'Content search', question: null, kind: 'no-api' }),
  Object.freeze({ id: 'devices', label: 'Devices', question: 7, kind: 'answer', questionId: 'q7_devices' }),
  Object.freeze({ id: 'degraded', label: 'Degraded collection', question: null, kind: 'degraded' }),
  Object.freeze({ id: 'audit', label: 'Audit', question: 10, kind: 'answer', questionId: 'q10_audit_trail' }),
  Object.freeze({ id: 'exports', label: 'Exports', question: null, kind: 'no-api' }),
  Object.freeze({ id: 'settings', label: 'Settings', question: null, kind: 'no-api' }),
  Object.freeze({ id: 'unavailable', label: 'What we cannot show', question: null, kind: 'catalogue' }),
  Object.freeze({ id: 'gallery', label: 'State gallery', question: null, kind: 'gallery' }),
]);

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
  /** The strip is persistent, so the last coverage and freshness blocks seen are kept. */
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
  async function load(screenId, params = {}) {
    const screen = SCREENS.find((s) => s.id === screenId) ?? SCREENS[0];
    const ctx = context({ preset: params.preset ?? 'd7', filters: params.filters ?? {}, now: now() });
    try {
      switch (screen.kind) {
        case 'posture': {
          const devices = await ask('q7_devices', context({ preset: ctx.preset, limit: 500, filters: params.filters ?? {}, now: now() }));
          return { view: postureView({ devices, watermarks: [...observed.watermarks.values()] }), shell: shell() };
        }
        case 'answer': {
          if (screen.questionId === 'q6_subject_series' || screen.questionId === 'q9_event_detail') {
            return { view: needsInputView({ id: screen.id, title: screen.label, question: null, hint: 'This screen needs an identifier from another screen.' }), shell: shell() };
          }
          const state = await ask(screen.questionId, ctx);
          return { view: viewFor(screen.id, state, params), shell: shell() };
        }
        case 'input': {
          if (screen.id === 'person') {
            const subject = params.filters?.subject;
            if (!subject) return { view: needsInputView({ id: 'person', title: 'Person', question: null, hint: 'Enter a user reference. There is no screen that lists people.' }), shell: shell() };
            const state = await ask('q6_subject_series', context({ preset: ctx.preset, filters: { subject }, now: now() }));
            return { view: personView(state, { subject }), shell: shell() };
          }
          const submissionId = params.filters?.submission_id;
          if (!submissionId) return { view: needsInputView({ id: 'event', title: 'Event detail', question: null, hint: 'Reached from a finding or an activity row.' }), shell: shell() };
          const state = await ask('q9_event_detail', context({ filters: { submission_id: submissionId, received_at_hint: params.filters?.received_at_hint }, now: now() }));
          return { view: eventView(state), shell: shell() };
        }
        case 'degraded': {
          const devices = await ask('q7_devices', context({ preset: ctx.preset, limit: 500, filters: params.filters ?? {}, now: now() }));
          return { view: degradedCollectionView({ devices }), shell: shell() };
        }
        case 'catalogue':
          return { view: unavailableView(), shell: shell() };
        case 'no-api':
          return {
            view: noApiView({
              id: screen.id,
              title: screen.label,
              subtitle: screen.id === 'search'
                ? 'Text search runs in content-vault against an index this component cannot read, through an endpoint this API does not expose.'
                : 'This screen has no endpoint behind it in the query API.',
            }),
            shell: shell(),
          };
        case 'gallery':
        default:
          return { view: null, shell: shell(), gallery: true };
      }
    } catch (error) {
      return { view: refusalFrom(error, { title: screen.label }), shell: shell() };
    }
  }

  function viewFor(screenId, state, params) {
    switch (screenId) {
      case 'tools': return toolsView(state);
      case 'unsanctioned': return unsanctionedView(state);
      case 'classes': return classesView(state);
      case 'teams': return teamsView(state);
      case 'findings': return findingsView(state);
      case 'activity': return activityView(state);
      case 'devices': return devicesView(state);
      case 'audit': return state.resultState === 'audit_chain_broken' ? refusalView(state, { title: 'Audit' }) : auditView(state);
      default: return refusalView(state, { title: screenId });
    }
    void params;
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
  return { id, scenario: id === 'gallery' ? parts[1] : undefined, preset: filters.preset, filters };
}

const NAV_ITEMS = SCREENS.map((s) => ({ id: s.id, label: s.label, question: s.question }));

/**
 * Boot the dashboard into a document. The only function in this package that touches the DOM.
 *
 * @param {object} input
 * @param {Document} input.document
 * @param {string} [input.scenario] one of SCENARIO_NAMES; ignored when `api` is supplied
 * @param {object} [input.api]      a real api, e.g. over httpTransport
 */
export async function boot({ document, scenario = 'realistic', api } = {}) {
  const root = document.getElementById('app');
  const nav = document.getElementById('nav');
  // One dashboard for the whole session, so the coverage strip persists across navigation: the
  // strip is a property of the shell, not of a screen.
  const stubs = api ? null : scenarioTransport(scenario);
  const active = api ?? createQueryApi({ transport: stubs });
  const dashboard = createDashboard({ api: active });

  async function render() {
    const { id, scenario: routeScenario, preset, filters } = parseHash(document.location.hash);
    if (routeScenario && stubs && typeof stubs.setScenario === 'function') stubs.setScenario(routeScenario);
    const screen = SCREENS.find((s) => s.id === id) ?? SCREENS[0];
    nav.innerHTML = renderNav(NAV_ITEMS, screen.id);
    const { view, shell, gallery } = await dashboard.load(screen.id, { preset, filters });
    if (gallery) {
      root.innerHTML = renderGallery(SCENARIO_NAMES.map((name) => ({ id: name, label: SCENARIOS[name].label })), stubs ? stubs.scenario() : 'live')
        + `<p class="gallery-hint">Scenario in force: <strong>${stubs ? stubs.label() : 'live API'}</strong>. Pick another, then visit any screen.</p>`;
      return;
    }
    root.innerHTML = renderScreen(view, shell);
  }

  // The handler returns the render promise. A browser ignores a listener's return value, so this
  // costs nothing there; it means a test can await a navigation and read the finished page rather
  // than racing it.
  document.addEventListener('hashchange', render);
  await render();

  return { render, dashboard };
}
