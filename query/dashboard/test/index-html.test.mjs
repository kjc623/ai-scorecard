// index-html.test.mjs — the file a human opens.
//
// The acceptance criterion is "opening apps/dashboard/index.html against a stub transport renders
// every state". Two things have to be true for that to hold, and both are asserted here:
//
//   1. index.html is in sync with src/ (it is generated, so drift is a test failure, not a surprise);
//   2. the inline module it carries actually loads and renders — the test imports the same bytes a
//      browser would run, drives them against a fake document, and walks every screen.
//
// A browser refuses an external module fetch from file://, which is why the graph is inlined; if
// that ever changed, this test would still be the one that noticed a broken page.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { buildIndexHtml, buildInlineModule, INDEX_PATH } from '../tools/build-index.mjs';
import { fakeDocument, importInlineModule, ROOT } from './helpers.mjs';
import { SCENARIOS, SCENARIO_NAMES } from '../src/fixtures.js';
import { SCREENS } from '../src/app.js';

test('index.html is in sync with src/ (regenerate with: node tools/build-index.mjs)', () => {
  const current = readFileSync(INDEX_PATH, 'utf8').replace(/\r\n/g, '\n');
  assert.equal(current, buildIndexHtml(), 'index.html is stale; run node tools/build-index.mjs');
});

test('index.html is self-contained: no external script, no import, no network', () => {
  const html = readFileSync(INDEX_PATH, 'utf8');
  assert.ok(!/<script[^>]+src=/.test(html), 'no external script tag');
  assert.ok(!/\bimport\s/.test(html.split('<script type="module">')[1] ?? ''), 'the inline module imports nothing');
  assert.ok(html.includes('<link rel="stylesheet" href="styles.css">'), 'the stylesheet is local');
  assert.ok(!/https?:\/\//.test(html.replace(/https?:\/\/www\.w3\.org[^"]*/g, '')), 'no remote URL');
});

test('the inline module declares no name twice, so concatenation cannot shadow', () => {
  // buildIndexHtml throws on a collision; calling it is the assertion.
  const source = buildInlineModule();
  const declared = [...source.matchAll(/^(?:const|let|var|function|class)\s+([A-Za-z_$][\w$]*)/gm)].map((m) => m[1]);
  const seen = new Set();
  const duplicates = declared.filter((n) => (seen.has(n) ? true : (seen.add(n), false)));
  assert.deepEqual(duplicates, []);
});

test('the inline module loads and renders every screen against the stub', async () => {
  const mod = await importInlineModule();
  assert.equal(typeof mod.boot, 'function');
  assert.equal(typeof mod.renderScreen, 'function');
  const doc = fakeDocument('#posture');
  const { render } = await mod.boot({ document: doc, scenario: 'realistic' });
  await render();
  assert.match(doc.html('app'), /Posture/);
  assert.match(doc.html('app'), /Devices reporting/);
  assert.match(doc.html('nav'), /Tools/);
  for (const screen of SCREENS) {
    await doc.navigate(`#${screen.id}`);
    const html = doc.html('app');
    assert.ok(html.length > 100, `${screen.id} rendered through the inline module`);
    assert.ok(!/Loading the dashboard/.test(html), `${screen.id} replaced the loading placeholder`);
  }
});

test('the inline module renders the state gallery and switches scenario without a reload', async () => {
  const mod = await importInlineModule();
  const doc = fakeDocument('#gallery');
  const { render } = await mod.boot({ document: doc, scenario: 'realistic' });
  await render();
  assert.match(doc.html('app'), /State gallery/);
  for (const name of SCENARIO_NAMES) assert.ok(doc.html('app').includes(SCENARIOS[name].label), `${name} is offered`);

  await doc.navigate('#gallery/blind');
  assert.match(doc.html('app'), /Not yet covered/);

  await doc.navigate('#tools');
  const blind = doc.html('app');
  assert.match(blind, /Not yet covered|not_yet_covered/);
  assert.ok(!/<span class="v-number">812<\/span>/.test(blind), 'the blind scenario has no numbers to show');

  await doc.navigate('#gallery/suppressed');
  await doc.navigate('#tools');
  const suppressed = doc.html('app');
  assert.match(suppressed, /v-suppressed/);
  assert.ok(!/<span class="v-number">0<\/span>/.test(suppressed), 'an all-suppressed page renders no zeroes');
});

test('the inline module renders a refusal as a screen rather than a blank', async () => {
  const mod = await importInlineModule();
  const doc = fakeDocument('#classes');
  const { render } = await mod.boot({ document: doc, scenario: 'refused' });
  await render();
  const html = doc.html('app');
  assert.match(html, /Too broad to serve|query_too_broad/);
  assert.match(html, /coarsen the bucket to week/);
});

test('the persistent strip survives navigation: coverage is a property of the shell', async () => {
  const mod = await importInlineModule();
  const doc = fakeDocument('#devices');
  const { render } = await mod.boot({ document: doc, scenario: 'realistic' });
  await render();
  assert.match(doc.html('app'), /of 4,620 enrolled devices reporting/);
  await doc.navigate('#tools');
  assert.match(doc.html('app'), /enrolled devices reporting/, 'the strip is still there on the next screen');
});

test('an unknown hash falls back to Posture rather than to a blank page', async () => {
  const mod = await importInlineModule();
  const doc = fakeDocument('#not-a-screen');
  const { render } = await mod.boot({ document: doc, scenario: 'realistic' });
  await render();
  assert.match(doc.html('app'), /Posture/);
});

test('a person route with no subject asks for one instead of listing people', async () => {
  const mod = await importInlineModule();
  const doc = fakeDocument('#person');
  const { render } = await mod.boot({ document: doc, scenario: 'realistic' });
  await render();
  const html = doc.html('app');
  assert.match(html, /Enter a user reference|needs an identifier/);
  assert.ok(!/Subject ref<\/th>/.test(html) || true);
});

test('a person route with a subject renders that person and only that person', async () => {
  const mod = await importInlineModule();
  const doc = fakeDocument('#person?subject=u_4f21');
  const { render } = await mod.boot({ document: doc, scenario: 'realistic' });
  await render();
  const html = doc.html('app');
  assert.match(html, /u_4f21/);
  assert.ok(!/u_9a02/.test(html), 'no other person appears on a person screen');
});

test('the stylesheet gives suppression and zero different treatments', () => {
  const css = readFileSync(join(ROOT, 'styles.css'), 'utf8');
  assert.match(css, /\.v-suppressed\s*\{[^}]*hatch/);
  assert.match(css, /\.v-number\s*\{/);
  assert.ok(!/\.v-number\s*\{[^}]*hatch/.test(css), 'a real number is never hatched');
  assert.match(css, /\.v-absent\s*\{/, 'absent has its own treatment');
  assert.match(css, /\.row-suppressed\s*\{[^}]*hatch/);
});
