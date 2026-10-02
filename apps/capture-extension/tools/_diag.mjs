// Scratch: is the stuck entry a stale expectation or a lost ack?
import { createHarness, settle, waitFor } from '../test-support/harness.mjs';
import { chromeRequest } from '../test-support/fake-chrome.mjs';
import { CHAT_BODY } from '../test-support/fixtures.mjs';

const CHAT_URL = 'https://chat.example-ai.invalid/v1/chat/completions';

const h = createHarness({ failConnect: true, connectCooldownMs: 0 });
await h.app.start();
h.app.applyPolicy({ policy_version: 'b1', bundle: { policy_version: 'b1', default_mode: 'm1' } });
await settle();

// Instrument the queue: peak depth, every enqueue seq, every ack.
const q = h.app.queue;
const log = { peaks: 0, enqueued: [], acked: [], sends: [] };
const origEnqueue = q.enqueue.bind(q);
q.enqueue = (kind, payload, size) => { const s = origEnqueue(kind, payload, size); log.enqueued.push(s); log.peaks = Math.max(log.peaks, q.size()); return s; };
const origAck = q.ack.bind(q);
q.ack = (seq, n) => { const r = origAck(seq, n); if (r) log.acked.push(seq); return r; };

const coreObs = h.core.observations.bind(h.core);
h.core.observations = () => { const o = coreObs(); log.sends = o.map((x) => x.client_id); return o; };

console.log('--- phase 1: dead channel ---');
await h.fake.drive('body', chromeRequest({ url: CHAT_URL, headers: { 'content-type': 'application/json' }, body: JSON.stringify(CHAT_BODY) }));
await waitFor(() => q.size() === 1, { label: 'held' });
console.log('depth', q.size(), 'peak', log.peaks, 'enqueued', JSON.stringify(log.enqueued), 'acked', JSON.stringify(log.acked));
console.log('errors:', JSON.stringify(h.app.health.counters.snapshot().counters.errors));

console.log('--- phase 2: channel back ---');
h.fake.state.failConnect = false;
const sent = await h.app.pipeline.drainQueue(10);
console.log('drain result:', JSON.stringify(sent));
console.log('depth after drain', q.size(), 'peak', log.peaks, 'enqueued', JSON.stringify(log.enqueued), 'acked', JSON.stringify(log.acked));
console.log('frames at core:', JSON.stringify(log.sends));
console.log('delivered_total', q.stats().delivered_total, 'emitted', h.app.health.counters.snapshot().counters.emitted);
console.log('core:', h.app.health.report().core);
