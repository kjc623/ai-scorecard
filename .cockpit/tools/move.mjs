/**
 * Project Cockpit — move the package to its own folder, verified.
 *
 * The plugin is meant to be used on any project, so it should not live inside
 * one. Moving it means four things, and three of them are easy to get wrong:
 *
 *   1. copy the package (never move-and-hope: the destination is verified first);
 *   2. re-point the profile's `file:` dependency, which holds an ABSOLUTE path;
 *   3. re-link the dev-only SDK packages, whose junctions die with the old path;
 *   4. prove it — the destination's own test suite must pass there, and the
 *      profile must resolve the new path.
 *
 * Nothing is deleted. The old copy stays until you remove it yourself.
 *
 * Usage:
 *   node tools/move.mjs --to "C:\\Tools\\dsh-project-cockpit"          # dry run
 *   node tools/move.mjs --to "C:\\Tools\\dsh-project-cockpit" --apply
 */

import { cpSync, existsSync, mkdirSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs'
import { spawnSync } from 'node:child_process'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const here = path.dirname(fileURLToPath(import.meta.url))
const source = path.resolve(here, '..')
const profileDir = process.env.DSH_PROFILE_DIR ?? path.join(process.env.DSH_HOME ?? path.join(process.env.USERPROFILE ?? '', '.dsh'), 'profiles', 'desktop')
const profileManifestPath = path.join(profileDir, 'package.json')

const toIndex = process.argv.indexOf('--to')
const destination = toIndex >= 0 ? args(toIndex) : undefined
const apply = process.argv.includes('--apply')

/** Read the value after a flag, accepting a quoted path. */
function args(index) {
  const value = process.argv[index + 1]
  if (value === undefined || value.startsWith('--')) {
    process.stderr.write(`--to needs a path\n`)
    process.exit(1)
  }
  return path.resolve(value)
}

if (destination === undefined) {
  process.stderr.write('Usage: node tools/move.mjs --to "<destination>" [--apply]\n')
  process.exit(1)
}

// --- what would happen -------------------------------------------------------

process.stdout.write(`source      ${source}\n`)
process.stdout.write(`destination ${destination}\n`)
process.stdout.write(`profile     ${profileManifestPath}\n\n`)

if (destination === source || source.startsWith(destination + path.sep)) {
  process.stderr.write('Refusing: the destination is the source, or contains it.\n')
  process.exit(1)
}
if (!existsSync(profileManifestPath)) {
  process.stderr.write(`Refusing: no profile manifest at ${profileManifestPath}.\n`)
  process.exit(1)
}
if (existsSync(destination) && statSync(destination).isFile()) {
  process.stderr.write('Refusing: the destination exists and is a file.\n')
  process.exit(1)
}

const profile = JSON.parse(readFileSync(profileManifestPath, 'utf8'))
const dependencyKey = 'dsh-project-cockpit'
const currentDependency = profile.dependencies?.[dependencyKey]
const bundles = profile.dsh?.profile?.bundles ?? []
const inBundles = bundles.includes(dependencyKey)

process.stdout.write(`profile dependency now : ${String(currentDependency)}\n`)
process.stdout.write(`listed in bundles      : ${inBundles}\n`)
if (!inBundles) {
  process.stdout.write('NOTE: the package is not in dsh.profile.bundles, so the plugin is not loaded.\n')
}

if (!apply) {
  process.stdout.write('\nDry run. Re-run with --apply to copy and re-point.\n')
  process.exit(0)
}

// --- 1. copy ----------------------------------------------------------------

process.stdout.write('\ncopying...\n')
mkdirSync(destination, { recursive: true })
cpSync(source, destination, {
  recursive: true,
  // The SDK links point at the old location and must be rebuilt, not copied.
  filter: (from) => !from.includes(`${path.sep}node_modules${path.sep}@deepseek-ai`),
})
process.stdout.write(`copied to ${destination}\n`)

// --- 2. re-point the profile's absolute file: dependency --------------------

const nextProfile = structuredClone(profile)
nextProfile.dependencies = { ...nextProfile.dependencies, [dependencyKey]: `file:${destination.replace(/\\/gu, '/')}` }
writeFileSync(profileManifestPath, `${JSON.stringify(nextProfile, null, 2)}\n`)
process.stdout.write(`profile dependency -> ${nextProfile.dependencies[dependencyKey]}\n`)

// --- 3. re-link the dev-only SDK packages at the new location ---------------

process.stdout.write('\nre-linking the SDK packages...\n')
const link = spawnSync(process.execPath, [path.join(destination, 'tools', 'link-sdk.mjs')], { cwd: destination, encoding: 'utf8' })
process.stdout.write(`${link.stdout ?? ''}${link.stderr ?? ''}`)

// --- 4. prove it ------------------------------------------------------------

process.stdout.write('running the moved copy\'s own suite...\n')
const suite = spawnSync(process.execPath, [path.join(destination, 'test', 'all.mjs')], { cwd: destination, encoding: 'utf8' })
const suiteOutput = `${suite.stdout ?? ''}${suite.stderr ?? ''}`
const summary = suiteOutput.split(/\r?\n/u).filter((line) => line.includes('suites passed') || line.includes('FAIL'))
process.stdout.write(`${summary.join('\n')}\n`)

if (suite.status !== 0) {
  process.stderr.write(
    [
      '',
      'The moved copy did not pass its own suite, so the profile has NOT been left pointing at it.',
      `Revert the dependency to: ${String(currentDependency)}`,
      `and remove ${destination} when you are ready.`,
      '',
    ].join('\n'),
  )
  process.exit(1)
}

process.stdout.write(
  [
    '',
    'Done. The destination passes its own suite and the profile points at it.',
    '',
    'Still yours to do:',
    `  1. Quit DSH, then run   node "${path.join(destination, 'tools', 'sync-install.mjs')}" --apply`,
    '     (the profile holds a COPY; a restart loads it)',
    '  2. Start DSH and open the Project Cockpit icon in the left sidebar.',
    `  3. Delete the old copy at ${source} once you are satisfied.`,
    '',
  ].join('\n'),
)
