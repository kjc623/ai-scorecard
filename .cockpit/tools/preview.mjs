/**
 * Project Cockpit â€” render the panel as a standalone HTML file.
 *
 * The panel lives inside the harness's `main` slot, which only exists once the
 * host has re-composed. This renders the same view model with the same stylesheet
 * into one self-contained file you can open in a browser â€” so the cockpit can be
 * seen, reviewed and shown to someone without restarting anything.
 *
 * It is not a second implementation: it reads the real index from
 * `createProjectStore` and uses the real stylesheet from `lib/client/styles.js`,
 * so what you see is what the panel draws. The tab switching is the only part
 * re-implemented, because the panel's React is the shell's.
 *
 * Usage:
 *   node tools/preview.mjs [projectDir] [--out <file>]
 */

import { writeFileSync } from 'node:fs'
import path from 'node:path'

import { createProjectStore } from '../lib/store.js'
import { agentBrief } from '../lib/discover.js'
import { cockpitCss } from '../lib/client/styles.js'
import { groupByLayer, kpis, mergeAgents, mergeTasks, planProgress, statusBreakdown, statusColor, statusLabel, warnings } from '../lib/client/viewmodel.js'

const args = process.argv.slice(2)
const outIndex = args.indexOf('--out')
const outFile = outIndex >= 0 ? path.resolve(args[outIndex + 1]) : undefined
const rootArg = args.find((value, index) => !value.startsWith('--') && index !== outIndex + 1)
const root = rootArg === undefined ? process.cwd() : path.resolve(rootArg)

const store = createProjectStore({ root })
const index = await store.get()
const status = store.manifestStatus()
const progress = planProgress(index.components)
const breakdown = statusBreakdown(index.components)
const merged = mergeAgents(index, undefined)
const taskMerge = mergeTasks(index, undefined)
const findings = warnings(index, undefined)

