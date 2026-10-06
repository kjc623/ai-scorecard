// roles.js — the closed set of analyst-app roles and what each may read.
//
// The role names are the `roles` claim of the product access token control-api mints; control-api
// maps each customer identity provider's role values onto these names, so this file never sees an
// IdP's spelling.
//
//   viewer          aggregates and the device list. No events, findings, people or content.
//   analyst         everything a viewer may, plus event and finding lists, the per-person screens
//                   and prompt-text search. Not stored-content retrieval.
//   content_reader  everything an analyst may, plus opening one event's stored prompt.
//   admin           configuration, sanction decisions and the audit trail. It reads no events,
//                   no findings and no content.
//
// Roles are not a ladder (content_reader is not "above" admin), so every check asks whether a role
// carries a capability, never how roles rank.

import { REASON, QueryError } from './errors.js';

/** The four product roles, in the order the navigation shows them. */
export const ROLES = Object.freeze(['viewer', 'analyst', 'content_reader', 'admin']);

/** Every capability the read path knows. */
export const CAPABILITIES = Object.freeze(['aggregate', 'device', 'subject', 'search', 'content', 'audit', 'settings', 'sanction']);

/** What each role may do. Frozen so a caller cannot widen a role by mutation. */
export const ROLE_CAPABILITIES = Object.freeze({
  viewer: Object.freeze(['aggregate', 'device']),
  analyst: Object.freeze(['aggregate', 'device', 'subject', 'search']),
  content_reader: Object.freeze(['aggregate', 'device', 'subject', 'search', 'content']),
  admin: Object.freeze(['aggregate', 'audit', 'settings', 'sanction']),
});

/**
 * The capability a source needs. A source missing here needs `subject`, the stricter common case,
 * so a new source is closed until it is classified.
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
  '/v1/list-export': 'subject',
  // The data-subject export and erasure are admin operations; they reuse the `settings`
  // capability, which only the admin role carries.
  '/v1/subject-export': 'settings',
  '/v1/subject-erasure': 'settings',
});

/** True for the four product roles; false for null and anything unrecognised. */
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

/**
 * The typed refusal for a wrong role: 403, state `unauthorised_role`, code `role`, naming the
 * missing capability. It is a QueryError, so it renders like every other rejection.
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
