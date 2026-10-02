/**
 * Project Cockpit — the browser half.
 *
 * Registers two seats in the shell:
 *   - a `sidebar.panellist` glyph, which is the toolbar button's icon;
 *   - a keyed `main` panel, which is the full-width page the button opens.
 *
 * The panel reads the project index from the host half over `/api/project-cockpit`
 * (authenticated by the shell's browser session like every other `/api` route)
 * and the live team roster from the session projections the Agent Teams plugin
 * already publishes. It writes nothing anywhere.
 */

import * as React from 'react'

import { CLASS, cockpitCss } from './styles.js'
import {
  buildFileTree,
  formatBytes,
  groupByLayer,
  kpis,
  mergeAgents,
  mergeTasks,
  pickLeadSession,
  planProgress,
  sessionsKey,
  statusBreakdown,
  statusColor,
  statusLabel,
  warnings,
} from './viewmodel.js'

const NS = 'project-cockpit'
const PANEL_ID = 'project-cockpit'
const API = 'api/project-cockpit'

/** The panel's identity in the sidebar toolbar. */
export const name = PANEL_ID

/** Services the client half needs: the slot registry and the session store. */
export const inject = ['slots', 'sessions']

const h = React.createElement

// ---------------------------------------------------------------------------
// styles
// ---------------------------------------------------------------------------

/** Install the stylesheet once per page. */
function installStyles() {
  const tagId = `${NS}/cockpit`
  if (document.querySelector(`style[data-plugin-css="${tagId}"]`) !== null) return
  const tag = document.createElement('style')
  tag.dataset.plugin = NS
  tag.dataset.pluginCss = tagId
  tag.textContent = cockpitCss()
  document.head.appendChild(tag)
}

// ---------------------------------------------------------------------------
// data
// ---------------------------------------------------------------------------

/**
 * Subscribe to a plain snapshot store (`{ getSnapshot, subscribe }`) without
 * needing a store instance of our own.
 * @param source the store, or undefined when the service is absent.
 * @returns the current value, re-rendering on change.
 */
function useSnapshot(source) {
  const subscribe = React.useCallback(
    (onChange) => {
      if (source === undefined || source === null || typeof source.subscribe !== 'function') return () => {}
      return source.subscribe(onChange)
    },
    [source],
  )
  const getSnapshot = React.useCallback(() => {
    if (source === undefined || source === null || typeof source.getSnapshot !== 'function') return undefined
    return source.getSnapshot()
  }, [source])
  return React.useSyncExternalStore(subscribe, getSnapshot, getSnapshot)
}

/** Fetch one shape from the host's cockpit route. */
async function fetchCockpit(view, signal, sessionId) {
  const url = new URL(API, window.location.href)
  if (view !== undefined) url.searchParams.set('view', view)
  // Name the session whose project this is. The panel is mounted once for the
  // whole shell, so without this the host can only guess at which project the
  // reader means — and with the cockpit usable on any project, a guess is a bug.
  if (typeof sessionId === 'string' && sessionId !== '') url.searchParams.set('session', sessionId)
  const response = await fetch(url, { signal, headers: { accept: 'application/json' } })
  if (!response.ok) throw new Error(`cockpit request failed: HTTP ${response.status}`)
  const payload = await response.json()
  if (payload !== null && typeof payload === 'object' && typeof payload.error === 'string') throw new Error(payload.error)
  return payload
}

/**
 * Post one planning action.
 *
 * Read failures and action failures are separated on purpose: a read that fails
 * means the panel has nothing to draw, an action that fails is a sentence to
 * show beside the button that caused it. The host's reason is surfaced verbatim
 * — a 409 from a stale revision is information, not an error to swallow.
 *
 * @param action the action name.
 * @param payload its arguments.
 * @param sessionId the session whose team is being changed.
 * @returns the host's result.
 * @throws Error carrying the host's reason.
 */
async function postAction(action, payload, sessionId) {
  const response = await fetch(new URL(API, window.location.href), {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ action, sessionId, payload }),
  })
  const body = await response.json().catch(() => ({}))
  if (!response.ok) throw new Error(typeof body.error === 'string' ? body.error : `HTTP ${response.status}`)
  return body
}

/**
 * The panel's data: the project index from the host, plus the live team state
 * from this session's projections.
 * @param sessions the client sessions service.
 * @returns `{ index, team, loading, error, reload }`.
 */
function useCockpit(sessions) {
  const [index, setIndex] = React.useState(null)
  const [error, setError] = React.useState(null)
  const [loading, setLoading] = React.useState(true)
  const [nonce, setNonce] = React.useState(0)
  // The board read straight from the team service. It carries each task's
  // revision, which the projection does not — and an action needs the revision
  // to be a compare-and-set rather than a blind overwrite.
  const [board, setBoard] = React.useState(null)
  const [boardError, setBoardError] = React.useState(null)
  // What the running host half says it can do. The two halves can be at
  // different revisions, so the panel asks rather than assuming.
  const [capabilities, setCapabilities] = React.useState(null)
  // Which directory the host actually read, and which rule chose it. Shown in
  // the footer, because "which project am I looking at" must never be a guess.
  const [resolvedRoot, setResolvedRoot] = React.useState(null)
  const [rootSource, setRootSource] = React.useState(null)

  // Which session's project to show. Resolved before the fetch so the request
  // can name it: the host must not have to guess which project the reader means.
  const list = useSnapshot(sessions?.list)
  const sessionId = pickLeadSession(list)
  const snapshotKey = sessionsKey(list)

  React.useEffect(() => {
    const controller = new AbortController()
    let live = true
    setLoading(true)
    fetchCockpit('index', controller.signal, sessionId)
      .then((payload) => {
        if (!live) return
        setIndex(payload.index)
        setCapabilities(payload.capabilities ?? null)
        setResolvedRoot(payload.root ?? null)
        setRootSource(payload.rootSource ?? null)
        setError(null)
      })
      .catch((reason) => {
        if (!live || controller.signal.aborted) return
        setError(reason instanceof Error ? reason.message : String(reason))
      })
      .finally(() => {
        if (live) setLoading(false)
      })
    return () => {
      live = false
      controller.abort()
    }
  }, [nonce, sessionId])

  const projections = list?.projectionsBySession
  const team = sessionId === undefined ? undefined : projections?.[sessionId]?.values?.agentTeam

  // Only ask the service for the board when a session is open; the answer is
  // "no team" otherwise and the request would be noise.
  React.useEffect(() => {
    if (sessionId === undefined) {
      setBoard(null)
      setBoardError(null)
      return undefined
    }
    let live = true
    postAction('read_team', {}, sessionId)
      .then((payload) => {
        if (!live) return
        setBoard({ members: payload.members ?? [], tasks: payload.tasks ?? [] })
        setBoardError(null)
      })
      .catch((reason) => {
        if (!live) return
        setBoard(null)
        setBoardError(reason instanceof Error ? reason.message : String(reason))
      })
    return () => {
      live = false
    }
  }, [sessionId, nonce, snapshotKey])

  const reload = React.useCallback(() => setNonce((value) => value + 1), [])
  return { index, team, board, boardError, capabilities, loading, error, reload, sessionId, snapshotKey, resolvedRoot, rootSource }
}

