/**
 * Project Cockpit — client-side derivation.
 *
 * Pure functions that turn the host's project index plus the live team
 * projection into the numbers and groupings the panels render. No React, no
 * fetch, no host APIs: if a figure in the cockpit is wrong, the reason is in
 * here and it can be tested without a browser.
 */

/** Status → display colour variable, shared by components, tasks and agents. */
export const STATUS_COLOR = {
  done: 'var(--pcx-ok)',
  completed: 'var(--pcx-ok)',
  'in-progress': 'var(--pcx-brand)',
  in_progress: 'var(--pcx-brand)',
  scaffolded: 'var(--pcx-warn)',
  running: 'var(--pcx-ok)',
  pending: 'var(--pcx-idle)',
  proposed: 'var(--pcx-idle)',
  inactive: 'var(--pcx-idle)',
  provisioning: 'var(--pcx-warn)',
  blocked: 'var(--pcx-err)',
  failed: 'var(--pcx-err)',
  cut: 'var(--pcx-idle)',
  active: 'var(--pcx-ok)',
}

/** Status → human label. */
export const STATUS_LABEL = {
  'in-progress': 'in progress',
  in_progress: 'in progress',
  done: 'done',
  completed: 'completed',
  scaffolded: 'scaffolded',
  proposed: 'proposed',
  blocked: 'blocked',
  cut: 'cut',
  pending: 'pending',
  running: 'running',
  inactive: 'inactive',
  provisioning: 'provisioning',
  failed: 'failed',
  active: 'active',
}

/**
 * Resolve a status to its colour, falling back to the muted idle colour.
 * @param status raw status string.
 * @returns a CSS colour value.
 */
export function statusColor(status) {
  return STATUS_COLOR[status] ?? 'var(--pcx-idle)'
}

/**
 * Resolve a status to a readable label.
 * @param status raw status string.
 * @returns a label.
 */
export function statusLabel(status) {
  if (status === undefined || status === null || status === '') return 'unknown'
  return STATUS_LABEL[status] ?? String(status).replace(/[_-]/gu, ' ')
}

/**
 * Count components by status, in a fixed display order.
 * @param components component rows.
 * @returns `{ segments, total }` where each segment carries its share.
 */
export function statusBreakdown(components) {
  const order = ['done', 'in-progress', 'scaffolded', 'proposed', 'blocked', 'cut']
  const counts = new Map(order.map((status) => [status, 0]))
  for (const component of components) {
    const status = order.includes(component.status) ? component.status : 'proposed'
    counts.set(status, (counts.get(status) ?? 0) + 1)
  }
  const total = components.length
  const segments = order
    .map((status) => ({
      status,
      label: statusLabel(status),
      count: counts.get(status) ?? 0,
      color: statusColor(status),
      share: total === 0 ? 0 : (counts.get(status) ?? 0) / total,
    }))
    .filter((segment) => segment.count > 0)
  return { segments, total }
}

/**
 * Group components by their declared layer, preserving first-seen layer order.
 * @param components component rows.
 * @param layers the layer order the host derived.
 * @returns `[{ layer, components }]`.
 */
export function groupByLayer(components, layers) {
  const order = [...(layers ?? [])]
  for (const component of components) if (!order.includes(component.layer)) order.push(component.layer)
  return order
    .map((layer) => ({ layer, components: components.filter((component) => component.layer === layer) }))
    .filter((group) => group.components.length > 0)
}

/**
 * How far through the plan the project is, by component count.
 * @param components component rows.
 * @returns `{ pct, done, total }`.
 */
export function planProgress(components) {
  const total = components.length
  const done = components.filter((component) => component.status === 'done').length
  return { pct: total === 0 ? 0 : Math.round((done / total) * 100), done, total }
}

/**
 * The project's headline numbers.
 * @param index the host's project index.
 * @returns KPI rows.
 */
export function kpis(index) {
  const reachable = index.components.filter((component) => component.status !== 'cut').length
  const ready = index.tasks.filter((task) => task.status === 'pending' && !task.blocked).length
  const complete = index.tasks.filter((task) => task.status === 'completed').length
  return [
    { label: 'components', value: `${reachable}`, hint: `${index.layers.length} layers` },
    { label: 'agents', value: `${index.agents.length}`, hint: 'planned roster' },
    { label: 'tasks', value: `${complete}/${index.tasks.length}`, hint: `${ready} ready` },
    { label: 'files on disk', value: `${index.stats.files}`, hint: `${index.unownedFiles.length} unclaimed` },
    { label: 'declared paths absent', value: `${index.stats.missingPaths}`, hint: `${index.stats.declaredPaths} declared` },
  ]
}

