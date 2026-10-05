// questions.js — the ten questions of docs/04 §3, as request builders.
//
// Each one returns a request body produced by dsl.js, so a question cannot express anything the
// DSL does not admit. The ten are deliberately narrow: the API's own template layer pins the
// source, the dimensions, the measures and the ordering, and the client supplies only values.

import { buildTemplate, buildDocument, windowFor } from './dsl.js';
import { SOURCES, TEMPLATES } from './vocab.js';

/** The context every screen carries: what the analyst has chosen, and nothing else. */
export function context(overrides = {}) {
  const preset = overrides.preset ?? 'd7';
  const window = overrides.window ?? windowFor(preset, overrides.now ? new Date(overrides.now) : new Date());
  return Object.freeze({
    preset,
    window,
    bucket: overrides.bucket ?? window.bucket,
    limit: overrides.limit ?? null,
    cursor: overrides.cursor ?? null,
    filters: Object.freeze({ ...(overrides.filters ?? {}) }),
  });
}

/** Drop undefined/empty filter values so a parameter the analyst did not set is not sent. */
function present(filters) {
  const out = {};
  for (const [key, value] of Object.entries(filters)) {
    if (value === undefined || value === null || value === '') continue;
    out[key] = value;
  }
  return out;
}

/**
 * @typedef {object} Question
 * @property {string} id            one of TEMPLATE_NAMES
 * @property {number} number        1..10
 * @property {string} title
 * @property {string} screen        the information-architecture screen it belongs to
 * @property {(ctx: object) => object} request
 */

/** @type {Record<string, Question>} */
export const QUESTIONS = Object.freeze({
  q1_tools_ranked: {
    id: 'q1_tools_ranked',
    number: 1,
    title: TEMPLATES.q1_tools_ranked.title,
    screen: 'tools',
    request: (ctx) => buildTemplate('q1_tools_ranked', present({
      window: ctx.window,
      bucket: ctx.bucket,
      limit: ctx.limit ?? 400,
      tool: ctx.filters.tool,
      sanctioned_state: ctx.filters.sanctioned_state,
    })),
  },

  q2_unsanctioned_users: {
    id: 'q2_unsanctioned_users',
    number: 2,
    title: TEMPLATES.q2_unsanctioned_users.title,
    screen: 'unsanctioned',
    // A tool filter is not optional in practice: the API refuses a subject-grouped window beyond
    // seven days unless it is narrowed by a tool or a subject, and naming the tool is what makes
    // the question "who is using THIS" rather than "list everyone".
    request: (ctx) => buildTemplate('q2_unsanctioned_users', present({
      window: ctx.window,
      bucket: ctx.bucket,
      limit: ctx.limit ?? 500,
      tool: ctx.filters.tool,
      subject: ctx.filters.subject,
      sanctioned_state: ctx.filters.sanctioned_state ?? 'unsanctioned',
    })),
  },

  q3_team_growth: {
    id: 'q3_team_growth',
    number: 3,
    title: TEMPLATES.q3_team_growth.title,
    screen: 'teams',
    request: (ctx) => buildTemplate('q3_team_growth', present({
      window: ctx.window,
      bucket: ctx.bucket,
      limit: ctx.limit ?? 2000,
      department: ctx.filters.department,
      population: ctx.filters.population,
    })),
  },

  q4_class_mix: {
    id: 'q4_class_mix',
    number: 4,
    title: TEMPLATES.q4_class_mix.title,
    screen: 'classes',
    request: (ctx) => buildTemplate('q4_class_mix', present({
      window: ctx.window,
      bucket: ctx.bucket,
      limit: ctx.limit ?? 2000,
      dimensions: ctx.filters.dimensions,
      class: ctx.filters.class,
      severity: ctx.filters.severity,
    })),
  },

  q5_findings: {
    id: 'q5_findings',
    number: 5,
    title: TEMPLATES.q5_findings.title,
    screen: 'findings',
    request: (ctx) => buildTemplate('q5_findings', present({
      window: ctx.window,
      limit: ctx.limit ?? 50,
      cursor: ctx.cursor,
      severity: ctx.filters.severity,
      rule: ctx.filters.rule,
      review_state: ctx.filters.review_state,
      subject: ctx.filters.subject,
      tool: ctx.filters.tool,
      class: ctx.filters.class,
    })),
  },

  q6_subject_series: {
    id: 'q6_subject_series',
    number: 6,
    title: TEMPLATES.q6_subject_series.title,
    screen: 'person',
    request: (ctx) => buildTemplate('q6_subject_series', present({
      subject: ctx.filters.subject,
      window: ctx.window,
      bucket: ctx.bucket,
      limit: ctx.limit ?? 400,
    })),
  },

  q7_devices: {
    id: 'q7_devices',
    number: 7,
    title: TEMPLATES.q7_devices.title,
    screen: 'devices',
    request: (ctx) => buildTemplate('q7_devices', present({
      limit: ctx.limit ?? 50,
      cursor: ctx.cursor,
      liveness: ctx.filters.liveness,
      collector_state: ctx.filters.collector_state,
      collector: ctx.filters.collector,
      device_os: ctx.filters.device_os,
      managed_state: ctx.filters.managed_state,
      region: ctx.filters.region,
    })),
  },

  q8_activity: {
    id: 'q8_activity',
    number: 8,
    title: TEMPLATES.q8_activity.title,
    screen: 'activity',
    request: (ctx) => buildTemplate('q8_activity', present({
      window: ctx.window,
      limit: ctx.limit ?? 50,
      cursor: ctx.cursor,
      subject: ctx.filters.subject,
      tool: ctx.filters.tool,
      device: ctx.filters.device,
      class: ctx.filters.class,
      content_state: ctx.filters.content_state,
      action: ctx.filters.action,
      mode: ctx.filters.mode,
      department: ctx.filters.department,
    })),
  },

  q9_event_detail: {
    id: 'q9_event_detail',
    number: 9,
    title: TEMPLATES.q9_event_detail.title,
    screen: 'event',
    request: (ctx) => buildTemplate('q9_event_detail', present({
      submission_id: ctx.filters.submission_id,
      received_at_hint: ctx.filters.received_at_hint,
    })),
  },

  q10_audit_trail: {
    id: 'q10_audit_trail',
    number: 10,
    title: TEMPLATES.q10_audit_trail.title,
    screen: 'audit',
    request: (ctx) => buildTemplate('q10_audit_trail', present({
      window: ctx.window,
      limit: ctx.limit ?? 50,
      cursor: ctx.cursor,
      actor: ctx.filters.actor,
      action: ctx.filters.action,
      object_type: ctx.filters.object_type,
      subject: ctx.filters.subject,
      case: ctx.filters.case,
    })),
  },
});

export const QUESTION_NAMES = Object.freeze(Object.keys(QUESTIONS));

/** The API's documented parameter list for a question, for a screen that wants to show its bounds. */
export function questionParams(id) {
  return TEMPLATES[id]?.params ?? [];
}

/** The source a question reads, so the UI can say where a number came from. */
export function questionSource(id) {
  return TEMPLATES[id]?.source ?? null;
}

export { buildDocument, SOURCES };
