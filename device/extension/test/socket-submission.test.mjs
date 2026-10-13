/**
 * test/socket-submission.test.mjs — the socket recognisers read a Copilot chat turn out of the
 * frame the page sent, and read nothing out of anything else.
 *
 * `socketSubmission` is the whole of what the main-world hook relies on: it names the socket by host
 * and path and returns the typed prompt, or null. The frames a Copilot socket carries are mostly not
 * chat turns (a handshake, options, a ping, a stop, the streamed answer), and a frame from a socket
 * no recipe names is never read. Each of those is checked here, because a recogniser that is too
 * eager stores the wrong text and one that is too shy stores none.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { socketSubmission } from '../src/socket-submission.js';

const RS = '\u001e';
const CLASSIC = 'wss://copilot.microsoft.com/c/api/chat?source=web';
const M365 = 'wss://substrate.office.com/m365Copilot/Chathub?source=bizchat';

test('consumer Copilot: the send frame yields its typed text, attributed to microsoft_copilot', () => {
  const frame = JSON.stringify({ event: 'send', conversationId: 'abc', mode: 'chat', content: [{ type: 'text', text: 'book me a flight to Lisbon' }] });
  assert.deepEqual(socketSubmission(CLASSIC, frame), { app_key: 'microsoft_copilot', prompt: 'book me a flight to Lisbon' });
});

test('consumer Copilot: option and non-send frames are not a turn', () => {
  for (const obj of [
    { event: 'setOptions', optionsSets: ['copilot'] },
    { event: 'challengeResponse', token: 'x' },
    { event: 'send', content: [] },
    { event: 'send', content: [{ type: 'image', url: 'blob:x' }] },
  ]) {
    assert.equal(socketSubmission(CLASSIC, JSON.stringify(obj)), null, JSON.stringify(obj));
  }
});

test('consumer Copilot: the text parts of a mixed-content send are joined, trimmed', () => {
  const frame = JSON.stringify({ event: 'send', content: [{ type: 'image', url: 'blob:x' }, { type: 'text', text: '  describe ' }, { type: 'text', text: 'this photo  ' }] });
  assert.deepEqual(socketSubmission(CLASSIC, frame), { app_key: 'microsoft_copilot', prompt: 'describe this photo' });
});

test('Microsoft 365 Copilot: a type-4 chat invocation yields arguments[0].message.text', () => {
  const frame = JSON.stringify({ type: 4, invocationId: '0', target: 'chat', arguments: [{ source: 'BizChat', message: { text: 'summarise the Q3 deck' } }] }) + RS;
  assert.deepEqual(socketSubmission(M365, frame), { app_key: 'm365_copilot', prompt: 'summarise the Q3 deck' });
});

test('Microsoft 365 Copilot: the handshake, a ping, a stop and other targets are not a turn', () => {
  for (const frame of [
    '{"protocol":"json","version":1}' + RS,
    JSON.stringify({ type: 6 }) + RS,
    JSON.stringify({ type: 5, invocationId: '0' }) + RS,
    JSON.stringify({ type: 4, target: 'typing', arguments: [{ message: { text: 'x' } }] }) + RS,
    JSON.stringify({ type: 1, target: 'chat', arguments: [{ message: { text: 'x' } }] }) + RS,
  ]) {
    assert.equal(socketSubmission(M365, frame), null, frame);
  }
});

test('Microsoft 365 Copilot: the chat turn is found among several record-separated frames in one message', () => {
  const message = [
    JSON.stringify({ type: 6 }),
    JSON.stringify({ type: 4, target: 'chat', arguments: [{ message: { text: 'what changed this week' } }] }),
    '',
  ].join(RS);
  assert.deepEqual(socketSubmission(M365, message), { app_key: 'm365_copilot', prompt: 'what changed this week' });
});

test('a bad or empty frame, and a socket no recipe names, are read as nothing', () => {
  assert.equal(socketSubmission(CLASSIC, 'not json'), null);
  assert.equal(socketSubmission(CLASSIC, ''), null);
  assert.equal(socketSubmission(CLASSIC, RS + RS), null);
  assert.equal(socketSubmission('wss://chat.example.invalid/ws', JSON.stringify({ event: 'send', content: [{ type: 'text', text: 'x' }] })), null, 'unknown host');
  assert.equal(socketSubmission('wss://copilot.microsoft.com/other/path', JSON.stringify({ event: 'send', content: [{ type: 'text', text: 'x' }] })), null, 'wrong path');
  assert.equal(socketSubmission('not a url', 'x'), null);
  assert.equal(socketSubmission(CLASSIC, { event: 'send' }), null, 'a non-string frame is a binary frame, not a chat turn');
});
