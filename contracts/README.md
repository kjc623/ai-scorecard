# `contracts/` — the wire contract

The single source of shape for everything that crosses a boundary in this system. One JSON Schema
describes the event envelope; the TypeScript and Go types are **generated** from it, and a gate fails
if the committed output drifts from the schema.

| Path | What it is |
|---|---|
| [`event-envelope.schema.json`](event-envelope.schema.json) | The contract. JSON Schema 2020-12. A discriminated union over a closed kind registry |
| [`generated/`](generated/) | The bindings, committed, with their own README |
| [`tools/`](tools/) | The generator and its suite |

## The envelope

An observation is what a device saw; the envelope is the record of it. The schema defines a
discriminated union keyed on `kind` and `collection_mode`, with a **closed** registry: a kind the
schema does not name is not a new kind, it is an invalid message, and ingest refuses it rather than
storing something nothing can read. The full reasoning, including why a closed registry was chosen
over an open one, is [ADR 0010](../docs/adr/0010-the-envelope-is-a-discriminated-union-with-a-closed-kind-registry.md).

The property that constrains everything else: **M0 forbids six content-derived fields**. At the
metadata-only collection mode a device may report that a submission happened and how large it was,
and may not report its content, a digest of it, or anything derived from it. That is enforced in the
schema, in `endpoint/protocol`, and in the database's shape constraints — three independent places,
because a rule enforced in one place is a rule with one bug away from being no rule.

`subject_name` is the one field that reverses an earlier position: the wire was pseudonymous end to
end (`user_ref` only), and ADR 0021 adds an optional clear account name beside it for tenants that
choose `device_identity = 'clear'`. It is not content-derived and is permitted at every kind and
mode; a `hashed` tenant's device sends neither it nor the clear hostname.

Observations are immutable and the envelope is the record, per
[ADR 0004](../docs/adr/0004-observations-are-immutable-and-the-closed-envelope-is-the-record.md).

## Why the output is committed

A generated file that is not committed makes every consumer's build depend on running a generator
first. Committing it and checking it for drift gives both: consumers read a file, and
`node contracts/tools/generate.mjs --check` fails the build the moment the schema and the committed
types disagree. That check is gate 2 of `node tools/accept.mjs`.

## What this directory is not

It is not a validation library. Ingest validates against the schema **at runtime** rather than
against generated Go types, and `tools/check-seams.mjs` exists because a component can name a field
the contract does not define while still compiling on both sides.

## Consumers

The endpoint is the producer: `endpoint/capture-core` mints the envelope, using the
`endpoint/protocol` vocabulary. `extension/` does not mint envelopes — it hands `capture-core`
protocol observation messages — but its fields feed the envelope. `ingestion/ingest-api` and the
database are the consumers. When you change the schema you are changing all of them, which is why the change
starts here rather than in any one of them.
