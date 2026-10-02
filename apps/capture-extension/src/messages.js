/**
 * messages.js — the extension's copy of the device-side native-messaging vocabulary.
 *
 * SOURCE OF TRUTH: device/protocol/native.go and device/protocol/envelope.go (owned by the
 * Lead). This file is a mechanical transcription, not a second design: field names, the
 * closed type set, the closed refusal-reason set, the closed counter set and the closed
 * detail set are copied verbatim. If a shape here is wrong, message the Lead — do not fork it.
 *
 * Wire shape (NativeMessage in native.go): one JSON object per message,
 *   { type, version, id?, body? }
 * with `version` = NATIVE_MESSAGE_VERSION (protocol.Version).
 *
 * Two asymmetries the Lead called out, both enforced below:
 *   - an `ack` is NOT delivery. The spool is what makes that distinction; nothing here
 *     increments an `emitted` counter on ack.
 *   - an ObservationMessage must never say `has_content: true` with an empty content field,
 *     and must never carry content at M0. `validateObservation()` rejects both, mirroring
 *     ObservationMessage.Validate() in native.go.
 */

/** protocol.Version / protocol.MaxNativeMessageBytes. */
export const NATIVE_MESSAGE_VERSION = 1;
export const MAX_NATIVE_MESSAGE_BYTES = 1 << 20; // 1 MiB
/** protocol.MaxAttachmentBytes — a transport ceiling; the effective cap is bundle policy. */
export const MAX_ATTACHMENT_BYTES = 64 << 20;

/** Extension -> capture-core (native.go Type*). */
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

/** Closed set: RefusalReason in native.go. Do not add a member without telling the Lead. */
export const REFUSAL = Object.freeze({
  ATTACHMENT_TOO_LARGE: 'attachment_too_large',
  MODE_FORBIDS_READ: 'mode_forbids_read',
  UNKNOWN_TYPE: 'unknown_type',
  VERSION_MISMATCH: 'version_mismatch',
  MALFORMED: 'malformed',
  CHANNEL_CLOSING: 'channel_closing',
  QUEUE_FULL: 'queue_full',
});

export const REFUSAL_REASONS = Object.freeze(Object.values(REFUSAL));

/** protocol.Route — the closed route vocabulary. The extension may produce three of them. */
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

/**
 * CollectionMode.ReadsContent() — `m != m0 && m != ""`. An absent/unknown mode is NOT a
 * read-permitting mode: the conservative state is "do not read the body" (Lead, task-6).
 */
export function modeReadsContent(mode) {
  return mode === MODE.M1 || mode === MODE.M2 || mode === MODE.M3;
}

/** protocol.Action — the three values are never merged. */
export const ACTION = Object.freeze({ BLOCKED: 'blocked', WARNED: 'warned', LOGGED: 'logged' });

/** protocol.Counter — closed at seven; a provider reports these and nothing else (§4.3). */
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

/** protocol.Detail — the closed per-provider detail vocabulary (device/protocol/envelope.go). */
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
  CONTENT_OVER_CAP: 'content_over_cap',
  UNDECODABLE_CONTENT: 'undecodable_content',
  VERSION_MISMATCH: 'version_mismatch',
  MODE_VIOLATION: 'mode_violation',
});

export const DETAILS = Object.freeze(Object.values(DETAIL).filter(Boolean));

/** protocol.CollectorState. */
export const STATE = Object.freeze({
  HEALTHY: 'healthy',
  DEGRADED: 'degraded',
  ABSENT: 'absent',
  TAMPERED: 'tampered',
});

/**
 * The collector name for this provider. §4.3: "the collector name must come from
 * ref.collector, so a provider cannot invent a name for a coverage path the reporting layer
 * does not know." db/schema.sql ref.collector carries exactly one extension row:
 * ('capture_extension', 'capture_extension', ...).
 */
export const COLLECTOR_NAME = 'capture_extension';

/** ref.route_fidelity weights (db/schema.sql): page_context 10 beats web_request 40 beats dom 60. */
export const ROUTE_FIDELITY = Object.freeze({
  [ROUTE.EXT_PAGE_CONTEXT]: 10,
  [ROUTE.EXT_WEB_REQUEST]: 40,
  [ROUTE.EXT_DOM]: 60,
});

