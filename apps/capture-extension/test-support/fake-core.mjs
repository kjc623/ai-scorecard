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
import { sha256Prefixed } from '../src/codec.js';
import { webcrypto } from 'node:crypto';

const CRYPTO = { subtle: webcrypto.subtle };

/**
 * Base64 exactly as `encoding/json` reads a `[]byte`: the input must be well-formed base64, and
 * anything else is an error rather than something to reinterpret. Returns null when it is not.
 */
function decodeBase64Strict(s) {
  if (typeof s !== 'string' || s.length === 0) return null;
  if (!/^[A-Za-z0-9+/]*={0,2}$/.test(s)) return null;
  if (s.length % 4 !== 0) return null;
  const buf = Buffer.from(s, 'base64');
  // Round-trip guard: Buffer.from is lenient, so a payload that re-encodes differently was not
  // valid base64 in the first place.
  if (buf.toString('base64') !== s) return null;
  return buf;
}

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

  async function handle(message) {
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

        // **The behaviour that matters most.** `ObservationMessage.Content` is `[]byte` with
        // `json:"content"`, so `encoding/json` base64-decodes the wire field. A raw-text `content`
        // therefore fails to decode — and text that happens to BE valid base64 silently decodes to
        // different bytes than the user typed, while `content_digest` was taken over the original.
        // This fake reproduces both, so the content-identity break cannot come back unnoticed.
        let decoded = null;
        if (typeof body.content === 'string' && body.content.length > 0) {
          decoded = decodeBase64Strict(body.content);
          if (decoded === null) {
            return reply(CORE_TYPE.REFUSAL, id, {
              reason: REFUSAL.MALFORMED,
              message: `illegal base64 data in content: ${JSON.stringify(body.content.slice(0, 24))}...`,
            });
          }
          if (body.content_digest) {
            // Go's consumer computes its own digest over the decoded bytes. If the two disagree,
            // the record's digest does not describe its content — reject it here.
            const computed = await sha256Prefixed(CRYPTO, new Uint8Array(decoded));
            if (computed !== body.content_digest) {
              return reply(CORE_TYPE.REFUSAL, id, {
                reason: REFUSAL.MALFORMED,
                message: `content_digest ${body.content_digest} does not match the digest of the decoded content ${computed}`,
              });
            }
          }
        }
        emitted.push({ type, body, decoded_content: decoded ? new Uint8Array(decoded) : null });
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
    /** The bytes `encoding/json` would have decoded `content` into, for the last observation. */
    lastDecodedContent() {
      const obs = emitted.filter((e) => e.type === 'observation');
      return obs.length ? obs[obs.length - 1].decoded_content : null;
    },
    /** The digest the consumer would compute over the decoded bytes. */
    async lastComputedDigest() {
      const bytes = this.lastDecodedContent();
      if (!bytes) return null;
      return sha256Prefixed(CRYPTO, bytes);
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
