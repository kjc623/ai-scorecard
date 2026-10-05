# content-vault

The content vault of [docs/06-security-and-threat-model.md §5.3, §6](../../docs/06-security-and-threat-model.md)
and [docs/02-ingest-and-transport.md §10–11](../../docs/02-ingest-and-transport.md), and the only
component that can unwrap a content key (master **D7**). It is what makes **INV-1** true: content
crosses the network only on a per-event grant.

Go, standard library only in the default build. That build carries no PostgreSQL driver, so
`--store sql` refuses to start in it; the SQL lives as statement text in one file, and
`tools/live-schema-check.ps1` executes that same text against the live schema: 20 statements prepared,
the ADR 0014 pair and the tier/mode rule exercised, and the content-object lifecycle and
retrieval-grant put/claim/trigger path run inside rolled-back transactions. A build with the
`sac_sql_driver` tag links `pgx` (`cmd/content-vault/driver_tagged.go`, the same arrangement as
ingest-api and control-api) and serves from PostgreSQL. The vault never stores a KEK. It reads a stored
object only when it is given a ciphertext endpoint, for two purposes: to open it for an approved
redemption, and to index its text when the tenant's tier is `full_text`.

## The key hierarchy as built

Object plaintext is encrypted by the device under a per-object data key (the DEK). The DEK is wrapped by
a per-tenant key-encryption key (the KEK), by version; the vendor's Key Vault, the customer's vault under
federated identity, and the customer's HSM that the KEK never leaves are the three custody modes. What
`ops.content_object` holds is the wrapped DEK with its `kek_id` and `kek_version`, beside the blob
reference — never the ciphertext, and never the KEK.

`internal/keys` is six methods wide and holds the invariants structurally: `KeyWrapper` has no
`GetKey`/`ExportKey`/`ListKeys`/`ImportKey`, and a test asserts the whole method set, so nothing can ask
for key bytes; `Wrap`/`Unwrap` return or take a 32-byte DEK that the service drops when the operation
returns, and it is never stored in a grant; a wrapped key is bound to its row by GCM additional
authenticated data (`sac.dek.v1` plus length-prefixed tenant, object, KEK id and KEK version), so a key
moved elsewhere fails authentication rather than decrypting; the wrap is AES-256-GCM with a fresh 96-bit
nonce (§6.4 point 3); and `vault.Service` is the only caller of `Unwrap`, in a service with internal
ingress only.

`keys.LocalKeyWrapper` (AES-256-GCM software, in memory or a 0600 file) is what tests and local
development use. With `--key-file`, a KEK the service creates on a tenant's first wrap is written to
that file before the wrapped key is returned, so a restart does not orphan the objects wrapped under
it; a failed write fails the prepare. `keys.KMSKeyWrapper` is **explicitly unimplemented**: every method returns
`ErrNotImplemented`, `Health()` says so, and the binary refuses to start with `--key-backend kms` unless
the operator passes `--allow-unimplemented-kms`. Its doc comment is the specification of what Azure Key
Vault / Managed HSM must provide — `wrapKey`/`unwrapKey` with the AAD inside the wrapped plaintext,
version listing, rotate, and soft-delete-plus-purge with the **actual** recovery window in the receipt.

**Rotation re-wraps and never re-encrypts:** `RotateTenant` creates version N+1, unwraps each object's
DEK under the version its row names, re-wraps it under N+1 and updates `wrapped_dek` + `kek_version` in
one guarded statement (`kek_version = <expected>`, so two concurrent rotations cannot both win). The test
encrypts a payload with the DEK *before* rotation and decrypts it *after* with the re-wrapped DEK: the
ciphertext and digest are byte-identical, so nothing was re-encrypted.

## The grant matrix

A retrieval is two steps, both audited before they serve anything (docs/02 §11, §6.3). `Retrieve`
commits the audit row *before* the object is read, then unwraps the DEK to prove the content is
readable now: it returns a grant id, an expiry, the digest of the bytes that will be served, and a
single-use **retrieval URL** — never content. Fetching that URL (`RedeemURL`) applies the matrix and
serves the bytes; the URL is the capability, so it needs no caller header.

| Attempt | Refusal (closed set) |
|---|---|
| no grant / unknown grant id | `grant_required` |
| grant issued to another principal | `grant_principal_mismatch` |
| grant for event A used against event B | `grant_event_mismatch` |
| grant past its expiry, or already redeemed | `grant_expired`, `grant_already_used` |
| no case reference, no second approver, or an approver who is the requester | `case_reference_required`, `second_approver_required`, `second_approver_not_distinct` |
| the event has no stored object, or reads are disabled for the tenant | `no_content_object`, `retrieval_disabled` |
| a body tenant that disagrees with the principal | `tenant_mismatch` (docs/02 §12) |
| the audit row cannot be committed | `audit_unavailable` (fail closed) |

