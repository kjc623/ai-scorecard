# content-vault

The content vault of [docs/06-security-and-threat-model.md §..3, §6](../../docs/06-security-and-threat-model.md)
and [docs/02-ingest-and-transport.md §10-11](../../docs/02-ingest-and-transport.md), and the only
component that can unwrap a content key (master **D7**). It is what makes **INV-1** true: content
crosses the network only on a per-event grant.

Go, standard library only. No PostgreSQL wire driver is fetchable offline (ADR 0016), so the SQL
lives as statement text in one file and is verified against the live schema by a harness
(`tools/live-schema-check.ps1`): 20 statements prepared, the ADR 0014 pair and the tier/mode rule
exercised, the content-object lifecycle and the retrieval-grant put/claim/trigger path run against
the real tables.

## The key hierarchy as built

```
Object plaintext  (one submission's prompt or attachment)
    │  AES-2.6-GCM under a per-object data key (the DEK), 2.6 bits, fresh per object
    ▼
Per-object DEK ──wrapped by──► per-tenant key-encryption key (KEK), by version
    │                              │
    │                              ├─ vendor            Key Vault / Managed HSM
    │                              ├─ customer_managed  the customer's vault, federated identity
    │                              └─ customer_held     the customer's HSM; the KEK never leaves it
    ▼
ops.content_object: wrapped_dek + kek_id + kek_version, beside the blob reference — never the ciphertext
```

`internal/keys` is five methods wide and holds the invariants structurally:

| Invariant (§..3) | How it is held |
|---|---|
| The KEK never leaves the key store | `KeyWrapper` has no `GetKey`/`ExportKey`/`ListKeys`; a test asserts the method set, so nothing can ask for key bytes |
| The unwrapped key exists for one operation only | `Wrap`/`Unwrap` return or take a 32-byte DEK; the service drops it when the operation returns, and it is never stored in a grant |
| A wrapped key opens only in its own row | GCM additional authenticated data is `sac.dek.v1` + length-prefixed tenant, object, KEK id and KEK version; a wrapped key moved to another row fails authentication rather than decrypting |
| 2.6-bit wrap, not brute-forceable (§6.4 point 3) | AES-2.6-GCM with a fresh 96-bit nonce per wrap |
| Only one service unwraps (D7) | `vault.Service` is the only caller of `Unwrap`, and the service has internal ingress only |

**Key backends.** `keys.LocalKeyWrapper` (AES-2.6-GCM software, in memory or a 0600 file) is what
tests and local development use. `keys.KMSKeyWrapper` is **explicitly unimplemented**: every method
returns `ErrNotImplemented`, `Health()` says so, and the binary refuses to start with
`--key-backend kms` unless the operator passes `--allow-unimplemented-kms`. Its doc comment is the
specification of what Azure Key Vault / Managed HSM must provide, operation by operation —
`wrapKey`/`unwrapKey` with the AAD inside the wrapped plaintext, version listing, rotate, and
soft-delete-plus-purge with the **actual** recovery window reported in the receipt (A4).

**Rotation** re-wraps and never re-encrypts: `RotateTenant` creates version N+1, unwraps each
object's DEK under the version its row names, re-wraps it under N+1 and updates `wrapped_dek` +
`kek_version` in one guarded statement (`kek_version = <expected>`, so two concurrent rotations
cannot both win). The test encrypts a payload with the DEK *before* rotation and decrypts it *after*
with the re-wrapped DEK: the ciphertext, blob path and digest are byte-identical, so nothing was
re-encrypted. The old version stays in the key store, so a row that failed to re-wrap is still
readable; a new write is sealed under N+1.

## The grant matrix

A retrieval is two steps, both audited before they serve anything (docs/02 §11, §6.3):

1. `Retrieve` — C16's case reference plus a **distinct** second approver, the audit row committed
   *before* the object is read, then the DEK unwrapped to prove the content is readable now. It
   returns a grant id, an expiry and the digest of the bytes that will be served — never content.
2. `Redeem` — the grant matrix, then the content.

