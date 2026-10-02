/**
 * Project Cockpit — sync the installed copy from this workspace.
 *
 * The workspace is the source of truth; the profile directory holds a copy that
 * `pnpm add file:` made. Windows keeps the profile's files open while DSH runs,
 * so they cannot be replaced from inside a session — run this after quitting the
 * harness and before relaunching it.
 *
 * Reading and copying only; nothing here writes to the workspace.
 *
 * Usage:
 *   node .cockpit/tools/sync-install.mjs            # report what differs
 *   node .cockpit/tools/sync-install.mjs --apply    # copy the workspace over the install
 */

import { copyFileSync, existsSync, mkdirSync, readFileSync } from 'node:fs'
import { createHash } from 'node:crypto'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const here = path.dirname(fileURLToPath(import.meta.url))
const source = path.resolve(here, '..')
// The running harness exports its profile directory; fall back to the default
// location so the script also works from a plain shell.
const profileDir = process.env.DSH_PROFILE_DIR ?? path.join(process.env.USERPROFILE ?? '', '.dsh', 'profiles', 'desktop')
const target = path.join(profileDir, 'node_modules', 'dsh-project-cockpit')

/** Every file the installed copy needs, relative to the package root. */
const FILES = [
  'package.json',
  'cordis.patch.yml',
  'README.md',
  'lib/index.js',
  'lib/actions.js',
  'lib/store.js',
  'lib/discover.js',
  'lib/manifest.js',
  'lib/client.js',
  'lib/client/index.js',
  'lib/client/styles.js',
  'lib/client/viewmodel.js',
  'schema/manifest-v1.schema.json',
]

/** The sha1 of a file, or null when it is absent. */
function digest(file) {
  if (!existsSync(file)) return null
  return createHash('sha1').update(readFileSync(file)).digest('hex').slice(0, 12)
}

const apply = process.argv.includes('--apply')
if (!existsSync(target)) {
  process.stderr.write(`The install does not exist at ${target}\nStart DSH once with the plugin in the profile, or install it with:\n  dsh plugin --profile desktop add file:${source}\n`)
  process.exitCode = 1
} else {
  let missing = 0
  let stale = 0
  let copied = 0
  for (const relative of FILES) {
    const from = path.join(source, relative)
    const to = path.join(target, relative)
    const expected = digest(from)
    const actual = digest(to)
    if (expected === null) {
      process.stdout.write(`  ??  ${relative}: not in the workspace\n`)
      continue
    }
    if (actual === null) {
      missing += 1
      process.stdout.write(`  MISSING ${relative}\n`)
    } else if (actual !== expected) {
      stale += 1
      process.stdout.write(`  STALE   ${relative}\n`)
    }
    if (actual === expected) continue
    if (!apply) continue
    try {
      mkdirSync(path.dirname(to), { recursive: true })
      copyFileSync(from, to)
      copied += 1
      process.stdout.write(`  copied  ${relative}\n`)
    } catch (error) {
      process.stderr.write(`  FAILED  ${relative}: ${error instanceof Error ? error.message : String(error)}\n`)
      process.exitCode = 1
    }
  }
  process.stdout.write(`\n${missing} missing, ${stale} stale, ${copied} copied\n`)
  if (!apply && missing + stale > 0) {
    process.stdout.write('Run again with --apply (with DSH closed) to update the install.\n')
    process.exitCode = 1
  }
  if (apply && copied > 0) {
    process.stdout.write('Install updated. Start DSH; the plugin will load at the new revision.\n')
  }
}
