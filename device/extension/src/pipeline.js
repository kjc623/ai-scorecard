/**
 * pipeline.js — one observed request in, zero or one queued observation out, plus the decision
 * whether the request may proceed.
 *
 *   1. observe what the browser reports (the metadata lane carries no body at all);
 *   2. classify by shape, size and structure, before any content question is asked;
 *   3. emit only a positive match, after the effective mode has decided what may be read.
 *
 * `readBodyForMode()` is the only door to the bytes, and at M0 it returns `bytes: null` with an
 * `unread_reason`, so no path produces an M0 observation with a digest or content.
 *
 * The inline decision is the only work that delays a request; digesting, fingerprinting and
 * sending happen after it is released. At a mode that reads content, the files the user attached in
 * the sending tab are transferred to capture-core before the observation that names them.
 */

import { ExtError } from './adapter.js';
import { CONFIRMATION_WINDOW_MS, DECISION, DEFAULT_RULE_ID, REASON, decideSync, decideWithConfirmation, degradedDetail } from './enforce.js';
import { sha256Prefixed } from './codec.js';
import {
  COUNTER,
  DETAIL,
  MODE,
  ROUTE,
  TYPE,
  modeReadsContent,
  observationBody,
  validateObservation,
} from './messages.js';
import { classifyWithResponse, CANDIDATE_FLOOR, predicateRequest } from './predicate.js';
import { computeToolFingerprint } from './tool-fingerprint.js';
import { COOLDOWN_ERROR_CODE } from './native.js';

/** Bounded: a service worker can be killed at any time, and an unanswered candidate must not accumulate. */
export const MAX_CANDIDATES = 256;

/** How many queued observations one drain pass attempts. */
export const DRAIN_BATCH = 20;

