// parity.test.mjs — the client's mirror of the closed DSL must equal the server's registry.
//
// The dashboard's vocab.js is a *mirror*: it exists so a mistake is caught in the browser rather
// than as a 400. A mirror that drifts is worse than no mirror, because it would let the client
// build a request the API refuses and call it valid. This test imports the read path's own
// registry and compares name for name.
//
// It is the one cross-package import in this suite, and it is deliberately one-directional: the
// dashboard reads the query API's registry, never the other way round.

import test from 'node:test';
import assert from 'node:assert/strict';
import { existsSync } from 'node:fs';
import { join } from 'node:path';
import { OPERATORS, SOURCES, TEMPLATES, TEMPLATE_NAMES, RESULT_STATE_NAMES, K } from '../src/vocab.js';
import { STATE_PAIRS, GAP_REASONS } from '../src/vocab.js';
import { REPO } from './helpers.mjs';

const REGISTRY = join(REPO, 'services', 'query-api', 'src', 'registry.js');
const SERVER_TEMPLATES = join(REPO, 'services', 'query-api', 'src', 'templates.js');
const SERVER_ERRORS = join(REPO, 'services', 'query-api', 'src', 'errors.js');
const asUrl = (path) => `file://${path.replace(/\\/g, '/')}`;
const registry = existsSync(REGISTRY) ? await import(asUrl(REGISTRY)) : null;
const serverTemplates = existsSync(SERVER_TEMPLATES) ? await import(asUrl(SERVER_TEMPLATES)) : null;
const serverErrors = existsSync(SERVER_ERRORS) ? await import(asUrl(SERVER_ERRORS)) : null;

test('the query-api registry is present to compare against', { skip: registry ? false : 'services/query-api/src/registry.js is not on disk' }, () => {
  assert.ok(registry.SOURCES, 'the registry exports SOURCES');
});

test('every source name matches the read path exactly', { skip: registry ? false : 'registry absent' }, () => {
  assert.deepEqual(Object.keys(SOURCES).sort(), Object.keys(registry.SOURCES).sort());
  for (const [name, spec] of Object.entries(SOURCES)) {
    const server = registry.SOURCES[name];
    assert.equal(spec.kind, server.kind, `${name} kind`);
    assert.deepEqual([...spec.dimensions].sort(), Object.keys(server.dimensions).sort(), `${name} dimensions`);
    assert.deepEqual([...spec.measures].sort(), Object.keys(server.measures).sort(), `${name} measures`);
  }
});

test('the operator vocabulary matches, including what is absent', { skip: registry ? false : 'registry absent' }, () => {
  assert.deepEqual([...OPERATORS], [...registry.OPERATORS]);
  assert.ok(!OPERATORS.includes('regexp'));
  assert.ok(OPERATORS.includes('starts_with'));
});

test('the ten templates and their parameters match', { skip: serverTemplates ? false : 'registry absent' }, () => {
  assert.deepEqual([...TEMPLATE_NAMES].sort(), Object.keys(serverTemplates.TEMPLATES).sort());
  for (const name of TEMPLATE_NAMES) {
    assert.deepEqual(
      [...TEMPLATES[name].params].sort(),
      [...serverTemplates.TEMPLATES[name].params].sort(),
      `${name} parameters`,
    );
    assert.equal(TEMPLATES[name].source, serverTemplates.TEMPLATES[name].source, `${name} source`);
  }
});

test('the result-state vocabulary is the API\'s, not an invented one', { skip: serverErrors ? false : 'registry absent' }, () => {
  assert.deepEqual([...RESULT_STATE_NAMES].sort(), [...serverErrors.RESULT_STATE_NAMES].sort());
});

test('k is the same number on both sides', { skip: registry ? false : 'registry absent' }, () => {
  assert.equal(K, registry.K);
  assert.equal(K, registry.SUPPRESSION_K);
});

test('the state pairs the client will not merge are the ones the schema constrains', () => {
  assert.deepEqual(STATE_PAIRS.liveness, ['reporting', 'stale', 'never_reported', 'revoked']);
  assert.deepEqual(STATE_PAIRS.collector_state, ['healthy', 'degraded', 'absent', 'tampered']);
  assert.deepEqual(STATE_PAIRS.content_state, ['not_captured', 'local_only', 'uploaded', 'shredded']);
  assert.deepEqual(STATE_PAIRS.sanctioned_state, ['sanctioned', 'unsanctioned', 'unknown']);
  assert.deepEqual(STATE_PAIRS.review_state, ['open', 'disputed', 'confirmed']);
  assert.equal(GAP_REASONS.includes('unknown'), true, 'unknown is a reason value, not a blank');
  assert.equal(GAP_REASONS.length, 8);
});
