/**
 * attachments/sender.js — §7.3's chunked transfer, manifest first, driven from the **content
 * script** because that is where the `File` object lives.
 *
 * The order is the point. The manifest goes to `capture-core` — relayed through the service
 * worker, which owns the native channel — **before any byte**. §3.4: "attachment bytes are
 * chunked behind a manifest ... that lets `capture-core` refuse an oversized upload *before*
 * transfer". So the interesting assertion is not that a refusal is handled; it is that, on a
 * refusal, `readSlice` is never called at all.
 *
 * The other two properties §7.3 fixes:
 *   - **snapshot-on-send, never read-through**: `files.js` holds the page's `File` reference and
 *     this module reads it in chunks, hashing as it goes. Nothing is written to extension storage.
 *   - **a failed attachment read never fails the submission**: every failure path returns a
 *     result. None of them throws past this boundary, so the prompt text is still captured.
 */

import { ExtError, errorCode } from '../adapter.js';
import { sha256Prefixed } from '../codec.js';
import { CORE_TYPE, MAX_ATTACHMENT_BYTES, TYPE } from '../messages.js';

/** Chunks are zero-based and contiguous; a gap is refused rather than assembled (native.go). */
export const DEFAULT_CHUNK_BYTES = 256 * 1024;

/**
 * @param {object} opts
 * @param {{sendMessage: (m: any) => Promise<any>}} opts.bridge  the content-script -> worker relay
 * @param {Crypto} opts.crypto
 * @param {(kind: string, n?: number) => void} [opts.count]
 * @param {number} [opts.chunkBytes]
 * @param {number} [opts.maxAttachmentBytes]
 * @param {() => string[]} [opts.newIds]
 */
