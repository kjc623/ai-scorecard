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

Node 22, **one dependency**: [`jose`](https://github.com/panva/jose), pinned in `package-lock.json`,
which itself has none. It verifies the product access token (`src/http/auth.js` says why a library
and not a hand-rolled verifier); everything else is the standard library, with no TypeScript
compiler and no framework. Run `npm ci` once before `node --test`; the image runs it at build time.
The package is runnable **ESM JavaScript with JSDoc type annotations** rather than
TypeScript. The PostgreSQL driver is **injected** into `executePlan` (anything with
`query(text, params)`, `begin`, `commit`, `rollback`) rather than imported by the pipeline, which
is also what makes the fail-closed path testable without a server. The driver the service injects
is the package's own `src/pg/`.

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
| `src/http/server.js` | `/healthz`, `/readyz` and `POST /v1/query`, plus the body ceiling, the concurrency gate and the error rendering. It also routes the two content reads to `content.js`, and the two audited configuration writes (`POST /v1/finding-review`, `POST /v1/tool-sanction`) to `review.js` and `sanction.js` |
| `src/http/content.js` | `POST /v1/content-search` and `POST /v1/content/retrieval`, forwarded to `content-vault`. It decides nothing about content |
| `src/http/pool.js` | Per-request connections, and the **mandatory tenant reset** on release |
| `src/http/main.js` | Resolve config, prove the database, listen, drain on `SIGTERM` |

Three of the names `src/http/config.js` reads bound how much the service does at once. Each is an
integer; a value outside its range is a refusal to start.

| Variable | Default | Range | What it bounds |
|---|---|---|---|
| `SAC_MAX_CONCURRENCY` | `8` | 1–1000 | Reads the gate in `server.js` admits at the same time |
| `SAC_MAX_QUEUE` | `32` | 0–10000 | Requests that may wait at the gate before the next one is answered `429 busy`; also the pool's wait-queue bound |
| `SAC_MAX_CONNECTIONS` | `40` | 1–1000 | The hard ceiling on connections the pool in `pool.js` opens |

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

The authenticated session is the **product access token** control-api mints (one issuer, one JWKS;
customer IdP tokens never reach this service). The dashboard's server presents it as a bearer, and
`src/http/auth.js` verifies it with `jose`: ES256 only, `typ` `at+jwt`, a named `kid`, exact `iss`,
`aud` containing `sac-query`, `exp`/`nbf`/`iat` within 60 s and no older than ten minutes, keys only
from the JWKS (a kid miss refetches at most once per 30 s). The tenant is `sac_tenant`, the actor
`actor`, the roles `roles` (unknown names dropped, none left refused), and `sid` goes into each audit
row's `detail`. A request without a verified token is refused `401` with a `WWW-Authenticate: Bearer`
challenge, unless `SAC_DEV_TRUST_PRINCIPAL=1` is set, which accepts the same development header
`ingestion/ingest-api` uses — the memory lab's escape hatch, off in a deployment. A presented token
that fails is a 401 even then; it never falls through to the header. `src/roles.js` holds the closed
role set (`viewer`, `analyst`, `content_reader`, `admin`) and the capability each source and endpoint
needs, including the two writes; a wrong role is `403 unauthorised_role`. A deployment that configures
no issuer refuses every read by design.

| Variable | Default | What it is |
|---|---|---|
| `SAC_AUTH_ISSUER` | none | The token issuer, control-api (e.g. `http://control-api:8080`). Empty: no token path |
| `SAC_AUTH_AUDIENCE` | `sac-query` | This service's audience. Without an issuer it is a refusal to start |
| `SAC_AUTH_JWKS_URL` | `{issuer}/.well-known/jwks.json` | Where the issuer's keys are. Without an issuer it is a refusal to start |
| `SAC_DEV_TRUST_PRINCIPAL` | off | `1` trusts `x-sac-dev-tenant` on a request that carries no bearer. Lab only, logged as a warning |

### The two content reads

This service cannot see content: it has no `SELECT` on `ops.content_object` or `ingest.search_text` and
no unwrap right. `src/http/content.js` forwards two requests to `content-vault` on its internal
ingress, as the service `query-api`, with the tenant and the actor taken from the session and never
from the body. With a product token, the caller's own `Authorization: Bearer` is forwarded too: the
vault verifies it itself (audience `sac-vault`) and refuses `X-Sac-*` headers that disagree with it:

| Route | Body | What happens |
|---|---|---|
| `POST /v1/content-search` | `{ query, limit?, cursor?, subject?, tool?, device?, mode?, window? }` | Forwarded as the vault's `terms` search for the configured scope, with the person, tool, device, mode and received-at window filters. Answers `{ state: "available", hits: [{ submission_id, snippet, rank }], truncated, next_cursor }`. It validates the filters here but decides nothing about content |
| `POST /v1/content/retrieval` | `{ event_ids, case_reference, second_approver, justification }` | Asks the vault to authorise one read for the first of the events that has a stored object. Answers `{ state: "available", event_id, grant_id, raw_digest, expires_at, retrieval_url }`, or the vault's `no_longer_available` result with its reason. The URL is the browser's to fetch; no content byte is relayed |

A refusal carries the vault's own reason code and status. `event_ids` are the observations of the
submission the analyst is looking at, which the record read already returned; the stored object is held
against one of them and this service cannot look up which, so it asks the vault about each until one
is not `no_content_object`. Every attempt is a request the vault audits. Both routes share the
concurrency gate with `/v1/query`.

| Variable | What it is |
|---|---|
| `SAC_CONTENT_VAULT_URL` | The vault's internal base URL. Empty: both routes answer `503 content_vault_not_configured` |
| `SAC_CONTENT_SEARCH_SCOPE` | The search scope asked of the vault. The vault must name it with a tier, or the search is refused `search_tier_not_in_scope` |

Three things are as built rather than as designed. The retrieval request relays the vault's
short-lived **single-use retrieval URL** and no content byte (docs/02 §11); the browser fetches that
URL through the analyst web tier, and a deployment's ingress routes it to the vault. The search answer
carries no index-coverage block. And the role gate sits here as well as in the vault: search needs
`analyst` or `content_reader`, minting a retrieval URL needs `content_reader`.

## Layout

| File | What it owns |
|---|---|
| `src/registry.js` | the frozen allow-list: sources, dimensions, measures, operators, buckets, cost classes. Every identifier that can reach SQL text lives here and nowhere else. |
| `src/errors.js` | §13's result states as a typed rejection, and the fields §2.1/§2.4 reject *by name*. |
| `src/validate.js` | the closed-document gate: unknown keys, fields, operators and buckets are rejected, not ignored. |
| `src/compile.js` | DSL to parameterised SQL. Identifiers are resolved from the registry; values only ever become `$n`. |
| `src/guard.js` | §12's cost guard: rejection, not degradation, before the statement runs. |
| `src/suppress.js` | §6 k-suppression and complementary suppression; a suppressed cell is never a zero. |
| `src/cursor.js` | §7 cursors: signed self-contained tokens, opaque server-side ids, `cursor_expired` for every failure of a signed token (DSL.md §7 lists what the server-side path refuses as `unsupported_query_shape`). |
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
| The content forwarder adds identity and decides nothing | `test/content-forward.test.mjs`, against a vault double: the principal and scope come from the session and configuration and not the body, a retrieval relays the vault's single-use URL and carries no content byte, the vault's refusal is carried through and mints no URL, and a malformed request never reaches the vault. |
| Only a verified product token is a session, and every role boundary holds | `test/auth.test.mjs` refuses each bad token by name (issuer, audience, `alg` none/HS256/RS256, `typ`, `kid`, `crit`, a foreign key, a DER signature, `exp`, `nbf`, age, an unknown kid after one rate-limited refetch, no product role). `test/http-server.test.mjs` drives real ES256 tokens through the server: every role against every route class (aggregate, device, subject-level, audit, both writes, search, retrieval), 401 without a session, the bearer forwarded to the vault, `sid` in the audit row, and the development header path unchanged. |
| The transport cannot be talked into a wrong answer | `test/http-server.test.mjs` and `test/pool.test.mjs`: the tenant is refused when unestablished, the tenant is bound rather than interpolated, the tenant is cleared before a connection is reused, and a request shed by the gate is answered `429 busy` rather than queued for ever. A refusal from the pool itself is not mapped to `busy`; it is rendered as the generic `500`. |

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
| `auth.test.mjs` | The product access token verifier and the `SAC_AUTH_*` configuration |
| `http-server.test.mjs` | The transport over a real socket, with a fake driver; the role matrix with real tokens |
| `pool.test.mjs` | Concurrency, and the tenant reset that makes connection reuse safe |
| `content-forward.test.mjs` | The two content reads, against a vault double |
| `snapshots.test.mjs` | That the committed compiled-SQL snapshot has not drifted |
| `helpers.mjs` | Shared fixtures, container discovery, the skip logic that distinguishes "no server" from "no schema", and a stand-in token issuer whose key is generated per run |

## What is in `tools/`

| File | What it does |
|---|---|
| `snapshot.mjs` | Regenerates `test/snapshots/compiled-sql.json`; `--check` fails if the compiled SQL changed |
| `check-sql.mjs` | Runs every compiled statement against the live schema |
| `evidence.mjs` | Renders a run's evidence |
