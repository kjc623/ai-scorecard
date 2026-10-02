/**
 * Project Cockpit â€” host half.
 *
 * Serves one derived view model of the project: the declared architecture, the
 * real workspace tree, the agents the project is planned around, and exactly
 * which files each agent will be handed. It is read-only by construction â€”
 * every command here reads, none writes.
 *
 * Two consumers, one source of truth:
 *   - the browser panel fetches `GET /api/project-cockpit` (authenticated by
 *     the Connection layer's browser session, like every other `/api` route);
 *   - the model calls the `project_cockpit` tool.
 *
 * Both go through `createProjectStore`, so the panel and the agent can never
 * disagree about what the project contains.
 */

import z from '@deepseek-ai/schemastery'
import { defineTool } from '@deepseek-ai/dsh-tools'

import { createProjectStore, MANIFEST_CANDIDATES } from './store.js'
import { agentBrief } from './discover.js'
import { performAction, readTeam, CockpitActionError, ACTION_NAMES } from './actions.js'

/** Cordis plugin name. */
export const name = 'project-cockpit'

/** Services required before this plugin may load. */
export const inject = ['tools', 'connection']

/** Loader schema. */
export const Config = z.object({
  /** Workspace root override; empty means "use each caller's session cwd". */
  root: z.string().default(''),
  /** How long a built index stays warm, in milliseconds. */
  ttlMs: z.natural().min(0).max(600000).default(2000),
  /** Maximum files the workspace walk will report. */
  maxFiles: z.natural().min(1).max(200000).default(4000),
  /** Serve the browser panel's data route. */
  serveRoute: z.boolean().default(true),
  /** Register the model-facing tool. */
  exposeTool: z.boolean().default(true),
  /** Let the panel put work on the shared team task board. */
  allowActions: z.boolean().default(true),
})

/**
 * One store per workspace root. A long-lived host can serve several projects,
 * and each keeps its own warm cache rather than fighting over one.
 * @type {Map<string, ReturnType<typeof createProjectStore>>}
 */
const stores = new Map()

/**
 * Resolve a store for a workspace root.
 * @param root absolute directory.
 * @param config resolved plugin config.
 * @returns the store.
 */
function storeFor(root, config) {
  const key = root
  const existing = stores.get(key)
  if (existing !== undefined) {
    return existing
  }
  const created = createProjectStore({ root, ttlMs: config.ttlMs, maxFiles: config.maxFiles })
  stores.set(key, created)
  return created
}

/**
 * Where the project lives for one caller.
 *
 * Nothing here is anchored to the plugin's own directory: the cockpit is a tool
 * that reads *a* project, and the project is chosen per call. Three sources, in
 * order, and the chosen root is always reported back so a wrong answer is
 * visible rather than plausible:
 *
 *   1. an explicit `root` in the profile config, which pins one project;
 *   2. the calling session's `cwd` — the directory the user started the session
 *      in, which is what "the project" means for a tool call;
 *   3. the host process's working directory, which is the honest last resort.
 *
 * @param exec tool execution context, when the caller is a tool call.
 * @param config resolved plugin config.
 * @returns absolute workspace root.
 */
function resolveRoot(exec, config) {
  if (config.root !== '') return config.root
  const sessionCwd = exec?.agent?.session?.header?.cwd
  if (typeof sessionCwd === 'string' && sessionCwd !== '') return sessionCwd
  const workspace = exec?.agent?.session?.header?.workspaceRoot
  if (typeof workspace === 'string' && workspace !== '') return workspace
  return process.cwd()
}

/**
 * Where the project lives for a browser request.
 *
 * The panel is mounted once for the whole shell, so it tells the route which
 * session it is showing and the route resolves *that* session's project — the
 * same directory the model-facing tool would use for the same session. Without
 * the session (or without a live agent for it) this falls back to the configured
 * root, then the host's own directory.
 *
 * @param sessionId the session the panel is displaying, if it said.
 * @param config resolved plugin config.
 * @param services `{ agents }` from the composition, when present.
 * @returns `{ root, source }` where `source` names which rule decided.
 */
function resolveRouteRoot(sessionId, config, services) {
  if (config.root !== '') return { root: config.root, source: 'configured root' }
  if (typeof sessionId === 'string' && sessionId !== '') {
    const agent = services?.agents?.get?.(sessionId)
    const cwd = agent?.session?.header?.cwd
    if (typeof cwd === 'string' && cwd !== '') return { root: cwd, source: 'session working directory' }
  }
  return { root: process.cwd(), source: 'host working directory' }
}

