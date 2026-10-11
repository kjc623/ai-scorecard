/**
 * test/tool-capture.test.mjs — every web tool's recorded submission, replayed through the extension
 * as Chrome hands it over, captures exactly as its fixture says.
 *
 * Each fixture in test/tools/ states how many events one submission becomes, whether its prompt is
 * observed, attributed to the tool and stored as typed, and why not where it is not. The check is exact in both directions: a change
 * that breaks a tool fails here, and so does one that fixes a tool, until its fixture says so. Run
 * `node tools/tool-capture-report.mjs` for the same replay as a table.
 */

import test from 'node:test';
import assert from 'node:assert/strict';

import { extensionCatalogue, loadFixtures, replay } from '../test-support/tool-replay.mjs';

const catalogue = extensionCatalogue();
const fixtures = loadFixtures();

test('every web tool in the app catalogue has a capture fixture', () => {
  const covered = new Set(fixtures.map((f) => f.app_key));
  for (const { app_key, display_name } of catalogue.values()) {
    assert.ok(covered.has(app_key), `${display_name} (${app_key}) has no fixture in test/tools/`);
  }
});

for (const fixture of fixtures) {
  test(`${fixture.tool}: ${describe(fixture.expect)}`, async () => {
    for (const key of ['tool', 'app_key', 'typed', 'request', 'expect']) assert.ok(fixture[key], `${fixture.file} names its ${key}`);
    assert.ok(['documented', 'captured'].includes(fixture.source?.kind), `${fixture.file} says whether it was captured or written from documentation`);
    const expected = fixture.expect;
    if (!expected.observed || !expected.attributed || !expected.prompt || expected.events !== 1) assert.ok(expected.why, `${fixture.file} says why the tool is not fully captured`);

    const got = await replay(fixture, { catalogue });
    const events = got.events.map((e) => `${e.route} ${e.named ?? 'not catalogued'} ${JSON.stringify(e.content)}`).join('; ');
    const detail = `score ${got.score} [${got.signals.join(', ')}], events: ${events || 'none'}`;
    assert.deepEqual(
      { events: got.events.length, observed: got.observed, attributed: got.attributed, prompt: got.prompt },
      { events: expected.events, observed: expected.observed, attributed: expected.attributed, prompt: expected.prompt },
      `${fixture.tool}: ${detail}`,
    );
  });
}

function describe(expect) {
  if (!expect.observed) return 'not captured';
  if (expect.events === 1 && expect.attributed && expect.prompt) return 'captured once, attributed and stored as typed';
  const missing = [
    expect.events !== 1 && `${expect.events} events`,
    !expect.attributed && 'not attributed',
    !expect.prompt && 'prompt not stored as typed',
  ].filter(Boolean);
  return `captured, ${missing.join(', ')}`;
}
