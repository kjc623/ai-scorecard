/**
 * attachments/sender.js — the chunked attachment transfer, driven from the content script because
 * that is where the `File` object lives.
 *
 * The manifest goes to capture-core (relayed by the service worker, which owns the native channel)
 * before any byte, so an oversized upload is refused before a single byte is read. The file is then
 * read in chunks and hashed as it goes; nothing is written to extension storage. Every failure path
 * returns a result rather than throwing, so a failed attachment never fails the submission.
 */

import { ExtError, errorCode } from '../adapter.js';
import { bytesToBase64, sha256Prefixed } from '../codec.js';
import { CORE_TYPE, MAX_ATTACHMENT_BYTES, TYPE } from '../messages.js';

/** Chunks are zero-based and contiguous; capture-core refuses a gap rather than assembling it. */
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
    // The manifest carries the protocol's descriptor fields; the page-side reference stays here.
    const wire = { name: desc.name, size_bytes: declared, ...(desc.media_type ? { media_type: desc.media_type } : {}) };

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
      began = await relay(TYPE.ATTACHMENT_MANIFEST, { transfer_id, observation_id, descriptor: wire });
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
          // A failed read is counted; the submission itself is unaffected.
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
      descriptor: digest ? { ...wire, content_digest: digest } : wire,
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

  return { send };
}

/**
 * WebCrypto has no streaming SHA-256, so the hasher retains the chunks it read. That is bounded by
 * the attachment cap, which is enforced before the first byte is read.
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