/** A compact one-line-per-item rendering of a component, for the model. */
function componentLine(component) {
  const deps = component.dependsOn.length === 0 ? '' : ` â† ${component.dependsOn.join(', ')}`
  const files = component.fileCount === 0 ? '' : ` [${component.fileCount} file(s)]`
  const missing = component.missingPaths === 0 ? '' : ` (${component.missingPaths} declared path(s) absent)`
  return `- ${component.id} â€” ${component.name} Â· ${component.status} Â· ${component.layer}/${component.kind}${deps}${files}${missing}`
}

/**
 * The index as text for the model. Deliberately terse: the whole point of the
 * tool is that the model does not have to read the manifest itself.
 *
 * @param index the view model.
 * @param mode one of `overview`, `agents`, `files`, `tasks`.
 * @returns markdown-ish plain text.
 */
export function formatIndex(index, mode) {
  const lines = []
  const stats = index.stats
  if (mode === 'overview') {
    lines.push(`Project: ${index.project.name || '(unnamed)'} Â· stage ${index.project.stage}`)
    if (index.project.summary !== '') lines.push(index.project.summary)
    lines.push(`Root: ${index.root}`)
    lines.push(`Manifest: ${index.hasManifest ? index.manifestPath : `none found (looked for ${MANIFEST_CANDIDATES.join(', ')})`}`)
    lines.push(
      `${stats.components} components Â· ${stats.agents} agents Â· ${stats.tasks} tasks Â· ` +
        `${stats.files} files on disk Â· ${stats.missingPaths} declared path(s) absent` +
        (index.truncated ? ' Â· workspace scan truncated' : ''),
    )
    if (index.components.length > 0) {
      lines.push('', 'Components:')
      for (const component of [...index.components].sort((a, b) => a.rank - b.rank)) lines.push(componentLine(component))
    }
    if (index.collisions.length > 0) {
      lines.push('', 'Write-scope collisions between agents (advisory):')
      for (const collision of index.collisions) lines.push(`- ${collision.a} and ${collision.b} both claim ${collision.left} / ${collision.right}`)
    }
    return lines.join('\n')
  }
  if (mode === 'agents') {
    if (index.agents.length === 0) return 'No agents are declared in the manifest.'
    for (const agent of index.agents) {
      const brief = agentBrief(agent)
      lines.push(`${agent.emoji ? `${agent.emoji} ` : ''}${agent.id} â€” ${agent.role}`)
      if (agent.purpose !== '') lines.push(`  purpose: ${agent.purpose}`)
      lines.push(`  task: ${agent.task === undefined ? agent.task || '(none declared)' : `${agent.task.subject} [${agent.task.status}${agent.task.blocked ? ', blocked' : ''}]`}`)
      if (agent.task !== undefined && agent.task.subject !== '') lines.push(`  statement: ${agent.task.subject}`)
      lines.push(`  components: ${agent.components.length === 0 ? '(none)' : agent.components.map((component) => component.id).join(', ')}`)
      lines.push(`  provided files (read): ${brief.read.length === 0 ? '(none)' : brief.read.join(', ')}`)
      lines.push(`  write scopes: ${brief.write.length === 0 ? '(none)' : brief.write.join(', ')}`)
      if (brief.missing.length > 0) lines.push(`  declared but absent: ${brief.missing.join(', ')}`)
      lines.push('')
    }
    return lines.join('\n').trimEnd()
  }
  if (mode === 'tasks') {
    if (index.tasks.length === 0) return 'No tasks are declared in the manifest.'
    for (const task of [...index.tasks].sort((a, b) => a.order - b.order)) {
      const owners = task.owners.length === 0 ? 'unowned' : task.owners.join(', ')
      const blockers = task.blockers.length === 0 ? '' : ` blocked by ${task.blockers.map((blocker) => `${blocker.id}(${blocker.status})`).join(', ')}`
      lines.push(`${task.id} â€” ${task.subject} Â· ${task.status} Â· owner: ${owners}${blockers}`)
      if (task.description !== '') lines.push(`  ${task.description}`)
      if (task.writeScopes.length > 0) lines.push(`  write scopes: ${task.writeScopes.join(', ')}`)
    }
    return lines.join('\n')
  }
  // files: the join the cockpit exists for â€” file, owning component, owning agents.
  const owned = index.files.filter((file) => file.components.length > 0 || file.agents.length > 0)
  lines.push(`${owned.length} of ${index.files.length} files are claimed by a component or an agent.`)
  if (index.unownedFiles.length > 0) {
    lines.push('', `Unclaimed (first ${Math.min(40, index.unownedFiles.length)}):`)
    for (const file of index.unownedFiles.slice(0, 40)) lines.push(`- ${file}`)
  }
  lines.push('', 'Claimed:')
  for (const file of owned.slice(0, 200)) {
    const parts = []
    if (file.components.length > 0) parts.push(`component ${file.components.join('+')}`)
    if (file.agents.length > 0) parts.push(`agent ${file.agents.join('+')}`)
    lines.push(`- ${file.path} â†’ ${parts.join(', ')}`)
  }
  if (owned.length > 200) lines.push(`â€¦ ${owned.length - 200} more`)
  return lines.join('\n')
}

