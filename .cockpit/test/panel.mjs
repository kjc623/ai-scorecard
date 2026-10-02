/**
 * Project Cockpit Ã¢â‚¬â€ panel render checks.
 *
 * Loads the built client bundle the way the shell does and renders the
 * registered panel through a small React-shaped runtime, so the assertions are
 * about what a reader would see rather than about our own helpers.
 *
 * The runtime is deliberately naive in one place: `useEffect` runs
 * synchronously when scheduled, and the renderer loops until state settles.
 * That is not React's timing, but it makes every data-dependent state
 * reachable, which is what these checks are for.
 *
 * Run: node .cockpit/test/panel.mjs
 */

import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { validateManifest } from '../lib/manifest.js'
import { buildProjectIndex } from '../lib/discover.js'

const here = path.dirname(fileURLToPath(import.meta.url))
const root = path.resolve(here, '..')

// ---------------------------------------------------------------------------
// a small React-shaped runtime
// ---------------------------------------------------------------------------

let cursor = null
let hookStore = new WeakMap()
/** State changed during the current pass, so one more render is needed. */
let dirtied = false

/**
 * Forget every component's hook state.
 *
 * Each check renders the panel from scratch; without this the panel would keep
 * the previous check's fetched index and every later assertion would describe
 * the first fixture.
 */
function resetHooks() {
  hookStore = new WeakMap()
}

const React = {
  createElement: (type, props, ...children) => ({
    $$typeof: 'element',
    type,
    props: { ...(props ?? {}), ...(children.length === 0 ? {} : { children: children.length === 1 ? children[0] : children }) },
    key: props?.key ?? null,
  }),
  Fragment: 'Fragment',
  useCallback: (fn, deps) => {
    const index = next('useCallback')
    const previous = cursor.hooks[index]
    if (previous !== undefined && sameDeps(previous.deps, deps)) return previous.value
    cursor.hooks[index] = { deps, value: fn }
    return fn
  },
  useState: (initial) => {
    const index = next('useState')
    if (cursor.hooks[index] === undefined) cursor.hooks[index] = { value: typeof initial === 'function' ? initial() : initial }
    const slot = cursor.hooks[index]
    return [
      slot.value,
      (value) => {
        const resolved = typeof value === 'function' ? value(slot.value) : value
        if (!Object.is(resolved, slot.value)) {
          slot.value = resolved
          dirtied = true
        }
      },
    ]
  },
  useEffect: (effect, deps) => {
    const index = next('useEffect')
    const previous = cursor.hooks[index]
    if (previous !== undefined && sameDeps(previous.deps, deps)) return
    if (previous !== undefined && typeof previous.cleanup === 'function') previous.cleanup()
    const slot = { deps, cleanup: undefined }
    cursor.hooks[index] = slot
    // Run immediately: the fetch resolves to a promise the renderer awaits.
    const cleanup = effect()
    slot.cleanup = typeof cleanup === 'function' ? cleanup : undefined
  },
  useRef: (initial) => {
    const index = next('useRef')
    if (cursor.hooks[index] === undefined) cursor.hooks[index] = { value: { current: initial } }
    return cursor.hooks[index].value
  },
  useSyncExternalStore: (subscribe, getSnapshot) => {
    next('useSyncExternalStore')
    return getSnapshot()
  },
}

/** Claim a hook slot, catching the classic conditional-hook bug. */
function next(name) {
  if (cursor === null) throw new Error(`${name} was called outside a render`)
  const index = cursor.index
  if (index >= cursor.maxHooks && cursor.maxHooks !== 0) {
    throw new Error(`${name} was called more times than the previous render (${cursor.maxHooks})`)
  }
  cursor.index = index + 1
  if (cursor.index > cursor.declared) cursor.declared = cursor.index
  return index
}

function sameDeps(left, right) {
  if (left === undefined || right === undefined) return false
  if (left.length !== right.length) return false
  return left.every((value, index) => Object.is(value, right[index]))
}

