# services/query-api

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
TypeScript. `node --check` passes on every file, and `node --test` is the runner. The PostgreSQL
driver is **injected** into `executePlan` (anything with `query(text, params)`, `begin`, `commit`,
`rollback`) rather than bundled, which is also what makes the fail-closed path testable without a
server.

## Layout

| File | What it owns |
|---|---|
| `src/registry.js` | the frozen allow-list: sources, dimensions, measures, operators, buckets, cost classes. Every identifier that can reach SQL text lives here and nowhere else. |
| `src/errors.js` | §13's result states as a typed rejection, and the fields §2.1/§2.4 reject *by name*. |
| `src/validate.js` | the closed-document gate: unknown keys, fields, operators and buckets are rejected, not ignored. |
| `src/compile.js` | DSL → parameterised SQL. Identifiers are resolved from the registry; values only ever become `$n`. |
| `src/guard.js` | §12's cost guard: rejection, not degradation, before the statement runs. |
| `src/suppress.js` | §6 k-suppression and complementary suppression; a suppressed cell is never a zero. |
| `src/cursor.js` | §7 cursors: signed self-contained tokens, opaque server-side ids, `cursor_expired` for every failure. |
| `src/audit.js` | §5 audit-on-read: the decision, the parameterised insert, and in-SQL hash-chain verification. |
| `src/envelope.js` | §2.3's envelope, §4.5 freshness, §11.3 coverage; a data-bearing response cannot omit its state. |
| `src/blocks.js` | the side reads: watermark, coverage, `newer_events_exist`, the class total, the unmapped residual, the flush check, Q9's detail. |
| `src/templates.js` | §3's ten questions as named templates over `mart`/`ops`. |
| `src/spike.js` | §3.6's spike rule (median + 4×MAD, ≥ 14 buckets) and its two annotations. |
| `src/plan.js` | the pipeline, and the one-transaction executor. |

## Commands

```bash
node --test test/                      # the suite (146 tests), offline
node tools/snapshot.mjs                # regenerate test/snapshots/compiled-sql.json
node tools/snapshot.mjs --check        # fail if the compiled SQL changed
node tools/check-sql.mjs               # run every compiled statement against the live schema
```

`test/db.test.mjs` executes the compiled SQL against the real `db/schema.sql` in a running
`shadowpg*` container, inside a transaction that is rolled back. It **skips** (loudly) when no
container is reachable; a skip is not a pass. From the repository root:

```bash
node tools/verify-all.mjs --only node
```

## What is verified, and how

| Claim | Evidence |
|---|---|
| The browser never speaks SQL | `test/hostile.test.mjs`: quotes, `;`, comment sequences, `UNION`, nested JSON, oversized arrays, unknown operators, prototype pollution, hostile identifiers. A hostile value either produces a typed rejection or compiles to **byte-identical SQL** to the benign document, with the value present only in `params`. |
| The allow-list is real | `test/db.test.mjs` runs every compiled statement, bound, against PostgreSQL 17.11 and the applied schema. A renamed column or a missing view is a syntax error here and nothing else would catch it. |
| k-suppression and cursor stability | `test/suppress-cursor.test.mjs`, plus `test/audit-plan.test.mjs` for the audit-before-suppression ordering. |
| The ten questions are answerable | `test/templates.test.mjs`, and the same ten shapes in `test/snapshots/compiled-sql.json`. |
| Nothing is served unaudited | `test/audit-plan.test.mjs`: a failing audit insert serves zero rows and returns `503 audit_unavailable`. |
