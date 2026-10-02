/**
 * Project Cockpit — link the SDK packages the local test suite needs.
 *
 * The plugin's host half imports `@deepseek-ai/schemastery` and
 * `@deepseek-ai/dsh-tools`, and `test/composition.mjs` imports the shipped
 * `@deepseek-ai/dsh-client-modules`. The running harness resolves these for an
 * installed plugin; a bare Node process cannot, because DSH ships them inside its
 * own package tree rather than publishing them.
 *
 * So this script finds them and links them into `.cockpit/node_modules/`. That
 * keeps the package relocatable: the links are development-only, they are
 * recreated by re-running this, and nothing in the shipped plugin depends on
 * them. Windows junctions need no elevation; other platforms get symlinks.
 *
 * Sources, in order:
 *   1. `--from <dir>` — a DSH installation root you name;
 *   2. `DSH_SDK_DIR` — the same thing from the environment;
 *   3. the `.tools/dsh-src` extraction in this workspace;
 *   4. the running harness, read out of `app.asar` when it can be found.
 *
 * Usage:
 *   node .cockpit/tools/link-sdk.mjs              # report and link
 *   node .cockpit/tools/link-sdk.mjs --from "C:\\path\\to\\dsh"
 *   node .cockpit/tools/link-sdk.mjs --check      # report only, exit 1 on gaps
 */

