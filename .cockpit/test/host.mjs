/**
 * Project Cockpit â€” host half checks.
 *
 * Loads the real plugin entry and calls its real `apply()` against a stub
 * context, so what is verified is the actual registrations: the `/api` route
 * the browser panel calls and the model-facing tool the agent calls. A host
 * plugin that throws on mount takes the whole harness boot with it, which is
 * exactly the failure this file exists to catch before installation.
 *
 * Run: node .cockpit/test/host.mjs
 */

import assert from 'node:assert/strict'
import { existsSync, readFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { apply, name, inject, Config } from '../lib/index.js'

const here = path.dirname(fileURLToPath(import.meta.url))
const workspaceRoot = path.resolve(here, '..', '..')

/**
 * Whether this checkout still sits inside the project it was built for.
 *
 * The suite is meant to travel: the plugin can be moved to its own folder and
 * used on any project. Several checks below read the Shadow AI Capture package
 * that happens to live beside it, and those are only meaningful where it is
 * present â€” so they are skipped, with a reason, rather than failing on a moved
 * copy. The derivation checks that do not read it run everywhere.
 */
const HAS_FIXTURE = existsSync(path.join(workspaceRoot, 'docs', '00-architecture.md')) && existsSync(path.join(workspaceRoot, '.cockpit', 'project.json'))

let skipped = 0

/**
 * Run a check only when the local fixture project is present.
 * @param label what the check proves.
 * @param fn the check.
 */
async function testWithFixture(label, fn) {
  if (!HAS_FIXTURE) {
    skipped += 1
    process.stdout.write(`  skip ${label} â€” no fixture project beside this package\n`)
    return
  }
  await test(label, fn)
}

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

/** Build a stub host context that records what the plugin registers. */
function makeContext(services = {}) {
  const routes = []
  const tools = []
  const effects = []
  const ctx = {
    tools: {
      register: (definition) => {
        tools.push(definition)
        return () => {}
      },
    },
    connection: {
      fetch: {
        register: (route) => {
          routes.push(route)
          return () => {}
        },
      },
    },
    effect: (callback, label) => {
      effects.push(label)
      return callback()
    },
    get: (serviceName) => services[serviceName],
  }
  return { ctx, routes, tools, effects }
}

/** A stand-in Agent Teams service that records the calls it receives. */
function makeAgentTeams() {
  const calls = []
  const lead = { id: 'sess-1' }
  return {
    calls,
    service: {
      listMembers: (agent) => {
        calls.push(['listMembers', agent.id])
        return [{ id: 'sess-1', name: 'lead', role: 'lead', status: 'running', diagnostics: [] }]
      },
      listTasks: (agent) => {
        calls.push(['listTasks', agent.id])
        return [{ id: 'task-1', revision: 3, subject: 'Existing', description: '', status: 'pending', blockedBy: [], writeScopes: [], ready: true, writeScopeWarnings: [] }]
      },
      createTask: async (agent, request) => {
        calls.push(['createTask', agent.id, request])
        return { id: 'task-2', revision: 1, subject: request.subject, description: request.description, status: 'pending', blockedBy: request.blockedBy ?? [], writeScopes: request.writeScopes ?? [], ready: true, writeScopeWarnings: [] }
      },
      updateTask: async (agent, request) => {
        calls.push(['updateTask', agent.id, request])
        if (request.expectedRevision !== 3) throw new Error('TEAM_TASK_STALE_REVISION')
        return { id: request.taskId, revision: 4, subject: 'Existing', description: '', status: 'in_progress', blockedBy: [], writeScopes: [], ready: false, writeScopeWarnings: [] }
      },
      sendMessage: async (agent, request) => {
        calls.push(['sendMessage', agent.id, request])
        return { messageId: 'msg-1', delivered: 'queued' }
      },
    },
    lead,
  }
}

/** An agent registry holding one live agent. */
function makeAgents(id = 'sess-1') {
  return { get: (sessionId) => (sessionId === id ? { id, session: { id } } : undefined) }
}

// ---------------------------------------------------------------------------

process.stdout.write('plugin shape\n')

await test('every module the host half imports exists', () => {
  // The failure this guards: a profile install is a *copy*, and a partial copy
  // leaves `lib/index.js` importing a file that is not there. DSH's boot is
  // all-or-nothing, so that does not degrade the plugin â€” it stops the whole
  // harness. Every relative import must resolve to a real file.
  const hostDir = path.resolve(here, '..', 'lib')
  const seen = new Set()
  const missing = []
  const walk = (file) => {
    if (seen.has(file)) return
    seen.add(file)
    let source
    try {
      source = readFileSync(file, 'utf8')
    } catch {
      missing.push(file)
      return
    }
    for (const match of source.matchAll(/^import\s[\s\S]*?from\s+'(\.[^']+)'/gmu)) {
      const resolved = path.resolve(path.dirname(file), match[1])
      if (!existsSync(resolved)) missing.push(`${path.relative(hostDir, file)} imports ${match[1]}`)
      else walk(resolved)
    }
  }
  walk(path.join(hostDir, 'index.js'))
  assert.deepEqual(missing, [], `unresolvable host imports (a partial install would fail to boot): ${missing.join('; ')}`)
  assert.ok(seen.size >= 5, `expected the host half to be more than one file, walked ${seen.size}`)
})

