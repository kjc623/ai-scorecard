/**
 * test/socket-capture.test.mjs — every web tool whose prompt rides a WebSocket, replayed through the
 * extension as the main-world hook hands its frames over, captures exactly as its fixture says.
 *
 * `webRequest` sees a socket open and never its frames, so these tools (Microsoft Copilot, Microsoft
 * 365 Copilot) are observed only when the main-world hook wraps `WebSocket.prototype.send` and relays
 * each frame to the worker. A fixture in test/sockets/ holds the socket URL, the frames the page sent
 * for one turn (option and handshake frames among them), and the text the person typed. The replay
 * drives each frame through the worker's `capture_ws_frame` path and reports what reached capture-core:
 *   observed    at least one observation reached capture-core
 *   attributed  every one is named by the tool's own `app:` row, so it is named on the dashboard
 *   prompt      every one's content is exactly what the person typed
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  bundleCatalog,
  loadFixtures,
  replaySocket,
  socketTools,
  toolCatalogue,
  SOCKET_FIXTURES,
} from '../test-support/tool-replay.mjs';

const catalogue = toolCatalogue();
const catalog = bundleCatalog();
const fixtures = loadFixtures(SOCKET_FIXTURES);

test('every socket tool in the app catalogue has a capture fixture', () => {
  const covered = new Set(fixtures.map((f) => f.app_key));
  for (const [app_key, display_name] of socketTools(catalog, catalogue)) {
    assert.ok(covered.has(app_key), `${display_name} (${app_key}) has no fixture in test/sockets/`);
  }
});

for (const fixture of fixtures) {
  test(`${fixture.tool}: ${describe(fixture.expect)}`, async () => {
    for (const key of ['tool', 'app_key', 'typed', 'socket', 'expect']) assert.ok(fixture[key], `${fixture.file} names its ${key}`);
    assert.ok(Array.isArray(fixture.socket.frames) && fixture.socket.frames.length, `${fixture.file} carries the socket frames`);
    assert.ok(['documented', 'captured'].includes(fixture.source?.kind), `${fixture.file} says whether it was captured or written from documentation`);
    const expected = fixture.expect;
    if (!expected.observed || !expected.attributed || !expected.prompt || expected.events !== 1) assert.ok(expected.why, `${fixture.file} says why the tool is not fully captured`);

    const got = await replaySocket(fixture, { catalogue, catalog });
    const events = got.events.map((e) => `${e.route} ${e.named ?? 'not catalogued'} ${JSON.stringify(e.content)}`).join('; ');
    assert.deepEqual(
      { events: got.events.length, observed: got.observed, attributed: got.attributed, prompt: got.prompt },
      { events: expected.events, observed: expected.observed, attributed: expected.attributed, prompt: expected.prompt },
      `${fixture.tool}: events: ${events || 'none'}`,
    );
  });
}

for (const fixture of fixtures) {
  test(`${fixture.tool}: every event is named by its own app row`, async () => {
    const got = await replaySocket(fixture, { catalogue, catalog });
    for (const event of got.events) assert.equal(event.fingerprint, `app:${fixture.app_key}`, `${fixture.tool}: the ${event.route} event`);
  });
}

function describe(expect) {
  if (!expect.observed) return 'not captured';
  if (expect.events === 1 && expect.attributed && expect.prompt) return 'captured once, attributed and stored as typed';
  const missing = [
    expect.events !== 1 && `${expect.events} events`,
    !expect.attributed && 'not attributed',
    !expect.prompt && 'prompt not stored as typed',
  ].filter(Boolean);
  return `captured, ${missing.join(', ')}`;
}
