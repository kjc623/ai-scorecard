# 0002. PostgreSQL is the server data store; SQLite is only the device-side spool

Status: proposed
Date: 2026-10-02

## Context

The original request asked explicitly which database to use — Postgres, SQLite, or Supabase. The brief
settles the scale question: ~4.4M submissions and 5–9 GB per year per full tenant (brief §3.1).

Two properties carry the decision, and neither is about volume:

1. Brief C32 requires tenant isolation "enforced at the storage layer, not only in application code",
   and that a cross-tenant read be "structurally impossible, not merely unauthorised". That is a
   description of forced row-level security.
2. Brief §3.3 requires per-tenant encryption keys with customer-held keys supported. Key custody and
   row storage have to be able to move independently, which means the store must not be the thing that
   owns the keys.

## Decision

**PostgreSQL 16 on Azure Database for PostgreSQL Flexible Server** is the server store.

**SQLite in WAL mode** is the device-side spool, encrypted at the application layer, and is never a
server store.

**As built.** PostgreSQL 16 is the deployment target the infrastructure pins; the local lab and the
recorded schema verification ran on PostgreSQL 17, to which the schema applies unmodified. The device
spool is not SQLite: `endpoint/capture-spool` is an append-only, encrypted segment log behind the
`protocol.Store` interface, which a SQLite-backed store could satisfy without changing a caller. See
[endpoint/capture-spool/README.md](../../endpoint/capture-spool/README.md).

Supabase is rejected. Not because it is bad — it is managed PostgreSQL with an attached product surface
— but because every requirement above is a plain PostgreSQL feature, and the attached surface is
procurement friction in a 500–5,000-employee buyer segment while changing nothing about region pinning or
key custody.

## Alternatives considered

- **SQLite as the server store.** Rejected on four counts: no network protocol, so every service would
  need shared filesystem semantics; no row-level security, so C32's "structurally impossible" becomes
  "carefully implemented"; single-writer concurrency, which the aggregator alone would violate; and no
  managed point-in-time recovery, which brief §3.4's retention obligations and §12's disaster recovery
  both assume. It remains exactly right on the device, where the requirements are the opposite ones:
  bounded, transactional, crash-safe, and no server.
- **Supabase.** Rejected as above. It would be a reasonable choice for a prototype with one first-party
  client.
- **A document store for the envelope.** Rejected: the query layer's ten questions (brief §3.6) are
  aggregates and filters over typed dimensions, which is a relational workload, and brief C32's isolation
  requirement is cheapest to satisfy where row-level security already exists.

## Consequences

Easier: forced row-level security, custom roles per component, point-in-time recovery, private
networking, a chosen region, and per-tenant keys held outside the database are all available without
inventing anything. `sha256()` and `gen_random_uuid()` are built in, so the audit hash chain works on a
stock instance and the `pgcrypto` question in master doc Q12 disappears. The only extensions the schema
needs are `pg_trgm` and `btree_gin`, both for the content search index of
[ADR 0014](0014-content-search-is-a-per-tenant-capability.md).

Harder: connection management needs a pooler in front of the service tier, and long-running aggregations
need direct connections so they cannot occupy the pool.

We now maintain: schema migrations gated in CI, and a role and grant model
([database/schema.sql](../../database/schema.sql) §10) that has to be extended deliberately whenever a table is added
— default privileges are revoked precisely so a new table is invisible until someone grants on purpose.

Revisit if: ingestion volume grows by two orders of magnitude — which would mean per-keystroke or
per-request capture, which brief C9 forbids — or if a tenant's needs outgrow a shared instance, at which
point the answer is a dedicated database rather than a different engine.
