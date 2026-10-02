/**
 * Project Cockpit — check the standalone preview is a real page, not a fragment.
 *
 * A generator that emits a broken page is worse than no generator, so this reads
 * the produced HTML and asserts the things a browser needs: a doctype, a closed
 * document, one pane per tab with content in it, the stylesheet inlined, and no
 * serialization artefacts (`undefined`, `NaN`, `[object Object]`) that would mean
 * a value went missing on the way out.
 *
 * Run: node tools/check-preview.mjs [path-to-preview.html]
 */

import { existsSync, readFileSync, statSync } from 'node:fs'
import path from 'node:path'

const target = process.argv[2] === undefined ? path.join(process.cwd(), 'cockpit-preview.html') : path.resolve(process.argv[2])

/**
 * Find the index just past the `</div>` that closes a `<div>` opened at `start`.
 *
 * Counts nested opens and closes so the whole pane is measured. Void elements do
 * not matter here: this page contains none inside a pane.
 *
 * @param source the HTML.
 * @param start index just after the opening tag.
 * @returns the index of the closing tag's end.
 */
function balancedDivEnd(source, start) {
  const tag = /<(\/?)div\b[^>]*>/gu
  tag.lastIndex = start
  let depth = 1
  let match
  while ((match = tag.exec(source)) !== null) {
    depth += match[1] === '/' ? -1 : 1
    if (depth === 0) return match.index
  }
  return source.length
}

if (!existsSync(target)) {
  process.stderr.write(`No preview at ${target}. Generate one with:\n  node tools/preview.mjs "<project dir>"\n`)
  process.exitCode = 1
} else {
  const html = readFileSync(target, 'utf8')
  let failed = 0
  const check = (label, ok, detail) => {
    process.stdout.write(`  ${ok ? 'ok  ' : 'FAIL'} ${label}${detail === undefined ? '' : ` — ${detail}`}\n`)
    if (!ok) failed += 1
  }

  check('starts with a doctype', html.startsWith('<!doctype html>'))
  check('closes the document', html.trimEnd().endsWith('</html>'))
  check('inlines the panel stylesheet', html.includes('.pcx-root') && html.includes('--dsw-alias-label-primary'))
  check('is one file, no external requests', !/<(?:script|link)[^>]+(?:src|href)=/iu.test(html), 'no src/href attributes')

  // Every tab must have a matching, non-empty pane. Content is extracted with a
  // balanced-tag scan rather than a lazy regex: the panes nest dozens of divs, so
  // a regex stops at the first `</div>` and reports a full pane as empty.
  const tabIds = [...html.matchAll(/data-tab="([^"]+)"/gu)].map((match) => match[1])
  const panes = new Map()
  for (const match of html.matchAll(/<div class="pcx-view" data-view="([^"]+)"/gu)) {
    const start = match.index + match[0].length
    const end = balancedDivEnd(html, start)
    panes.set(match[1], html.slice(start, end))
  }
  check('five tabs', tabIds.length === 5, tabIds.join(', '))
  for (const id of tabIds) {
    const body = panes.get(id) ?? ''
    check(`  pane "${id}" renders real content`, body.length > 400 && body.includes('<'), `${body.length} chars`)
  }

  // And each pane says what it is about, so a mis-wired generator cannot pass on
  // size alone.
  const markers = {
    architecture: ['Build state', 'pcx-node'],
    agents: ['Roster', 'Files provided'],
    tasks: ['Written plan', 'pcx-taskcard'],
    files: ['Which files each agent is handed'],
    verify: ['Findings', 'Coverage'],
  }
  for (const [id, needles] of Object.entries(markers)) {
    const body = panes.get(id) ?? ''
    const missing = needles.filter((needle) => !body.includes(needle))
    check(`  pane "${id}" is the right view`, missing.length === 0, missing.length === 0 ? undefined : `missing ${missing.join(', ')}`)
  }

  check('the tab switch script is present', html.includes("querySelectorAll('[data-tab]')"))
  check('no serialization artefacts', !/\bundefined\b|\bNaN\b|\[object Object\]/u.test(html))

  const kilobytes = (statSync(target).size / 1024).toFixed(1)
  process.stdout.write(`\n${failed === 0 ? 'all checks passed' : `${failed} failed`} · ${kilobytes} KB · ${path.basename(target)}\n`)
  if (failed > 0) process.exitCode = 1
}
