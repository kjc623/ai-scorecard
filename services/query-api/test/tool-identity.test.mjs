// tool-identity.test.mjs — one tool is one row in every listing, however many fingerprints it was
// seen under.
//
// A tool's rows are recorded under several fingerprints: an extension host hash, the app's own
// `app:` id, a site the app's domain covers. Every source that can show a tool returns, groups,
// orders, pages and filters it by the tool the row counts as (`app:<key>`, or the fingerprint
// itself when no app claims it), never by the fingerprint the row was recorded under. The database
// gate (invariant T87) proves the views and ops.tool_* functions resolve one tool to one id, and
// db.test.mjs runs the listings against a live database.

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { plan } from '../src/plan.js';
import { SOURCES, SOURCE_IDS, MAX_DIMENSIONS } from '../src/registry.js';
import { subjectToolsStatement, peopleStatement } from '../src/blocks.js';
import { shapes } from '../tools/snapshot.mjs';
import { NOW, WINDOW } from './helpers.mjs';

const SCHEMA = readFileSync(new URL('../../database/schema.sql', import.meta.url), 'utf8');

/** The tool a row counts as, from its fingerprint and the joined catalogue and host rows. */
const toolIdOf = (fingerprint, catalogue, host) => `coalesce('app:' || coalesce(${catalogue}.app_key, ${host}.app_key), ${fingerprint})`;

const TOOL_SOURCES = SOURCE_IDS.filter((id) => SOURCES[id].dimensions.tool);

function selectsOf(source) {
  return [...(source.extraSelect ?? []), ...(source.listSelect ?? [])].map((e) => (typeof e === 'string' ? e : e.sql));
}

/** A view's definition in schema.sql, up to the statement's end. */
function viewSql(name) {
  const start = SCHEMA.indexOf(`CREATE VIEW ${name}\n`);
  assert.ok(start >= 0, `${name} is defined in schema.sql`);
  return SCHEMA.slice(start, SCHEMA.indexOf(';', start));
}

/**
 * Whether a source's `tool` is the tool its rows count as: ops.tool_id() of the fingerprint, the
 * tool_id of rows read through the catalogue and the host view, or a view column schema.sql
 * defines as one.
 */
function isToolId(source) {
  const { sql, fingerprints } = source.dimensions.tool;
  const alias = fingerprints.split('.')[0];
  if (sql === `ops.tool_id(${fingerprints})`) return true;
  if (source.from === 'mart.v_tool_usage t') {
    const view = viewSql('mart.v_tool_usage');
    return sql === 't.tool_id' && view.includes(`${toolIdOf('a.tool_fingerprint', 'c', 'h')} AS tool_id`)
      && view.includes('LEFT JOIN ref.tool_catalogue c') && view.includes('LEFT JOIN mart.v_tool_host h');
  }
  if (source.from === 'mart.v_tool_first_seen f') {
    const view = viewSql('mart.v_tool_first_seen');
    return sql === 'f.tool_fingerprint' && view.includes(`${toolIdOf('a.tool_fingerprint', 'c', 'h')} AS tool_fingerprint`)
      && /GROUP BY 1, 2, 3$/.test(view.trim()) && view.includes('LEFT JOIN mart.v_tool_host h');
  }
  return sql === `${alias}.tool_id`
    && source.from.endsWith(`) ${alias}`)
    && source.from.includes(`${toolIdOf('x.tool_fingerprint', 'xc', 'xh')} AS tool_id`)
    && source.from.includes('LEFT JOIN ref.tool_catalogue xc ON xc.tool_fingerprint = x.tool_fingerprint')
    && source.from.includes('LEFT JOIN mart.v_tool_host xh ON xh.tenant_id = x.tenant_id AND xh.tool_fingerprint = x.tool_fingerprint');
}

/** The clause of a compiled read that starts with `keyword`, to the end of its line. */
function clause(text, keyword) {
  const line = text.split('\n').find((l) => l.trimStart().startsWith(keyword));
  return line ?? '';
}

/** Every grouping that shows a tool: the tool alone and beside each other dimension. */
function toolGroupings(source) {
  const others = Object.keys(source.dimensions).filter((d) => d !== 'tool' && d !== 'bucket');
  const out = [['tool']];
  for (const other of others) out.push(['tool', other]);
  if (MAX_DIMENSIONS >= 3) for (let i = 0; i < others.length; i += 1) for (let j = i + 1; j < others.length; j += 1) out.push(['tool', others[i], others[j]]);
  return out;
}

