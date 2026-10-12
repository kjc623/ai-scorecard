/**
 * test/tool-catalogue.test.mjs — the web tools the extension observes are named from the shared
 * catalogue, and the names cannot silently drift from what the extension emits.
 *
 * The extension names a request after the app whose inference domain covers its host, from the app
 * catalog in the policy bundle. This file runs the extension's own resolver over the catalog seeded
 * in services/database/schema.sql, and looks the result up in the tool catalogue seeded there: every
 * web tool's hosts, a host under a tool's domain that no row lists, a domain an installed app shares
 * with a web service, and a host no app claims. It also checks that every `tf2:` row is the
 * fingerprint of the host its evidence names, so the prompts stored under it keep their tool.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';

import { toolFingerprint } from '../src/tool-fingerprint.js';
import { bundleCatalog, toolCatalogue } from '../test-support/tool-replay.mjs';

const catalog = bundleCatalog();
const catalogue = toolCatalogue();

/** The display name the dashboard shows for a request to `url`, or null when nothing names it. */
function named(url, apps = catalog) {
  return catalogue.get(toolFingerprint(apps, url))?.display_name ?? null;
}

test('every web tool is named at each of its hosts, including one no row lists', () => {
  for (const [name, url] of [
    ['ChatGPT (web)', 'https://chatgpt.com/backend-api/f/conversation'],
    ['ChatGPT (web)', 'https://m.chatgpt.com/backend-api/f/conversation'],
    ['ChatGPT (web)', 'https://any-new-host.chatgpt.com/backend-api/some/new/path'],
    ['ChatGPT (web)', 'https://chat.openai.com/backend-api/conversation'],
    ['Gemini (web)', 'https://gemini.google.com/_/BardChatUi/data/assistant.lamda.BardFrontendService/StreamGenerate'],
    ['Perplexity (web)', 'https://www.perplexity.ai/rest/sse/perplexity_ask'],
    ['Perplexity (web)', 'https://perplexity.ai/rest/sse/perplexity_ask'],
  ]) {
    assert.equal(named(url), name, url);
  }
});

test('a web tool is named by its domain: without it, a host the catalogue does not list is unnamed', () => {
  const without = catalog.map((app) => app.app_key !== 'chatgpt_web' ? app
    : { ...app, signals: app.signals.filter((s) => s.value !== '.chatgpt.com').concat({ platform: 'any', kind: 'inference_domain', value: 'chatgpt.com' }) });
  assert.equal(named('https://chatgpt.com/', without), 'ChatGPT (web)', 'the exact host still names it');
  assert.equal(toolFingerprint(without, 'https://m.chatgpt.com/backend-api/f/conversation'), 'host:m.chatgpt.com');
  assert.equal(named('https://m.chatgpt.com/backend-api/f/conversation', without), null, 'a host under the domain is not ChatGPT without the domain');
});

test('a browser request is named after the web service, never an installed app that shares its domain', () => {
  assert.equal(toolFingerprint(catalog, 'https://chatgpt.com/backend-api/f/conversation'), 'app:chatgpt_web', 'ChatGPT Desktop also lists chatgpt.com');
  assert.equal(toolFingerprint(catalog, 'https://api.anthropic.com/v1/messages'), 'app:anthropic_api', 'Claude Code also lists api.anthropic.com');
  assert.equal(toolFingerprint(catalog, 'https://api.perplexity.ai/chat/completions'), 'app:perplexity_api', 'an exact host beats a domain');
  assert.equal(toolFingerprint(catalog, 'https://claude.ai/api/organizations/o/chat_conversations/c/completion'), 'app:claude_web',
    'Claude Desktop also lists claude.ai');
});

test('a host no app claims is named by itself', () => {
  assert.equal(toolFingerprint(catalog, 'https://chat.example-ai.invalid/v1/chat/completions'), 'host:chat.example-ai.invalid');
  assert.equal(toolFingerprint(catalog, 'https://Chat.Example-AI.invalid.:8443/v1/chat'), 'host:chat.example-ai.invalid', 'lower-case, without a port or a trailing dot');
  assert.equal(toolFingerprint([], 'https://chatgpt.com/'), 'host:chatgpt.com', 'with no catalog in force');
  const long = `${'a'.repeat(60)}.${'b'.repeat(60)}.example-ai.invalid`;
  assert.equal(toolFingerprint(catalog, `https://${long}/`), 'host:example-ai.invalid', 'a host too long for a fingerprint is named by its registered domain');
});

test('every tf2 row is the fingerprint of the host its evidence names', () => {
  const rows = [...catalogue].filter(([fingerprint]) => fingerprint.startsWith('tf2:'));
  assert.ok(rows.some(([, row]) => row.evidence.host === 'm.chatgpt.com'), 'the prompts stored under m.chatgpt.com are named');
  for (const [fingerprint, row] of rows) {
    assert.equal(row.signal_kind, 'extension', fingerprint);
    assert.equal(tf2(row.evidence.host), fingerprint, `${row.display_name} at ${row.evidence.host}`);
  }
});

/** "tf2:" + base32(sha256(canonical({v: 2, host}))): the fingerprint of a prompt stored by its host. */
function tf2(host) {
  const digest = createHash('sha256').update(JSON.stringify({ host, v: 2 })).digest();
  const alphabet = 'abcdefghijklmnopqrstuvwxyz234567';
  let out = '';
  let acc = 0;
  let bits = 0;
  for (const byte of digest) {
    acc = (acc << 8) | byte;
    bits += 8;
    while (bits >= 5) {
      bits -= 5;
      out += alphabet[(acc >> bits) & 31];
    }
  }
  if (bits > 0) out += alphabet[(acc << (5 - bits)) & 31];
  return `tf2:${out}`;
}
