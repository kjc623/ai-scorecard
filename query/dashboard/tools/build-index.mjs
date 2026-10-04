// build-index.mjs — inline the module graph into apps/dashboard/index.html.
//
// WHY THIS EXISTS. Browsers refuse an external ES module fetch from a `file://` page: the module
// request is cross-origin from origin `null`, so `<script type="module" src="./src/app.js">` fails
// in every mainstream browser when the page is opened straight off disk. The requirement is that a
// human can open the dashboard and see every state, so index.html carries the whole graph in one
// inline module block: an inline module performs no fetch, and `file://` loading works.
//
// This is not a build step for the application. `src/*.js` are the source of truth and are what the
// tests import; index.html is a *snapshot* of them, and `test/index-html.test.mjs` regenerates it
// and fails if it has drifted. Anyone editing src/ runs `node tools/build-index.mjs` and commits
// both, exactly as they would for any generated artefact with a drift gate.
//
//   node tools/build-index.mjs            # regenerate index.html
//   node tools/build-index.mjs --check    # fail if index.html is stale

import { readFileSync, writeFileSync, existsSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const ROOT = join(HERE, '..');
export const INDEX_PATH = join(ROOT, 'index.html');
const TEMPLATE_PATH = join(HERE, 'index.template.html');

/** Dependency order. A module may only reference names declared earlier in this list. */
export const MODULE_ORDER = Object.freeze([
  'vocab.js',
  'format.js',
  'states.js',
  'dsl.js',
  'transport.js',
  'fixtures.js',
  'scenarios.js',
  'questions.js',
  'views.js',
  'unavailable.js',
  'render.js',
  'app.js',
]);

/** Names the epilogue exports, so a test can import the inline module and drive it. */
const PUBLIC_NAMES = Object.freeze([
  'boot', 'createDashboard', 'parseHash', 'SCREENS', 'SCREEN_IDS',
  'readState', 'measureOf', 'isSuppressed', 'sumMeasure', 'renderScreen', 'renderValue',
  'createQueryApi', 'createStubTransport', 'scenarioTransport', 'SCENARIO_NAMES',
  'QUESTIONS', 'buildDocument', 'buildTemplate', 'GAPS',
]);

/** A page this tool generates: its module graph, what it exports, and how it starts. */
const INDEX_PAGE = Object.freeze({
  order: MODULE_ORDER,
  publicNames: PUBLIC_NAMES,
  bootCall: "boot({ document, scenario: 'realistic' });",
  template: TEMPLATE_PATH,
});

/** explore.html: the search page. It shares the data layer and carries only the modules it uses. */
export const EXPLORE_PATH = join(ROOT, 'explore.html');
export const EXPLORE_MODULE_ORDER = Object.freeze([
  'vocab.js',
  'format.js',
  'states.js',
  'dsl.js',
  'transport.js',
  'fixtures.js',
  'questions.js',
  'views.js',
  'render.js',
  'explore-model.js',
  'explore-stub.js',
  'explore-render.js',
  'explore-app.js',
]);
const EXPLORE_PAGE = Object.freeze({
  order: EXPLORE_MODULE_ORDER,
  publicNames: Object.freeze([
    'bootExplore', 'createExplorer', 'createExploreStub', 'createQueryApi', 'readState',
    'EXPLORE_DATASETS', 'parseExploreQuery', 'renderExploreResults', 'renderExploreDetail',
  ]),
  bootCall: 'bootExplore({ document });',
  template: join(HERE, 'explore.template.html'),
});

/** Strip the module syntax and return the body, plus the top-level names it declares. */
function stripModule(source, name) {
  let body = source;
  // import statements, single- or multi-line
  body = body.replace(/^import\s[\s\S]*?from\s*'[^']*';\s*$/gm, '');
  // `export { a, b };` re-export blocks
  body = body.replace(/^export\s*\{[\s\S]*?\};\s*$/gm, '');
  // `export default` is not used anywhere in this package, and would not survive concatenation
  if (/^export\s+default\b/m.test(body)) {
    throw new Error(`${name}: export default cannot be inlined; use a named export.`);
  }
  // the `export` keyword on declarations
  body = body.replace(/^export\s+(?=(const|let|var|function|class|async)\b)/gm, '');
  if (/^\s*import\s/m.test(body)) throw new Error(`${name}: an import survived stripping.`);
  if (/^export\s/m.test(body)) throw new Error(`${name}: an export survived stripping.`);

  const names = new Set();
  for (const m of body.matchAll(/^(?:const|let|var|function|class|async function)\s+([A-Za-z_$][\w$]*)/gm)) names.add(m[1]);
  return { body, names: [...names] };
}

/** The inlined module body. Throws on a name collision, because concatenation has no scopes. */
export function buildInlineModule(page = INDEX_PAGE) {
  const seen = new Map();
  const parts = [];
  for (const name of page.order) {
    const path = join(ROOT, 'src', name);
    if (!existsSync(path)) throw new Error(`src/${name} is missing.`);
    const { body, names } = stripModule(readFileSync(path, 'utf8'), name);
    for (const declared of names) {
      if (seen.has(declared)) {
        throw new Error(`top-level name collision: "${declared}" is declared in both ${seen.get(declared)} and ${name}. Rename one before inlining.`);
      }
      seen.set(declared, name);
    }
    parts.push(`// ─── src/${name} ${'─'.repeat(Math.max(0, 62 - name.length))}\n${body.trim()}\n`);
  }
  const missing = page.publicNames.filter((n) => !seen.has(n));
  if (missing.length > 0) throw new Error(`the epilogue exports names no module declares: ${missing.join(', ')}`);

  return [
    '// GENERATED by tools/build-index.mjs — edit src/*.js and regenerate. Drift is a test failure.',
    '// One inline module, so this page opens from the filesystem: a browser refuses an external',
    '// module fetch from file://, and an inline module fetches nothing.',
    '',
    ...parts,
    '// ─── epilogue ─────────────────────────────────────────────────────────────────',
    'let __booted = false;',
    'function __boot() {',
    '  if (__booted) return;',
    '  __booted = true;',
    `  ${page.bootCall}`,
    '}',
    "if (typeof document !== 'undefined') {",
    "  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', __boot);",
    '  else __boot();',
    '}',
    '',
    `export { ${page.publicNames.join(', ')} };`,
    '',
  ].join('\n');
}

function buildPageHtml(page) {
  const template = readFileSync(page.template, 'utf8');
  const marker = '<!--__INLINE_MODULE__-->';
  if (!template.includes(marker)) throw new Error(`${page.template} has no <!--__INLINE_MODULE__--> marker.`);
  const script = `<script type="module">\n${buildInlineModule(page)}</script>`;
  // A function replacement, so a `$` in the inlined source is never read as a replacement pattern.
  return template.replace(marker, () => script);
}

export function buildIndexHtml() {
  return buildPageHtml(INDEX_PAGE);
}

export function buildExploreHtml() {
  return buildPageHtml(EXPLORE_PAGE);
}

const invokedDirectly = process.argv[1]?.endsWith('build-index.mjs') ?? false;
if (invokedDirectly) {
  const pages = [
    { name: 'index.html', path: INDEX_PATH, html: buildIndexHtml(), modules: MODULE_ORDER.length },
    { name: 'explore.html', path: EXPLORE_PATH, html: buildExploreHtml(), modules: EXPLORE_MODULE_ORDER.length },
  ];
  if (process.argv.includes('--check')) {
    let stale = false;
    for (const page of pages) {
      const current = existsSync(page.path) ? readFileSync(page.path, 'utf8').replace(/\r\n/g, '\n') : '';
      if (current !== page.html) {
        console.error(`${page.name} is stale: run \`node tools/build-index.mjs\` from query/dashboard.`);
        stale = true;
      } else {
        console.log(`${page.name} is in sync (${page.html.length} bytes, ${page.modules} modules inlined)`);
      }
    }
    process.exit(stale ? 1 : 0);
  }
  for (const page of pages) {
    writeFileSync(page.path, page.html);
    console.log(`wrote ${page.name} (${page.html.length} bytes, ${page.modules} modules inlined)`);
  }
}
