/**
 * Project Cockpit — manifest validation and normalization.
 *
 * The manifest is the project's declaration of intent: which components exist,
 * how they depend on each other, which agents will build them, what each agent
 * is handed, and which files each one may write. Nothing here touches the
 * filesystem; this module is pure so it can be tested directly and reused by
 * both the host half and any offline tooling.
 *
 * Validation is deliberately strict and reports every problem it finds with a
 * JSON pointer, because a silently misread manifest would draw a confident,
 * wrong architecture.
 */

/** Component lifecycle values, most- to least-progressed. */
export const COMPONENT_STATUSES = ['done', 'in-progress', 'scaffolded', 'proposed', 'blocked', 'cut']

/** Task lifecycle values. */
export const TASK_STATUSES = ['completed', 'in_progress', 'pending', 'blocked']

const ID_PATTERN = /^[a-z0-9][a-z0-9-]*$/

/**
 * Normalize a workspace-relative path for comparison: forward slashes, no
 * leading or trailing separator, no `./` prefix. Paths are compared as
 * component prefixes, so `docs/adr` and `docs/adr/0001.md` overlap.
 *
 * @param value raw path from a manifest.
 * @returns the normalized path.
 */
export function normalizePath(value) {
  return String(value ?? '')
    .replace(/\\/gu, '/')
    .replace(/^\.\//u, '')
    .replace(/\/+/gu, '/')
    .replace(/\/+$/u, '')
    .trim()
}

/**
 * Whether two normalized path prefixes name the same file or nest inside one
 * another. This is the advisory-overlap rule the team task board uses for
 * `writeScopes`, applied here to agent ownership.
 *
 * @param left first normalized prefix.
 * @param right second normalized prefix.
 * @returns whether either prefix contains the other.
 */
export function pathsOverlap(left, right) {
  if (left === '' || right === '') return false
  return left === right || left.startsWith(`${right}/`) || right.startsWith(`${left}/`)
}

/**
 * Collect one validation problem.
 * @param problems accumulator.
 * @param path JSON pointer-ish location of the problem.
 * @param message human-readable explanation.
 */
function problem(problems, path, message) {
  problems.push({ path, message })
}

/**
 * Require a string field.
 * @returns the trimmed string, or the fallback when absent.
 */
function str(value, fallback = '') {
  return typeof value === 'string' ? value.trim() : fallback
}

/**
 * Normalize a list of path-like strings.
 * @param value candidate array.
 * @returns normalized, de-duplicated, non-empty paths in declaration order.
 */
function pathList(value) {
  if (!Array.isArray(value)) return []
  const out = []
  for (const entry of value) {
    const normalized = normalizePath(entry)
    if (normalized !== '' && !out.includes(normalized)) out.push(normalized)
  }
  return out
}

/**
 * Normalize a list of ids.
 * @param value candidate array.
 * @returns de-duplicated non-empty ids in declaration order.
 */
function idList(value) {
  if (!Array.isArray(value)) return []
  const out = []
  for (const entry of value) {
    const id = str(entry)
    if (id !== '' && !out.includes(id)) out.push(id)
  }
  return out
}

/**
 * Validate and normalize a raw manifest object.
 *
 * The returned `problems` array is empty when the manifest is sound. Every
 * problem carries the location so a user can fix it without guessing; the
 * normalized model is still returned so the UI can render whatever is valid
 * and mark the rest, rather than showing nothing.
 *
 * @param raw parsed manifest value (untrusted).
 * @returns `{ model, problems }` where `model` is the normalized manifest.
 */
export function validateManifest(raw) {
  const problems = []
  if (raw === null || typeof raw !== 'object' || Array.isArray(raw)) {
    return {
      model: emptyModel(),
      problems: [{ path: '/', message: 'manifest must be a JSON object' }],
    }
  }

  if (raw.schemaVersion !== 1) {
    problem(problems, '/schemaVersion', `expected schemaVersion 1, found ${JSON.stringify(raw.schemaVersion)}`)
  }

  const projectRaw = raw.project
  let project = { name: '', summary: '', stage: 'design', docsRoot: '', testCommand: '', runCommand: '' }
  if (projectRaw === null || typeof projectRaw !== 'object' || Array.isArray(projectRaw)) {
    problem(problems, '/project', 'missing required "project" object')
  } else {
    project = {
      name: str(projectRaw.name),
      summary: str(projectRaw.summary),
      stage: str(projectRaw.stage, 'design'),
      docsRoot: normalizePath(projectRaw.docsRoot),
      testCommand: str(projectRaw.testCommand),
      runCommand: str(projectRaw.runCommand),
    }
    if (project.name === '') problem(problems, '/project/name', 'project.name is required')
    if (!['design', 'scaffold', 'building', 'hardening', 'shipped'].includes(project.stage)) {
      problem(problems, '/project/stage', `unknown stage ${JSON.stringify(project.stage)}`)
    }
  }

  // --- components -----------------------------------------------------------
  const components = []
  const seenComponentIds = new Set()
  if (!Array.isArray(raw.components) || raw.components.length === 0) {
    problem(problems, '/components', 'at least one component is required')
  } else {
    raw.components.forEach((entry, index) => {
      const at = `/components/${index}`
      if (entry === null || typeof entry !== 'object' || Array.isArray(entry)) {
        problem(problems, at, 'component must be an object')
        return
      }
      const id = str(entry.id)
      if (id === '') problem(problems, `${at}/id`, 'component id is required')
      else if (!ID_PATTERN.test(id)) problem(problems, `${at}/id`, `id ${JSON.stringify(id)} must be lowercase kebab-case`)
      else if (seenComponentIds.has(id)) problem(problems, `${at}/id`, `duplicate component id ${JSON.stringify(id)}`)
      seenComponentIds.add(id)

      const status = str(entry.status, 'proposed')
      if (!COMPONENT_STATUSES.includes(status)) problem(problems, `${at}/status`, `unknown status ${JSON.stringify(status)}`)

      components.push({
        id,
        name: str(entry.name, id),
        kind: str(entry.kind, 'component'),
        layer: str(entry.layer, 'unassigned'),
        language: str(entry.language),
        runtime: str(entry.runtime),
        status,
        summary: str(entry.summary),
        dependsOn: idList(entry.dependsOn),
        paths: pathList(entry.paths),
        docs: pathList(entry.docs),
        decisions: pathList(entry.decisions),
        notes: str(entry.notes),
      })
    })
  }

  // Dependency edges must point at declared components, and must not form a
  // cycle — a cycle would render as a knot and means the design is unbuildable
  // in the stated order.
  components.forEach((component, index) => {
    component.dependsOn.forEach((target, edgeIndex) => {
      if (!seenComponentIds.has(target)) {
        problem(problems, `/components/${index}/dependsOn/${edgeIndex}`, `unknown component ${JSON.stringify(target)}`)
      }
      if (target === component.id) {
        problem(problems, `/components/${index}/dependsOn/${edgeIndex}`, 'component depends on itself')
      }
    })
  })
  for (const cycle of findCycles(components)) {
    problem(problems, '/components', `dependency cycle: ${cycle.join(' → ')}`)
  }

  // --- tasks ----------------------------------------------------------------
  const tasks = []
  const seenTaskIds = new Set()
  if (raw.tasks !== undefined && !Array.isArray(raw.tasks)) {
    problem(problems, '/tasks', 'tasks must be an array')
  } else {
    ;(raw.tasks ?? []).forEach((entry, index) => {
      const at = `/tasks/${index}`
      if (entry === null || typeof entry !== 'object' || Array.isArray(entry)) {
        problem(problems, at, 'task must be an object')
        return
      }
      const id = str(entry.id)
      if (id === '') problem(problems, `${at}/id`, 'task id is required')
      else if (seenTaskIds.has(id)) problem(problems, `${at}/id`, `duplicate task id ${JSON.stringify(id)}`)
      seenTaskIds.add(id)

      const status = str(entry.status, 'pending')
      if (!TASK_STATUSES.includes(status)) problem(problems, `${at}/status`, `unknown status ${JSON.stringify(status)}`)

      tasks.push({
        id,
        subject: str(entry.subject, id),
        description: str(entry.description),
        status,
        dependsOn: idList(entry.dependsOn),
        componentIds: idList(entry.componentIds),
        writeScopes: pathList(entry.writeScopes),
        owner: str(entry.owner),
        order: typeof entry.order === 'number' && Number.isFinite(entry.order) ? entry.order : tasks.length,
      })
    })
  }
  tasks.forEach((task, index) => {
    task.componentIds.forEach((target, at) => {
      if (!seenComponentIds.has(target)) {
        problem(problems, `/tasks/${index}/componentIds/${at}`, `unknown component ${JSON.stringify(target)}`)
      }
    })
    task.dependsOn.forEach((target, at) => {
      if (!seenTaskIds.has(target)) problem(problems, `/tasks/${index}/dependsOn/${at}`, `unknown task ${JSON.stringify(target)}`)
      if (target === task.id) problem(problems, `/tasks/${index}/dependsOn/${at}`, 'task depends on itself')
    })
  })

  // --- agents ---------------------------------------------------------------
  const agents = []
  const seenAgentIds = new Set()
  if (raw.agents !== undefined && !Array.isArray(raw.agents)) {
    problem(problems, '/agents', 'agents must be an array')
  } else {
    ;(raw.agents ?? []).forEach((entry, index) => {
      const at = `/agents/${index}`
      if (entry === null || typeof entry !== 'object' || Array.isArray(entry)) {
        problem(problems, at, 'agent must be an object')
        return
      }
      const id = str(entry.id)
      if (id === '') problem(problems, `${at}/id`, 'agent id is required')
      else if (!ID_PATTERN.test(id)) problem(problems, `${at}/id`, `id ${JSON.stringify(id)} must be lowercase kebab-case`)
      else if (seenAgentIds.has(id)) problem(problems, `${at}/id`, `duplicate agent id ${JSON.stringify(id)}`)
      seenAgentIds.add(id)

      const taskId = str(entry.taskId)
      if (taskId !== '' && !seenTaskIds.has(taskId)) {
        problem(problems, `${at}/taskId`, `unknown task ${JSON.stringify(taskId)}`)
      }

      agents.push({
        id,
        role: str(entry.role, id),
        emoji: str(entry.emoji),
        purpose: str(entry.purpose),
        taskId,
        task: str(entry.task),
        componentIds: idList(entry.componentIds),
        readPaths: pathList(entry.readPaths),
        writeScopes: pathList(entry.writeScopes),
        model: str(entry.model),
        notes: str(entry.notes),
      })
    })
  }
  agents.forEach((agent, index) => {
    agent.componentIds.forEach((target, at) => {
      if (!seenComponentIds.has(target)) {
        problem(problems, `/agents/${index}/componentIds/${at}`, `unknown component ${JSON.stringify(target)}`)
      }
    })
  })

  // Two agents writing the same path is the classic way a team corrupts its own
  // work. It is advisory, never fatal: the cockpit reports it and the Lead
  // decides. A prefix overlap counts, because one agent creating a directory
  // and another adding a file inside it collide just as surely.
  const collisions = []
  for (let i = 0; i < agents.length; i += 1) {
    for (let j = i + 1; j < agents.length; j += 1) {
      for (const left of agents[i].writeScopes) {
        for (const right of agents[j].writeScopes) {
          if (pathsOverlap(left, right)) {
            collisions.push({ a: agents[i].id, b: agents[j].id, left, right })
          }
        }
      }
    }
  }

  // --- invariants -----------------------------------------------------------
  const invariants = []
  if (raw.invariants !== undefined && !Array.isArray(raw.invariants)) {
    problem(problems, '/invariants', 'invariants must be an array')
  } else {
    ;(raw.invariants ?? []).forEach((entry, index) => {
      const at = `/invariants/${index}`
      if (entry === null || typeof entry !== 'object' || Array.isArray(entry)) {
        problem(problems, at, 'invariant must be an object')
        return
      }
      const id = str(entry.id)
      if (id === '') problem(problems, `${at}/id`, 'invariant id is required')
      const statement = str(entry.statement)
      if (statement === '') problem(problems, `${at}/statement`, 'invariant statement is required')
      invariants.push({
        id,
        statement,
        componentIds: idList(entry.componentIds),
        record: normalizePath(entry.record),
      })
    })
  }

  return {
    model: { schemaVersion: 1, project, components, tasks, agents, invariants, collisions },
    problems,
  }
}

/**
 * Find dependency cycles among components using an iterative depth-first walk.
 *
 * @param components normalized components.
 * @returns one representative cycle per strongly connected region, as id lists.
 */
export function findCycles(components) {
  const byId = new Map(components.map((component) => [component.id, component]))
  const state = new Map() // id -> 0 unvisited, 1 on stack, 2 done
  const cycles = []
  const reported = new Set()

  for (const start of components) {
    if (state.get(start.id) === 2) continue
    const stack = [{ id: start.id, next: 0 }]
    const onPath = []
    state.set(start.id, 1)
    onPath.push(start.id)
    while (stack.length > 0) {
      const frame = stack[stack.length - 1]
      const node = byId.get(frame.id)
      const edges = node === undefined ? [] : node.dependsOn
      if (frame.next >= edges.length) {
        state.set(frame.id, 2)
        stack.pop()
        onPath.pop()
        continue
      }
      const target = edges[frame.next]
      frame.next += 1
      if (!byId.has(target)) continue
      const targetState = state.get(target) ?? 0
      if (targetState === 1) {
        const cycleStart = onPath.indexOf(target)
        const cycle = onPath.slice(cycleStart >= 0 ? cycleStart : 0).concat(target)
        const key = [...cycle].sort().join('|')
        if (!reported.has(key)) {
          reported.add(key)
          cycles.push(cycle)
        }
      } else if (targetState === 0) {
        state.set(target, 1)
        onPath.push(target)
        stack.push({ id: target, next: 0 })
      }
    }
  }
  return cycles
}

/**
 * An empty, valid model — what the UI renders when there is no manifest yet.
 * @returns an empty normalized manifest.
 */
export function emptyModel() {
  return {
    schemaVersion: 1,
    project: { name: '', summary: '', stage: 'design', docsRoot: '', testCommand: '', runCommand: '' },
    components: [],
    tasks: [],
    agents: [],
    invariants: [],
    collisions: [],
  }
}
