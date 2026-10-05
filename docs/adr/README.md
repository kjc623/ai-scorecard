# Architecture decision records

Twenty-one records. Each states the context, the decision, the alternatives that were actually
considered, and the consequences — including what becomes harder and what would change the decision.

Twenty are `proposed` and one, 0008, is superseded by 0014. None has been reviewed outside this
package.

**This file is the index.** When a subsystem document and a record disagree, the record is the
decision and the document is the explanation. The subsystem documents are listed in
[`../README.md`](../README.md).

| # | Decision | Status |
| --- | --- | --- |
| [0001](0001-one-validating-write-path-collectors-hold-no-database-credential.md) | Every event enters through one validating ingest path; no collector holds a database credential | proposed |
| [0002](0002-postgresql-is-the-server-store-sqlite-is-only-the-device-spool.md) | PostgreSQL is the server data store; SQLite is only the device-side spool (as built, the spool is an append-only encrypted segment log, not SQLite — see [endpoint/capture-spool/README.md](../../endpoint/capture-spool/README.md)) | proposed |
| [0003](0003-the-dashboard-reads-through-a-query-api-with-a-closed-query-dsl.md) | The dashboard reads through a query API with a closed query DSL | proposed |
| [0004](0004-observations-are-immutable-and-the-closed-envelope-is-the-record.md) | Observations are immutable, and the closed envelope **is** the normalised record | proposed |
| [0005](0005-every-device-holds-its-own-revocable-credential-bound-to-transport.md) | Every device holds its own revocable credential, bound to its transport | proposed |
| [0006](0006-content-is-ciphertext-under-per-object-keys-wrapped-by-a-per-tenant-key.md) | Content is stored only as ciphertext under per-object keys wrapped by a per-tenant key | proposed |
| [0007](0007-tenant-residency-is-pinned-and-fails-closed-at-ingest.md) | Tenant residency is pinned and fails closed at ingest | proposed |
| [0008](0008-no-server-side-content-search-in-any-key-mode.md) | ~~There is no server-side full-text search over content, in any key mode~~ | **superseded by 0014** |
| [0009](0009-events-are-not-partitioned-at-v1-volume.md) | Events are not partitioned at v1 volume; tenant leads every key and index | proposed |
| [0010](0010-the-envelope-is-a-discriminated-union-with-a-closed-kind-registry.md) | The event envelope is a discriminated union on `kind`, with a closed kind registry | proposed |
| [0011](0011-collector-health-is-a-keyed-operational-channel-not-an-event-stream.md) | Collector health is a keyed operational channel, not an event stream | proposed |
| [0012](0012-erasure-deletes-events-and-destroys-content-keys.md) | Erasure deletes events and destroys content keys; receipts state what survived | proposed |
| [0013](0013-no-kernel-driver-and-no-apple-entitlement-in-v1.md) | No kernel-mode component and no Apple restricted entitlement in v1 | proposed |
| [0014](0014-content-search-is-a-per-tenant-capability.md) | Content search is a per-tenant capability; the key model is chosen with it, not against it | proposed |
| [0015](0015-suspension-is-a-human-decision-and-usage-is-metered-forward.md) | Suspension is a human decision with two separate gates, and usage is metered forward | proposed |
| [0016](0016-the-classifier-host-is-one-go-source-built-for-native-and-js-wasm.md) | The classifier host is one Go source built for native and `js/wasm` | proposed |
| [0017](0017-the-m3-content-state-marker-is-device-local.md) | The M3 content-state marker is device-local and never enters the envelope | proposed |
| [0018](0018-model-detection-carries-no-window.md) | `model_detection` carries no window, and the contract's kind branches are exhaustive | proposed |
| [0019](0019-the-origin-validates-the-device-certificate-itself.md) | The origin validates the device certificate itself; the edge is a filter | proposed |
| [0020](0020-device-transport-is-application-gateway-with-a-pluggable-authenticator.md) | Device transport is Application Gateway with a pluggable authenticator: mTLS, DPoP, or a dev secret | proposed |
| [0021](0021-device-identity-is-clear-by-default.md) | Device identity is clear by default, gated per tenant at the device, and a real username rides beside `user_ref` | proposed |

Twenty-one records. One is superseded, and the supersession is the most consequential edit in the set:
**ADR 0014 reverses ADR 0008 on customer requirement.** ADR 0008 is kept, marked superseded, because
its reasoning about brief §3.5 is still correct — what changed is the response to it. **ADR 0016** is an
amendment of a different kind: it changes the classifier host's language, and with it a §4.1 clause, while
leaving every property §9.1 requires of that component intact. **ADR 0017** resolves a contradiction rather
than a preference: §11.3's M3 row describes a content-state marker the closed envelope contract has no field
for, and the record decides it in the contract's favour. **ADR 0019** records a requirement the design
already states and the deployment does not yet implement: the origin validates the device certificate
itself, so the edge stays a filter rather than becoming the authority — and it is the record that makes
"the origin is reachable only through the edge" a correctness requirement rather than a hardening note.
**ADR 0020** amends 0019 on the one point research invalidated: the edge is Application Gateway, not
Front Door, and the origin authenticates a forwarded certificate through a pluggable seam (mTLS, DPoP, or
an acknowledged development principal) rather than terminating the client TLS handshake itself.
**ADR 0021** reverses the package's pseudonymity position on the owner's direction: device identity
is clear by default and a real username rides beside `user_ref`, gated per tenant at the device — it
supersedes `docs/04` A8 and `docs/06` A6, and is the one record here that widens what personal data
the product holds rather than narrowing it.

## Which decisions carry the most weight

If only three are read, read these:

- **0014** supersedes 0008 and resolves brief risk R10 the other way, on customer requirement. It
  decides what the product is in a different sense from the others: content search is now a per-tenant
  capability, `full_text` requires vendor-readable content, and brief §3.5's mutual exclusivity is
  enforced as a check constraint rather than avoided by refusing the feature.
- **0009** is the only deliberate deviation from a literal requirement in the brief (C32's "every
  partition"). It records why, and the trigger that reverses it.
- **0013** removes the kernel-component and restricted-entitlement blockers on all three endpoint
  platforms — Apple's restricted entitlements on macOS, a kernel driver on Windows, and an eBPF/TC hook on
  Linux — by showing that v1 needs none of them. It is the largest schedule de-risk in the package, and
  the cost is stated in the same record: coverage is lower, and the product has to say so.

## Records that amend earlier ones

This package supersedes an earlier design produced before the brief was available, which described a
different product and is not kept in this repository. Where a decision survived
re-derivation it is marked as superseding its predecessor:

| This record | Supersedes | What changed |
| --- | --- | --- |
| 0001 | 0001 (pre-brief) | Re-derived against nine usage modes and the 70–85% management-coverage reality |
| 0002 | 0002 (pre-brief) | Adds explicit reasoning for rejecting Supabase, and notes no extension is required |
| 0004 | 0004 (pre-brief) | **Reversed in part.** The pre-brief record claimed "normalisation happens downstream"; that layer is gone, because a closed versioned contract means the envelope is already the normalised record |
| 0005 | 0005 (pre-brief) | Adds transport binding and the rotation schedule |
| 0006 | 0006 (pre-brief) | **Redirected.** The pre-brief record encrypted fields inside event rows; this one protects the content store, which is the only tier where the product's promise is at stake |
| 0007 | 0007 (pre-brief) | Same decision, but its justification changed from a stated EU requirement to a labelled assumption, because the brief does not mention residency at all |
