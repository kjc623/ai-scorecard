# 0001. Every event enters through one validating ingest path; no collector holds a database credential

Status: proposed
Date: 2026-10-02

Supersedes: ADR 0001 of the pre-brief package (same decision, re-derived against the Shadow AI Capture brief).

## Context

Collection happens across nine usage modes (brief §2) on up to 5,000 managed devices per tenant. Every
device is treated as untrusted: brief §7 and the whole shape of the product assume that any one of
them may be compromised, and the design must limit what that compromise buys an attacker.

Facts that decided this:

- Devices buffer through outages and flush in bursts, so delivery is at-least-once and duplicate
  suppression has to live somewhere trustworthy. Brief C12 requires idempotency "enforced by the store".
- The browser extension is updated by a browser vendor on a schedule the enterprise does not control,
  and the desktop agent is updated through MDM rings with deliberate delay. Two clients, on two
  cadences, would both need the physical table shape if they wrote to the database directly.
- 5,000 devices holding pooled connections would exhaust the connection budget of a small managed
  instance for a workload of 0.14 events/second — a remarkable amount of cost and risk for no benefit.
- Brief C32 requires cross-tenant reads to be structurally impossible; brief C3 requires the collection
  ceiling to be applied before a policy bundle is signed. Neither can be enforced at a point the client
  can bypass.
- Brief E25: no EDR or MDM platform exposes content. This product ships its own collection, so its
  collection surface is also its attack surface.

## Decision

There is exactly one write path into the platform. Devices call `ingest-api` (`POST /v1/events`) and
`control-api` (`/v1/enrol`, `/v1/policy`, `/v1/health`, `/v1/content/grant`). No device receives a
PostgreSQL credential of any kind, and the database has no public endpoint.

`ingest-api` authenticates the device, validates each envelope against
[contracts/event-envelope.schema.json](../../contracts/event-envelope.schema.json), and calls
`ingest.record_event()`. The database is reachable only from the service tier over a private endpoint.

The authority for the write decision is the database function, not the service: `record_event` owns the
dedup tier decision, the fidelity tie-break and the retention materialisation. The transport layer
validates and reports; it does not decide.

## Alternatives considered

- **Direct database connections with a per-device role.** Rejected on connection budget, and because
  distributing 5,000 long-lived database secrets to laptops is a worse problem than 5,000 scoped device
  credentials — brief C11 and §4.2 assume a revocable per-device identity, and revocation must be an API
  operation rather than a database administration task.
- **PostgREST or Supabase auto-generated REST with row-level security doing the authorisation.**
  Genuinely viable and it removes a service to build. It lost because it couples two independently
  released client codebases to the physical table shape, leaves per-reason rejection detail (C12) to
  database constraints, and has no place to implement region pinning or the grant decision. It remains
  the right answer for a single first-party client shipping in lockstep with the schema.
- **One gateway for the desktop agent and direct access for the extension.** Rejected: it doubles the
  trust model and puts the least controllable client on the widest path.

## Consequences

Easier: schema evolution is a server-side concern; a compromised device yields one revocable credential
rather than database access; validation, rejection diagnostics, rate limiting and region pinning have
exactly one enforcement point; collectors can be tested against a local fake with no database.

Harder: `ingest-api` is on the critical path for every write and must scale and stay available through
the burst that follows an outage. It must stay tolerant of older collectors, because brief §5.5 puts the
endpoint on a slower update cadence than the backend.

We now maintain: a device-facing API surface with its own versioning contract, backwards compatibility
for at least two `schema_version` values at a time, and a quarantine workflow someone has to operate.

Revisit if: a customer requires devices to reach no public endpoint at all (then Private Link plus a
relay inside the corporate network), or if the platform collapses to a single first-party client
released in lockstep with the schema — at which point PostgREST-style direct access becomes cheaper and
correct.