import { existsSync, mkdirSync, readdirSync, readFileSync, symlinkSync, rmSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { findAsarCandidates, hasPackage, listAsarFiles, openAsar, readAsarFile } from './asar.mjs'

const here = path.dirname(fileURLToPath(import.meta.url))
const pkgRoot = path.resolve(here, '..')
const linkRoot = path.join(pkgRoot, 'node_modules', '@deepseek-ai')

/** Packages the suite needs, plus everything they pull in. */
const WANTED = [
  'schemastery',
  'dsh-tools',
  'dsh-client-modules',
  'cordis',
  'cosmokit',
  'dsh-scope',
  'dsh-llm',
  'dsh-typert-protocol',
  'dsh-util-values',
  'dsh-util-crypto',
  'dsh-brand',
  'dsh-timeout',
  'dsh-sandbox',
  'dsh-client-store',
]

const args = process.argv.slice(2)
const checkOnly = args.includes('--check')
const fromIndex = args.indexOf('--from')
const explicit = fromIndex >= 0 ? args[fromIndex + 1] : process.env.DSH_SDK_DIR

/**
 * Candidate directories that hold an `@deepseek-ai` package tree, most specific
 * first. The package is meant to be relocatable, so this must not depend on
 * sitting inside any particular workspace.
 * @returns candidate directories.
 */
function candidateRoots() {
  const roots = []
  if (explicit !== undefined && explicit !== '') {
    // Accept a directory of packages, the one that contains it, or a DSH
    // installation root.
    roots.push(
      path.join(explicit, 'node_modules', '@deepseek-ai'),
      path.join(explicit, '@deepseek-ai'),
      path.join(explicit, 'resources', 'app.asar.unpacked', 'dsh', 'node_modules', '@deepseek-ai'),
      explicit,
    )
  }
  const home = process.env.DSH_HOME ?? path.join(process.env.USERPROFILE ?? '', '.dsh')
  const runtimeNodeModules = path.join(home, 'dsh-runtimes', 'dsh-primary-runtime', 'dependencies', 'node', 'node_modules', '@deepseek-ai')
  roots.push(runtimeNodeModules)
  // A sibling extraction, if this package still lives beside one.
  roots.push(path.join(pkgRoot, '..', '.tools', 'dsh-src', 'dsh', 'node_modules', '@deepseek-ai'))
  roots.push(path.join(pkgRoot, '..', '.research', 'asar-all'))
  // The installed desktop app, whose packages live inside `app.asar` and so are
  // listed but will not resolve without extraction.
  const localAppData = process.env.LOCALAPPDATA ?? ''
  roots.push(path.join(localAppData, 'Programs', 'DeepSeek Harness', 'resources', 'app.asar.unpacked', 'dsh', 'node_modules', '@deepseek-ai'))
  return roots
}

/**
 * Find a directory that actually contains the wanted packages.
 * @returns `{ dir, found }` or undefined.
 */
function findSource() {
  for (const candidate of candidateRoots()) {
    if (!existsSync(candidate)) continue
    const found = WANTED.filter((name) => existsSync(path.join(candidate, name, 'package.json')))
    if (found.length > 0) return { dir: candidate, found }
  }
  return undefined
}

/**
 * Link one package, replacing whatever is there.
 * @param name package name under the scope.
 * @param target the real package directory.
 */
function link(name, target) {
  const linkPath = path.join(linkRoot, name)
  try {
    if (existsSync(linkPath)) rmSync(linkPath, { recursive: true, force: true })
  } catch {
    // A link we cannot clear will surface as a copy failure below, which is the
    // honest report; do not mask it here.
  }
  try {
    symlinkSync(target, linkPath, 'junction')
    return true
  } catch (error) {
    process.stderr.write(`  could not link ${name}: ${error instanceof Error ? error.message : String(error)}\n`)
    return false
  }
}

/**
 * Extract the wanted packages out of an installed harness into `.cockpit/node_modules`.
 *
 * This is what makes the package relocatable: the SDK is not on npm, so a copy of
 * the plugin that lives outside a DSH checkout has no other source for it. Files
 * are written, not linked, because an asar has no directory to link to.
 *
 * @param packages names to extract.
 * @param inside slash-separated directory in the archive holding the packages.
 * @returns `{ dir, extracted }` or undefined when no archive is usable.
 */
function extractFromAsar(packages, inside = 'dsh/node_modules/@deepseek-ai') {
  for (const archivePath of findAsarCandidates()) {
    if (!existsSync(archivePath)) continue
    let opened
    try {
      opened = openAsar(archivePath)
    } catch {
      continue
    }
    if (!hasPackage(opened, inside, packages[0])) continue
    mkdirSync(linkRoot, { recursive: true })
    let extracted = 0
    for (const name of packages) {
      if (!hasPackage(opened, inside, name)) continue
      const files = listAsarFiles(opened, `${inside}/${name}`)
      for (const relative of files) {
        const bytes = readAsarFile(opened, `${inside}/${name}/${relative}`)
        if (bytes === undefined) continue
        const out = path.join(linkRoot, name, relative)
        mkdirSync(path.dirname(out), { recursive: true })
        writeFileSync(out, bytes)
      }
      extracted += 1
    }
    if (extracted > 0) return { dir: `${archivePath}!${inside}`, extracted }
  }
  return undefined
}

const source = findSource()
if (source === undefined) {
  // No checkout to link from: read the packages out of the installed harness.
  const extracted = checkOnly ? undefined : extractFromAsar(WANTED)
  if (extracted === undefined) {
    process.stderr.write(
      [
        'No @deepseek-ai package tree found, so the local test suite cannot resolve the SDK.',
        '',
        'Point this script at one, either a directory of packages or a DSH install root:',
        '  node tools/link-sdk.mjs --from "<dsh installation root>"',
        '  node tools/link-sdk.mjs --from "<path containing @deepseek-ai>"',
        '',
        'The plugin itself does not need this — only running the tests outside the harness does.',
        '',
      ].join('\n'),
    )
    process.exitCode = 1
  } else {
    process.stdout.write(`extracted ${extracted.extracted} packages from ${extracted.dir}\n`)
    process.stdout.write('Run the suite now: node test/all.mjs\n')
  }
} else {
  process.stdout.write(`source: ${source.dir}\n`)
  mkdirSync(linkRoot, { recursive: true })
  let linked = 0
  let already = 0
  const absent = []
  for (const name of WANTED) {
    const target = path.join(source.dir, name)
    if (!existsSync(path.join(target, 'package.json'))) {
      absent.push(name)
      continue
    }
    const linkPath = path.join(linkRoot, name)
    if (existsSync(linkPath) && readFileSync(path.join(linkPath, 'package.json'), 'utf8') === readFileSync(path.join(target, 'package.json'), 'utf8')) {
      already += 1
      continue
    }
    if (checkOnly) {
      process.stdout.write(`  MISSING ${name}\n`)
      continue
    }
    if (link(name, target)) {
      linked += 1
      process.stdout.write(`  linked  ${name}\n`)
    }
  }
  process.stdout.write(`\n${linked} linked, ${already} already present\n`)
  if (absent.length > 0) {
    process.stdout.write(`Not in that source (not needed for every suite): ${absent.join(', ')}\n`)
  }
  if (checkOnly && linked === 0 && already < WANTED.length - absent.length) process.exitCode = 1
}
