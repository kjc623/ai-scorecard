# 0004. Observations are immutable, and the closed envelope is the normalised record

Status: proposed
Date: 2026-10-02

Amends: ADR 0004 of the pre-brief package, which claimed "normalisation happens downstream". That layer
is gone; see Decision.

## Context

The pre-brief package assumed an open payload: an opaque `jsonb` blob whose shape would be settled later.
That assumption was wrong for this product, and the correction is the reason this ADR changed rather
than survived.

Brief §4.1 fixes the event envelope field by field, and brief §4.3 requires validation "against a
versioned schema". A versioned, closed record changes the architecture in two ways:

- There is no untyped remainder to normalise. The envelope *is* the normalised record; every field is a
  column, with one exception: `attachments` has no column on `ingest.observation`, and its store-side
  home is `ingest.search_text`.
- Reprocessing becomes a migration rather than a re-derivation, because the raw form is already
  structured and already validated.

What still holds from the original decision is immutability, and brief §7 is why: a security product
whose recorded history can be edited after the fact cannot answer "what did this person send" in a way
anyone should believe.

## Decision

`ingest.observation` is append-only. In-place `UPDATE` is refused by trigger for every role, including
the owner. `DELETE` is refused unless the session has explicitly declared that it is performing
retention expiry (`sac.retention_delete = on`), so that a row's disappearance is always attributable
either to a scheduled run or to a privileged intervention that had to announce itself.

There is **no separate `core` normalisation layer**, and no second copy of the envelope as `jsonb`. The
contract is closed (`additionalProperties: false`), so a parallel blob would only be a way for two
representations to disagree. `mart` remains derived and rebuildable from `ingest`.

## Alternatives considered

- **Keep an opaque payload with a `core` layer to normalise it later.** This was the pre-brief design.
  Rejected because it was built for a product whose data was not yet known; here the data is specified,
  and an untyped remainder would mean classification labels, findings and aggregates all key off a blob
  whose shape nothing validates.
- **Store the raw envelope alongside the columns for future reprocessing.** Rejected on the same
  grounds. Because the contract is closed and versioned, a future shape is a `schema_version` change
  with a migration, which is cheaper and more honest than carrying a duplicate of every row on the
  chance that reprocessing becomes necessary.
- **Allow `UPDATE` for correction.** Rejected. Brief §3.2 separates states that must never be merged and
  brief §4.1 requires the record to carry which route produced it; both imply that the historical
  observation is evidence. A correction is a new observation, not an edit.

## Consequences

Easier: the audit story is simple, because the event history cannot be rewritten by anything short of a
superuser disabling a trigger. Aggregates are provably derived. The retention path is explicit and
greppable rather than distributed across update statements.

Harder: a defect in a collector cannot be fixed retroactively in stored rows; the corrected collector
produces new observations, and the reconciliation report explains the discontinuity. The envelope
carries no collector version — that is reported through the health channel, as
`ops.collector_state.version`. Storage is not reclaimed until expiry, because there is no compaction by update.

We now maintain: a trigger that blocks mutation, and a documented escape hatch
(`sac.retention_delete`) whose only legitimate caller is the reconciler.

Revisit if: a regulator or customer requires in-place correction of a specific record, at which point
the answer is a superseding observation plus an audit entry, not a relaxation of this ADR.
