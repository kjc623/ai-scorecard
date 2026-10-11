/**
 * tool-replay.mjs — replays one recorded request per web tool through the real extension, as
 * Chrome would hand it over, and reports what reached capture-core.
 *
 * A fixture in test/tools/ holds the request a tool's page sends when a person submits a prompt,
 * the response's status and headers, and the text the person typed. The replay drives the request
 * through the body lane and its completion through the response lane, with only the fields Chrome
 * supplies: `onBeforeRequest` never carries request headers, and `onCompleted` carries response
 * headers only when the listener was registered with `responseHeaders`.
 *
 * What reached capture-core is reported per tool, as the events one submission became and three
 * facts about them:
 *   observed    at least one observation reached capture-core
 *   attributed  every one's fingerprint is catalogued under the tool's app_key, so it is named on the dashboard
 *   prompt      every one's content is exactly what the person typed
 */

import { readdirSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { createHarness, settle } from './harness.mjs';
import { toArrayBuffer } from './fake-chrome.mjs';
import { TYPE } from '../src/messages.js';
import { predicateRequest } from '../src/predicate.js';
import { normaliseBody } from '../src/request-body.js';

const HERE = dirname(fileURLToPath(import.meta.url));
export const FIXTURES = join(HERE, '..', 'test', 'tools');
const SCHEMA = join(HERE, '..', '..', '..', 'services', 'database', 'schema.sql');

/** Every fixture, by file name. */
export function loadFixtures(dir = FIXTURES) {
  return readdirSync(dir)
    .filter((f) => f.endsWith('.json'))
    .sort()
    .map((f) => ({ file: f, ...JSON.parse(readFileSync(join(dir, f), 'utf8')) }));
}

/** The extension rows of the tool catalogue seed: fingerprint -> {display_name, app_key}. */
export function extensionCatalogue(source = readFileSync(SCHEMA, 'utf8')) {
  const start = source.indexOf('INSERT INTO ref.tool_catalogue');
  const end = source.indexOf(';', start);
  const rows = new Map();
  const re = /\(\s*'([^']+)'\s*,\s*'([^']+)'\s*,\s*'[^']*'\s*,\s*'extension'\s*,\s*'[^']*'\s*,\s*'([^']+)'\s*\)/g;
  for (const m of source.slice(start, end).matchAll(re)) rows.set(m[1], { display_name: m[2], app_key: m[3] });
  return rows;
}

/** The `requestBody` Chrome hands over: a form parsed, anything else as raw bytes. */
function requestBodyOf(request) {
  if (request.form) return { formData: request.form };
  const text = request.json !== undefined ? JSON.stringify(request.json) : request.text;
  if (text === undefined) return null;
  return { raw: [{ bytes: toArrayBuffer(new TextEncoder().encode(text)) }] };
}

function headerList(headers) {
  return Object.entries(headers || {}).map(([name, value]) => ({ name, value }));
}

/**
 * Replay one fixture in a fresh extension whose tenant reads content.
 * @returns {Promise<{observed: boolean, attributed: boolean, prompt: boolean, events: Array<{route: string, fingerprint: string, named: string|null, app_key: string|null, content: string|null}>, score: number, signals: string[]}>}
 */
export async function replay(fixture, { catalogue = extensionCatalogue() } = {}) {
  const h = createHarness();
  await h.app.start();
  h.app.applyPolicy({ policy_version: 'replay', bundle: { version: 'replay', tenant_default_mode: 'm3' } });
  await settle();
  h.core.received.length = 0;

  const { request, response } = fixture;
  const detail = {
    requestId: 'replay-1',
    url: request.url,
    method: request.method || 'POST',
    type: request.type || 'xmlhttprequest',
    tabId: 1,
    frameId: 0,
    timeStamp: 1_700_000_000_000,
    initiator: new URL(request.url).origin,
    fromCache: false,
  };
  const requestBody = requestBodyOf(request);
  if (requestBody) detail.requestBody = requestBody;
  await h.fake.drive('body', detail);
  await settle();

  const complete = h.fake.completeListener();
  if (complete && response) {
    const completion = { ...detail, statusCode: response.status };
    delete completion.requestBody;
    if (h.fake.completeExtra().includes('responseHeaders')) completion.responseHeaders = headerList(response.headers);
    complete(completion);
    await settle();
  }

  const events = h.core.received
    .filter((m) => m.type === TYPE.OBSERVATION)
    .map((m) => {
      const entry = catalogue.get(m.body.tool_fingerprint);
      return {
        route: m.body.route,
        fingerprint: m.body.tool_fingerprint,
        named: entry ? entry.display_name : null,
        app_key: entry ? entry.app_key : null,
        content: m.body.content ? Buffer.from(m.body.content, 'base64').toString('utf8') : null,
      };
    });
  const shape = predicateRequest({ url: request.url, method: detail.method, headers: {}, ...shapeBody(requestBody) });
  return {
    observed: events.length > 0,
    attributed: events.length > 0 && events.every((e) => e.app_key === fixture.app_key),
    prompt: events.length > 0 && events.every((e) => e.content === fixture.typed),
    events,
    score: shape.score,
    signals: shape.signals.map((s) => s.id),
  };
}

/** The predicate's view of a body, as the pipeline builds it. */
function shapeBody(requestBody) {
  const body = normaliseBody(requestBody);
  return {
    size_bytes: body.size,
    bytes: body.bytes,
    form: body.form,
    form_text: body.source === 'form' ? 'form' : null,
  };
}