export function createAttachmentSender({
  bridge,
  crypto,
  count = () => {},
  chunkBytes = DEFAULT_CHUNK_BYTES,
  maxAttachmentBytes = MAX_ATTACHMENT_BYTES,
  newIds = () => [crypto.randomUUID ? crypto.randomUUID() : String(Date.now())],
}) {
  /**
   * @param {object} input
   * @param {string} input.observation_id
   * @param {{ref_id: string, name: string, media_type: string, size_bytes: number}} input.descriptor
   * @param {(refId: string, offset: number, length: number) => Promise<{ok: boolean, bytes?: Uint8Array, code?: string, message?: string}>} input.readSlice
   * @param {() => boolean} [input.isCancelled]
   * @returns {Promise<{status: string, reason: string, chunks: number, bytes_sent: number, digest: string|null, descriptor: object, message?: string}>}
   */
  async function send({ observation_id, descriptor, readSlice, isCancelled = () => false }) {
    const transfer_id = newIds()[0];
    const desc = { ...descriptor };
    const declared = Number.isFinite(desc.size_bytes) ? desc.size_bytes : 0;

    // Local pre-check against the transport ceiling. `capture-core` is the authority — the
    // effective cap is bundle policy and may be tighter — but refusing here means no manifest and
    // no byte for a file that cannot possibly be accepted.
    if (declared > maxAttachmentBytes) {
      count('attachment_refused_locally', 1);
      return {
        status: 'refused',
        reason: 'attachment_too_large',
        chunks: 0,
        bytes_sent: 0,
        digest: null,
        descriptor: desc,
        message: `${desc.name} is ${declared} bytes, transport ceiling is ${maxAttachmentBytes}`,
      };
    }

    // 1. Manifest first.
    let began;
    try {
      began = await relay(TYPE.ATTACHMENT_MANIFEST, { transfer_id, observation_id, descriptor: { ...desc, digest: undefined } });
    } catch (e) {
      count('attachment_channel_failed', 1);
      return failure(errorCode(e), desc, String((e && e.message) || e));
    }

    // 2. The answer decides whether any byte moves. A refusal is typed and closed.
    if (began.type === CORE_TYPE.REFUSAL) {
      count('attachment_refused', 1);
      return {
        status: 'refused',
        reason: (began.body && began.body.reason) || 'malformed',
        chunks: 0,
        bytes_sent: 0,
        digest: null,
        descriptor: desc,
        message: (began.body && began.body.message) || 'refused',
      };
    }
    const answer = began.body || {};
    const capacityBytes = Number.isFinite(answer.capacity_bytes) ? answer.capacity_bytes : declared;
    const effectiveChunk = Number.isFinite(answer.chunk_bytes) && answer.chunk_bytes > 0 ? answer.chunk_bytes : chunkBytes;
    if (declared > capacityBytes) {
      count('attachment_refused', 1);
      return {
        status: 'refused',
        reason: 'attachment_too_large',
        chunks: 0,
        bytes_sent: 0,
        digest: null,
        descriptor: desc,
        message: `${desc.name} is ${declared} bytes, effective cap is ${capacityBytes}`,
      };
    }

    // 3. Chunks, zero-based and contiguous, hashed as they are read so the descriptor describes
    //    what was actually sent rather than what was selected.
    const hash = createChunkedHasher(crypto);
    let offset = 0;
    let seq = 0;
    try {
      while (offset < declared) {
        if (isCancelled()) {
          await safeComplete(transfer_id, seq, 'cancelled');
          count('attachment_cancelled', 1);
          return failure('cancelled', desc, 'transfer cancelled', seq, offset);
        }
        const want = Math.min(effectiveChunk, declared - offset);
        const r = await readSlice(desc.ref_id, offset, want);
        if (!r || !r.ok) {
          // §7.3: a failed attachment read never fails the submission — it is counted and the
          // prompt text is still captured.
          await safeComplete(transfer_id, seq, `${(r && r.code) || 'read_failed'}: ${(r && r.message) || ''}`);
          count('attachment_read_failed', 1);
          return failure('attachment_read_failed', desc, (r && r.message) || 'read failed', seq, offset);
        }
        const bytes = r.bytes;
        if (!bytes || bytes.byteLength === 0) {
          await safeComplete(transfer_id, seq, 'short_read');
          count('attachment_read_failed', 1);
          return failure('attachment_read_failed', desc, 'short read', seq, offset);
        }
        hash.update(bytes);
        await relay(TYPE.ATTACHMENT_CHUNK, { transfer_id, seq, data: bytesToBase64(bytes) });
        offset += bytes.byteLength;
        seq += 1;
      }
    } catch (e) {
      count('attachment_channel_failed', 1);
      await safeComplete(transfer_id, seq, String((e && e.message) || e));
      return failure(errorCode(e), desc, String((e && e.message) || e), seq, offset);
    }

    // 4. The digest is over exactly the bytes sent.
    let digest = null;
    try {
      digest = await hash.digest();
    } catch (e) {
      count('attachment_digest_failed', 1);
    }
    await safeComplete(transfer_id, seq, '');
    count('attachment_sent', 1);
    return {
      status: 'sent',
      reason: 'ok',
      chunks: seq,
      bytes_sent: offset,
      digest,
      descriptor: digest ? { ...desc, digest } : desc,
    };
  }

  async function relay(frame_type, body) {
    const r = await bridge.sendMessage({ type: 'capture_attachment_frame', frame_type, body });
    if (!r || r.ok !== true) throw new ExtError('native_unavailable', (r && r.error) || 'relay failed');
    return r.answer || { type: 'ack', body: {} };
  }

  async function safeComplete(transfer_id, chunks, error) {
    try {
      await relay(TYPE.ATTACHMENT_COMPLETE, { transfer_id, chunks, error: error || '' });
    } catch (e) {
      // A failed completion must not mask the transfer's own outcome.
    }
  }

  function failure(reason, desc, message, chunks = 0, bytes_sent = 0) {
    return { status: 'failed', reason, chunks, bytes_sent, digest: null, descriptor: desc, message };
  }

  return { send, chunkBytes, maxAttachmentBytes };
}

/** `encoding/json` marshals []byte as base64, so the frame carries base64 too. */
export function bytesToBase64(bytes) {
  const table = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/';
  let out = '';
  for (let i = 0; i < bytes.byteLength; i += 3) {
    const n = (bytes[i] << 16) | ((bytes[i + 1] || 0) << 8) | (bytes[i + 2] || 0);
    out += table[(n >> 18) & 63] + table[(n >> 12) & 63] +
      (i + 1 < bytes.byteLength ? table[(n >> 6) & 63] : '=') +
      (i + 2 < bytes.byteLength ? table[n & 63] : '=');
  }
  return out;
}

/**
 * A streaming SHA-256 is not available in WebCrypto, so the hasher retains the chunks it read.
 * For the sizes §7.3 deals in that is bounded by the bundle's attachment cap, and the alternative
 * — a hand-written SHA-256 — would be a second implementation of a primitive the device already
 * has. The cap is enforced before the first byte is read, so this cannot grow without bound.
 */
export function createChunkedHasher(crypto) {
  const parts = [];
  let total = 0;
  return {
    update(bytes) {
      parts.push(bytes);
      total += bytes.byteLength;
    },
    get bytes() {
      return total;
    },
    async digest() {
      const all = new Uint8Array(total);
      let o = 0;
      for (const p of parts) {
        all.set(p, o);
        o += p.byteLength;
      }
      return sha256Prefixed(crypto, all);
    },
  };
}
