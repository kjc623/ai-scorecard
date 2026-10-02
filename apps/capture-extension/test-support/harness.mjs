/**
 * harness.mjs — wires a fake chrome plus a fake capture-core into the real extension code, so
 * every test drives the same modules the browser will.
 *
 * The fake core is wired in *before* the adapter is built, because the adapter closes over
 * `chrome.runtime.connectNative` at construction time. Wiring it afterwards would leave every
 * request unanswered, and a test that "passes" against an unanswered channel is worse than none.
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
 * chrome, and a fake core answering on every native port.
 */
export function createHarness({ core = {}, capacity = 200, failConnect = false, deviceId = null, document = null } = {}) {
  const fake = createFakeChrome();
  fake.state.failConnect = failConnect;

  const crypto = fakeCrypto();
  const fakeCore = createFakeCore(core);

  // Route every native port into the fake core, before anything builds an adapter.
  const origConnect = fake.chrome.runtime.connectNative.bind(fake.chrome.runtime);
  let connectCount = 0;
  fake.chrome.runtime.connectNative = (application) => {
    connectCount += 1;
    if (connectCount > 50) {
      // A reconnect loop is a defect in the extension, and a test that hangs instead of failing
      // hides it. Fail loudly.
      throw new Error(`connectNative called ${connectCount} times: the channel is reconnecting in a loop`);
    }
    if (failConnect) return origConnect(application);
    const port = origConnect(application);
    const realPost = port.postMessage.bind(port);
    port.postMessage = (message) => {
      realPost(message);
      void fakeCore
        .handle(message)
        .then((answer) => {
          if (answer) queueMicrotask(() => port.__answer(answer));
        })
        .catch((e) => {
          // A throw inside the fake core must surface as a refusal, not as a request that never
          // gets answered — a hang hides the defect the fake exists to catch.
          queueMicrotask(() =>
            port.__answer({
              type: 'refusal',
              version: 1,
              id: message.id,
              body: { reason: 'malformed', message: `fake core threw: ${(e && e.message) || e}` },
            }),
          );
        });
    };
    return port;
  };

  const chromeObj = { ...fake.chrome, crypto };
  const scope = { chrome: chromeObj, crypto, performance: globalThis.performance };

  const adapter = createChromeAdapter(scope);
  const contentAdapter = createContentScriptAdapter(scope);
  const app = bootstrapWorker(adapter, { capacity, deviceId });
  const content = bootstrapContentScript(contentAdapter, { document });

  return { fake, scope, adapter, contentAdapter, content, core: fakeCore, app, crypto };
}

/** Let queued microtasks (native replies) settle. */
export async function settle(n = 8) {
  for (let i = 0; i < n; i++) await new Promise((resolve) => setImmediate(resolve));
}

export { createFakeChrome, createFakeCore, NATIVE_MESSAGE_VERSION };
