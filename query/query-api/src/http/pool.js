// pool.js — connections, and the one rule that makes them safe to reuse.
//
// TWO DEFECTS THIS EXISTS TO FIX, both found by the agent who wrote the driver and both real:
//
//   1. A shared client cannot serve concurrent requests. The first version of createHandler() was
//      handed ONE client and ran `SELECT 1` for readiness and a BEGIN..COMMIT transaction for each
//      read on it. Two requests — or one request and one probe — therefore interleave two
//      transactions on one session, and the second gets SAC_BUSY. A connection is a session, and a
//      session has one transaction at a time. §12.3 already says what to do about it: a pool, 8
//      concurrent statements per tenant and a global ceiling of ~40 connections.
//
//   2. Nothing set `app.tenant_id`. database/schema.sql enforces isolation with FORCE ROW LEVEL
//      SECURITY and ops.current_tenant() reading that setting, so a session without it sees nothing
//      (fail-closed, correctly). The read path would have answered every query with an empty result
//      set — an absence of data where the honest answer was "I did not ask as anyone".
//
// The reset in release() is the sharp edge. `set_config(..., false)` is session-scoped, so a pooled
// connection carries its last tenant. Returning one to the idle set without clearing that is a
// cross-tenant read waiting to happen — and it is invisible, because the pool would look healthy.
// So the reset is mandatory here rather than left to a caller, and it happens BEFORE the connection
// is considered free.

/** How long an idle connection may sit before it is closed rather than handed out again. */
const DEFAULT_IDLE_TIMEOUT_MS = 30_000;

/** A connection stuck in a transaction must not be reused; it is closed instead. */
const DEFAULT_ACQUIRE_TIMEOUT_MS = 10_000;

export class PoolError extends Error {
  constructor(message, { busy = false } = {}) {
    super(message);
    this.name = 'PoolError';
    this.busy = busy;
  }
}

/**
 * @param {object} options
 * @param {(opts: object) => object} options.createClient  the driver's factory
 * @param {object} options.clientOptions                    passed through to it
 * @param {number} options.max                              the hard ceiling on connections
 * @param {number} [options.maxQueue]                       waiters allowed before a busy refusal
 * @param {number} [options.idleTimeoutMs]
 * @param {object} [options.log]
 */
export function createPool({
  createClient,
  clientOptions,
  max,
  maxQueue = 32,
  idleTimeoutMs = DEFAULT_IDLE_TIMEOUT_MS,
  acquireTimeoutMs = DEFAULT_ACQUIRE_TIMEOUT_MS,
  log = console,
}) {
  if (typeof createClient !== 'function') throw new Error('createPool needs createClient');
  if (!Number.isInteger(max) || max < 1) throw new Error('createPool needs a positive max');

  /** @type {Array<{client: object, idleSince: number|null}>} */
  const idle = [];
  /** Every connection handed out right now. */
  const inUse = new Set();
  const waiters = [];
  let total = 0;
  let closing = false;

  function makeConnection() {
    const client = createClient(clientOptions);
    total += 1;
    return { client, idleSince: null };
  }

  async function connect() {
    const slot = makeConnection();
    try {
      await slot.client.connect();
      return slot;
    } catch (error) {
      total -= 1;
      throw error;
    }
  }

  function takeIdle() {
    while (idle.length > 0) {
      const slot = idle.pop();
      if (slot.idleSince !== null && Date.now() - slot.idleSince > idleTimeoutMs) {
        total -= 1;
        Promise.resolve(slot.client.close?.()).catch(() => {});
        continue;
      }
      slot.idleSince = null;
      return slot;
    }
    return null;
  }

  /**
   * Borrow a connection, or refuse with `busy`.
   *
   * Refusing loudly is the point: a waiter that is queued for ever turns one slow tenant into every
   * tenant's latency, which §12.3 forbids by name.
   */
  async function acquire() {
    if (closing) throw new PoolError('the pool is closing');
    const existing = takeIdle();
    if (existing) {
      inUse.add(existing);
      return existing.client;
    }
    if (total < max) {
      const slot = await connect();
      inUse.add(slot);
      return slot.client;
    }
    if (waiters.length >= maxQueue) {
      throw new PoolError(`no connection available and the wait queue is full (${maxQueue})`, { busy: true });
    }
    // A waiter is resolved with an ALREADY-BORROWED slot: whoever hands it over has added it to
    // `inUse` on the waiter's behalf, so this function has nothing left to record.
    return new Promise((resolve, reject) => {
      const waiter = {
        settle: null,
        fail: null,
      };
      const timer = setTimeout(() => {
        const index = waiters.indexOf(waiter);
        if (index >= 0) waiters.splice(index, 1);
        reject(new PoolError(`no connection became available within ${acquireTimeoutMs}ms`, { busy: true }));
      }, acquireTimeoutMs);
      waiter.settle = (slot) => {
        clearTimeout(timer);
        resolve(slot.client);
      };
      waiter.fail = (error) => {
        clearTimeout(timer);
        reject(error);
      };
      waiters.push(waiter);
    });
  }

  /**
   * Clear the session's tenant and return the connection to the idle set.
   *
   * `mandatory` rather than best-effort: a connection whose reset failed is not reusable, and the
   * only safe thing to do with it is close it. Reusing it would leave the previous request's tenant
   * in force, which is the cross-tenant read this whole module exists to prevent.
   */
  async function release(client) {
    const slot = [...inUse].find((s) => s.client === client);
    if (!slot) return;
    inUse.delete(slot);

    let reusable = !closing;
    if (reusable) {
      try {
        // The tenant is cleared, and the setting is left empty rather than set to another tenant:
        // ops.current_tenant() then fails closed for any statement that arrives without one.
        await client.query("SELECT set_config('app.tenant_id', '', false)");
      } catch (error) {
        reusable = false;
        log?.warn?.(`pool: discarding a connection whose tenant reset failed: ${error?.message ?? error}`);
      }
    }

    if (!reusable) {
      total -= 1;
      Promise.resolve(client.close?.()).catch(() => {});
      drainWaiters();
      return;
    }

    slot.idleSince = Date.now();
    idle.push(slot);
    drainWaiters();
  }

  function drainWaiters() {
    while (waiters.length > 0) {
      const next = takeIdle();
      if (next) {
        inUse.add(next);
        waiters.shift().settle(next);
        continue;
      }
      if (total < max) {
        connect()
          .then((slot) => {
            inUse.add(slot);
            const waiter = waiters.shift();
            if (waiter) waiter.settle(slot);
            else {
              inUse.delete(slot);
              slot.idleSince = Date.now();
              idle.push(slot);
            }
          })
          .catch((error) => {
            const waiter = waiters.shift();
            if (waiter) waiter.fail(error);
          });
        continue;
      }
      return;
    }
  }

  return {
    acquire,
    release,
    /** Set the session tenant. Always called before a statement that RLS will judge. */
    async useTenant(client, tenant) {
      await client.query("SELECT set_config('app.tenant_id', $1, false)", [tenant]);
    },
    get size() {
      return total;
    },
    get idleCount() {
      return idle.length;
    },
    get inUseCount() {
      return inUse.size;
    },
    get waiting() {
      return waiters.length;
    },
    async close() {
      closing = true;
      for (const waiter of waiters.splice(0)) waiter.fail(new PoolError('the pool is closing'));
      const slots = [...idle, ...inUse];
      idle.length = 0;
      inUse.clear();
      total = 0;
      await Promise.allSettled(slots.map((s) => Promise.resolve(s.client.close?.())));
    },
  };
}
