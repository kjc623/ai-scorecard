// layout.test.mjs — where every page sits: the old addresses and where they land, the tabs under
// each destination's title, the audit trail's filters, and the Overview cards that open as a whole.

import test from 'node:test';
import assert from 'node:assert/strict';
import { boot, redirectFor } from '../src/app.js';
import { createQueryApi } from '../src/transport.js';
import { renderScreen } from '../src/render.js';
import { readState } from '../src/states.js';
import { postureView } from '../src/views.js';
import { RULE_MATCH_ROWS, RULE_ROWS, TOOL_ROWS, fixtureTransport } from './fixtures.mjs';
import { COMPLETE, FRESH, envelope, fakeDocument } from './helpers.mjs';

const EVERY_PAGE = ['posture', 'tools', 'teams', 'person', 'explore', 'rules', 'labels', 'sanction', 'activity', 'insights', 'settings', 'devices', 'directory', 'integrations', 'audit'];

/** A booted dashboard with the requests it sent and a way to fire the events a browser would. */
async function booted(hash, { pages = EVERY_PAGE } = {}) {
  const sent = [];
  const transport = fixtureTransport();
  const doc = fakeDocument(hash);
  const listeners = [];
  const add = doc.addEventListener;
  doc.addEventListener = (type, fn) => { listeners.push({ type, fn }); add(type, fn); };
  await boot({
    document: doc,
    api: createQueryApi({ transport: { send: (body) => { sent.push(body); return transport.send(body); } } }),
    admin: null,
    session: { actor: 'a@lab.test', roles: ['analyst'], pages },
  });
  const fire = (type, event) => Promise.all(listeners.filter((l) => l.type === type).map((l) => l.fn(event)));
  return { doc, sent, fire };
}

const tabs = (html) => [...html.matchAll(/<a class="page-tab" href="([^"]*)"( aria-current="page")?>([^<]*)<\/a>/g)].map((m) => [m[3], m[1].replace(/&amp;/g, '&'), Boolean(m[2])]);

// ---------------------------------------------------------------------------------------------
// The old addresses
// ---------------------------------------------------------------------------------------------

test('an address from the old layout names where its page is now; every other address stays', () => {
  const at = (hash, allowed = null) => {
    const [id, query = ''] = hash.slice(1).split('?');
    return redirectFor({ id, filters: Object.fromEntries(new URLSearchParams(query)) }, allowed);
  };
  assert.equal(at('#deployment'), '#devices');
  assert.equal(at('#rules?view=labels'), '#labels');
  assert.equal(at('#tools?view=unsanctioned&preset=d30'), '#tools?preset=d30&sanction=unsanctioned');
  assert.equal(at('#tools?view=classes&preset=d30', new Set(['tools', 'activity'])), '#activity?preset=d30');
  assert.equal(at('#tools?view=classes&preset=d30', new Set(['tools'])), '#tools?preset=d30', 'where Activity is not offered the classes land on Tools');
  assert.equal(at('#insights?view=exposure&preset=h24'), '#activity?preset=h24');
  assert.equal(at('#insights?view=discovered&preset=d7'), '#insights?view=risk', 'Insights reads one window, so the moved tab carries none');
  assert.equal(at('#insights?view=demand'), '#insights?view=licensing');
  assert.equal(at('#insights?view=agents'), '#insights?view=adoption');
  for (const kept of ['#tools', '#tools?sanction=unknown', '#rules', '#insights', '#insights?view=risk', '#devices?device=x', '#audit?actor=a']) assert.equal(at(kept), null, kept);
});

test('the old address is replaced, not added to the history, and the page it names is shown', async () => {
  const { doc } = await booted('#tools?view=unsanctioned&preset=d30');
  assert.equal(doc.location.hash, '#tools?preset=d30&sanction=unsanctioned');
  assert.match(doc.html('app'), /<a class="seg-item" href="#tools\?sanction=unsanctioned&amp;preset=d30" aria-current="true">Unsanctioned<\/a>/);
  await doc.navigate('#insights?view=exposure');
  assert.equal(doc.location.hash, '#activity');
  assert.match(doc.html('app'), /<h3>Where sensitive prompts went<\/h3>/);
});

// ---------------------------------------------------------------------------------------------
// The tabs
// ---------------------------------------------------------------------------------------------

