/**
 * contract.test.mjs — checks about the package rather than a behaviour: the manifest is valid MV3,
 * the tree calls `chrome.*` in exactly two modules, and the protocol vocabulary matches
 * `endpoint/protocol`.
 */

import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const REPO_ROOT = resolve(HERE, '..');

const manifest = JSON.parse(readFileSync(join(HERE, 'manifest.json'), 'utf8'));

test('manifest.json parses as JSON and declares MV3', () => {
  assert.equal(manifest.manifest_version, 3, 'Chromium loads MV3 extensions only');
  assert.equal(typeof manifest.version, 'string', 'Chrome requires the version as a string of 1-4 integers');
  assert.match(manifest.version, /^\d+(\.\d+){0,3}$/);
  assert.match(manifest.name, /\S/);
  assert.match(manifest.description, /\S/);
  assert.ok(manifest.description.length <= 132, 'Chrome truncates a longer description');
});

test('the service worker is an ES module, which is what lets `src/` be plain ES modules', () => {
  assert.equal(manifest.background.service_worker, 'background/service-worker.js');
  assert.equal(manifest.background.type, 'module', 'without "type": "module" the static imports fail at load');
  assert.ok(
    readFileSync(join(HERE, manifest.background.service_worker), 'utf8').includes('import '),
    'the entry point must actually use the module form',
  );
});

test('webRequestBlocking is requested: a force-installed extension keeps it, and blocking depends on it', () => {
  assert.ok(manifest.permissions.includes('webRequest'));
  assert.ok(manifest.permissions.includes('webRequestBlocking'), 'blocked cancels only through webRequestBlocking');
  assert.ok(manifest.permissions.includes('nativeMessaging'), 'native messaging is the transport to capture-core');
});

test('the permission list is exactly the minimum, with no permission present that no module uses', () => {
  // Every permission here has a consumer. `storage` is absent: the extension holds nothing durable.
  assert.deepEqual(
    [...manifest.permissions].sort(),
    ['alarms', 'nativeMessaging', 'tabs', 'webRequest', 'webRequestBlocking'],
  );
  assert.equal(manifest.permissions.includes('storage'), false, 'no durable storage in this extension');
  assert.equal(manifest.permissions.includes('<all_urls>'), false, 'host permissions are not a `permissions` member');
});

test('broad observation is declared as a host permission', () => {
  assert.deepEqual(manifest.host_permissions, ['<all_urls>']);
  assert.ok(Array.isArray(manifest.content_scripts) && manifest.content_scripts.length === 1);
  assert.deepEqual(manifest.content_scripts[0].matches, ['<all_urls>']);
  assert.equal(manifest.content_scripts[0].all_frames, true);
  assert.equal(manifest.content_scripts[0].run_at, 'document_start', 'the drop listener must be installed before the page runs');
});

test('the content script is isolated-world only: it must not run in the page (MAIN)', () => {
  const cs = manifest.content_scripts[0];
  assert.equal(cs.world === undefined || cs.world === 'ISOLATED', true, 'a page must not be able to reach the extension');
});

