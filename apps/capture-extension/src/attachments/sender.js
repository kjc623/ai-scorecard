/**
 * attachments/sender.js — §7.3's chunked transfer, manifest first.
 *
 * The order is the point: the **manifest goes to `capture-core` before any byte**, so an
 * oversized upload is refused before transfer (§3.4: "a browser-enforced message size ceiling,
 * so attachment bytes are chunked behind a manifest ... that lets capture-core refuse an
 * oversized upload *before* transfer, with the cap taken from the bundle's mode cap"). A refusal
 * is a normal outcome, not an exception: it is returned with a reason from native.go's closed
 * set and counted.
 *
 * Second point: **a failed attachment read never fails the submission.** Every failure path
 * returns a result; none of them propagates to the observation. The prompt text is captured
 * regardless, and the failure is reported as `attachment_read_failed` — the case §7.3 says is
 * "counted and reported" rather than fatal.
 *
 * Third point: the digest in the descriptor is computed from the bytes as they are read, so a
 * file the user edited between selection and send is described by what was actually sent.
 */

import { ExtError, errorCode } from '../adapter.js';
import { sha256Prefixed } from '../codec.js';
import { CORE_TYPE, TYPE } from '../messages.js';

/** native.go's struct is zero-based and contiguous; a gap is refused rather than assembled. */
export const DEFAULT_CHUNK_BYTES = 256 * 1024;
/** protocol.MaxAttachmentBytes, the transport ceiling; the effective cap is bundle policy. */
export const MAX_ATTACHMENT_BYTES = 64 << 20;

/**
 * @param {object} opts
 * @param {ReturnType<import('../native.js').createNativeClient>} opts.native
 * @param {import('../adapter.js').Adapter} opts.adapter
 * @param {(counter: string, n?: number) => void} [opts.count]
 * @param {number} [opts.chunkBytes]
 * @param {number} [opts.maxAttachmentBytes]
 */
export function createAttachmentSender({ native, adapter, count = () => {}, chunkBytes = DEFAULT_CHUNK_BYTES, maxAttachmentBytes = MAX_ATTACHMENT_BYTES }) {
  /**
   * Send one attachment. `readSlice(ref_id, offset, length)` is supplied by the content script
   * (`files.js`), which is the only place the `File` object lives.
   *
   * @returns {Promise<{status: 'sent'|'refused'|'failed'|'too_large_locally',
   *                    reason: string, chunks: number, bytes_sent: number, digest: string|null,
   *                    descriptor: object, message?: string}>}
   */
  async function send({ observation_id, descriptor, transfer_id, readSlice, isCancelled = () => false }) {
    const desc = { ...descriptor };
    const declared = Number.isFinite(desc.size_bytes) ? desc.size_bytes : 0;

    // Local pre-check against the transport ceiling. `capture-core` is the authority (the
    // effective cap is bundle policy and may be smaller), but refusing here means no manifest
    // and no byte for a file that cannot possibly be accepted.
    if (declared > maxAttachmentBytes) {
      count('attachment_refused_locally', 1);
      return {
        status: 'too_large_locally',
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
      began = await native.sendRequest(TYPE.ATTACHMENT_MANIFEST, {
        transfer_id,
        observation_id,
        descriptor: desc,
      });
    } catch (e) {
      count('attachment_channel_failed', 1);
      return {
        status: 'failed',
        reason: errorCode(e),
        chunks: 0,
        bytes_sent: 0,
        digest: null,
        descriptor: desc,
        message: String((e && e.message) || e),
      };
    }

    // 2. The answer decides whether any byte moves. A refusal is typed and closed.
    const answer = began.body || {};
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
    const capacityBytes = Number.isFinite(answer.capacity_bytes) ? answer.capacity_bytes : declared;
    const effectiveChunk = Number.isFinite(answer.chunk_bytes) && answer.chunk_bytes > 0 ? answer.chunk_bytes : chunkBytes;
    if (declared > capacityBytes) {
      // The core's mode cap is tighter than the transport ceiling: refuse before transfer.
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

    // 3. Chunks, zero-based and contiguous.
    const digestInput = [];
    let offset = 0;
    let seq = 0;
    try {
      while (offset < declared) {
        if (isCancelled()) {
          await complete(transfer_id, seq, 'cancelled');
          count('attachment_cancelled', 1);
          return { status: 'failed', reason: 'cancelled', chunks: seq, bytes_sent: offset, digest: null, descriptor: desc, message: 'transfer cancelled' };
        }
        const want = Math.min(effectiveChunk, declared - offset);
        const r = await readSlice(desc.ref_id, offset, want);
        if (!r.ok) {
          // §7.3: a failed attachment read never fails the submission.
          await complete(transfer_id, seq, `${r.code}: ${r.message}`);
          count('attachment_read_failed', 1);
          return {
            status: 'failed',
            reason: 'attachment_read_failed',
            chunks: seq,
            bytes_sent: offset,
            digest: null,
            descriptor: desc,
            message: r.message,
          };
        }
        const bytes = r.bytes;
        if (bytes.byteLength === 0) {
          // A short read would leave a gap; native.go refuses a gap rather than assembling it.
          await complete(transfer_id, seq, 'short_read');
          count('attachment_read_failed', 1);
          return { status: 'failed', reason: 'attachment_read_failed', chunks: seq, bytes_sent: offset, digest: null, descriptor: desc, message: 'short read' };
        }
        digestInput.push(bytes);
        await native.sendRequest(TYPE.ATTACHMENT_CHUNK, {
          transfer_id,
          seq,
          data: toBase64(bytes),
        });
        offset += bytes.byteLength;
        seq++;
      }
    } catch (e) {
      count('attachment_channel_failed', 1);
      await complete(transfer_id, seq, String((e && e.message) || e));
      return {
        status: 'failed',
        reason: errorCode(e),
        chunks: seq,
        bytes_sent: offset,
        digest: null,
        descriptor: desc,
        message: String((e && e.message) || e),
      };
    }

    // 4. The digest is over exactly the bytes sent.
    let digest = null;
    try {
      digest = await sha256Prefixed(adapter.crypto, join(digestInput));
    } catch (e) {
      count('attachment_digest_failed', 1);
    }
    await complete(transfer_id, seq, '');
    count('attachment_sent', 1);
    return { status: 'sent', reason: 'ok', chunks: seq, bytes_sent: offset, digest, descriptor: { ...desc, digest: digest || undefined } };
  }

  async function complete(transfer_id, chunks, error) {
    try {
      await native.sendRequest(TYPE.ATTACHMENT_COMPLETE, { transfer_id, chunks, error: error || '' });
    } catch (e) {
      if (!(e instanceof ExtError)) throw e;
      // A failed completion is counted by the caller through the returned status; it must not
      // mask the original outcome of the transfer.
    }
  }

  return { send, chunkBytes, maxAttachmentBytes };
}

/** `encoding/json` marshals []byte as base64, so the frame carries base64 too. */
export function toBase64(bytes) {
  let binary = '';
  for (let i = 0; i < bytes.byteLength; i++) binary += String.fromCharCode(bytes[i]);
  return globalThis.btoa ? globalThis.btoa(binary) : Buffer.from(bytes).toString('base64');
}

function join(parts) {
  const total = parts.reduce((n, p) => n + p.byteLength, 0);
  const out = new Uint8Array(total);
  let o = 0;
  for (const p of parts) {
    out.set(p, o);
    o += p.byteLength;
  }
  return out;
}
