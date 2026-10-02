# 0009. Events are not partitioned at v1 volume; tenant leads every key and index

Status: proposed
Date: 2026-10-02

Records a deliberate deviation from a literal reading of brief C32.

## Context

Brief C32 says: **"Tenant is the leading dimension of every index and every partition."**

The volume, from brief §3.1: ~4.4M submissions per year per full tenant, 1–2 KB per row, 5–9 GB/year.
Four years of a maximally sized tenant is under 18M rows.

There is nothing to partition. Partitioning at this volume would add migration complexity, plan-time
cost across hundreds of partitions, and a class of bug where a query silently misses a partition — in
exchange for nothing.

## Decision

**No partitioning in v1.** Tenant is the leading column of every primary key and every index, and every
access path is expressed through a tenant-leading key. Row-level security is enabled and forced on every
tenant-scoped table.

This satisfies the *intent* of C32 — tenant-first access and structural isolation — and deviates from its
letter only in the word "partition", because there are none. The deviation is recorded here rather than
quietly ignored.

**The trigger for revisiting:** a single tenant exceeding **50M event rows**, which is roughly eleven
years at full scale or a 30× larger customer than the brief describes.

**The migration at that point:** monthly range partitioning on `received_at`, which is a migration and
not an application change, because `ingest.submission` and `ingest.observation` were designed for it —
every access path is already tenant-leading, and every row already carries a materialised `expires_at`
so that detach becomes a third retention mechanism alongside the two in brief C34.

## Alternatives considered

- **Partition by tenant.** Rejected: with many small tenants it creates partition sprawl, and with one
  large tenant it produces exactly one useful partition. It also cannot serve retention-by-detach,
  which needs time as the partition key.
- **Partition by receive day, immediately.** Rejected at this volume: 365 partitions per year for a
  workload that does not need them, and every query pays the planning cost.
- **Two-level (monthly range, then hash by tenant).** Rejected for v1: it is the right shape at scale and
  the wrong shape now. It stays available because the keys are already tenant-leading.
- **Partition by tenant only for large tenants.** Rejected as premature: it introduces two physical
  shapes for one logical table, and the operational cost of that is real while the benefit is
  hypothetical until a tenant actually crosses the threshold.

## Consequences

Easier: one table per concept, simple migrations, no risk of a query missing a partition, and no
partition-maintenance job to operate. Retention is a `DELETE` and, later, a detach.

Harder: retention at v1 is a row-level delete rather than an instantaneous metadata operation, so a very
large expiry run is a long transaction that has to be batched — which the reconciler does. Detaching a
month of data is not available as a cheap operation until the trigger fires.

We now maintain: a capacity watch on the largest tenant's event row count, because the trigger is
invisible until someone looks for it.

Revisit if: any tenant crosses 50M event rows, or if the brief's volume assumptions change by an order
of magnitude — at which point the plan above is the change, and this ADR is superseded rather than
amended.