/** Render one component to text through the runtime. */
function renderTree(element) {
  // Only the top-level element of a pass is remembered, so a later click can
  // search the whole rendered tree rather than whichever leaf was walked last.
  if (captureRoot) {
    lastTree = element
    captureRoot = false
  }
  if (element === null || element === undefined || element === false || element === true) return ''
  if (typeof element === 'string' || typeof element === 'number') return String(element)
  if (Array.isArray(element)) return element.map(renderTree).join('')
  if (typeof element === 'function') return renderTree(call(element, element.props ?? {}))
  if (element.$$typeof === 'element' && typeof element.type === 'function') return renderTree(call(element.type, element.props))
  return renderTree(element.props?.children)
}

/** Render the panel once, remembering the tree this pass produced. */
function renderPanel(component, props) {
  captureRoot = true
  return renderTree(React.createElement(component, props))
}

/** Call a component with a fresh hook cursor over its persistent slots. */
function call(component, props) {
  const previous = cursor
  const state = hookStore.get(component) ?? { hooks: [], maxHooks: 0 }
  hookStore.set(component, state)
  cursor = { hooks: state.hooks, index: 0, declared: 0, maxHooks: state.maxHooks }
  try {
    const element = component(props)
    state.maxHooks = Math.max(state.maxHooks, cursor.declared)
    return element
  } finally {
    cursor = previous
  }
}

/**
 * Render until state stops changing, awaiting any async effects.
 * @param component the component to render.
 * @param props its props.
 * @param options `{ click }` Ã¢â‚¬â€ text of a button to click once the tree has
 *   settled, which is how a tab is reached.
 * @returns the settled text.
 */
async function settle(component, props, options = {}) {
  resetHooks()
  let text = ''
  for (let pass = 0; pass < 6; pass += 1) {
    dirtied = false
    text = renderPanel(component, props)
    // Effects that returned promises keep a microtask queue busy; drain it.
    for (let turn = 0; turn < 12; turn += 1) await Promise.resolve()
    if (!dirtied) break
  }
  if (options.click === undefined) return text
  // Click, then settle again Ã¢â‚¬â€ one extra round is enough for a tab switch,
  // which only changes local state.
  clickButton(options.click)
  for (let pass = 0; pass < 6; pass += 1) {
    dirtied = false
    text = renderPanel(component, props)
    for (let turn = 0; turn < 12; turn += 1) await Promise.resolve()
    if (!dirtied) break
  }
  return text
}

/** Every element the last render produced, for finding a button to click. */
let lastTree = null
/** Whether the next renderTree call is the root of a pass. */
let captureRoot = false

/** Invoke the onClick of the first button whose text contains `label`. */
function clickButton(label) {
  const found = findButton(lastTree, label)
  if (found === null) throw new Error(`no button matching ${JSON.stringify(label)} was rendered; buttons seen: ${listButtons(lastTree).join(' | ')}`)
  found.props.onClick()
}

/**
 * Depth-first search for a button element whose text contains `label`.
 *
 * Purely structural: it must not *render* anything while searching, because a
 * render would run hooks and reset the tree this search is walking. Function
 * components are entered directly, and their returned elements searched in turn.
 *
 * @param element the tree.
 * @param label text to look for.
 * @returns the element, or null.
 */
function findButton(element, label, depth = 0) {
  if (element === null || typeof element !== 'object' || depth > 80) return null
  if (Array.isArray(element)) {
    for (const child of element) {
      const found = findButton(child, label, depth + 1)
      if (found !== null) return found
    }
    return null
  }
  if (element.$$typeof !== 'element') return null
  if (element.type === 'button') {
    return textOf(element).includes(label) ? element : null
  }
  if (typeof element.type === 'function') {
    return findButton(call(element.type, element.props), label, depth + 1)
  }
  const children = element.props?.children
  if (Array.isArray(children)) {
    for (const child of children) {
      const found = findButton(child, label, depth + 1)
      if (found !== null) return found
    }
    return null
  }
  return findButton(children, label, depth + 1)
}