test('the declared content script is a classic script, and contains no static import', () => {
  // Chromium loads a declared content script as a classic script (the `content_scripts` entry has no
  // `type` field), so one static import is a syntax error that silently disables the script. A
  // suite that imports the module the ordinary way cannot see that.
  const cs = manifest.content_scripts[0];
  const declared = cs.js[0];
  const source = readFileSync(join(HERE, declared), 'utf8');
  assert.equal(
    /^\s*import\s/m.test(source),
    false,
    `${declared} is loaded as a classic script: a single static import makes it a syntax error`,
  );
  assert.equal(cs.type, undefined, 'there is no "type" field for a content script; its presence only looks like a fix');
  // It must still reach the modules, or it would be a classic script that does nothing.
  assert.match(source, /import\(/, `${declared} must load its modules with a dynamic import()`);
  const used = [...source.matchAll(/getURL\('([^']+)'\)/g)].map((m) => m[1]);
  assert.ok(used.length > 0, `${declared} must name the extension files it loads`);
  const war = (manifest.web_accessible_resources || []).flatMap((w) => w.resources || []);
  for (const file of used) {
    assert.ok(war.includes(file), `${file} is dynamically imported and must be web-accessible`);
  }
});

test('the content script modules the script imports are web-accessible, which Chrome requires', () => {
  const war = (manifest.web_accessible_resources || []).flatMap((w) => w.resources || []);
  for (const needed of ['src/chrome-adapter.js', 'src/adapter.js', 'src/attachments/files.js', 'src/attachments/sender.js', 'src/codec.js', 'src/messages.js']) {
    assert.ok(war.includes(needed), `${needed} must be listed or the content script's import fails in a browser`);
  }
});

test('the native messaging host name is the one the installers register', () => {
  const src = readFileSync(join(HERE, 'src', 'native.js'), 'utf8');
  assert.match(src, /export const NATIVE_APP = 'com\.shadowaicapture\.capture_core'/);
  assert.match(src, /com\.shadowaicapture\.capture_core/);
});

test('the manifest declares no remote code, no eval and no externally_connectable surface', () => {
  const raw = readFileSync(join(HERE, 'manifest.json'), 'utf8');
  assert.equal(/content_security_policy/.test(raw), false, 'the MV3 default CSP is what we want; do not loosen it');
  assert.equal(/externally_connectable/.test(raw), false, 'no page may speak to this extension');
  assert.equal(/https?:\/\//.test(raw.replace(/"[^"]*<all_urls>[^"]*"/g, '')), false, 'no remote URL is referenced');
});

test('every JSON file in the package parses', () => {
  for (const file of walk(HERE).filter((f) => f.endsWith('.json'))) {
    const text = readFileSync(file, 'utf8');
    assert.doesNotThrow(() => JSON.parse(text), `${relative(HERE, file)} must be valid JSON`);
  }
});

test('`chrome.` appears in exactly two modules: the adapter and its browser binding', () => {
  // `adapter.js` names the global once and documents the surface; `chrome-adapter.js` is the only
  // place chrome.* is called.
  const allowed = new Set(['src/adapter.js', 'src/chrome-adapter.js']);
  const offenders = [];
  for (const file of walk(join(HERE, 'src')).concat([join(HERE, 'background', 'service-worker.js'), join(HERE, 'content', 'content-script.js')])) {
    if (!file.endsWith('.js')) continue;
    const rel = relative(HERE, file).replace(/\\/g, '/');
    const text = readFileSync(file, 'utf8');
    // Ignore the documented-surface comment block and the MV3 entry-point guard.
    const code = text
      .replace(/\/\*[\s\S]*?\*\//g, '')
      .replace(/^\s*\/\/.*$/gm, '')
      .replace(/chrome\.runtime && typeof chrome\.runtime\.connectNative === 'function'/g, '');
    if (/\bchrome\s*\./.test(code) && !allowed.has(rel)) offenders.push(rel);
  }
  assert.deepEqual(offenders, [], 'only src/adapter.js and src/chrome-adapter.js may call chrome.*');
});

test('the extension ships no runtime dependency and its sources are ES modules', () => {
  const pkg = JSON.parse(readFileSync(join(HERE, 'package.json'), 'utf8'));
  assert.equal(pkg.dependencies, undefined, 'the browser loads these files as they are: no bundler, no runtime dependency');
  assert.equal(pkg.type, 'module', '.js files in this package are ES modules, as the manifest requires');
});

test('the protocol vocabulary is a verbatim transcription of endpoint/protocol', () => {
  // Not a spelling check: native.go owns these strings, and a divergence here would be a wire
  // mismatch that only shows up against a real capture-core.
  const go = readFileSync(join(REPO_ROOT, 'endpoint', 'protocol', 'native.go'), 'utf8');
  const types = [...go.matchAll(/Type\w+\s*=\s*"([a-z_]+)"/g)].map((m) => m[1]);
  assert.ok(types.length >= 12, 'native.go must declare both directions');
  const messages = readFileSync(join(HERE, 'src', 'messages.js'), 'utf8');
  for (const t of types) {
    assert.ok(messages.includes(`'${t}'`), `message type ${t} from native.go must appear in messages.js`);
  }

  const reasons = [...go.matchAll(/RefusalReason\s*=\s*"([a-z_]+)"/g)].map((m) => m[1]);
  assert.equal(reasons.length, 7, 'the refusal set is closed at seven');
  for (const r of reasons) {
    assert.ok(messages.includes(`'${r}'`), `refusal reason ${r} from native.go must appear in messages.js`);
  }
});

test('the counter set and the detail vocabulary match protocol/envelope.go', () => {
  const go = readFileSync(join(REPO_ROOT, 'endpoint', 'protocol', 'envelope.go'), 'utf8');
  const counters = [...go.matchAll(/Counter\w+\s+Counter\s*=\s*"([a-z_]+)"/g)].map((m) => m[1]);
  assert.deepEqual(counters.length, 7, 'the counter set is closed at seven');
  const messages = readFileSync(join(HERE, 'src', 'messages.js'), 'utf8');
  for (const c of counters) assert.ok(messages.includes(`'${c}'`), `counter ${c} must be in the closed set`);

  const details = [...go.matchAll(/Detail\w+\s+Detail\s*=\s*"([a-z_]+)"/g)].map((m) => m[1]);
  for (const d of details) assert.ok(messages.includes(`'${d}'`), `detail ${d} must be in the closed set`);
});

test('the collector name is the one database/schema.sql registers in ref.collector', () => {
  const sql = readFileSync(join(REPO_ROOT, 'database', 'schema.sql'), 'utf8');
  assert.ok(sql.includes("('capture_extension', 'capture_extension'"), 'ref.collector must carry this name');
  const messages = readFileSync(join(HERE, 'src', 'messages.js'), 'utf8');
  assert.match(messages, /COLLECTOR_NAME = 'capture_extension'/);
});

test('the observation body carries only fields native.go declares', () => {
  const go = readFileSync(join(REPO_ROOT, 'endpoint', 'protocol', 'native.go'), 'utf8');
  const block = go.slice(go.indexOf('type ObservationMessage struct'), go.indexOf('func (o ObservationMessage) Validate'));
  const fields = [...block.matchAll(/json:"([a-z_]+)/g)].map((m) => m[1]);
  assert.ok(fields.length > 8, 'the observation body must have its fields declared with json tags');
  const pipeline = readFileSync(join(HERE, 'src', 'pipeline.js'), 'utf8');
  const protocolBody = pipeline.slice(pipeline.indexOf('function protocolBody'), pipeline.indexOf('function drainQueue'));
  for (const f of fields) {
    assert.ok(protocolBody.includes(`.${f}`), `protocolBody must be able to carry ${f}`);
  }
  // And nothing local may leak into the wire body.
  assert.equal(/body\.[a-z_]*mode_resolved/.test(protocolBody), false, 'extension-local fields stay out of the frame');
  assert.equal(/body\.unread_reason/.test(protocolBody), false);
});

test('MaxNativeMessageBytes is budgeted against, not assumed', () => {
  const go = readFileSync(join(REPO_ROOT, 'endpoint', 'protocol', 'native.go'), 'utf8');
  assert.ok(go.includes('MaxNativeMessageBytes = 1 << 20'));
  const messages = readFileSync(join(HERE, 'src', 'messages.js'), 'utf8');
  assert.match(messages, /MAX_NATIVE_MESSAGE_BYTES = 1 << 20/);
});

function walk(dir) {
  const out = [];
  for (const entry of readdirSync(dir)) {
    if (entry === 'node_modules' || entry.startsWith('.')) continue;
    const p = join(dir, entry);
    if (statSync(p).isDirectory()) out.push(...walk(p));
    else out.push(p);
  }
  return out;
}