test('a destination\'s tabs sit under its title, and a tab that reads a window keeps the one chosen', async () => {
  const { doc } = await booted('#teams?preset=d30');
  assert.match(doc.html('app'), /<h2>Usage<\/h2>/);
  assert.deepEqual(tabs(doc.html('app')), [['Tools', '#tools?preset=d30', false], ['Teams', '#teams?preset=d30', true], ['Users', '#person?preset=d30', false]]);
  assert.match(doc.html('nav'), /<a href="#tools" class="active" aria-current="page">/, 'Usage is the current destination');

  await doc.navigate('#activity?preset=d30');
  assert.deepEqual(tabs(doc.html('app')), [['Rules', '#rules', false], ['Labels', '#labels', false], ['Tools', '#sanction', false], ['Activity', '#activity?preset=d30', true]],
    'Rules, Labels and Tools read no window');
});

test('a tab the session may not open is not offered, and a destination left with one page shows no tabs', async () => {
  const { doc } = await booted('#rules', { pages: ['posture', 'tools', 'teams', 'rules', 'sanction', 'devices'] });
  assert.deepEqual(tabs(doc.html('app')).map(([label]) => label), ['Rules', 'Tools']);
  await doc.navigate('#devices');
  assert.deepEqual(tabs(doc.html('app')), []);
  assert.match(doc.html('app'), /<span class="eyebrow">Settings<\/span><h2>Devices<\/h2>/, 'the page is named under its destination');
});

test('a record opened from a list is headed by its destination and shows no tabs', async () => {
  const { doc } = await booted('#tools?tool=tls_b6681b043244c43f&preset=d30');
  assert.match(doc.html('app'), /<span class="eyebrow">Usage<\/span>/);
  assert.deepEqual(tabs(doc.html('app')), []);
  assert.match(doc.html('nav'), /<a href="#tools" class="active" aria-current="page">/);
  await doc.navigate('#devices?device=9f1c0b6e-0000-4000-8000-000000000002');
  assert.match(doc.html('app'), /<span class="eyebrow">Settings<\/span><h2>MAC-DESIGN-2<\/h2>/);
  assert.match(doc.html('app'), /<h3>Collectors<\/h3>/);
});

// ---------------------------------------------------------------------------------------------
// The audit trail
// ---------------------------------------------------------------------------------------------

test('the audit trail filters by actor, action, object type and case, in the window chosen', async () => {
  const { doc, sent, fire } = await booted('#audit?preset=d30');
  const page = doc.html('app');
  assert.match(page, /<form class="filter-form card" data-screen="audit"/);
  for (const name of ['actor', 'action', 'object_type', 'case']) assert.match(page, new RegExp(`<input class="dp-input" name="${name}" type="search"`));
  assert.doesNotMatch(page, />Clear</, 'nothing to clear');

  const form = { dataset: { screen: 'audit' }, elements: { actor: { value: ' r.okafor@customer.example ' }, action: { value: '' }, object_type: { value: 'ingest.submission' }, case: { value: '' } } };
  await fire('submit', { target: form, preventDefault() {} });
  assert.equal(doc.location.hash, '#audit?preset=d30&actor=r.okafor%40customer.example&object_type=ingest.submission');
  await doc.navigate(doc.location.hash);
  const read = sent.filter((b) => b.template === 'q10_audit_trail').pop();
  assert.equal(read.params.actor, 'r.okafor@customer.example');
  assert.equal(read.params.object_type, 'ingest.submission');
  assert.equal(read.params.action, undefined, 'an empty field filters nothing');
  const filtered = doc.html('app');
  assert.match(filtered, /name="actor"[^>]* value="r\.okafor@customer\.example"/);
  assert.match(filtered, /<a class="[^"]*" href="#audit\?preset=d30">Clear<\/a>/);
});

// ---------------------------------------------------------------------------------------------
// Overview's cards
// ---------------------------------------------------------------------------------------------

const ok = (data, source, meta = {}) => readState(envelope('ok', { data, freshness: FRESH, coverage: COMPLETE, meta: { source, ...meta } }));
const usage = () => ok(TOOL_ROWS, 'mart.v_tool_usage', { applied_bucket: 'day' });
const rules = (data = RULE_ROWS) => ok(data, 'ops.enforcement_rule');
const matches = (data = RULE_MATCH_ROWS) => ok(data, 'mart.agg_rule_period', { applied_bucket: 'week' });
const refused = () => readState(envelope('query_too_broad', { error: { code: 'cost_estimate_exceeded', message: 'Too broad' } }));
const policy = (view) => view.blocks.find((b) => b.title === 'Policy at work');