await test('exports the cordis plugin triple and no default export', () => {
  assert.equal(name, 'project-cockpit')
  assert.deepEqual(inject, ['tools', 'connection'])
  assert.equal(typeof apply, 'function')
})

await test('declares a Config schema with usable defaults', () => {
  assert.equal(typeof Config, 'function', 'schemastery schemas are callable with a raw config')
  const resolved = Config({})
  assert.equal(resolved.root, '')
  assert.equal(resolved.serveRoute, true)
  assert.equal(resolved.exposeTool, true)
  assert.ok(resolved.maxFiles >= 1)
})

process.stdout.write('\nmounting\n')

const mounted = makeContext()
apply(mounted.ctx, Config({}))

await test('registers the browser route at the path the panel fetches', () => {
  assert.equal(mounted.routes.length, 1)
  const route = mounted.routes[0]
  assert.equal(route.path, '/api/project-cockpit')
  // GET serves the index; POST carries the narrow set of planning actions.
  assert.deepEqual(route.methods, ['GET', 'POST'])
  assert.equal(typeof route.fetch, 'function')
})

await test('registers exactly one model-facing tool', () => {
  assert.equal(mounted.tools.length, 1)
  assert.equal(mounted.tools[0].name, 'project_cockpit')
})

await test('owns both registrations through effects, so disabling removes them', () => {
  assert.equal(mounted.effects.length, 2)
  assert.ok(mounted.effects.some((label) => label.includes('/api/project-cockpit')))
})

await test('refuses to serve or expose when configured off', () => {
  const off = makeContext()
  apply(off.ctx, Config({ serveRoute: false, exposeTool: false }))
  assert.equal(off.routes.length, 0)
  assert.equal(off.tools.length, 0)
})

process.stdout.write('\nthe route\n')

const route = mounted.routes[0]

/** Build a GET request for the cockpit route. */
function request(query = '') {
  return new Request(`http://127.0.0.1:19387/api/project-cockpit${query}`)
}

/** Point the plugin at this workspace. */
const pinned = makeContext()
apply(pinned.ctx, Config({ root: workspaceRoot, ttlMs: 0 }))
const pinnedRoute = pinned.routes[0]

await testWithFixture('serves the project index as JSON', async () => {
  const response = await pinnedRoute.fetch(request('?view=index'))
  assert.equal(response.status, 200)
  const payload = await response.json()
  assert.equal(payload.root, workspaceRoot)
  assert.ok(payload.index !== undefined, 'the index view must carry an index')
  assert.equal(payload.index.hasManifest, true, 'this workspace must have a manifest')
  assert.ok(payload.index.stats.files > 10)
})