/** The text inside one element, without rendering any component it contains. */
function textOf(element, depth = 0) {
  if (element === null || element === undefined || element === false || depth > 40) return ''
  if (typeof element === 'string' || typeof element === 'number') return String(element)
  if (Array.isArray(element)) return element.map((child) => textOf(child, depth + 1)).join('')
  if (typeof element === 'function') return ''
  return textOf(element.props?.children, depth + 1)
}

/** Every button label in a tree, for diagnostics. */
function listButtons(element, out = [], depth = 0) {
  if (element === null || typeof element !== 'object' || depth > 80) return out
  if (Array.isArray(element)) {
    for (const child of element) listButtons(child, out, depth + 1)
    return out
  }
  if (element.$$typeof !== 'element') return out
  if (element.type === 'button') {
    out.push(JSON.stringify(textOf(element).slice(0, 32)))
    return out
  }
  if (typeof element.type === 'function') return listButtons(call(element.type, element.props), out, depth + 1)
  return listButtons(element.props?.children, out, depth + 1)
}

// ---------------------------------------------------------------------------
// the shell environment
// ---------------------------------------------------------------------------

const styleTags = []
const documentStub = {
  querySelector: (selector) => styleTags.find((tag) => `style[data-plugin-css="${tag.dataset.pluginCss}"]` === selector) ?? null,
  createElement: () => ({ dataset: {}, textContent: '' }),
  head: { appendChild: (element) => (styleTags.push(element), element) },
}

const registrations = []
const injections = []
let sessionsSnapshot = { ids: [], byId: {}, projectionsBySession: {} }
const sessionsStub = { list: { getSnapshot: () => sessionsSnapshot, subscribe: () => () => {} } }
const ctx = {
  sessions: sessionsStub,
  slots: {
    inject: (slot, register) => {
      injections.push(slot)
      register()
    },
    register: (options, component) => {
      registrations.push({ options, component })
      return () => {}
    },
  },
}

const loaded = { id: undefined, exports: undefined }
const facade = {
  load: ({ id, factory }) => {
    loaded.id = id
    loaded.exports = factory((specifier) => {
      if (specifier === 'react') return React
      if (specifier === 'react-dom' || specifier === 'react/jsx-runtime') return {}
      throw new Error(`module ${specifier} is not in the platform table`)
    })
  },
}

let fetchHandler = async () => {
  throw new Error('fetch was not expected during this check')
}
const sandbox = {
  window: { __ModuleLoader__: facade, location: { href: 'http://127.0.0.1:19387/' } },
  document: documentStub,
  fetch: (url, init) => fetchHandler(url, init),
  console,
  setTimeout,
  clearTimeout,
  AbortController,
  URL,
}

// ---------------------------------------------------------------------------
// fixture
// ---------------------------------------------------------------------------

const manifest = validateManifest({
  schemaVersion: 1,
  project: { name: 'Shadow Bee', summary: 'Records what employees send to generative AI tools.', stage: 'design', testCommand: 'psql -f db/schema.sql' },
  components: [
    { id: 'capture-core', name: 'Capture Core', kind: 'service', layer: 'device', language: 'Go', status: 'in-progress', paths: ['db/schema.sql'], docs: ['docs/00-architecture.md'] },
    { id: 'ingest-api', name: 'Ingest API', kind: 'service', layer: 'backend', language: 'Go', status: 'proposed', paths: ['contracts/event-envelope.schema.json'], dependsOn: ['capture-core'] },
    { id: 'phantom', name: 'Not Built Yet', kind: 'service', layer: 'backend', status: 'blocked', paths: ['src/phantom/index.ts'], dependsOn: ['ingest-api'] },
  ],
  tasks: [
    { id: 't1', subject: 'Land the envelope contract', status: 'in_progress', componentIds: ['ingest-api'], writeScopes: ['contracts'], owner: 'contractor' },
    { id: 't2', subject: 'Wire the write path', status: 'pending', dependsOn: ['t1'], componentIds: ['ingest-api'], writeScopes: ['db'] },
  ],
  agents: [
    { id: 'contractor', role: 'contract owner', emoji: 'C', purpose: 'Owns the wire contract.', taskId: 't1', componentIds: ['ingest-api'], readPaths: ['README.md'], writeScopes: ['contracts'] },
    { id: 'dbuilder', role: 'data engineer', purpose: 'Owns the schema.', taskId: 't2', componentIds: ['ingest-api', 'capture-core'], readPaths: ['db/schema.sql'], writeScopes: ['contracts'] },
  ],
  invariants: [{ id: 'INV-1', statement: 'One write path.', componentIds: ['ingest-api'], record: 'docs/adr/0001.md' }],
}).model

