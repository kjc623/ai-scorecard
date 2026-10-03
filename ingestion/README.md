# Ingestion — the write path into the analytics store

This tree holds the service that receives device observations and puts them in the database. It is
one half of "how data reaches the cloud" ([docs/02-ingest-and-transport.md](../docs/02-ingest-and-transport.md));
the read half is [query/](../query/README.md).

It exists because the collectors must not be trusted with the database. A collector holds no database
credential and no SQL ([ADR 0001](../docs/adr/0001-one-validating-write-path-collectors-hold-no-database-credential.md)):
its only capability is to call one endpoint as itself, presenting a per-device credential, and the
envelope it sends is the whole of what it can say. Everything that turns an envelope into a row —
validation against the frozen contract, the closed reason vocabulary, idempotency, the dedup
tie-break and retention — happens here, and the part of it that decides *merge* semantics happens one
layer further down, in the database's `ingest.record_event()`.

## What lives here

| Path | What it is |
| --- | --- |
| [ingest-api/](ingest-api/README.md) | The ingest service: `POST /v1/events`, one transaction per batch. The only thing in this tree |

There is one service and it has one job. The dependency edge points inward: this component consumes
the wire contract from [contracts/](../contracts/README.md) and the device vocabulary from
[endpoint/](../endpoint/README.md) and declares nothing of its own on the wire.

## What the write path deliberately does not do

- **It does not serve content.** The only content-egress path in the system is a per-event grant, and
  it runs through control-api and [content-vault](../vault/README.md). Ingest sees a digest, a size and
  a mode, never a payload (docs/02 §10, §11).
- **It does not decide whether an event is new.** §6 requires idempotency and the fidelity tie-break
  to be enforced by the store's unique constraints, not by a read-then-write in application code. The
  service calls `ingest.record_event()` and reports what it returned.
- **It does not do blob I/O.** `SAC_BLOB_CIPHERTEXT_ENDPOINT` is read, validated and reported unused,
  because the deployment passes it and a parameter nobody reads is worse than one that is read and
  declared inert.
- **It does not rate-limit.** There is no 429 path; `retry_after_s` appears only on 503.

## State, stated plainly

The device-to-cloud transport does not exist yet: `capture-core` spools observations and nothing sends
them to a server ([README.md](../README.md) "Known state"). So this is a verified server with no
production client. The whole-batch replay guard is per-process, the SQL plumbing is exercised only
under the `sac_sql_driver` build tag, and the dedup fixture that keeps the in-memory double honest is
explained in [ingest-api/README.md](ingest-api/README.md#not-verified).
