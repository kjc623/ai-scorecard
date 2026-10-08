/**
 * test/content-script.test.mjs — the isolated-world wiring: the page resolves a `File` handle, and
 * the transfer is driven from here through the service worker's relay to capture-core. The page is
 * a fake document; tools/in-browser-check.mjs covers a real one.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { bootstrapContentScript, showBlocked, showWarning } from '../content/content-script.js';
import { createContentScriptAdapter } from '../src/chrome-adapter.js';
import { fakeCrypto } from '../test-support/harness.mjs';
import { CORE_TYPE, NATIVE_MESSAGE_VERSION, REFUSAL, TYPE } from '../src/messages.js';

/** A content-script world: a fake document, a bridge, and the real content-script wiring. */
function makeWorld({ core, files = [], drop = null, document: doc = null } = {}) {
  const relayed = [];
  const posted = [];
  const fakeCore = core;

  const scope = {
    crypto: fakeCrypto(),
    performance: globalThis.performance,
    document: doc,
  };

  const chromeObj = {
    runtime: {
      id: 'fake-extension-id',
      onMessage: { addListener: (fn) => (scope.__handler = fn) },
      sendMessage: async (message) => {
        relayed.push(message);
        if (message.type !== 'capture_attachment_frame') return { ok: false, error: 'unexpected' };
        const answer = await fakeCore.handle({
          type: message.frame_type,
          version: NATIVE_MESSAGE_VERSION,
          id: `r${relayed.length}`,
          body: message.body,
        });
        posted.push(answer);
        return { ok: true, answer };
      },
    },
  };
  scope.chrome = chromeObj;

  const adapter = createContentScriptAdapter(scope);
  const filesRegistry = [];
  const inputFiles = files.map((f) => ({
    name: f.name,
    type: f.media_type || '',
    size: f.bytes.byteLength,
    lastModified: 1,
    slice(offset, end) {
      filesRegistry.push({ name: f.name, offset, end });
      return { arrayBuffer: async () => f.bytes.slice(offset, end).buffer };
    },
  }));
  const document = doc || {
    querySelectorAll: (s) => (s === 'input[type="file"]' ? [{ files: inputFiles }] : []),
    addEventListener: () => {},
    createElement: () => makeElement(),
    body: { appendChild: () => {} },
    documentElement: {},
  };

  const app = bootstrapContentScript(adapter, { document });
  if (drop) app.registry.noteDrop(drop);

  return {
    app,
    relayed,
    posted,
    /**
     * Drive a message the way Chrome does: the listener returns `true` to keep the channel open and
     * answers through `sendResponse` asynchronously. Awaiting the response is what makes the test
     * observe the same value the service worker would.
     */
    dispatch: (message) =>
      new Promise((resolve) => {
        const returned = scope.__handler(message, { tab: { id: 1 } }, resolve);
        if (returned !== true) resolve(returned);
      }),
    get listener() {
      return scope.__handler;
    },
    filesRegistry,
  };
}

function makeElement() {
  const el = {
    style: { cssText: '' },
    children: [],
    textContent: '',
    setAttribute: () => {},
    addEventListener: (type, fn) => (el['on' + type] = fn),
    append(...kids) {
      el.children.push(...kids);
    },
    remove: () => {},
  };
  return el;
}

// ── resolving the file input ───────────────────────────────────────────────────────────────

test('capture_upload_check answers with metadata only, and never reads a byte', async () => {
  const world = makeWorld({
    core: { handle: async () => ({ type: CORE_TYPE.ACK, version: 1, body: {} }) },
    files: [{ name: 'quarterly.xlsx', media_type: 'application/vnd.ms-excel', bytes: new Uint8Array(9000) }],
  });

  const result = await world.dispatch({ type: 'capture_upload_check' });

  assert.equal(result.ok, true);
  assert.equal(result.reachable, true);
  assert.equal(result.candidates.length, 1);
  assert.equal(result.candidates[0].name, 'quarterly.xlsx');
  assert.equal(result.candidates[0].size_bytes, 9000);
  assert.equal(typeof result.candidates[0].ref_id, 'string', 'the File stays in the page; only a ref travels');
  assert.equal('bytes' in result.candidates[0], false, 'snapshot-on-send: no bytes at check time');
  assert.deepEqual(world.filesRegistry, [], 'no slice was taken: reading at selection holds bytes the user never sends');
});