const scan = {
  files: [
    { path: 'README.md', size: 4200, mtimeMs: 0 },
    { path: 'db/schema.sql', size: 22000, mtimeMs: 0 },
    { path: 'contracts/event-envelope.schema.json', size: 9000, mtimeMs: 0 },
    { path: 'docs/00-architecture.md', size: 30000, mtimeMs: 0 },
    { path: 'stray.txt', size: 10, mtimeMs: 0 },
  ],
  directories: ['db', 'contracts', 'docs'],
  truncated: false,
}
const index = buildProjectIndex({ manifest, scan, documents: {}, root: '/repo' })

/**
 * Serve one index (or a failure) from the host route, and answer the panel's
 * POST reads the way the host does.
 *
 * @param value the index the GET returns.
 * @param options `{ fail }` to make the read fail, `{ board }` to answer
 *   `read_team` with a board, `{ actions }` to record posted actions.
 */
function serve(value, options = {}) {
  fetchHandler = async (url, init) => {
    const parsed = new URL(url)
    assert.ok(parsed.pathname.endsWith('/api/project-cockpit'), `unexpected request ${parsed.pathname}`)
    if (init?.method === 'POST') {
      const body = JSON.parse(init.body)
      if (options.actions !== undefined) options.actions.push(body)
      if (body.action === 'read_team') {
        if (options.boardError !== undefined) return { ok: false, status: 409, json: async () => ({ error: options.boardError }) }
        return { ok: true, status: 200, json: async () => ({ sessionId: body.sessionId, members: options.board?.members ?? [], tasks: options.board?.tasks ?? [] }) }
      }
      if (options.actionError !== undefined) return { ok: false, status: 409, json: async () => ({ error: options.actionError }) }
      return { ok: true, status: 200, json: async () => ({ task: { id: 'task-9', revision: 1 } }) }
    }
    if (options.fail === true) return { ok: false, status: 503, json: async () => ({}) }
    // The host advertises what it can do; a panel that does not see the action
    // contract must offer no actions, which the tests below also cover.
    return { ok: true, status: 200, json: async () => ({ root: '/repo', capabilities: options.capabilities ?? { contract: 2, actions: ['create_task', 'update_task', 'send_message', 'read_team'] }, index: value }) }
  }
}

/** A session whose team exists, so the action paths are reachable. */
function liveSession() {
  sessionsSnapshot = {
    ids: ['sess-1'],
    byId: { 'sess-1': { sessionId: 'sess-1', title: 'Cockpit', cwd: '/repo', running: false, retainedBy: { mainView: 1 } } },
    projectionsBySession: { 'sess-1': { values: { agentTeam: { members: [{ id: 'sess-1', name: 'lead', role: 'lead', phase: 'active' }], tasks: [] } } } },
  }
}

// ---------------------------------------------------------------------------
// run
// ---------------------------------------------------------------------------

let passed = 0
let failed = 0

async function test(name, fn) {
  try {
    await fn()
    passed += 1
    process.stdout.write(`  ok   ${name}\n`)
  } catch (error) {
    failed += 1
    process.stdout.write(`  FAIL ${name}\n         ${error instanceof Error ? error.message : String(error)}\n`)
  }
}

