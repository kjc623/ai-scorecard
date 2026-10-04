# protocol — the device-side wire and IPC contract

**Owner: the Lead.** Every device component consumes this package and none of them redefines its
shapes; if a shape here is wrong, the Lead changes it rather than a component forking it.

The package exists to close a specific gap. The wire envelope is **not** defined here — it is
defined by [contracts/event-envelope.schema.json](../../contracts/event-envelope.schema.json) and the
generated Go types, per [ADR 0010](../../docs/adr/0010-the-envelope-is-a-discriminated-union-with-a-closed-kind-registry.md).
What this package adds is everything the contract deliberately does not cover: the local paths
between device processes, which never appear on the wire. Extension to core over Chromium native
messaging; core to classifier-host over length-prefixed frames on a local socket; core to the spool
as stored records; core to `ingest-api` as an HTTPS batch.

## What is in it

| File | Contract |
|---|---|
| `envelope.go` | The envelope as `json.RawMessage` plus the closed registries: kind (`prompt`, `usage_rollup`, `model_detection`), route (`ext.web_request`, `ext.page_context`, `ext.dom`, `proxy.tls`, `proxy.loopback`, `proc.detect`, `cli.shim`) and collection mode (M0–M3). |
| `frames.go` | The local-socket framing: one version byte, a big-endian 4-byte length, then payload. Version-per-frame, so a mismatch is detected on the frame that carries it rather than corrupting the stream. |
| `classifier.go` | `ClassifyRequest` / `ClassifyResponse`. The request has **no** tenant, device, user, tool, host or route field, and a compile-time guard keeps it that way. |
| `spool.go` | The spool record and `protocol.Store` — the interface `capture-core` calls and `capture-spool` implements. |
| `native.go` | Chromium native messaging: one JSON object with a `type` discriminator, attachment bytes chunked behind a manifest so an oversized upload is refused before transfer. |
| `batch.go` | `POST /v1/events` request and response shapes, and the batch caps (1–500 events, 8 MiB compressed, 32 MiB decompressed, 256 KiB per envelope). |
| `auth.go` | The device-authentication and enrolment vocabulary: the closed `x509`/`dpop` `AuthMode` set with its refusal-by-default check, the `X-Client-Cert`/`DPoP`/`Authorization` header names, `POST /v1/enrol` request and response, the `hardware_identity_hash` idempotency key, the issued-credential shape, and the RFC 7517 JWK subset with its RFC 7638 thumbprint (ADR 0020). |
| `content.go` | The M3 content grant ([docs/02 §5.5, §10](../../docs/02-ingest-and-transport.md)): `POST /v1/content/grant` request and response, the `X-Sac-*` upload metadata headers, the grant path's three reason codes, and the sealed-object format — `SealContent` / `OpenContent` (AES-256-GCM, nonce prepended, the event id as additional data) and `RawDigest`. |

## Two properties the shapes enforce

**Closed sets refuse rather than default.** `CollectionMode.Valid()` rejects anything outside
M0–M3 instead of picking one; a default would be a silent mode widening, which is the single failure
this enum exists to make impossible. The same pattern covers decision actions, health details,
counter names and ingest reason codes.

**The mode gates content.** `CollectionMode.ReadsContent()` is false for M0 and for the empty string,
and every content path consults it before touching bytes. A `ClassifyRequest` carrying content at M0
is a defect the classifier rejects, not something it quietly ignores.

`content.go` is here, rather than in the drain, for the same reason the envelope registries are:
three components must agree on it and none may fork it. The device seals an object, `control-api`
relays the key and names the upload headers, and `content-vault` opens the object for an approved
retrieval. The sealed format having one implementation is what lets the vault check the stored
bytes against `RawDigest` and open them with the key it unwrapped. The event id is the additional
data so that an object served against a different event fails to open.

The envelope stays `json.RawMessage` on purpose: the spool must persist exactly the bytes the device
will send, and `ingest-api` validates those same bytes against the schema. A second Go struct here
would be a second source of truth for the wire shape.

## What it deliberately does not do

- **No envelope validation.** The device validates before emitting (the shape check here exists so
  the device cannot spool something ingest would certainly reject) and `ingest-api` validates again
  as the one validating write path — [ADR 0001](../../docs/adr/0001-one-validating-write-path-collectors-hold-no-database-credential.md).
- **No transport, no I/O policy.** Frame read/write functions take an `io.Reader`/`io.Writer`;
  dialling, listening, reconnecting, timeouts and retries belong to the components.
- **Not Chromium's framing.** Native messaging is a 4-byte *little-endian* prefix with no version
  byte; the local socket is version byte plus big-endian length. Different peers, different
  transports, so they are different code (`native.go`, `frames.go`) rather than one shape bent to fit.
- **Not the authority on the wire contract.** Field names and closed vocabularies that also appear in
  the schema (`$defs/attachment`, confidence bands, reason codes) are kept identical to the contract
  by test, not by a second definition here.

## Tests

`protocol_test.go` covers framing round trips, version mismatch, oversize refusal before allocation,
truncated payloads, the closed vocabularies, the batch envelope rules, the spool entry shape, and the
two structural claims above: a `ClassifyRequest` cannot carry identity, and it refuses content at M0.
`auth_test.go` covers the closed `AuthMode` set, the header and version constants, enrolment
request/response round trips, the token-type refusal, and `JWK.Thumbprint` against the RFC 7638 §3.1
vector and the RFC 7515 Appendix A.3 EC key. `content.go` has no test in this package: the sealed
format's round trip, and the refusal of a substituted object, are asserted where it is opened, in
`vault/content-vault/vaultinvariants/serve_test.go`.
