# internal — the ingest-api packages

Eight packages, none of them large. The split follows the seams the design actually has: the wire
contract, the device credential, the batch's own shape, the ladder's arithmetic, the atomic write, and
two test doubles that exist so the write path can be checked without a database.

Nothing in here re-declares the wire contract. Envelopes and responses come from
[endpoint/protocol](../../../endpoint/protocol/) and the generated contract types
([contracts/](../../../contracts/README.md)); the packages below consume them.

| Package | Why it exists |
|---|---|
| `auth` | Resolves the authenticated principal from the TLS client certificate: device uuid in the subject CN, tenant uuid in an organisational unit, credential id derived from the certificate bytes so a re-issue is a different, separately revocable row. Tenant and region come from the principal and never from the body (C32). The edge is a filter, never the authority: this re-validates the certificate and the per-device credential status on every request, with no cache, because the check is a point lookup at the design's mean rate. |
| `batchguard` | §5.3's `duplicate_batch` rule: a replayed `batch_id` is answered at batch level and the device re-sends with a fresh one. The check happens *before* the write and the mark *after* it commits, so a batch refused for its shape can be re-sent unchanged. Known limit, stated rather than discovered: the window is per process, so on a scale-out deployment a replay that lands elsewhere degrades to per-event `duplicate` outcomes — correct for the data, wrong only for the diagnosis. |
| `contract` | The adapter between the raw wire bytes and the rest of the service. `DecodeEnvelope` runs the generated contract types (the compile-time field vocabulary); `Schema.ValidateEnvelope` walks `contracts/event-envelope.schema.json` for the JSON Pointer and violated-constraint text §7's `detail` is made of. `Envelope` itself is only a name-keyed projection over the raw object, used for reading fields, redaction and the presence map. |
| `dedup` | §4 — the normative deduplication specification — in the parts reachable from this service: the 300-second bucket (§4.3), the material tier and route ranking (§4.4), and the key ladder (§4.5). The wire carries a finished `content_digest`, so the service recomputes the *tier* (not a wire field), derives ladder keys, and offers a diagnostic comparison against the device-supplied `dedup_key`. §4.2's NFC step is not applied here: the default normaliser is the identity, and this module does not import the repository's NFC implementation (`endpoint/canon/`); it is off the request path. |
| `httpapi` | The transport, and only the transport: size caps, optional gzip, one authenticated call into the service, and §5's error envelope. It cannot invent an outcome the write path did not make, and it logs an internal cause while sending only the device-facing message. It also carries `DevHeader`, the development authenticator, which still goes through the store's authoritative credential check. |
| `ingest` | The service: the batch envelope's own shape (§5.3), per-event validation against the contract in §7's fixed order, and the response that reports every event's outcome in request order. It deliberately does **not** own the dedup ladder — whether an event is new, merged or duplicate is `ingest.record_event()`'s decision, and `ingest.Error` keeps the device-facing message and the internal cause apart. Validation runs entirely in memory before the transaction opens. |
| `ladder` | The §4 conformance fixture: envelope builders and one scenario table, in a normal package rather than a `_test.go` file because two test packages must run the same table — the in-memory mirror and the live stored procedure. If the two ever disagree, a scenario fails on one side instead of a comment claiming they match. |
| `store` | The persistence seam: four methods, one of which is the write. `Memory` is the test double; `SQLStore` issues statements that are all constants in `sql.go`, which is where the service's entire SQL surface sits in one screen. `QuarantineReason` is the one place the §7 wire vocabulary is translated into `ingest.rejected.reason_code`, and it is honest about the two codes that have no quarantine row. |

## What is deliberately not here

There is no content handling, no blob client and no read path: ingest writes observations and never
serves content. There is no retry or queueing layer — a write failure is a 503 and the device retries
with a new `batch_id` — and no rate limiter, which is why there is no 429 path. See
[../README.md](../README.md#not-verified) for what is asserted and what is not.