// ---------------------------------------------------------------------------
// small building blocks
// ---------------------------------------------------------------------------

/** A coloured dot + status word. */
function StatusPill({ status, label }) {
  return h(
    'span',
    { className: CLASS.badge },
    h('span', { className: CLASS.dot, style: { background: statusColor(status) } }),
    label ?? statusLabel(status),
  )
}

/** A row of KPI figures. */
function KpiRow({ rows }) {
  return h(
    'div',
    { className: CLASS.kpi },
    rows.map((row) =>
      h(
        'div',
        { key: row.label },
        h('div', { className: CLASS.kpiValue }, row.value),
        h('div', { className: CLASS.kpiLabel }, row.label),
        row.hint === undefined ? null : h('div', { className: `${CLASS.muted} ${CLASS.mono}` }, row.hint),
      ),
    ),
  )
}

/** A labelled file path with a present/absent marker. */
function PathChip({ entry, access }) {
  const status = entry.status
  const color = status === 'missing' ? 'var(--pcx-err)' : access === 'write' ? 'var(--pcx-brand)' : 'var(--pcx-ok)'
  const title = status === 'missing'
    ? `${entry.path} — declared but not on disk`
    : status === 'directory'
      ? `${entry.path} — directory, ${entry.matches} file(s)`
      : `${entry.path} — file`
  return h(
    'span',
    { className: CLASS.badge, title, style: { borderColor: color } },
    h('span', { className: CLASS.dot, style: { background: color } }),
    h('span', { className: CLASS.mono }, entry.path),
    status === 'directory' && entry.matches > 0 ? h('span', { className: CLASS.muted }, `(${entry.matches})`) : null,
    access === 'write' ? h('span', { className: CLASS.tag }, 'write') : null,
  )
}

/** A collapsible block, closed by default so the page stays scannable. */
function Collapsible({ summary, children, open }) {
  return h(
    'details',
    { className: CLASS.details, open: open === true },
    h('summary', { className: CLASS.summary }, summary),
    h('div', { style: { marginTop: '8px' } }, children),
  )
}

/**
 * A button that performs one planning action and reports its own outcome.
 *
 * The result stays beside the button that caused it rather than in a page-wide
 * banner, because "the task was created" and "the revision was stale" are
 * answers about *that* row. The button disables itself while the request is in
 * flight so a double-click cannot post twice.
 *
 * @param props `{ label, title, run, onDone, disabled }` where `run` performs
 *   the action and `onDone` refreshes whatever the action changed.
 */
function ActionButton({ label, title, run, onDone, disabled }) {
  const [state, setState] = React.useState({ phase: 'idle', message: '' })
  const click = React.useCallback(() => {
    setState({ phase: 'running', message: '' })
    Promise.resolve()
      .then(run)
      .then((result) => {
        setState({ phase: 'done', message: typeof result === 'string' ? result : 'Done.' })
        if (onDone !== undefined) onDone()
      })
      .catch((reason) => {
        setState({ phase: 'error', message: reason instanceof Error ? reason.message : String(reason) })
      })
  }, [run, onDone])

  return h(
    'span',
    { style: { display: 'inline-flex', alignItems: 'center', gap: '6px', flexWrap: 'wrap' } },
    h(
      'button',
      {
        type: 'button',
        className: CLASS.button,
        title,
        disabled: disabled === true || state.phase === 'running',
        onClick: click,
      },
      state.phase === 'running' ? 'Working…' : label,
    ),
    state.phase === 'idle'
      ? null
      : h(
          'span',
          { className: state.phase === 'error' ? CLASS.err : CLASS.ok, style: { fontSize: '11.5px' } },
          state.message,
        ),
  )
}

/** The empty / error / loading states, kept in one place so they read alike. */
function Empty({ children }) {
  return h('div', { className: CLASS.empty }, children)
}

// ---------------------------------------------------------------------------
// the sidebar glyph
// ---------------------------------------------------------------------------

/**
 * The toolbar icon: a small architecture map — two inputs feeding one node,
 * that node feeding two outputs — drawn in the sidebar's own cell.
 * @param props `{ size, active }` supplied by the sidebar.
 */
function CockpitGlyph({ size = 16, active = false }) {
  const stroke = 'currentColor'
  return h(
    'svg',
    {
      width: size,
      height: size,
      viewBox: '0 0 16 16',
      fill: 'none',
      'aria-hidden': 'true',
      style: { display: 'block', opacity: active ? 1 : 0.85 },
    },
    h('path', {
      d: 'M2.2 2.6h2.6M2.2 13.4h2.6M11.2 5.4h2.6M11.2 10.6h2.6',
      stroke,
      strokeWidth: '1.3',
      strokeLinecap: 'round',
    }),
    h('rect', { x: '5.4', y: '5.4', width: '5.2', height: '5.2', rx: '1.3', stroke, strokeWidth: '1.3' }),
    h('path', {
      d: 'M4.8 2.6c1.6 0 1.8 3.6 1.8 4.4M4.8 13.4c1.6 0 1.8-3.6 1.8-4.4M11.2 5.4c-1.6 0-1.8 1.2-1.8 2M11.2 10.6c-1.6 0-1.8-1.2-1.8-2',
      stroke,
      strokeWidth: '1.1',
      strokeLinecap: 'round',
      opacity: '0.75',
    }),
  )
}

// ---------------------------------------------------------------------------
// Architecture
// ---------------------------------------------------------------------------

