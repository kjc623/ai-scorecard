/**
 * fake-core.mjs — a stand-in for `capture-core`, implementing the parts of
 * device/protocol/native.go that the extension can observe.
 *
 * It exists so the suite can test the *protocol* half of the extension's behaviour without a
 * browser or a Go process: refusals are typed, an oversized manifest is refused before transfer,
 * and a self-contradicting observation is rejected the way `ObservationMessage.Validate()` rejects
 * it. The refusal strings are copied from native.go's closed set.
 */

import { CORE_TYPE, NATIVE_MESSAGE_VERSION, REFUSAL } from '../src/messages.js';

export function createFakeCore({
  /** protocol.MaxAttachmentBytes is 64 MiB; the effective cap here stands in for the bundle's mode cap. */
  attachmentCapacityBytes = 1 << 20,
  chunkBytes = 64 * 1024,
  /** When true, every manifest is refused as if the destination's mode forbade the read. */
  modeForbidsRead = false,
  policyVersion = '2026.01.0',
  bundle = null,
  /** Answer mode queries with this instead of the bundle. */
  modeAnswer = null,
} = {}) {
  const received = [];
  /** @type {Map<string, {chunks: number[], bytes: number, complete: boolean}>} */
  const transfers = new Map();
  const emitted = [];

  function handle(message) {
    received.push(message);
    const { type, version, id, body } = message;

    if (version !== NATIVE_MESSAGE_VERSION) {
      return reply(CORE_TYPE.REFUSAL, id, { reason: REFUSAL.VERSION_MISMATCH, message: `version ${version}` });
    }

    switch (type) {
      case 'observation': {
        // Mirror of ObservationMessage.Validate().
        if (body.tool_fingerprint === '' || body.tool_fingerprint === undefined) {
          return reply(CORE_TYPE.REFUSAL, id, { reason: REFUSAL.MALFORMED, message: 'observation has no tool fingerprint' });
        }
        if (!body.has_content && body.content && body.content.length > 0) {
          return reply(CORE_TYPE.REFUSAL, id, {
            reason: REFUSAL.MALFORMED,
            message: `observation carries content while has_content is false`,
          });
        }
        if (body.has_content && (!body.content || body.content.length === 0)) {
          return reply(CORE_TYPE.REFUSAL, id, { reason: REFUSAL.MALFORMED, message: 'has_content with no content' });
        }
        for (const a of body.attachments || []) {
          if (!a.name) return reply(CORE_TYPE.REFUSAL, id, { reason: REFUSAL.MALFORMED, message: 'attachment without a name' });
          if (a.size_bytes > 64 * 1024 * 1024) {
            return reply(CORE_TYPE.REFUSAL, id, { reason: REFUSAL.ATTACHMENT_TOO_LARGE, message: 'attachment over cap' });
          }
        }
        emitted.push({ type, body });
        return reply(CORE_TYPE.ACK, id, { detail: 'spooled' });
      }

      case 'attachment_manifest': {
        const { transfer_id, descriptor } = body;
        if (modeForbidsRead) {
          return reply(CORE_TYPE.REFUSAL, id, { reason: REFUSAL.MODE_FORBIDS_READ, message: 'mode forbids read' });
        }
        if (!descriptor || !descriptor.name) {
          return reply(CORE_TYPE.REFUSAL, id, { reason: REFUSAL.MALFORMED, message: 'descriptor without a name' });
        }
        if (descriptor.size_bytes > attachmentCapacityBytes) {
          // native.go: refuse *before* transfer. No byte has moved at this point.
          return reply(CORE_TYPE.REFUSAL, id, {
            reason: REFUSAL.ATTACHMENT_TOO_LARGE,
            message: `${descriptor.name} is ${descriptor.size_bytes} bytes, cap is ${attachmentCapacityBytes}`,
          });
        }
        transfers.set(transfer_id, { chunks: [], bytes: 0, complete: false, descriptor });
        return reply(CORE_TYPE.ACK, id, { transfer_id, capacity_bytes: attachmentCapacityBytes, chunk_bytes: chunkBytes });
      }

      case 'attachment_chunk': {
        const t = transfers.get(body.transfer_id);
        if (!t) return reply(CORE_TYPE.REFUSAL, id, { reason: REFUSAL.MALFORMED, message: 'chunk for an unknown transfer' });
        if (body.seq !== t.chunks.length) {
          // Zero-based and contiguous: a gap is refused rather than assembled.
          return reply(CORE_TYPE.REFUSAL, id, { reason: REFUSAL.MALFORMED, message: `gap: expected seq ${t.chunks.length}, got ${body.seq}` });
        }
        t.chunks.push(body.data);
        t.bytes += byteLengthOfBase64(body.data);
        return reply(CORE_TYPE.ACK, id, { seq: body.seq });
      }

      case 'attachment_complete': {
        const t = transfers.get(body.transfer_id);
        if (!t) return reply(CORE_TYPE.REFUSAL, id, { reason: REFUSAL.MALFORMED, message: 'complete for an unknown transfer' });
        t.complete = true;
        t.error = body.error || '';
        return reply(CORE_TYPE.ACK, id, { transfer_id: body.transfer_id, chunks: t.chunks.length });
      }

      case 'health':
        emitted.push({ type, body });
        return reply(CORE_TYPE.HEALTH_SNAPSHOT, id, { ok: true });

      case 'policy_sync':
        return reply(CORE_TYPE.POLICY_BUNDLE, id, {
          policy_version: policyVersion,
          bundle: bundle,
          unchanged: Boolean(body && body.known_version) && body.known_version === policyVersion,
        });

      case 'mode_query':
        return reply(CORE_TYPE.MODE_ANSWER, id, modeAnswer || { mode: 'm1', policy_version: policyVersion, reason: 'test' });

      case 'decision_record':
        emitted.push({ type, body });
        return reply(CORE_TYPE.ACK, id, { detail: 'recorded' });

      default:
        return reply(CORE_TYPE.REFUSAL, id, { reason: REFUSAL.UNKNOWN_TYPE, message: `unknown type ${type}` });
    }
  }

  function reply(type, id, body) {
    const out = { type, version: NATIVE_MESSAGE_VERSION };
    if (id !== undefined && id !== null && id !== '') out.id = id;
    if (body !== undefined) out.body = body;
    return out;
  }

  return {
    handle,
    received,
    emitted,
    transfers,
    get observationCount() {
      return emitted.filter((e) => e.type === 'observation').length;
    },
    get healthCount() {
      return emitted.filter((e) => e.type === 'health').length;
    },
    lastObservation() {
      const obs = emitted.filter((e) => e.type === 'observation');
      return obs.length ? obs[obs.length - 1].body : null;
    },
    observations() {
      return emitted.filter((e) => e.type === 'observation').map((e) => e.body);
    },
  };
}

function byteLengthOfBase64(b64) {
  const clean = String(b64 || '').replace(/[^A-Za-z0-9+/]/g, '');
  const padding = String(b64 || '').endsWith('==') ? 2 : String(b64 || '').endsWith('=') ? 1 : 0;
  return Math.floor((clean.length * 3) / 4) - padding;
}