await testWithFixture('the served index separates planned from present', async () => {
  const payload = await (await pinnedRoute.fetch(request('?view=index'))).json()
  const index = payload.index
  assert.ok(index.components.length >= 10, `expected the declared components, got ${index.components.length}`)
  assert.ok(index.agents.length >= 5, 'expected the declared agents')
  assert.ok(index.stats.declaredPaths > 20, 'expected the declared paths to be counted')
  const missing = index.components.flatMap((component) => [...component.paths, ...component.docs]).filter((entry) => entry.status === 'missing')
  assert.ok(missing.length >= 0, 'absent paths must be representable')
})

await testWithFixture('serves the manifest status, including its candidates', async () => {
  const payload = await (await pinnedRoute.fetch(request('?view=manifest'))).json()
  assert.equal(payload.relPath, '.cockpit/project.json')
  assert.deepEqual(payload.problems, [], 'the manifest must validate cleanly')
  assert.ok(Array.isArray(payload.candidates) && payload.candidates.length > 0)
})

await testWithFixture('serves one agent with the files it will be handed', async () => {
  const payload = await (await pinnedRoute.fetch(request('?view=agent&agent=dbuilder'))).json()
  assert.equal(payload.agent.id, 'dbuilder')
  assert.ok(payload.brief.read.length > 0, 'the agent must be handed its read paths')
  assert.ok(payload.brief.write.includes('db'), 'the agent write scope must be reported')
})

await test('answers 404 for an agent that is not declared', async () => {
  const response = await pinnedRoute.fetch(request('?view=agent&agent=nobody'))
  assert.equal(response.status, 404)
  const payload = await response.json()
  assert.ok(payload.error.includes('nobody'))
})

await testWithFixture('serves the component slice', async () => {
  const payload = await (await pinnedRoute.fetch(request('?view=components'))).json()
  assert.ok(payload.components.length > 0)
  assert.ok(payload.layers.length > 0)
  assert.equal(typeof payload.generatedAt, 'string')
})

await testWithFixture('honours refresh=1 by rebuilding from disk', async () => {
  const first = await (await pinnedRoute.fetch(request('?view=index'))).json()
  const second = await (await pinnedRoute.fetch(request('?view=index&refresh=1'))).json()
  assert.ok(second.index.generatedAt >= first.index.generatedAt)
})

await test('reports a failure as JSON rather than throwing at the route', async () => {
  const broken = makeContext()
  apply(broken.ctx, Config({ root: path.join(workspaceRoot, 'does-not-exist'), ttlMs: 0 }))
  const response = await broken.routes[0].fetch(request('?view=index'))
  // A missing root is not fatal: the walk yields nothing and the index says so.
  assert.equal(response.status, 200)
  const payload = await response.json()
  assert.equal(payload.index.stats.files, 0)
})

process.stdout.write('\nthe tool\n')

const tool = mounted.tools[0]

/** Call the tool the way the runtime does. */
async function callTool(args, exec = {}) {
  return tool.execute(args, { signal: new AbortController().signal, ...exec })
}

await test('declares the required output shape', () => {
  assert.ok(tool.output !== undefined, 'a registered tool must declare output')
  assert.equal(tool.output.schema.type, 'object')
  assert.ok(tool.output.render !== undefined)
  assert.equal(typeof tool.execute, 'function')
})

await test('describes itself and its views', () => {
  assert.ok(tool.description.toLowerCase().includes('read-only'), 'the tool must say it does not write')
  // defineTool compiles the parameter DSL into JSON Schema, so the enum is
  // under `properties`, not at the top level.
  assert.deepEqual(tool.parameters.properties.view.enum, ['overview', 'agents', 'files', 'tasks'])
  assert.equal(tool.parameters.properties.refresh.type, 'boolean')
  assert.equal(tool.parameters.type, 'object')
})

await testWithFixture('returns an overview of this workspace when the session is rooted here', async () => {
  const value = await callTool({ view: 'overview' }, { agent: { session: { header: { cwd: workspaceRoot } } } })
  assert.equal(value.root, workspaceRoot)
  assert.equal(value.view, 'overview')
  assert.equal(value.hasManifest, true)
  assert.ok(value.text.includes('Shadow AI Capture'), 'the project name must be in the text')
  assert.ok(value.text.includes('capture-core'), 'the components must be listed')
  assert.deepEqual(value.problems, [])
})

