# Vault — where content is encrypted, and the only place it can be read

This tree holds `content-vault`, the one component that can unwrap a content key
([docs/06-security-and-threat-model.md](../docs/06-security-and-threat-model.md) §5.3, decision **D7**).
It exists because of the product's central property: content leaves the device only on an explicit
per-event grant, and the ciphertext is useless to everyone else in the system. The database holds a
wrapped key beside a blob reference and no plaintext; the vendor's other services hold no key material
at all. Only this service holds the KEK, and only for the duration of one operation.

It is also where the awkward half of the design lives. Content search is a per-tenant capability
(`disabled`, `attachment_names`, `full_text`) and `full_text` requires content the vendor *can* read, so
the vault is the component that has to refuse the impossible combinations, index only what the tenant's
tier permits, and delete that index by row when erasure cannot reach it with a key
([ADR 0014](../docs/adr/0014-content-search-is-a-per-tenant-capability.md)).

## What lives here

| Path | What it is |
| --- | --- |
| [content-vault/](content-vault/README.md) | The service and its Go module. The only thing in this tree |

## What the vault deliberately does not do

- **It has no device-facing and no user-facing endpoint.** Devices reach content through control-api's
  grant decision and then write ciphertext straight to blob storage; browsers reach content through
  query-api. The binary refuses a non-loopback bind without an explicit acknowledgement, and the
  acknowledgement is not a substitute for locking the origin to those two callers.
- **It never holds a KEK it could export.** The key interface is six methods wide and has no
  `GetKey`/`ExportKey`/`ListKeys`; a test asserts the method set, because an interface that cannot
  express the request is the only version of "the KEK never leaves the key store" a reviewer can check.
- **It has no blob client and mints no retrieval URL.** A deployment returns a short-lived storage URL;
  this build does not. Given `--blob-ciphertext-endpoint` it reads a stored object from that endpoint
  with a plain GET, checks it against the recorded digest, opens it with the object key and returns
  the content in the response; without it, `Redeem` returns the object's reference and digests and no
  bytes. That read is a development stand-in, exercised against the local lab's storage stand-in and
  against no storage account.
- **It has no working cloud KMS.** `--key-backend kms` refuses to start unless an operator
  acknowledges that the backend is unimplemented, and every method of the KMS wrapper returns
  `ErrNotImplemented`. Modes 2 and 3 are interfaces and refusals, not working code.

## State, stated plainly

The service is Go on the standard library only in its default build, where `--store sql` refuses to
start and the SQL is statement text verified against the live schema by a harness. A build with the
`sac_sql_driver` tag links a PostgreSQL driver, as ingest-api and control-api do, and serves from the
database; that is how the local auth lab runs it, where the whole content path — a device upload under
a grant, the object recorded and indexed, an approved retrieval returning the content — has been run
end to end. [content-vault/README.md](content-vault/README.md) carries the grant matrix, the key
hierarchy as built, and the full "not verified" list — including the one host-specific reason the Go
integration test skips here.
