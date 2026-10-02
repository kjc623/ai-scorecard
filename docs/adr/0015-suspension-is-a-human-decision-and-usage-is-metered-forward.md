# 0015. Suspension is a human decision with two separate gates, and usage is metered forward

Status: proposed
Date: 2026-10-02

## Context

The product owner operates this as a multi-tenant service and sells it monthly. Two facts follow
that the design did not previously carry.

**First, suspending collection is destructive here in a way it is not for ordinary SaaS.** Brief §1:
the employer is not the account holder on the AI tools, so there is no vendor-side API — if the system
is not observing, the data does not exist anywhere. A suspension that stops ingest therefore destroys
history that can never be re-collected. A suspension that stops only *reads* is fully reversible. Those
are different decisions, and a single `suspended` status hides the difference inside one word.

The product owner's rule is that nothing is automated: a human decides. That moves the risk out of the
system and into the process — which means the system's obligation changes from *enforcing* a policy to
**making the decision informed and recorded**.

**Second, billing and erasure pull in opposite directions.** Brief §3.4 requires subject erasure to
remove data, and [03-data-platform](../03-data-platform.md) §7 leans on that deliberately: aggregates
*replace* their bucket rather than incrementing, precisely so an erasure can decrease a count. Billing
wants the opposite — the tenant consumed the service, and the invoice must not fall because a data
subject exercised a right. Any usage figure derived from `ingest.submission` silently understates the
bill after every erasure, and nobody notices until a customer asks why their usage graph has a step in
it.

## Decision

**Two enforcement gates, separate from the commercial lifecycle.**

`ops.tenant` gains `ingest_enabled` and `read_enabled`, with `status` remaining the commercial label
(`active`/`suspended`/`offboarding`/`closed`). The flags are what the runtime checks; `status` is what
the invoice and the account manager talk about. Collapsing them would bury the reversible/irreversible
choice inside a status change.

**An unattributed gate closure is unrepresentable.** A check constraint requires a reason, an actor and
a timestamp whenever either gate is closed. An open tenant needs no justification; a closed gate cannot
be recorded without one. With no automation, the audit trail is the only thing between a destructive
action and an unexplained one.

**The operator can see what a closure would cost.** `mart.v_tenant_suspension_impact` reports, per
tenant, enrolled and reporting devices, events currently spooled on devices, and events already dropped.
An operator about to close `ingest_enabled` should be able to see that N devices are holding M events
that will be dropped oldest-first and cannot be recovered. The view is row-level-security scoped to one
tenant rather than being a cross-tenant console: looking at one tenant before acting on it is the safer
shape, because a bulk view of every tenant's exposure invites a bulk action and only one of the two
gates is reversible.

**Usage is written forward and never derived.** `ops.usage_daily` holds one row per tenant per day:
accepted events, content bytes added, and an end-of-day device snapshot. It carries **no subject
reference and no device identifier**, so it is not personal data and a subject erasure neither touches
it nor changes what it says. `ingest-api` increments it in the same transaction that stores the events,
so a retried batch adds nothing (duplicates are rejected) and an erasure subtracts nothing.

Device counts are snapshotted daily rather than read live, because "how many devices in March" is a
question about March and a device removed in April must not change the answer.

**`ops.subscription` holds the basis and the period, and no price.** `billing_basis` is data, not code,
so changing what is metered is a row change rather than a release. `billed_through` freezes a period
once invoiced. Pricing lives in the billing system; what this system owes is *billable usage*.

## Alternatives considered

- **Automate suspension on payment failure.** Rejected by the product owner, and the design agrees for
  an independent reason: the failure mode is silent, irreversible data loss triggered by a card expiry.
  A system that can destroy uncollectable history without a person deciding to is a system that will
  eventually do so.
- **One `suspended` status with no split.** Rejected: it makes the reversible and the irreversible
  action the same action, and the difference is the whole point.
- **Derive usage from `ingest.submission` at period close.** Rejected as the erasure conflict above.
  It is also worse in a second way: a tenant could reduce their bill by exercising erasure rights.
- **Meter per active user rather than per enrolled device.** Not chosen; it would require a distinct-user
  count per period, which is materially closer to personal data than a device count and would need its
  own erasure analysis. The ledger records what the chosen basis needs, and `billing_basis` can add
  `events` or `content_bytes` later without a schema change.
- **A separate billing system of record.** Rejected *for the counters*: the ledger must be written in the
  same transaction as the events it counts, and a cross-system call on the ingest path is a availability
  dependency for a billing number. Invoicing and payment remain elsewhere.

## Consequences

Easier: a commercial action is a recorded, attributed, informed decision. The bill is stable under
erasure. A flat price can be checked against real per-tenant volume, because `content_bytes_added` is
recorded even though nothing bills on it — and content is the only unbounded cost in the system
(brief §3.1).

Harder: `ingest-api` now writes one extra row per batch, in the hot path, and a failure there must fail
the batch rather than silently lose a billing fact. The gate split means two things can be true at once
("active tenant, reads off"), which support tooling and the dashboard have to render without confusing
it with a suspension.

We now maintain: a nightly device snapshot, a period-close job that advances `billed_through`, and a
runbook for the operator who closes a gate (see [05-platform-delivery](../05-platform-delivery.md)).

Revisit if: the billing basis becomes usage-based, at which point the ledger already carries the bytes
and events and only `billing_basis` changes — or if a tenant population appears for which automated
enforcement is contractually required, which would be a product decision against this record.
