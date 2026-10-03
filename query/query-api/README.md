# query/query-api

The only read path for the dashboard: a **closed query DSL** compiled server-side to
**parameterised SQL** over `mart` views and `ingest.submission`.

* **Normative grammar:** [`DSL.md`](DSL.md) — frozen for `query_version: "1"`.
* **Decision:** [ADR 0003](../../docs/adr/0003-the-dashboard-reads-through-a-query-api-with-a-closed-query-dsl.md).
* **Specification:** [`docs/04-dashboard-and-query.md`](../../docs/04-dashboard-and-query.md) §2.3, §2.4, §3, §5, §6, §7, §12, §13.

## The one sentence

> No part of a client-supplied value is ever concatenated into SQL text; a value is **bound**, and
> a dimension name that is not in the enumerated vocabulary is **rejected rather than escaped**.
> The DSL is closed because the alternative is a SQL parser with an allow-list, which is a
> well-known way to ship an injection.

## Runtime, and why this is plain JavaScript

Node 22, **zero dependencies**, offline: no `npm install`, no TypeScript compiler, no framework.
The package is therefore runnable **ESM JavaScript with JSDoc type annotations** rather than
TypeScript. The PostgreSQL driver is **injected** into `executePlan` (anything with
`query(text, params)`, `begin`, `commit`, `rollback`) rather than bundled, which is also what makes
the fail-closed path testable without a server.

The package ships **two entry points into the same pipeline**:

| Entry point | What it is for |
|---|---|
| `src/index.js` | The library: `plan()`, `executePlan()` and the registry, importable and injectable. Everything below the transport lives here |
| `src/http/main.js` | The service: reads the environment, opens a pool, proves the database, listens. This is what the container runs |

## The HTTP surface

`src/http/` is transport only. It makes no decision about what a query means, what a value may be, or
what an error is — those live in the modules below, and `QueryError` already carries its own HTTP
status, so the transport repeats the pipeline's decision rather than inventing a second one.

| File | Responsibility |
|---|---|
| `src/http/config.js` | The deployment's `SAC_*` vocabulary in one place. Fails closed on anything it does not recognise; there is no "prefer" TLS mode, because a silent fallback to plaintext is a downgrade rather than a mode |
| `src/http/server.js` | `/healthz`, `/readyz` and `POST /v1/query`, plus the body ceiling, the concurrency gate and the error rendering |
| `src/http/pool.js` | Per-request connections, and the **mandatory tenant reset** on release |
| `src/http/main.js` | Resolve config, prove the database, listen, drain on `SIGTERM` |

Two properties of that layer are load-bearing and easy to get wrong:

- **The tenant is set on the session before anything else**, with
  `set_config('app.tenant_id', $1, false)`. Every scoped row-level-security policy reads
  `ops.current_tenant()`, so a read that never sets it returns **nothing at all** — fail-closed rather
  than a leak, but an absence of data presented where the honest answer is "this session asked as
  nobody". `SET LOCAL` cannot be used: a utility statement takes no bind parameter, and a tenant
  interpolated into SQL text is the one thing this service exists to avoid.
- **A connection is cleared before it is reused.** `set_config(..., false)` is session-scoped, so a
  pooled connection carries its last tenant; releasing one without clearing it is a cross-tenant read
  that looks like a healthy pool. A connection whose reset fails is closed rather than returned.

The authenticated session is **not built yet**: no Entra principal is resolved to a tenant, so a
deployment refuses every read with `403` until it exists. `localdev/` runs the service with
`SAC_DEV_TRUST_PRINCIPAL=1`, the same explicit escape hatch `ingestion/ingest-api` uses.

## Layout

| File | What it owns |
|---|---|
| `src/registry.js` | the frozen allow-list: sources, dimensions, measures, operators, buckets, cost classes. Every identifier that can reach SQL text lives here and nowhere else. |
| `src/errors.js` | §13's result states as a typed rejection, and the fields §2.1/§2.4 reject *by name*. |
| `src/validate.js` | the closed-document gate: unknown keys, fields, operators and buckets are rejected, not ignored. |
| `src/compile.js` | DSL to parameterised SQL. Identifiers are resolved from the registry; values only ever become `$n`. |
| `src/guard.js` | §12's cost guard: rejection, not degradation, before the statement runs. |
| `src/suppress.js` | §6 k-suppression and complementary suppression; a suppressed cell is never a zero. |
| `src/cursor.js` | §7 cursors: signed self-contained tokens, opaque server-side ids, `cursor_expired` for every failure. |
| `src/audit.js` | §5 audit-on-read: the decision, the parameterised insert, and in-SQL hash-chain verification. |
| `src/envelope.js` | §2.3's envelope, §4.5 freshness, §11.3 coverage; a data-bearing response cannot omit its state. |
| `src/blocks.js` | the side reads: watermark, coverage, `newer_events_exist`, the class total, the unmapped residual, the flush check, Q9's detail. |
| `src/templates.js` | §3's ten questions as named templates over `mart`/`ops`. |
| `src/spike.js` | §3.6's spike rule (median + 4×MAD, ≥ 14 buckets) and its two annotations. |
| `src/plan.js` | the pipeline, and the one-transaction executor. |
| `src/pg/` | the zero-dependency PostgreSQL wire client the service runs on. Its README explains why there is no driver dependency. |

