/**
 * Project Cockpit — workspace discovery and index derivation.
 *
 * Two responsibilities, kept apart on purpose:
 *   - `scanWorkspace` walks a directory and returns a bounded file list. It
 *     touches the filesystem, so the host half owns it.
 *   - `buildProjectIndex` is pure: manifest + real files + document extracts
 *     in, one view model out. The UI and the tests both consume this, and it is
 *     where "planned" is separated from "actually on disk".
 */

import { normalizePath, pathsOverlap, emptyModel } from './manifest.js'

/** Directories never worth showing in an architecture view. */
export const IGNORED_DIRECTORIES = new Set([
  '.git',
  '.hg',
  '.svn',
  'node_modules',
  '.pnpm-store',
  '.venv',
  'venv',
  '__pycache__',
  '.mypy_cache',
  '.pytest_cache',
  '.ruff_cache',
  '.turbo',
  '.next',
  '.nuxt',
  'dist',
  'build',
  'out',
  'target',
  'coverage',
  '.cache',
  '.idea',
  '.vscode-server',
])

/** File extensions the cockpit treats as prose worth extracting. */
export const DOCUMENT_EXTENSIONS = new Set(['.md', '.mdx', '.txt', '.rst'])

/** Largest single file the scanner will report; bigger files are listed but not read. */
export const MAX_FILE_BYTES = 4 * 1024 * 1024

/**
 * Walk a directory tree breadth-first, returning a bounded, pruned file list.
 *
 * The walk is bounded on three axes — depth, file count, and per-directory
 * entries — so pointing the cockpit at a huge repository degrades into a
 * partial view instead of hanging the host. `truncated` records that it
 * happened, and the UI says so rather than pretending the tree is complete.
 *
 * @param root absolute directory to scan.
 * @param options `{ maxDepth, maxFiles, maxEntriesPerDirectory, ignore }`.
 * @returns `{ files, directories, truncated }` with workspace-relative posix paths.
 */
export async function scanWorkspace(root, options = {}) {
  const maxDepth = options.maxDepth ?? 12
  const maxFiles = options.maxFiles ?? 4000
  const maxEntriesPerDirectory = options.maxEntriesPerDirectory ?? 500
  const ignore = options.ignore ?? IGNORED_DIRECTORIES
  const fs = options.fs ?? (await import('node:fs/promises'))
  const path = options.path ?? (await import('node:path'))

  const files = []
  const directories = []
  let truncated = false

  /** @type {Array<{ dir: string, rel: string, depth: number }>} */
  const queue = [{ dir: root, rel: '', depth: 0 }]
  while (queue.length > 0) {
    const current = queue.shift()
    let entries
    try {
      entries = await fs.readdir(current.dir, { withFileTypes: true })
    } catch {
      // An unreadable directory is reported as absent rather than fatal: the
      // cockpit must still render the rest of the tree.
      continue
    }
    if (entries.length > maxEntriesPerDirectory) {
      truncated = true
      entries = entries.slice(0, maxEntriesPerDirectory)
    }
    for (const entry of entries) {
      const rel = current.rel === '' ? entry.name : `${current.rel}/${entry.name}`
      if (entry.isDirectory()) {
        if (ignore.has(entry.name) || entry.name.startsWith('.') && entry.name === '.git') continue
        directories.push(rel)
        if (current.depth + 1 <= maxDepth) {
          queue.push({ dir: path.join(current.dir, entry.name), rel, depth: current.depth + 1 })
        } else {
          truncated = true
        }
        continue
      }
      if (!entry.isFile()) continue
      if (files.length >= maxFiles) {
        truncated = true
        break
      }
      let size = 0
      let mtimeMs = 0
      try {
        const stat = await fs.stat(path.join(current.dir, entry.name))
        size = stat.size
        mtimeMs = stat.mtimeMs
      } catch {
        // Keep the entry with unknown size; a vanished file is still a fact.
      }
      files.push({ path: rel, size, mtimeMs: Math.round(mtimeMs) })
    }
    if (files.length >= maxFiles) {
      truncated = true
      break
    }
  }

  files.sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0))
  return { files, directories, truncated }
}