/**
 * Everything the reader needs to know is wrong, in one list. Each row names a
 * concrete consequence rather than restating the raw data.
 * @param index the host's project index.
 * @param team live team state, when a session is open.
 * @returns warning rows `{ level, text }`.
 */
export function warnings(index, team) {
  const rows = []
  if (index.truncated) {
    rows.push({ level: 'warn', text: 'The workspace scan was truncated — the file list is partial.' })
  }
  if (!index.hasManifest) {
    rows.push({ level: 'err', text: `No manifest found. The cockpit is showing the raw workspace tree; create ${index.manifestPath} to declare the architecture.` })
  }
  for (const collision of index.collisions) {
    rows.push({ level: 'warn', text: `Write-scope overlap: ${collision.a} and ${collision.b} both claim ${collision.left} / ${collision.right}.` })
  }
  for (const component of index.components) {
    if (component.status !== 'cut' && component.missingPaths > 0) {
      rows.push({ level: 'warn', text: `${component.id} declares ${component.missingPaths} path(s) that are not on disk.` })
    }
  }
  for (const task of index.tasks) {
    // A pending task with unfinished dependencies is the plan working, not a
    // problem — that is what a dependency is for. Only an in-progress task that
    // still has incomplete blockers is genuinely contradictory.
    if (task.status === 'in_progress' && task.blockers.length > 0) {
      rows.push({ level: 'warn', text: `Task ${task.id} is in progress but blocked by ${task.blockers.map((blocker) => blocker.id).join(', ')}.` })
    }
    if (task.status === 'in_progress' && task.owners.length === 0) {
      rows.push({ level: 'warn', text: `Task ${task.id} is in progress with no declared owner.` })
    }
  }
  if (team !== undefined && team.members !== undefined) {
    for (const member of team.members) {
      if (member.phase === 'failed') rows.push({ level: 'err', text: `Team member ${member.name} failed to provision${member.error === undefined ? '' : `: ${member.error}`}.` })
    }
    if (team.failure !== undefined) rows.push({ level: 'err', text: `Team projection reports a failure: ${team.failure}` })
  }
  return rows
}

/**
 * Merge the declared roster with live team state.
 *
 * The point of the merge is to answer "is the plan running?" — so an agent with
 * a live teammate shows the teammate's real status, an agent with no live
 * counterpart is plainly marked as not created, and a live member the manifest
 * never anticipated still appears (a roster that hides unknown members would
 * be lying about what is running).
 *
 * @param index the host's project index.
 * @param team live team projection, or undefined when no session is open.
 * @returns `{ rows, live, declared, unplanned }`.
 */
export function mergeAgents(index, team) {
  const members = team?.members ?? []
  const memberByName = new Map(members.map((member) => [member.name, member]))
  const rows = index.agents.map((agent) => {
    const member = memberByName.get(agent.id)
    return {
      ...agent,
      live: member,
      liveStatus: member === undefined ? 'not-created' : member.phase === 'active' ? 'active' : member.phase,
      discrepancy: member !== undefined && member.phase === 'failed',
    }
  })
  const planned = new Set(index.agents.map((agent) => agent.id))
  const unplanned = members.filter((member) => member.role === 'teammate' && !planned.has(member.name))
  const live = rows.filter((row) => row.liveStatus === 'active').length
  const failed = rows.filter((row) => row.liveStatus === 'failed').length
  const idle = rows.filter((row) => row.liveStatus === 'inactive').length
  return {
    rows,
    live: members.filter((member) => member.role === 'teammate').length,
    active: live,
    idle,
    failed,
    declared: index.agents.length,
    /** Planned agents that do not exist in the live team yet. */
    notCreated: rows.filter((row) => row.liveStatus === 'not-created').length,
    unplanned,
  }
}

/**
 * Where the declared plan and the live board agree, and where they do not.
 *
 * The manifest describes intended work; the team task board is what is actually
 * happening. Matching them on subject text is the only honest join available —
 * ids are independent — so this reports an unmatched count rather than
 * pretending every task lines up.
 * @param index the host's project index.
 * @param team live team projection.
 * @returns `{ liveTasks, matched, liveOnly, planOnly }`.
 */
