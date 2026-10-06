// subject.js — the subject export and the subject erasure request.
//
// Both start from one user reference, resolved to the canonical ref through ops.user_ref_alias (a
// device may have derived a person under several refs; ingest.record_event stores the canonical one,
// so every event, finding and stored prompt is reachable from it).
//
// Subject export reads the person's events, findings and stored prompts (the prompts decrypted by
// content-vault, the only component that can) and packs them into one archive. Subject erasure
// records a request that the jobs' erase job performs later, so the removal of immutable
// observations stays on the retention path and is always backed by an ops.erasure_receipt.

import { REASON, unsupported } from './errors.js';
import { rowsToCsv } from './export.js';
import { zipSync } from './zip.js';

/** A user reference is at most this long, matching the DSL's subject filter value cap. */
const MAX_SUBJECT_REF = 256;
/** One subject export is bounded; a larger one is refused rather than truncated silently. */
export const SUBJECT_EXPORT_MAX_ROWS = 50_000;

/** Resolve a subject reference to its canonical ref: an alias maps to its canonical, anything else is used as-is. */
export const RESOLVE_SUBJECT_SQL = `
SELECT coalesce(
         (SELECT a.user_ref FROM ops.user_ref_alias a
           WHERE a.tenant_id = ops.current_tenant() AND a.alias_ref = $1::text),
         $1::text) AS user_ref,
       (SELECT ud.display_name FROM ops.user_dim ud
         WHERE ud.tenant_id = ops.current_tenant() AND ud.user_ref = coalesce(
           (SELECT a.user_ref FROM ops.user_ref_alias a
             WHERE a.tenant_id = ops.current_tenant() AND a.alias_ref = $1::text), $1::text)) AS display_name`;

/** The subject's events (the deduplicated submissions), newest first. Metadata only. */
export const SUBJECT_SUBMISSIONS_SQL = `
SELECT s.submission_id::text AS submission_id, s.received_at, s.first_occurred_at, s.last_occurred_at,
       s.user_ref AS subject, s.tool_fingerprint AS tool, ops.tool_display_name(s.tool_fingerprint) AS tool_name,
       s.device_id::text AS device, s.collection_mode AS mode, s.policy_action AS action,
       s.content_state AS content_state, s.winning_source AS route, s.kind AS detection_basis,
       s.prompt_kind, s.merge_confidence, s.confidence, s.observation_count, s.size_bytes,
       s.labels, s.observed_routes
  FROM ingest.submission s
 WHERE s.tenant_id = ops.current_tenant() AND s.user_ref = $1::text
 ORDER BY s.received_at DESC, s.submission_id DESC
 LIMIT $2::int`;

/** The subject's observations (one row per route), newest first. Metadata only. */
export const SUBJECT_OBSERVATIONS_SQL = `
SELECT o.event_id::text AS event_id, o.device_id::text AS device, o.subject_name, o.tool_fingerprint AS tool,
       o.direction, o.kind, o.occurred_at, o.received_at, o.source, o.confidence,
       o.collection_mode AS mode, o.size_bytes, o.labels, o.classifier_version
  FROM ingest.observation o
 WHERE o.tenant_id = ops.current_tenant() AND o.user_ref = $1::text
 ORDER BY o.received_at DESC, o.event_id DESC
 LIMIT $2::int`;

/** The subject's findings, newest first. */
export const SUBJECT_FINDINGS_SQL = `
SELECT f.submission_id::text AS submission_id, f.detected_at, f.rule_id AS rule, f.rule_title,
       f.class_code AS class, f.severity, f.user_ref AS subject, f.tool_fingerprint AS tool,
       ops.tool_display_name(f.tool_fingerprint) AS tool_name, f.collection_mode AS mode,
       f.decided_locally, f.review_state, f.reviewed_by, f.reviewed_at, f.policy_action
  FROM mart.v_finding f
 WHERE f.tenant_id = ops.current_tenant() AND f.user_ref = $1::text
 ORDER BY f.detected_at DESC, f.submission_id DESC
 LIMIT $2::int`;

/** Store one generated export, single-use and short-lived. */
export const INSERT_EXPORT_SQL = `
INSERT INTO ops.export (tenant_id, export_id, kind, source, subject_ref, row_count, content_type, payload,
                        requested_by, requested_at, expires_at)
VALUES (ops.current_tenant(), $1::uuid, $2::text, $3::text, $4::text, $5::int, $6::text, $7::bytea,
        $8::text, now(), now() + $9::interval)
RETURNING export_id::text, expires_at`;

/** Read one export for download. */
export const EXPORT_FOR_DOWNLOAD_SQL = `
SELECT export_id::text, kind, content_type, payload, expires_at, used_at
  FROM ops.export
 WHERE tenant_id = ops.current_tenant() AND export_id = $1::uuid`;

/** Claim an export for download: single-use, and only while unexpired. */
export const CLAIM_EXPORT_SQL = `
UPDATE ops.export
   SET used_at = now()
 WHERE tenant_id = ops.current_tenant() AND export_id = $1::uuid AND used_at IS NULL AND expires_at > now()
RETURNING kind, source, subject_ref, content_type, payload`;

