#!/usr/bin/env node
// Replays every web tool's fixture in test/tools/ through the extension and prints what reached
// capture-core, one row per tool, as a Markdown table.
//
//   node tools/tool-capture-report.mjs           # the table
//   node tools/tool-capture-report.mjs --json    # the same, as JSON
//
// A row whose result differs from its fixture's expectation is marked, and the exit code is 1.

import { extensionCatalogue, loadFixtures, replay } from '../test-support/tool-replay.mjs';

const catalogue = extensionCatalogue();
const rows = [];
for (const fixture of loadFixtures()) {
  const got = await replay(fixture, { catalogue });
  const expected = fixture.expect;
  const asExpected = got.events.length === expected.events
    && got.observed === expected.observed
    && got.attributed === expected.attributed
    && got.prompt === expected.prompt;
  rows.push({ fixture: fixture.file, tool: fixture.tool, source: fixture.source.kind, ...got, as_expected: asExpected, why: expected.why || '' });
}

if (process.argv.includes('--json')) {
  console.log(JSON.stringify(rows, null, 2));
} else {
  const mark = (b) => (b ? 'yes' : 'no');
  console.log('| Tool | Fixture | Score | Events | Observed | Attributed | Prompt as typed | Matches fixture |');
  console.log('|---|---|---|---|---|---|---|---|');
  for (const r of rows) {
    console.log(`| ${r.tool} | ${r.source} | ${r.score} | ${r.events.length} | ${mark(r.observed)} | ${mark(r.attributed)} | ${mark(r.prompt)} | ${r.as_expected ? 'yes' : '**no**'} |`);
  }
  for (const r of rows.filter((x) => x.why)) console.log(`\n${r.tool}: ${r.why}`);
}
process.exit(rows.every((r) => r.as_expected) ? 0 : 1);
