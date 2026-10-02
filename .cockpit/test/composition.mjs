/**
 * Project Cockpit — does the shell's own client-module composition accept this
 * package?
 *
 * This is the one thing the other suites cannot answer. test/bundle.mjs proves
 * the *bundle* is well-formed; test/bundle.mjs and the README describe what the
 * host *should* look for. Neither runs the host's own scanner. So this file
 * imports `ClientModuleRegistry` from the shipped `@deepseek-ai/dsh-client-modules`
 * — the exact class the running harness instantiates — points it at a stub
 * loader carrying this profile's rows, and asks the composed graph whether the
 * cockpit is in it.
 *
 * It runs the real `resolveMeta`: the real `dsh.client` parser, the real
 * `exports["./client"]` lookup, and the real bundle-path join. If this passes,
 * a host restart is the only thing between the package and a served panel.
 *
 * Run: node .cockpit/test/composition.mjs
 */

import assert from 'node:assert/strict'
import { Context, Service } from '@deepseek-ai/cordis'
import { ClientModuleRegistry } from '@deepseek-ai/dsh-client-modules'
import { existsSync, readFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const here = path.dirname(fileURLToPath(import.meta.url))
const root = path.resolve(here, '..')

/**
 * The profile whose rows the composition should scan.
 *
 * The running harness names its own profile directory in the environment, so
 * this works wherever the package lives and whichever profile is booting. When
 * neither is available the check reports that it could not run rather than
 * pretending the package failed to compose.
 */
function findProfileDir() {
  if (typeof process.env.DSH_PROFILE_DIR === 'string' && process.env.DSH_PROFILE_DIR !== '') return process.env.DSH_PROFILE_DIR
  const home = process.env.DSH_HOME ?? path.join(process.env.USERPROFILE ?? '', '.dsh')
  for (const name of ['desktop', 'web']) {
    const candidate = path.join(home, 'profiles', name)
    if (existsSync(path.join(candidate, 'package.json'))) return candidate
  }
  return undefined
}

const PROFILE_DIR = findProfileDir()

let passed = 0
let failed = 0

/** Run one named check, awaiting it when async. */
async function test(label, fn) {
  try {
    await fn()
    passed += 1
    process.stdout.write(`  ok   ${label}\n`)
  } catch (error) {
    failed += 1
    process.stdout.write(`  FAIL ${label}\n         ${error instanceof Error ? error.message : String(error)}\n`)
  }
}

// ---------------------------------------------------------------------------
// a loader stub carrying the rows the profile composes
// ---------------------------------------------------------------------------

/**
 * The loader rows to feed the scanner, built with the shape a real entry has.
 *
 * The scanner is strict about this: it only considers an entry that carries a
 * `fiber`, is not disabled, and can report its tree's resolution base — so a
 * half-built stub silently composes nothing, which is exactly the trap this
 * check would otherwise fall into.
 *
 * @param baseUrl the tree's resolution base.
 * @returns rows, one of which is not a client package.
 */
function buildRows(baseUrl) {
  const tree = { ctx: { baseUrl } }
  return [
    { options: { id: 'project-cockpit', name: 'dsh-project-cockpit' }, fiber: { entry: { options: { name: 'dsh-project-cockpit' } } }, parent: { tree }, disabled: false },
    // A row with no client half, so the scanner is exercised on a negative too.
    { options: { id: 'tool-todo', name: '@deepseek-ai/dsh-tool-todo' }, fiber: { entry: { options: { name: '@deepseek-ai/dsh-tool-todo' } } }, parent: { tree }, disabled: false },
  ]
}

/**
 * Build a context the scanner can read.
 *
 * `ClientModuleRegistry` extends Cordis's `Service` and reads `ctx.loader.entries()`
 * plus each entry's tree base URL; it registers through `internal/plugin`, which
 * never fires here because nothing else is mounted.
 *
 * @returns the owning context.
 */
function makeContext() {
  const app = new Context()
  // The scanner's package resolution is anchored at the row's base URL, so the
  // base must be the profile directory — the same anchor the real boot uses.
  const baseUrl = `${PROFILE_DIR}${path.sep}`
  app.baseUrl = baseUrl
  const rows = buildRows(baseUrl)
  Object.defineProperty(app, 'loader', {
    value: {
      entries: () => rows,
      // Only used by the HMR path, which this check does not enter.
      builtins: {},
    },
    configurable: true,
  })
  // `logger.warn` is only reached when a row fails to compose, which is the
  // failure this check reports directly; keep it quiet but present.
  Object.defineProperty(app, 'logger', { value: { warn: () => {}, info: () => {}, error: () => {} }, configurable: true })
  return app
}

/** Find the composed row for a package, in any phase. */
function rowFor(registry, packageName) {
  const graph = registry.graph()
  const phases = [graph?.modules, graph?.application, graph?.rows, graph?.entries, graph?.boot]
  for (const phase of phases) {
    if (!Array.isArray(phase)) continue
    const found = phase.find((row) => row.id === packageName)
    if (found !== undefined) return found
  }
  // Fall back to the internal table: the graph's shape is the shell's business,
  // but membership is what this check is about.
  return registry.table?.get?.(packageName)?.entry ?? registry.table?.get?.(packageName)
}

// ---------------------------------------------------------------------------
// run
// ---------------------------------------------------------------------------

process.stdout.write('client-module composition\n')

if (PROFILE_DIR === undefined) {
  // Not a failure of the package: without a profile there is nothing to compose
  // against, and inventing one would test a fiction.
  process.stdout.write('  skip  no DSH profile found (set DSH_PROFILE_DIR to run this suite)\n\n0 passed, 0 failed\n')
} else {
  process.stdout.write(`  (scanning rows for the profile at ${PROFILE_DIR})\n`)

  let registry
  await test('the scanner accepts this package and composes a row for it', () => {
    const app = makeContext()
    registry = new ClientModuleRegistry(app)
    assert.ok(registry.table instanceof Map, 'the registry keeps a composition table')
    assert.ok(
      registry.table.has('dsh-project-cockpit'),
      `expected the cockpit in the table, found: ${[...registry.table.keys()].join(', ') || '(nothing)'}`,
    )
  })

  await test('the composed row resolves to the real bundle path', () => {
    const record = registry.table.get('dsh-project-cockpit')
    assert.ok(record !== undefined)
    const clientPath = record.entry?.clientPath ?? record.meta?.clientPath ?? record.clientPath
    assert.ok(typeof clientPath === 'string' && clientPath.endsWith('client.js'), `expected a client.js path, got ${String(clientPath)}`)
    // The install path runs through the profile's node_modules; what matters is
    // that it names the right package and that the artifact is really there.
    assert.ok(clientPath.includes(`${path.sep}dsh-project-cockpit${path.sep}lib${path.sep}client.js`), `unexpected bundle path ${clientPath}`)
    const source = readFileSync(clientPath, 'utf8')
    assert.ok(source.includes('__ModuleLoader__.load'), 'the composed artifact must be a module-loader bundle')
    // The bundle the scanner composed must be the one this workspace just built,
    // or a stale install would quietly pass.
    assert.equal(source, readFileSync(path.join(root, 'lib', 'client.js'), 'utf8'), 'the installed bundle must match the workspace build')
  })

  await test('the composed row is keyed by the package name the bundle registers', () => {
    const record = registry.table.get('dsh-project-cockpit')
    const clientPath = record.entry?.clientPath ?? record.meta?.clientPath ?? record.clientPath
    const source = readFileSync(clientPath, 'utf8')
    const id = /id:\s*"([^"]+)"/u.exec(source)?.[1]
    assert.equal(id, 'dsh-project-cockpit', 'the browser module id must be the package name the shell keys it by')
  })

  await test('the composed row carries the revision the bundle route needs', () => {
    const record = registry.table.get('dsh-project-cockpit')
    const rev = record.entry?.rev
    assert.ok(typeof rev === 'string' && /^[0-9a-f]{12}$/u.test(rev), `expected a 12-hex revision, got ${String(rev)}`)
  })

  await test('a package with no client half is not composed', () => {
    // The negative direction: the scanner must reject a host-only row rather than
    // inventing a bundle for it.
    const rejected = registry.table.has('@deepseek-ai/dsh-tool-todo')
    assert.equal(rejected, false, 'a row without dsh.client must not appear in the client graph')
  })

  await test('the scanner answers a boot graph without throwing', () => {
    const graph = registry.graph()
    assert.ok(graph !== undefined && graph !== null, 'the shell renders this as window.__DSH_BOOT__')
  })

  process.stdout.write(`\n${passed} passed, ${failed} failed\n`)
  if (failed > 0) process.exitCode = 1
}