| Attempt | Refusal (closed set) |
|---|---|
| no grant / unknown grant id | `grant_required` |
| grant issued to another principal | `grant_principal_mismatch` |
| grant for event A used against event B | `grant_event_mismatch` |
| grant past its expiry | `grant_expired` |
| grant already redeemed (including concurrently) | `grant_already_used` |
| retrieval with no case reference | `case_reference_required` |
| retrieval with no second approver | `second_approver_required` |
| second approver equals the requester | `second_approver_not_distinct` |
| the event has no stored object | `no_content_object` |
| reads disabled for the tenant | `retrieval_disabled` |
| a body tenant that disagrees with the principal | `tenant_mismatch` (docs/02 §12) |
| the audit row cannot be committed | `audit_unavailable` (fail closed) |

Eight concurrent redemptions of one grant yield exactly one success and seven
`grant_already_used`, because the claim is a single conditional write (`used_at IS NULL`).

`vault.AllDenialReasons` is the closed set; the first four are
[docs/02 §10.2](../../docs/02-ingest-and-transport.md)'s grant-decision reasons verbatim, carried
through when a caller asks for content whose grant was denied. **§10.2 enumerates four grant
reasons and says nothing about retrieval**, so the redemption-specific values are additions, named
here rather than presented as if the document had listed them.

Content that is gone is a **result**, not an error (C17, §11): `200` with
`{"state":"no_longer_available","reason":…,"receipt_ref":…}` and a reason from `retention_expired`,
`erasure`, `hold_released`, `tenant_offboarded`, `key_unavailable` — never `404` and never an empty
body.

## Erasure and the index

`ShredObject` destroys the wrapped key and records the shred in the same statement, deletes the
object's `ingest.search_text` rows (key destruction does not reach a table that is not under a
key — §6.4's footnote), writes the erasure receipt, and optionally destroys the whole tenant KEK,
which is the offboarding path. After erasure the read path reports `no_longer_available` with the
receipt linked; a second erasure is idempotent and writes no second receipt.

`search_text` is written only for units the tenant's tier permits, and a unit that is not permitted
is **reported** in the response rather than silently dropped.

## Search tiers and ADR 0014

`opts.tenant`'s pair is a database constraint, and the service refuses the same combinations
independently — a dropped constraint, a bypassed migration or a stale replica must not turn the
vault into an indexer:

| Situation | Refusal |
|---|---|
| `full_text` with `customer_held` | `key_custody_search_conflict` |
| `full_text` with a ceiling below M3 | `search_tier_requires_m3` |
| tenant tier `disabled` | `search_disabled` |
| the signed bundle does not name the scope | `search_tier_not_in_scope` |
| `prompt_body` below `full_text`, or any unit below `attachment_names` | `index_unit_not_permitted` / `search_unit_not_permitted` |

The bundle narrows the tenant ceiling per scope and an unnamed scope carries `disabled`. The three
forms are closed: `terms` (sanitised into a tsquery this service constructs, so an analyst cannot
make the database raise or inject an operator), `substring` and `fuzzy` (filenames only, served by
the partial trigram index).

## The HTTP surface

One surface, **internal ingress only**: `POST /v1/content/object`, `/finalise`, `/retrieval`,
`/redeem`, `/shred`, `/rotate`, `/content-search`, plus `GET /healthz`. The device-facing routes
(`/v1/events`, `/v1/content/grant`, the content upload) and the analyst routes (`/v1/query`,
`/v1/policy`, `/v1/enrol`) return **404** — a test asserts it, because "nobody would add that route"
ages badly. `--addr` refuses a non-loopback bind without an explicit acknowledgement; the
acknowledgement is not a substitute for locking the origin to query-api and control-api (docs/02
§12).

Identity comes from the internal ingress (`internal/auth`): the header authenticator trusts the
headers **only** because the ingress authenticates the peer and strips any inbound copy. Every
missing fact is a refusal — there is no anonymous principal and no default tenant.

## Build and test