/**
 * Extract a navigable outline from a Markdown document.
 *
 * Design packages are read as prose, so the headings are the interface: they
 * become the cockpit's documentation map and the anchors a component links to.
 * Fenced code blocks are skipped so a `#` inside a shell transcript is not
 * mistaken for a heading.
 *
 * @param markdown raw document text.
 * @param maxHeadings cap on returned headings.
 * @returns heading rows `{ level, title, line }`.
 */
export function extractHeadings(markdown, maxHeadings = 60) {
  const headings = []
  const lines = String(markdown ?? '').split(/\r?\n/u)
  let fence = null
  for (let index = 0; index < lines.length; index += 1) {
    const line = lines[index]
    const fenceMatch = /^\s*(```+|~~~+)/u.exec(line)
    if (fenceMatch !== null) {
      if (fence === null) fence = fenceMatch[1][0]
      else if (fenceMatch[1][0] === fence) fence = null
      continue
    }
    if (fence !== null) continue
    const match = /^(#{1,6})\s+(.+?)\s*#*\s*$/u.exec(line)
    if (match === null) continue
    headings.push({ level: match[1].length, title: match[2].trim(), line: index + 1 })
    if (headings.length >= maxHeadings) break
  }
  return headings
}

/**
 * Whether a declared path is satisfied by the real tree, and by what.
 *
 * A declared path may be an exact file or a directory prefix. Both are
 * legitimate declarations; the difference matters to the reader, so it is
 * reported instead of flattened.
 *
 * `matches` always counts the real *files* a declaration stands for, whether
 * the declaration named a directory that exists or a prefix that does not — the
 * reader wants "how much is behind this", not "is this string a directory".
 *
 * @param declared normalized declared path.
 * @param fileSet set of real workspace-relative file paths.
 * @param directorySet set of real workspace-relative directory paths.
 * @returns `{ status, exact, matches, isDirectory }` with `status` one of
 *   `present` | `directory` | `missing`.
 */
export function resolveDeclaredPath(declared, fileSet, directorySet) {
  if (fileSet.has(declared)) return { status: 'present', exact: true, matches: 1, isDirectory: false }
  const prefix = `${declared}/`
  let matches = 0
  for (const file of fileSet) {
    if (file.startsWith(prefix)) matches += 1
  }
  if (directorySet.has(declared)) return { status: 'directory', exact: false, matches, isDirectory: true }
  if (matches > 0) return { status: 'directory', exact: false, matches, isDirectory: false }
  return { status: 'missing', exact: false, matches: 0, isDirectory: false }
}

/** Component statuses ordered so the map reads as a build order. */
const STATUS_RANK = { done: 0, 'in-progress': 1, scaffolded: 2, proposed: 3, blocked: 4, cut: 5 }

/**
 * Build the complete cockpit view model.
 *
 * @param input `{ manifest, scan, documents, root, generatedAt, manifestPath }`.
 *   `manifest` is a validated model from `validateManifest().model`.
 *   `scan` is the result of `scanWorkspace`.
 *   `documents` is a map of workspace-relative path → extracted headings.
 * @returns the view model the client renders.
 */
export function buildProjectIndex(input) {
  const manifest = input.manifest ?? emptyModel()
  const scan = input.scan ?? { files: [], directories: [], truncated: false }
  const documents = input.documents ?? {}

  const fileSet = new Set(scan.files.map((file) => normalizePath(file.path)))
  const directorySet = new Set(scan.directories.map((dir) => normalizePath(dir)))

  /** Resolve one declared path against the tree. */
  const resolve = (declared) => {
    const resolved = resolveDeclaredPath(declared, fileSet, directorySet)
    return {
      path: declared,
      status: resolved.status,
      exact: resolved.exact,
      matches: resolved.matches,
      /** The concrete files a directory declaration stands for, capped for payload size. */
      sample: resolved.exact || resolved.status === 'missing' ? [] : sampleUnder(declared, fileSet, 12),
      headings: documents[declared] ?? [],
    }
  }

  const components = manifest.components.map((component) => {
    const paths = component.paths.map(resolve)
    const docs = component.docs.map(resolve)
    const decisions = component.decisions.map(resolve)
    const declared = [...paths, ...docs, ...decisions]
    return {
      ...component,
      paths,
      docs,
      decisions,
      presentPaths: declared.filter((entry) => entry.status !== 'missing').length,
      missingPaths: declared.filter((entry) => entry.status === 'missing').length,
      fileCount: declared.reduce((total, entry) => total + (entry.exact ? 1 : entry.matches), 0),
      dependents: manifest.components.filter((other) => other.dependsOn.includes(component.id)).map((other) => other.id),
      rank: STATUS_RANK[component.status] ?? 9,
    }
  })

  const tasks = manifest.tasks.map((task) => {
    const blockers = task.dependsOn
      .map((id) => manifest.tasks.find((candidate) => candidate.id === id))
      .filter((candidate) => candidate !== undefined)
    return {
      ...task,
      scopeStatus: task.writeScopes.map(resolve),
      owners: manifest.agents.filter((agent) => agent.taskId === task.id || agent.id === task.owner).map((agent) => agent.id),
      blocked: task.status !== 'completed' && blockers.some((blocker) => blocker.status !== 'completed'),
      blockers: blockers.map((blocker) => ({ id: blocker.id, subject: blocker.subject, status: blocker.status })),
    }
  })

  const agents = manifest.agents.map((agent) => {
    const readPaths = agent.readPaths.map(resolve)
    const writeScopes = agent.writeScopes.map(resolve)
    const task = agent.taskId === '' ? undefined : tasks.find((candidate) => candidate.id === agent.taskId)
    const componentsOwned = agent.componentIds
      .map((id) => components.find((candidate) => candidate.id === id))
      .filter((candidate) => candidate !== undefined)
    return {
      ...agent,
      readPaths,
      writeScopes,
      task: task === undefined ? undefined : { id: task.id, subject: task.subject, status: task.status, blocked: task.blocked },
      components: componentsOwned.map((component) => ({ id: component.id, name: component.name, status: component.status })),
      /** Everything this agent is handed: read context first, then what it owns. */
      providedFiles: [
        ...readPaths.map((entry) => ({ ...entry, access: 'read' })),
        ...writeScopes.map((entry) => ({ ...entry, access: 'write' })),
      ],
      missingProvided: [...readPaths, ...writeScopes].filter((entry) => entry.status === 'missing').length,
    }
  })

  // Which component owns each real file, and which agent owns it. This is the
  // join the whole tool exists for: a file on disk, the design element it
  // belongs to, and the agent that will touch it.
  const ownerByPath = new Map()
  const claim = (filePath, claimValue) => {
    const existing = ownerByPath.get(filePath)
    if (existing === undefined) {
      ownerByPath.set(filePath, { components: [claimValue.component], agents: [...claimValue.agents] })
      return
    }
    if (!existing.components.includes(claimValue.component)) existing.components.push(claimValue.component)
    for (const agent of claimValue.agents) if (!existing.agents.includes(agent)) existing.agents.push(agent)
  }
  for (const component of components) {
    for (const entry of [...component.paths, ...component.docs, ...component.decisions]) {
      if (entry.status === 'missing') continue
      for (const file of filesUnder(entry, fileSet)) {
        claim(file, { component: component.id, agents: [] })
      }
    }
  }
  for (const agent of agents) {
    for (const entry of agent.writeScopes) {
      if (entry.status === 'missing') continue
      for (const file of filesUnder(entry, fileSet)) {
        claim(file, { component: null, agents: [agent.id] })
      }
    }
    for (const entry of agent.readPaths) {
      if (entry.status !== 'present') continue
      claim(entry.path, { component: null, agents: [agent.id] })
    }
  }

  const files = scan.files.map((file) => {
    const normalized = normalizePath(file.path)
    const owner = ownerByPath.get(normalized)
    return {
      path: normalized,
      size: file.size,
      ext: extensionOf(normalized),
      components: owner === undefined ? [] : owner.components.filter((id) => id !== null),
      agents: owner === undefined ? [] : owner.agents,
    }
  })

  const unowned = files.filter((file) => file.components.length === 0 && file.agents.length === 0)
  const collisions = manifest.collisions.map((collision) => ({
    ...collision,
    leftStatus: resolveDeclaredPath(collision.left, fileSet, directorySet).status,
    rightStatus: resolveDeclaredPath(collision.right, fileSet, directorySet).status,
  }))

  const layerOrder = []
  for (const component of components) {
    if (!layerOrder.includes(component.layer)) layerOrder.push(component.layer)
  }

  const declaredPathCount = new Set()
  for (const entry of [...components.flatMap((c) => [...c.paths, ...c.docs, ...c.decisions]), ...agents.flatMap((a) => [...a.readPaths, ...a.writeScopes]), ...tasks.flatMap((t) => t.scopeStatus)]) {
    declaredPathCount.add(entry.path)
  }

  return {
    generatedAt: input.generatedAt ?? new Date().toISOString(),
    root: input.root ?? '',
    manifestPath: input.manifestPath ?? '.cockpit/project.json',
    /** False when no manifest was found, so the UI can offer to create one. */
    hasManifest: manifest.components.length > 0,
    truncated: scan.truncated === true,
    project: manifest.project,
    layers: layerOrder,
    components,
    tasks,
    agents,
    invariants: manifest.invariants,
    collisions,
    stats: {
      files: files.length,
      directories: scan.directories.length,
      components: components.length,
      tasks: tasks.length,
      agents: agents.length,
      declaredPaths: declaredPathCount.size,
      missingPaths: [...components.flatMap((c) => [...c.paths, ...c.docs, ...c.decisions]), ...agents.flatMap((a) => [...a.readPaths, ...a.writeScopes])].filter((entry) => entry.status === 'missing').length,
      unownedFiles: unowned.length,
      duplicateComponents: scan.files.length - files.length,
    },
    files,
    unownedFiles: unowned.slice(0, 200).map((file) => file.path),
  }
}

/**
 * List the real files a declared path stands for.
 * @param entry a resolved declared path.
 * @param fileSet real file paths.
 * @returns matching file paths (the exact file, or every file under a directory).
 */
function filesUnder(entry, fileSet) {
  if (entry.status === 'missing') return []
  if (entry.exact) return [entry.path]
  const prefix = `${entry.path}/`
  const out = []
  for (const file of fileSet) if (file.startsWith(prefix)) out.push(file)
  return out
}

/**
 * Take a bounded sample of the files under a declared directory.
 * @param declared normalized directory prefix.
 * @param fileSet real file paths.
 * @param limit maximum sample size.
 * @returns sorted sample paths.
 */
function sampleUnder(declared, fileSet, limit) {
  const prefix = `${declared}/`
  const out = []
  for (const file of fileSet) {
    if (!file.startsWith(prefix)) continue
    out.push(file)
    if (out.length >= limit) break
  }
  return out.sort()
}

/**
 * The lowercase extension including the dot, or `''` for extensionless names.
 * @param filePath workspace-relative path.
 * @returns the extension.
 */
function extensionOf(filePath) {
  const base = filePath.slice(filePath.lastIndexOf('/') + 1)
  const dot = base.lastIndexOf('.')
  return dot <= 0 ? '' : base.slice(dot).toLowerCase()
}

/**
 * The files an agent will be handed — the question the cockpit exists to
 * answer. Kept as its own function so the UI and the model-facing tool cannot
 * drift apart.
 *
 * @param agent one entry from `buildProjectIndex().agents`.
 * @returns `{ read, write, missing }` path lists.
 */
export function agentBrief(agent) {
  return {
    read: agent.readPaths.filter((entry) => entry.status !== 'missing').map((entry) => entry.path),
    write: agent.writeScopes.filter((entry) => entry.status !== 'missing').map((entry) => entry.path),
    missing: agent.providedFiles.filter((entry) => entry.status === 'missing').map((entry) => entry.path),
  }
}

/**
 * Whether any two agents claim overlapping write scopes, recomputed against a
 * live agent list rather than the manifest's own record. The team task board
 * calls this "advisory"; so does the cockpit.
 *
 * @param agents agent entries carrying `writeScopes` as resolved path objects.
 * @returns collision rows `{ a, b, left, right }`.
 */
export function findScopeCollisions(agents) {
  const collisions = []
  for (let i = 0; i < agents.length; i += 1) {
    for (let j = i + 1; j < agents.length; j += 1) {
      for (const left of agents[i].writeScopes) {
        for (const right of agents[j].writeScopes) {
          if (left.status === 'missing' && right.status === 'missing') continue
          if (pathsOverlap(left.path, right.path)) {
            collisions.push({ a: agents[i].id, b: agents[j].id, left: left.path, right: right.path })
          }
        }
      }
    }
  }
  return collisions
}
