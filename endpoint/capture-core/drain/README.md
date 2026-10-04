# drain — the device-to-cloud drain

The capture-core device-to-cloud path (ADR 0020): it reads the spool oldest-first, batches
observations into `POST /v1/events` requests over the configured HTTPS transport, and settles each
record from the per-event outcome. At M3 it also carries held content to the server, one event at a
time and only under a grant.

- **Transport** (`transport.go`): x509 mTLS from the issued leaf, or DPoP-signed requests
  (`Authorization: DPoP …` + a per-request proof), pinned to the `--ca-file` CA set with TLS 1.3.
- **Enrolment** (`POST /v1/enrol`): a generated keypair plus a PKCS#10 CSR (`x509`) or the public JWK
  with a proof of possession (`dpop`); the issued credential is sealed by `credential/`.
- **Token** (`POST /v1/token`, dpop only): a signed assertion exchanged for a short-lived,
  sender-constrained access token, cached and refreshed before expiry.
- **Batch/settle** (`batch.go`): 1–500 events, 8 MiB gzip / 32 MiB decompressed / 256 KiB per
  envelope; each event is settled `delivered`/`rejected` via `protocol.Outcome.SettleState`.
- **Backoff** (`backoff.go`): exponential with full jitter, `duplicate_batch` re-sent under a fresh
  batch id, terminal rejections retain the spool.
- **Retention**: before each pass the drainer drops records past `occurred_at + retention` and logs
  the count. Without that log a device (or a test fixture) whose observations predate the retention
  window delivers nothing and looks identical to an empty spool.

- **Content** (`content.go`, M3 only): the device half of the grant path
  ([docs/02 §3, §5.5, §10](../../../docs/02-ingest-and-transport.md)). The drainer does this
  because it owns the credential and the transport; [`contentstore/`](../contentstore/README.md)
  owns the content. Three steps:
  - `noteDelivered`: when an M3 prompt settles `delivered`, the store is told, with the digest and
    rule id from the envelope. A grant is decided about an event the server has, so content is not
    requestable before this.
  - `contentPass`: runs in every pass, before the spool is read, and handles at most eight objects
    so a backlog of held content cannot starve event delivery. It also applies local retention to
    the store.
  - `uploadOne`: `POST /v1/content/grant` with the device credential; on `granted`, seal the content
    under the object key the grant carries (`protocol.SealContent`) and make the one request the
    grant names, repeating its headers verbatim and adding `X-Sac-Raw-Digest` for the bytes
    actually written. A denial is terminal and the content stays local. `409` on either request
    means the object is already there and settles as uploaded; `422` settles as denied. Anything
    else is retried with the drain's backoff.

  A content failure never fails the event drain. This path was run against the local auth lab with
  an `x509` credential; the `dpop` branch of `authed` is written and has not been run. `content.go`
  has no unit test of its own.

The drainer holds no counter of its own; its accounting is the spool's `Stats`, and its health is a
`degraded`/`healthy` status with a closed `protocol.Detail` error code surfaced in the device-level
health snapshot.
