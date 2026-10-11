// report.test.mjs — Insights' Monthly report: one calendar month as the tabs show it, on a page made
// to print, which the browser saves as a PDF.

import test from 'node:test';
import assert from 'node:assert/strict';
import { renderScreen } from '../src/render.js';
import { boot, createDashboard } from '../src/app.js';
import { createQueryApi } from '../src/transport.js';
import { monthOf } from '../src/dsl.js';
import { fixtureTransport } from './fixtures.mjs';
import { fakeDocument } from './helpers.mjs';

const NOW = () => new Date('2026-10-11T09:30:00Z');

async function load(filters, scenario = 'realistic') {
  const sent = [];
  const fixtures = fixtureTransport(scenario);
  const transport = { send: (body) => { sent.push(body); return fixtures.send(body); } };
  const dashboard = createDashboard({ api: createQueryApi({ transport }), now: NOW });
  const { view } = await dashboard.load('insights', { preset: 'd7', filters: { view: 'report', ...filters } });
  return { view, sent };
}

const windowOf = (body) => body.params?.window ?? body.window;

test('the report reads the chosen month as the tabs do, the month before first for the changes', async () => {
  const { view, sent } = await load({ month: '2026-08' });
  assert.deepEqual(sent.map((b) => [b.template ?? b.source, windowOf(b).from.slice(0, 10), windowOf(b).to.slice(0, 10)]), [
    ['q1_tools_ranked', '2026-07-01', '2026-08-01'],
    ['q1_tools_ranked', '2026-08-01', '2026-09-01'],
    ['q4_class_mix', '2026-08-01', '2026-09-01'],
    ['mart.v_tool_first_seen', '2026-08-01', '2026-09-01'],
    ['mart.v_tool_usage', '2026-08-01', '2026-09-01'],
    ['mart.agg_discovery_period', '2026-08-01', '2026-09-01'],
  ]);
  assert.equal(sent[1].params.bucket ?? sent[1].bucket, 'day', 'a month is read by the day, as its trend is drawn');
  assert.equal(view.title, 'Monthly report');
  assert.equal(view.subtitle, 'August 2026: 2026-08-01 to 2026-08-31, in UTC. Made 2026-10-11 09:30Z.');
});

test('the report holds the Exposure funnel and trend, What was exposed, People using AI, New this period and Found on devices', async () => {
  const { view } = await load({ month: '2026-09' });
  assert.deepEqual(view.lead.map((b) => [b.kind, b.title]), [['funnel', 'Exposure']]);
  assert.deepEqual(view.blocks.map((b) => [b.kind, b.title]), [
    ['series', 'Sensitive prompts per day'],
    ['table', 'What was exposed'],
    ['share', 'People using AI'],
    ['table', 'New this period'],
    ['table', 'Found on devices'],
  ]);
  assert.equal(view.lead[0].stages[0].change.text, 'Same as August 2026', 'each change is against the month before, by name');
  assert.match(view.blocks[4].summary, /As this report was made, the inventory scan had not reported in the last 24 hours on 200 of 4,620 enrolled devices, so what they hold is not known\.$/);
  assert.ok(view.notes.includes('Every figure is the one the Insights tabs show for September 2026. Each change compares it with August 2026.'));
  assert.equal(view.notesOpen, true, 'paper has nothing to click, so the notes print open');
});