/** One component in the architecture map. */
function ComponentNode({ component, selected, onSelect }) {
  return h(
    'button',
    {
      type: 'button',
      className: selected ? `${CLASS.node} ${CLASS.nodeActive}` : CLASS.node,
      style: { borderLeftColor: statusColor(component.status) },
      onClick: () => onSelect(component.id),
      title: component.summary === '' ? component.name : component.summary,
    },
    h(
      'span',
      { className: CLASS.nodeTop },
      h('span', { className: CLASS.nodeName }, component.name),
      h(StatusPill, { status: component.status }),
    ),
    h(
      'span',
      { className: CLASS.nodeKind },
      [component.kind, component.language, component.status === 'cut' ? 'cut' : null].filter(Boolean).join(' · '),
    ),
    component.dependsOn.length === 0
      ? h('span', { className: CLASS.nodeDeps }, 'no dependencies')
      : h('span', { className: CLASS.nodeDeps }, `← ${component.dependsOn.join(', ')}`),
    component.missingPaths > 0
      ? h('span', { className: `${CLASS.nodeDeps} ${CLASS.warn}` }, `${component.missingPaths} declared path(s) absent`)
      : null,
  )
}

/** The architecture tab. */
function ArchitectureView({ index, selected, onSelect }) {
  const { segments, total } = statusBreakdown(index.components)
  const groups = groupByLayer(index.components, index.layers)
  const component = index.components.find((candidate) => candidate.id === selected)

  return h(
    'div',
    { className: CLASS.view },
    h(
      'section',
      { className: CLASS.section },
      h('h2', { className: CLASS.sectionTitle }, 'Build state'),
      h(KpiRow, { rows: kpis(index) }),
      h(
        'div',
        { style: { marginTop: '14px' } },
        h(
          'div',
          { className: CLASS.bar, role: 'img', 'aria-label': 'component status distribution' },
          segments.map((segment) =>
            h('div', { key: segment.status, className: CLASS.barSeg, style: { background: segment.color, flexGrow: segment.count } }),
          ),
        ),
        h(
          'div',
          { className: CLASS.legend },
          segments.map((segment) =>
            h(
              'span',
              { key: segment.status, className: CLASS.legendItem },
              h('span', { className: CLASS.swatch, style: { background: segment.color } }),
              `${segment.count} ${segment.label}`,
            ),
          ),
          total === 0 ? h('span', { className: CLASS.muted }, 'no components declared') : null,
        ),
      ),
    ),

    component === undefined
      ? null
      : h(ComponentDetail, { component, onSelect }),

    groups.length === 0
      ? h(Empty, null, 'No components are declared yet. Add them to the manifest and they appear here as a build-order map.')
      : groups.map((group) =>
          h(
            'section',
            { key: group.layer, className: CLASS.layer },
            h(
              'div',
              { className: CLASS.layerHead },
              h('span', { className: CLASS.layerName }, group.layer),
              h('span', { className: CLASS.layerRule }),
              h('span', { className: CLASS.muted }, `${group.components.length}`),
            ),
            h(
              'div',
              { className: CLASS.nodes },
              group.components.map((entry) =>
                h(ComponentNode, { key: entry.id, component: entry, selected: entry.id === selected, onSelect }),
              ),
            ),
          ),
        ),

    index.invariants.length === 0
      ? null
      : h(
          'section',
          { className: CLASS.section },
          h('h2', { className: CLASS.sectionTitle }, 'Invariants'),
          index.invariants.map((invariant) =>
            h(
              'div',
              { key: invariant.id, className: CLASS.inv },
              h('span', { className: CLASS.invId }, invariant.id),
              h(
                'span',
                null,
                invariant.statement,
                invariant.record === '' ? null : h('span', { className: `${CLASS.muted} ${CLASS.mono}` }, ` · ${invariant.record}`),
              ),
            ),
          ),
        ),
  )
}

/** The expanded detail for one selected component. */
function ComponentDetail({ component, onSelect }) {
  const declared = [
    ...component.paths.map((entry) => ({ entry, access: 'own' })),
    ...component.docs.map((entry) => ({ entry, access: 'doc' })),
    ...component.decisions.map((entry) => ({ entry, access: 'adr' })),
  ]
  return h(
    'section',
    { className: CLASS.section },
    h(
      'div',
      { className: CLASS.cardHeader },
      h('h2', { className: CLASS.heading }, component.name),
      h(StatusPill, { status: component.status }),
      h('span', { className: `${CLASS.muted} ${CLASS.mono}` }, component.id),
      h('span', { style: { flex: 1 } }),
      h('button', { type: 'button', className: CLASS.button, onClick: () => onSelect(undefined) }, 'Close'),
    ),
    component.summary === '' ? null : h('p', { className: CLASS.cardMeta }, component.summary),
    h(
      'div',
      { className: CLASS.cols, style: { marginTop: '12px' } },
      h(
        'div',
        { className: CLASS.col },
        h('div', { className: CLASS.headingL2 }, `Declared paths (${declared.length})`),
        declared.length === 0
          ? h('span', { className: CLASS.muted }, 'none declared')
          : h(
              'div',
              { className: CLASS.deps },
              declared.map(({ entry, access }) => h(PathChip, { key: `${access}:${entry.path}`, entry, access: access === 'own' ? undefined : access })),
            ),
      ),
      h(
        'div',
        { className: CLASS.col },
        h('div', { className: CLASS.headingL2 }, 'Relationships'),
        h(
          'div',
          { className: CLASS.list },
          h(
            'div',
            { className: CLASS.listItem },
            h('span', { className: CLASS.muted }, 'depends on'),
            component.dependsOn.length === 0
              ? h('span', { className: CLASS.muted }, '—')
              : component.dependsOn.map((id) => h('span', { key: id, className: CLASS.dep }, id)),
          ),
          h(
            'div',
            { className: CLASS.listItem },
            h('span', { className: CLASS.muted }, 'depended on by'),
            component.dependents.length === 0
              ? h('span', { className: CLASS.muted }, '—')
              : component.dependents.map((id) => h('span', { key: id, className: CLASS.dep }, id)),
          ),
          h('div', { className: CLASS.listItem }, h('span', { className: CLASS.muted }, 'files on disk'), h('span', null, `${component.fileCount}`)),
        ),
        component.notes === '' ? null : h('p', { className: CLASS.cardMeta }, component.notes),
      ),
    ),
  )
}

// ---------------------------------------------------------------------------
// Agents
// ---------------------------------------------------------------------------

