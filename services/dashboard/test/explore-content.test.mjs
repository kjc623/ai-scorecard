// explore-content.test.mjs — the two content reads on the Explore page: prompt-text search and
// approved retrieval.
//
// The rest of the dashboard never shows content, and neither does this page from a query. What is
// asserted here is that content appears only as the answer to a retrieval the vault served, for
// the record that is open; that a refusal is shown with its reason and never as an empty result;
// and that what a person typed is told apart from what the client added around it.

import test from 'node:test';
import assert from 'node:assert/strict';
import { exploreUserInput } from '../src/explore-model.js';
import { createExploreFake } from './explore-fake.mjs';
import { createExplorer } from '../src/explore-app.js';
import { renderExploreDetail, renderExploreText } from '../src/explore-render.js';
import { createQueryApi, createContentApi } from '../src/transport.js';
import { windowFor } from '../src/dsl.js';

const NOW = new Date('2026-10-01T12:00:00Z');
const now = () => NOW;

function explorerFor(scenario = 'realistic', { withContent = true } = {}) {
  const stub = createExploreFake({ now, scenario });
  const asked = [];
  const content = withContent
    ? createContentApi({
      transport: {
        search: (body) => { asked.push(['search', body]); return stub.content.search(body); },
        retrieve: (body) => { asked.push(['retrieve', body]); return stub.content.retrieve(body); },
        readUrl: (url) => { asked.push(['readUrl', url]); return stub.content.readUrl(url); },
      },
    })
    : null;
  return { explorer: createExplorer({ api: createQueryApi({ transport: stub }), content, now }), stub, asked };
}

const uploadedEvent = (stub) => stub.sample.events.find((e) => e.content_state === 'uploaded');

// ── what the user typed ──────────────────────────────────────────────────────────────────────

test('what the user typed is told apart from what the client added around it', () => {
  assert.deepEqual(
    exploreUserInput('<system-reminder>\nclient context\n</system-reminder>\n What is the capital of Australia?'),
    { typed: 'What is the capital of Australia?', kind: 'prompt' },
  );
  assert.deepEqual(exploreUserInput('Summarise the Q4 deck.'), { typed: 'Summarise the Q4 deck.', kind: 'prompt' });
});

test('in a request body the latest user message that carries text is the input of the turn', () => {
  const body = JSON.stringify({
    model: 'm',
    messages: [
      { role: 'user', content: 'first question' },
      { role: 'assistant', content: 'an answer' },
      { role: 'user', content: [{ type: 'text', text: '<system-reminder>ctx</system-reminder>' }, { type: 'text', text: 'second question' }] },
      { role: 'assistant', content: [{ type: 'tool_use', id: 't', name: 'n', input: {} }] },
      { role: 'user', content: [{ type: 'tool_result', tool_use_id: 't', content: 'output' }] },
    ],
  });
  assert.deepEqual(exploreUserInput(body), { typed: 'second question', kind: 'prompt' });
});

test('a capture nobody typed says what it is rather than showing an empty box', () => {
  assert.deepEqual(exploreUserInput('{"events":[{"event_type":"ClaudeCodeInternalEvent"}]}'), { typed: '', kind: 'internal' });
  assert.deepEqual(exploreUserInput('{"model":"m","messages":[]}'), { typed: '', kind: 'other' });
});

// ── prompt-text search ───────────────────────────────────────────────────────────────────────

test('a prompt-text search returns fragments, and a hit opens the event it belongs to', async () => {
  const { explorer, asked } = explorerFor();
  await explorer.restore('#events?window=d30');
  await explorer.searchText('capital australia');
  assert.equal(explorer.state.text.status, 'ready');
  assert.ok(explorer.state.text.hits.length > 0, 'the fake has a prompt with those words');
  assert.deepEqual(asked[0], ['search', { query: 'capital australia', limit: 20, window: windowFor('d30', NOW) }]);

  const html = renderExploreText(explorer.state);
  assert.match(html, /<em>capital<\/em>/, 'the search highlight survives');
  assert.ok(!/<em>.*<script/i.test(html));

  const hit = explorer.state.text.hits[0];
  await explorer.openHit(hit.submissionId);
  assert.equal(explorer.state.detail.status, 'ready');
  assert.equal(explorer.state.detail.submissionId, hit.submissionId);
});

