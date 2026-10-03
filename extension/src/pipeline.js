/**
 * pipeline.js — the orchestrator: one observed request in, zero or one queued observation out,
 * plus one decision about whether the request may proceed.
 *
 * §7.1's three layers, in the order the document fixes them:
 *
 *   1. **observe** what the browser tells us — on the metadata lane there is no body at all,
 *      because the mode gate lives in `registration.js` one layer up;
 *   2. **classify** with §8.2 over shape, size and structure, *before* any content question is
 *      asked (§7.3);
 *   3. **emit** only a positive match, and only after the effective mode has decided what may be
 *      read (§11.2 steps 2 and 3).
 *
 * The M0 rule is structural here, not a matter of care: `readBodyForMode()` is the only door to
 * the bytes and it returns `bytes: null` plus `unread_reason` for M0, so no path exists on which
 * an M0 observation carries a digest, a label set or an attachment descriptor. The test for it
 * asserts on the *observations the pipeline made*, not merely on the message that came out.
 *
 * Classification is deferred (§7.2): the inline decision is the only work permitted to delay a
 * request, and everything else — digesting, fingerprinting, sending — happens after the request
 * has been released.
 */

import { ExtError, errorCode } from './adapter.js';
import { decideSync, decideWithConfirmation, degradedDetail } from './enforce.js';
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

/** Bounded: a service worker can be killed at any time, and a candidate that is never answered must not accumulate. */
export const MAX_CANDIDATES = 256;

/** How many queued observations one drain pass attempts. */
export const DRAIN_BATCH = 20;