const source = readFileSync(path.join(root, 'lib', 'client.js'), 'utf8')
// eslint-disable-next-line no-new-func
new Function('window', 'document', 'fetch', 'console', 'setTimeout', 'clearTimeout', 'AbortController', 'URL', source)(
  sandbox.window,
  sandbox.document,
  sandbox.fetch,
  sandbox.console,
  sandbox.setTimeout,
  sandbox.clearTimeout,
  sandbox.AbortController,
  sandbox.URL,
)

loaded.exports.apply(ctx)
const panel = registrations.find((entry) => entry.options.name === 'main')

process.stdout.write('panel render\n')

await test('shows the project, the stage and the read-only footer', async () => {
  serve(index)
  const text = await settle(panel.component, { sessions: sessionsStub })
  assert.ok(text.includes('Shadow Bee'), 'the project name must appear')
  assert.ok(text.includes('design'), 'the stage chip must appear')
  assert.ok(text.includes('read-only'), 'the footer must say the panel does not write')
  assert.ok(text.includes('Architecture') && text.includes('Agents') && text.includes('Tasks'), 'the tab strip must appear')
})

await test('draws the architecture map with layers, edges and absent paths', async () => {
  serve(index)
  const text = await settle(panel.component, { sessions: sessionsStub })
  assert.ok(text.includes('Capture Core') && text.includes('Ingest API'), 'components must be listed')
  assert.ok(text.includes('device') && text.includes('backend'), 'layers must head their groups')
  assert.ok(text.includes('capture-core'), 'the dependency edge must be drawn as text')
  assert.ok(text.includes('1 declared path(s) absent'), 'an absent declared path must be visible')
})

await test('summarises the plan in KPIs', async () => {
  serve(index)
  const text = await settle(panel.component, { sessions: sessionsStub })
  assert.ok(text.includes('components'))
  assert.ok(text.includes('files on disk'))
  assert.ok(text.includes('declared paths absent'))
})

await test('says plainly when no live team is open', async () => {
  serve(index)
  sessionsSnapshot = { ids: [], byId: {}, projectionsBySession: {} }
  const text = await settle(panel.component, { sessions: sessionsStub })
  assert.ok(text.includes('no live team in this session'), 'a missing team must be stated, not implied')
})

await test('counts teammates, not the lead, when a team is live', async () => {
  serve(index)
  sessionsSnapshot = {
    ids: ['sess-1'],
    byId: { 'sess-1': { sessionId: 'sess-1', title: 'Cockpit', cwd: '/repo', running: true, retainedBy: { mainView: 1 } } },
    projectionsBySession: {
      'sess-1': {
        values: {
          agentTeam: {
            members: [
              { id: 'sess-1', name: 'lead', role: 'lead', phase: 'active' },
              { id: 'child-1', name: 'contractor', role: 'teammate', phase: 'active' },
              { id: 'child-2', name: 'ghost', role: 'teammate', phase: 'failed', error: 'provider refused' },
            ],
            tasks: [],
          },
        },
      },
    },
  }
  const text = await settle(panel.component, { sessions: sessionsStub })
  assert.ok(text.includes('2 teammate(s) live'), `expected two teammates, got: ${text.slice(0, 240)}`)
})

await test('offers a retry and hides stale data when the host route fails', async () => {
  serve(index, { fail: true })
  const text = await settle(panel.component, { sessions: sessionsStub })
  assert.ok(text.includes('Project Cockpit'))
  assert.ok(text.includes('Retry'), 'a failed load must offer a retry')
  assert.ok(!text.includes('Shadow Bee'), 'a failed load must not render stale project data')
})

await test('renders a project with no manifest and no components', async () => {
  const bare = buildProjectIndex({
    manifest: validateManifest(undefined).model,
    scan: { files: [{ path: 'a.txt', size: 1, mtimeMs: 0 }], directories: [], truncated: false },
    documents: {},
    root: '/repo',
  })
  serve(bare)
  const text = await settle(panel.component, { sessions: sessionsStub })
  assert.ok(text.includes('Untitled project'))
  assert.ok(text.includes('No components are declared yet'))
})

