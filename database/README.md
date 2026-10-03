# `database/` — the PostgreSQL schema

The server store, and the only one. SQLite is the *device* spool and nothing else
([ADR 0002](../docs/adr/0002-postgresql-is-the-server-store-sqlite-is-only-the-device-spool.md)).

| Path | What it is |
|---|---|
| [`schema.sql`](schema.sql) | The whole schema: DDL, roles, grants, row-level security, triggers, functions, seed data |
| [`invariants.test.sql`](invariants.test.sql) | 47 assertions that the schema's properties actually hold |
| [`tools/`](tools/) | The checkers, the live-server runner, and the log tally |
| [`evidence/`](evidence/) | Captured runs and their README. The README is tracked; the raw logs are not |

## What the schema holds

Four schemas, split by who may read them: `ref` (closed reference data), `ops` (tenants, policy,
grants, audit, retention), `ingest` (submissions, observations, quarantine, the content-search index)
and `mart` (the aggregates the dashboard reads). Tenant isolation is structural rather than
conventional — forced row-level security on every tenant-scoped table, tenant-leading keys, and
per-tenant content keys as an independent second layer. The full model is
[docs/03-data-platform.md](../docs/03-data-platform.md).

## How it is verified

The schema is not a claim. It is applied to a real PostgreSQL server and its properties are asserted
**as the runtime roles**, never as a superuser — a superuser bypasses row-level security and would
therefore prove nothing about it:

```
powershell -NoProfile -ExecutionPolicy Bypass -File database/tools/run-invariants.ps1
```

That starts a throwaway server, applies `schema.sql`, runs `invariants.test.sql`, and reports the
assertion tally. It is gate 8 of `node tools/accept.mjs`. The assertions cover the collection-mode
boundary, tenant isolation (including fail-closed behaviour with no tenant set), the two-tier dedup
ladder, the policy ceiling, the audit hash chain, append-only enforcement, retention materialisation,
and the states the brief says must never be merged.

Two things the runner states about itself rather than hiding:

- **The deployment target is PostgreSQL 16; the host runs 17.11.** Every construct used has a
  minimum version ≤13, which is an argument, not a run.
- **The runner judges on its TALLY line, not its exit code**, because that exit code once lied (its
  error counter matched `ON_ERROR_STOP` in the echoed command). `tools/accept.mjs` follows the same
  rule deliberately: a gate that trusts an exit code it has seen lie once will lie again.

## The checkers

`schema.sql` is also checked **statically**, without a server, by `tools/check-schema.mjs`. It parses
the DDL and compares it against the wire contract and against two independent artefacts — the reason
codes `endpoint/protocol` can emit and the codes `ingestion/ingest-api` actually maps — and it parses
`README.md`, `docs/00-architecture.md` and `docs/03-data-platform.md` for the assertion count so a
stale number in prose fails the build instead of confusing a reader.

That last check exists because of a defect worth knowing about: an early rewrite of the quarantine
reason-code CHECK silently dropped `revoked_device`, and the behavioural assertion **passed anyway**,
because the same misunderstanding was in both the code and the test's expected list. Comparing the
CHECK against two artefacts outside the database is what caught it.
