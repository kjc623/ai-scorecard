/**
 * test/attachments.test.mjs — §7.3's attachment capture.
 *
 * The load-bearing assertion is not that a refusal is *handled*; it is that on a refusal
 * `readSlice` is **never called**, because §3.4's manifest exists so that "capture-core [can]
 * refuse an oversized upload *before* transfer". A test that only checks the returned status
 * would pass against an implementation that streamed the whole file and then gave up.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { createAttachmentSender, bytesToBase64, createChunkedHasher } from '../src/attachments/sender.js';
import { createFileRegistry } from '../src/attachments/files.js';
import { createFakeCore } from '../test-support/fake-core.mjs';
import { CORE_TYPE, MAX_ATTACHMENT_BYTES, NATIVE_MESSAGE_VERSION, REFUSAL, TYPE } from '../src/messages.js';
import { fakeCrypto, settle } from '../test-support/harness.mjs';

/** A content-script-shaped sender whose frames go straight to a fake core. */
function makeSender(core, overrides = {}) {
  const counts = [];
  const bridge = {
    async sendMessage(message) {
      const answer = await core.handle({ type: message.frame_type, version: NATIVE_MESSAGE_VERSION, id: `m${counts.length}`, body: message.body });
      return { ok: true, answer };
    },
  };
  const sender = createAttachmentSender({
    bridge,
    crypto: fakeCrypto(),
    count: (kind, n = 1) => counts.push([kind, n]),
    ...overrides,
  });
  return { sender, counts, bridge };
}

/** A File stand-in that records every slice the sender asked for. */
function fakeFile(bytes, { name = 'quarterly.xlsx', type = 'application/vnd.ms-excel', failsAt = null } = {}) {
  const reads = [];
  return {
    reads,
    descriptor: { ref_id: 'f1', name, media_type: type, size_bytes: bytes.byteLength },
    async readSlice(_refId, offset, length) {
      reads.push({ offset, length });
      if (failsAt !== null && reads.length - 1 === failsAt) {
        return { ok: false, code: 'read_failed', message: 'the page revoked the handle' };
      }
      const end = Math.min(offset + length, bytes.byteLength);
      return { ok: true, bytes: bytes.slice(offset, end) };
    },
    bytes,
  };
}

test('an oversized attachment is refused on the manifest, and NOT ONE BYTE is read', async () => {
  // The core's effective cap is 1 MiB; the file is 8 MiB. §3.4: refuse before transfer.
  const core = createFakeCore({ attachmentCapacityBytes: 1 << 20 });
  const { sender, counts } = makeSender(core);
  const file = fakeFile(new Uint8Array(8 << 20));

  const result = await sender.send({ observation_id: 'o1', descriptor: file.descriptor, readSlice: file.readSlice });

  assert.equal(result.status, 'refused');
  assert.equal(result.reason, REFUSAL.ATTACHMENT_TOO_LARGE);
  assert.equal(result.chunks, 0);
  assert.equal(result.bytes_sent, 0);
  assert.deepEqual(file.reads, [], 'readSlice must never be called on a refused transfer');
  assert.equal(core.received.filter((m) => m.type === TYPE.ATTACHMENT_CHUNK).length, 0, 'no chunk frame was sent');
  assert.equal(core.received.filter((m) => m.type === TYPE.ATTACHMENT_MANIFEST).length, 1, 'exactly one manifest');
  assert.ok(counts.some(([k]) => k === 'attachment_refused'));
});

test('a file over the transport ceiling is refused locally, with no manifest at all', async () => {
  const core = createFakeCore();
  const { sender } = makeSender(core);
  const file = fakeFile(new Uint8Array(MAX_ATTACHMENT_BYTES + 1));
  const result = await sender.send({ observation_id: 'o1', descriptor: file.descriptor, readSlice: file.readSlice });
  assert.equal(result.status, 'refused');
  assert.equal(result.reason, REFUSAL.ATTACHMENT_TOO_LARGE);
  assert.deepEqual(file.reads, []);
  assert.equal(core.received.length, 0, 'a file that cannot be accepted never reaches capture-core');
});

