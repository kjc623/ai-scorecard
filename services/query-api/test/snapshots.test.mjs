// snapshots.test.mjs — the compiled SQL is pinned, so a change to the compiler is a diff.
//
// It fails when the compiler's output changes in any way, including in a way that would be easy to
// miss in review (a reordered tie-break, a lost cast, a bucket predicate that disappeared).

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { buildSnapshot, serialise, SNAPSHOT_PATH } from '../tools/snapshot.mjs';

test('test/snapshots/compiled-sql.json matches the current compiler output exactly', () => {
  const current = readFileSync(SNAPSHOT_PATH, 'utf8').replace(/\r\n/g, '\n');
  const expected = serialise(buildSnapshot());
  assert.equal(
    current,
    expected,
    'compiled SQL changed; if the change is intended, regenerate with: node tools/snapshot.mjs',
  );
});

test('the snapshot covers all ten templates and every registered source', () => {
  const snapshot = JSON.parse(readFileSync(SNAPSHOT_PATH, 'utf8'));
  const names = Object.keys(snapshot);
  for (let q = 1; q <= 10; q += 1) {
    assert.ok(names.some((n) => n.startsWith(`template:q${q}_`)), `question ${q} is snapshotted`);
  }
  for (const name of names) {
    assert.ok(snapshot[name].statements.length > 0, `${name} has statements`);
    for (const statement of snapshot[name].statements) {
      assert.match(statement.text, /^[\x20-\x7e\n\t]*$/, `${name}/${statement.id}`);
      assert.ok(!statement.text.includes(';'), `${name}/${statement.id} contains a statement terminator`);
    }
  }
});

test('no snapshot statement contains a value from its own parameters', () => {
  const snapshot = JSON.parse(readFileSync(SNAPSHOT_PATH, 'utf8'));
  for (const [name, entry] of Object.entries(snapshot)) {
    for (const statement of entry.statements) {
      for (const value of statement.params) {
        if (typeof value !== 'string' || value.length < 6) continue;
        if (value.startsWith('00000000-') || value.includes('2026-') || value.includes('2025-')) {
          assert.ok(!statement.text.includes(value), `${name}/${statement.id} interpolated ${value}`);
        }
      }
    }
  }
});