export function mergeTasks(index, team) {
  const liveTasks = team?.tasks ?? []
  const key = (subject) => String(subject ?? '').trim().toLowerCase().replace(/\s+/gu, ' ')
  const liveBySubject = new Map(liveTasks.map((task) => [key(task.subject), task]))
  const matched = []
  const planOnly = []
  for (const task of index.tasks) {
    const live = liveBySubject.get(key(task.subject))
    if (live === undefined) planOnly.push(task)
    else matched.push({ planned: task, live })
  }
  const plannedKeys = new Set(index.tasks.map((task) => key(task.subject)))
  return {
    liveTasks,
    matched,
    planOnly,
    liveOnly: liveTasks.filter((task) => !plannedKeys.has(key(task.subject))),
  }
}

/**
 * The agents that will be handed a given file, keyed both ways.
 *
 * @param index the host's project index.
 * @returns `{ byFile, byComponent }` where `byFile` maps a path to the agents
 *   that read or write it, and `byComponent` maps a component id to its files.
 */
export function fileOwnership(index) {
  const byFile = new Map()
  for (const file of index.files) {
    for (const agentId of file.agents) {
      const entry = byFile.get(file.path) ?? new Set()
      entry.add(agentId)
      byFile.set(file.path, entry)
    }
  }
  const byComponent = new Map()
  for (const component of index.components) {
    const paths = new Set()
    for (const entry of [...component.paths, ...component.docs, ...component.decisions]) {
      if (entry.status === 'missing') continue
      if (entry.exact) paths.add(entry.path)
      for (const sample of entry.sample) paths.add(sample)
    }
    byComponent.set(component.id, paths)
  }
  return { byFile, byComponent }
}

/**
 * Group a flat file list into a collapsible directory tree.
 *
 * @param files file rows carrying `path`.
 * @param options `{ maxDepth }`.
 * @returns root nodes `{ name, path, dir, children, file }`.
 */
export function buildFileTree(files, options = {}) {
  const maxDepth = options.maxDepth ?? 4
  const root = { name: '', path: '', dir: true, children: new Map() }
  for (const file of files) {
    const parts = String(file.path).split('/')
    if (parts.length > maxDepth + 1) continue
    let node = root
    for (let index = 0; index < parts.length; index += 1) {
      const name = parts[index]
      const isLeaf = index === parts.length - 1
      const path = parts.slice(0, index + 1).join('/')
      let child = node.children.get(name)
      if (child === undefined) {
        child = { name, path, dir: !isLeaf, children: new Map(), file: isLeaf ? file : undefined, size: 0 }
        node.children.set(name, child)
      }
      if (isLeaf) child.file = file
      child.size += file.size ?? 0
      node = child
    }
  }
  /** Collapse the map form into sorted arrays. */
  const finalize = (node) => ({
    name: node.name,
    path: node.path,
    dir: node.dir,
    file: node.file,
    size: node.size,
    children: [...node.children.values()]
      .sort((left, right) => (left.dir === right.dir ? (left.name < right.name ? -1 : 1) : left.dir ? -1 : 1))
      .map(finalize),
  })
  return finalize(root).children
}

/**
 * Render a byte count the way a file manager does.
 * @param bytes size in bytes.
 * @returns a short human-readable size.
 */
export function formatBytes(bytes) {
  const value = Number(bytes) || 0
  if (value < 1024) return `${value} B`
  if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KB`
  return `${(value / (1024 * 1024)).toFixed(1)} MB`
}

/**
 * Which root session's team the panel should show.
 *
 * The cockpit is not itself a session, so it has to pick: the session the user
 * is actually looking at, identified by the shell's retention counter, falling
 * back to the newest root session. Subagent sessions are skipped — their team
 * belongs to their Lead.
 *
 * @param sessionsSnapshot the client `sessions` list snapshot.
 * @returns a session id, or undefined.
 */
export function pickLeadSession(sessionsSnapshot) {
  const byId = sessionsSnapshot?.byId
  if (byId === undefined || byId === null) return undefined
  const ids = sessionsSnapshot.ids ?? Object.keys(byId)
  const roots = ids.map((id) => byId[id]).filter((row) => row !== undefined && row !== null && row.subagent === undefined)
  const retained = roots.filter((row) => (row.retainedBy?.mainView ?? 0) > 0)
  return retained[0]?.sessionId ?? roots[0]?.sessionId ?? ids[0]
}

/**
 * A stable key for a list of sessions, so effects can depend on identity
 * without depending on object identity (which changes on every mutation).
 * @param sessionsSnapshot the client `sessions` list snapshot.
 * @returns a comparable string.
 */
export function sessionsKey(sessionsSnapshot) {
  const ids = sessionsSnapshot?.ids ?? []
  return ids.join('|')
}