/** Record a subject erasure request; the jobs' erase job performs it. */
export const INSERT_ERASURE_REQUEST_SQL = `
INSERT INTO ops.erasure_request (tenant_id, request_id, subject_ref, requested_by, requested_at)
VALUES (ops.current_tenant(), $1::uuid, $2::text, $3::text, now())
RETURNING request_id::text`;

/**
 * Validate a subject reference request body: {subject_ref}. Unknown keys and a body-supplied tenant
 * are refused rather than ignored.
 *
 * @param {unknown} body
 * @returns {Readonly<{subjectRef: string}>}
 */
export function validateSubjectRequest(body) {
  if (!body || typeof body !== 'object' || Array.isArray(body)) {
    throw unsupported(REASON.MALFORMED_DOCUMENT, 'The request body must be a JSON object.', {});
  }
  for (const key of Object.keys(body)) {
    if (key === 'subject_ref') continue;
    if (key === 'tenant_id' || key === 'tenant') {
      throw unsupported(REASON.TENANT_IN_REQUEST, 'The tenant comes from the authenticated session and never from the request.', { key });
    }
    throw unsupported(REASON.UNKNOWN_KEY, `Unknown key "${key}" in the request.`, { key, known_keys: ['subject_ref'] });
  }
  const subjectRef = body.subject_ref;
  if (typeof subjectRef !== 'string' || subjectRef.trim() === '') {
    throw unsupported(REASON.TYPE_MISMATCH, 'subject_ref must be a non-empty string naming one person.', { field: 'subject_ref' });
  }
  if (subjectRef.length > MAX_SUBJECT_REF) {
    throw unsupported(REASON.VALUE_TOO_LONG, `subject_ref is capped at ${MAX_SUBJECT_REF} characters.`, { field: 'subject_ref' });
  }
  return Object.freeze({ subjectRef: subjectRef.trim() });
}

/** The CSV columns for the archive's events, observations and findings. */
const ARCHIVE_COLUMNS = Object.freeze({
  submissions: Object.freeze([
    'submission_id', 'received_at', 'first_occurred_at', 'last_occurred_at', 'subject', 'tool',
    'tool_name', 'device', 'mode', 'action', 'content_state', 'route', 'detection_basis',
    'prompt_kind', 'merge_confidence', 'confidence', 'observation_count', 'size_bytes', 'labels',
    'observed_routes',
  ]),
  observations: Object.freeze([
    'event_id', 'device', 'subject_name', 'tool', 'direction', 'kind', 'occurred_at', 'received_at',
    'source', 'confidence', 'mode', 'size_bytes', 'labels', 'classifier_version',
  ]),
  findings: Object.freeze([
    'submission_id', 'detected_at', 'rule', 'rule_title', 'class', 'severity', 'subject', 'tool',
    'tool_name', 'mode', 'decided_locally', 'review_state', 'reviewed_by', 'reviewed_at',
    'policy_action',
  ]),
});

/**
 * Build the subject-export archive: a manifest, the person's events, observations and findings as
 * CSV, and one file per stored prompt (the plaintext content-vault decrypted).
 *
 * @param {{subjectRef:string, canonicalRef:string, displayName:string|null, generatedAt:string,
 *          submissions:Array, observations:Array, findings:Array, prompts:Array}} input
 * @returns {Buffer}
 */
export function buildSubjectArchive({ subjectRef, canonicalRef, displayName, generatedAt, submissions, observations, findings, prompts }) {
  const manifest = JSON.stringify({
    subject_ref: subjectRef,
    canonical_ref: canonicalRef,
    display_name: displayName,
    generated_at: generatedAt,
    counts: {
      events: submissions.length,
      observations: observations.length,
      findings: findings.length,
      stored_prompts: prompts.length,
    },
  }, null, 2);

  const entries = [
    { name: 'manifest.json', data: Buffer.from(`${manifest}\n`, 'utf8') },
    { name: 'events.csv', data: Buffer.from(rowsToCsv(ARCHIVE_COLUMNS.submissions, submissions), 'utf8') },
    { name: 'observations.csv', data: Buffer.from(rowsToCsv(ARCHIVE_COLUMNS.observations, observations), 'utf8') },
    { name: 'findings.csv', data: Buffer.from(rowsToCsv(ARCHIVE_COLUMNS.findings, findings), 'utf8') },
  ];

  prompts.forEach((prompt, i) => {
    const plaintext = Buffer.from(prompt.plaintext, 'base64');
    const header = `event_id: ${prompt.event_id ?? ''}\nsubmission_id: ${prompt.submission_id ?? ''}\nprompt_kind: ${prompt.prompt_kind ?? ''}\nraw_digest: ${prompt.raw_digest ?? ''}\n\n`;
    entries.push({ name: `prompts/${String(i + 1).padStart(4, '0')}.txt`, data: Buffer.concat([Buffer.from(header, 'utf8'), plaintext]) });
  });

  return zipSync(entries);
}
