// parity.test.mjs — what the dashboard mirrors of query-api must equal query-api.
//
// vocab.js mirrors the closed DSL so a mistake is caught in the browser rather than as a 400, and
// the server's role map decides which pages a role is offered. A mirror that drifts would let the
// client build a request the API refuses, or offer a page the API will not serve. The imports are
// one-directional: the dashboard's tests read query-api's modules, never the other way round.

import test from 'node:test';
import assert from 'node:assert/strict';
import { SOURCES, TEMPLATES, TEMPLATE_NAMES, RESULT_STATES, K, STATE_PAIRS } from '../src/vocab.js';
import { ROLE_CAPABILITIES } from '../server/session.mjs';
import * as registry from '../../query-api/src/registry.js';
import * as serverTemplates from '../../query-api/src/templates.js';
import * as serverErrors from '../../query-api/src/errors.js';
import * as serverRoles from '../../query-api/src/roles.js';

test('every source, with its kind, dimensions and measures, matches the registry', () => {
  assert.deepEqual(Object.keys(SOURCES).sort(), Object.keys(registry.SOURCES).sort());
  for (const [name, spec] of Object.entries(SOURCES)) {
    const server = registry.SOURCES[name];
    assert.equal(spec.kind, server.kind, `${name} kind`);
    assert.deepEqual([...spec.dimensions].sort(), Object.keys(server.dimensions).sort(), `${name} dimensions`);
    assert.deepEqual([...spec.measures].sort(), Object.keys(server.measures).sort(), `${name} measures`);
  }
});

test('the ten templates, their sources and their parameters match', () => {
  assert.deepEqual([...TEMPLATE_NAMES].sort(), Object.keys(serverTemplates.TEMPLATES).sort());
  for (const name of TEMPLATE_NAMES) {
    assert.deepEqual([...TEMPLATES[name].params].sort(), [...serverTemplates.TEMPLATES[name].params].sort(), `${name} parameters`);
    assert.equal(TEMPLATES[name].source, serverTemplates.TEMPLATES[name].source, `${name} source`);
  }
});

test('the result-state vocabulary is the API\'s', () => {
  assert.deepEqual(Object.keys(RESULT_STATES).sort(), [...serverErrors.RESULT_STATE_NAMES].sort());
});

test('k is the same number on both sides', () => {
  assert.equal(K, registry.K);
});

test('the role-to-capability map is the one query-api enforces', () => {
  assert.deepEqual(ROLE_CAPABILITIES, serverRoles.ROLE_CAPABILITIES);
});

test('the state pairs the client will not merge are the ones the schema constrains', () => {
  assert.deepEqual(STATE_PAIRS.liveness, ['reporting', 'stale', 'never_reported', 'revoked']);
  assert.deepEqual(STATE_PAIRS.collector_state, ['healthy', 'degraded', 'absent', 'tampered']);
  assert.deepEqual(STATE_PAIRS.content_state, ['not_captured', 'local_only', 'uploaded', 'shredded']);
  assert.deepEqual(STATE_PAIRS.sanctioned_state, ['sanctioned', 'unsanctioned', 'unknown']);
  assert.deepEqual(STATE_PAIRS.review_state, ['open', 'disputed', 'confirmed']);
});