await test('counts every declared path that is not on disk', async () => {
  // Three component paths, one component doc, and four agent paths (two read,
  // two write) Ã¢â‚¬â€ every declaration is counted, not just component paths.
  const empty = buildProjectIndex({
    manifest,
    scan: { files: [], directories: [], truncated: false },
    documents: {},
    root: '/repo',
  })
  assert.equal(empty.stats.missingPaths, 8)
  serve(empty)
  const text = await settle(panel.component, { sessions: sessionsStub })
  assert.ok(text.includes('Shadow Bee'))
  assert.ok(text.includes('8 declared path(s) absent'), `expected the absent chip, got: ${text.slice(0, 300)}`)
})

process.stdout.write('\nplanning actions in the panel\n')

await test('shows a created task as already on the board, with its live revision', async () => {
  liveSession()
  serve(index, {
    board: {
      members: [{ id: 'sess-1', name: 'lead', role: 'lead' }],
      tasks: [{ id: 'task-3', revision: 7, subject: 'Land the envelope contract', description: '', status: 'in_progress', blockedBy: [], writeScopes: [], ready: false, writeScopeWarnings: [] }],
    },
  })
  const text = await settle(panel.component, { sessions: sessionsStub }, { click: 'Tasks' })
  assert.ok(text.includes('On the board as task-3'), `expected the board id, got: ${text.slice(0, 600)}`)
  assert.ok(text.includes('revision 7'), 'the live revision is what a later change must quote, so it must be shown')
  assert.ok(text.includes('Board reachable'), 'the panel must say whether the shared board answered')
})

await test('offers to create a planned task that is not on the board', async () => {
  liveSession()
  const actions = []
  serve(index, { board: { members: [], tasks: [] }, actions })
  const text = await settle(panel.component, { sessions: sessionsStub }, { click: 'Tasks' })
  assert.ok(text.includes('Add to team board'), 'an unmatched plan task must be offerable')
  assert.equal(actions.filter((entry) => entry.action === 'read_team').length, 1, 'the board must be read once per settle, not per render')
  assert.ok(actions.every((entry) => entry.sessionId === 'sess-1'), 'every action must name the session it belongs to')
})

await test('says there is no board rather than offering a dead button', async () => {
  sessionsSnapshot = { ids: [], byId: {}, projectionsBySession: {} }
  serve(index, { board: { members: [], tasks: [] } })
  const text = await settle(panel.component, { sessions: sessionsStub }, { click: 'Tasks' })
  assert.ok(text.includes('No open session, so no board to plan onto'))
  assert.ok(!text.includes('Add to team board'), 'with no session there is nothing to add a task to')
})

await test('reports a board that refuses to answer, and keeps rendering', async () => {
  liveSession()
  serve(index, { boardError: 'The Agent Teams service is not available in this composition.' })
  const text = await settle(panel.component, { sessions: sessionsStub }, { click: 'Tasks' })
  assert.ok(text.includes('The shared board could not be read'))
  assert.ok(text.includes('Shadow Bee'), 'a board failure must not take the project view down with it')
})

await test('offers no action when the running host half is older', async () => {
  // The client bundle can be newer than the host half — the panel is served
  // from the file, the host needs a restart. An older host has no POST route,
  // so the panel must say so rather than render a button that would fail.
  liveSession()
  serve(index, { board: { members: [], tasks: [] }, capabilities: { contract: 1 } })
  const text = await settle(panel.component, { sessions: sessionsStub }, { click: 'Tasks' })
  assert.ok(text.includes('needs the host half of the cockpit to be restarted'), `expected the staleness notice, got: ${text.slice(0, 600)}`)
  assert.ok(!text.includes('Add to team board'), 'an older host must not be offered an action it cannot perform')
})

process.stdout.write(`\n${passed} passed, ${failed} failed\n`)
if (failed > 0) process.exitCode = 1
