// evidence.mjs — print the evidence the report quotes. Run: node tools/evidence.mjs
import { plan } from '../src/plan.js';
import { K } from '../src/registry.js';

const NOW = new Date('2026-10-02T12:00:00Z');
const WINDOW = { from: '2026-09-01T00:00:00Z', to: '2026-09-08T00:00:00Z' };
const doc = (over = {}) => ({
  query_version: '1',
  source: 'mart.v_tool_usage',
  bucket: 'day',
  dimensions: ['tool'],
  measures: ['submissions', 'users'],
  filters: [],
  window: { ...WINDOW },
  limit: 50,
  ...over,
});

const HOSTILE = "'; DROP TABLE ingest.submission; --";

console.log('=== 1. a hostile value is bound, not written ===');
const accepted = plan(doc({ filters: [{ field: 'tool', op: 'eq', value: HOSTILE }] }), { now: NOW, actorId: 'analyst-1' });
const read = accepted.statements.find((s) => s.id === 'read');
console.log(read.text);
console.log('params:', JSON.stringify(read.params));
console.log('statement text contains the hostile string:', read.text.includes(HOSTILE));
console.log('statement text contains ";" or "--":', read.text.includes(';') || read.text.includes('--'));

console.log('\n=== 2. typed rejections (§13 state / reason / HTTP) ===');
const attempts = [
  ['sql', doc({ sql: 'DROP TABLE ops.audit' })],
  ['where', doc({ where: '1=1' })],
  ['order_by', doc({ order_by: 'tool' })],
  ['tenant_id', doc({ tenant_id: 'other-tenant' })],
  ['content_match', doc({ content_match: 'iban' })],
  ['unknown key', doc({ groupby: ['tool'] })],
  ['unknown dimension', doc({ dimensions: ['salary'] })],
  ['unknown measure', doc({ measures: ['revenue'] })],
  ['unknown operator', doc({ filters: [{ field: 'tool', op: 'regexp', value: 'x' }] })],
  ['unknown bucket', doc({ bucket: 'fortnight' })],
  ['unsupported version', doc({ query_version: '2' })],
  ['starts_with on subject', doc({ filters: [{ field: 'sanctioned_state', op: 'starts_with', value: 'un' }] })],
  ['oversized in-list', doc({ filters: [{ field: 'tool', op: 'in', value: Array.from({ length: 10_000 }, (_, i) => `v${i}`) }] })],
  ['oversized value', doc({ filters: [{ field: 'tool', op: 'eq', value: 'x'.repeat(300) }] })],
  ['nested JSON value', doc({ filters: [{ field: 'tool', op: 'eq', value: { a: { b: { c: { d: 1 } } } } }] })],
  ['eq null', doc({ filters: [{ field: 'tool', op: 'eq', value: null }] })],
  ['no window', doc({ window: undefined })],
  ['four-year window', doc({ window: { from: '2023-01-01T00:00:00Z', to: '2026-01-01T00:00:00Z' }, dimensions: [], limit: undefined })],
  ['prohibited by shape', doc({ rollup: true, cursor: 'a.b' })],
];
attempts.push(['prototype pollution', JSON.parse('{"query_version":"1","__proto__":{"polluted":true},"source":"mart.v_tool_usage"}')]);
attempts.push(['non-object body', 'SELECT 1']);

for (const [label, request] of attempts) {
  try {
    plan(request, { now: NOW });
    console.log(`${label.padEnd(22)} ACCEPTED (would be a bug)`);
  } catch (error) {
    console.log(`${label.padEnd(22)} ${String(error.resultState).padEnd(24)} ${String(error.reason).padEnd(28)} HTTP ${error.http}`);
  }
}
console.log('prototype polluted:', {}.polluted !== undefined);

console.log('\n=== 3. k-suppression and the envelope (fake client) ===');
const { executePlan } = await import('../src/plan.js');
const smallCells = [
  { bucket: new Date('2026-09-01T00:00:00Z'), tool: 'legal_ai', submissions: 4, users: 2, __k_subjects: 2 },
  { bucket: new Date('2026-09-01T00:00:00Z'), tool: 'claude_web', submissions: 900, users: 40, __k_subjects: 40 },
  { bucket: new Date('2026-09-01T00:00:00Z'), tool: 'quiet_tool', submissions: 0, users: 0, __k_subjects: 0 },
];
const client = {
  async begin() {}, async commit() {}, async rollback() {},
  async query(text) {
    if (text.includes('FROM ops.aggregate_watermark')) {
      return { rows: [{ aggregate_name: 'mart.agg_tool_period', bucket_size: 'day', last_run_at: new Date('2026-10-02T11:57:00Z'), last_complete_bucket: new Date('2026-10-02T11:00:00Z') }] };
    }
    if (text.includes('AS devices_enrolled')) {
      return { rows: [{ devices_enrolled: 4620, devices_reporting: 4180, expected_collector_days: 27720, observed_collector_days: 25080, gap_reasons: { not_enrolled: 440 } }] };
    }
    return { rows: smallCells };
  },
};
const envelope = await executePlan(plan(doc({ limit: 50 }), { now: NOW, actorId: 'analyst-1' }), { client, now: NOW });
console.log(JSON.stringify({
  result_state: envelope.result_state,
  data: envelope.data,
  suppression: envelope.suppression,
  freshness: { state: envelope.freshness.state, lag_seconds: envelope.freshness.lag_seconds },
  coverage: { state: envelope.coverage.state, devices_reporting: envelope.coverage.devices_reporting, devices_enrolled: envelope.coverage.devices_enrolled },
  audit: envelope.audit,
}, null, 1));
console.log('k =', K);

console.log('\n=== 4. every registered source, one compiled read each ===');
const { SOURCES, SOURCE_IDS } = await import('../src/registry.js');
for (const id of SOURCE_IDS) {
  const source = SOURCES[id];
  const request = source.kind === 'aggregate'
    ? { query_version: '1', source: id, bucket: 'day', dimensions: Object.keys(source.dimensions).filter((d) => d !== 'bucket' && d !== 'subject').slice(0, 1), measures: Object.keys(source.measures).slice(0, 2), filters: source.dimensions.device ? [{ field: 'device', op: 'eq', value: '00000000-0000-4000-8000-0000000000aa' }] : [], window: WINDOW, limit: 20 }
    : { query_version: '1', source: id, filters: [], ...(source.time ? { window: WINDOW } : {}), limit: 20 };
  const p = plan(request, { now: NOW, actorId: 'evidence' });
  const r = p.statements.find((s) => s.id === 'read');
  console.log(`${id.padEnd(28)} vars=${String(r.text.match(/\$\d+/g)?.length ?? 0).padStart(2)}  bytes=${String(r.text.length).padStart(4)}  audit=${p.audit.decision.phase}`);
}
