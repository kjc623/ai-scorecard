// probe.mjs — render every screen through the built index.html and print what came out.
//
// This is the evidence for the acceptance criterion "opening index.html renders every state": it
// imports the same inline module a browser would run, drives it against a fake document, and
// reports, per scenario and screen, the result state and whether the page carries the coverage
// strip. `node tools/probe.mjs`
//
// Not a test — test/index-html.test.mjs is the assertion. This is the readout a human wants when
// something looks wrong.

import { fakeDocument, importInlineModule } from '../test/helpers.mjs';
import { SCENARIO_NAMES } from '../src/fixtures.js';
import { SCREENS } from '../src/app.js';

const mod = await importInlineModule();
const doc = fakeDocument('#posture');
const { render } = await mod.boot({ document: doc, scenario: 'realistic' });
await render();

const pad = (s, n) => String(s).padEnd(n);
console.log(`${pad('scenario', 18)} ${pad('screen', 20)} ${pad('state', 20)} strip  bytes`);
console.log('-'.repeat(80));

let rows = 0;
let stripped = 0;
for (const scenario of SCENARIO_NAMES) {
  await doc.navigate(`#gallery/${scenario}`);
  for (const screen of SCREENS) {
    await doc.navigate(`#${screen.id}`);
    const html = doc.html('app');
    const state = /class="strip strip-([a-z_]+)"/.exec(html)?.[1]
      ?? (/class="needs"/.test(html) ? 'needs-input' : 'gallery');
    const hasStrip = /<div class="strip/.test(html);
    rows += 1;
    if (hasStrip) stripped += 1;
    console.log(`${pad(scenario, 18)} ${pad(screen.id, 20)} ${pad(state, 20)} ${hasStrip ? 'yes  ' : 'no   '} ${html.length}`);
  }
}
console.log('-'.repeat(80));
console.log(`${rows} screen renders, ${stripped} carrying the coverage strip`);

// The two states the whole exercise is about, printed side by side.
await doc.navigate('#gallery/realistic');
await doc.navigate('#tools');
const realistic = doc.html('app');
const suppressedCell = /<tr class="row-suppressed">[\s\S]*?<\/tr>/.exec(realistic)?.[0] ?? '(none)';
const zeroCell = /<tr>(?:(?!<\/tr>)[\s\S])*?<span class="v-number">0<\/span>[\s\S]*?<\/tr>/.exec(realistic)?.[0] ?? '(none)';
console.log('\na suppressed cell renders as:');
console.log('  ' + suppressedCell.replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim());
console.log('a genuine zero renders as:');
console.log('  ' + zeroCell.replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim());