/** One agent card: its job, its files, and whether it exists yet. */
function AgentCard({ row, expanded, onToggle, actions }) {
  const color = row.liveStatus === 'not-created' ? 'var(--pcx-idle)' : statusColor(row.liveStatus)
  return h(
    'div',
    { className: CLASS.card, style: { borderLeft: `3px solid ${color}` } },
    h(
      'button',
      { type: 'button', className: CLASS.agentRow, onClick: () => onToggle(row.id), 'aria-expanded': expanded },
      h('span', { className: CLASS.emoji }, row.emoji === '' ? '🤖' : row.emoji),
      h(
        'span',
        { style: { flex: 1, minWidth: 0 } },
        h('span', { className: CLASS.agentName }, row.id),
        h('span', { className: CLASS.agentRole }, ` · ${row.role}`),
        h('div', { className: CLASS.cardMeta }, row.purpose === '' ? 'no purpose declared' : row.purpose),
      ),
      h(StatusPill, { status: row.liveStatus, label: row.liveStatus === 'not-created' ? 'not created' : statusLabel(row.liveStatus) }),
    ),

    h(
      'div',
      { style: { marginTop: '9px' } },
      h('div', { className: CLASS.headingL2 }, 'Task'),
      h(
        'div',
        { className: CLASS.taskBody },
        row.task === undefined
          ? row.task === undefined && row.notes === ''
            ? h('span', { className: CLASS.muted }, 'No task declared for this agent.')
            : row.notes
          : h(
              'span',
              null,
              h('strong', null, row.task.subject),
              ' ',
              h(StatusPill, { status: row.task.status }),
              row.task.blocked ? h('span', { className: CLASS.warn }, ' · blocked') : null,
            ),
      ),
    ),

    h(
      'div',
      { style: { marginTop: '9px' } },
      h('div', { className: CLASS.headingL2 }, `Files provided (${row.providedFiles.length})`),
      row.providedFiles.length === 0
        ? h('span', { className: CLASS.muted }, 'This agent is handed no files.')
        : h(
            'div',
            { className: CLASS.deps },
            (expanded ? row.providedFiles : row.providedFiles.slice(0, 6)).map((entry) =>
              h(PathChip, { key: `${entry.access}:${entry.path}`, entry, access: entry.access }),
            ),
            !expanded && row.providedFiles.length > 6
              ? h('span', { className: CLASS.muted }, `+${row.providedFiles.length - 6} more`)
              : null,
          ),
      row.missingProvided > 0 ? h('div', { className: `${CLASS.warn} ${CLASS.mono}`, style: { marginTop: '6px' } }, `${row.missingProvided} declared path(s) are not on disk`) : null,
    ),

    actions === undefined || actions.sendTask === undefined
      ? null
      : h('div', { style: { marginTop: '9px' } }, actions.sendTask(row)),
  )
}

/** The agents tab: the planned roster against the live team. */
function AgentsView({ index, team, expanded, onToggle, actions }) {
  const merged = mergeAgents(index, team)
  return h(
    'div',
    { className: CLASS.view },
    h(
      'section',
      { className: CLASS.section },
      h('h2', { className: CLASS.sectionTitle }, 'Roster'),
      h(KpiRow, {
        rows: [
          { label: 'planned agents', value: `${merged.declared}` },
          { label: 'live teammates', value: `${merged.live}` },
          { label: 'active now', value: `${merged.active}`, hint: team === undefined ? 'no session' : undefined },
          { label: 'inactive', value: `${merged.idle}` },
          { label: 'not created yet', value: `${merged.notCreated}` },
          ...(merged.failed > 0 ? [{ label: 'failed', value: `${merged.failed}` }] : []),
        ],
      }),
      team === undefined
        ? h('p', { className: CLASS.cardMeta }, 'No Agent Team is open in this session, so live status is unknown. The roster below is the declared plan.')
        : null,
      merged.unplanned.length === 0
        ? null
        : h(
            'div',
            { style: { marginTop: '12px' } },
            h('div', { className: CLASS.headingL2 }, 'Live members the manifest does not declare'),
            h(
              'div',
              { className: CLASS.list },
              merged.unplanned.map((member) =>
                h(
                  'div',
                  { key: member.id, className: CLASS.listItem },
                  h(StatusPill, { status: member.phase }),
                  h('span', { className: CLASS.mono }, member.name),
                  h('span', { className: CLASS.muted }, member.error ?? ''),
                ),
              ),
            ),
          ),
    ),

    index.agents.length === 0
      ? h(Empty, null, 'No agents are declared. Add an "agents" array to the manifest to plan the roster — each agent pairs a task with the exact files it will be handed.')
      : h(
          'div',
          { className: CLASS.grid },
          index.agents.map((agent) => {
            const row = merged.rows.find((candidate) => candidate.id === agent.id) ?? agent
            return h(AgentCard, { key: agent.id, row, expanded: expanded.has(agent.id), onToggle, actions })
          }),
        ),
  )
}

// ---------------------------------------------------------------------------
// Tasks
// ---------------------------------------------------------------------------

/**
 * The board action for one planned task.
 *
 * Three outcomes, and the difference between them matters to the reader:
 * there is no board to add to; the task is already on it (shown with its live
 * revision, because the revision is what a later change must quote); or it can
 * be created now. Matching is by subject text — the only join available between
 * the manifest's ids and the board's — so an unmatched task is offered rather
 * than assumed absent.
 *
 * @param props `{ task, board, sessionId, onDone }`.
 */
function TaskBoardAction({ task, board, sessionId, supported, onDone }) {
  if (supported === false) {
    return h(
      'div',
      { className: `${CLASS.warn} ${CLASS.mono}`, style: { marginTop: '4px' } },
      'Creating tasks needs the host half of the cockpit to be restarted.',
    )
  }
  if (sessionId === undefined || sessionId === '') {
    return h('div', { className: `${CLASS.muted} ${CLASS.mono}`, style: { marginTop: '4px' } }, 'No open session, so no board to plan onto.')
  }
  if (board === null) {
    return h('div', { className: `${CLASS.muted} ${CLASS.mono}`, style: { marginTop: '4px' } }, 'No live board in this session.')
  }
  const norm = (value) => String(value ?? '').trim().toLowerCase().replace(/\s+/gu, ' ')
  const existing = board.tasks.find((entry) => norm(entry.subject) === norm(task.subject))
  if (existing !== undefined) {
    return h(
      'div',
      { className: `${CLASS.ok} ${CLASS.mono}`, style: { marginTop: '4px' } },
      `On the board as ${existing.id} · revision ${existing.revision} · ${existing.status}`,
    )
  }
  return h(
    'div',
    { style: { marginTop: '4px' } },
    h(ActionButton, {
      label: 'Add to team board',
      title: 'Create this task on the shared team task board',
      run: async () => {
        const result = await postAction(
          'create_task',
          {
            subject: task.subject,
            description: task.description === '' ? task.subject : task.description,
            writeScopes: task.writeScopes,
          },
          sessionId,
        )
        return `Created ${result.task?.id ?? 'the task'} on the board.`
      },
      onDone,
    }),
  )
}