/**
 * Serve the panel's data.
 * @param request incoming authenticated request.
 * @param config resolved plugin config.
 * @param services `{ agentTeams, agents }`, resolved per call.
 * @returns JSON response.
 */
async function serveIndex(request, config, services) {
  const url = new URL(request.url)
  const sessionId = url.searchParams.get('session') ?? ''
  const { root, source } = resolveRouteRoot(sessionId, config, services)
  const store = storeFor(root, config)
  const view = url.searchParams.get('view') ?? 'index'
  try {
    if (request.method === 'POST') return await serveAction(request, url, config, services)
    const index = url.searchParams.get('refresh') === '1' ? await store.refresh() : await store.get()
    if (view === 'manifest') {
      return Response.json({ root, rootSource: source, sessionId, ...store.manifestStatus() })
    }
    if (view === 'agent') {
      const agentId = url.searchParams.get('agent')
      const agent = index.agents.find((candidate) => candidate.id === agentId)
      if (agent === undefined) return Response.json({ error: `unknown agent ${JSON.stringify(agentId)}` }, { status: 404 })
      return Response.json({ root, rootSource: source, sessionId, agent, brief: agentBrief(agent) })
    }
    if (view === 'components') {
      return Response.json({ root, rootSource: source, generatedAt: index.generatedAt, components: index.components, layers: index.layers })
    }
    if (view === 'team') {
      try {
        return Response.json({ root, sessionId, ...readTeam({ ...services, sessionId }) })
      } catch (error) {
        if (error instanceof CockpitActionError) return Response.json({ root, error: error.message }, { status: error.status })
        throw error
      }
    }
    if (view === 'capabilities') {
      return Response.json({ root, rootSource: source, ...CAPABILITIES })
    }
    return Response.json({ root, rootSource: source, sessionId, capabilities: CAPABILITIES, index })
  } catch (error) {
    return Response.json({ root, error: error instanceof Error ? error.message : String(error) }, { status: 500 })
  }
}

/**
 * Handle one planning action for the panel.
 *
 * The cockpit may change the shared task board and send a message; it may not
 * touch files, spawn members, or change policy. The panel sends the session
 * whose team it is showing, and the action is attributed to that session's live
 * Lead â€” never to "some agent", which would attribute one project's work to
 * another.
 *
 * @param request the POST request.
 * @param url its parsed URL.
 * @param config resolved plugin config.
 * @param services `{ agentTeams, agents }`.
 * @returns JSON response.
 */
async function serveAction(request, url, config, services) {
  if (!config.allowActions) {
    return Response.json({ error: 'Planning actions are disabled for the project cockpit in this profile.' }, { status: 403 })
  }
  let body
  try {
    body = await request.json()
  } catch {
    return Response.json({ error: 'The request body must be JSON.' }, { status: 400 })
  }
  const action = typeof body?.action === 'string' ? body.action : url.searchParams.get('action') ?? ''
  const sessionId = typeof body?.sessionId === 'string' ? body.sessionId : url.searchParams.get('session') ?? ''
  try {
    if (action === 'read_team') {
      return Response.json({ sessionId, ...readTeam({ ...services, sessionId }) })
    }
    const result = await performAction(action, body?.payload ?? body, { ...services, sessionId })
    return Response.json({ sessionId, ...result })
  } catch (error) {
    if (error instanceof CockpitActionError) {
      return Response.json({ error: error.message, code: error.name }, { status: error.status })
    }
    // A team-service rejection (a stale revision, a name that is not a member)
    // is a legitimate answer, not a cockpit fault, so it carries its own text.
    return Response.json({ error: error instanceof Error ? error.message : String(error) }, { status: 409 })
  }
}

/**
 * Register the model-facing tools.
 * @param ctx plugin context carrying the `tools` service.
 * @param config resolved plugin config.
 */
