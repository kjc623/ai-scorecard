#!/usr/bin/env node
// Turns the request a tool's page sent, saved from the browser's DevTools as a HAR file, into a
// capture fixture in test/tools/.
//
//   node tools/har-to-fixture.mjs <capture.har> <fixture-name> "<the prompt you typed>"
//
// The request chosen is the POST whose body holds the typed prompt. Only its URL, method, body and
// the response's status and content type are kept: no request or response header, cookie or
// response body is written. The fixture is marked `captured`; an existing fixture of that name
// keeps its tool, app_key and expectation, so the test then shows whether the documented
// expectation holds for the real traffic. A new fixture's expectation must be filled in by hand.

import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

import { FIXTURES, extensionCatalogue, replay } from '../test-support/tool-replay.mjs';

const [harPath, name, typed] = process.argv.slice(2);
if (!harPath || !name || !typed) {
  console.error('usage: node tools/har-to-fixture.mjs <capture.har> <fixture-name> "<the prompt you typed>"');
  process.exit(2);
}

const har = JSON.parse(readFileSync(harPath, 'utf8').replace(/^﻿/, ''));
const entries = (har.log && har.log.entries) || [];
const candidates = entries.filter((e) => e.request.method === 'POST' && bodyText(e.request).includes(typed));
if (candidates.length === 0) {
  console.error(`no POST in ${harPath} carries "${typed}"; check the prompt is typed exactly as sent`);
  process.exit(1);
}
const entry = candidates[0];
if (candidates.length > 1) console.error(`${candidates.length} requests carry the prompt; using the first, ${entry.request.url}`);

const path = join(FIXTURES, `${name}.json`);
const existing = existsSync(path) ? JSON.parse(readFileSync(path, 'utf8')) : null;
const fixture = {
  tool: existing ? existing.tool : name,
  app_key: existing ? existing.app_key : name.replace(/-/g, '_'),
  source: { kind: 'captured', from: `a HAR saved from ${har.log?.browser?.name || 'the browser'} ${har.log?.browser?.version || ''}`.trim(), checked: new Date().toISOString().slice(0, 10) },
  typed,
  request: { url: entry.request.url, method: entry.request.method, ...requestBody(entry.request) },
  response: { status: entry.response.status, headers: { 'content-type': contentTypeOf(entry.response.headers) } },
  expect: existing ? existing.expect : { events: 0, observed: false, attributed: false, prompt: false, why: 'fill in from the replay below' },
};
writeFileSync(path, `${JSON.stringify(fixture, null, 2)}\n`);

const got = await replay(fixture, { catalogue: extensionCatalogue() });
console.log(`wrote ${path}`);
console.log(`replay: score ${got.score} [${got.signals.join(', ')}]`);
for (const e of got.events) console.log(`  ${e.route} -> ${e.named || 'not catalogued'}: ${JSON.stringify((e.content || '').slice(0, 120))}`);
console.log(`events ${got.events.length}, observed ${got.observed}, attributed ${got.attributed}, prompt as typed ${got.prompt}`);

function bodyText(request) {
  const post = request.postData;
  if (!post) return '';
  if (typeof post.text === 'string') return post.text + (post.params || []).map((p) => p.value || '').join('');
  return (post.params || []).map((p) => decodeURIComponent(p.value || '')).join('');
}

/** The body as Chrome hands it to the extension: a parsed form, JSON, or text. */
function requestBody(request) {
  const post = request.postData || {};
  const mime = String(post.mimeType || '').toLowerCase();
  if (mime.startsWith('application/x-www-form-urlencoded')) {
    const form = {};
    for (const [k, v] of new URLSearchParams(post.text || '')) (form[k] ||= []).push(v);
    return { form };
  }
  if (mime.startsWith('multipart/form-data')) {
    const form = {};
    for (const p of post.params || []) (form[p.name] ||= []).push(p.fileName || p.value || '');
    return { form };
  }
  try {
    return { json: JSON.parse(post.text) };
  } catch {
    return { text: post.text || '' };
  }
}

function contentTypeOf(headers) {
  const h = (headers || []).find((x) => x.name.toLowerCase() === 'content-type');
  return h ? h.value : '';
}