test('no reachable File handle reports no_reachable_file, which is what records content_no_attachments', async () => {
  const world = makeWorld({
    core: { handle: async () => ({ type: CORE_TYPE.ACK, version: 1, body: {} }) },
    files: [],
  });
  const result = await world.dispatch({ type: 'capture_upload_check' });
  assert.equal(result.ok, true);
  assert.equal(result.reachable, false);
  assert.equal(result.reason, 'no_reachable_file');
  assert.deepEqual(result.candidates, []);
});

test('a dropped file is offered first, because it is the element the user used', async () => {
  const world = makeWorld({
    core: { handle: async () => ({ type: CORE_TYPE.ACK, version: 1, body: {} }) },
    files: [{ name: 'from-input.pdf', media_type: 'application/pdf', bytes: new Uint8Array(10) }],
    drop: [{ name: 'dropped.pdf', type: 'application/pdf', size: 2, lastModified: 1 }],
  });
  const result = await world.dispatch({ type: 'capture_upload_check' });
  assert.equal(result.candidates[0].name, 'dropped.pdf');
  assert.equal(result.reason, 'drop_and_input');
});

// ── the chunked transfer, driven from the page ───────────────────────────────────────────────

test('an accepted upload is transferred as manifest-then-chunks through the relay', async () => {
  const seen = [];
  const world = makeWorld({
    core: {
      handle: async (message) => {
        seen.push(message.type);
        if (message.type === TYPE.ATTACHMENT_MANIFEST) {
          return { type: CORE_TYPE.ACK, version: 1, id: message.id, body: { capacity_bytes: 1 << 20, chunk_bytes: 3 } };
        }
        return { type: CORE_TYPE.ACK, version: 1, id: message.id, body: {} };
      },
    },
    files: [{ name: 'a.bin', media_type: 'application/octet-stream', bytes: new Uint8Array([1, 2, 3, 4, 5, 6, 7]) }],
  });

  const check = await world.dispatch({ type: 'capture_upload_check' });
  const result = await world.dispatch({
    type: 'capture_upload_send',
    observation_id: 'obs-1',
    descriptor: check.candidates[0],
  });

  assert.equal(result.ok, true);
  assert.equal(result.result.status, 'sent');
  assert.equal(result.result.chunks, 3, '7 bytes in 3-byte chunks');
  assert.equal(result.result.bytes_sent, 7);
  assert.match(result.result.digest, /^sha256:[0-9a-f]{64}$/, 'the descriptor carries a digest exactly because the bytes were read');

  assert.equal(seen[0], TYPE.ATTACHMENT_MANIFEST, 'manifest first');
  assert.equal(seen[1], TYPE.ATTACHMENT_CHUNK);
  assert.equal(seen[seen.length - 1], TYPE.ATTACHMENT_COMPLETE);
  assert.deepEqual(world.relayed.map((m) => m.frame_type), seen, 'every frame went through the worker relay, not straight to native');
});

test('an oversized upload is refused on the manifest and NO slice is ever taken', async () => {
  const world = makeWorld({
    core: {
      handle: async (message) => {
        if (message.type === TYPE.ATTACHMENT_MANIFEST) {
          // native.go: RefusalAttachmentTooLarge, before any byte moves.
          return {
            type: CORE_TYPE.REFUSAL,
            version: 1,
            id: message.id,
            body: { reason: REFUSAL.ATTACHMENT_TOO_LARGE, message: 'too big' },
          };
        }
        return { type: CORE_TYPE.ACK, version: 1, id: message.id, body: {} };
      },
    },
    files: [{ name: 'huge.bin', media_type: 'application/octet-stream', bytes: new Uint8Array(4 * 1024 * 1024) }],
  });

  const check = await world.dispatch({ type: 'capture_upload_check' });
  const result = await world.dispatch({ type: 'capture_upload_send', observation_id: 'obs-1', descriptor: check.candidates[0] });

  assert.equal(result.ok, true);
  assert.equal(result.result.status, 'refused');
  assert.equal(result.result.reason, REFUSAL.ATTACHMENT_TOO_LARGE);
  assert.equal(result.result.chunks, 0);
  assert.deepEqual(world.filesRegistry, [], 'not one slice was read: the manifest is what makes refusal-before-transfer real');
  assert.deepEqual(
    world.relayed.map((m) => m.frame_type),
    [TYPE.ATTACHMENT_MANIFEST],
    'only the manifest was sent',
  );
});

