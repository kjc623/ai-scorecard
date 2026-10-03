# drain — the device-to-cloud drain

The capture-core device-to-cloud path (ADR 0020): it reads the spool oldest-first, batches
observations into `POST /v1/events` requests over the configured HTTPS transport, and settles each
record from the per-event outcome.

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

The drainer holds no counter of its own; its accounting is the spool's `Stats`, and its health is a
`degraded`/`healthy` status with a closed `protocol.Detail` error code surfaced in the device-level
health snapshot.
