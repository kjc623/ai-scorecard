/**
 * fixtures.mjs — request records for the predicate tests: a chat call and a draft save from the
 * same origin, distinguished by their bodies rather than their paths.
 */

import { toArrayBuffer } from './fake-chrome.mjs';

const encoder = new TextEncoder();

export function bytesOf(value) {
  return typeof value === 'string' ? encoder.encode(value) : value;
}

/** The canonical positive: a `messages` array with role/content, model parameters, POST+JSON. */
export const CHAT_BODY = {
  model: 'example-large-2026',
  messages: [
    { role: 'system', content: 'You are a helpful assistant for Example Ltd.' },
    { role: 'user', content: 'Summarise the attached quarterly figures and flag anything unusual in the margin.' },
  ],
  temperature: 0.2,
  max_tokens: 1024,
  stream: true,
};

/** A SaaS draft save: same origin, POST, JSON, a text field — and no generative structure. */
export const DRAFT_BODY = {
  draft_id: 'd-8812',
  title: 'Q3 notes',
  body: 'Some notes about the quarter that a person typed into a text field.',
  saved_at: '2026-10-02T13:00:00Z',
};

/** The same origin, a chat call whose body reveals it. */
export function chatRequest(overrides = {}) {
  const body = bytesOf(JSON.stringify(CHAT_BODY));
  return {
    url: 'https://saas.example-ai.invalid/api/messages',
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    size_bytes: body.byteLength,
    bytes: body,
    ...overrides,
  };
}

export function draftRequest(overrides = {}) {
  const body = bytesOf(JSON.stringify(DRAFT_BODY));
  return {
    url: 'https://saas.example-ai.invalid/api/drafts',
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    size_bytes: body.byteLength,
    bytes: body,
    ...overrides,
  };
}

/** A long prose body with no generative structure: the false positive the threshold must reject. */
export function longProseRequest(overrides = {}) {
  const text = 'The quarterly review covered staffing, logistics and the renewal pipeline in some depth. '.repeat(3);
  const body = bytesOf(JSON.stringify({ note_id: 'n1', text }));
  return {
    url: 'https://saas.example-ai.invalid/api/notes',
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    size_bytes: body.byteLength,
    bytes: body,
    ...overrides,
  };
}

/** A tool-declaration request: the second shape that is generative on its own. */
export function toolCallRequest(overrides = {}) {
  const body = bytesOf(
    JSON.stringify({
      model: 'example-large-2026',
      tools: [{ type: 'function', function: { name: 'search_orders', description: 'Search customer orders' } }],
      messages: [{ role: 'user', content: 'Find the last order for account 4471 and summarise it.' }],
    }),
  );
  return {
    url: 'https://agent.example-ai.invalid/v1/respond',
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    size_bytes: body.byteLength,
    bytes: body,
    ...overrides,
  };
}

/** A generative response contract: streaming framing for the same request. */
export const STREAMING_RESPONSE = {
  status: 200,
  headers: { 'content-type': 'text/event-stream', 'transfer-encoding': 'chunked' },
};

export const JSON_RESPONSE = {
  status: 200,
  headers: { 'content-type': 'application/json' },
};

/** Chrome's request-detail shape, for the lane tests. */
export function chromeDetail({ requestId = 'r1', url, method = 'POST', headers = {}, body = null, formData = null, type = 'xmlhttprequest' } = {}) {
  const requestHeaders = Object.entries(headers).map(([name, value]) => ({ name, value }));
  const detail = {
    requestId,
    url: url || 'https://example-ai.invalid/v1/chat/completions',
    method,
    type,
    tabId: 1,
    frameId: 0,
    timeStamp: 1_700_000_000_000,
    initiator: 'https://example-ai.invalid/',
    requestHeaders,
    fromCache: false,
  };
  if (body !== null) detail.requestBody = { raw: [{ bytes: toArrayBuffer(body) }] };
  if (formData !== null) detail.requestBody = { formData };
  return detail;
}