await testWithFixture('returns the agent brief, naming every file an agent is handed', async () => {
  const value = await callTool({ view: 'agents' }, { agent: { session: { header: { cwd: workspaceRoot } } } })
  assert.ok(value.text.includes('dbuilder'))
  assert.ok(value.text.includes('provided files (read)'), 'the brief must label the read set')
  assert.ok(value.text.includes('write scopes'), 'the brief must label the write set')
})

await test('says so plainly when the manifest declares no agents', async () => {
  const value = await callTool({ view: 'agents' })
  // No session cwd: the tool falls back to the host process cwd, which is this
  // workspace when the check is run from it.
  assert.ok(typeof value.text === 'string' && value.text.length > 0)
})

await test('falls back to the session workspaceRoot when cwd is absent', async () => {
  const value = await callTool({}, { agent: { session: { header: { workspaceRoot } } } })
  assert.equal(value.root, workspaceRoot)
})

await testWithFixture('reports every declared task with its blockers', async () => {
  const value = await callTool({ view: 'tasks' }, { agent: { session: { header: { cwd: workspaceRoot } } } })
  assert.ok(value.text.includes('T1'))
  assert.ok(value.text.includes('blocked by'), 'a blocked task must name its blockers')
})

await testWithFixture('reports which files are claimed and which are not', async () => {
  const value = await callTool({ view: 'files' }, { agent: { session: { header: { cwd: workspaceRoot } } } })
  assert.ok(value.text.includes('files are claimed by a component or an agent'))
  assert.ok(value.text.includes('contracts/event-envelope.schema.json'), 'a claimed file must be listed with its owner')
})

await test('surfaces manifest problems instead of failing on a bad manifest', async () => {
  const value = await callTool({ view: 'overview', refresh: true }, { agent: { session: { header: { cwd: path.join(workspaceRoot, '.cockpit') } } } })
  // `.cockpit` has no manifest at its own root, so the tool must say so rather than throw.
  assert.equal(value.hasManifest, false)
  assert.ok(value.text.includes('none found') || value.text.includes('No manifest found'))
})

process.stdout.write('\nplanning actions\n')

const team = makeAgentTeams()
const acting = makeContext({ agentTeams: team.service, agents: makeAgents('sess-1') })
apply(acting.ctx, Config({ root: workspaceRoot, ttlMs: 0 }))
const actRoute = acting.routes[0]

/** POST one action the way the panel does. */
async function post(action, payload, sessionId = 'sess-1') {
  const response = await actRoute.fetch(
    new Request('http://127.0.0.1:19387/api/project-cockpit', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ action, sessionId, payload }),
    }),
  )
  return { status: response.status, body: await response.json() }
}

await test('declares both methods on the one route', () => {
  assert.deepEqual(actRoute.methods, ['GET', 'POST'])
})

await test('reads the live team for a named session', async () => {
  const { status, body } = await post('read_team')
  assert.equal(status, 200)
  assert.equal(body.members.length, 1)
  assert.equal(body.tasks[0].id, 'task-1')
  assert.deepEqual(team.calls.at(-1), ['listTasks', 'sess-1'])
})

await test('creates a task with its blockers and write scopes', async () => {
  const { status, body } = await post('create_task', { subject: 'Do the thing', description: 'Properly.', blockedBy: ['T1'], writeScopes: ['services/x', 'services/x'] })
  assert.equal(status, 200)
  assert.equal(body.task.subject, 'Do the thing')
  const call = team.calls.at(-1)
  assert.equal(call[0], 'createTask')
  assert.equal(call[1], 'sess-1', 'the action must be attributed to the named session')
  assert.deepEqual(call[2].blockedBy, ['T1'])
  assert.deepEqual(call[2].writeScopes, ['services/x'], 'write scopes must be de-duplicated')
})