function installTools(ctx, config) {
  ctx.tools.register(
    defineTool({
      name: 'project_cockpit',
      description:
        'Read the project cockpit: the declared architecture (components, layers, dependencies), the real workspace tree, ' +
        'the planned agents, the shared task plan, and exactly which files each agent will be handed. ' +
        'Read-only. Use it to orient in a project before planning or delegating work.',
      parameters: {
        view: {
          type: 'string',
          description: 'Which slice to return.',
          enum: ['overview', 'agents', 'files', 'tasks'],
        },
        refresh: {
          type: 'boolean',
          description: 'Rebuild from disk instead of using the warm cache.',
        },
      },
      output: {
        schema: {
          type: 'object',
          additionalProperties: false,
          properties: {
            root: { type: 'string', required: true },
            view: { type: 'string', required: true },
            manifestPath: { type: 'string', required: true },
            hasManifest: { type: 'boolean', required: true },
            stats: { type: 'string', required: true },
            text: { type: 'string', required: true },
            problems: { type: 'array', required: true, items: { type: 'string' } },
          },
        },
        render: (_args, value) => [{ type: 'text', text: value.text }],
      },
      isConcurrencySafe: () => true,
      async execute(args, exec) {
        const root = resolveRoot(exec, config)
        const store = storeFor(root, config)
        const index = args.refresh === true ? await store.refresh() : await store.get()
        const mode = typeof args.view === 'string' ? args.view : 'overview'
        const status = store.manifestStatus()
        const problems = status.problems.map((entry) => `${entry.path}: ${entry.message}`)
        const text = [
          problems.length === 0 ? '' : `Manifest problems:\n${problems.map((line) => `- ${line}`).join('\n')}\n`,
          formatIndex(index, mode),
        ]
          .filter((part) => part !== '')
          .join('\n')
        return {
          root,
          view: mode,
          manifestPath: index.manifestPath,
          hasManifest: index.hasManifest,
          stats: `${index.stats.components} components, ${index.stats.agents} agents, ${index.stats.tasks} tasks, ${index.stats.files} files`,
          text,
          problems,
        }
      },
    }),
  )
}

/** The action names the route accepts, re-exported for the panel to discover. */
export { ACTION_NAMES }

/**
 * What this build of the plugin can do.
 *
 * The panel and the host can be at different revisions â€” the client bundle is
 * served from the file and hot-reloaded, while the host half needs a restart â€”
 * so the panel asks before offering an action, and says which half is stale
 * instead of showing a button that will fail.
 */
export const CAPABILITIES = {
  contract: 2,
  actions: ACTION_NAMES,
  views: ['index', 'manifest', 'agent', 'components', 'team'],
}

/**
 * Read the Agent Teams and Agent registry services, if this composition has
 * them.
 *
 * Neither is a hard dependency: the cockpit's read-only half is useful without
 * Agent Teams, and a profile that omits it should lose the action buttons, not
 * the whole panel. The services are re-read per request because a plugin can be
 * enabled or disabled after this one mounts â€” the cockpit follows.
 *
 * @param ctx plugin context.
 * @returns `{ agentTeams, agents }` with `undefined` for anything absent.
 */
function resolveTeamServices(ctx) {
  let agentTeams
  let agents
  try {
    agentTeams = ctx.get('agentTeams')
  } catch {
    agentTeams = undefined
  }
  try {
    agents = ctx.get('agents')
  } catch {
    agents = undefined
  }
  return { agentTeams, agents }
}

/**
 * Plugin entry.
 * @param ctx plugin context.
 * @param config raw config, already defaulted by the Loader schema.
 */
export function apply(ctx, config) {
  const resolved = config

  if (resolved.exposeTool) {
    installTools(ctx, resolved)
  }

  if (resolved.serveRoute) {
    // The route lives and dies with this plugin's fiber, so disabling the
    // cockpit in the profile removes its endpoint rather than leaving a
    // served surface behind with no owner.
    ctx.effect(
      () =>
        ctx.connection.fetch.register({
          path: '/api/project-cockpit',
          methods: ['GET', 'POST'],
          requestBody: 'buffered',
          fetch: (request) => serveIndex(request, resolved, resolveTeamServices(ctx)),
        }),
      'project-cockpit: /api/project-cockpit',
    )
  }

  // The stores hold no watchers, so disposal only has to forget them.
  ctx.effect(() => () => {
    stores.clear()
  }, 'project-cockpit: store cache')
}