/** The tasks tab: the written plan against the live board. */
function TasksView({ index, team, board, boardError, actions }) {
  const merged = mergeTasks(index, team)
  const byStatus = (status) => index.tasks.filter((task) => task.status === status)
  const groups = [
    { status: 'in_progress', rows: byStatus('in_progress') },
    { status: 'pending', rows: byStatus('pending') },
    { status: 'blocked', rows: byStatus('blocked') },
    { status: 'completed', rows: byStatus('completed') },
  ].filter((group) => group.rows.length > 0)

  return h(
    'div',
    { className: CLASS.view },
    h(
      'section',
      { className: CLASS.section },
      h('h2', { className: CLASS.sectionTitle }, 'Live board vs written plan'),
      h(KpiRow, {
        rows: [
          { label: 'planned tasks', value: `${index.tasks.length}` },
          { label: 'on the live board', value: `${merged.liveTasks.length}`, hint: team === undefined ? 'no session' : undefined },
          { label: 'matched by subject', value: `${merged.matched.length}` },
          { label: 'planned, not on board', value: `${merged.planOnly.length}` },
          { label: 'on board, not planned', value: `${merged.liveOnly.length}` },
        ],
      }),
      boardError === null
        ? null
        : h('div', { className: CLASS.warnBox, style: { marginTop: '10px' } }, `The shared board could not be read: ${boardError}`),
      h(
        'p',
        { className: CLASS.cardMeta },
        board === null
          ? 'No live team board is available in this session, so nothing can be added to it from here.'
          : `Board reachable · ${board.tasks.length} task(s) · ${board.members.length} member(s). A task is added with its compare-and-set revision, so two writers cannot overwrite each other.`,
      ),
      merged.liveOnly.length === 0
        ? null
        : h(
            'div',
            { style: { marginTop: '12px' } },
            h('div', { className: CLASS.headingL2 }, 'Live tasks with no written counterpart'),
            h(
              'div',
              { className: CLASS.list },
              merged.liveOnly.map((task) =>
                h(
                  'div',
                  { key: task.id, className: CLASS.listItem },
                  h(StatusPill, { status: task.status }),
                  h('span', { className: CLASS.mono }, task.id),
                  h('span', { className: CLASS.cell }, task.subject),
                  task.ownerName === undefined ? null : h('span', { className: CLASS.muted }, task.ownerName),
                ),
              ),
            ),
          ),
    ),

    groups.length === 0
      ? h(Empty, null, 'No tasks are declared. Tasks give the cockpit its build order — add them with dependencies and write scopes.')
      : groups.map((group) =>
          h(
            'section',
            { key: group.status, className: CLASS.layer },
            h(
              'div',
              { className: CLASS.layerHead },
              h('span', { className: CLASS.layerName }, statusLabel(group.status)),
              h('span', { className: CLASS.layerRule }),
              h('span', { className: CLASS.muted }, `${group.rows.length}`),
            ),
            h(
              'div',
              { className: CLASS.grid },
              group.rows.map((task) =>
                h(
                  'div',
                  { key: task.id, className: CLASS.taskCard, style: { borderLeftColor: statusColor(task.status) } },
                  h(
                    'div',
                    { className: CLASS.taskHead },
                    h('span', { className: `${CLASS.mono}` }, task.id),
                    h('strong', { style: { flex: 1, minWidth: 0 } }, task.subject),
                    task.blocked ? h('span', { className: CLASS.badge }, h('span', { className: CLASS.dot, style: { background: 'var(--pcx-err)' } }), 'blocked') : null,
                  ),
                  task.description === '' ? null : h('div', { className: CLASS.taskBody }, task.description),
                  h(
                    'div',
                    { className: CLASS.listItem },
                    h('span', { className: CLASS.muted }, 'owner'),
                    task.owners.length === 0 ? h('span', { className: CLASS.muted }, 'unassigned') : task.owners.map((id) => h('span', { key: id, className: CLASS.tag }, id)),
                  ),
                  task.blockers.length > 0
                    ? h(
                        'div',
                        { className: CLASS.listItem },
                        h('span', { className: CLASS.muted }, 'waiting on'),
                        task.blockers.map((blocker) => h('span', { key: blocker.id, className: CLASS.dep }, `${blocker.id} ${blocker.status}`)),
                      )
                    : null,
                  task.writeScopes.length === 0
                    ? null
                    : h(
                        'div',
                        { className: CLASS.deps },
                        task.scopeStatus.map((entry) => h(PathChip, { key: entry.path, entry, access: 'write' })),
                      ),
                  h(TaskBoardAction, { task, board, sessionId: actions === undefined ? undefined : actions.sessionId, supported: actions === undefined ? undefined : actions.supported, onDone: actions === undefined ? undefined : actions.onDone }),
                ),
              ),
            ),
          ),
        ),
  )
}

// ---------------------------------------------------------------------------
// Files
// ---------------------------------------------------------------------------

/** One directory level of the workspace tree. */
function TreeNode({ node, depth, ownership, expanded, onToggle }) {
  if (!node.dir) {
    const agents = ownership.byFile.get(node.path)
    return h(
      'div',
      { className: CLASS.treeRow, style: { paddingLeft: `${6 + depth * 14}px` } },
      h('span', { className: CLASS.treeName, title: node.path }, node.name),
      agents === undefined ? null : [...agents].map((id) => h('span', { key: id, className: CLASS.tag }, id)),
      h('span', { className: CLASS.treeMeta }, formatBytes(node.size)),
      h('span', { className: CLASS.fileType, style: { display: 'none' } }),
    )
  }
  const open = expanded.has(node.path)
  return h(
    'div',
    null,
    h(
      'button',
      {
        type: 'button',
        className: CLASS.treeRow,
        style: { paddingLeft: `${6 + depth * 14}px`, border: 0, background: 'transparent', width: '100%', cursor: 'pointer', color: 'inherit', textAlign: 'left' },
        onClick: () => onToggle(node.path),
      },
      h('span', { className: CLASS.treeMeta }, open ? '▾' : '▸'),
      h('span', { className: CLASS.treeName }, node.name),
      h('span', { className: CLASS.treeMeta }, `${node.children.length}`),
    ),
    open ? node.children.map((child) => h(TreeNode, { key: child.path, node: child, depth: depth + 1, ownership, expanded, onToggle })) : null,
  )
}

