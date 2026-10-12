/**
 * test/submission.test.mjs — a catalogued web app's prompt request is recognised by its method and
 * path, and only its typed text is taken; anything else is left to the predicate.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { toArrayBuffer } from '../test-support/fake-chrome.mjs';
import { normaliseBody } from '../src/request-body.js';
import { knownSubmission } from '../src/submission.js';

const raw = (value) => normaliseBody({ raw: [{ bytes: toArrayBuffer(new TextEncoder().encode(typeof value === 'string' ? value : JSON.stringify(value))) }] });
const form = (fields) => normaliseBody({ formData: fields });
const post = (url) => ({ url, method: 'POST' });

const CLAUDE = 'https://claude.ai/api/organizations/org-1/chat_conversations/conv-1/completion';
const GEMINI = 'https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate?rt=c';
const PERPLEXITY = 'https://www.perplexity.ai/rest/sse/perplexity_ask';
const geminiForm = (text) => form({ 'f.req': [JSON.stringify([null, JSON.stringify([[text, 0, null], ['en']])])], at: ['token'] });

test('each catalogued app gives up the typed text alone', () => {
  assert.equal(knownSubmission('app:claude_web', post(CLAUDE), raw({ prompt: 'hello claude', model: 'm', tools: [] })), 'hello claude');
  assert.equal(knownSubmission('app:gemini_web', post(GEMINI), geminiForm('hello gemini')), 'hello gemini');
  assert.equal(knownSubmission('app:perplexity_web', post(PERPLEXITY), raw({ query_str: 'hello perplexity', params: {} })), 'hello perplexity');
});

test('a request that is not the app\'s prompt request is left to the predicate', () => {
  const body = raw({ prompt: 'hello', model: 'm' });
  assert.equal(knownSubmission('app:claude_web', post('https://claude.ai/api/organizations/org-1/chat_conversations'), body), null, 'another path');
  assert.equal(knownSubmission('app:claude_web', { url: CLAUDE, method: 'PUT' }, body), null, 'another method');
  assert.equal(knownSubmission('host:claude.ai', post(CLAUDE), body), null, 'a destination the catalog does not name');
  assert.equal(knownSubmission('app:chatgpt_web', post(CLAUDE), body), null, 'an app without a known request');
  assert.equal(knownSubmission('app:toString', post(CLAUDE), body), null, 'a key that is not an app');
});

test('a prompt request whose body does not hold the prompt where the app puts it is left to the predicate', () => {
  assert.equal(knownSubmission('app:claude_web', post(CLAUDE), raw({ model: 'm' })), null, 'no prompt member');
  assert.equal(knownSubmission('app:claude_web', post(CLAUDE), raw({ prompt: '   ' })), null, 'an empty prompt');
  assert.equal(knownSubmission('app:claude_web', post(CLAUDE), raw({ prompt: ['a'] })), null, 'a prompt that is not text');
  assert.equal(knownSubmission('app:claude_web', post(CLAUDE), raw('prompt=hello')), null, 'a body that is not JSON');
  assert.equal(knownSubmission('app:perplexity_web', post(PERPLEXITY), raw([{ query_str: 'x' }])), null, 'an array, not an object');
  assert.equal(knownSubmission('app:gemini_web', post(GEMINI), form({ 'f.req': ['not json'] })), null, 'f.req that is not JSON');
  assert.equal(knownSubmission('app:gemini_web', post(GEMINI), form({ 'f.req': [JSON.stringify([null, 'not json'])] })), null, 'an inner request that is not JSON');
  assert.equal(knownSubmission('app:gemini_web', post(GEMINI), raw({ 'f.req': 'x' })), null, 'a raw body, not a form');
});

test('a body held only up to the cap is never read for its prompt', () => {
  const capped = normaliseBody({ raw: [{ bytes: toArrayBuffer(new TextEncoder().encode(JSON.stringify({ prompt: 'x'.repeat(64) }))) }] }, { capBytes: 16 });
  assert.equal(knownSubmission('app:claude_web', post(CLAUDE), capped), null);
});