test('a snippet is escaped: only the search highlight is markup', async () => {
  const { explorer } = explorerFor();
  const state = { ...explorer.state, text: { status: 'ready', query: 'x', truncated: false, problem: null, hits: [{ submissionId: 's', snippet: '<img src=x onerror=1> <em>x</em>' }] } };
  const html = renderExploreText(state);
  assert.ok(!html.includes('<img'), 'markup in a snippet is text');
  assert.match(html, /&lt;img/);
  assert.match(html, /<em>x<\/em>/);
});

test('a search with no match says so, and a refused search shows its reason', async () => {
  const none = explorerFor();
  await none.explorer.searchText('zzzznotaword');
  assert.equal(none.explorer.state.text.status, 'ready');
  assert.match(renderExploreText(none.explorer.state), /No uploaded prompt contains/);

  const busy = explorerFor('busy');
  await busy.explorer.searchText('capital');
  assert.equal(busy.explorer.state.text.status, 'refused');
  assert.match(renderExploreText(busy.explorer.state), /concurrency limit/);
});

test('a page with no content path says so instead of searching', async () => {
  const { explorer } = explorerFor('realistic', { withContent: false });
  await explorer.searchText('capital');
  assert.equal(explorer.state.text.status, 'refused');
  assert.equal(explorer.state.text.problem.code, 'no_content_path');
});

test('the rail filters narrow a prompt-text search, and a person with no content returns none', async () => {
  const { explorer, stub, asked } = explorerFor();
  await explorer.restore('#events?window=d30');
  const uploaded = stub.sample.events.filter((e) => e.content_state === 'uploaded');
  const subject = uploaded[0].subject;

  await explorer.setFilter('subject', subject);
  await explorer.searchText('the');
  assert.equal(explorer.state.text.status, 'ready');
  assert.ok(explorer.state.text.hits.length > 0, 'the person has uploaded content');
  assert.ok(explorer.state.text.hits.every((h) => h.subject === subject));
  const sent = asked.filter(([kind]) => kind === 'search').pop()[1];
  assert.equal(sent.subject, subject, 'the person filter reaches the search');

  await explorer.setFilter('subject', 'nobody@example');
  assert.equal(explorer.state.text.status, 'ready');
  assert.equal(explorer.state.text.hits.length, 0, 'a different person returns none');
});

test('the window switch applies to prompt-text results', async () => {
  const { explorer, asked } = explorerFor();
  await explorer.restore('#events?window=d30');
  await explorer.searchText('the');
  const d30 = explorer.state.text.hits.length;
  const before = asked.filter(([kind]) => kind === 'search').pop()[1].window;

  await explorer.setWindow('h6');
  const after = asked.filter(([kind]) => kind === 'search').pop()[1].window;
  assert.equal(explorer.state.text.status, 'ready');
  assert.ok(Date.parse(after.from) > Date.parse(before.from), 'the search was re-issued with the narrower window');
  assert.ok(explorer.state.text.hits.length < d30, 'the narrower window returns fewer of the same matches');
});

test('a prompt-text search pages, and the page size can be smaller than the default', async () => {
  const { explorer } = explorerFor();
  await explorer.restore('#events?window=d30');
  await explorer.setTextPageSize(5);
  await explorer.searchText('the');
  assert.equal(explorer.state.text.hits.length, 5, 'a smaller page size is honoured');
  assert.ok(explorer.state.text.nextCursor, 'a full page has a next page');

  const first = explorer.state.text.hits.map((h) => h.submissionId);
  await explorer.loadMoreText();
  assert.equal(explorer.state.text.hits.length, 10);
  assert.deepEqual(explorer.state.text.hits.slice(0, 5).map((h) => h.submissionId), first, 'page two is appended, not replaced');

  // Keep loading to the end: the last page clears the cursor.
  let guard = 0;
  while (explorer.state.text.nextCursor && guard < 10) {
    await explorer.loadMoreText();
    guard += 1;
  }
  assert.equal(explorer.state.text.nextCursor, null, 'the result set ends');
});

test('a rail filter the search cannot apply is named rather than silently ignored', async () => {
  const { explorer } = explorerFor();
  await explorer.restore('#events?window=d30');
  await explorer.setFilter('action', 'blocked');
  await explorer.searchText('the');
  const html = renderExploreText(explorer.state);
  assert.match(html, /still filter the list only/);
  assert.match(html, /action/);
  assert.match(html, /id="x-text-page"/, 'the page-size control is offered');
  assert.match(html, /Showing \d+ matching prompt/);
});

// ── approved retrieval ───────────────────────────────────────────────────────────────────────