/** The files tab: the real tree, the agent↔file join, and the unclaimed set. */
function FilesView({ index, expandedTree, onToggleTree, focusAgent }) {
  const ownership = { byFile: new Map() }
  for (const file of index.files) ownership.byFile.set(file.path, new Set(file.agents))
  const tree = buildFileTree(index.files, { maxDepth: 5 })
  const agents = focusAgent === undefined || focusAgent === '' ? index.agents : index.agents.filter((agent) => agent.id === focusAgent)
  const agentPaths = new Set()
  for (const agent of agents) for (const entry of agent.providedFiles) agentPaths.add(entry.path)
  const providedFiles = index.files.filter((file) => agentPaths.has(file.path) || [...(ownership.byFile.get(file.path) ?? [])].some((id) => agents.some((agent) => agent.id === id)))

  return h(
    'div',
    { className: CLASS.view },
    h(
      'section',
      { className: CLASS.section },
      h('h2', { className: CLASS.sectionTitle }, 'Which files each agent is handed'),
      h(
        'div',
        { className: CLASS.table },
        h(
          'div',
          { className: `${CLASS.row} ${CLASS.muted}` },
          h('span', { className: CLASS.cell, style: { width: '120px', flex: 'none' } }, 'agent'),
          h('span', { className: CLASS.cell, style: { width: '90px', flex: 'none' } }, 'state'),
          h('span', { className: CLASS.cell, style: { width: '70px', flex: 'none' } }, 'read'),
          h('span', { className: CLASS.cell, style: { width: '70px', flex: 'none' } }, 'write'),
          h('span', { className: CLASS.cell }, 'task'),
        ),
        index.agents.length === 0
          ? h('div', { className: CLASS.muted }, 'No agents declared.')
          : index.agents.map((agent) =>
              h(
                'div',
                { key: agent.id, className: CLASS.row },
                h('span', { className: `${CLASS.cell} ${CLASS.mono}` }, agent.id),
                h('span', { className: CLASS.cell, style: { flex: 'none' } }, h(StatusPill, { status: 'proposed' })),
                h('span', { className: CLASS.cell, style: { flex: 'none' } }, `${agent.readPaths.length}`),
                h('span', { className: CLASS.cell, style: { flex: 'none' } }, `${agent.writeScopes.length}`),
                h('span', { className: `${CLASS.cell} ${CLASS.muted}` }, agent.task?.subject ?? '—'),
              ),
            ),
      ),
      h(
        'p',
        { className: CLASS.cardMeta },
        'Read paths are handed to the agent as context; write scopes are what it may modify. A path shown in red is declared but not on disk yet.',
      ),
    ),

    h(
      'section',
      { className: CLASS.section },
      h('h2', { className: CLASS.sectionTitle }, `Files claimed by an agent (${providedFiles.length})`),
      providedFiles.length === 0
        ? h(Empty, null, 'No file on disk is currently claimed by a declared agent.')
        : h(
            'div',
            { className: CLASS.table },
            providedFiles.map((file) =>
              h(
                'div',
                { key: file.path, className: CLASS.row },
                h('span', { className: `${CLASS.cell} ${CLASS.mono}`, style: { flex: 1 } }, file.path),
                h(
                  'span',
                  { className: CLASS.cell, style: { flex: 'none' } },
                  file.agents.map((id) => h('span', { key: id, className: CLASS.tag, style: { marginLeft: '4px' } }, id)),
                ),
                h(
                  'span',
                  { className: `${CLASS.cell} ${CLASS.muted}`, style: { flex: 'none' } },
                  file.components.length === 0 ? '' : `component: ${file.components.join(', ')}`,
                ),
              ),
            ),
          ),
    ),

    h(
      'section',
      { className: CLASS.section },
      h('h2', { className: CLASS.sectionTitle }, `Workspace tree (${index.stats.files} files)`),
      tree.length === 0
        ? h(Empty, null, 'The workspace scan returned no files.')
        : h(
            'div',
            { className: CLASS.tree },
            tree.map((node) => h(TreeNode, { key: node.path, node, depth: 0, ownership, expanded: expandedTree, onToggle: onToggleTree })),
          ),
      index.unownedFiles.length === 0
        ? null
        : h(
            Collapsible,
            { summary: `${index.unownedFiles.length} file(s) belong to no declared component or agent` },
            h(
              'div',
              { className: CLASS.table },
              index.unownedFiles.map((file) => h('div', { key: file, className: `${CLASS.row} ${CLASS.mono} ${CLASS.muted}` }, file)),
            ),
          ),
    ),
  )
}

// ---------------------------------------------------------------------------
// Verify
// ---------------------------------------------------------------------------

