/**
 * Project Cockpit — the host-side project store.
 *
 * The store owns the only expensive work in the cockpit: reading the manifest,
 * walking the workspace, and extracting document outlines. Everything is
 * cached behind a short TTL and invalidated explicitly, so a browser that
 * polls the index never turns into a disk-scan loop.
 *
 * The view model it returns is plain JSON with no host objects in it, which is
 * what lets one function serve the client panel, the model-facing tool, and
 * the tests without three code paths.
 */

import { readFile, stat } from 'node:fs/promises'
import path from 'node:path'

import { validateManifest, emptyModel, normalizePath } from './manifest.js'
import { buildProjectIndex, scanWorkspace, extractHeadings, DOCUMENT_EXTENSIONS, MAX_FILE_BYTES, IGNORED_DIRECTORIES } from './discover.js'

/**
 * Where a manifest is looked for, in priority order, relative to the project root.
 *
 * The first is the normal case. `cockpit.json` is last so a tool can describe
 * itself with its own manifest without displacing the project's — which is what
 * this package does when the cockpit is pointed at its own folder.
 */
export const MANIFEST_CANDIDATES = [
  '.cockpit/project.json',
  'project.cockpit.json',
  'cockpit.project.json',
  'cockpit.json',
]

/** How long a built index stays warm before the next read rebuilds it. */
const DEFAULT_TTL_MS = 2000

/**
 * Read and parse one JSON file.
 * @param filePath absolute path.
 * @returns `{ value }` on success, `{ error }` when the file is absent or malformed.
 */
async function readJsonFile(filePath) {
  let text
  try {
    text = await readFile(filePath, 'utf8')
  } catch (error) {
    return { error: `cannot read ${filePath}: ${error instanceof Error ? error.message : String(error)}` }
  }
  try {
    return { value: JSON.parse(text) }
  } catch (error) {
    return { error: `${filePath} is not valid JSON: ${error instanceof Error ? error.message : String(error)}` }
  }
}

/**
 * Find the manifest that governs a workspace.
 *
 * @param root absolute workspace root.
 * @returns `{ path, relPath, raw }` for the first candidate that exists, else `{ path: null }`.
 */
export async function locateManifest(root) {
  for (const candidate of MANIFEST_CANDIDATES) {
    const absolute = path.join(root, candidate)
    const result = await readJsonFile(absolute)
    if (result.value !== undefined) return { path: absolute, relPath: candidate, raw: result.value }
    // A malformed manifest is reported rather than skipped: silently falling
    // through to "no manifest" would hide the user's typo.
    if (result.error !== undefined && !result.error.includes('ENOENT') && !result.error.includes('cannot read')) {
      return { path: absolute, relPath: candidate, error: result.error }
    }
  }
  return { path: null, relPath: MANIFEST_CANDIDATES[0] }
}

/**
 * Extract document outlines for every Markdown-ish file the manifest or the
 * tree mentions. Prose is the architecture in a design package, so headings are
 * promoted to first-class data instead of being left for the reader to open.
 *
 * @param root absolute workspace root.
 * @param files workspace-relative file list from the scan.
 * @param wanted extra paths to read even when absent from the scan.
 * @param limit maximum documents to read.
 * @returns map of workspace-relative path → heading rows.
 */
export async function readDocumentOutlines(root, files, wanted, limit = 120) {
  const documents = {}
  const seen = new Set()
  const consider = (filePath) => {
    const normalized = normalizePath(filePath)
    if (normalized === '' || seen.has(normalized)) return
    const ext = path.extname(normalized).toLowerCase()
    if (ext !== '' && !DOCUMENT_EXTENSIONS.has(ext)) return
    seen.add(normalized)
    return normalized
  }
  const targets = []
  for (const filePath of [...(wanted ?? []), ...files.map((file) => file.path)]) {
    const normalized = consider(filePath)
    if (normalized !== undefined && targets.length < limit) targets.push(normalized)
  }
  for (const target of targets) {
    const absolute = path.join(root, target)
    try {
      const info = await stat(absolute)
      if (info.size > MAX_FILE_BYTES) continue
      const text = await readFile(absolute, 'utf8')
      const headings = extractHeadings(text)
      if (headings.length > 0) documents[target] = headings
    } catch {
      // A document that cannot be read simply contributes no outline.
    }
  }
  return documents
}

/**
 * Create the project store for one workspace root.
 *
 * @param options `{ root, ttlMs, maxFiles, manifestPath }`.
 * @returns a store with `get()` (cached view model), `refresh()` (rebuild now),
 *   and `manifest()` (the raw manifest plus its validation problems).
 */
export function createProjectStore(options) {
  const root = path.resolve(options.root)
  const ttlMs = options.ttlMs ?? DEFAULT_TTL_MS
  const maxFiles = options.maxFiles ?? 4000
  const ignore = options.ignore ?? IGNORED_DIRECTORIES

  /** @type {{ at: number, value: unknown } | null} */
  let cache = null
  /** @type {Promise<unknown> | null} */
  let inflight = null
  let lastManifest = { relPath: MANIFEST_CANDIDATES[0], problems: [], error: undefined }

  /** Build the view model from disk. */
  async function build() {
    const located = await locateManifest(root)
    let model = emptyModel()
    let problems = []
    let manifestError = located.error

    if (located.raw !== undefined) {
      const validated = validateManifest(located.raw)
      model = validated.model
      problems = validated.problems
    } else if (located.error !== undefined) {
      problems = [{ path: '/', message: located.error }]
    }

    const scan = await scanWorkspace(root, { maxFiles, ignore })

    // Declared-but-absent documents are still worth linking, so the manifest's
    // own paths are read before the discovered tree fills in the rest.
    const declaredPaths = [
      ...model.components.flatMap((component) => [...component.paths, ...component.docs, ...component.decisions]),
      ...model.agents.flatMap((agent) => [...agent.readPaths, ...agent.writeScopes]),
      ...model.invariants.map((invariant) => invariant.record),
    ].filter((entry) => entry !== '')
    const documents = await readDocumentOutlines(root, scan.files, declaredPaths)

    lastManifest = { relPath: located.relPath, problems, error: manifestError }

    return buildProjectIndex({
      manifest: model,
      scan,
      documents,
      root,
      manifestPath: located.relPath,
      generatedAt: new Date().toISOString(),
    })
  }

  return {
    root,
    /**
     * The current view model, rebuilt at most once per TTL.
     * @returns the cockpit view model.
     */
    async get() {
      const now = Date.now()
      if (cache !== null && now - cache.at < ttlMs) return cache.value
      if (inflight !== null) return inflight
      inflight = build()
        .then((value) => {
          cache = { at: Date.now(), value }
          return value
        })
        .finally(() => {
          inflight = null
        })
      return inflight
    },
    /**
     * Rebuild immediately, ignoring the cache. Used after a write or on demand.
     * @returns the fresh view model.
     */
    async refresh() {
      cache = null
      return this.get()
    },
    /**
     * The manifest location, its validation problems, and the file's own error.
     * @returns manifest status.
     */
    manifestStatus() {
      return { ...lastManifest, candidates: MANIFEST_CANDIDATES }
    },
    /** Drop the cache without rebuilding. */
    invalidate() {
      cache = null
    },
  }
}
