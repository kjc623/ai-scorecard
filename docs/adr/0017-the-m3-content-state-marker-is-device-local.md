# 0017. The M3 content-state marker is device-local and never enters the envelope

Status: proposed · Amends the M3 row of [01-collectors.md §11.3](../01-collectors.md) ·
Related: [0010](0010-the-envelope-is-a-discriminated-union-with-a-closed-kind-registry.md),
[0006](0006-content-is-ciphertext-under-per-object-keys-wrapped-by-a-per-tenant-key.md)
Date: 2026-10-02

## Context

`docs/01-collectors.md` §11.3's mode table describes M3's emission as "M1 fields, **no excerpt** (the schema
forbids it at M3), and a **local content-state marker**", and the flow diagram above it says the envelope
"notes only that content is held locally". `docs/00-architecture.md` §5.2 repeats the phrase.

The contract does not have such a field. `contracts/event-envelope.schema.json` defines the envelope as a
discriminated union with `additionalProperties: false` and a closed kind registry, so there is nowhere for a
content-state marker to go; and the 1.0 record's own description of `received_at` establishes the house
convention for extending the wire — a new field is a `schema_version` change, deliberately.

Building `capture-core` surfaced the contradiction as a blocking decision rather than a documentation
inconsistency: the device cannot emit a field the contract forbids, and it must not silently drop a field the
design says it emits.

## Decision

**The M3 content-state marker stays on the device.** The envelope for an M3 prompt carries exactly the M1
fields, no excerpt, and nothing about local content; the "content is held locally" fact lives in the device's
local content store and its spool record, where the content actually is.

The server-side consequence is deliberate and is the better half of the trade:

- A device *claiming* it holds content is not evidence that it does. The server learns that content exists
  when a grant is requested **for a specific event** and the device then uploads it — at which point the
  server has the bytes, the digest and the receipt. A self-reported flag adds a field the server cannot rely
  on while guaranteeing that every M3 event carries a permanent false negative until an upload happens.
- Grant issuance is driven by the event the analyst wants to see, not by an inventory of what devices say
  they hold. Nothing in §10's decision inputs (mode, budget, retention class, case reference) reads a
  content-state marker, so no read path changes.
- The marker remains useful where it is observable: the device's own coverage row can report content objects
  held and their retention deadlines, which is the same honesty property as INV-6 and is what an operator
  actually needs when diagnosing a missing upload.

## Alternatives considered

- **Add the field to the contract and bump `schema_version` to 1.1.** Rejected for v1: it makes every
  collector, validator and stored row carry a field with no consumer, and the contract's own convention treats
  a version change as the moment a seam is *used* rather than described. If a future need appears for the
  server to know what a device holds — for example a pre-grant inventory — that is the point to spend the
  version, and this record is what it would supersede.
- **Emit the marker as part of `content_excerpt`.** Rejected outright: M3 forbids an excerpt precisely so the
  wire cannot become a content channel, and a marker dressed as an excerpt reopens the door the schema closes.
- **Put it in the health channel instead.** Rejected as the *replacement*: health is per-collector and keyed
  by `(tenant_id, device_id, collector)`, so it cannot attribute content to an event, and per-event inventory
  in health is the high-cardinality stream that channel exists to prevent (ADR 0011). Reporting *counts* of
  held content objects there is fine and is what the decision above does.

## Consequences

**Easier.** The envelope stays closed and validating, so a collector defect cannot introduce a field, and the
"content crossing the network" path remains exactly one thing — a granted retrieval — rather than a path with
a metadata shadow of it.

**Harder.** An operator cannot ask the server "which events can this device still produce content for?"
without asking the device. That is a real loss of visibility and it is accepted: the alternative was a field
whose absence on every record meant nothing and whose presence proved nothing.

**We now maintain.** The divergence between §11.3 of the collectors document and the contract, which this
record resolves in the contract's favour. `docs/01-collectors.md` §11.3's M3 row and the §11.3 flow note are
the places a future reader will hit it.

**Revisit if:** a workflow appears that genuinely needs a server-side inventory of held content — most likely
a customer asking for "how much content is still retrievable before retention drops it". At that point the
answer is a device-side inventory reported through the health channel's counters, or a `schema_version` 1.1
with a real consumer, not a flag bolted onto v1.0.
