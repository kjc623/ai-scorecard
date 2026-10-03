# `src/http/` — the service surface

The transport that turns the library next door into something a container can run. It answers the
two paths the deployment probes and the one read path the dashboard calls.

| File | Responsibility |
|---|---|
| `config.js` | The deployment's `SAC_*` vocabulary in one place, resolved once at startup |
| `server.js` | The three routes, the body ceiling, the concurrency gate, and error rendering |
| `pool.js` | Borrowing a connection per request, and clearing its tenant before reuse |
| `main.js` | The entry point: resolve config, prove the database, listen, drain on `SIGTERM` |

## It owns no semantics

This layer does not decide what a query means, what a value may be, or what an error is. It calls
`plan()` and `executePlan()` from `../plan.js`, which own validation, the cost guard, compilation,
the audit and suppression. `QueryError` already carries its own HTTP status, so `renderError()`
repeats the pipeline's decision instead of forming a second opinion about it.

That matters more than it sounds: two places deciding what an error means is two places that can
disagree, and the one a client sees would be whichever ran last.

## Three things that are easy to get wrong here

**The tenant goes on the session before anything else.** Every scoped row-level-security policy reads
`ops.current_tenant()`, which reads `app.tenant_id`. A read that never sets it returns nothing at
all: fail-closed rather than a leak, but an absence of data where the honest answer is "this session
asked as nobody". It is set with `set_config('app.tenant_id', $1, false)` — `SET LOCAL` cannot be
used, because a utility statement takes no bind parameter, and a tenant spliced into SQL text is
exactly what this service exists to avoid.

**The tenant is cleared before a connection is reused.** `set_config(..., false)` is session-scoped,
so a pooled connection carries its last tenant. `pool.release()` clears it, and a connection whose
reset fails is closed rather than returned — reusing it would be a cross-tenant read that looks like
a healthy pool.

**Readiness is not liveness.** `/healthz` answers for the process; `/readyz` asks the database. A
service holding a broken database connection should be taken out of rotation, not killed. `/readyz`
borrows its **own** connection rather than sharing one with a read: a probe that queues behind a query
reports a busy service as unready.

## The gate

Concurrency is bounded and the queue is bounded, per §12.3. Beyond both, a request is answered
`429 busy` immediately rather than queued — unbounded queueing turns one tenant's pathological query
into every tenant's latency. `createGate()` is exported and separately tested. The `429` is the
gate's: a refusal from the pool itself (no connection free and its wait queue full, or none freed
within its wait) is not a `QueryError`, so `renderError()` answers it with the generic `500`.

## Running it

```
node src/http/main.js
```

It refuses to start without `SAC_PG_HOST` and `SAC_PG_DATABASE`, and proves it can reach the database
before it listens. There is deliberately no in-memory fallback: a read path with no store would
answer every query with a number that came from nowhere.