test('Policy at work ranks the rules by the prompts each decided, and says the rules that matched nothing and the tools awaiting a decision', () => {
  const card = policy(postureView({ devices: null, tools: usage(), rules: rules(), ruleMatches: matches() }));
  assert.equal(card.cardHref, '#rules');
  assert.deepEqual(card.items.map((i) => [i.label, i.value.value, i.chip.key]), [
    ['claude-code.warn', 310, 'warned'], ['pii.warn', 80, 'warned'], ['credential.allow', 66, 'allowed'], ['secrets.block', 13, 'blocked'],
  ], 'the busiest four, each with what it does; a removed rule is not listed');
  assert.ok(card.items.every((i) => i.href === null), 'nothing inside the card is a link of its own');
  assert.match(card.summary, /^1 rule matched nothing in this window · /);
  assert.match(card.summary, /tools? waits? for a sanction decision\.$|every tool in use has a sanction decision\.$/);
});

test('Policy at work says why it is empty: no rule set, the rules not read, or their matches not read', () => {
  assert.equal(policy(postureView({ devices: null, tools: usage(), rules: rules([]), ruleMatches: matches([]) })).emptyText, 'No enforcement rule is set, so every prompt is logged.');
  assert.equal(policy(postureView({ devices: null, tools: usage(), rules: refused(), ruleMatches: matches() })).emptyText, 'The rules could not be read.');
  assert.equal(policy(postureView({ devices: null, tools: usage(), rules: rules(), ruleMatches: refused() })).emptyText, 'What the rules matched could not be read for this window.');
  assert.equal(policy(postureView({ devices: null, tools: usage(), rules: rules(), ruleMatches: matches([]) })).emptyText, 'No rule matched a prompt in this window.');
});

test('a card with nothing to open inside it is one link, with no path written on it', () => {
  const view = postureView({ devices: null, tools: usage(), rules: rules(), ruleMatches: matches() }, { preset: 'd30' });
  const html = renderScreen(view, {});
  assert.match(html, /<a class="share-block card card-link" href="#sanction" id="b-tools-by-decision"><div class="card-core"><header class="block-head"><h3>Tools by decision<\/h3>/);
  assert.match(html, /<a class="rank-block card card-link rank-flat" href="#rules" id="b-policy-at-work"><div class="card-core"><header class="block-head"><h3>Policy at work<\/h3>/);
  assert.doesNotMatch(html, /Decide which tools are sanctioned|→/, 'no link text names where a card goes');
  const cards = html.split('card-link').slice(1).map((part) => part.slice(0, part.indexOf('</a>')));
  assert.ok(cards.every((card) => !/<a /.test(card)), 'no link inside a card that is a link');
  assert.match(html, /<section class="rank-block card rank-flat" id="b-tools-in-use"><div class="card-core"><header class="block-head"><h3>Tools in use<\/h3>.*?<a class="block-link" href="#tools\?preset=d30">All tools<\/a>/, 'a card listing links keeps its own');
});

// ---------------------------------------------------------------------------------------------
// A card that opens a block on its own page, and the (i) beside a label
// ---------------------------------------------------------------------------------------------

test('a tile whose figure is a table on the same page opens that table, and the table carries the id it is opened by', async () => {
  const { doc } = await booted('#teams?team=7d0c1a52-58f4-4a51-9b1e-1f6d2f0a0001');
  const html = doc.html('app');
  assert.match(html, /<div class="tile card tile-link" role="link" tabindex="0" data-jump="b-prompts-by-user"><div class="card-core"><span class="tile-label">Prompts</, 'the figure opens the roster it is folded from');
  assert.match(html, /<section class="table-block card" id="b-prompts-by-user">/, 'which is reached by that id');
  assert.doesNotMatch(html, /<a class="tile card tile-link"[^>]*data-jump/, 'a jump is not an address');
});

test('what a figure counts sits behind an (i) in the label, never as a sentence on the card', () => {
  const view = postureView({ devices: null, tools: usage(), rules: rules(), ruleMatches: matches() }, { preset: 'd30' });
  const html = renderScreen(view, {});
  assert.match(html, /<span class="tile-label">Users<span class="tip" tabindex="0"><svg class="tip-icon"[^>]*>.*?<\/svg><span class="tip-text" role="tooltip">The most users seen in any one period of the window, never a sum across periods\.<\/span><\/span><\/span>/);
  assert.doesNotMatch(html, /<span class="tile-note">[^<]*(?:counted once|lower bound|metadata-only)/, 'the explanations are not under the figures');
  assert.match(html, /<h3>Tools by decision<\/h3><span class="tip" tabindex="0">/, 'a block carries its (i) beside its title');
});