/** The verify tab: what is wrong, and how to prove the project still works. */
function VerifyView({ index }) {
  const rows = warnings(index, undefined)
  const manifest = index.manifestPath
  return h(
    'div',
    { className: CLASS.view },
    h(
      'section',
      { className: CLASS.section },
      h('h2', { className: CLASS.sectionTitle }, `Findings (${rows.length})`),
      rows.length === 0
        ? h('div', { className: `${CLASS.ok}` }, 'Nothing is inconsistent: every declared path resolves, no write scopes overlap, and no task is blocked while in progress.')
        : rows.map((row, index2) =>
            h(
              'div',
              { key: index2, className: CLASS.warnBox, style: { borderColor: row.level === 'err' ? 'var(--pcx-err)' : 'var(--pcx-warn)', marginBottom: '8px' } },
              row.text,
            ),
          ),
    ),

    h(
      'section',
      { className: CLASS.section },
      h('h2', { className: CLASS.sectionTitle }, 'Commands'),
      h(
        'div',
        { className: CLASS.list },
        h(
          'div',
          { className: CLASS.listItem },
          h('span', { className: CLASS.muted, style: { minWidth: '90px' } }, 'verify'),
          h('code', { className: CLASS.mono }, index.project.testCommand === '' ? '(none declared — set project.testCommand)' : index.project.testCommand),
        ),
        h(
          'div',
          { className: CLASS.listItem },
          h('span', { className: CLASS.muted, style: { minWidth: '90px' } }, 'run'),
          h('code', { className: CLASS.mono }, index.project.runCommand === '' ? '(none declared — set project.runCommand)' : index.project.runCommand),
        ),
        h(
          'div',
          { className: CLASS.listItem },
          h('span', { className: CLASS.muted, style: { minWidth: '90px' } }, 'manifest'),
          h('code', { className: CLASS.mono }, manifest),
        ),
      ),
    ),

    h(
      'section',
      { className: CLASS.section },
      h('h2', { className: CLASS.sectionTitle }, 'Coverage'),
      h(KpiRow, {
        rows: [
          { label: 'files on disk', value: `${index.stats.files}` },
          { label: 'declared paths', value: `${index.stats.declaredPaths}` },
          { label: 'declared and present', value: `${index.stats.declaredPaths - index.stats.missingPaths}` },
          { label: 'declared but absent', value: `${index.stats.missingPaths}` },
          { label: 'unclaimed files', value: `${index.stats.unownedFiles}` },
        ],
      }),
      h('p', { className: CLASS.cardMeta }, 'A declared-but-absent path is work the architecture promises and the tree does not have yet. A high unclaimed count means the manifest has drifted behind the code.'),
    ),

    index.components.length === 0
      ? null
      : h(
          'section',
          { className: CLASS.section },
          h('h2', { className: CLASS.sectionTitle }, 'Per-component resolution'),
          h(
            'div',
            { className: CLASS.table },
            index.components.map((component) =>
              h(
                'div',
                { key: component.id, className: CLASS.row },
                h('span', { className: `${CLASS.cell} ${CLASS.mono}`, style: { width: '170px', flex: 'none' } }, component.id),
                h('span', { className: CLASS.cell, style: { flex: 'none' } }, h(StatusPill, { status: component.status })),
                h('span', { className: `${CLASS.cell} ${CLASS.present}`, style: { flex: 'none' } }, `${component.presentPaths} present`),
                h('span', { className: `${CLASS.cell} ${component.missingPaths > 0 ? CLASS.missing : CLASS.muted}`, style: { flex: 'none' } }, `${component.missingPaths} absent`),
                h('span', { className: `${CLASS.cell} ${CLASS.muted}` }, `${component.fileCount} file(s)`),
              ),
            ),
          ),
        ),
  )
}

// ---------------------------------------------------------------------------
// the panel
// ---------------------------------------------------------------------------

const TABS = [
  { id: 'architecture', label: 'Architecture' },
  { id: 'agents', label: 'Agents' },
  { id: 'tasks', label: 'Tasks' },
  { id: 'files', label: 'Files → Agents' },
  { id: 'verify', label: 'Verify' },
]

