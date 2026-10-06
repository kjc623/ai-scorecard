/**
 * queue.js — the bounded in-memory queue for observations capture-core has not yet accepted.
 *
 * The extension cannot reach the device spool, so undeliverable observations are held in memory
 * only: bounded on write, dropped oldest-first, and every drop counted. Nothing is written to
 * extension storage; the queue dies with the service worker, which is why it is small.
 */

/** A buffer, not a second spool. */
export const DEFAULT_CAPACITY = 200;

export function createQueue({ capacity = DEFAULT_CAPACITY, onDrop = null } = {}) {
  if (!Number.isInteger(capacity) || capacity < 1) throw new RangeError('capacity must be a positive integer');

  /** @type {Array<{kind: string, payload: any, size_bytes: number, enqueued_at: number}>} */
  const entries = [];
  let dropped = 0;
  let droppedBytes = 0;
  /** @type {Record<string, number>} why entries were dropped, so the health report can say */
  const droppedByKind = Object.create(null);
  let enqueued = 0;
  let delivered = 0;
  let sequence = 0;

  function enqueue(kind, payload, sizeBytes) {
    const entry = {
      seq: sequence++,
      kind,
      payload,
      size_bytes: Number.isFinite(sizeBytes) ? sizeBytes : 0,
      enqueued_at: Date.now(),
    };
    entries.push(entry);
    enqueued++;
    while (entries.length > capacity) {
      const evicted = entries.shift();
      droppedEntries(evicted, 1);
    }
    return entry.seq;
  }

  function droppedEntries(evicted, n) {
    dropped += n;
    droppedBytes += evicted.size_bytes || 0;
    droppedByKind[evicted.kind] = (droppedByKind[evicted.kind] || 0) + 1;
    if (onDrop) onDrop(evicted);
  }

  /** Peek without removing: the drain must not lose an entry that fails mid-send. */
  function peek(n = 1) {
    return entries.slice(0, n);
  }

  function ack(seq, n = 1) {
    const idx = entries.findIndex((e) => e.seq === seq);
    if (idx < 0) return 0;
    const removed = entries.splice(idx, n);
    delivered += removed.length;
    return removed.length;
  }

  function size() {
    return entries.length;
  }

  function depthBytes() {
    return entries.reduce((n, e) => n + (e.size_bytes || 0), 0);
  }

  function stats() {
    return {
      depth: entries.length,
      capacity,
      dropped_total: dropped,
      dropped_bytes_total: droppedBytes,
      dropped_by_kind: { ...droppedByKind },
      enqueued_total: enqueued,
      delivered_total: delivered,
      depth_bytes: depthBytes(),
    };
  }

  return { enqueue, peek, ack, size, stats };
}
