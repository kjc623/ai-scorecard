# Architecture decision records

Thirteen records. Each states the context, the decision, the alternatives that were actually
considered, and the consequences — including what becomes harder and what would change the decision.

All are `proposed`. None has been reviewed outside this package.

| # | Decision | Status |
| --- | --- | --- |
| [0001](0001-one-validating-write-path-collectors-hold-no-database-credential.md) | Every event enters through one validating ingest path; no collector holds a database credential | proposed |
| [0002](0002-postgresql-is-the-server-store-sqlite-is-only-the-device-spool.md) | PostgreSQL is the server data store; SQLite is only the device-side spool | proposed |
| [0003](0003-the-dashboard-reads-through-a-query-api-with-a-closed-query-dsl.md) | The dashboard reads through a query API with a closed query DSL | proposed |
| [0004](0004-observations-are-immutable-and-the-closed-envelope-is-the-record.md) | Observations are immutable, and the closed envelope **is** the normalised record | proposed |
| [0005](0005-every-device-holds-its-own-revocable-credential-bound-to-transport.md) | Every device holds its own revocable credential, bound to its transport | proposed |
| [0006](0006-content-is-ciphertext-under-per-object-keys-wrapped-by-a-per-tenant-key.md) | Content is ciphertext under per-object keys wrapped by a per-tenant key | proposed |
| [0007](0007-tenant-residency-is-pinned-and-fails-closed-at-ingest.md) | Tenant residency is pinned and fails closed at ingest | proposed |
| [0008](0008-no-server-side-content-search-in-any-key-mode.md) | ~~There is no server-side full-text search over content, in any key mode~~ | **superseded by 0014** |
| [0009](0009-events-are-not-partitioned-at-v1-volume.md) | Events are not partitioned at v1 volume; tenant leads every key and index | proposed |
| [0010](0010-the-envelope-is-a-discriminated-union-with-a-closed-kind-registry.md) | The envelope is a discriminated union on `kind`, with a closed kind registry | proposed |
| [0011](0011-collector-health-is-a-keyed-operational-channel-not-an-event-stream.md) | Collector health is a keyed operational channel, not an event stream | proposed |
| [0012](0012-erasure-deletes-events-and-destroys-content-keys.md) | Erasure deletes events and destroys content keys; receipts state what survived | proposed |
| [0013](0013-no-kernel-driver-and-no-apple-entitlement-in-v1.md) | No kernel-mode component and no Apple restricted entitlement in v1 | proposed |
| [0014](0014-content-search-is-a-per-tenant-capability.md) | Content search is a per-tenant capability; the key model is chosen with it, not against it | proposed |
| [0015](0015-suspension-is-a-human-decision-and-usage-is-metered-forward.md) | Suspension is a human decision with two separate gates, and usage is metered forward | proposed |

Fifteen records. One is superseded, and the supersession is the most consequential edit in the set:
**ADR 0014 reverses ADR 0008 on customer requirement.** ADR 0008 is kept, marked superseded, because
its reasoning about brief §3.5 is still correct — what changed is the response to it.

## Which decisions carry the most weight

If only three are read, read these:

- **0014** supersedes 0008 and resolves brief risk R10 the other way, on customer requirement. It
  decides what the product is in a different sense from the others: content search is now a per-tenant
  capability, `full_text` requires vendor-readable content, and brief §3.5's mutual exclusivity is
  enforced as a check constraint rather than avoided by refusing the feature.
- **0009** is the only deliberate deviation from a literal requirement in the brief (C32's "every
  partition"). It records why, and the trigger that reverses it.
- **0013** removes two schedule blockers — Apple's restricted entitlements and a Windows kernel driver —
  by showing that v1 needs neither. It is the largest schedule de-risk in the package, and the cost is
  stated in the same record: coverage is lower, and the product has to say so.

## Records that amend earlier ones

This package supersedes an earlier design produced before the brief was available (see
`archive/pre-brief-generic-collector/`), which described a different product. Where a decision survived
re-derivation it is marked as superseding its predecessor:

| This record | Supersedes | What changed |
| --- | --- | --- |
| 0001 | 0001 (pre-brief) | Re-derived against nine usage modes and the 70–85% management-coverage reality |
| 0002 | 0002 (pre-brief) | Adds explicit reasoning for rejecting Supabase, and notes no extension is required |
| 0004 | 0004 (pre-brief) | **Reversed in part.** The pre-brief record claimed "normalisation happens downstream"; that layer is gone, because a closed versioned contract means the envelope is already the normalised record |
| 0005 | 0005 (pre-brief) | Adds transport binding and the rotation schedule |
| 0006 | 0006 (pre-brief) | **Redirected.** The pre-brief record encrypted fields inside event rows; this one protects the content store, which is the only tier where the product's promise is at stake |
| 0007 | 0007 (pre-brief) | Same decision, but its justification changed from a stated EU requirement to a labelled assumption, because the brief does not mention residency at all |