test('a failed read never fails the submission: the transfer closes and reports a value, not an exception', async () => {
  let calls = 0;
  const world = makeWorld({
    core: {
      handle: async (message) => {
        if (message.type === TYPE.ATTACHMENT_MANIFEST) {
          return { type: CORE_TYPE.ACK, version: 1, id: message.id, body: { capacity_bytes: 1 << 20, chunk_bytes: 2 } };
        }
        return { type: CORE_TYPE.ACK, version: 1, id: message.id, body: {} };
      },
    },
    files: [{ name: 'b.bin', media_type: '', bytes: new Uint8Array(8) }],
  });
  // Make the second slice fail, as a revoked handle would.
  const inputs = world.app.registry;
  const [candidate] = inputs.collectCandidates().candidates;
  const original = inputs.readSlice;
  inputs.readSlice = async (refId, offset, length) => {
    calls += 1;
    if (calls === 2) return { ok: false, code: 'read_failed', message: 'the page revoked the handle' };
    return original.call(inputs, refId, offset, length);
  };

  const result = await world.dispatch({ type: 'capture_upload_send', observation_id: 'obs-1', descriptor: candidate });

  assert.equal(result.ok, true);
  assert.equal(result.result.status, 'failed');
  assert.equal(result.result.reason, 'attachment_read_failed');
  assert.ok(result.result.chunks >= 1, 'the chunks that succeeded are reported');
  const complete = world.relayed.find((m) => m.frame_type === TYPE.ATTACHMENT_COMPLETE);
  assert.ok(complete, 'the transfer is closed even in failure');
  assert.match(complete.body.error, /read_failed/);
});

test('the File reference is dropped once the transfer ends, so nothing is held between sends', async () => {
  const world = makeWorld({
    core: {
      handle: async (message) =>
        message.type === TYPE.ATTACHMENT_MANIFEST
          ? { type: CORE_TYPE.ACK, version: 1, id: message.id, body: { capacity_bytes: 1 << 20, chunk_bytes: 1024 } }
          : { type: CORE_TYPE.ACK, version: 1, id: message.id, body: {} },
    },
    files: [{ name: 'c.bin', media_type: '', bytes: new Uint8Array(4) }],
  });
  const check = await world.dispatch({ type: 'capture_upload_check' });
  assert.equal(world.app.registry.heldCount(), 1);
  await world.dispatch({ type: 'capture_upload_send', observation_id: 'obs-1', descriptor: check.candidates[0] });
  assert.equal(world.app.registry.heldCount(), 0, 'snapshot-on-send, and the reference is not retained afterwards');
});

// ── the warn confirmation, rendered in the page ────────────────────────────────────────────

test('capture_warn renders the confirmation and answers with the user\'s explicit choice', async () => {
  const world = makeWorld({ core: { handle: async () => ({ type: CORE_TYPE.ACK, version: 1, body: {} }) } });
  const elements = [];
  const doc = {
    createElement: () => {
      const el = makeElement();
      elements.push(el);
      return el;
    },
    body: { appendChild: () => {} },
    documentElement: {},
  };

  const pending = showWarning(doc, { host: 'chat.example-ai.invalid', path: '/v1/chat', message: 'Held by policy', timeout_ms: 5000 });
  // The two buttons are the last element created; "Send anyway" is first.
  const host = elements[0];
  const buttons = host.children.filter((c) => c.textContent === 'Send anyway' || c.textContent === 'Cancel request');
  assert.equal(buttons.length, 2, 'both answers are rendered, and the default is not "proceed"');
  buttons[0].onclick?.();
  const answer = await pending;
  assert.equal(answer, true);
});

test('an unanswered confirmation resolves rather than holding the request forever', async () => {
  const doc = {
    createElement: () => makeElement(),
    body: { appendChild: () => {} },
    documentElement: {},
  };
  const answer = await showWarning(doc, { host: 'x.invalid', path: '/', message: 'm', timeout_ms: 5 });
  assert.equal(answer, true, 'bounded, and the caller turns this into fail-open `logged` with confidence: degraded');
});