test('an accepted attachment is sent as contiguous zero-based chunks, manifest first', async () => {
  const core = createFakeCore({ attachmentCapacityBytes: 1 << 20, chunkBytes: 1000 });
  const { sender, counts } = makeSender(core);
  const bytes = new Uint8Array(2500);
  for (let i = 0; i < bytes.length; i++) bytes[i] = i & 0xff;
  const file = fakeFile(bytes);

  const result = await sender.send({ observation_id: 'o1', descriptor: file.descriptor, readSlice: file.readSlice });

  assert.equal(result.status, 'sent');
  assert.equal(result.chunks, 3, '2500 bytes in 1000-byte chunks');
  assert.equal(result.bytes_sent, 2500);
  assert.ok(/^sha256:[0-9a-f]{64}$/.test(result.digest), 'the descriptor carries a digest only because the bytes were read');

  const order = core.received.map((m) => m.type);
  assert.equal(order[0], TYPE.ATTACHMENT_MANIFEST, 'the manifest is first, always');
  assert.deepEqual(
    order.slice(1, 4),
    [TYPE.ATTACHMENT_CHUNK, TYPE.ATTACHMENT_CHUNK, TYPE.ATTACHMENT_CHUNK],
  );
  assert.equal(order[4], TYPE.ATTACHMENT_COMPLETE);
  assert.deepEqual(
    core.received.filter((m) => m.type === TYPE.ATTACHMENT_CHUNK).map((m) => m.body.seq),
    [0, 1, 2],
    'seq is zero-based and contiguous, or capture-core refuses the gap',
  );
  assert.ok(counts.some(([k]) => k === 'attachment_sent'));
});

test('the digest is over exactly the bytes sent, so the manifest describes what moved', async () => {
  const core = createFakeCore({ attachmentCapacityBytes: 1 << 20, chunkBytes: 7 });
  const { sender } = makeSender(core);
  const bytes = new TextEncoder().encode('the quick brown fox jumps over the lazy dog');
  const file = fakeFile(bytes, { name: 'notes.txt' });
  const result = await sender.send({ observation_id: 'o1', descriptor: file.descriptor, readSlice: file.readSlice });

  const { createChunkedHasher: _unused, ...rest } = {};
  const { sha256Prefixed } = await import('../src/codec.js');
  const expected = await sha256Prefixed(fakeCrypto(), bytes);
  assert.equal(result.digest, expected);
});

test('a failed read never fails the submission: it is counted, reported, and the transfer closes', async () => {
  const core = createFakeCore({ attachmentCapacityBytes: 1 << 20, chunkBytes: 500 });
  const { sender, counts } = makeSender(core);
  const file = fakeFile(new Uint8Array(2000), { failsAt: 1 });

  const result = await sender.send({ observation_id: 'o1', descriptor: file.descriptor, readSlice: file.readSlice });

  assert.equal(result.status, 'failed');
  assert.equal(result.reason, 'attachment_read_failed');
  assert.equal(result.chunks, 1, 'the chunk that succeeded is reported');
  assert.equal(result.bytes_sent, 500);
  assert.ok(counts.some(([k]) => k === 'attachment_read_failed'));
  const complete = core.received.find((m) => m.type === TYPE.ATTACHMENT_COMPLETE);
  assert.ok(complete, 'the transfer is closed even when it failed');
  assert.match(complete.body.error, /read_failed/);
  // The observation is untouched by this: the caller still emits the prompt text.
  assert.equal(result.digest, null, 'a partial read must not produce a digest that claims completeness');
});