await test('refuses a task with no subject rather than creating a blank one', async () => {
  const { status, body } = await post('create_task', { subject: '   ', description: 'x' })
  assert.equal(status, 400)
  assert.ok(body.error.includes('subject'))
})

await test('applies a compare-and-set transition', async () => {
  const { status, body } = await post('update_task', { taskId: 'task-1', expectedRevision: 3, transition: 'claim' })
  assert.equal(status, 200)
  assert.equal(body.task.revision, 4)
  assert.deepEqual(team.calls.at(-1)[2], { taskId: 'task-1', expectedRevision: 3, action: 'claim' })
})

await test('surfaces a stale revision as a conflict instead of overwriting', async () => {
  const { status, body } = await post('update_task', { taskId: 'task-1', expectedRevision: 2, transition: 'claim' })
  assert.equal(status, 409)
  assert.ok(body.error.includes('STALE'))
})

await test('refuses a transition it does not own, such as delete', async () => {
  const { status, body } = await post('update_task', { taskId: 'task-1', expectedRevision: 3, transition: 'delete' })
  assert.equal(status, 400)
  assert.ok(body.error.includes('delete'))
})

await test('refuses an update with no revision', async () => {
  const { status, body } = await post('update_task', { taskId: 'task-1', transition: 'claim' })
  assert.equal(status, 400)
  assert.ok(body.error.includes('revision'))
})

await test('passes reassignment through with the chosen owner', async () => {
  await post('update_task', { taskId: 'task-1', expectedRevision: 3, transition: 'reassign', owner: 'dbuilder' })
  assert.equal(team.calls.at(-1)[2].owner, 'dbuilder')
})

await test('sends a durable message to a member', async () => {
  const { status, body } = await post('send_message', { target: 'dbuilder', message: 'Please land T2 first.' })
  assert.equal(status, 200)
  assert.equal(body.target, 'dbuilder')
  const call = team.calls.at(-1)
  assert.deepEqual(call[2].content, [{ type: 'text', text: 'Please land T2 first.' }])
})

await test('refuses a message with no content', async () => {
  const { status } = await post('send_message', { target: 'dbuilder', message: '' })
  assert.equal(status, 400)
})

await test('refuses an unknown action by name', async () => {
  const { status, body } = await post('drop_database', {})
  assert.equal(status, 400)
  assert.ok(body.error.includes('drop_database'))
})

await test('refuses an action with no session rather than guessing a team', async () => {
  const { status, body } = await post('create_task', { subject: 'x', description: 'y' }, '')
  assert.equal(status, 400)
  assert.ok(body.error.includes('session'))
})

await test('refuses an action for a session that has no live agent', async () => {
  const { status, body } = await post('create_task', { subject: 'x', description: 'y' }, 'sess-gone')
  assert.equal(status, 409)
  assert.ok(body.error.includes('sess-gone'))
})

await test('answers 501 when this composition has no Agent Teams service', async () => {
  const bare = makeContext()
  apply(bare.ctx, Config({ root: workspaceRoot, ttlMs: 0 }))
  const response = await bare.routes[0].fetch(
    new Request('http://127.0.0.1:19387/api/project-cockpit', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ action: 'read_team', sessionId: 'sess-1' }),
    }),
  )
  assert.equal(response.status, 501)
})

await test('a malformed body is a 400, not a crash', async () => {
  const response = await actRoute.fetch(
    new Request('http://127.0.0.1:19387/api/project-cockpit', { method: 'POST', headers: { 'content-type': 'application/json' }, body: 'not json' }),
  )
  assert.equal(response.status, 400)
})

await test('can be switched off entirely, leaving the read-only panel', async () => {
  const readOnly = makeContext({ agentTeams: team.service, agents: makeAgents('sess-1') })
  apply(readOnly.ctx, Config({ root: workspaceRoot, allowActions: false }))
  const response = await readOnly.routes[0].fetch(
    new Request('http://127.0.0.1:19387/api/project-cockpit', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ action: 'read_team', sessionId: 'sess-1' }),
    }),
  )
  assert.equal(response.status, 403)
  // The GET half still answers.
  const indexResponse = await readOnly.routes[0].fetch(new Request('http://127.0.0.1:19387/api/project-cockpit?view=index'))
  assert.equal(indexResponse.status, 200)
})