export function createPipeline({
  adapter,
  policy,
  native,
  queue,
  health,
  onEmit = null,
  clock = () => Date.now(),
  monotonic = () => (globalThis.performance ? globalThis.performance.now() : Date.now()),
}) {
  const counters = health.counters;
  let sequence = 0;
  /** @type {Map<string, object>} requestId -> candidate awaiting a response contract (§7.5 Mode B) */
  const candidates = new Map();
  /** One drain at a time; a burst coalesces instead of stacking overlapping sends. */
  let drainPromise = null;
  let drainAgain = false;

  function nextClientId() {
    sequence += 1;
    return `c${sequence}`;
  }

  /**
   * The mode gate — the only place in this module that decides whether bytes may be touched.
   * @returns {{mode: string, resolution: object, bytes: Uint8Array|null, unread_reason: string|null}}
   */
  function readBodyForMode(target, body) {
    const resolution = policy.modeFor(target);
    if (!modeReadsContent(resolution.mode)) {
      return { mode: resolution.mode, resolution, bytes: null, unread_reason: 'mode_forbids_read' };
    }
    return { mode: resolution.mode, resolution, bytes: body ? body.bytes : null, unread_reason: null };
  }

  /** The predicate record, shared by the metadata lane and the body lane so both classify identically. */
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
   * Observe one request on the **body lane** — the lane that was registered with `requestBody`
   * and therefore only matches hosts the policy cache does not resolve to M0.
   *
   * @returns {Promise<{cancel: boolean, queued: boolean, skipped: boolean, observation: object|null, mode: string, candidate: object|null}>}
   */
  async function captureWithBody({ detail, body, tab_context = [] }) {
    counters.inc(COUNTER.OBSERVED);
    const url = detail.url;
    const host = hostOf(url);

    // ── 2. shape classification, before the mode is consulted for content (§7.3) ──────────
    const request = shapeInput(detail, body, tab_context);
    const shape = predicateRequest(request);

    // §7.5 Mode B: a request above the candidate floor but below the threshold is *held* — not
    // emitted, because "a negative match is counted, never emitted" (§7.3) — so the response
    // contract can settle it. Without this the conjunction §7.5 describes could never be
    // evaluated at all, because the response arrives after the request has gone.
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
      // §7.3: "A negative match is counted, not emitted."
      counters.inc(COUNTER.SKIPPED_NOT_GENERATIVE);
      return { cancel: false, queued: false, skipped: true, observation: null, mode: MODE.M0, candidate: null };
    }

    // ── 3. the mode decides what may be read, and only then is anything read ─────────────
    const gate = readBodyForMode({ host, size_bytes: body.size }, body);
    const mode = gate.mode;

    const fingerprint = await computeToolFingerprint(adapter, {
      ...request,
      body_read: modeReadsContent(mode),
    });

    const decision = await decide({
      url,
      method: detail.method,
      host,
      tool_fingerprint: fingerprint.fingerprint,
      size_bytes: gate.bytes ? gate.bytes.byteLength : body.size,
      mode,
      tab_id: detail.tabId,
      request_id: detail.requestId,
    });

    let contentDigest;
    let contentBytes;
    let contentIsBinary = false;
    if (gate.bytes && modeReadsContent(mode)) {
      // §7.2: digest over the bytes as sent; a strict decode, or an explicit binary marker. The
      // digest is over the BYTES in both cases, which is why the two paths cannot disagree.
      contentDigest = await sha256Prefixed(adapter.crypto, gate.bytes);
      contentIsBinary = body.decode.encoding === 'binary';
      // `observationBody` base64-encodes this, because the protocol's `Content []byte` is what
      // `encoding/json` base64-decodes. Passing a string here would be the content-identity break.
      contentBytes = gate.bytes;
    }

    // §9.7's "degraded, exactly": a whole-payload read that did not happen is a degraded
    // classification even when the decision path itself was fine. `content_over_cap` is the
    // protocol's own name for it, and it is why an over-cap body is never reported as "clean".
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
    // §7.4: a blocked request is still an event — emit first, then cancel.
    emit(observation);

    return {
      cancel: Boolean(decision.cancel),
      queued: true,
      skipped: false,
      observation,
      mode,
      candidate: observation,
      decision,
    };
  }

  /**
   * Observe one request on the **metadata lane** — the lane without `requestBody`. Two callers:
   * a destination the policy cache resolves to M0 (§11.2), and any request the body lane's filter
   * excluded. On this lane the extension genuinely has not read content, so the observation is an
   * M0 record carrying only identity, volume, time, size and the decision.
   */
  async function captureMetadata({ detail, tab_context = [] }) {
    counters.inc(COUNTER.OBSERVED);
    const url = detail.url;
    const host = hostOf(url);
    const shape = predicateRequest(shapeInput(detail, null, tab_context));

    const isSocket = detail.type === 'websocket' || /^wss?:/i.test(url);
    if (isSocket) return captureHandshake({ detail, host, tab_context, shape });

    // On this lane "match" cannot be decided from the body, so the candidate rule is the
    // metadata subset of §8.2 (method, content type, size, path, destination sets, context).
    // Deliberately lower than the body-lane threshold: what is emitted at M0 is identity and
    // volume, which is the strongest thing this route may honestly report.
    if (!shape.metadata_candidate) {
      counters.inc(COUNTER.SKIPPED_NOT_GENERATIVE);
      return { cancel: false, queued: false, skipped: true, observation: null, mode: MODE.M0, candidate: null };
    }

    const gate = readBodyForMode({ host, size_bytes: 0 }, null);
    const fingerprint = await computeToolFingerprint(adapter, {
      ...shapeInput(detail, null, tab_context),
      body_read: false,
    });
    const decision = decideSync({
      url,
      method: detail.method,
      host,
      tool_fingerprint: fingerprint.fingerprint,
      size_bytes: 0,
      mode: gate.mode,
      rules: policy.rules(),
      release_state: policy.releaseState(),
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

  /** §7.4/E4: the WebSocket handshake is captured; the frames after it are not, and no workaround is attempted. */
  async function captureHandshake({ detail, host, tab_context, shape }) {
    const fingerprint = await computeToolFingerprint(adapter, {
      ...shapeInput(detail, null, tab_context),
      body_read: false,
      forced_method: 'GET',
    });
    const mode = policy.modeFor({ host }).mode;
    const decision = decideSync({
      url: detail.url,
      method: 'GET',
      host,
      tool_fingerprint: fingerprint.fingerprint,
      size_bytes: 0,
      mode,
      rules: policy.rules(),
      release_state: policy.releaseState(),
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
    // Identity and volume only: "no messages, so a submission contributes identity_volume,
    // never content" (§7.4).
    observation.websocket = true;
    observation.handshake_only = true;
    observation.identity_volume_only = true;
    observation.upgrade = String((detail.requestHeaders || {}).upgrade || 'websocket');
    emit(observation);
    return { cancel: Boolean(decision.cancel), queued: true, skipped: false, observation, mode, candidate: observation, decision };
  }

  /**
   * A response record for a request the predicate kept as a weak candidate. §7.5 Mode B: the
   * response contract is what separates a draft save from a chat call on the same origin, and it
   * is available only after the request has gone (so §7.2's "classification afterwards" is not an
   * optimisation here, it is the only possible order).
   *
   * A positive verdict emits a second, higher-fidelity observation on `ext.page_context` — the
   * canonical route per db/schema.sql route_fidelity (10, ahead of web_request's 40) — carrying
   * the same digest, so the ingest-side dedup ladder collapses the pair rather than double
   * counting it.
   */
  async function onResponse({ detail, tab_id, tab_context = [] }) {
    const entry = candidates.get(detail.requestId);
    if (!entry) return { matched: false, reason: 'not_a_candidate' };
    candidates.delete(detail.requestId);

    const request = entry.request;
    // A request body is not available on the response event; re-run the predicate over the
    // retained shape evidence plus the response contract.
    const verdict = classifyWithResponse({ ...request, size_bytes: entry.size_bytes }, {
      status: detail.statusCode,
      headers: detail.responseHeaders || {},
    });
    if (!verdict.match) return { matched: false, reason: verdict.reason, score: verdict.score };

    // Nothing further to read: the request has already been released and its bytes were either
    // read under the mode's permission or never read at all.
    //
    // The follow-up carries the same content decision as the request did, so the two observations
    // of one submission agree on `has_content` and `content_digest`. Where the request lane was
    // not permitted to read at all (M0), there is nothing to carry and the follow-up is metadata
    // too — which is what keeps a response contract from becoming a way to read content at M0.
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
        decision: entry.decision || { rule_id: 'NONE', action: 'logged', decided_locally: true },
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

  /**
   * Hold a request whose score is above the candidate floor but below the threshold, so the
   * response contract can settle it (§7.5 Mode B). Nothing is emitted for it: §7.3's "a negative
   * match is counted, never emitted" is not suspended by the fact that a response may later
   * change the verdict — the counting happens in `captureWithBody` either way.
   */
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

    // Keep the weak candidates so a response contract can upgrade them (§7.5 Mode B); keep
    // strong ones too, because the response is what tells us which endpoint actually answered.
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

  /** §7.2: the listener "must not block unless it is deciding to block". */
  async function decide(input) {
    const base = {
      url: input.url,
      method: input.method,
      tool_fingerprint: input.tool_fingerprint,
      size_bytes: input.size_bytes,
      mode: input.mode,
      rules: policy.rules(),
      release_state: policy.releaseState(),
    };
    const sync = decideSync(base);
    if (!sync.needs_confirmation) return sync;

    // A `warned` verdict is rendered before the request proceeds, and the answer is recorded as
    // part of the decision (§7.4). Rule evaluation stays inside the 300 ms decision budget; the
    // wait for a human is bounded separately (see CONFIRMATION_WINDOW_MS — a decision the
    // document leaves open, reported to the Lead).
    const confirmed = await decideWithConfirmation({
      ...base,
      confirm: () =>
        adapter.warnUser({
          url: input.url,
          host: input.host,
          path: pathOf(input.url),
          rule_id: sync.rule_id,
          message: 'Your organisation\u2019s policy requires confirmation before this request is sent.',
          tab_id: input.tab_id,
          timeout_ms: policy.confirmationWindowMs(),
          request_id: input.request_id,
        }),
      confirmationWindowMs: policy.confirmationWindowMs(),
    });
    if (confirmed.degraded) counters.countError(confirmed.reason === 'budget_exceeded' ? 'budget_exceeded' : 'warn_unavailable');
    return confirmed;
  }

  /**
   * Emit one observation. §7.1: emission is narrow — a negative match never reaches here.
   *
   * **Always enqueue, then deliver.** There is no "send it directly when the channel looks up" path,
   * and that is deliberate. `native.isConnected()` is true from the moment `connectNative()` returns
   * a port until the browser delivers the disconnect for a host that does not exist, and an
   * observation emitted inside that window was posted into a dead port, counted `emitted`, and lost —
   * a silent undercount, which is the one failure mode this whole design exists to make visible.
   *
   * The queue is the only route out. An entry leaves it when `capture-core` acks it (`native.drain`),
   * so `emitted` means "accepted by capture-core" rather than "handed to a port". That is what §3.4
   * describes and what `native.go` says an ack is worth.
   */
  function emit(observation) {
    const payload = protocolBody(observation);
    // §3.4: bounded, in extension memory only, dropped oldest-first with a counter, merged into
    // the next health report when the channel returns.
    queue.enqueue(TYPE.OBSERVATION, payload, JSON.stringify(payload).length);
    if (onEmit) onEmit(observation);
    scheduleDrain();
    return { queued: true, depth: queue.size() };
  }

  /**
   * Deliver what is queued, coalescing a burst into one drain. A drain already running just sets a
   * flag, so N observations produce at most two passes rather than N overlapping ones, and an entry
   * that fails keeps its place for the next attempt.
   */
  function scheduleDrain() {
    if (drainInFlight) {
      drainAgain = true;
      return;
    }
    drainInFlight = true;
    void Promise.resolve()
      .then(() => drainQueue(DRAIN_BATCH))
      .catch(() => {
        /* drainQueue already counts and reports; a rejection here must not escape a listener */
      })
      .finally(() => {
        drainInFlight = false;
        if (drainAgain) {
          drainAgain = false;
          scheduleDrain();
        }
      });
  }

  /** Exactly the fields endpoint/protocol/native.go declares for ObservationMessage, and nothing local. */
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
    // `observation.content` is already the base64 wire form (observationBody encoded it), so this
    // passes it through unchanged. See observationBody() for why the wire form is base64.
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
   * Attach descriptors to an already-emitted observation. §7.3: attachment names are metadata
   * obtainable without reading the file at all, so this is what the lane does when it has a
   * filename and a reachable `File` handle but has not yet read a byte.
   */
  function attachDescriptors(observation, attachments) {
    observation.attachments = attachments;
    if (observation.attachments.length > 0) observation.page_context_attachments = true;
    return observation;
  }

  /**
   * Deliver what is queued. **One drain at a time, whichever caller asks** — the invalidation guard
   * has to live here rather than in `scheduleDrain`, because the service worker also drains directly
   * (on `connected`, and on the health alarm). Two concurrent drains read the same head entry before
   * either acks it and send it twice, which is how this was caught: the fake core received
   * `['a','a','b','b']`.
   *
   * A caller that arrives while a drain is running joins it and marks that another pass is wanted,
   * so a burst coalesces into at most two passes and every entry still leaves.
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
          // A refusal-while-cooling-down is the same failure still standing, not a new one. Counting
          // it per attempt is how a backoff becomes a counter that climbs while nothing changed —
          // the ~23,700-error run the browser gate caught. The original disconnect was counted once.
          if (code !== COOLDOWN_ERROR_CODE) {
            counters.countError(code);
            health.onChannelAbsent();
          }
        } else if (result.sent > 0) {
          // `emitted` counts envelopes capture-core has ACKED. It is not a delivery claim either —
          // the spool is what separates accepted from delivered (native.go on Ack) — but "acked" is
          // the strongest thing the extension can observe, and it is what makes `core: connected`
          // and `emitted` honest rather than optimistic.
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

  /** Kick a drain without waiting for it; a burst coalesces instead of stacking overlapping sends. */
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
    captureHandshake,
    onResponse,
    drainQueue,
    protocolBody,
    attachDescriptors,
    remember,
    candidates,
    get candidateCount() {
      return candidates.size;
    },
    get warnCapable() {
      return typeof adapter.warnUser === 'function';
    },
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
