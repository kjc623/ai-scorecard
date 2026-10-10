/**
 * test/predicate.test.mjs — the shape predicate as a pure function.
 *
 *   - a positive match needs enough evidence, and the two strong shapes match on their own;
 *   - a negative match is counted, not emitted: this file proves the predicate returns false, the
 *     worker test proves `skipped_not_generative` moves and nothing is sent;
 *   - a draft save and a chat call on one origin differ by body, not path;
 *   - destination and path are evidence, never sufficient.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import {
  CANDIDATE_FLOOR,
  DEFAULT_THRESHOLD,
  LONG_TEXT_CHARS,
  STRUCTURAL_FLOOR,
  classifyWithResponse,
  formFileCandidates,
  hostOf,
  isUploadBearing,
  normalisePath,
  predicateRequest,
  predicateResponse,
  registeredDomain,
} from '../src/predicate.js';
import { tabContextOf } from '../src/registration.js';
import {
  CHAT_BODY,
  DRAFT_BODY,
  JSON_RESPONSE,
  STREAMING_RESPONSE,
  chatRequest,
  draftRequest,
  longProseRequest,
  toolCallRequest,
} from '../test-support/fixtures.mjs';

test('a chat-shaped submission matches, and the evidence is named', () => {
  const r = predicateRequest(chatRequest());
  assert.equal(r.match, true, `score ${r.score} should clear ${r.threshold}`);
  const ids = r.signals.map((s) => s.id);
  assert.ok(ids.includes('body_messages_array'), 'the ordered role/content array is the strongest signal');
  assert.ok(ids.includes('body_role_discriminator'));
  assert.ok(ids.includes('body_content_payload'));
  assert.ok(ids.includes('body_model_params'));
  assert.equal(r.structural, true, 'a messages array is a generative request structure on its own');
  assert.equal(r.structure, 'object');
  assert.equal(r.messages.count, 2);
  assert.ok(r.messages.role_values.includes('system'));
});

test('a draft save on the same origin does NOT match — the separation is the body, not the path', () => {
  const r = predicateRequest(draftRequest());
  assert.equal(r.match, false, `score ${r.score} must stay below ${r.threshold}`);
  assert.equal(r.structural, false);
  assert.equal(r.messages, null);
});

test('a long prose note does not match: text alone is also the shape of a form field', () => {
  const req = longProseRequest();
  const parsed = JSON.parse(new TextDecoder().decode(req.bytes));
  assert.ok(parsed.text.length >= LONG_TEXT_CHARS, 'the fixture must actually be long enough to be tempting');
  const r = predicateRequest(req);
  assert.equal(r.match, false);
  assert.ok(r.signals.some((s) => s.id === 'body_long_text'), 'the text signal fires');
  assert.ok(r.signals.some((s) => s.id === 'request_post'));
  assert.ok(r.signals.some((s) => s.id === 'request_json_content_type'));
  assert.ok(r.score < DEFAULT_THRESHOLD, 'the three weak signals together still must not reach the threshold');
});

test('a tool-declaration body is generative on request structure alone', () => {
  const r = predicateRequest(toolCallRequest());
  assert.equal(r.match, true);
  assert.equal(r.structural, true);
  assert.ok(r.signals.some((s) => s.id === 'body_tool_declarations'));
});

test('a destination in the bundle is evidence, never sufficient', () => {
  const req = { ...longProseRequest(), bundle: { sanctioned: ['saas.example-ai.invalid'] } };
  const r = predicateRequest(req);
  assert.ok(r.signals.some((s) => s.id === 'destination_sanctioned_set'), 'hostname is a signal');
  assert.equal(r.match, false, 'a sanctioned host must not turn a draft save into a submission');
});

test('a conversational path is evidence, never sufficient', () => {
  const tiny = {
    url: 'https://unknown.invalid/chat/completions',
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    size_bytes: 8,
    bytes: new TextEncoder().encode('{"a": 1}'),
  };
  const r = predicateRequest(tiny);
  assert.ok(r.signals.some((s) => s.id === 'path_conversational_vocabulary'));
  assert.equal(r.match, false, 'the path alone must not decide');
  assert.equal(r.path_signal, '/chat/completions');
});

test('a GET with no body never matches, however conversational the path', () => {
  const r = predicateRequest({ url: 'https://example-ai.invalid/v1/messages', method: 'GET', headers: {}, size_bytes: 0, bytes: null });
  assert.equal(r.match, false);
  assert.equal(r.metadata_candidate, false);
});

test('a weak request only becomes a match when the response contract agrees', () => {
  // A prose body alone is below threshold; the streaming contract is the second half of the
  // conjunction. This is the row that separates an in-SaaS AI feature from a draft save.
  const req = longProseRequest({ size_bytes: 400 });
  const withoutResponse = classifyWithResponse(req, JSON_RESPONSE);
  assert.equal(withoutResponse.match, false);

  const withStreaming = classifyWithResponse(req, STREAMING_RESPONSE);
  assert.equal(withStreaming.match, true, 'the streaming contract must be able to carry it over the line');
  assert.equal(withStreaming.reason, 'request_plus_response_contract');
  assert.ok(withStreaming.response_score > 0);
});

test('a request that is nowhere near the candidate floor is not rescued by a response contract', () => {
  const noise = {
    url: 'https://tiles.example.invalid/img/1.png',
    method: 'POST',
    headers: { 'content-type': 'image/png' },
    size_bytes: 3,
    bytes: new TextEncoder().encode('abc'),
  };
  const r = classifyWithResponse(noise, STREAMING_RESPONSE);
  assert.equal(r.match, false, `score ${r.score} is below the candidate floor ${CANDIDATE_FLOOR}`);
  assert.equal(r.reason, 'below_threshold');
});

test('a structural match is accepted without response corroboration', () => {
  const r = classifyWithResponse(chatRequest(), JSON_RESPONSE);
  assert.equal(r.match, true);
  assert.equal(r.reason, 'request_structure');
  assert.ok(r.score >= STRUCTURAL_FLOOR);
});

test('predicateResponse reads the response contract, never the response body', () => {
  const s = predicateResponse(STREAMING_RESPONSE);
  assert.equal(s.streaming, true);
  assert.deepEqual(s.signals.map((x) => x.id), ['response_streaming_contract', 'response_chunked_framing']);
  const completion = predicateResponse({ status: 200, headers: { 'content-type': 'application/json', 'openai-processing-ms': '120' } });
  assert.ok(completion.signals.some((x) => x.id === 'response_completion_hook'));
  assert.equal(completion.streaming, false);
  const none = predicateResponse({ status: 500, headers: { 'content-type': 'text/html' } });
  assert.equal(none.score, 0);
  assert.deepEqual(none.signals, []);
});

test('an over-cap body contributes what a prefix can honestly contribute, and says so', () => {
  const truncated = new TextEncoder().encode('{"messages":[{"role":"user","content":"a very long pro');
  const r = predicateRequest({
    url: 'https://example-ai.invalid/v1/chat',
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    size_bytes: 8 * 1024 * 1024,
    bytes: truncated,
    truncated: true,
  });
  assert.equal(r.over_cap, true, 'the caller must be able to emit confidence: degraded');
  assert.equal(r.structure, 'unparsed', 'the prefix is not a complete JSON document');
  assert.ok(r.signals.some((s) => s.id === 'body_messages_named_member'), 'the prefix key scan finds the chat member');
  assert.ok(r.signals.some((s) => s.id === 'body_role_discriminator'));
  assert.ok(r.signals.some((s) => s.id === 'body_content_payload'));
  assert.ok(r.signals.some((s) => s.id === 'request_non_trivial_size'));
  assert.equal(r.match, true, 'and a truncated chat payload is still a submission');
});

test('an invalid-UTF-8 body contributes KEY evidence but never value evidence', () => {
  // The distinction that matters: shape is carried by ASCII key names, which a replacement
  // character cannot corrupt, while the payload's *values* are only ever a digest and a size.
  // So the key scan runs, and no text evidence is invented from bytes that were never decoded.
  const r = predicateRequest({
    url: 'https://example-ai.invalid/v1/chat',
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    size_bytes: 8,
    bytes: new Uint8Array([0xc3, 0x28, 0xc3, 0x28, 0xc3, 0x28, 0xc3, 0x28]),
  });
  assert.equal(r.match, false, 'undecodable noise with no structure is not a submission');
  assert.equal(r.structure, 'binary');
  assert.equal(r.signals.filter((s) => s.id === 'body_long_text').length, 0, 'no text evidence is invented');

  const structured = predicateRequest({
    url: 'https://example-ai.invalid/v1/chat',
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    size_bytes: 40,
    bytes: new Uint8Array([
      ...new TextEncoder().encode('{"messages":[{"role":"user","content":"'),
      0xc3, 0x28, 0xff, 0xfe,
      ...new TextEncoder().encode('"}]}'),
    ]),
  });
  assert.equal(structured.structure, 'binary');
  assert.ok(structured.signals.some((s) => s.id === 'body_messages_named_member'), 'the key is still readable');
  assert.equal(structured.match, true, 'and a chat-shaped payload with undecodable bytes is still a submission');
});

test('form bodies are classified from parsed key/value pairs', () => {
  const body = new URLSearchParams({ prompt: 'x'.repeat(200), model: 'example-large-2026' }).toString();
  const r = predicateRequest({
    url: 'https://example-ai.invalid/api/generate',
    method: 'POST',
    headers: { 'content-type': 'application/x-www-form-urlencoded' },
    size_bytes: body.length,
    bytes: new TextEncoder().encode(body),
    form: { prompt: ['x'.repeat(200)], model: ['example-large-2026'] },
    form_text: body,
  });
  assert.equal(r.structure, 'form');
  const ids = r.signals.map((s) => s.id);
  assert.ok(ids.includes('body_prompt_member'));
  assert.ok(ids.includes('body_model_params'));
  assert.ok(ids.includes('body_long_text'));
});

test('upload-bearing detection: multipart, and a form value that carries a filename', () => {
  assert.equal(isUploadBearing({ headers: { 'content-type': 'multipart/form-data; boundary=x' }, form: null }), true);
  const candidates = formFileCandidates({ file: ['report.pdf'], note: ['hello'] });
  assert.deepEqual(candidates, [{ field: 'file', name: 'report.pdf' }]);
  assert.equal(isUploadBearing({ headers: {}, form: { file: ['report.pdf'] } }), true);
  assert.equal(isUploadBearing({ headers: { 'content-type': 'application/json' }, form: null }), false);
});

test('a form carrying a filename is reported on the predicate result as an attachment candidate', () => {
  const r = predicateRequest({
    url: 'https://example-ai.invalid/api/upload',
    method: 'POST',
    headers: { 'content-type': 'multipart/form-data; boundary=x' },
    size_bytes: 120,
    bytes: new TextEncoder().encode('------x'),
    form: { file: ['quarterly.xlsx'], prompt: ['summarise this'] },
    form_text: 'file=quarterly.xlsx&prompt=summarise this',
  });
  assert.equal(r.upload_bearing, true);
  assert.deepEqual(r.attachment_candidates, [{ field: 'file', name: 'quarterly.xlsx' }]);
});

test('path shapes are normalised so two sessions on one tool agree', () => {
  assert.equal(normalisePath('/v1/c/8f3a1b2c-1111-2222-3333-444455556666/chat'), '/v1/c/:uuid/chat');
  assert.equal(normalisePath('/v1/c/12345/chat'), '/v1/c/:n/chat');
  assert.equal(
    normalisePath('/v1/c/12345/chat'),
    normalisePath('/v1/c/67890/chat'),
    'an 8f3…/91a… pair must collapse to one shape, which is what makes the fingerprint stable',
  );
  assert.equal(
    normalisePath('/v1/c/8f3a1b2c-1111-2222-3333-444455556666/chat'),
    normalisePath('/v1/c/91a2b3c4-5555-6666-7777-888899990000/chat'),
  );
  // The normalisation must not erase the part that identifies the endpoint.
  assert.notEqual(normalisePath('/v1/c/12345/chat'), normalisePath('/v1/c/12345/embed'));
});

test('registered domain collapses subdomains so one tool yields one destination', () => {
  assert.equal(registeredDomain('chat.example-ai.invalid'), 'example-ai.invalid');
  assert.equal(registeredDomain('api.chat.example-ai.invalid'), 'example-ai.invalid');
  assert.equal(registeredDomain('example.co.uk'), 'example.co.uk');
  assert.equal(registeredDomain('a.example.co.uk'), 'example.co.uk');
  assert.equal(hostOf('https://Chat.Example-INVALID/x'), 'chat.example-invalid');
});

test('the automation marker is read from request headers, and is never required', () => {
  assert.deepEqual(tabContextOf({ requestHeaders: { 'x-automation': '1' } }), ['automation_marker']);
  assert.deepEqual(tabContextOf({ requestHeaders: { 'user-agent': 'Mozilla/5.0 HeadlessChrome/120' } }), ['automation_marker']);
  assert.deepEqual(tabContextOf({ requestHeaders: { 'user-agent': 'Mozilla/5.0 Chrome/120' } }), []);
});

test('the predicate is pure: the same record gives the same score twice', () => {
  const req = chatRequest();
  const a = predicateRequest(req);
  const b = predicateRequest(req);
  assert.equal(a.score, b.score);
  assert.deepEqual(a.signals, b.signals);
});

test('the fixture bodies are what the test names claim', () => {
  assert.ok(CHAT_BODY.messages.length >= 2);
  assert.equal(typeof DRAFT_BODY.body, 'string');
  assert.ok(DRAFT_BODY.body.length < LONG_TEXT_CHARS, 'the draft fixture must not accidentally be a long-text case');
});
