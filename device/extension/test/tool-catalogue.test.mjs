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
    ['Microsoft Copilot', 'https://copilot.microsoft.com/c/api/chat'],
    ['Microsoft Copilot', 'https://www.copilot.microsoft.com/'],
    ['Microsoft 365 Copilot', 'https://copilot.cloud.microsoft/chat'],
    ['Microsoft 365 Copilot', 'https://m365.cloud.microsoft/chat'],
  ]) {
    assert.equal(named(url), name, url);
  }
});

test('the Microsoft 365 Copilot socket host carries no name of its own: substrate.office.com is shared', () => {
  // The chat turn on wss://substrate.office.com is attributed by the socket recipe, not by the host,
  // because Office shares substrate.office.com across services. A plain request there stays unnamed,
  // so nothing else on that host is mislabelled Copilot.
  assert.equal(toolFingerprint(catalog, 'https://substrate.office.com/m365Copilot/Chathub'), 'host:substrate.office.com');
  assert.equal(named('https://substrate.office.com/m365Copilot/Chathub'), null);
});

test('the Copilot endpoints decrypted at the proxy are named, GitHub and Microsoft kept apart', () => {
  // tls rows are the proxy's view: ToolFingerprint(host, path) in device/capture-core/proxy/tlsproxy.
  // copilot.microsoft.com is consumer Microsoft Copilot, not GitHub Copilot; the GitHub Business and
  // Enterprise plan hosts are GitHub Copilot, named like the individual plan's host.
  for (const [host, path, name, app_key] of [
    ['copilot.microsoft.com', '/chat/completions', 'Microsoft Copilot', 'microsoft_copilot'],
    ['api.githubcopilot.com', '/chat/completions', 'GitHub Copilot', 'github_copilot'],
    ['api.business.githubcopilot.com', '/chat/completions', 'GitHub Copilot', 'github_copilot'],
    ['api.enterprise.githubcopilot.com', '/chat/completions', 'GitHub Copilot', 'github_copilot'],
  ]) {
    const row = catalogue.get(tls(host, path));
    assert.ok(row, `${host} has a tls row`);
    assert.equal(row.display_name, name, host);
    assert.equal(row.app_key, app_key, host);
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

/** "tls_" + first 8 bytes of sha256("tls|host|path"), the proxy's ToolFingerprint for a decrypted request. */
function tls(host, path) {
  const digest = createHash('sha256').update(`tls|${host.toLowerCase()}|${path.replace(/^\/+|\/+$/g, '')}`).digest('hex');
  return `tls_${digest.slice(0, 16)}`;
}

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