```powershell
$env:GOCACHE="$PWD\.tools\gocache"; $env:GOPROXY="off"; $env:GOTOOLCHAIN="local"; $env:GOFLAGS="-mod=mod"
cd services/content-vault
go build ./... && go vet ./... && go test ./... -count=1
# live-schema evidence (needs the docker client and the PostgreSQL container)
pwsh -File tools/live-schema-check.ps1
```

`go test ./...` runs 38 tests: the grant matrix and its concurrency case, rotation, erasure,
tenant-key destruction, retention expiry, the search-tier and ADR 0014 refusals, audit-before-serve
by call ordering, fail-closed on a broken audit path, the key hierarchy (AAD binding, versions,
destruction, persistence, the unimplemented KMS, and the interface's method set), and the HTTP
surface including the edge-route rejection.

## NOT VERIFIED, and why

1. **Any cloud KMS.** `--key-backend kms` refuses to start without an explicit acknowledgement, and
   every method of `KMSKeyWrapper` returns `ErrNotImplemented`. Nothing in this package has been
   exercised against Azure Key Vault or Managed HSM: there is no network, no SDK and no credential
   on this host (TOOLCHAIN-DECISION.md §3). Modes 2 and 3 are *interfaces and refusals*, not
   working code.
2. **The `database/sql` plumbing.** `SQLStore` is written against the real schema, but no
   PostgreSQL wire driver is fetchable offline, so the statements are verified as *text* against the
   live database (`tools/live-schema-check.ps1`, 17 statements prepared; the ADR 0014 pair and the
   tier/mode rule exercised; the content-object lifecycle executed inside a rolled-back
   transaction) rather than through the driver. `--store sql` refuses to start in this build.
3. **The Go test that runs those statements in-process** (`internal/store/sql_integration_test.go`)
   skips *on this host* with the exact reason: the DSH file sandbox denies a child process the
   Docker named pipe (`npipe:////./pipe/dockerDesktopLinuxEngine`), so `go test` cannot reach the
   container. On a machine without that restriction it runs; here the harness above produces the
   same evidence from a shell that can reach it.
4. **Blob storage.** There is no blob store offline, so `Redeem` returns the object's reference and
   digests and — only if the binary wires `Options.FetchBlob` — the bytes. A deployment returns a
   short-lived storage URL and the content never transits the vault's response (docs/02 §11). The
   *authorisation* is identical either way; the *serving* half is a stand-in.
5. **The retrieval-grant SQL path is verified as text, not through a driver.**
   `ops.retrieval_grant` landed (database owner, T44/T45) with the two CHECKs this service asked
   for and a `retrieval_grant_single_use` trigger that refuses an unguarded UPDATE of a redeemed
   grant. The live-schema harness now exercises put, claim, a second guarded claim matching zero
   rows, and that trigger, all against the real table inside a rolled-back transaction. What is
   still not exercised is the `database/sql` plumbing around those statements (item 2).
6. **RLS as the second isolation layer.** The database owner's 43 assertions cover tenant isolation;
   this service sets the session tenant inside every transaction (`set_config(..., true)`) and the
   search statements now compare `tenant_id` to a bound parameter *as well*, but the vault's own
   tests do not re-prove RLS.

## Decisions this leaves open

- **§10.2 does not enumerate retrieval refusals.** The extra reasons above are a closed set in this
  package; if the design wants one closed set for both surfaces, §10.2 or the schema should carry
  them.
- **The retrieval grant is a row, and the audit row is separate.** `ops.retrieval_grant` carries
  the single-use claim (`used_at`/`used_by`, guarded by the claim statement and by db's trigger);
  `ops.audit` carries the record that the attempt happened, written before anything is served, for
  refusals and for searched-but-empty alike (§6.3).
- **`full_text` sets a plaintext-derived copy outside the key hierarchy** (§..6, §6.4). The service
  refuses the impossible custody pair, but nothing here changes the fact that a `full_text` tenant's
  prompt text is server-readable by design; the erasure path reaches it by row deletion because key
  destruction cannot.