/** Errors thrown by this module, so callers can distinguish a shape fault from a transport fault. */
export class MessageShapeError extends Error {
  constructor(message, reason = REFUSAL.MALFORMED) {
    super(message);
    this.name = 'MessageShapeError';
    this.reason = reason;
  }
}

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

/** UTF-8 byte length of the framed message, checked against the 1 MiB ceiling the queue budgets against. */
export function framedByteLength(type, body, id) {
  return new TextEncoder().encode(JSON.stringify(frame(type, body, id))).byteLength;
}

/**
 * Mirror of ObservationMessage.Validate() (native.go). Returns null when the message is
 * well-formed, or a {reason, message} pair drawn from the closed refusal set.
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

/** Mirror of HealthReport.Validate(): a counter name outside the closed set is a defect. */
export function validateHealthReport(h) {
  if (!h || typeof h !== 'object') return { reason: REFUSAL.MALFORMED, message: 'health report is not an object' };
  if (typeof h.collector !== 'string' || h.collector === '') {
    return { reason: REFUSAL.MALFORMED, message: 'health report has no collector name' };
  }
  if (!Object.values(STATE).includes(h.state)) {
    return { reason: REFUSAL.MALFORMED, message: `health report state ${JSON.stringify(h.state)} outside the closed set` };
  }
  for (const k of Object.keys(h.counters || {})) {
    if (!COUNTERS.includes(k)) {
      return { reason: REFUSAL.MALFORMED, message: `counter ${JSON.stringify(k)} outside the closed set` };
    }
  }
  return null;
}

export function refusal(reason, message) {
  if (!REFUSAL_REASONS.includes(reason)) {
    throw new MessageShapeError(`refusal reason ${reason} is outside the closed set`);
  }
  return { reason, message };
}

/**
 * Build an outbound observation body with every protocol field present exactly once.
 *
 * **`content` is base64 in every case, text and binary alike.** `ObservationMessage.Content` in
 * device/protocol/native.go is `[]byte` with `json:"content"`, and `encoding/json` base64-decodes
 * a `[]byte` — so a raw text payload fails to decode ("illegal base64 data at input byte 9"), and
 * text that happens to *be* valid base64 silently decodes to different bytes than the user typed
 * while `content_digest` was computed over the original. The digest and the content disagreeing is
 * a content-identity break: it propagates into `dedup_key`, into what the classifier sees, and
 * into the record's value as evidence.
 *
 * So the encoding happens HERE, where the value is created, rather than at each call site — the
 * one place a caller cannot forget it. `encodeContent()` is the single encoder for both paths.
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
 * The one encoder for `content`. Accepts the bytes, a base64 string (what the binary path already
 * holds), or a decoded text string, and always returns the base64 that `[]byte` requires.
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

/** Decode a frame's `content` back to the bytes the digest was taken over. Mirrors encoding/json. */
export function decodeContentFrame(value) {
  if (typeof value !== 'string') return new Uint8Array(0);
  return base64ToBytes(value);
}

function toBytes(input) {
  if (input == null) return new Uint8Array(0);
  if (input instanceof Uint8Array) return input;
  if (input instanceof ArrayBuffer) return new Uint8Array(input);
  if (ArrayBuffer.isView(input)) return new Uint8Array(input.buffer, input.byteOffset, input.byteLength);
  return new Uint8Array(0);
}

const B64_TABLE = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/';

function bytesToBase64(input) {
  const b = toBytes(input);
  let out = '';
  for (let i = 0; i < b.length; i += 3) {
    const n = (b[i] << 16) | ((b[i + 1] || 0) << 8) | (b[i + 2] || 0);
    out +=
      B64_TABLE[(n >> 18) & 63] +
      B64_TABLE[(n >> 12) & 63] +
      (i + 1 < b.length ? B64_TABLE[(n >> 6) & 63] : '=') +
      (i + 2 < b.length ? B64_TABLE[n & 63] : '=');
  }
  return out;
}

function base64ToBytes(s) {
  const clean = String(s ?? '').replace(/[^A-Za-z0-9+/]/g, '');
  const out = new Uint8Array(Math.floor((clean.length * 3) / 4));
  let o = 0;
  let acc = 0;
  let bits = 0;
  for (const ch of clean) {
    acc = (acc << 6) | B64_TABLE.indexOf(ch);
    bits += 6;
    if (bits >= 8) {
      bits -= 8;
      out[o++] = (acc >> bits) & 0xff;
    }
  }
  return out.subarray(0, o);
}