function requestsFor(id) {
  const source = SOURCES[id];
  const scope = source.requiresSubjectScope ? [{ field: 'subject', op: 'eq', value: 'user_ref_0001' }] : [];
  if (source.kind !== 'aggregate') {
    return [{ query_version: '1', source: id, filters: scope, ...(source.time ? { window: WINDOW } : {}), limit: 20 }];
  }
  const measures = Object.keys(source.measures);
  const out = [];
  for (const dimensions of toolGroupings(source)) {
    for (const [bucket, rollup] of [['day', false], ['week', true]]) {
      out.push({ query_version: '1', source: id, bucket, rollup, dimensions, measures, filters: scope, window: WINDOW, limit: 20 });
    }
    out.push({ query_version: '1', source: id, dimensions, measures, filters: scope, window: WINDOW, limit: 20 });
  }
  return out;
}

function planRead(request) {
  try {
    const planned = plan(request, { now: NOW, actorId: 'tool-identity' });
    return { planned, read: planned.statements.find((s) => s.id === 'read') };
  } catch (error) {
    // The cost guard may refuse a wide grouping; a refused read shows no tool at all.
    assert.equal(error.resultState, 'query_too_broad', `${JSON.stringify(request)}: ${error.message}`);
    return null;
  }
}

test('every source that shows a tool has a tool dimension, and returns no catalogue key beside it', () => {
  for (const id of SOURCE_IDS) {
    const source = SOURCES[id];
    const selects = selectsOf(source);
    if (selects.some((sql) => /AS "?tool"?$/.test(sql))) assert.ok(source.dimensions.tool, `${id} returns a tool it cannot filter or group`);
    assert.ok(!selects.some((sql) => /AS "?tool_key"?$/.test(sql)), `${id} returns a catalogue key: a reader keying on it and on tool would count one tool twice`);
  }
  assert.deepEqual(TOOL_SOURCES, [
    'ingest.submission', 'mart.agg_class_period', 'mart.agg_discovery_period', 'mart.agg_org_period', 'mart.agg_team_period',
    'mart.agg_tool_period', 'mart.agg_tool_user_period', 'mart.v_finding', 'mart.v_tool_first_seen', 'mart.v_tool_usage',
  ], 'a new source that shows a tool is checked here');
});

test('every source\'s tool is the tool its rows count as, with the fingerprint kept for filters', () => {
  for (const id of TOOL_SOURCES) {
    const source = SOURCES[id];
    const tool = source.dimensions.tool;
    assert.match(tool.fingerprints ?? '', /^\w+\.tool_fingerprint$/, `${id}: the tool's raw fingerprint column`);
    assert.ok(isToolId(source), `${id}: tool is ${tool.sql}, which is not the tool its rows count as`);
    const selects = selectsOf(source);
    if (source.kind !== 'aggregate') assert.ok(selects.includes(`${tool.sql} AS "tool"`), `${id} lists the tool by ${tool.sql}`);
    const name = selects.find((sql) => /AS "?tool_name"?$/.test(sql));
    assert.ok(name, `${id} names its tool`);
    assert.ok(
      [`ops.tool_display_name(${tool.sql})`, `ops.tool_display_name(${tool.fingerprints})`].some((e) => name.startsWith(e)),
      `${id} names the tool from its id or fingerprint (ops.tool_display_name resolves either to the tool's name)`,
    );
  }
});

test('every read that shows a tool selects, groups, orders and pages it by the tool, never by a fingerprint', () => {
  let reads = 0;
  for (const id of TOOL_SOURCES) {
    const source = SOURCES[id];
    const { sql, fingerprints } = source.dimensions.tool;
    for (const request of requestsFor(id)) {
      const result = planRead(request);
      if (!result) continue;
      reads += 1;
      const { text } = result.read;
      const label = `${id} ${JSON.stringify(request.dimensions ?? [])} ${request.bucket ?? 'list'}`;
      const select = text.slice(0, text.indexOf('\n  FROM '));
      assert.ok(select.includes(`${sql} AS "tool"`), `${label}: returns ${sql} as the tool`);
      assert.ok(!/AS "?tool_key"?/.test(select), `${label}: returns a catalogue key`);
      if (sql !== fingerprints) {
        assert.ok(!select.includes(`${fingerprints} AS`), `${label}: returns a raw fingerprint`);
        const group = clause(text, 'GROUP BY');
        if (source.kind === 'aggregate') assert.ok(group.includes(sql), `${label}: groups by the tool`);
        assert.ok(!group.includes(fingerprints), `${label}: groups by a fingerprint, so one tool is several rows`);
        assert.ok(!clause(text, 'ORDER BY').includes(fingerprints), `${label}: orders by a fingerprint`);
      }
      if (source.kind === 'aggregate') {
        // Two fingerprints of one tool in a cell are two rows of people: a count of them is a floor.
        const users = result.planned.meta.measure_semantics?.users;
        if (users) assert.equal(users, 'distinct_lower_bound', `${label}: people summed across a tool's fingerprints are exact`);
      }
    }
  }
  assert.ok(reads >= TOOL_SOURCES.length * 3, `only ${reads} reads compiled`);
});