test('a refusal with mode_forbids_read is passed through verbatim from the closed set', async () => {
  const core = createFakeCore({ modeForbidsRead: true });
  const { sender } = makeSender(core);
  const file = fakeFile(new Uint8Array(100));
  const result = await sender.send({ observation_id: 'o1', descriptor: file.descriptor, readSlice: file.readSlice });
  assert.equal(result.status, 'refused');
  assert.equal(result.reason, REFUSAL.MODE_FORBIDS_READ);
  assert.deepEqual(file.reads, [], 'a mode refusal also happens before any byte');
});

test('a broken channel is reported as a failure, not thrown at the submission', async () => {
  const bridge = {
    async sendMessage() {
      return { ok: false, error: 'native host gone' };
    },
  };
  const sender = createAttachmentSender({ bridge, crypto: fakeCrypto(), count: () => {} });
  const file = fakeFile(new Uint8Array(10));
  const result = await sender.send({ observation_id: 'o1', descriptor: file.descriptor, readSlice: file.readSlice });
  assert.equal(result.status, 'failed');
  assert.equal(result.reason, 'native_unavailable');
  assert.deepEqual(file.reads, []);
});

test('a cancelled transfer stops at a chunk boundary and closes the transfer', async () => {
  const core = createFakeCore({ attachmentCapacityBytes: 1 << 20, chunkBytes: 100 });
  const { sender, counts } = makeSender(core);
  const file = fakeFile(new Uint8Array(1000));
  let n = 0;
  const result = await sender.send({
    observation_id: 'o1',
    descriptor: file.descriptor,
    readSlice: file.readSlice,
    isCancelled: () => n++ >= 2,
  });
  assert.equal(result.status, 'failed');
  assert.equal(result.reason, 'cancelled');
  assert.ok(counts.some(([k]) => k === 'attachment_cancelled'));
  assert.ok(core.received.some((m) => m.type === TYPE.ATTACHMENT_COMPLETE && m.body.error === 'cancelled'));
});

test('base64 is what encoding/json emits for []byte, and it round-trips', () => {
  const bytes = new Uint8Array([0, 1, 2, 253, 254, 255, 128]);
  const b64 = bytesToBase64(bytes);
  assert.equal(Buffer.from(b64, 'base64').length, bytes.byteLength);
  assert.deepEqual([...Buffer.from(b64, 'base64')], [...bytes]);
});

test('the chunked hasher digests what was fed to it, in order', async () => {
  const hasher = createChunkedHasher(fakeCrypto());
  hasher.update(new Uint8Array([1, 2]));
  hasher.update(new Uint8Array([3]));
  assert.equal(hasher.bytes, 3);
  const digest = await hasher.digest();
  const { sha256Prefixed } = await import('../src/codec.js');
  assert.equal(digest, await sha256Prefixed(fakeCrypto(), new Uint8Array([1, 2, 3])));
});

// ── files.js: the page-context half ──────────────────────────────────────────────────────────

function fakeDocument(inputs) {
  return {
    querySelectorAll(selector) {
      if (selector !== 'input[type="file"]') return [];
      return inputs.map((files) => ({ files }));
    },
  };
}

test('a file input resolves to File-like candidates with metadata only', () => {
  const registry = createFileRegistry({
    document: fakeDocument([[{ name: 'report.pdf', type: 'application/pdf', size: 1234, lastModified: 5 }]]),
  });
  const result = registry.collectCandidates();
  assert.equal(result.reachable, true);
  assert.equal(result.reason, 'input');
  assert.equal(result.candidates.length, 1);
  assert.deepEqual(Object.keys(result.candidates[0]).sort(), ['last_modified', 'media_type', 'name', 'ref_id', 'size_bytes', 'source']);
  assert.equal(result.candidates[0].name, 'report.pdf');
  assert.equal(result.candidates[0].source, 'input');
});

test('§7.3: no reachable File handle is `no_reachable_file`, which is what content_no_attachments records', () => {
  const registry = createFileRegistry({ document: fakeDocument([]) });
  const result = registry.collectCandidates();
  assert.deepEqual(result.candidates, []);
  assert.equal(result.reachable, false);
  assert.equal(result.reason, 'no_reachable_file', 'a filename alone is not attachment capture');
});

