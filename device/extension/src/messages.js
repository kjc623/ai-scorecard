/**
 * messages.js — the extension's copy of the native-messaging vocabulary in device/protocol
 * (native.go, envelope.go). Field names and the closed sets are transcribed verbatim; a contract
 * test fails if they drift from the Go source.
 *
 * Wire shape: one JSON object per message, `{ type, version, id?, body? }`.
 *
 * An `ack` means capture-core accepted the message, not that it was delivered; the spool owns
 * that distinction. An ObservationMessage never says `has_content: true` without content, and never
 * carries content at M0; `validateObservation()` mirrors ObservationMessage.Validate().
 */

import { bytesToBase64, toBytes } from './codec.js';

/** protocol.Version / protocol.MaxNativeMessageBytes. */
export const NATIVE_MESSAGE_VERSION = 1;
export const MAX_NATIVE_MESSAGE_BYTES = 1 << 20; // 1 MiB
/** protocol.MaxAttachmentBytes — a transport ceiling; the effective cap is bundle policy. */
export const MAX_ATTACHMENT_BYTES = 64 << 20;

/** Extension -> capture-core. */
export const TYPE = Object.freeze({
  OBSERVATION: 'observation',
  ATTACHMENT_MANIFEST: 'attachment_manifest',
  ATTACHMENT_CHUNK: 'attachment_chunk',
  ATTACHMENT_COMPLETE: 'attachment_complete',
  HEALTH: 'health',
  POLICY_SYNC: 'policy_sync',
  MODE_QUERY: 'mode_query',
  DECISION_RECORD: 'decision_record',
});

/** capture-core -> extension. */
export const CORE_TYPE = Object.freeze({
  ACK: 'ack',
  REFUSAL: 'refusal',
  POLICY_BUNDLE: 'policy_bundle',
  MODE_ANSWER: 'mode_answer',
  HEALTH_SNAPSHOT: 'health_snapshot',
});

/** protocol.RefusalReason, a closed set. */
export const REFUSAL = Object.freeze({
  ATTACHMENT_TOO_LARGE: 'attachment_too_large',
  MODE_FORBIDS_READ: 'mode_forbids_read',
  UNKNOWN_TYPE: 'unknown_type',
  VERSION_MISMATCH: 'version_mismatch',
  MALFORMED: 'malformed',
  CHANNEL_CLOSING: 'channel_closing',
  QUEUE_FULL: 'queue_full',
});

/** protocol.Route. The extension produces the three `ext.*` routes. */
export const ROUTE = Object.freeze({
  EXT_WEB_REQUEST: 'ext.web_request',
  EXT_PAGE_CONTEXT: 'ext.page_context',
  EXT_DOM: 'ext.dom',
  PROXY_TLS: 'proxy.tls',
  PROXY_LOOPBACK: 'proxy.loopback',
  PROC_DETECT: 'proc.detect',
  CLI_SHIM: 'cli.shim',
});

/** Routes ObservationMessage.Validate() accepts from this extension. */
export const EXTENSION_ROUTES = Object.freeze([ROUTE.EXT_WEB_REQUEST, ROUTE.EXT_PAGE_CONTEXT, ROUTE.EXT_DOM]);

/** protocol.CollectionMode. */
export const MODE = Object.freeze({ M0: 'm0', M1: 'm1', M2: 'm2', M3: 'm3' });

/** CollectionMode.ReadsContent(). An absent or unknown mode does not permit reading the body. */
export function modeReadsContent(mode) {
  return mode === MODE.M1 || mode === MODE.M2 || mode === MODE.M3;
}

/** protocol.Action. */
export const ACTION = Object.freeze({ BLOCKED: 'blocked', WARNED: 'warned', LOGGED: 'logged' });

/** protocol.Counter, closed at seven: a provider reports these and nothing else. */
export const COUNTERS = Object.freeze([
  'observed',
  'emitted',
  'skipped_not_generative',
  'blind_tunnelled',
  'not_cooperative',
  'dropped',
  'errors',
]);

/** Named access to the same closed set, so no call site hard-codes a counter string. */
export const COUNTER = Object.freeze({
  OBSERVED: 'observed',
  EMITTED: 'emitted',
  SKIPPED_NOT_GENERATIVE: 'skipped_not_generative',
  BLIND_TUNNELLED: 'blind_tunnelled',
  NOT_COOPERATIVE: 'not_cooperative',
  DROPPED: 'dropped',
  ERRORS: 'errors',
});