export function createPipeline({
  adapter,
  policy,
  native,
  queue,
  health,
  attachments = null,
  clock = () => Date.now(),
  monotonic = () => (globalThis.performance ? globalThis.performance.now() : Date.now()),
}) {
  const counters = health.counters;
  let sequence = 0;
  /** @type {Map<string, object>} requestId -> candidate awaiting its response contract */
  const candidates = new Map();
  /** One drain at a time; a burst coalesces instead of stacking overlapping sends. */
  let drainPromise = null;
  let drainAgain = false;

  function nextClientId() {
    sequence += 1;
    return `c${sequence}`;
  }

  /**
   * The mode gate: the only place in this module that decides whether bytes may be touched.
   * @param {{mode: string}} resolution  from `resolveMode()` or `policy.modeFor()`
   * @returns {{mode: string, resolution: object, bytes: Uint8Array|null, unread_reason: string|null}}
   */
  function readBodyForMode(resolution, body) {
    if (!modeReadsContent(resolution.mode)) {
      return { mode: resolution.mode, resolution, bytes: null, unread_reason: 'mode_forbids_read' };
    }
    return { mode: resolution.mode, resolution, bytes: body ? body.bytes : null, unread_reason: null };
  }

  /**
   * The effective mode for a request to a tool: from the bundle when it holds every input, otherwise
   * capture-core's answer to a `mode_query`. No answer resolves to M0.
   */
  async function resolveMode(target) {
    if (!policy.needsCoreMode()) return policy.modeFor(target);
    try {
      const answer = await native.sendRequest(TYPE.MODE_QUERY, { tool_fingerprint: target.tool_fingerprint || '', host: target.host || '' });
      return policy.modeFor(target, { override: (answer && answer.body && answer.body.mode) || '' });
    } catch {
      return { mode: MODE.M0, reason: 'core_unanswered', policy_version: policy.snapshot().policy_version, unsigned: true };
    }
  }

  /** The predicate record, shared by both lanes so they classify identically. */
  function shapeInput(detail, body, tab_context) {
    return {
      url: detail.url,
      method: detail.method,
      headers: detail.requestHeaders || {},
      size_bytes: body ? body.size : 0,
      bytes: body ? body.bytes : null,
      truncated: Boolean(body && body.truncated_reason),
      form: body && body.form ? body.form : null,
      form_text: body && body.source === 'form' ? formTextOf(body) : null,
      tab_context,
      bundle: policy.discoverySets(),
    };
  }

  /**
   * Observe one request on the body lane, which is registered with `requestBody` and only matches
   * destinations the policy does not resolve to M0.
   *
   * @returns {Promise<{cancel: boolean, queued: boolean, skipped: boolean, observation: object|null, mode: string, candidate: object|null}>}
   */
  async function captureWithBody({ detail, body, tab_context = [] }) {
    counters.inc(COUNTER.OBSERVED);
    const url = detail.url;
    const host = hostOf(url);

    // Shape classification comes before the mode is consulted for content.
    const request = shapeInput(detail, body, tab_context);
    const shape = predicateRequest(request);

    // A request above the candidate floor but below the threshold is held, not emitted, so its
    // response contract can settle it once the response arrives.
    if (!shape.match && shape.score >= CANDIDATE_FLOOR) {
      holdCandidate({
        request_id: detail.requestId,
        client_id: null,
        tool_fingerprint: null,
        vector: null,
        shape,
        request,
        size_bytes: body.size,
        content: null,
        content_digest: null,
        decision: null,
        mode: MODE.M0,
        host,
      });
    }

    if (!shape.match) {
      // A negative match is counted, never emitted.
      counters.inc(COUNTER.SKIPPED_NOT_GENERATIVE);
      return { cancel: false, queued: false, skipped: true, observation: null, mode: MODE.M0, candidate: null };
    }

    // The mode decides what may be read, and only then is anything read. The tool's mode is keyed by
    // its fingerprint, which comes from the destination alone, so one tool keeps one fingerprint
    // whatever its own mode is and whatever the request carries.
    const fingerprint = await computeToolFingerprint(adapter, request);
    const gate = readBodyForMode(await resolveMode({ host, tool_fingerprint: fingerprint.fingerprint }), body);
    const mode = gate.mode;

    const decision = await decide({
      url,
      host,
      tool_fingerprint: fingerprint.fingerprint,
      tab_id: detail.tabId,
      request_id: detail.requestId,
    });

    let contentDigest;
    let contentBytes;
    let contentIsBinary = false;
    if (gate.bytes && modeReadsContent(mode)) {
      // The digest is over the bytes as sent, for text and binary alike.
      contentDigest = await sha256Prefixed(adapter.crypto, gate.bytes);
      contentIsBinary = body.decode.encoding === 'binary';
      // observationBody base64-encodes the bytes for the wire.
      contentBytes = gate.bytes;
    }

    // A decision that degraded names its cause; otherwise a body read only up to the cap is
    // `content_over_cap`, never reported as clean.
    const degradedReason = decision.degraded
      ? degradedDetail(decision)
      : body.truncated_reason
        ? DETAIL.CONTENT_OVER_CAP
        : undefined;

    const observation = decorate(
      observationBody({
        client_id: nextClientId(),
        route: ROUTE.EXT_WEB_REQUEST,
        tool_fingerprint: fingerprint.fingerprint,
        occurred_at: new Date(clock()).toISOString(),
        monotonic_offset_ms: Math.max(0, Math.round(monotonic())),
        size_bytes: body.size,
        has_content: Boolean(gate.bytes && modeReadsContent(mode)),
        content: contentBytes,
        content_digest: contentDigest,
        content_is_binary: contentIsBinary,
        over_cap: Boolean(body.truncated_reason),
        decision: decision.decision,
        degraded_reason: degradedReason,
        attachments: [],
      }),
      {
        mode,
        gate,
        fingerprint,
        shape,
        host,
        unread_reason: gate.unread_reason,
        decision,
        request,
        detail,
      },
    );
    // A blocked request is still an event. The request is released (or cancelled) without waiting
    // for the files: `emitted` settles once the observation is queued.
    const emitted =
      attachments && modeReadsContent(mode) && Number.isInteger(detail.tabId) && detail.tabId >= 0
        ? attachThenEmit(observation, detail.tabId)
        : Promise.resolve(emit(observation));

    return {
      cancel: Boolean(decision.cancel),
      queued: true,
      skipped: false,
      observation,
      mode,
      candidate: observation,
      decision,
      emitted,
    };
  }

  /**
   * Observe one request on the metadata lane (no `requestBody`): a destination the policy resolves
   * to M0, or any request the body filter excluded. The observation carries identity, volume,
   * time and the decision, never content.
   */
  async function captureMetadata({ detail, tab_context = [] }) {
    counters.inc(COUNTER.OBSERVED);
    const url = detail.url;
    const host = hostOf(url);
    const shape = predicateRequest(shapeInput(detail, null, tab_context));

    const isSocket = detail.type === 'websocket' || /^wss?:/i.test(url);
    if (isSocket) return captureHandshake({ detail, host, tab_context, shape });

    // Without a body, "match" cannot be decided; the metadata subset of the evidence decides
    // whether an identity-and-volume observation is worth emitting.
    if (!shape.metadata_candidate) {
      counters.inc(COUNTER.SKIPPED_NOT_GENERATIVE);
      return { cancel: false, queued: false, skipped: true, observation: null, mode: MODE.M0, candidate: null };
    }

    const fingerprint = await computeToolFingerprint(adapter, shapeInput(detail, null, tab_context));
    const gate = readBodyForMode(policy.modeFor({ host, tool_fingerprint: fingerprint.fingerprint }), null);
    const decision = await decide({
      url,
      host,
      tool_fingerprint: fingerprint.fingerprint,
      tab_id: detail.tabId,
      request_id: detail.requestId,
    });

    const observation = decorate(
      observationBody({
        client_id: nextClientId(),
        route: ROUTE.EXT_WEB_REQUEST,
        tool_fingerprint: fingerprint.fingerprint,
        occurred_at: new Date(clock()).toISOString(),
        monotonic_offset_ms: Math.max(0, Math.round(monotonic())),
        size_bytes: 0,
        has_content: false,
        decision: decision.decision,
      }),
      {
        mode: gate.mode,
        gate,
        fingerprint,
        shape,
        host,
        unread_reason: 'mode_forbids_read',
        decision,
        request: shapeInput(detail, null, tab_context),
        detail,
      },
    );
    emit(observation);
    return { cancel: Boolean(decision.cancel), queued: true, skipped: false, observation, mode: gate.mode, candidate: observation, decision };
  }

  /** A WebSocket handshake is observed; the frames after it are not visible to webRequest. */
  async function captureHandshake({ detail, host, tab_context, shape }) {
    const fingerprint = await computeToolFingerprint(adapter, shapeInput(detail, null, tab_context));
    const mode = policy.modeFor({ host, tool_fingerprint: fingerprint.fingerprint }).mode;
    const decision = await decide({
      url: detail.url,
      host,
      tool_fingerprint: fingerprint.fingerprint,
      tab_id: detail.tabId,
      request_id: detail.requestId,
    });
    const observation = decorate(
      observationBody({
        client_id: nextClientId(),
        route: ROUTE.EXT_WEB_REQUEST,
        tool_fingerprint: fingerprint.fingerprint,
        occurred_at: new Date(clock()).toISOString(),
        monotonic_offset_ms: Math.max(0, Math.round(monotonic())),
        size_bytes: 0,
        has_content: false,
        decision: decision.decision,
      }),
      {
        mode,
        gate: { mode, resolution: { reason: 'websocket_handshake' }, bytes: null, unread_reason: 'websocket_frames_not_observable' },
        fingerprint,
        shape,
        host,
        unread_reason: 'websocket_frames_not_observable',
        decision,
        request: shapeInput(detail, null, tab_context),
        detail,
      },
    );
    // Identity and volume only, never content.
    observation.websocket = true;
    observation.handshake_only = true;
    observation.identity_volume_only = true;
    observation.upgrade = String((detail.requestHeaders || {}).upgrade || 'websocket');
    emit(observation);
    return { cancel: Boolean(decision.cancel), queued: true, skipped: false, observation, mode, candidate: observation, decision };
  }

  /**
   * A response for a request held as a candidate. The response contract is what separates a draft
   * save from a chat call on the same origin, and it exists only after the request has gone.
   *
   * A positive verdict emits a second observation on `ext.page_context`, the higher-fidelity route,
   * carrying the same digest so ingest's dedup collapses the pair rather than double counting it.
   */
  async function onResponse({ detail }) {
    const entry = candidates.get(detail.requestId);
    if (!entry) return { matched: false, reason: 'not_a_candidate' };
    candidates.delete(detail.requestId);

    const request = entry.request;
    // The body is not available on the response event: re-run the predicate over the retained
    // shape evidence plus the response contract.
    const verdict = classifyWithResponse({ ...request, size_bytes: entry.size_bytes }, {
      status: detail.statusCode,
      headers: detail.responseHeaders || {},
    });
    if (!verdict.match) return { matched: false, reason: verdict.reason, score: verdict.score };

    // The follow-up carries the request's own content decision, so both observations of one
    // submission agree on `has_content` and `content_digest`. At M0 there is nothing to carry, so a
    // response contract can never become a way to read content.
    const carriesContent = Boolean(entry.content_digest) && entry.has_content;
    const followUp = decorate(
      observationBody({
        client_id: entry.client_id || 'pending',
        route: ROUTE.EXT_PAGE_CONTEXT,
        tool_fingerprint: entry.tool_fingerprint || 'tf1:unread',
        occurred_at: new Date(clock()).toISOString(),
        monotonic_offset_ms: Math.max(0, Math.round(monotonic())),
        size_bytes: entry.size_bytes,
        has_content: carriesContent,
        content: carriesContent ? entry.content : undefined,
        content_digest: carriesContent ? entry.content_digest : undefined,
        decision: entry.decision || { rule_id: DEFAULT_RULE_ID, action: DECISION.LOGGED, decided_locally: true },
      }),
      {
        mode: entry.mode,
        gate: { mode: entry.mode, resolution: { reason: 'response_contract' }, bytes: null, unread_reason: null },
        fingerprint: { fingerprint: entry.tool_fingerprint || 'tf1:unread', vector: entry.vector || {}, canonical: '' },
        shape: entry.shape,
        host: entry.host,
        unread_reason: null,
        decision: entry.decision,
        request,
        detail,
      },
    );
    followUp.response_contract = verdict.signals.filter((s) => s.id.startsWith('response_')).map((s) => s.id);
    followUp.upgrade_reason = verdict.reason;
    if (entry.client_id) followUp.supersedes = entry.client_id;
    emit(followUp);
    return { matched: true, reason: verdict.reason, score: verdict.score, observation: followUp };
  }

  function remember(observation, { size_bytes, content, content_digest, decision, request, shape, vector, mode, host, client_id, tool_fingerprint }) {
    candidates.set(observation.request_id, {
      client_id,
      tool_fingerprint,
      vector,
      shape,
      request,
      size_bytes,
      content,
      content_digest,
      has_content: Boolean(observation.has_content),
      decision,
      mode,
      host,
      at: clock(),
    });
    while (candidates.size > MAX_CANDIDATES) candidates.delete(candidates.keys().next().value);
  }

  /** Hold a weak candidate for its response contract. Nothing is emitted for it here. */
  function holdCandidate(entry) {
    candidates.set(entry.request_id, { ...entry, at: clock() });
    while (candidates.size > MAX_CANDIDATES) candidates.delete(candidates.keys().next().value);
  }

  function decorate(observation, ctx) {
    const { mode, gate, fingerprint, shape, host, unread_reason, decision, request, detail } = ctx;
    observation.mode_resolved = mode;
    observation.mode_reason = gate.resolution ? gate.resolution.reason : '';
    observation.policy_version = gate.resolution ? gate.resolution.policy_version : null;
    observation.unread_reason = unread_reason;
    observation.host = host;
    observation.path_shape = fingerprint.vector.path_shape;
    observation.body_shape = shape.structure;
    observation.matched_evidence = shape.signals.filter((s) => s.weight > 0).map((s) => s.id);
    observation.decision_reason = decision ? decision.reason : '';
    observation.decision_confirmed = decision && decision.confirmed !== undefined ? decision.confirmed : null;
    observation.request_id = detail.requestId;
    observation.monotonic = Math.max(0, Math.round(monotonic()));

    const invalid = validateObservation(observation);
    if (invalid) {
      counters.countError('internal_error');
      throw new ExtError('internal_error', `refusing to emit a malformed observation: ${invalid.message}`);
    }

    // Keep every emitted observation as a candidate too: the response tells which endpoint answered.
    remember(observation, {
      client_id: observation.client_id,
      tool_fingerprint: observation.tool_fingerprint,
      vector: fingerprint.vector,
      shape,
      request,
      size_bytes: observation.size_bytes,
      content: observation.content,
      content_digest: observation.content_digest,
      decision: observation.decision,
      mode,
      host,
    });
    return observation;
  }

  /**
   * The listener blocks only while it is deciding whether to block. The extension classifies
   * nothing, so the rules are evaluated with unknown labels: a rule that lists labels does not
   * match here.
   */
  async function decide(input) {
    const base = {
      bundle: policy.rules(),
      route: ROUTE.EXT_WEB_REQUEST,
      tool_fingerprint: input.tool_fingerprint,
      labels: [],
      labels_known: false,
    };
    const overlay = (sync) => ({
      url: input.url,
      host: input.host,
      path: pathOf(input.url),
      rule_id: sync.rule_id,
      message: sync.message,
      link: sync.link,
      tab_id: input.tab_id,
      timeout_ms: CONFIRMATION_WINDOW_MS,
      request_id: input.request_id,
    });
    const sync = decideSync(base);
    if (sync.reason === REASON.RULE_BLOCK) {
      // The request is cancelled now; the overlay only tells the user why, and a block nobody was
      // told about is counted.
      void Promise.resolve()
        .then(() => adapter.showBlocked(overlay(sync)))
        .then((r) => r && r.shown)
        .catch(() => false)
        .then((shown) => shown || counters.countError('block_notice_unavailable'));
      return sync;
    }
    if (!sync.needs_confirmation) return sync;

    // A `warned` verdict is put to the user before the request proceeds. Rule evaluation stays
    // inside the decision budget; the wait for the human is bounded by the confirmation window.
    const confirmed = await decideWithConfirmation({
      ...base,
      confirm: (decision) => adapter.warnUser(overlay(decision)),
      confirmationWindowMs: CONFIRMATION_WINDOW_MS,
    });
    if (confirmed.degraded) counters.countError(confirmed.reason === 'budget_exceeded' ? 'budget_exceeded' : 'warn_unavailable');
    return confirmed;
  }

  /**
   * Transfer the sending tab's files to capture-core, then emit the observation with their
   * descriptors. The observation is emitted whatever happens to the files.
   */
  async function attachThenEmit(observation, tabId) {
    try {
      const found = await attachments.collect({ tabId, observationId: observation.client_id });
      if (found.attachments.length) observation.attachments = found.attachments;
      if (found.reachable) observation.page_context_attachments = true;
      if (validateObservation(observation)) {
        counters.countError('internal_error');
        delete observation.attachments;
      }
    } catch {
      counters.countError('internal_error');
    }
    return emit(observation);
  }

  /**
   * Emit one observation: always enqueue, then drain. There is no direct send, because a port can
   * look connected until the browser delivers the disconnect for a missing host, and a message
   * posted in that window is lost. An entry leaves the queue only when capture-core acks it, so
   * `emitted` means "accepted by capture-core".
   */
  function emit(observation) {
    const payload = protocolBody(observation);
    queue.enqueue(TYPE.OBSERVATION, payload, JSON.stringify(payload).length);
    scheduleDrain();
    return { queued: true, depth: queue.size() };
  }

  /** Exactly the fields ObservationMessage declares, and nothing extension-local. */
  function protocolBody(observation) {
    const body = {
      client_id: observation.client_id,
      route: observation.route,
      tool_fingerprint: observation.tool_fingerprint,
      occurred_at: observation.occurred_at,
      monotonic_offset_ms: observation.monotonic_offset_ms,
      size_bytes: observation.size_bytes,
      has_content: observation.has_content,
    };
    if (observation.content_digest) body.content_digest = observation.content_digest;
    // `observation.content` is already the base64 wire form (observationBody encoded it).
    if (observation.has_content && observation.content) body.content = observation.content;
    if (observation.content_is_binary) body.content_is_binary = true;
    if (observation.over_cap) body.over_cap = true;
    if (observation.attachments && observation.attachments.length) body.attachments = observation.attachments;
    if (observation.decision) body.decision = observation.decision;
    if (observation.degraded_reason) body.degraded_reason = observation.degraded_reason;
    if (observation.page_context_attachments) body.page_context_attachments = true;
    return body;
  }

  /**
   * Deliver what is queued, one drain at a time whichever caller asks: two concurrent drains would
   * read the same head entry before either acks it and send it twice. A caller arriving mid-drain
   * joins it and asks for another pass, so a burst coalesces and every entry still leaves.
   */
  function drainQueue(max = DRAIN_BATCH) {
    if (drainPromise) {
      drainAgain = true;
      return drainPromise;
    }
    drainPromise = (async () => {
      try {
        const result = await native.drain(queue, max);
        if (result.failures.length) {
          const code = result.failures[0].code;
          // A refusal while cooling down is the original failure still standing, already counted
          // once; counting it per attempt would make the error counter climb while nothing changes.
          if (code !== COOLDOWN_ERROR_CODE) {
            counters.countError(code);
            health.onChannelAbsent();
          }
        } else if (result.sent > 0) {
          // `emitted` counts acks: accepted by capture-core, the most the extension can observe.
          health.markEmitted(result.sent);
        }
        return result;
      } finally {
        drainPromise = null;
        if (drainAgain) {
          drainAgain = false;
          scheduleDrain();
        }
      }
    })();
    return drainPromise;
  }

  /** Kick a drain without waiting for it. */
  function scheduleDrain() {
    void Promise.resolve()
      .then(() => drainQueue(DRAIN_BATCH))
      .catch(() => {
        /* drainQueue counts and reports its own failures; nothing may escape a listener */
      });
  }

  return {
    captureWithBody,
    captureMetadata,
    onResponse,
    drainQueue,
  };
}

function hostOf(url) {
  try {
    return new URL(url).host.toLowerCase();
  } catch {
    return '';
  }
}

function pathOf(url) {
  try {
    return new URL(url).pathname;
  } catch {
    return '';
  }
}

function formTextOf(body) {
  return body.form ? Object.entries(body.form).map(([k, v]) => `${k}=${v.join(',')}`).join('&') : '';
}