/** The main cockpit panel. */
function CockpitPanel({ sessions }) {
  const { index, team, board, boardError, capabilities, loading, error, reload, sessionId, resolvedRoot, rootSource } = useCockpit(sessions)
  const [tab, setTab] = React.useState('architecture')
  const [selected, setSelected] = React.useState(undefined)
  const [expandedAgents, setExpandedAgents] = React.useState(() => new Set())
  const [expandedTree, setExpandedTree] = React.useState(() => new Set(['docs', 'contracts', 'db', '.cockpit']))

  // The live board is the authority on which tasks already exist. A manifest
  // task whose subject is already on the board is not offered again, because
  // creating it twice is how a board fills with duplicates.
  //
  // `contract` is the host half's revision. An older host has no POST route at
  // all, so the panel offers the action only when the host says it exists —
  // and says which half needs restarting when it does not.
  const actionsSupported = capabilities !== null && capabilities.contract >= 2
  const actions = {
    sessionId,
    supported: actionsSupported,
    onDone: reload,
    /** Hand this agent its task, with the files it is to work from. */
    sendTask: (agent) =>
      h(ActionButton, {
        key: agent.id,
        label: 'Send task to member',
        title: 'Message this teammate its task and the files it is handed',
        disabled: agent.liveStatus === 'not-created' || agent.liveStatus === 'failed',
        run: async () => {
          const sections = [`${agent.emoji === '' ? '' : `${agent.emoji} `}${agent.id} — ${agent.role}`]
          if (agent.purpose !== '') sections.push(agent.purpose)
          sections.push(agent.task === undefined ? agent.task || 'Your task is not yet declared in the manifest.' : `Task: ${agent.task.subject}`)
          if (agent.notes !== '') sections.push(agent.notes)
          if (agent.components.length > 0) sections.push(`Components you own: ${agent.components.map((component) => component.id).join(', ')}`)
          const read = agent.readPaths.filter((entry) => entry.status !== 'missing')
          const write = agent.writeScopes.filter((entry) => entry.status !== 'missing')
          if (read.length > 0) sections.push(['Files you are handed as context:', ...read.map((entry) => `  ${entry.path}`)].join('\n'))
          if (write.length > 0) sections.push([`Write scopes — yours alone: ${write.map((entry) => entry.path).join(', ')}`].join('\n'))
          const missing = agent.providedFiles.filter((entry) => entry.status === 'missing')
          if (missing.length > 0) sections.push(['Declared but not yet on disk:', ...missing.map((entry) => `  ${entry.path}`)].join('\n'))
          const result = await postAction('send_message', { target: agent.id, message: sections.join('\n\n') }, sessionId)
          const delivery = result.delivery
          const state = delivery !== null && typeof delivery === 'object' ? delivery.delivered ?? delivery.status ?? 'sent' : 'sent'
          return `Message ${state}.`
        },
        onDone: reload,
      }),
  }

  const toggleAgent = React.useCallback((id) => {
    setExpandedAgents((previous) => {
      const next = new Set(previous)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }, [])
  const toggleTree = React.useCallback((path) => {
    setExpandedTree((previous) => {
      const next = new Set(previous)
      if (next.has(path)) next.delete(path)
      else next.add(path)
      return next
    })
  }, [])

  if (error !== null && index === null) {
    return h(
      'div',
      { className: CLASS.root },
      h('div', { className: CLASS.header }, h('h1', { className: CLASS.title }, 'Project Cockpit')),
      h(
        'div',
        { className: CLASS.body },
        h('div', { className: CLASS.warnBox, style: { borderColor: 'var(--pcx-err)' } }, h('strong', null, 'The cockpit could not read the project. '), error),
        h('p', { className: CLASS.cardMeta }, 'The host half serves this panel at /api/project-cockpit. If it was disabled in the profile, the panel has nothing to read.'),
        h('button', { type: 'button', className: `${CLASS.button} ${CLASS.buttonPrimary}`, onClick: reload }, 'Retry'),
      ),
    )
  }

  if (index === null) {
    return h(
      'div',
      { className: CLASS.root },
      h('div', { className: CLASS.header }, h('h1', { className: CLASS.title }, 'Project Cockpit')),
      h('div', { className: CLASS.body }, h(Empty, null, loading ? 'Reading the project…' : 'No data.')),
    )
  }

  const counts = {
    architecture: index.components.length,
    agents: index.agents.length,
    tasks: index.tasks.length,
    files: index.stats.files,
    verify: warnings(index, team).length,
  }

  return h(
    'div',
    { className: CLASS.root },
    h(
      'div',
      { className: CLASS.header },
      h(
        'h1',
        { className: CLASS.title },
        index.project.name === '' ? 'Untitled project' : index.project.name,
        h(StatusPill, { status: index.project.stage === 'shipped' ? 'done' : index.project.stage === 'design' ? 'proposed' : 'in-progress', label: index.project.stage }),
      ),
      index.project.summary === '' ? null : h('p', { className: CLASS.subtitle }, index.project.summary),
      h(
        'div',
        { className: CLASS.chipRow },
        h('span', { className: CLASS.chip }, h('span', { className: `${CLASS.chipDot}`, style: { background: 'var(--pcx-brand)' } }), `${index.stats.components} components`),
        h('span', { className: CLASS.chip }, h('span', { className: `${CLASS.chipDot}`, style: { background: 'var(--pcx-ok)' } }), `${index.agents.length} agents planned`),
        h(
          'span',
          { className: CLASS.chip },
          h('span', { className: `${CLASS.chipDot}`, style: { background: team === undefined ? 'var(--pcx-idle)' : 'var(--pcx-ok)' } }),
          team === undefined ? 'no live team in this session' : `${(team.members ?? []).length - 1} teammate(s) live`,
        ),
        h('span', { className: CLASS.chip }, `${index.stats.files} files`),
        index.stats.missingPaths > 0
          ? h('span', { className: CLASS.chip, style: { borderColor: 'var(--pcx-warn)' } }, h('span', { className: `${CLASS.chipDot}`, style: { background: 'var(--pcx-warn)' } }), `${index.stats.missingPaths} declared path(s) absent`)
          : null,
        index.collisions.length > 0
          ? h('span', { className: CLASS.chip, style: { borderColor: 'var(--pcx-err)' } }, h('span', { className: `${CLASS.chipDot}`, style: { background: 'var(--pcx-err)' } }), `${index.collisions.length} write-scope overlap(s)`)
          : null,
      ),
      h(
        'div',
        { className: CLASS.actions },
        h('button', { type: 'button', className: `${CLASS.button} ${CLASS.buttonPrimary}`, onClick: reload, disabled: loading }, loading ? 'Refreshing…' : 'Refresh'),
        h('span', { className: `${CLASS.muted} ${CLASS.mono}` }, `${index.root}${sessionId === undefined ? '' : ` · session ${sessionId.slice(0, 8)}`}`),
      ),
      h(
        'div',
        { className: CLASS.tabs, role: 'tablist' },
        TABS.map((entry) =>
          h(
            'button',
            {
              key: entry.id,
              type: 'button',
              role: 'tab',
              'aria-selected': tab === entry.id,
              className: tab === entry.id ? `${CLASS.tab} ${CLASS.tabActive}` : CLASS.tab,
              onClick: () => setTab(entry.id),
            },
            entry.label,
            h('span', { className: CLASS.tabCount }, `${counts[entry.id] ?? 0}`),
          ),
        ),
      ),
    ),
    h(
      'div',
      { className: CLASS.body },
      tab === 'architecture' ? h(ArchitectureView, { index, selected, onSelect: setSelected }) : null,
      tab === 'agents' ? h(AgentsView, { index, team, expanded: expandedAgents, onToggle: toggleAgent, actions }) : null,
      tab === 'tasks' ? h(TasksView, { index, team, board, boardError, actions }) : null,
      tab === 'files' ? h(FilesView, { index, expandedTree, onToggleTree: toggleTree }) : null,
      tab === 'verify' ? h(VerifyView, { index }) : null,
    ),
    h(
      'div',
      { className: CLASS.footer },
      // Which project this is, and why. The cockpit reads any project, so the
      // directory it resolved is part of the answer, not a detail.
      h('span', { className: CLASS.mono, title: rootSource === null ? undefined : `chosen by: ${rootSource}` }, resolvedRoot ?? index.root),
      rootSource === null ? null : h('span', { className: CLASS.muted }, rootSource),
      h('span', { className: CLASS.mono }, index.manifestPath),
      h('span', null, `read ${new Date(index.generatedAt).toLocaleTimeString()}`),
      index.truncated ? h('span', { className: CLASS.warn }, 'scan truncated') : null,
      h('span', { className: CLASS.muted }, 'read-only'),
    ),
  )
}

// ---------------------------------------------------------------------------
// registration
// ---------------------------------------------------------------------------

/**
 * Client plugin entry.
 * @param ctx client context carrying `slots` and `sessions`.
 */
export function apply(ctx) {
  installStyles()
  const sessions = ctx.sessions

  // The toolbar glyph. Its own button chrome, label and accessibility belong to
  // the sidebar; a panellist entry supplies the icon and nothing else.
  ctx.slots.inject('sidebar.panellist', () =>
    ctx.slots.register(
      {
        name: 'sidebar.panellist',
        id: PANEL_ID,
        order: 40,
        label: 'Project Cockpit',
      },
      (ownerProps) => h(CockpitGlyph, { size: ownerProps?.size ?? 16, active: ownerProps?.active === true }),
    ),
  )

  // The page itself, keyed by the same id the sidebar button selects.
  ctx.slots.inject('main', () =>
    ctx.slots.register(
      {
        name: 'main',
        key: PANEL_ID,
        inject: () => ({ sessions }),
      },
      () => h(CockpitPanel, { sessions }),
    ),
  )
}