test('a filter on a tool matches every fingerprint of the tools it names', () => {
  for (const id of TOOL_SOURCES) {
    const { fingerprints } = SOURCES[id].dimensions.tool;
    const [request] = requestsFor(id);
    const members = `${fingerprints} = ANY ((SELECT ops.tool_fingerprints(`;
    for (const [op, value, negated] of [['eq', 'app:chatgpt_web', false], ['in', ['app:chatgpt_web', 'tls_x'], false], ['ne', 'app:chatgpt_web', true], ['not_in', ['app:chatgpt_web'], true]]) {
      const result = planRead({ ...request, filters: [...request.filters, { field: 'tool', op, value }] });
      assert.ok(result, `${id} ${op} compiles`);
      const { text } = result.read;
      assert.ok(text.includes(negated ? `NOT (${members}` : members), `${id}: tool ${op} matches the tool's every fingerprint`);
    }
  }
});

test('every template groups and orders a tool by the tool, and no statement counts fingerprints as tools', () => {
  for (const shape of shapes()) {
    const planned = plan(shape.request, { now: NOW, actorId: 'tool-identity' });
    const tool = SOURCES[planned.meta.source]?.dimensions.tool;
    for (const statement of planned.statements) {
      const label = `${shape.name} ${statement.id}`;
      if (statement.id === 'read' && tool && tool.sql !== tool.fingerprints) {
        for (const keyword of ['GROUP BY', 'ORDER BY']) {
          assert.ok(!clause(statement.text, keyword).includes(tool.fingerprints), `${label}: ${keyword} a fingerprint`);
        }
      }
      assert.ok(!/count\(DISTINCT \w+\.tool_fingerprint\)/.test(statement.text), `${label}: counts fingerprints as tools`);
    }
  }
});

test('a person\'s tools and the people by sanction read each tool as the tool lists do', () => {
  const window = { from: WINDOW.from, to: WINDOW.to };
  const tools = subjectToolsStatement('user_ref_0001', { window, bucket: 'day' }).text;
  assert.ok(tools.includes(`count(DISTINCT ${toolIdOf('u.tool_fingerprint', 'c', 'h')})`), 'each tool once, however many fingerprints');
  assert.ok(tools.includes('LEFT JOIN mart.v_tool_host h ON h.tenant_id = u.tenant_id AND h.tool_fingerprint = u.tool_fingerprint'));
  const people = peopleStatement({ window, bucket: 'day' }).text;
  assert.ok(people.includes('s.tool_key = coalesce(c.app_key, h.app_key)'), 'a site an app\'s domain covers carries the app\'s decision');
});

test('Q2 joins the present-tense sanction of the tool a row counts as, and filters on it', () => {
  const planned = plan({ query_version: '1', template: 'q2_unsanctioned_users', params: { window: WINDOW, limit: 100 } }, { now: NOW, actorId: 'tool-identity' });
  const { text } = planned.statements.find((s) => s.id === 'read');
  assert.match(text, /LEFT JOIN ops\.tool_sanction ot ON ot\.tenant_id = a\.tenant_id AND ot\.tool_key = a\.tool_key/);
  assert.match(text, /ot\.sanctioned_state = \$\d+::text/);
});

test('the display name is selected only when tool is grouped, so an unrelated grouping still compiles', () => {
  const classMix = plan({ query_version: '1', template: 'q4_class_mix', params: { window: WINDOW, limit: 100 } }, { now: NOW, actorId: 'tool-identity' });
  assert.ok(!classMix.statements.find((s) => s.id === 'read').text.includes('tool_name'), 'q4 must not select a name it cannot group');
});