test('a dropped file is preferred over an input, because it is the element the user used', () => {
  const registry = createFileRegistry({ document: fakeDocument([[{ name: 'from-input.pdf', type: 'application/pdf', size: 10, lastModified: 1 }]]) });
  // Chrome's File is a Blob with a name; the registry reads name/type/size/lastModified and holds the object.
  registry.noteDrop([{ name: 'dropped.pdf', type: 'application/pdf', size: 2, lastModified: 1 }]);
  const result = registry.collectCandidates();
  assert.equal(result.reason, 'drop_and_input', 'the drop is first in the list and the input tops it up');
  assert.equal(result.candidates.length, 2);
  assert.equal(result.candidates[0].name, 'dropped.pdf', 'the drop is the element the user actually used');
  assert.equal(result.candidates[0].source, 'drop');
  assert.equal(result.candidates[1].source, 'input');
});

test('the reason names which path resolved the files, and the cap is honoured', () => {
  const inputs = Array.from({ length: 40 }, (_, i) => ({ name: `f${i}.pdf`, type: '', size: 1, lastModified: 1 }));
  const registry = createFileRegistry({ document: fakeDocument([inputs]) });
  const result = registry.collectCandidates();
  assert.equal(result.reason, 'input');
  assert.equal(result.candidates.length, 32, 'E3/§7.3: the envelope admits 32 attachment descriptors');
  assert.equal(registry.heldCount(), 32);
});

test('the registry never reads a byte at selection time — snapshot-on-send, not read-through', () => {
  let sliced = 0;
  const file = {
    name: 'big.bin',
    type: 'application/octet-stream',
    size: 10_000_000,
    lastModified: 1,
    slice() {
      sliced += 1;
      return { arrayBuffer: async () => new ArrayBuffer(0) };
    },
  };
  const registry = createFileRegistry({ document: fakeDocument([[file]]) });
  registry.collectCandidates();
  assert.equal(sliced, 0, 'collect must read metadata only: reading at selection holds bytes for files the user never sends');
  assert.equal(registry.heldCount(), 1, 'the reference is held, and it is droppable');
});

test('a slice is read on demand and the reference can be dropped afterwards', async () => {
  const file = {
    name: 'a.txt',
    type: 'text/plain',
    size: 10,
    lastModified: 1,
    slice(offset, end) {
      return { arrayBuffer: async () => new Uint8Array([offset, end]).buffer };
    },
  };
  const registry = createFileRegistry({ document: fakeDocument([[file]]) });
  const [candidate] = registry.collectCandidates().candidates;
  const r = await registry.readSlice(candidate.ref_id, 2, 4);
  assert.equal(r.ok, true);
  assert.deepEqual([...r.bytes], [2, 6]);
  assert.equal(registry.forget(candidate.ref_id), true);
  assert.equal(registry.heldCount(), 0);
});

test('reading a reference that was never held fails as a value, not an exception', async () => {
  const registry = createFileRegistry({ document: fakeDocument([]) });
  const r = await registry.readSlice('nope', 0, 10);
  assert.equal(r.ok, false);
  assert.equal(r.code, 'unknown_ref');
});

test('a File whose read throws is reported as a failed read, never an exception', async () => {
  const file = {
    name: 'locked.doc',
    type: '',
    size: 4,
    lastModified: 1,
    slice() {
      throw new Error('NotAllowedError: the page revoked the handle');
    },
  };
  const errors = [];
  const registry = createFileRegistry({ document: fakeDocument([[file]]), onReadError: (e) => errors.push(e) });
  const [candidate] = registry.collectCandidates().candidates;
  const r = await registry.readSlice(candidate.ref_id, 0, 4);
  assert.equal(r.ok, false);
  assert.equal(r.code, 'read_failed');
  assert.equal(errors.length, 1, 'the failure is reported to the caller so it can be counted');
  assert.match(errors[0].message, /NotAllowedError/);
});
