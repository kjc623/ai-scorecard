/**
 * Project Cockpit â€” run every check.
 *
 *   node .cockpit/test/all.mjs
 *
 * Three layers, cheapest first:
 *   run.mjs     host derivation, manifest validation, real workspace scan
 *   bundle.mjs  the client bundle's contract with the shell
 *   panel.mjs   what the panel actually renders, from real index shapes
 *   host.mjs    the host plugin mounts, and every action answers
 *   composition.mjs  the shell's own client-module scanner accepts the package
 */

import { spawn } from 'node:child_process'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const here = path.dirname(fileURLToPath(import.meta.url))
const suites = ['run.mjs', 'bundle.mjs', 'panel.mjs', 'host.mjs', 'composition.mjs']

/**
 * Run one suite in its own process, streaming nothing but keeping the exit code.
 * @param file suite file name.
 * @returns its exit code.
 */
function runSuite(file) {
  return new Promise((resolve) => {
    const child = spawn(process.execPath, [path.join(here, file)], { stdio: 'inherit' })
    child.on('close', (code) => resolve(code ?? 1))
    child.on('error', () => resolve(1))
  })
}

let failed = 0
for (const suite of suites) {
  process.stdout.write(`\n=== ${suite} ===\n`)
  const code = await runSuite(suite)
  if (code !== 0) failed += 1
}

process.stdout.write(`\n${suites.length - failed}/${suites.length} suites passed\n`)
if (failed > 0) process.exitCode = 1
