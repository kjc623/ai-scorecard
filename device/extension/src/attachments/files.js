/**
 * attachments/files.js — the page-side file registry. Runs in the content script's isolated world,
 * the only place a `File` object exists.
 *
 *   - Noted as handed over: a file is noted when the user chooses it in a file input, drops it or
 *     pastes it, because a chat page usually uploads a chosen file at once and then clears its
 *     input (so the same file can be chosen again): by the time the prompt is sent, the input
 *     holds nothing. A file still held by an input is offered too.
 *   - Snapshot on send, never read-through: `collectCandidates()` reads metadata only and keeps the
 *     page's `File` reference; no byte is read until a transfer is opened.
 *   - `File` references are held, not copied, and `forget()` drops one when its transfer ends. A
 *     file that has been transferred is not offered again, so it attaches to one submission only.
 *   - A filename alone is not attachment capture: with no reachable handle the result says
 *     `no_reachable_file` rather than guessing.
 *   - A failed read never fails the submission: every error path returns a structured result.
 */

/**
 * @typedef {object} FileCandidate
 * @property {string} ref_id           opaque id for this snapshot; the File itself never leaves
 * @property {string} name
 * @property {string} media_type
 * @property {number} size_bytes
 * @property {number} last_modified
 * @property {string} source           'input' | 'drop' | 'paste'
 */

/** The order sources are named in a check's reason. */
const SOURCES = Object.freeze(['drop', 'paste', 'input']);

/**
 * Build the page-side file registry.
 * @param {object} opts
 * @param {Document} [opts.document]
 * @param {number} [opts.maxFiles]     the envelope admits 32 attachment descriptors
 */
export function createFileRegistry({ document = globalThis.document, maxFiles = 32, onReadError = null } = {}) {
  /** @type {Map<string, {file: any, candidate: FileCandidate, reads: number}>} */
  const handles = new Map();
  /** Files handed to the page and not yet attached to a submission, oldest first. */
  let noted = [];
  /** Files already attached to a submission; an input keeps its files after the page sends them. */
  const attached = new WeakSet();
  let counter = 0;

  function isFile(file) {
    return Boolean(file) && typeof file === 'object' && typeof file.name === 'string' && file.name !== '';
  }

  function snapshot(file, source) {
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

  /**
   * Note the files the user handed to the page: chosen in an input, dropped or pasted. A file
   * already noted or attached is not noted again; past `maxFiles` the oldest is let go.
   */
  function note(fileList, source) {
    for (const file of Array.from(fileList || [])) {
      if (!isFile(file) || attached.has(file) || noted.some((n) => n.file === file)) continue;
      noted.push({ file, source });
    }
    if (noted.length > maxFiles) noted = noted.slice(-maxFiles);
    return noted.length;
  }

  /**
   * The files to offer a submission: those noted, newest first, then any an input still holds.
   * Metadata only.
   * @returns {{candidates: FileCandidate[], reachable: boolean, reason: string}}
   */
  function collectCandidates() {
    noted = noted.filter((n) => !attached.has(n.file));
    const offered = new Set();
    const out = [];
    const offer = (file, source) => {
      if (out.length >= maxFiles || !isFile(file) || attached.has(file) || offered.has(file)) return;
      offered.add(file);
      out.push(snapshot(file, source));
    };
    for (const n of [...noted].reverse()) offer(n.file, n.source);
    if (document && typeof document.querySelectorAll === 'function') {
      for (const input of document.querySelectorAll('input[type="file"]')) {
        for (const file of Array.from((input && input.files) || [])) offer(file, 'input');
      }
    }
    if (out.length > 0) {
      // The reason names the paths that produced the candidates, because the caller records it.
      const used = new Set(out.map((c) => c.source));
      return { candidates: out, reachable: true, reason: SOURCES.filter((s) => used.has(s)).join('_and_') };
    }
    // The composer built the upload in a worker or canvas and no File handle is reachable. The
    // caller records `content_no_attachments` and counts it.
    return { candidates: [], reachable: false, reason: 'no_reachable_file' };
  }

  function get(refId) {
    const h = handles.get(refId);
    return h ? h.candidate : null;
  }

  /**
   * Read one slice. Returns bytes, or a structured failure; never throws, because a failed
   * attachment read must not fail the submission.
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

  /** Drop the reference once its transfer has ended; the file is not offered again. */
  function forget(refId) {
    const h = handles.get(refId);
    if (h) {
      attached.add(h.file);
      noted = noted.filter((n) => n.file !== h.file);
    }
    return handles.delete(refId);
  }

  function heldCount() {
    return handles.size;
  }

  return { collectCandidates, note, get, readSlice, forget, heldCount };
}
