# 0003. The dashboard reads through a query API with a closed query DSL

Status: proposed
Date: 2026-10-02

## Context

Brief §3.6 defines ten questions the query layer must answer, and adds four constraints that together
rule out letting a browser near the database:

- "Cursor pagination only. No unbounded result sets over the event table."
- "Every read of subject-level data writes an audit entry **as it is served**, not afterwards."
- "Time-bucketed aggregates must not scan raw events."
- Brief C32: cross-tenant reads must be structurally impossible.

A query layer that must audit its own reads, bound its own result sets, and suppress small cells (a
per-team aggregate for a team of two is de facto personal data) cannot be a thin database proxy. Those
behaviours are policy, and policy needs a place to live.

## Decision

`query-api` (TypeScript/Node, Fastify) is the only read path for the dashboard. It exposes a **closed
query DSL**: filters, groupings, orderings and bucketings drawn from enumerated vocabularies, compiled
server-side to parameterised SQL against `mart` views and `ingest.submission`.

No part of a client-supplied value is ever concatenated into SQL text; a value is bound, and a
dimension name that is not in the enumerated vocabulary is rejected rather than escaped. The tenant
always comes from the authenticated session and never from the request body.

The DSL is closed because the alternative is a SQL parser with an allowlist, which is a well-known way
to ship an injection.

**As built.** `query-api` is plain JavaScript on Node.js's own `node:http`, with no framework and no
dependencies — not TypeScript and not Fastify — and the dashboard is plain JavaScript as well. The
closed DSL and its compilation to parameterised SQL are as decided. Neither imports the TypeScript types
generated from the wire contract.

## Alternatives considered

- **Direct database connections from the dashboard with row-level security.** Rejected: it satisfies
  C32 but leaves audit-on-read, k-suppression, cursor bounding, the freshness watermark and the
  `no_longer_available` semantics (C17) to be reimplemented in every client, or omitted.
- **GraphQL over the event tables.** Rejected: unbounded query shape is the exact opposite of C29's
  cursor-pagination rule, and the ten questions are known in advance, so the flexibility buys nothing
  and costs a resolver-by-resolver authorisation review.
- **An open SQL endpoint for analysts.** Rejected as a product surface, and answered instead by the
  scheduled Parquet export to the customer's own storage (brief §3.6), where they can run whatever SQL
  they like on data in their own account.

## Consequences

Easier: read auditing, suppression, freshness and cursor semantics are implemented once and cannot be
bypassed by a new dashboard screen. Query cost is bounded by construction rather than by review. The
dashboard and the API share generated types from the wire contract.

Harder: a new question that does not fit the DSL needs an API change, which is deliberate friction. The
DSL is a versioned contract and needs the same compatibility discipline as the event envelope.

We now maintain: the DSL specification, its compiled-to-SQL tests, and per-query-class statement
timeouts and row limits.

Revisit if: tenants consistently need ad-hoc analytics the DSL cannot express — in which case the answer
is to extend the export path rather than to open a SQL endpoint, because the export is where
customer-held keys already force the analytics to happen.
