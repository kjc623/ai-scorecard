/**
 * Project Cockpit â€” host-side checks.
 *
 * These run against the real workspace, not fixtures, because the question the
 * cockpit answers ("what is actually on disk, and who will touch it?") is only
 * meaningful against real files. Run: node .cockpit/test/run.mjs
 */

import assert from 'node:assert/strict'
import { existsSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { validateManifest, normalizePath, pathsOverlap, findCycles } from '../lib/manifest.js'
import { extractHeadings, buildProjectIndex, scanWorkspace, resolveDeclaredPath, agentBrief } from '../lib/discover.js'
import { createProjectStore } from '../lib/store.js'

const here = path.dirname(fileURLToPath(import.meta.url))
const workspaceRoot = path.resolve(here, '..', '..')

let passed = 0
let failed = 0
const failures = []

/** Run one named check. */
function test(name, fn) {
  try {
    fn()
    passed += 1
    process.stdout.write(`  ok   ${name}\n`)
  } catch (error) {
    failed += 1
    failures.push({ name, error })
    process.stdout.write(`  FAIL ${name}\n         ${error instanceof Error ? error.message : String(error)}\n`)
  }
}

/** Run one async check. */
async function testAsync(name, fn) {
  try {
    await fn()
    passed += 1
    process.stdout.write(`  ok   ${name}\n`)
  } catch (error) {
    failed += 1
    failures.push({ name, error })
    process.stdout.write(`  FAIL ${name}\n         ${error instanceof Error ? error.message : String(error)}\n`)
  }
}

process.stdout.write('manifest validation\n')

test('accepts a minimal valid manifest', () => {
  const { problems, model } = validateManifest({
    schemaVersion: 1,
    project: { name: 'demo' },
    components: [{ id: 'core', name: 'Core' }],
  })
  assert.deepEqual(problems, [])
  assert.equal(model.components.length, 1)
  assert.equal(model.components[0].status, 'proposed')
})

test('rejects a non-object and a wrong schemaVersion', () => {
  assert.equal(validateManifest(null).problems.length, 1)
  const wrong = validateManifest({ schemaVersion: 2, project: { name: 'x' }, components: [{ id: 'a', name: 'A' }] })
  assert.ok(wrong.problems.some((entry) => entry.path === '/schemaVersion'))
})

test('reports duplicate component ids with their location', () => {
  const { problems } = validateManifest({
    schemaVersion: 1,
    project: { name: 'demo' },
    components: [
      { id: 'a', name: 'A' },
      { id: 'a', name: 'Again' },
    ],
  })
  assert.ok(problems.some((entry) => entry.path === '/components/1/id' && entry.message.includes('duplicate')))
})

test('rejects a malformed component id and keeps going', () => {
  const { problems, model } = validateManifest({
    schemaVersion: 1,
    project: { name: 'demo' },
    components: [
      { id: 'Not-Kebab', name: 'Bad' },
      { id: 'good', name: 'Good' },
    ],
  })
  assert.ok(problems.some((entry) => entry.path === '/components/0/id'))
  assert.equal(model.components.length, 2)
})

test('rejects a dangling dependency edge', () => {
  const { problems } = validateManifest({
    schemaVersion: 1,
    project: { name: 'demo' },
    components: [{ id: 'a', name: 'A', dependsOn: ['ghost'] }],
  })
  assert.ok(problems.some((entry) => entry.path === '/components/0/dependsOn/0'))
})

test('detects a dependency cycle instead of looping', () => {
  const components = [
    { id: 'a', name: 'A', dependsOn: ['b'] },
    { id: 'b', name: 'B', dependsOn: ['c'] },
    { id: 'c', name: 'C', dependsOn: ['a'] },
  ]
  const cycles = findCycles(components)
  assert.equal(cycles.length, 1)
  // The reported cycle closes the loop (first node repeated at the end).
  assert.deepEqual([...new Set(cycles[0])].sort(), ['a', 'b', 'c'])
  assert.equal(cycles[0][0], cycles[0][cycles[0].length - 1])
  const { problems } = validateManifest({ schemaVersion: 1, project: { name: 'demo' }, components })
  assert.ok(problems.some((entry) => entry.message.startsWith('dependency cycle')))
})

test('detects an agent write-scope collision as advisory, not fatal', () => {
  const { model, problems } = validateManifest({
    schemaVersion: 1,
    project: { name: 'demo' },
    components: [{ id: 'a', name: 'A' }],
    agents: [
      { id: 'one', role: 'builder', writeScopes: ['src/api'] },
      { id: 'two', role: 'builder', writeScopes: ['src/api/handlers.ts'] },
    ],
  })
  assert.deepEqual(problems, [])
  assert.equal(model.collisions.length, 1)
  assert.equal(model.collisions[0].left, 'src/api')
})

test('does not report a collision for disjoint scopes', () => {
  const { model } = validateManifest({
    schemaVersion: 1,
    project: { name: 'demo' },
    components: [{ id: 'a', name: 'A' }],
    agents: [
      { id: 'one', role: 'builder', writeScopes: ['src/api'] },
      { id: 'two', role: 'builder', writeScopes: ['src/ui'] },
    ],
  })
  assert.equal(model.collisions.length, 0)
})

test('rejects an unknown task reference and an unknown component reference', () => {
  const { problems } = validateManifest({
    schemaVersion: 1,
    project: { name: 'demo' },
    components: [{ id: 'a', name: 'A' }],
    tasks: [{ id: 't1', subject: 'T', dependsOn: ['nope'], componentIds: ['ghost'] }],
    agents: [{ id: 'one', role: 'builder', taskId: 'missing', componentIds: ['ghost'] }],
  })
  assert.ok(problems.some((entry) => entry.path === '/tasks/0/dependsOn/0'))
  assert.ok(problems.some((entry) => entry.path === '/tasks/0/componentIds/0'))
  assert.ok(problems.some((entry) => entry.path === '/agents/0/taskId'))
  assert.ok(problems.some((entry) => entry.path === '/agents/0/componentIds/0'))
})

process.stdout.write('\npath handling\n')

test('normalizePath canonicalizes separators and prefixes', () => {
  assert.equal(normalizePath('./docs\\adr\\'), 'docs/adr')
  assert.equal(normalizePath('a//b///c/'), 'a/b/c')
  assert.equal(normalizePath(undefined), '')
})

test('pathsOverlap treats a directory prefix as an overlap', () => {
  assert.ok(pathsOverlap('src/api', 'src/api/handlers.ts'))
  assert.ok(pathsOverlap('src/api/handlers.ts', 'src/api'))
  assert.ok(pathsOverlap('src', 'src'))
  assert.ok(!pathsOverlap('src/api', 'src/uix'))
  assert.ok(!pathsOverlap('', 'src'))
})

test('extractHeadings ignores fenced code and tracks nesting', () => {
  const headings = extractHeadings(['# Title', '', '```sh', '# not a heading', '```', '', '## Section', '### Deep'].join('\n'))
  assert.deepEqual(
    headings.map((entry) => [entry.level, entry.title]),
    [
      [1, 'Title'],
      [2, 'Section'],
      [3, 'Deep'],
    ],
  )
})

test('resolveDeclaredPath separates exact files, directories and absences', () => {
  const files = new Set(['docs/a.md', 'docs/b.md', 'README.md'])
  const dirs = new Set(['docs'])
  assert.equal(resolveDeclaredPath('README.md', files, dirs).status, 'present')
  assert.equal(resolveDeclaredPath('docs', files, dirs).status, 'directory')
  assert.equal(resolveDeclaredPath('docs', files, dirs).matches, 2)
  assert.equal(resolveDeclaredPath('docs/a.md', files, dirs).exact, true)
  assert.equal(resolveDeclaredPath('gone/x.md', files, dirs).status, 'missing')
})

process.stdout.write('\nindex derivation\n')

test('joins files to components and agents', () => {
  const manifest = {
    schemaVersion: 1,
    project: { name: 'demo', summary: '', stage: 'design', docsRoot: '', testCommand: '', runCommand: '' },
    components: [{ id: 'api', name: 'API', dependsOn: [], paths: ['src/api'], docs: [], decisions: [], status: 'in-progress', layer: 'backend', kind: 'service', language: '', runtime: '', summary: '', notes: '' }],
    tasks: [],
    agents: [{ id: 'builder', role: 'builder', emoji: '', purpose: '', taskId: '', task: '', componentIds: ['api'], readPaths: ['README.md'], writeScopes: ['src/api'], model: '', notes: '' }],
    invariants: [],
    collisions: [],
  }
  const scan = {
    files: [
      { path: 'src/api/index.ts', size: 10, mtimeMs: 0 },
      { path: 'README.md', size: 10, mtimeMs: 0 },
      { path: 'stray.txt', size: 1, mtimeMs: 0 },
    ],
    directories: ['src', 'src/api'],
    truncated: false,
  }
  const index = buildProjectIndex({ manifest, scan, documents: {}, root: '/tmp/demo' })
  const api = index.files.find((file) => file.path === 'src/api/index.ts')
  assert.deepEqual(api.components, ['api'])
  assert.deepEqual(api.agents, ['builder'])
  const readme = index.files.find((file) => file.path === 'README.md')
  assert.deepEqual(readme.agents, ['builder'])
  assert.deepEqual(index.unownedFiles, ['stray.txt'])
  assert.equal(index.stats.components, 1)
  assert.equal(index.agents[0].missingProvided, 0)
})

test('marks a declared path that is absent as missing, not silent', () => {
  const manifest = {
    schemaVersion: 1,
    project: { name: 'demo', summary: '', stage: 'design', docsRoot: '', testCommand: '', runCommand: '' },
    components: [{ id: 'ghost', name: 'Ghost', dependsOn: [], paths: ['never/created.ts'], docs: [], decisions: [], status: 'proposed', layer: 'x', kind: 'service', language: '', runtime: '', summary: '', notes: '' }],
    tasks: [],
    agents: [],
    invariants: [],
    collisions: [],
  }
  const index = buildProjectIndex({ manifest, scan: { files: [], directories: [], truncated: false }, documents: {}, root: '/tmp/demo' })
  assert.equal(index.components[0].paths[0].status, 'missing')
  assert.equal(index.components[0].missingPaths, 1)
  assert.equal(index.stats.missingPaths, 1)
})

test('agentBrief splits read, write and absent files', () => {
  const brief = agentBrief({
    readPaths: [{ path: 'a.md', status: 'present' }, { path: 'gone.md', status: 'missing' }],
    writeScopes: [{ path: 'src', status: 'directory' }],
    providedFiles: [
      { path: 'a.md', status: 'present' },
      { path: 'gone.md', status: 'missing' },
      { path: 'src', status: 'directory' },
    ],
  })
  assert.deepEqual(brief.read, ['a.md'])
  assert.deepEqual(brief.write, ['src'])
  assert.deepEqual(brief.missing, ['gone.md'])
})

process.stdout.write('\nreal workspace\n')

// The checks below are about a real tree, and the tree they describe is the one
// this package was built inside. A copy that has been moved to its own folder
// has no such neighbour, so they report that instead of failing â€” the plugin is
// meant to travel, and a moved copy is a supported place to be.
const HAS_FIXTURE = existsSync(path.join(workspaceRoot, '.cockpit', 'project.json'))
let skipped = 0

/** Run a check only where the fixture project is present. */
async function testWithFixture(label, fn) {
  if (!HAS_FIXTURE) {
    skipped += 1
    process.stdout.write(`  skip ${label} â€” no project beside this package\n`)
    return
  }
  await testAsync(label, fn)
}

await testWithFixture('scanWorkspace finds this package and prunes ignored directories', async () => {
  const scan = await scanWorkspace(workspaceRoot)
  const paths = scan.files.map((file) => file.path)
  assert.ok(paths.includes('README.md'), 'expected README.md in the scan')
  assert.ok(
    paths.some((file) => file.startsWith('.cockpit/lib/')),
    'expected the cockpit sources in the scan',
  )
  assert.ok(!paths.some((file) => file.includes('node_modules/')), 'node_modules must be pruned')
})

await testWithFixture('the project store builds an index for this workspace', async () => {
  const store = createProjectStore({ root: workspaceRoot })
  const index = await store.get()
  assert.equal(typeof index.generatedAt, 'string')
  assert.ok(index.stats.files > 10, `expected a non-trivial file count, got ${index.stats.files}`)
  assert.ok(Array.isArray(index.components))
})

// The cache is about the store, not the project, so it runs anywhere: point it
// at the package's own directory and it must behave the same.
await testAsync('the warm cache returns the same object and refresh rebuilds', async () => {
  const store = createProjectStore({ root: path.resolve(here, '..'), ttlMs: 60000 })
  const first = await store.get()
  const second = await store.get()
  assert.equal(first, second, 'a warm cache must not rebuild')
  const third = await store.refresh()
  assert.notEqual(first, third, 'refresh must rebuild')
})

process.stdout.write(`\n${passed} passed, ${failed} failed, ${skipped} skipped\n`)
if (failed > 0) {
  for (const failure of failures) {
    process.stdout.write(`\n${failure.name}\n${failure.error instanceof Error ? failure.error.stack : String(failure.error)}\n`)
  }
  process.exitCode = 1
}