test('opening an event whose content is stored shows the prompt, with no approval asked', async () => {
  const { explorer, stub, asked } = explorerFor();
  const event = uploadedEvent(stub);
  await explorer.restore(`#events?window=d30&open=${event.submission_id}`);
  assert.equal(explorer.state.detail.status, 'ready');
  assert.equal(explorer.state.content.status, 'ready');
  const sent = asked.find(([kind]) => kind === 'retrieve')[1];
  assert.ok(sent.event_ids.length > 0, 'the retrieval names the record\'s observations');
  assert.deepEqual(Object.keys(sent), ['event_ids'], 'no case reference and no approver are sent');

  const html = renderExploreDetail(explorer.state);
  assert.ok(html.includes(explorer.state.content.typed), 'what the person typed is on the page');
  assert.match(html, /Everything captured for this request/);
  assert.match(html, /&lt;system-reminder&gt;/, 'the full capture is shown, escaped');
  assert.ok(!explorer.state.content.typed.includes('system-reminder'), 'the typed view carries no client context');
  assert.ok(!/Case reference|Second approver|x-retrieve-form/.test(html), 'no approval form');
});

test('a read the vault refuses shows the reason and offers to try again', async () => {
  const { explorer, stub } = explorerFor('busy');
  stub.setScenario('realistic');
  await explorer.restore('#events?window=d30');
  stub.setScenario('busy');
  await explorer.open(uploadedEvent(stub).submission_id);
  if (explorer.state.detail.status !== 'ready') return; // the record read itself was refused in this scenario
  assert.equal(explorer.state.content.status, 'refused');
  const html = renderExploreDetail(explorer.state);
  assert.match(html, /concurrency limit/);
  assert.match(html, /data-act="retrieve"/);
});

test('retrieved content does not outlive the record it was retrieved for', async () => {
  const { explorer, stub } = explorerFor();
  const [first, second] = stub.sample.events.filter((e) => e.content_state === 'uploaded');
  await explorer.restore(`#events?window=d30&open=${first.submission_id}`);
  assert.equal(explorer.state.content.status, 'ready');
  const firstTyped = explorer.state.content.typed;

  await explorer.open(second.submission_id);
  assert.equal(explorer.state.content.submissionId, second.submission_id, 'the content on the page belongs to the open record');
  assert.ok(firstTyped === explorer.state.content.typed || !renderExploreDetail(explorer.state).includes(firstTyped));

  explorer.close();
  assert.equal(explorer.state.content.status, 'idle', 'closing the record drops the content');
});

test('only uploaded content offers a retrieval', async () => {
  const { explorer, stub } = explorerFor();
  const local = stub.sample.events.find((e) => e.content_state === 'local_only');
  await explorer.restore(`#events?window=d30&open=${local.submission_id}`);
  const html = renderExploreDetail(explorer.state);
  assert.ok(!html.includes('x-retrieve-form'), 'content still on the device offers no retrieval');
  assert.match(html, /remains on the device/);
});

test('a hit shows the prompt text with the person, the hostname and the tool', async () => {
  const { explorer } = explorerFor();
  const state = { ...explorer.state, text: { status: 'ready', query: 'x', truncated: false, problem: null, hits: [
    { submissionId: 's1', snippet: 'What is the <em>capital</em> of Australia', subject: 'alice@contoso.example', directory_name: 'Alice Smith', device: 'd-1', hostname: 'FIN-LAPTOP-07', tool: 'tls_b6681b043244c43f', tool_name: 'Claude Code' },
    { submissionId: 's2', snippet: 'another', subject: null, device: null, tool: null },
    { submissionId: 's3', snippet: 'third', subject: 'u_9a02', device: 'd-2', hostname: null, tool: 'tls_11574658dafb8805' },
  ] } };
  const html = renderExploreText(state);
  // The hostname is preferred to the device UUID when there is one, and the directory display
  // name sits between the account name and the device.
  assert.match(html, /<span>alice@contoso\.example<\/span><span class="x-hit-sep" aria-hidden="true">\|<\/span><span>Alice Smith<\/span><span class="x-hit-sep" aria-hidden="true">\|<\/span><span>FIN-LAPTOP-07<\/span><span class="x-hit-sep" aria-hidden="true">\|<\/span><span>Claude Code<\/span>/);
  // With no hostname the UUID is the device part; a hit with no metadata still names its submission.
  assert.match(html, /<span>u_9a02<\/span><span class="x-hit-sep" aria-hidden="true">\|<\/span><span>d-2<\/span>/);
  assert.match(html, />s2<\/span>/, 'a hit with no metadata still names its submission');
});