test('showWarning degrades to a value when there is no document to render into', async () => {
  assert.equal(await showWarning(null, { timeout_ms: 5 }), true);
});

/** A document that records every element it creates. */
function recordingDocument() {
  const elements = [];
  const doc = {
    createElement: (tag) => {
      const el = makeElement();
      el.tag = tag;
      el.attributes = {};
      el.setAttribute = (k, v) => (el.attributes[k] = v);
      elements.push(el);
      return el;
    },
    body: { appendChild: () => {} },
    documentElement: {},
  };
  return { doc, elements };
}

test('the confirmation shows the rule\'s message and link, and no text of its own', async () => {
  const { doc, elements } = recordingDocument();
  const pending = showWarning(doc, { host: 'x.invalid', path: '/', message: 'Check before you send.', link: 'https://intranet.example/ai', timeout_ms: 5 });
  const texts = elements[0].children.map((c) => c.textContent);
  assert.ok(texts.includes('Check before you send.'));
  const link = elements[0].children.find((c) => c.tag === 'a');
  assert.equal(link.attributes.href, 'https://intranet.example/ai');
  assert.equal(link.attributes.rel, 'noopener noreferrer');
  await pending;

  const bare = recordingDocument();
  const unlinked = showWarning(bare.doc, { host: 'x.invalid', path: '/', message: '', link: 'javascript:alert(1)', timeout_ms: 5 });
  assert.equal(bare.elements[0].children.some((c) => c.tag === 'a'), false, 'only an https link is offered');
  assert.equal(bare.elements[0].children[1].textContent, '', 'no default text stands in for the rule\'s message');
  await unlinked;
});

test('capture_block shows the rule\'s message and link with only a way to dismiss it', async () => {
  const page = recordingDocument();
  const world = makeWorld({
    core: { handle: async () => ({ type: CORE_TYPE.ACK, version: 1, body: {} }) },
    document: { ...page.doc, querySelectorAll: () => [] },
  });
  const { doc, elements } = recordingDocument();
  const host = showBlocked(doc, { host: 'chat.example-ai.invalid', path: '/v1/chat', message: 'Remove the credential and try again.', link: 'https://intranet.example/ai' });
  assert.equal(host, elements[0]);
  const texts = host.children.map((c) => c.textContent);
  assert.ok(texts.includes('Request blocked'));
  assert.ok(texts.includes('Remove the credential and try again.'));
  assert.equal(host.children.find((c) => c.tag === 'a').attributes.href, 'https://intranet.example/ai');
  const buttons = host.children.filter((c) => c.tag === 'button');
  assert.deepEqual(buttons.map((b) => b.textContent), ['Dismiss'], 'the request is already cancelled: nothing to send anyway');
  assert.equal(showBlocked(null, {}), null);

  const answer = await world.dispatch({ type: 'capture_block', spec: { message: 'm' } });
  assert.equal(answer.ok, true, 'the worker is answered once the notice is up');
  assert.ok(page.elements[0].children.some((c) => c.textContent === 'm'), 'in the page');
});

// ── routing ─────────────────────────────────────────────────────────────────────────────────

test('an unknown message type is refused as a value, never thrown', async () => {
  const world = makeWorld({ core: { handle: async () => ({ type: CORE_TYPE.ACK, version: 1, body: {} }) } });
  const result = await world.dispatch({ type: 'something_else' });
  assert.equal(result.ok, false);
  assert.match(result.error, /unknown message type/);
});

test('the content script installs a drop listener at capture time', () => {
  let installed = null;
  const doc = {
    querySelectorAll: () => [],
    addEventListener: (type, fn, capture) => (installed = { type, capture }),
    createElement: () => makeElement(),
    body: { appendChild: () => {} },
    documentElement: {},
  };
  const scope = {
    crypto: fakeCrypto(),
    performance: globalThis.performance,
    document: doc,
    chrome: { runtime: { id: 'x', onMessage: { addListener: () => {} }, sendMessage: async () => ({}) } },
  };
  bootstrapContentScript(createContentScriptAdapter(scope), { document: doc });
  assert.equal(installed.type, 'drop', 'a dropped file never appears in an input, so the listener is required');
  assert.equal(installed.capture, true, 'capture phase, so the page cannot stop it reaching us first');
});
