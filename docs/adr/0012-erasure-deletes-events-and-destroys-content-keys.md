# 0012. Erasure deletes events and destroys content keys; receipts state what survived

Status: proposed
Date: 2026-10-02

## Context

Brief §3.4 requires "subject-based erasure that produces a **verifiable record** — what was removed,
when, by which mechanism. A claim of deletion without a receipt is not usable by the customer."

The pre-brief package, designed against a much larger and unbounded dataset, answered erasure with
crypto-shredding: encrypt every sensitive field under a per-subject key and erase by destroying the key.
That reasoning does not survive contact with this product's numbers.

Brief §3.1: ~4.4M events/year per full tenant, 1–4 prompts per active user per day. A subject's events
therefore number in the hundreds to low thousands. Deleting them is cheap.

## Decision

**Events are deleted.** A subject erasure deletes the subject's `ingest.observation` and
`ingest.submission` rows, and recomputes every aggregate bucket they contributed to. The receipt records
counts per table.

**Content is destroyed by key, and also deleted.** Both operations, because either alone leaves a gap:
deleting the ciphertext while the wrapped key survives in a database backup has not destroyed anything,
and destroying the key while the ciphertext remains cannot honestly be described in a receipt as
"removed".

**Tenant offboarding destroys the tenant key**, which is the one place key destruction is the primary
mechanism, because it is the only practical way to render an entire tenant's content unreadable at once.

Three properties of the receipt are deliberate:

1. **It records what survived, and why.** `remaining_counts` covers rows under an active hold (brief
   C35), and the pseudonymous `subject_ref` in the append-only audit log. A receipt claiming total
   removal while a hold preserved something is worse than no receipt.
2. **It is hash-stamped** and linked to the reconciliation run that verified the deletion by an
   independent path (brief C34's two-mechanism rule).
3. **It is produced even when the answer is partial.** A partial erasure that says so is usable; a
   complete-sounding receipt that is not is not.

**Aggregates must be recomputed, not decremented.** This is the decisive architectural consequence and
the reason the aggregator recomputes buckets instead of incrementing them: an increment-only aggregate is
structurally incapable of honouring an erasure, because deleting a submission would leave its
contribution in every bucket it touched and the dashboard would keep reporting usage by an erased person.

## Alternatives considered

- **Crypto-shredding for events** (the pre-brief design). Rejected as over-engineering here. It is the
  right tool when data is too large or too replicated to delete; a few hundred rows are neither.
  It also produces strictly weaker evidence: "we destroyed a key" is an assertion, whereas "these 1,247
  rows were removed" is a countable fact.
- **Deleting content without destroying the key.** Rejected — see the backup gap above.
- **Destroying the key without deleting the ciphertext.** Rejected for the same reason, from the other
  direction: the object would still exist and the receipt would be false.
- **Soft delete with a tombstone.** Rejected: brief §3.2 requires `shredded` to be distinct from
  `local_only` and `uploaded`, which implies real removal rather than a marker.

## Consequences

Easier: the receipt is specific and countable. The erasure path is ordinary SQL plus two key operations.
No per-subject key ceremony exists for events at all.

Harder: aggregates must be recomputed after an erasure, so the erasure job touches `mart` as well as
`ingest`, and it must do so in the same transaction as the deletion or a crash leaves a dashboard
reporting data the customer has been told is gone.

We now maintain: the erasure ledger, its replay onto any restored database **before promotion**
(described in [03-data-platform](../03-data-platform.md) §9.2), and the reconciliation that proves both
deletion mechanisms ran.

Revisit if: a tenant's retention obligations require erasure to be near-instantaneous at a volume where
row deletion cannot keep up — at which point key destruction returns as the primary mechanism, and this
ADR is superseded rather than amended.
