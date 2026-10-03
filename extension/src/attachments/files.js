/**
 * attachments/files.js — the page-context half of §7.3. Runs in the content script's isolated
 * world, which is the only place a `File` object exists.
 *
 * The discipline §7.3 fixes, expressed as the shape of this API:
 *
 *   - **snapshot-on-send, never read-through.** `collectCandidates()` reads *metadata only*
 *     (`name`, `size`, `type`, `lastModified`) and keeps the page's `File` reference. No byte
 *     is read until a transfer is opened, so the extension never holds bytes for files the user
 *     never sends.
 *   - **`File` references are held, not values copied**: the object is reachable only from this
 *     closure, and `forget()` drops it once the transfer ends.
 *   - **a filename alone is not attachment capture.** `collectCandidates()` returning a name with
 *     no reachable handle is exactly the case §7.3 calls `content_no_attachments`, and the
 *     result says so rather than guessing.
 *   - **a failed read never fails the submission.** Every error path returns a structured result
 *     the caller can count; nothing here throws past its own boundary.
 */

/**
 * @typedef {object} FileCandidate
 * @property {string} ref_id           opaque id for this snapshot; the File itself never leaves
 * @property {string} name
 * @property {string} media_type
 * @property {number} size_bytes
 * @property {number} last_modified
 * @property {string} source           'input' | 'drop' | 'clipboard'
 */

/**
 * Build the page-side file registry.
 * @param {object} opts
 * @param {Document} [opts.document]
 * @param {number} [opts.maxFiles]     §7.3/E3's cap; the envelope allows 32 attachment descriptors.
 */
export function createFileRegistry({ document = globalThis.document, maxFiles = 32, onReadError = null } = {}) {
  /** @type {Map<string, {file: any, candidate: FileCandidate, reads: number}>} */
  const handles = new Map();
  /** Files seen on drop events, which never appear in an `<input type=file>`. */
  const dropped = [];
  let counter = 0;

  function snapshot(file, source) {
    if (!file || typeof file !== 'object') return null;
    if (typeof file.name !== 'string' || file.name === '') return null;
    const refId = `f${++counter}`;
    const candidate = {
      ref_id: refId,
      name: file.name,
      media_type: typeof file.type === 'string' ? file.type : '',
      size_bytes: Number.isFinite(file.size) ? file.size : 0,
      last_modified: Number.isFinite(file.lastModified) ? file.lastModified : 0,
      source,
    };
    handles.set(refId, { file, candidate, reads: 0 });
    return candidate;
  }

  /** Register the files on a drop event. The drop target is a page element the user chose. */
  function noteDrop(fileList) {
    const files = Array.from(fileList || []);
    for (const f of files) {
      if (dropped.length >= maxFiles) break;
      dropped.push(f);
    }
    return dropped.length;
  }

  /**
   * Resolve the page's file input or drop target to the `File` objects the user selected.
   * Metadata only: the order is newest-first for drops, then inputs.
   * @returns {{candidates: FileCandidate[], reachable: boolean, reason: string}}
   */
  function collectCandidates() {
    const out = [];
    // A drop target that already received files is the strongest signal: it is the element the
    // user actually used. Drops are reported first and inputs only top the list up afterwards.
    let sawDrop = false;
    for (const f of dropped.slice(-maxFiles).reverse()) {
      const c = snapshot(f, 'drop');
      if (c) {
        out.push(c);
        sawDrop = true;
      }
      if (out.length >= maxFiles) break;
    }
    let sawInput = false;
    if (out.length < maxFiles && document && typeof document.querySelectorAll === 'function') {
      for (const input of document.querySelectorAll('input[type="file"]')) {
        const files = input && input.files;
        if (!files || !files.length) continue;
        for (const f of Array.from(files).slice(0, maxFiles - out.length)) {
          const c = snapshot(f, 'input');
          if (c) {
            out.push(c);
            sawInput = true;
          }
        }
        if (out.length >= maxFiles) break;
      }
    }
    if (out.length > 0) {
      // The reason names the path that produced the candidates, because the caller records it:
      // §7.3 resolves "the page's file input **or** drop target", and which one answered is a
      // coverage fact rather than a detail.
      return { candidates: out, reachable: true, reason: sawDrop ? (sawInput ? 'drop_and_input' : 'drop') : 'input' };
    }
    // §7.3's honest case: the composer built the upload in a worker or canvas and no File handle
    // is reachable. The caller records `content_no_attachments` and counts it.
    return { candidates: [], reachable: false, reason: 'no_reachable_file' };
  }

  function get(refId) {
    const h = handles.get(refId);
    return h ? h.candidate : null;
  }

  function sizeOf(refId) {
    const h = handles.get(refId);
    return h ? h.candidate.size_bytes : null;
  }

  /**
   * Read one slice. Returns bytes, or a structured failure: never throws to the caller, because
   * "a failed attachment read never fails the submission" (§7.3).
   * @returns {Promise<{ok: true, bytes: Uint8Array}|{ok: false, code: string, message: string}>}
   */
  async function readSlice(refId, offset, length) {
    const h = handles.get(refId);
    if (!h) return { ok: false, code: 'unknown_ref', message: `no held File for ${refId}` };
    try {
      const end = Math.min(offset + length, h.candidate.size_bytes);
      const blob = h.file.slice(offset, end);
      let buf;
      if (typeof blob.arrayBuffer === 'function') {
        buf = await blob.arrayBuffer();
      } else if (typeof FileReader !== 'undefined') {
        buf = await new Promise((resolve, reject) => {
          const r = new FileReader();
          r.onload = () => resolve(r.result);
          r.onerror = () => reject(r.error || new Error('FileReader failed'));
          r.readAsArrayBuffer(blob);
        });
      } else {
        return { ok: false, code: 'no_reader', message: 'no way to read a Blob in this world' };
      }
      h.reads++;
      return { ok: true, bytes: new Uint8Array(buf) };
    } catch (e) {
      const failure = { ok: false, code: 'read_failed', message: String((e && e.message) || e) };
      if (onReadError) onReadError({ ref_id: refId, ...failure });
      return failure;
    }
  }

  function forget(refId) {
    return handles.delete(refId);
  }

  function forgetAll() {
    handles.clear();
    dropped.length = 0;
  }

  function heldCount() {
    return handles.size;
  }

  return { collectCandidates, noteDrop, get, sizeOf, readSlice, forget, forgetAll, heldCount };
}
