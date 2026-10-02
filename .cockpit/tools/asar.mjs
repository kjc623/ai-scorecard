/**
 * Project Cockpit — read packages out of an Electron `app.asar`.
 *
 * The SDK packages this plugin compiles against are not published to npm; DSH
 * ships them inside its own `app.asar`. So a copy of the plugin that lives
 * outside a DSH checkout has no other way to reach them — and
 * `tools/link-sdk.mjs` needs them to run the local test suite.
 *
 * Electron's archive format is simple: a 16-byte pickle header, a JSON directory
 * of `{ offset, size }` records, then the concatenated file bytes. Read-only.
 */

import { readFileSync } from 'node:fs'
import path from 'node:path'

/**
 * Parse an asar header.
 * @param asarPath absolute path to the archive.
 * @returns `{ buffer, header, baseOffset }`.
 */
export function openAsar(asarPath) {
  const buffer = readFileSync(asarPath)
  const headerPickleSize = buffer.readUInt32LE(4)
  const headerStringSize = buffer.readUInt32LE(12)
  const header = JSON.parse(buffer.subarray(16, 16 + headerStringSize).toString('utf8'))
  return { buffer, header, baseOffset: 8 + headerPickleSize }
}

/**
 * Walk one directory level inside an asar tree.
 * @param node a directory node from the header.
 * @returns `[name, child]` pairs.
 */
export function entriesOf(node) {
  return Object.entries(node?.files ?? {})
}

/**
 * Read one file out of an opened asar.
 * @param opened the result of `openAsar`.
 * @param filePath slash-separated path inside the archive.
 * @returns the file bytes, or undefined when it is not a file.
 */
export function readAsarFile(opened, filePath) {
  let node = opened.header
  for (const part of filePath.split('/')) {
    if (part === '') continue
    node = node?.files?.[part]
    if (node === undefined) return undefined
  }
  if (node.files !== undefined) return undefined
  const start = opened.baseOffset + Number(node.offset)
  return opened.buffer.subarray(start, start + Number(node.size))
}

/**
 * Find the `app.asar` files an installed DSH may have, without hardcoding one
 * user's layout more than necessary.
 * @returns candidate archive paths, most likely first.
 */
export function findAsarCandidates() {
  const candidates = []
  const localAppData = process.env.LOCALAPPDATA
  if (typeof localAppData === 'string' && localAppData !== '') {
    candidates.push(path.join(localAppData, 'Programs', 'DeepSeek Harness', 'resources', 'app.asar'))
  }
  return candidates
}

/**
 * Whether a directory inside the asar holds a named package.
 * @param opened the opened archive.
 * @param dir slash-separated directory inside the archive.
 * @param name package directory name to look for.
 * @returns whether `dir/name/package.json` exists.
 */
export function hasPackage(opened, dir, name) {
  return readAsarFile(opened, `${dir}/${name}/package.json`) !== undefined
}

/**
 * Everything under one archive directory, as relative paths.
 * @param opened the opened archive.
 * @param dir slash-separated directory inside the archive.
 * @param limit safety bound.
 * @returns relative file paths.
 */
export function listAsarFiles(opened, dir, limit = 20000) {
  const out = []
  const walk = (node, prefix) => {
    for (const [name, child] of entriesOf(node)) {
      if (out.length >= limit) return
      const next = prefix === '' ? name : `${prefix}/${name}`
      if (child.files !== undefined) walk(child, next)
      else out.push(next)
    }
  }
  let node = opened.header
  for (const part of dir.split('/')) {
    if (part === '') continue
    node = node?.files?.[part]
    if (node === undefined) return out
  }
  walk(node, '')
  return out
}
