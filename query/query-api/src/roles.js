// roles.js — the closed set of analyst-app roles and what each may read.
//
// The role names are the app-role claims the session's token carries (docs/04 §2.2, docs/06
// §4.1). The set is deliberately small: the design's six-role list was reconciled with the
// product owner before task 11 was built, and the reconciliation is recorded in this task's
// REPORT.md. The short version:
//
//   viewer          aggregates and the device list — no events, no findings, no people page,
//                   no content. (The owner ruled that the device list, with the user it now
//                   names, is visible to a viewer; the brief's "no per-person data" sentence
//                   is set aside for that one screen.)
//   analyst         everything a viewer may, plus event and finding lists, the per-person
//                   screens, and prompt-text search. Not stored-content retrieval.
//   content_reader  everything an analyst may, plus opening one event's stored prompt.
//   admin           configuration, sanction decisions, exports and the audit trail. The
//                   design's `tenant_admin`, `auditor` and `privacy_officer` folded into it:
//                   the owner chose one administrative role over three. It reads no events,
//                   no findings and no content.
//
// `dev` is not a product role. It exists only behind SAC_DEV_TRUST_PRINCIPAL and carries
// every capability, so the lab's pre-session escape hatch does not have to be rewritten as
// a role. A token can never name it: token verification accepts ROLES only.
//
// A capability, not a rank. The roles are not a ladder — content_reader is not "above"
// admin — so every check is `roleAllows(role, capability)`, never an ordering.

import { REASON, QueryError } from './errors.js';

/** The four product roles, in the order the navigation shows them. */
export const ROLES = Object.freeze(['viewer', 'analyst', 'content_reader', 'admin']);

/** The lab-only escape-hatch role. Never minted from a token. */
export const DEV_ROLE = 'dev';

/** Every capability the read path knows. */
export const CAPABILITIES = Object.freeze(['aggregate', 'device', 'subject', 'search', 'content', 'audit', 'settings', 'export', 'sanction']);

/** What each role may do. Frozen so a caller cannot widen a role by mutation. */
export const ROLE_CAPABILITIES = Object.freeze({
  viewer: Object.freeze(['aggregate', 'device']),
  analyst: Object.freeze(['aggregate', 'device', 'subject', 'search']),
  content_reader: Object.freeze(['aggregate', 'device', 'subject', 'search', 'content']),
  admin: Object.freeze(['aggregate', 'audit', 'settings', 'export', 'sanction']),
  [DEV_ROLE]: CAPABILITIES,
});

/**
 * The capability a source needs. The default is `subject` — the stricter of the two common
 * cases — so a source added later is closed until it is classified here. Device sources and
 * the audit source are the overrides.
 */
const SOURCE_CAPABILITY = Object.freeze({
  'mart.v_tool_usage': 'aggregate',
  'mart.agg_tool_period': 'aggregate',
  'mart.agg_org_period': 'aggregate',
  'mart.agg_class_period': 'aggregate',
  'ops.coverage_snapshot': 'aggregate',
  'mart.agg_device_period': 'device',
  'mart.v_device_liveness': 'device',
  'mart.agg_tool_user_period': 'subject',
  'mart.agg_user_period': 'subject',
  'mart.v_finding': 'subject',
  'ingest.submission': 'subject',
  'ops.audit': 'audit',
});

/** The capability an endpoint needs, for the routes that are not a source read. */
export const ENDPOINT_CAPABILITY = Object.freeze({
  '/v1/finding-review': 'subject',
  '/v1/tool-sanction': 'sanction',
  '/v1/content-search': 'search',
  '/v1/content/retrieval': 'content',
});

/** True for the four product roles; false for `dev`, null and anything unrecognised. */
export function isKnownRole(role) {
  return typeof role === 'string' && ROLES.includes(role);
}

/** The capability a source needs, or `subject` for a source this file has not classified. */
export function capabilityForSource(sourceId) {
  return SOURCE_CAPABILITY[sourceId] ?? 'subject';
}

/** The capability a path needs, or null when the path is not capability-gated here. */
export function capabilityForEndpoint(path) {
  return ENDPOINT_CAPABILITY[path] ?? null;
}

/** Does `role` carry `capability`? An unknown role carries nothing. */
export function roleAllows(role, capability) {
  const caps = ROLE_CAPABILITIES[role];
  return Array.isArray(caps) && caps.includes(capability);
}

/** Does any role in `roles` carry `capability`? The session's roles are a set. */
export function rolesAllow(roles, capability) {
  return Array.isArray(roles) && roles.some((role) => roleAllows(role, capability));
}

/** The capabilities a role carries, as a plain array, for the dashboard's own hiding. */
export function capabilitiesOf(role) {
  return ROLE_CAPABILITIES[role] ?? Object.freeze([]);
}

/**
 * The typed refusal a caller gets when the role is wrong. It is a `QueryError`, so the HTTP
 * layer renders it through the same §13 envelope as every other rejection: 403, state
 * `unauthorised_role`, code `role`, and the capability that was missing.
 */
export function unauthorisedRole(required, { role = null, path = null } = {}) {
  return new QueryError(
    'unauthorised_role',
    REASON.ROLE,
    role
      ? `Role "${role}" may not ${required}. Ask an administrator for a role that may.`
      : `This request must be made with a role that may ${required}.`,
    { required, ...(role ? { role } : {}), ...(path ? { path } : {}) },
  );
}