/** Escape text for HTML. */
const esc = (value) => String(value ?? '').replace(/[&<>"']/gu, (character) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[character])

/** A path chip: present, directory or absent. */
function chip(entry, access) {
  const color = entry.status === 'missing' ? 'var(--pcx-err)' : access === 'write' ? 'var(--pcx-brand)' : 'var(--pcx-ok)'
  const note = entry.status === 'directory' && entry.matches > 0 ? ` <span class="pcx-muted">(${entry.matches})</span>` : ''
  const tag = access === 'write' ? ' <span class="pcx-tag">write</span>' : ''
  return `<span class="pcx-badge" style="border-color:${color}"><span class="pcx-dot" style="background:${color}"></span><span class="pcx-mono">${esc(entry.path)}</span>${note}${tag}</span>`
}

/** A status pill. */
const pill = (status, label) => `<span class="pcx-badge"><span class="pcx-dot" style="background:${statusColor(status)}"></span>${esc(label ?? statusLabel(status))}</span>`

/** One KPI row. */
const kpiRow = (rows) =>
  `<div class="pcx-kpi">${rows.map((row) => `<div><div class="pcx-kpivalue">${esc(row.value)}</div><div class="pcx-kpilabel">${esc(row.label)}</div>${row.hint ? `<div class="pcx-muted pcx-mono">${esc(row.hint)}</div>` : ''}</div>`).join('')}</div>`

// --- the five tabs -----------------------------------------------------------

const architecture = `
<section class="pcx-section">
  <h2 class="pcx-section-title">Build state</h2>
  ${kpiRow(kpis(index))}
  <div style="margin-top:14px">
    <div class="pcx-bar">${breakdown.segments.map((segment) => `<div class="pcx-barseg" style="background:${segment.color};flex-grow:${segment.count}"></div>`).join('')}</div>
    <div class="pcx-legend">${breakdown.segments.map((segment) => `<span class="pcx-legenditem"><span class="pcx-swatch" style="background:${segment.color}"></span>${segment.count} ${esc(segment.label)}</span>`).join('')}</div>
  </div>
  <p class="pcx-cardmeta">${progress.done} of ${progress.total} components done (${progress.pct}%).</p>
</section>
${groupByLayer(index.components, index.layers)
  .map(
    (group) => `
<section class="pcx-layer">
  <div class="pcx-layerhead"><span class="pcx-layername">${esc(group.layer)}</span><span class="pcx-layerrule"></span><span class="pcx-muted">${group.components.length}</span></div>
  <div class="pcx-nodes">
    ${group.components
      .map(
        (component) => `
      <div class="pcx-node" style="border-left-color:${statusColor(component.status)}">
        <div class="pcx-nodetop"><span class="pcx-nodename">${esc(component.name)}</span>${pill(component.status)}</div>
        <div class="pcx-nodekind">${esc([component.kind, component.language].filter(Boolean).join(' Â· '))}</div>
        <div class="pcx-nodedeps">${component.dependsOn.length === 0 ? 'no dependencies' : `&larr; ${esc(component.dependsOn.join(', '))}`}</div>
        ${component.missingPaths > 0 ? `<div class="pcx-nodedeps pcx-warn">${component.missingPaths} declared path(s) absent</div>` : ''}
        ${component.paths.length === 0 ? '' : `<div class="pcx-deps" style="margin-top:6px">${component.paths.slice(0, 4).map((entry) => chip(entry)).join('')}</div>`}
      </div>`,
      )
      .join('')}
  </div>
</section>`,
  )
  .join('')}
${
  index.invariants.length === 0
    ? ''
    : `<section class="pcx-section"><h2 class="pcx-section-title">Invariants</h2>${index.invariants
        .map((invariant) => `<div class="pcx-inv"><span class="pcx-invid">${esc(invariant.id)}</span><span>${esc(invariant.statement)}${invariant.record === '' ? '' : ` <span class="pcx-muted pcx-mono">Â· ${esc(invariant.record)}</span>`}</span></div>`)
        .join('')}</section>`
}`

const agents = `
<section class="pcx-section">
  <h2 class="pcx-section-title">Roster</h2>
  ${kpiRow([
    { label: 'planned agents', value: `${merged.declared}` },
    { label: 'live teammates', value: `${merged.live}` },
    { label: 'active now', value: `${merged.active}` },
    { label: 'not created yet', value: `${merged.notCreated}` },
  ])}
  <p class="pcx-cardmeta">Rendered outside the harness, so live team state is unavailable â€” this is the declared roster. In the panel these read <em>running</em>, <em>inactive</em> or <em>not created</em> from the session's Agent Team projection.</p>
</section>
<div class="pcx-grid">
${index.agents
  .map((agent) => {
    const brief = agentBrief(agent)
    return `
  <div class="pcx-card" style="border-left:3px solid var(--pcx-idle)">
    <div class="pcx-cardheader">
      <span class="pcx-emoji">${esc(agent.emoji === '' ? 'ðŸ¤–' : agent.emoji)}</span>
      <span><span class="pcx-agentname">${esc(agent.id)}</span><span class="pcx-agentrole"> Â· ${esc(agent.role)}</span>
      <div class="pcx-cardmeta">${esc(agent.purpose === '' ? 'no purpose declared' : agent.purpose)}</div></span>
      <span style="flex:1"></span>${pill('not-created', 'not created')}
    </div>
    <div style="margin-top:9px"><div class="pcx-heading-l2">Task</div>
      <div class="pcx-taskbody">${agent.task === undefined ? '<span class="pcx-muted">No task declared for this agent.</span>' : `<strong>${esc(agent.task.subject)}</strong> ${pill(agent.task.status)}`}</div>
    </div>
    <div style="margin-top:9px"><div class="pcx-heading-l2">Files provided (${agent.providedFiles.length})</div>
      ${agent.providedFiles.length === 0 ? '<span class="pcx-muted">This agent is handed no files.</span>' : `<div class="pcx-deps">${agent.providedFiles.map((entry) => chip(entry, entry.access)).join('')}</div>`}
      ${brief.missing.length > 0 ? `<div class="pcx-warn pcx-mono" style="margin-top:6px">${brief.missing.length} declared path(s) are not on disk</div>` : ''}
    </div>
  </div>`
  })
  .join('')}
</div>`

const tasks = `
<section class="pcx-section">
  <h2 class="pcx-section-title">Written plan</h2>
  ${kpiRow([
    { label: 'planned tasks', value: `${index.tasks.length}` },
    { label: 'ready now', value: `${index.tasks.filter((task) => task.status === 'pending' && !task.blocked).length}` },
    { label: 'blocked', value: `${index.tasks.filter((task) => task.blocked).length}` },
    { label: 'owners', value: `${new Set(index.tasks.flatMap((task) => task.owners)).size}` },
  ])}
  <p class="pcx-cardmeta">The live board is read from the Agent Teams service inside the harness. Here the dependencies are the manifest's own, so this is the plan as written.</p>
</section>
<div class="pcx-grid">
${[...index.tasks]
  .sort((left, right) => left.order - right.order)
  .map(
    (task) => `
  <div class="pcx-taskcard" style="border-left-color:${statusColor(task.status)}">
    <div class="pcx-taskhead"><span class="pcx-mono">${esc(task.id)}</span><strong style="flex:1">${esc(task.subject)}</strong>${task.blocked ? pill('blocked') : pill(task.status)}</div>
    ${task.description === '' ? '' : `<div class="pcx-taskbody">${esc(task.description)}</div>`}
    <div class="pcx-listitem"><span class="pcx-muted">owner</span>${task.owners.length === 0 ? '<span class="pcx-muted">unassigned</span>' : task.owners.map((id) => `<span class="pcx-tag">${esc(id)}</span>`).join('')}</div>
    ${task.blockers.length === 0 ? '' : `<div class="pcx-listitem"><span class="pcx-muted">waiting on</span>${task.blockers.map((blocker) => `<span class="pcx-dep">${esc(blocker.id)} ${esc(blocker.status)}</span>`).join('')}</div>`}
    ${task.writeScopes.length === 0 ? '' : `<div class="pcx-deps">${task.scopeStatus.map((entry) => chip(entry, 'write')).join('')}</div>`}
  </div>`,
  )
  .join('')}
</div>`

const claimed = index.files.filter((file) => file.agents.length > 0)
const files = `
<section class="pcx-section">
  <h2 class="pcx-section-title">Which files each agent is handed</h2>
  <div class="pcx-table">
    ${index.agents
      .map(
        (agent) => `
    <div class="pcx-row">
      <span class="pcx-cell pcx-mono" style="width:120px;flex:none">${esc(agent.id)}</span>
      <span class="pcx-cell" style="width:70px;flex:none">${agent.readPaths.length} read</span>
      <span class="pcx-cell" style="width:80px;flex:none">${agent.writeScopes.length} write</span>
      <span class="pcx-cell pcx-muted">${esc(agent.task === undefined ? 'â€”' : agent.task.subject)}</span>
    </div>`,
      )
      .join('')}
  </div>
</section>
<section class="pcx-section">
  <h2 class="pcx-section-title">Files claimed by an agent (${claimed.length})</h2>
  ${
    claimed.length === 0
      ? '<div class="pcx-empty">No file on disk is currently claimed by a declared agent.</div>'
      : `<div class="pcx-table">${claimed
          .map(
            (file) => `
    <div class="pcx-row">
      <span class="pcx-cell pcx-mono" style="flex:1">${esc(file.path)}</span>
      <span class="pcx-cell" style="flex:none">${file.agents.map((id) => `<span class="pcx-tag" style="margin-left:4px">${esc(id)}</span>`).join('')}</span>
      <span class="pcx-cell pcx-muted" style="flex:none">${file.components.length === 0 ? '' : `component: ${esc(file.components.join(', '))}`}</span>
    </div>`,
          )
          .join('')}</div>`
  }
</section>
<section class="pcx-section">
  <h2 class="pcx-section-title">Unclaimed (${index.unownedFiles.length} shown of ${index.stats.unownedFiles})</h2>
  <div class="pcx-table">${index.unownedFiles.slice(0, 30).map((file) => `<div class="pcx-row pcx-mono pcx-muted">${esc(file)}</div>`).join('')}</div>
</section>`

const verify = `
<section class="pcx-section">
  <h2 class="pcx-section-title">Findings (${findings.length})</h2>
  ${findings.length === 0 ? '<div class="pcx-ok">Nothing is inconsistent: every declared path resolves, no write scopes overlap, and no task is blocked while in progress.</div>' : findings.map((row) => `<div class="pcx-warnbox" style="border-color:${row.level === 'err' ? 'var(--pcx-err)' : 'var(--pcx-warn)'};margin-bottom:8px">${esc(row.text)}</div>`).join('')}
</section>
<section class="pcx-section">
  <h2 class="pcx-section-title">Commands</h2>
  <div class="pcx-list">
    <div class="pcx-listitem"><span class="pcx-muted" style="min-width:90px">verify</span><code class="pcx-mono">${esc(index.project.testCommand === '' ? '(none declared)' : index.project.testCommand)}</code></div>
    <div class="pcx-listitem"><span class="pcx-muted" style="min-width:90px">manifest</span><code class="pcx-mono">${esc(index.manifestPath)}</code></div>
  </div>
</section>
${
  status.problems.length === 0
    ? ''
    : `<section class="pcx-section"><h2 class="pcx-section-title">Manifest problems</h2>${status.problems.map((problem) => `<div class="pcx-mono pcx-err">${esc(problem.path)} â€” ${esc(problem.message)}</div>`).join('')}</section>`
}
<section class="pcx-section">
  <h2 class="pcx-section-title">Coverage</h2>
  ${kpiRow([
    { label: 'files on disk', value: `${index.stats.files}` },
    { label: 'declared paths', value: `${index.stats.declaredPaths}` },
    { label: 'declared and present', value: `${index.stats.declaredPaths - index.stats.missingPaths}` },
    { label: 'declared but absent', value: `${index.stats.missingPaths}` },
    { label: 'unclaimed files', value: `${index.stats.unownedFiles}` },
  ])}
</section>`

// --- the page ----------------------------------------------------------------

const tabs = [
  { id: 'architecture', label: 'Architecture', count: index.components.length, html: architecture },
  { id: 'agents', label: 'Agents', count: index.agents.length, html: agents },
  { id: 'tasks', label: 'Tasks', count: index.tasks.length, html: tasks },
  { id: 'files', label: 'Files â†’ Agents', count: index.stats.files, html: files },
  { id: 'verify', label: 'Verify', count: findings.length, html: verify },
]

const page = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Project Cockpit â€” ${esc(index.project.name || 'untitled')}</title>
<style>
/* The panel's own stylesheet, verbatim. */
${cockpitCss()}
/* Page chrome, so this reads as a standalone document rather than a fragment. */
html,body{margin:0;height:100%;background:var(--dsw-alias-bg-base, #16171a)}
body{display:flex;align-items:stretch;justify-content:center}
.pcx-root{width:100%;max-width:1180px}
.pcx-note{max-width:1180px;margin:10px auto 0;padding:0 20px;color:var(--dsw-alias-label-secondary,#a2a9b4);font-size:11.5px}
[hidden]{display:none !important}
</style></head>
<body>
<div class="pcx-root">
  <div class="pcx-header">
    <h1 class="pcx-title">${esc(index.project.name || 'Untitled project')}${pill(index.project.stage === 'shipped' ? 'done' : index.project.stage === 'design' ? 'proposed' : 'in-progress', index.project.stage)}</h1>
    ${index.project.summary === '' ? '' : `<p class="pcx-subtitle">${esc(index.project.summary)}</p>`}
    <div class="pcx-chips">
      <span class="pcx-chip"><span class="pcx-chipdot" style="background:var(--pcx-brand)"></span>${index.stats.components} components</span>
      <span class="pcx-chip"><span class="pcx-chipdot" style="background:var(--pcx-ok)"></span>${index.agents.length} agents planned</span>
      <span class="pcx-chip">${index.stats.files} files</span>
      ${index.stats.missingPaths > 0 ? `<span class="pcx-chip" style="border-color:var(--pcx-warn)"><span class="pcx-chipdot" style="background:var(--pcx-warn)"></span>${index.stats.missingPaths} declared path(s) absent</span>` : ''}
      ${index.collisions.length > 0 ? `<span class="pcx-chip" style="border-color:var(--pcx-err)"><span class="pcx-chipdot" style="background:var(--pcx-err)"></span>${index.collisions.length} write-scope overlap(s)</span>` : ''}
    </div>
    <div class="pcx-tabs" role="tablist">
      ${tabs.map((entry, position) => `<button type="button" role="tab" class="pcx-tab${position === 0 ? ' pcx-tab-active' : ''}" data-tab="${entry.id}" aria-selected="${position === 0}">${esc(entry.label)}<span class="pcx-tabcount">${entry.count}</span></button>`).join('')}
    </div>
  </div>
  <div class="pcx-body">
    ${tabs.map((entry, position) => `<div class="pcx-view" data-view="${entry.id}"${position === 0 ? '' : ' hidden'}>${entry.html}</div>`).join('')}
  </div>
  <div class="pcx-footer">
    <span class="pcx-mono">${esc(index.root)}</span>
    <span class="pcx-mono">${esc(index.manifestPath)}</span>
    <span>read ${new Date(index.generatedAt).toLocaleString()}</span>
    <span class="pcx-muted">standalone preview</span>
  </div>
</div>
<p class="pcx-note">Rendered by <code>tools/preview.mjs</code> from the same index and the same stylesheet the panel uses. The live panel adds session-aware team state and the two task-board actions.</p>
<script>
for (const button of document.querySelectorAll('[data-tab]')) {
  button.addEventListener('click', () => {
    for (const other of document.querySelectorAll('[data-tab]')) {
      const on = other === button;
      other.classList.toggle('pcx-tab-active', on);
      other.setAttribute('aria-selected', String(on));
      document.querySelector('[data-view="' + other.dataset.tab + '"]').hidden = !on;
    }
  });
}
</script>
</body></html>
`

const target = outFile ?? path.join(root, 'cockpit-preview.html')
writeFileSync(target, page)
process.stdout.write(`wrote ${target}\n`)
process.stdout.write(`${index.project.name || '(unnamed)'} Â· ${index.stats.components} components Â· ${index.agents.length} agents Â· ${index.tasks.length} tasks Â· ${index.stats.files} files\n`)