test('the month picker offers this month so far and the five before it, and the last complete month by default', async () => {
  const { view } = await load({});
  assert.deepEqual(view.months.items.map((m) => [m.id, m.label]), [
    ['2026-05', 'May 2026'], ['2026-06', 'Jun 2026'], ['2026-07', 'Jul 2026'], ['2026-08', 'Aug 2026'], ['2026-09', 'Sep 2026'], ['2026-10', 'Oct 2026'],
  ]);
  assert.equal(view.months.current, '2026-09');
  assert.equal(view.months.items[0].href, '#insights?view=report&month=2026-05');
  const unknown = await load({ month: '2027-01' });
  assert.equal(unknown.view.months.current, '2026-09', 'a month not offered falls back to the last complete one');
  const running = await load({ month: '2026-10' });
  assert.equal(running.view.subtitle, 'October 2026: 2026-10-01 to 2026-10-11, so far, in UTC. Made 2026-10-11 09:30Z.');
  assert.equal(running.sent.filter((b) => b.template === 'q1_tools_ranked').length, 1, 'a month still running is not compared with a whole one');
  assert.equal(running.view.lead[0].stages[0].change, null);
  assert.ok(running.view.notes.includes('Every figure is the one the Insights tabs show for October 2026. October 2026 is not over, so it is not compared with September 2026.'));
});

test('an empty month says so in each section, and an unread month before shows no change', async () => {
  const { view } = await load({ month: '2026-09' }, 'empty');
  assert.equal(view.lead[0].emptyText, 'No prompts in this month.');
  assert.deepEqual(view.blocks.map((b) => [b.title, b.emptyText]), [
    ['What was exposed', 'No label a rule names was found in this month.'],
    ['People using AI', 'The people could not be counted for this window.'],
    ['New this period', 'No application sent its first prompt in this window.'],
    ['Found on devices', 'Discovery found no AI application on a device in this month.'],
  ]);
  const html = renderScreen(view, {});
  assert.match(html, /<details class="notes" open>/);
  assert.doesNotMatch(html, /u_[0-9a-f]{4}/, 'nobody is named');

  const fixtures = fixtureTransport();
  const refused = fixtureTransport('refused');
  // Only the month's own tool usage answers; every other read is refused.
  const transport = { send: (body) => (body.template === 'q1_tools_ranked' && body.params.window.from.startsWith('2026-09') ? fixtures : refused).send(body) };
  const partial = (await createDashboard({ api: createQueryApi({ transport }), now: NOW }).load('insights', { filters: { view: 'report', month: '2026-09' } })).view;
  assert.equal(partial.title, 'Monthly report');
  assert.ok(partial.notes.includes('Every figure is the one the Insights tabs show for September 2026. August 2026 could not be read, so no change is shown.'));
  assert.equal(partial.blocks.find((b) => b.title === 'Found on devices').emptyText, 'What discovery found could not be read for this month.');
});

test('Insights offers the Monthly report, and the report saves itself as a PDF through the browser', async () => {
  const doc = fakeDocument('#insights?view=adoption');
  const clicks = [];
  const listen = doc.addEventListener;
  doc.addEventListener = (type, fn) => {
    if (type === 'click') clicks.push(fn);
    listen(type, fn);
  };
  await boot({ document: doc, api: createQueryApi({ transport: fixtureTransport() }), admin: null, session: null });
  assert.match(doc.html('app'), /<a class="btn btn-small" href="#insights\?view=report">Monthly report<\/a>/);

  const month = monthOf(new Date(), 1);
  await doc.navigate(`#insights?view=report&month=${month}`);
  const html = doc.html('app');
  assert.match(html, /<button type="button" class="btn btn-small btn-primary" data-print>Save as PDF<\/button>/);
  assert.match(html, /<a class="btn btn-small btn-quiet" href="#insights">Back to Insights<\/a>/);
  assert.match(html, new RegExp(`<div class="seg" role="group" aria-label="Month">.*href="#insights\\?view=report&amp;month=${month}" aria-current="true"`));
  assert.doesNotMatch(html, /aria-label="Window"/, 'the month is the window');
  assert.doesNotMatch(html, />Coding agents<\/a>/, 'the report is not a tab');

  let printed = 0;
  doc.defaultView = { print: () => { printed += 1; } };
  for (const click of clicks) click({ target: { closest: (selector) => (selector === '[data-print]' ? {} : null) }, preventDefault() {} });
  assert.equal(printed, 1);
});
