// pool.test.mjs — the connection pool, and the two properties that make reuse safe.
//
// WHY THIS FILE EXISTS. The first version of the read path was handed ONE client and used it for
// everything: readiness probes and every read transaction. Two defects followed, both found by the
// agent who wrote the driver rather than by any test:
//
//   1. a second concurrent request — or a probe during a read — got SAC_BUSY, because one session
//      runs one transaction at a time; and
//   2. nothing set `app.tenant_id`, which every scoped row-level-security policy reads, so each
//      query would have returned nothing at all.
//
// Both are asserted here, because both were invisible: the service looked healthy, the probes
// answered, and the only symptom was an absence of data.

import test from 'node:test';
import assert from 'node:assert/strict';

import { createPool, PoolError } from '../src/http/pool.js';
import { loadConfig } from '../src/http/config.js';

/**
 * A driver stand-in that records everything and can be told to fail.
 *
 * `connectDelayMs` is what makes the concurrency assertions meaningful: without it, connections
 * appear instantly and a pool would look correct even if it handed the same session to everyone.
 */
function fakeDriver({ connectDelayMs = 0, failConnect = false, failResetFor = null } = {}) {
  const made = [];
  const createClient = () => {
    const client = {
      id: made.length + 1,
      connected: false,
      statements: [],
      async connect() {
        if (connectDelayMs) await new Promise((r) => setTimeout(r, connectDelayMs));
        if (failConnect) throw new Error('cannot connect');
        this.connected = true;
      },
      async query(text, params) {
        this.statements.push({ text, params });
        if (failResetFor !== null && /set_config\('app\.tenant_id', ''/.test(text) && this.id === failResetFor) {
          throw new Error('reset failed');
        }
        return { rows: [], rowCount: 0, fields: [] };
      },
      async begin() {},
      async commit() {},
      async rollback() {},
      async close() {
        this.closed = true;
      },
    };
    made.push(client);
    return client;
  };
  return { createClient, made };
}

const poolOf = (driver, overrides = {}) =>
  createPool({
    createClient: driver.createClient,
    clientOptions: {},
    max: 4,
    maxQueue: 2,
    log: { warn() {}, info() {}, error() {} },
    ...overrides,
  });

test('two concurrent borrowers get two different connections', async () => {
  // The defect this replaces: one shared client meant the second request waited for the first
  // request's COMMIT and then failed with SAC_BUSY.
  const driver = fakeDriver();
  const pool = poolOf(driver);
  const [a, b] = await Promise.all([pool.acquire(), pool.acquire()]);
  assert.notEqual(a, b, 'one session cannot serve two transactions');
  assert.equal(pool.size, 2);
  assert.equal(pool.inUseCount, 2);
  await pool.close();
});

test('a released connection is reused rather than reopened', async () => {
  const driver = fakeDriver();
  const pool = poolOf(driver);
  const first = await pool.acquire();
  await pool.release(first);
  assert.equal(pool.size, 1);
  assert.equal(pool.idleCount, 1);
  const second = await pool.acquire();
  assert.equal(second, first, 'the idle connection is handed back out');
  assert.equal(driver.made.length, 1, 'no second connection was opened');
  await pool.close();
});

test('the tenant is set on the session before a read, and cleared before reuse', async () => {
  // THE ONE THAT MATTERS. `set_config(..., false)` is session-scoped, so a pooled connection
  // carries its last tenant. If release() did not clear it, the next tenant's query would run as
  // the previous one — a cross-tenant read that looks like a healthy pool.
  const driver = fakeDriver();
  const pool = poolOf(driver);
  const conn = await pool.acquire();

  await pool.useTenant(conn, 'tenant-a');
  assert.deepEqual(conn.statements.at(-1), {
    text: "SELECT set_config('app.tenant_id', $1, false)",
    params: ['tenant-a'],
  });
  // The tenant is a BIND, never interpolated: a tenant id spliced into SQL text is the thing this
  // service exists to make unnecessary.
  assert.equal(conn.statements.at(-1).text.includes('tenant-a'), false, 'the tenant must not appear in the SQL text');

  await pool.release(conn);
  const clearing = conn.statements.at(-1);
  assert.match(clearing.text, /set_config\('app\.tenant_id', ''/, 'release must clear the session tenant');

  // And once cleared, the connection is only reused with a new tenant set.
  const again = await pool.acquire();
  assert.equal(again, conn);
  await pool.useTenant(again, 'tenant-b');
  assert.deepEqual(again.statements.at(-1).params, ['tenant-b']);
  await pool.close();
});

test('a connection whose tenant reset fails is closed, not returned to the idle set', async () => {
  // Reusing it would leave the previous tenant in force. Closing is the only safe answer, and the
  // pool must not quietly prefer the cheaper one.
  const driver = fakeDriver({ failResetFor: 1 });
  const pool = poolOf(driver);
  const conn = await pool.acquire();
  await pool.useTenant(conn, 'tenant-a');
  await pool.release(conn);
  assert.equal(conn.closed, true, 'a connection that cannot be cleared must be discarded');
  assert.equal(pool.idleCount, 0, 'and must not be offered to the next borrower');
  await pool.close();
});

test('past its ceiling and its queue, the pool refuses with busy instead of queueing for ever', async () => {
  const driver = fakeDriver();
  const pool = poolOf(driver, { max: 2, maxQueue: 1 });
  const a = await pool.acquire();
  const b = await pool.acquire();

  // The queue holds one; the next is refused immediately (not after a timeout).
  const queued = pool.acquire();
  await assert.rejects(() => pool.acquire(), (error) => {
    assert.ok(error instanceof PoolError);
    assert.equal(error.busy, true);
    return true;
  });

  // Releasing one admits the waiter, and nothing is lost.
  await pool.release(a);
  const admitted = await queued;
  assert.ok(admitted === a || admitted === b);
  assert.equal(pool.waiting, 0);
  await pool.close();
});

test('close() refuses new borrowers and releases every connection', async () => {
  const driver = fakeDriver();
  const pool = poolOf(driver);
  const conn = await pool.acquire();
  await pool.close();
  assert.equal(conn.closed, true, 'close must not leave a connection open');
  assert.equal(pool.size, 0);
  await assert.rejects(() => pool.acquire(), /closing/);
});

test('the configured ceilings are the ones §12.3 names', () => {
  // The numbers are not arbitrary, so they are asserted rather than left implicit: 8 concurrent
  // statements per tenant, a bounded queue of 32, and the ~40-connection global ceiling.
  const cfg = loadConfig({ SAC_PG_HOST: 'h', SAC_PG_DATABASE: 'd' });
  assert.equal(cfg.limits.maxConcurrency, 8);
  assert.equal(cfg.limits.maxQueue, 32);
  assert.equal(cfg.limits.maxConnections, 40);
});
