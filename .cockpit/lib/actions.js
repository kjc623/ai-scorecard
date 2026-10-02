/**
 * Project Cockpit — the planning actions.
 *
 * The cockpit reads in one direction and writes in exactly one narrow, guarded
 * direction: it can put work on the shared team task board and hand that work to
 * a member. Nothing here can edit a file, spawn a teammate, or change policy —
 * those stay with the agent tools, where the model decides with context the
 * cockpit does not have.
 *
 * Every action is authorised the same way the agent tools are: an action needs
 * an exact live Lead agent, resolved from the registry, and the team service
 * applies its own compare-and-set and authorisation on top. If no Lead is
 * available, the action refuses and says so — it never guesses an identity.
 */

/** Errors the panel is allowed to see, with the status it should render. */
export class CockpitActionError extends Error {
  /**
   * @param message what went wrong, in the reader's terms.
   * @param status the HTTP status to answer with.
   */
  constructor(message, status) {
    super(message)
    this.name = 'CockpitActionError'
    this.status = status
  }
}

/**
 * Normalize a caller-supplied string list.
 * @param value candidate array.
 * @returns trimmed, de-duplicated, non-empty entries.
 */
function stringList(value) {
  if (!Array.isArray(value)) return []
  const out = []
  for (const entry of value) {
    const text = typeof entry === 'string' ? entry.trim() : ''
    if (text !== '' && !out.includes(text)) out.push(text)
  }
  return out
}

/**
 * Resolve the Lead agent an action should be attributed to.
 *
 * The cockpit is mounted once for the whole shell but the team belongs to one
 * session, so the caller names the session (the panel already knows it) and this
 * resolves *that* agent. Falling back to "any root agent" would attribute one
 * session's task to another, which is exactly the kind of silent wrong the
 * cockpit is supposed to prevent.
 *
 * @param agents the agent registry, or undefined when it is unavailable.
 * @param sessionId the session the panel is showing.
 * @returns the live agent.
 * @throws CockpitActionError when there is no such live agent.
 */
export function resolveLeadAgent(agents, sessionId) {
  if (agents === undefined || agents === null) {
    throw new CockpitActionError('The agent registry is not available in this composition, so the cockpit cannot act on the team.', 501)
  }
  if (typeof sessionId !== 'string' || sessionId === '') {
    throw new CockpitActionError('No session was named, so the cockpit cannot tell whose team to change.', 400)
  }
  const agent = agents.get(sessionId)
  if (agent === undefined) {
    throw new CockpitActionError(`Session ${sessionId} has no live agent, so its team cannot be changed from here.`, 409)
  }
  return agent
}

/**
 * Perform one planning action.
 *
 * @param action the action name.
 * @param payload its arguments (untrusted).
 * @param context `{ agentTeams, agents, sessionId }`.
 * @returns a JSON-serializable result describing what changed.
 */
export async function performAction(action, payload, context) {
  const { agentTeams, agents, sessionId } = context
  if (agentTeams === undefined || agentTeams === null) {
    throw new CockpitActionError('The Agent Teams service is not available in this composition, so there is no shared task board to change.', 501)
  }
  const lead = resolveLeadAgent(agents, sessionId)
  const body = payload !== null && typeof payload === 'object' ? payload : {}

  switch (action) {
    case 'create_task': {
      const subject = typeof body.subject === 'string' ? body.subject.trim() : ''
      const description = typeof body.description === 'string' ? body.description.trim() : ''
      if (subject === '') throw new CockpitActionError('A task needs a subject.', 400)
      const task = await agentTeams.createTask(lead, {
        subject,
        description,
        blockedBy: stringList(body.blockedBy ?? body.blocked_by),
        writeScopes: stringList(body.writeScopes ?? body.write_scopes),
      })
      return { action, task }
    }
    case 'update_task': {
      const taskId = typeof body.taskId === 'string' ? body.taskId.trim() : ''
      const expectedRevision = Number(body.expectedRevision)
      const transition = typeof body.transition === 'string' ? body.transition : ''
      if (taskId === '') throw new CockpitActionError('A task id is required.', 400)
      if (!Number.isSafeInteger(expectedRevision) || expectedRevision < 1) {
        throw new CockpitActionError('A task change needs the current revision, so two writers cannot overwrite each other.', 400)
      }
      const allowed = new Set(['claim', 'release', 'complete', 'reopen', 'reassign'])
      if (!allowed.has(transition)) {
        throw new CockpitActionError(`The cockpit can apply ${[...allowed].join(', ')} — not ${JSON.stringify(transition)}.`, 400)
      }
      const request = { taskId, expectedRevision, action: transition }
      if (transition === 'reassign') {
        const owner = typeof body.owner === 'string' ? body.owner.trim() : ''
        // An omitted owner unassigns, which is how the team service reads it.
        request.owner = owner === '' ? undefined : owner
      }
      const task = await agentTeams.updateTask(lead, request)
      return { action, task }
    }
    case 'send_message': {
      const target = typeof body.target === 'string' ? body.target.trim() : ''
      const text = typeof body.message === 'string' ? body.message.trim() : ''
      if (target === '') throw new CockpitActionError('A message needs a target member.', 400)
      if (text === '') throw new CockpitActionError('A message needs content.', 400)
      const delivery = await agentTeams.sendMessage(lead, {
        target,
        content: [{ type: 'text', text }],
      })
      return { action, target, delivery }
    }
    default:
      throw new CockpitActionError(`Unknown cockpit action ${JSON.stringify(action)}.`, 400)
  }
}

/**
 * Read the team's live roster and task board for one session.
 *
 * This is the same data the `agentTeam` session projection carries to the
 * panel, asked for directly — which is what makes it usable as a check that the
 * panel and the service agree.
 *
 * @param context `{ agentTeams, agents, sessionId }`.
 * @returns `{ members, tasks }`.
 */
export function readTeam(context) {
  const { agentTeams, agents, sessionId } = context
  if (agentTeams === undefined || agentTeams === null) {
    throw new CockpitActionError('The Agent Teams service is not available in this composition.', 501)
  }
  const lead = resolveLeadAgent(agents, sessionId)
  return { members: agentTeams.listMembers(lead), tasks: agentTeams.listTasks(lead) }
}

/** The action names the route accepts, so the panel and route cannot drift. */
export const ACTION_NAMES = ['create_task', 'update_task', 'send_message', 'read_team']