/** protocol.Detail, the closed per-provider detail vocabulary. */
export const DETAIL = Object.freeze({
  NONE: '',
  CLASSIFIER_UNAVAILABLE: 'classifier_unavailable',
  SPOOL_UNWRITABLE: 'spool_unwritable',
  UPSTREAM_FAILURE: 'upstream_failure',
  UPSTREAM_UNREACHABLE: 'upstream_unreachable',
  CLIENT_PINNED: 'client_pinned',
  NOT_EFFECTIVE_PROXY: 'not_effective_proxy',
  TLS_PROBE_FAILED: 'tls_probe_failed',
  PORT_HELD_BY_OTHER: 'port_held_by_other',
  COOLING_DOWN: 'cooling_down',
  KILLED: 'killed',
  ENUMERATION_PARTIAL: 'enumeration_partial',
  SIGNATURE_SET_STALE: 'signature_set_stale',
  BUDGET_EXHAUSTED: 'budget_exhausted',
  MODEL_UNAVAILABLE: 'model_unavailable',
  NORMALISE_TRUNCATED: 'normalise_truncated',
  PARSER_FAILED: 'parser_failed',
  CONTENT_UNPROCESSABLE: 'content_unprocessable',
  HOST_UNREACHABLE: 'host_unreachable',
  RELEASE_LOAD_FAILED: 'release_load_failed',
  PARSER_MEMORY: 'parser_memory',
  PARSER_TIMEOUT: 'parser_timeout',
  PARSER_CRASH: 'parser_crash',
  PARSER_OUTPUT_CAP: 'parser_output_cap',
  BUNDLE_SIGNATURE_INVALID: 'bundle_signature_invalid',
  BUNDLE_SCHEMA_INVALID: 'bundle_schema_invalid',
  BUNDLE_VERSION_REGRESSION: 'bundle_version_regression',
  BUNDLE_ARTEFACT_MISSING: 'bundle_artefact_missing',
  // The manifest declares webRequestBlocking but the install was not granted it (any install that
  // is not force-installed by policy): observation works, cancelling a request does not.
  ENFORCEMENT_UNAVAILABLE: 'enforcement_unavailable',
  CONTENT_OVER_CAP: 'content_over_cap',
  UNDECODABLE_CONTENT: 'undecodable_content',
  VERSION_MISMATCH: 'version_mismatch',
  MODE_VIOLATION: 'mode_violation',
  CREDENTIAL_EXPIRED: 'credential_expired',
  TRUST_INSTALL_FAILED: 'trust_install_failed',
  TRUST_VERIFY_FAILED: 'trust_verify_failed',
  SHIM_PROFILE_MISSING: 'shim_profile_missing',
  SHIM_CA_BUNDLE_UNREADABLE: 'shim_ca_bundle_unreadable',
  SHIM_NOT_INHERITED: 'shim_not_inherited',
  IDENTITY_UNRESOLVED: 'identity_unresolved',
  DISABLED_BY_POLICY: 'disabled_by_policy',
});

export const DETAILS = Object.freeze(Object.values(DETAIL).filter(Boolean));

/** protocol.CollectorState. */
export const STATE = Object.freeze({
  HEALTHY: 'healthy',
  DEGRADED: 'degraded',
  ABSENT: 'absent',
  TAMPERED: 'tampered',
});

/** This provider's collector name, as registered in the schema's `ref.collector`. */
export const COLLECTOR_NAME = 'capture_extension';

/** Wrap a body in the NativeMessage envelope. */
export function frame(type, body, id = undefined) {
  const msg = { type, version: NATIVE_MESSAGE_VERSION };
  if (id !== undefined && id !== null && id !== '') msg.id = id;
  if (body !== undefined) msg.body = body;
  return msg;
}

/** Parse an inbound NativeMessage. Returns null for anything that is not one. */
export function unframe(raw) {
  if (raw === null || typeof raw !== 'object' || Array.isArray(raw)) return null;
  if (typeof raw.type !== 'string' || raw.type === '') return null;
  if (raw.version !== NATIVE_MESSAGE_VERSION) {
    return { type: raw.type, version: raw.version, id: raw.id, body: raw.body, versionMismatch: true };
  }
  return { type: raw.type, version: raw.version, id: raw.id, body: raw.body, versionMismatch: false };
}

