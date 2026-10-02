/**
 * harness.mjs — wires a fake chrome plus a fake capture-core into the real extension code, so
 * every test drives the same modules the browser will.
 */

import { webcrypto } from 'node:crypto';
import { createChromeAdapter, createContentScriptAdapter } from '../src/chrome-adapter.js';
import { createFakeChrome } from './fake-chrome.mjs';
import { createFakeCore } from './fake-core.mjs';
import { bootstrap as bootstrapWorker } from '../background/service-worker.js';
import { bootstrapContentScript } from '../content/content-script.js';
import { NATIVE_MESSAGE_VERSION } from '../src/messages.js';

/** Deterministic crypto: Node's WebCrypto, so digests are real, plus a counter-based UUID. */
export function fakeCrypto() {
  let n = 0;
  return {
    subtle: webcrypto.subtle,
    getRandomValues: (arr) => webcrypto.getRandomValues(arr),
    randomUUID: () => {
      n += 1;
      return `00000000-0000-4000-8000-${String(n).padStart(12, '0')}`;
    },
  };
}

/**
 * A complete extension under test: the real worker wiring, the real content-script wiring, a fake
 * chrome and a fake core answering on the native port.
 */
export function createHarness({ core = {}, capacity = 200, failConnect = false, deviceId = null } = {}) {
  const fake = createFakeChrome();
  fake.state.failConnect = failConnect;

  const crypto = fakeCrypto();
  const chromeObj = { ...fake.chrome, crypto: fake.crypto };
  // The adapter reads `scope.chrome` and `scope.crypto`.
  const scope = { chrome: chromeObj, crypto, performance: globalThis.performance };

  const adapter = createChromeAdapter(scope);
  const contentAdapter = createContentScriptAdapter(scope);

  const fakeCore = createFakeCore(core);

  const app = bootstrapWorker(adapter, { capacity, deviceId });

  // When a port connects, route its messages into the fake core and answer back.
  const origConnect = fake.chrome.runtime.connectNative.bind(fake.chrome.runtime);
  fake.chrome.runtime.connectNative = (application) => {
    const port = origConnect(application);
    port.onMessage.addListener(() => {});
    const realPost = port.postMessage.bind(port);
    port.postMessage = (message) => {
      realPost(message);
      const answer = fakeCore.handle(message);
      if (answer) queueMicrotask(() => port.__answer(answer));
    };
    return port;
  };

  const content = bootstrapContentScript(contentAdapter, { document: null });

  return { fake, scope, adapter, contentAdapter, content, core: fakeCore, app, crypto };
}

/** Let queued microtasks (native replies) settle. */
export async function settle(n = 6) {
  for (let i = 0; i < n; i++) await new Promise((resolve) => setImmediate(resolve));
}

export { createFakeChrome, createFakeCore, NATIVE_MESSAGE_VERSION };