## Commands

```bash
node --test                            # the suite
node tools/snapshot.mjs                # regenerate test/snapshots/compiled-sql.json
node tools/snapshot.mjs --check        # fail if the compiled SQL changed
node tools/check-sql.mjs               # run every compiled statement against the live schema
node src/http/main.js                  # run the service (needs SAC_PG_HOST and SAC_PG_DATABASE)
```

`node --test test/` does **not** work on Node 22.23.1 — the runner treats the argument as a file.
Use `node --test`, which is what `tools/verify-all.mjs` does.

`test/db.test.mjs` executes the compiled SQL against the real `database/schema.sql` inside a
transaction that is rolled back. It **skips** — loudly, with a reason — when no container is
reachable *or* when one is reachable but has no schema applied; the two are different situations and
the skip says which. A skip is not a pass.

## What is verified, and how

| Claim | Evidence |
|---|---|
| The browser never speaks SQL | `test/hostile.test.mjs`: quotes, `;`, comment sequences, `UNION`, nested JSON, oversized arrays, unknown operators, prototype pollution, hostile identifiers. A hostile value either produces a typed rejection or compiles to **byte-identical SQL** to the benign document, with the value present only in `params`. |
| The allow-list is real | `test/db.test.mjs` runs every compiled statement, bound, against PostgreSQL 17.11 and the applied schema. A renamed column or a missing view is a syntax error here and nothing else would catch it. |
| k-suppression and cursor stability | `test/suppress-cursor.test.mjs`, plus `test/audit-plan.test.mjs` for the audit-before-suppression ordering. |
| The ten questions are answerable | `test/templates.test.mjs`, and the same ten shapes in `test/snapshots/compiled-sql.json`. |
| Nothing is served unaudited | `test/audit-plan.test.mjs`: a failing audit insert serves zero rows and returns `503 audit_unavailable`. |
| The service reaches a real database | `test/pg-client.test.mjs` speaks the wire protocol to PostgreSQL, including SCRAM-SHA-256, bound parameters and SQLSTATE surfacing. |
| The transport cannot be talked into a wrong answer | `test/http-server.test.mjs` and `test/pool.test.mjs`: the tenant is refused when unestablished, the tenant is bound rather than interpolated, the tenant is cleared before a connection is reused, and a shed request is answered `429 busy` rather than queued for ever. |

## What is in `test/`

| File | What it covers |
|---|---|
| `hostile.test.mjs` | The security claim: hostile values either produce a typed rejection or compile to byte-identical SQL, with the value only in `params` |
| `validate.test.mjs`, `compile.test.mjs` | The closed-document gate and the compiler |
| `guard-envelope.test.mjs` | The cost guard, and that a data-bearing response cannot omit its freshness and coverage |
| `suppress-cursor.test.mjs` | k-suppression, complementary suppression, and cursor stability |
| `audit-plan.test.mjs` | Audit-on-read, and the audit-before-suppression ordering |
| `templates.test.mjs` | The ten questions |
| `spike.test.mjs` | The median + 4×MAD spike rule and its annotations |
| `db.test.mjs` | Every compiled statement, bound, against a real PostgreSQL. Skips with a reason |
| `pg-client.test.mjs` | The wire client: framing and SCRAM without a server; types and transactions against one |
| `http-server.test.mjs` | The transport over a real socket, with a fake driver |
| `pool.test.mjs` | Concurrency, and the tenant reset that makes connection reuse safe |
| `snapshots.test.mjs` | That the committed compiled-SQL snapshot has not drifted |
| `helpers.mjs` | Shared fixtures, container discovery, and the skip logic that distinguishes "no server" from "no schema" |

## What is in `tools/`

| File | What it does |
|---|---|
| `snapshot.mjs` | Regenerates `test/snapshots/compiled-sql.json`; `--check` fails if the compiled SQL changed |
| `check-sql.mjs` | Runs every compiled statement against the live schema |
| `evidence.mjs` | Renders a run's evidence |