Eight concurrent redemptions of one grant yield exactly one success and seven `grant_already_used`,
because the claim is a single conditional write (`used_at IS NULL`). `vault.AllDenialReasons` is the
closed set; the first four are [docs/02 §10.2](../../docs/02-ingest-and-transport.md)'s grant-decision
reasons verbatim, carried through when a caller asks for content whose grant was denied. **§10.2
enumerates four grant reasons and says nothing about retrieval**, so the redemption-specific values are
additions, named here rather than presented as if the document had listed them.

Content that is gone is a **result**, not an error (C17, §11): `200` with
`{"state":"no_longer_available","reason":…,"receipt_ref":…}` and a reason from `retention_expired`,
`erasure`, `hold_released`, `tenant_offboarded`, `key_unavailable` — never `404` and never an empty body.

## Erasure, rotation and the index

`ShredObject` destroys the wrapped key and records the shred in the same statement, deletes the object's
`ingest.search_text` rows (key destruction does not reach a table that is not under a key — §6.4's
footnote), writes the erasure receipt, and optionally destroys the whole tenant KEK, which is the
offboarding path. After erasure the read path reports `no_longer_available` with the receipt linked; a
second erasure is idempotent and writes no second receipt. `search_text` is written only for units the
tenant's tier permits, and a unit that is not permitted is **reported** rather than silently dropped.

Who supplies the text: the finaliser holds only ciphertext, so a finalise that carries no index units
is indexed by the vault itself when the tenant's tier is `full_text`, a ciphertext endpoint is
configured and the object names a submission. It unwraps the key, reads and opens the stored object,
and writes the text as the submission's `prompt_body` unit, capped at 200,000 characters. An object
that cannot be read or opened is reported as a refused unit and logged; it never fails the finalise,
because the object is stored and retrievable whether or not it could be indexed. Objects finalised
before the tier or the endpoint was set are not indexed afterwards — there is no re-index operation —
and attachment names are not indexed by this path.

`opts.tenant`'s custody/search pair is a database constraint, and the service refuses the same
combinations independently — a dropped constraint, a bypassed migration or a stale replica must not turn
the vault into an indexer: `full_text` with `customer_held` is `key_custody_search_conflict`; `full_text`
with a ceiling below M3 is `search_tier_requires_m3`; a tenant tier of `disabled` is `search_disabled`; a
scope the signed bundle does not name is `search_tier_not_in_scope`; and a unit below the tier is
`index_unit_not_permitted` or `search_unit_not_permitted`. The bundle narrows the tenant ceiling per
scope, and an unnamed scope carries `disabled`. The three search forms are closed: `terms` (sanitised
into a tsquery this service constructs, so an analyst cannot make the database raise or inject an
operator), `substring` and `fuzzy` (filenames only, served by the partial trigram index).

A search composes the terms with the person, tool, device, collection mode and received-at window it is
given, and returns matches newest first with a keyset cursor. The vault applies those filters itself, by
joining `ingest.search_text` to a column-scoped read of `ingest.submission` (the grant is in
`database/schema.sql`), because `query-api` is not granted the index and must not filter it. An
unreadable cursor is `search_cursor_invalid`; the filters are recorded in the search's audit row.

## The HTTP surface and identity

One surface, **internal ingress only**: `POST /v1/content/object`, `/v1/content/object/finalise`,
`/v1/content/retrieval` (which mints the single-use retrieval URL), `GET
/v1/content/retrieval/{tenant}/{grant}` (which serves it), `/v1/content/redeem`, `/v1/content/shred`,
`/v1/content/rotate` and `/v1/content-search`, plus `GET /healthz`. The device-facing routes (`/v1/events`,
`/v1/content/grant`, the content upload) and the analyst routes (`/v1/query`, `/v1/policy`, `/v1/enrol`)
return **404** — a test asserts it, because "nobody would add that route" ages badly. `--addr` refuses a
non-loopback bind without an explicit acknowledgement, and the acknowledgement is not a substitute for
locking the origin to query-api and control-api (docs/02 §12). Identity comes from `internal/auth`, and
every missing fact is a refusal — no anonymous principal, no default tenant. With `SAC_AUTH_ISSUER` set
(control-api, contract §2) the person on the two human routes (`/v1/content/retrieval`,
`/v1/content-search`) is the **product access token** query-api forwards as `Authorization: Bearer`,
verified here with the standard library: ES256 only, `typ` `at+jwt`, exact `iss`, `aud` containing
`sac-vault` (`SAC_AUTH_AUDIENCE`), 60 s leeway, no older than ten minutes, keys from
`SAC_AUTH_JWKS_URL` (default `{issuer}/.well-known/jwks.json`, cached ten minutes, a kid miss refetching
at most once per 30 s). An `X-Sac-Tenant`, `X-Sac-Subject` or `X-Sac-Roles` header that disagrees with
the token is refused `403 principal_mismatch`; a human route with no token is `401`; roles never come
from a header. Service callers acting for no person (control-api, ops) still authenticate by
`X-Sac-Service` as the ingress set it. With no issuer the vault trusts the headers alone — **only**
because the ingress authenticates the peer and strips any inbound copy — and says so in a startup
warning; that is the lab arrangement. The minted retrieval URL's redemption needs no identity under
either: the single-use grant is the credential (task 10).