/** UTF-8 byte length of the framed message, checked against the 1 MiB ceiling. */
export function framedByteLength(type, body, id) {
  return new TextEncoder().encode(JSON.stringify(frame(type, body, id))).byteLength;
}

/**
 * Mirror of ObservationMessage.Validate(). Returns null when the message is well-formed, or a
 * {reason, message} pair drawn from the closed refusal set.
 */
export function validateObservation(o) {
  if (!o || typeof o !== 'object') return { reason: REFUSAL.MALFORMED, message: 'observation is not an object' };
  if (!EXTENSION_ROUTES.includes(o.route)) {
    return { reason: REFUSAL.MALFORMED, message: `route ${JSON.stringify(o.route)} is not an extension route` };
  }
  if (typeof o.tool_fingerprint !== 'string' || o.tool_fingerprint === '') {
    return { reason: REFUSAL.MALFORMED, message: 'observation has no tool fingerprint' };
  }
  const hasContentBytes = typeof o.content === 'string' && o.content.length > 0;
  if (!o.has_content && hasContentBytes) {
    return {
      reason: REFUSAL.MALFORMED,
      message: `observation carries ${o.content.length} content bytes while has_content is false`,
    };
  }
  if (o.has_content && !hasContentBytes) {
    return { reason: REFUSAL.MALFORMED, message: 'observation claims has_content but carries no content' };
  }
  if (o.decision !== undefined && o.decision !== null) {
    if (![ACTION.BLOCKED, ACTION.WARNED, ACTION.LOGGED].includes(o.decision.action)) {
      return { reason: REFUSAL.MALFORMED, message: `decision action ${JSON.stringify(o.decision.action)} outside the closed set` };
    }
  }
  for (const a of o.attachments || []) {
    if (!a || typeof a.name !== 'string' || a.name === '') {
      return { reason: REFUSAL.MALFORMED, message: 'attachment descriptor without a name' };
    }
    if (a.size_bytes > MAX_ATTACHMENT_BYTES) {
      return {
        reason: REFUSAL.ATTACHMENT_TOO_LARGE,
        message: `attachment ${JSON.stringify(a.name)} is ${a.size_bytes} bytes, cap is ${MAX_ATTACHMENT_BYTES}`,
      };
    }
  }
  if (o.degraded_reason !== undefined && o.degraded_reason !== null && !DETAILS.includes(o.degraded_reason)) {
    return { reason: REFUSAL.MALFORMED, message: `degraded_reason ${JSON.stringify(o.degraded_reason)} outside the closed set` };
  }
  return null;
}

/**
 * Build an outbound observation body with every protocol field present exactly once.
 *
 * `content` is base64 in every case, text and binary alike: ObservationMessage.Content is a Go
 * `[]byte`, which encoding/json base64-decodes. Raw text would fail to decode, and text that happens
 * to be valid base64 would decode to different bytes than `content_digest` describes. Encoding here,
 * where the value is created, means no call site can forget it.
 */
export function observationBody(fields) {
  const body = {
    client_id: fields.client_id,
    route: fields.route,
    tool_fingerprint: fields.tool_fingerprint,
    occurred_at: fields.occurred_at,
    monotonic_offset_ms: fields.monotonic_offset_ms,
    size_bytes: fields.size_bytes,
    has_content: Boolean(fields.has_content),
  };
  if (fields.content_digest) body.content_digest = fields.content_digest;
  if (fields.has_content && fields.content !== undefined && fields.content !== null) {
    body.content = encodeContent(fields.content);
  }
  if (fields.content_is_binary) body.content_is_binary = true;
  if (fields.over_cap) body.over_cap = true;
  if (fields.attachments && fields.attachments.length) body.attachments = fields.attachments;
  if (fields.decision) body.decision = fields.decision;
  if (fields.degraded_reason) body.degraded_reason = fields.degraded_reason;
  if (fields.page_context_attachments) body.page_context_attachments = true;
  return body;
}

/**
 * The one encoder for `content`. Accepts bytes, a base64 string (`alreadyBase64`), or decoded text,
 * and returns the base64 that `[]byte` requires.
 *
 * @param {Uint8Array|ArrayBuffer|string} value
 * @param {{alreadyBase64?: boolean}} [opts]
 */
export function encodeContent(value, opts = {}) {
  if (typeof value === 'string') {
    if (opts.alreadyBase64) return value;
    return bytesToBase64(new TextEncoder().encode(value));
  }
  return bytesToBase64(toBytes(value));
}