await testWithFixture('reads whichever project the session is in, not the one it lives in', async () => {
  // The question this answers: can this plugin be moved to its own folder and
  // still serve any project? The tool is anchored to the calling session's cwd,
  // so two calls from two directories must read two different projects â€” with
  // the plugin's own location never consulted.
  const other = path.join(workspaceRoot, '.cockpit')
  assert.notEqual(other, workspaceRoot, 'the two roots must actually differ')
  const here2 = await callTool({ view: 'overview', refresh: true }, { agent: { session: { header: { cwd: workspaceRoot } } } })
  const there = await callTool({ view: 'overview', refresh: true }, { agent: { session: { header: { cwd: other } } } })
  assert.equal(here2.root, workspaceRoot)
  assert.equal(there.root, other)
  assert.equal(here2.hasManifest, true, 'the workspace declares a manifest')
  assert.equal(there.hasManifest, false, 'a directory without one must report no manifest, not the other project')
})

await test('an explicit root pins one project regardless of the session', async () => {
  const pinned2 = makeContext()
  apply(pinned2.ctx, Config({ root: workspaceRoot, ttlMs: 0 }))
  const value = await pinned2.tools[0].execute({ view: 'overview' }, { signal: new AbortController().signal, agent: { session: { header: { cwd: '/somewhere/else' } } } })
  assert.equal(value.root, workspaceRoot, 'a configured root wins over the session cwd')
})

await testWithFixture('the browser route resolves the project from the named session', async () => {
  // The panel is mounted once for the whole shell, so it names the session it is
  // showing and the route resolves that session's project â€” the same directory
  // the tool would use. Without this the route could only guess.
  const routed = makeContext({ agents: { get: (id) => (id === 'sess-9' ? { session: { header: { cwd: workspaceRoot } } } : undefined) } })
  apply(routed.ctx, Config({ ttlMs: 0 }))
  const response = await routed.routes[0].fetch(new Request('http://127.0.0.1:19387/api/project-cockpit?view=index&session=sess-9'))
  const payload = await response.json()
  assert.equal(payload.root, workspaceRoot)
  assert.equal(payload.rootSource, 'session working directory')
  assert.equal(payload.index.hasManifest, true)
})

await test('the browser route says which rule chose the project', async () => {
  const pinned3 = makeContext()
  apply(pinned3.ctx, Config({ root: workspaceRoot, ttlMs: 0 }))
  const payload = await (await pinned3.routes[0].fetch(new Request('http://127.0.0.1:19387/api/project-cockpit?view=index&session=anything'))).json()
  assert.equal(payload.rootSource, 'configured root', 'a configured root must be reported as the reason')
})

await test('the browser route falls back and admits it when no session is named', async () => {
  const bare2 = makeContext()
  apply(bare2.ctx, Config({ ttlMs: 0 }))
  const payload = await (await bare2.routes[0].fetch(new Request('http://127.0.0.1:19387/api/project-cockpit?view=index'))).json()
  assert.equal(payload.rootSource, 'host working directory', 'the last resort must be named, not hidden')
})

await test('serves the route root as JSON for the panel footer', async () => {
  const pinned4 = makeContext({ agents: { get: () => undefined } })
  apply(pinned4.ctx, Config({ root: workspaceRoot, ttlMs: 0 }))
  const payload = await (await pinned4.routes[0].fetch(new Request('http://127.0.0.1:19387/api/project-cockpit?view=capabilities'))).json()
  assert.equal(payload.root, workspaceRoot)
  assert.equal(payload.contract, 2)
  assert.ok(payload.actions.includes('create_task'))
})

process.stdout.write(`\n${passed} passed, ${failed} failed, ${skipped} skipped\n`)
if (failed > 0) process.exitCode = 1
