/**
 * Print the cockpit's own view of this workspace.
 *
 * Not a test — a way to look at what the panel will render without opening the
 * browser, and the fastest way to see a manifest mistake.
 *
 * Run: node .cockpit/tools/inspect.mjs
 */

import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { createProjectStore } from '../lib/store.js'
import { agentBrief } from '../lib/discover.js'
import { warnings, statusBreakdown, mergeAgents } from '../lib/client/viewmodel.js'

const here = path.dirname(fileURLToPath(import.meta.url))
const root = path.resolve(here, '..', '..')

const store = createProjectStore({ root })
const index = await store.get()
const status = store.manifestStatus()

const line = (label, value) => process.stdout.write(`${label.padEnd(22)} ${value}\n`)

process.stdout.write(`\n${index.project.name || '(unnamed project)'} — ${index.project.stage}\n`)
process.stdout.write(`${index.project.summary}\n\n`)
line('root', index.root)
line('manifest', `${index.manifestPath}${index.hasManifest ? '' : ' (NOT FOUND)'}`)
line('components', `${index.stats.components} across ${index.layers.length} layers`)
line('agents', `${index.stats.agents} declared`)
line('tasks', `${index.stats.tasks}`)
line('files on disk', `${index.stats.files}`)
line('declared paths', `${index.stats.declaredPaths}`)
line('declared absent', `${index.stats.missingPaths}`)
line('unclaimed files', `${index.stats.unownedFiles}`)
line('scan truncated', `${index.truncated}`)

if (status.problems.length > 0) {
  process.stdout.write(`\nmanifest problems (${status.problems.length}):\n`)
  for (const problem of status.problems) process.stdout.write(`  ${problem.path}  ${problem.message}\n`)
}

const { segments } = statusBreakdown(index.components)
process.stdout.write(`\nbuild state: ${segments.map((segment) => `${segment.count} ${segment.label}`).join(' · ')}\n`)

if (index.collisions.length > 0) {
  process.stdout.write(`\nwrite-scope collisions (advisory):\n`)
  for (const collision of index.collisions) {
    process.stdout.write(`  ${collision.a} + ${collision.b}: ${collision.left} / ${collision.right}\n`)
  }
}

process.stdout.write('\ncomponents by layer:\n')
for (const layer of index.layers) {
  const rows = index.components.filter((component) => component.layer === layer)
  process.stdout.write(`  ${layer}\n`)
  for (const component of rows) {
    const deps = component.dependsOn.length === 0 ? '' : ` <- ${component.dependsOn.join(', ')}`
    process.stdout.write(`    ${component.id.padEnd(20)} ${component.status.padEnd(12)} ${component.fileCount} file(s)${deps}\n`)
  }
}

process.stdout.write('\nagents and the files they are handed:\n')
for (const agent of index.agents) {
  const brief = agentBrief(agent)
  process.stdout.write(`  ${agent.emoji} ${agent.id} — ${agent.role}\n`)
  process.stdout.write(`      task: ${agent.task === undefined ? '(none)' : `${agent.task.id} ${agent.task.subject} [${agent.task.status}${agent.task.blocked ? ', blocked' : ''}]`}\n`)
  process.stdout.write(`      read:  ${brief.read.length === 0 ? '(none)' : brief.read.join(', ')}\n`)
  process.stdout.write(`      write: ${brief.write.length === 0 ? '(none)' : brief.write.join(', ')}\n`)
  if (brief.missing.length > 0) process.stdout.write(`      absent: ${brief.missing.join(', ')}\n`)
}

process.stdout.write('\ntasks:\n')
for (const task of index.tasks) {
  const blockers = task.blockers.length === 0 ? '' : ` blocked by ${task.blockers.map((blocker) => blocker.id).join(', ')}`
  process.stdout.write(`  ${task.id} ${task.status.padEnd(12)} ${task.subject}${blockers}\n`)
}

const merged = mergeAgents(index, undefined)
process.stdout.write(`\nroster vs live team: ${merged.declared} declared, ${merged.notCreated} not created (no team in this process)\n`)

const rows = warnings(index, undefined)
process.stdout.write(`\nfindings (${rows.length}):\n`)
for (const row of rows) process.stdout.write(`  [${row.level}] ${row.text}\n`)
process.stdout.write('\n')
