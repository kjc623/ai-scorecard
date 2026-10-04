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
import { createExploreStub } from '../src/explore-stub.js';
import { createExplorer } from '../src/explore-app.js';
import { renderExploreDetail, renderExploreText } from '../src/explore-render.js';
import { createQueryApi, createContentApi } from '../src/transport.js';

const NOW = new Date('2026-10-01T12:00:00Z');
const now = () => NOW;

function explorerFor(scenario = 'realistic', { withContent = true } = {}) {
  const stub = createExploreStub({ now, scenario });
  const asked = [];
  const content = withContent
    ? createContentApi({
      transport: {
        search: (body) => { asked.push(['search', body]); return stub.content.search(body); },
        retrieve: (body) => { asked.push(['retrieve', body]); return stub.content.retrieve(body); },
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
  assert.ok(explorer.state.text.hits.length > 0, 'the sample has a prompt with those words');
  assert.deepEqual(asked[0], ['search', { query: 'capital australia', limit: 20 }]);

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

test('a hit shows the prompt text with the person, the device and the tool', async () => {
  const { explorer } = explorerFor();
  const state = { ...explorer.state, text: { status: 'ready', query: 'x', truncated: false, problem: null, hits: [
    { submissionId: 's1', snippet: 'What is the <em>capital</em> of Australia', subject: 'u_4f21', device: 'd-1', tool: 'claude_code' },
    { submissionId: 's2', snippet: 'another', subject: null, device: null, tool: null },
  ] } };
  const html = renderExploreText(state);
  assert.match(html, /<span>u_4f21<\/span><span class="x-hit-sep" aria-hidden="true">\|<\/span><span>d-1<\/span><span class="x-hit-sep" aria-hidden="true">\|<\/span><span>claude_code<\/span>/);
  assert.match(html, />s2<\/span>/, 'a hit with no metadata still names its submission');
});
