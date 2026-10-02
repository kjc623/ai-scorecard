# 0007. Tenant residency is pinned and fails closed at ingest

Status: proposed
Date: 2026-10-02

Amends: ADR 0007 of the pre-brief package. The decision survives; its justification changed from a stated
EU requirement to a labelled assumption.

## Context

**The brief does not mention data residency anywhere.** This ADR rests on an inference, and it says so:
enterprise buyers in the 500–5,000-employee segment, handling regulated personal data (the data class
established for this product), routinely require in-region storage or EU-only processing. The brief's own
scope note excludes legal and jurisdictional review, so that requirement is not available from this
document set.

The engineering consequence is the same whether or not the inference holds: a residency field and a
fail-closed check are cheap now and expensive to retrofit once data exists in the wrong region.

## Decision

`ops.tenant.residency_region` pins a tenant to a region at onboarding.

The region is derived **server-side** from the authenticated device against its tenant's pin. A device
claiming a tenant in another region is rejected at ingest with `region_mismatch` and the rejection is
alarmed, because it is either a mis-enrolled device or an attack, and neither should be written locally
and sorted out later.

Physical residency is a per-region deployment of the whole stack, not a column. There is no cross-region
replication of tenant data by default: brief §3.3's residency requirement and a replication decision are
the same decision, so replication is opt-in per tenant and recorded.

## Alternatives considered

- **A residency column with no enforcement.** Rejected: a field that does not fail closed is
  documentation, and "we store it in the right region because the application chooses to" is not an
  answer anyone accepts in a security review.
- **A single global deployment with per-tenant row labels.** Rejected: it cannot satisfy an in-region
  requirement at all, and the data-protection argument for physical separation is not answered by a
  column.
- **Cross-region active-active.** Rejected as the default: it multiplies the residency question by the
  number of regions and solves a availability problem the brief does not have. Brief §8 asks for 99.9%
  ingest availability, which per-region single-primary with device-side buffering meets.

## Consequences

Easier: residency is answerable per tenant with a row, and the answer is enforced rather than asserted.
Region becomes a deployment parameter, so adding a region is an infrastructure exercise rather than a
code change.

Harder: every region is a full stack — database, key vault, blob container, container apps — so the
fixed monthly cost is per region, not per tenant. [05-platform-delivery](../05-platform-delivery.md) §11
sizes this, and it is the reason v1 launches in one region with the ladder built to instantiate more.

We now maintain: a region inventory, a per-region key vault and blob container, and a migration path for
a tenant that needs to change region — which is a full export, erase and re-import, not an `UPDATE`.

Revisit if: **master doc Q1** is answered and residency turns out not to be required. The pin then
becomes a low-cost default rather than a load-bearing constraint, and nothing else changes.
