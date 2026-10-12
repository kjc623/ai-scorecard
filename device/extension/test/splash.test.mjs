/**
 * The redirect splash page: it shows the rule's message and a button to its link, as text, and
 * offers no way on when the rule is not a redirect in force.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { renderSplash, splashParams } from '../splash/redirect.js';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const IDS = ['sp-from', 'sp-message', 'sp-go', 'sp-link'];

/** The page's elements by id, holding what a real element would: text, attributes and hidden. */
function fakePage() {
  const els = Object.fromEntries(IDS.map((id) => [id, {
    textContent: '', hidden: id === 'sp-go', attrs: {},
    setAttribute(k, v) { this.attrs[k] = String(v); },
    removeAttribute(k) { delete this.attrs[k]; },
  }]));
  return { els, document: { getElementById: (id) => els[id] } };
}

test('the page holds every element the script fills, and the manifest lets a redirect reach it', () => {
  const html = readFileSync(join(ROOT, 'splash', 'redirect.html'), 'utf8');
  for (const id of IDS) assert.match(html, new RegExp(`id="${id}"`));
  assert.match(html, /<script type="module" src="redirect.js"><\/script>/, 'no inline script: an extension page refuses one');
  const manifest = JSON.parse(readFileSync(join(ROOT, 'manifest.json'), 'utf8'));
  const exposed = manifest.web_accessible_resources.flatMap((r) => r.resources);
  for (const f of ['splash/redirect.html', 'splash/redirect.css', 'splash/redirect.js']) assert.ok(exposed.includes(f), `${f} is web-accessible`);
});

test('a redirect rule shows its message, where the person was going, and a button to its link', () => {
  const { els, document } = fakePage();
  renderSplash(document, { message: 'Use Copilot for work. <b>Really</b>.', link: 'https://copilot.example/start?from=chatgpt' }, 'chatgpt.com');
  assert.equal(els['sp-from'].textContent, 'You tried to open chatgpt.com.');
  assert.equal(els['sp-message'].textContent, 'Use Copilot for work. <b>Really</b>.', 'the message is text, never markup');
  assert.equal(els['sp-go'].hidden, false);
  assert.equal(els['sp-go'].attrs.href, 'https://copilot.example/start?from=chatgpt');
  assert.equal(els['sp-go'].textContent, 'Continue to copilot.example');
  assert.equal(els['sp-link'].textContent, 'https://copilot.example/start?from=chatgpt');
});

test('without a redirect rule in force, or with a link that is not https, there is no way on', () => {
  for (const spec of [null, { message: 'x', link: 'javascript:alert(1)' }, { message: 'x', link: 'http://copilot.example/' }]) {
    const { els, document } = fakePage();
    renderSplash(document, spec, '');
    assert.equal(els['sp-go'].hidden, true);
    assert.equal(els['sp-go'].attrs.href, undefined);
    assert.equal(els['sp-link'].textContent, '');
    assert.equal(els['sp-from'].textContent, '');
  }
  const { els, document } = fakePage();
  renderSplash(document, null, 'chatgpt.com');
  assert.equal(els['sp-message'].textContent, 'Your organisation does not allow this app.');
});

test('the address carries the rule id and the host, and nothing else is read from it', () => {
  assert.deepEqual(splashParams('?rule=example_to_copilot&from=chat.example-ai.invalid&message=evil&link=https%3A%2F%2Fevil.example'),
    { rule_id: 'example_to_copilot', from: 'chat.example-ai.invalid' });
  assert.deepEqual(splashParams(''), { rule_id: '', from: '' });
});