`GET /readyz` is added by the binary, not the service handler: `azure/modules/container-app.bicep`
probes both `/healthz` and `/readyz`, and before this the second path did not exist, so a container from
this binary would have stayed unready behind a 404. Readiness answers a question this service can
actually fail — *is the key backend implemented?* — and returns 503 for a process started with an
acknowledged-but-absent KMS, which is alive and must not be sent traffic. Configuration, flags and
probes are in [cmd/content-vault/README.md](cmd/content-vault/README.md); the six packages are in
[internal/README.md](internal/README.md).

## Build and test

```powershell
$env:GOCACHE="$PWD\.tools\gocache"; $env:GOPROXY="off"; $env:GOTOOLCHAIN="local"; $env:GOFLAGS="-mod=mod"
cd vault/content-vault
go build ./... && go vet ./... && go test ./... -count=1
pwsh -File tools/live-schema-check.ps1   # needs the docker client and the PostgreSQL container
```

`go test ./...` runs the module's tests across seven packages: the binary's configuration and
deployment-agreement tests, the grant matrix and its concurrency case,
rotation, erasure, tenant-key destruction, retention expiry, the search-tier and ADR 0014 refusals,
audit-before-serve by call ordering, fail-closed on a broken audit path, the key hierarchy (AAD binding,
versions, destruction, persistence, the unimplemented KMS and the interface's method set), the blob
reader (the credential it presents, the managed-identity token fetch and refresh, an unauthorized read
and an absent object), the retrieval-URL matrix (minted, redeemed once, expired, replayed, and read
through `Options.Blobs`), the HTTP surface including the edge-route rejection, and
[vaultinvariants/](vaultinvariants/README.md), which
now also asserts that a redemption returns the content the device sealed and refuses a stored object
that is not the bytes that were finalised. Indexing at finalise and key-file persistence on first wrap
have no test: both were exercised only in the local auth lab.

## NOT VERIFIED, and why

1. **Azure blob storage with a managed identity, and any cloud KMS.** `--key-backend kms` refuses to
   start without an explicit acknowledgement, and every method of `KMSKeyWrapper` returns
   `ErrNotImplemented`: nothing here has been exercised against Azure Key Vault or Managed HSM,
   because this build carries no cloud SDK and holds no credential for either. Modes 2 and 3 are
   *interfaces and refusals*, not working code. The blob reader is real code but the same shape: with
   `--blob-identity managed` it fetches an AAD access token from the instance metadata service and
   presents it on a Blob REST `GET`, and the token fetch and refresh are exercised against a fake IMDS
   in `internal/blob`; **no request has been made to a storage account**, because this host has none
   and no managed identity. What the lab exercises is `--blob-identity static`, a shared bearer the
   local `contentlab` stand-in checks, together with the digest check and `protocol.OpenContent`. A
   retrieval URL is minted and served end to end in the auth lab; the URL's `SAC_RETRIEVAL_URL_BASE`
   origin is a deployment setting the lab leaves empty.
2. **The `database/sql` plumbing in the default build.** `SQLStore` is written against the real schema.
   The default build carries no PostgreSQL driver and `--store sql` refuses to start in it; a build with
   the `sac_sql_driver` tag links pgx (`cmd/content-vault/driver_tagged.go`) and serves from PostgreSQL,
   which is how the local auth lab runs it (prepare, finalise, retrieval and redeem against the live
   schema, as the database owner). The Go test that runs those statements in-process skips
   *on this host* with the exact reason — the file sandbox denies a child process the Docker named pipe
   (`npipe:////./pipe/dockerDesktopLinuxEngine`) — and runs on a machine without that restriction.
3. **The retrieval-grant SQL path is verified as text.** `ops.retrieval_grant` landed with the two CHECKs
   this service asked for and a `retrieval_grant_single_use` trigger that refuses an unguarded UPDATE of a
   redeemed grant; the harness exercises put, claim, a second guarded claim matching zero rows, and that
   trigger, against the real table inside a rolled-back transaction. What is still not exercised is the
   `database/sql` plumbing around those statements (item 2).
4. **RLS as the second isolation layer.** The database's own assertions cover tenant isolation; this
   service sets the session tenant inside every transaction (`set_config(..., true)`) and the search
   statements compare `tenant_id` to a bound parameter *as well*, but the vault's own tests do not
   re-prove RLS.

## Decisions this leaves open

- **§10.2 does not enumerate retrieval refusals.** The extra reasons above are a closed set in this
  package; if the design wants one closed set for both surfaces, §10.2 or the schema should carry them.
- **The retrieval grant is a row, and the audit row is separate.** `ops.retrieval_grant` carries the
  single-use claim (`used_at`/`used_by`, guarded by the claim statement and by the database's trigger);
  `ops.audit` carries the record that the attempt happened, written before anything is served, for
  refusals and for searched-but-empty alike (§6.3).
- **`full_text` sets a plaintext-derived copy outside the key hierarchy** (§5.6, §6.4). The service
  refuses the impossible custody pair, but a `full_text` tenant's prompt text is server-readable by
  design; the erasure path reaches it by row deletion because key destruction cannot.
