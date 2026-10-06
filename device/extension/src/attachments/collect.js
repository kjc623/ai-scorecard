/**
 * attachments/collect.js — the files a matched submission carries, gathered from the tab that sent
 * it. Runs in the service worker.
 *
 * The content script holds the `File` objects the user selected or dropped. This asks it which are
 * reachable and has it transfer each one to capture-core, manifest first, before the observation
 * that names them is sent, so the agent can classify the bytes on the device when the observation
 * arrives. The observation itself carries only each file's name, media type, size and digest.
 *
 * Every step is bounded and none can fail the submission: a tab that does not answer, a refused or
 * failed transfer, or a timeout leaves a descriptor without a digest (or none at all) and is counted.
 */

import { MAX_ATTACHMENT_BYTES } from '../messages.js';

export const CHECK_TIMEOUT_MS = 2_000;
export const TRANSFER_TIMEOUT_MS = 120_000;

/**
 * @param {object} opts
 * @param {import('../adapter.js').Adapter} opts.adapter
 * @param {{countError: (code: string) => void}} opts.counters
 */
export function createAttachmentCollector({ adapter, counters, checkTimeoutMs = CHECK_TIMEOUT_MS, transferTimeoutMs = TRANSFER_TIMEOUT_MS }) {
  /** One message to the tab's content script; null when it does not answer in time. */
  async function ask(tabId, message, timeoutMs) {
    let timer;
    try {
      return await Promise.race([
        Promise.resolve(adapter.tabs.sendMessage(tabId, message)),
        new Promise((resolve) => {
          timer = setTimeout(() => resolve(null), timeoutMs);
        }),
      ]);
    } catch {
      return null;
    } finally {
      clearTimeout(timer);
    }
  }

  /**
   * Transfer the reachable files of tab `tabId` for the observation `observationId`.
   * @returns {Promise<{attachments: object[], reachable: boolean}>}
   */
  async function collect({ tabId, observationId }) {
    if (!Number.isInteger(tabId) || tabId < 0) return { attachments: [], reachable: false };
    const check = await ask(tabId, { type: 'capture_upload_check' }, checkTimeoutMs);
    if (!check || check.ok !== true || !Array.isArray(check.candidates) || check.candidates.length === 0) {
      return { attachments: [], reachable: false };
    }

    const attachments = [];
    for (const candidate of check.candidates) {
      const answer = await ask(tabId, { type: 'capture_upload_send', observation_id: observationId, descriptor: candidate }, transferTimeoutMs);
      const result = answer && answer.ok === true ? answer.result : null;
      if (result && result.status === 'sent' && result.descriptor && result.descriptor.content_digest) {
        attachments.push(result.descriptor);
        continue;
      }
      counters.countError(!result ? 'native_timeout' : result.status === 'refused' ? 'core_refused' : result.reason || 'attachment_read_failed');
      attachments.push(describe(candidate));
    }
    return { attachments, reachable: true };
  }

  return { collect };
}

/** A descriptor for a file whose bytes were not transferred: what the page knows, and no digest. */
function describe(candidate) {
  const d = { name: candidate.name };
  if (candidate.media_type) d.media_type = candidate.media_type;
  // A size over the transport cap would make the whole observation invalid; the name still says a
  // file was attached.
  if (Number.isFinite(candidate.size_bytes) && candidate.size_bytes <= MAX_ATTACHMENT_BYTES) d.size_bytes = candidate.size_bytes;
  return d;
}
