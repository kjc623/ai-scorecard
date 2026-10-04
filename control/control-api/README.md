# control-api — device enrolment, DPoP token issuance and content grants

The device-facing control plane ([ADR 0020](../../docs/adr/0020-device-transport-is-application-gateway-with-a-pluggable-authenticator.md)
decisions 3 and 4; [docs/02-ingest-and-transport.md](../../docs/02-ingest-and-transport.md) §5.1,
§5.2, §5.5, §10). It turns a bootstrap credential into a per-device credential and a device credential
into a short-lived, sender-constrained access token, and it decides whether one event's content may be
uploaded. Go, standard library only in the default build.

## The endpoints, exactly

**`POST /v1/enrol`** takes `protocol.EnrolmentRequest` and answers with `protocol.EnrolmentResponse`,
both consumed from `endpoint/protocol` and never re-declared. It is mode-agnostic:

| Mode | The device sends | The service does |
|---|---|---|
| `x509` | a PKCS#10 CSR (PEM) | signs a leaf through the `CertificateSigner` with CN=device_id, OU=tenant_id, EKU=clientAuth and the configured validity/SANs, and inserts an `x509` credential |
| `dpop` | a public EC P-256 JWK and a proof of possession | verifies the proof, computes the RFC 7638 thumbprint and inserts a `dpop` credential with `public_key_jwk` and `public_key_thumbprint` |

The tenant always comes from the enrolment token or the current credential, **never the body**
(§12); the region pin is checked and fails closed; and `hardware_identity_hash` is the C11
idempotency key, so a re-image returns the existing `device_id` with `reenrolled: true` and a revoked
device is refused a fresh identity. The token is marked used only after the credential commits, so a
failure is retryable with the same token. Re-enrolment for rotation presents the **current** credential
(the forwarded certificate for `x509`, or the access token plus a fresh DPoP proof for `dpop`) instead
of a token.

**`POST /v1/token`** takes `protocol.TokenRequest` and answers with `protocol.TokenResponse`. It is
RFC 7523 plus RFC 9449: the `assertion` is a compact ES256 JWS signed by the device's registered key
whose `sub` is `device_id` and whose `tenant_id` names the tenant the credential lives in; the `DPoP`
header is a proof bound to `htm`/`htu` with a `jti`. Both are verified against the registered
credential and the proof key must equal the registered key. The issued
access token is DPoP-bound and carries no bearer fallback. `token_type` is always `DPoP`.

**`POST /v1/content/grant`** takes `protocol.ContentGrantRequest` and answers with
`protocol.ContentGrantResponse` (`endpoint/protocol/content.go`). The device authenticates with its
current credential — the forwarded certificate for `x509`, or the access token plus a DPoP proof for
`dpop` — and the tenant and device come from that credential, never the body. The decision
(`internal/content`) reads server-side state only:

| Outcome | When |
|---|---|
| `404 unknown_event` | the device has no such observation; another device's event is not found |
| `422 mode_violation` | the event is not an M3 prompt, so there is no content to upload |
| `409 grant_consumed` | the submission's content is already `uploaded` or `shredded` |
| `413 oversize` | the declared object is over the per-object cap (64 MiB) |
| `200` `denied` | `mode_not_permitted` (tenant ceiling below M3), `retention_expired`, or `over_budget` (`content_budget_bytes_per_day`, which defaults to 0) |
| `200` `granted` | one upload URL, the object key content-vault minted, and the metadata headers the device repeats on the upload |

A denial is terminal for the event and is returned again on a repeat request; a live grant is returned
again with the same `grant_id` and upload URL. The upload URL is signed with a key shared with the
storage layer (one object, one grant, an expiry of at most 15 minutes) — it is **not** a storage
user-delegation SAS, which this build does not mint. The first grant request for an event moves the
submission's `content_state` to `local_only`. The route answers 503 unless a vault URL, the SQL store
and the signing key are all configured.

**`POST /internal/v1/content/finalise`** is the finaliser (§10.4), called by the storage layer when an
upload lands and authenticated by an HMAC of the body under the same signing key. It is not a device
route and the edge does not forward it. The object is promoted only if the grant is live and names
this object and event, the bytes the store measured are the bytes the device declared, and the event
has no content yet; then content-vault records the object, the submission moves to `uploaded`, and the
bytes are added to `ops.usage_daily.content_bytes_added`. A mismatch voids the grant and answers 422,
which tells the storage layer to delete the staged bytes.

`/healthz` and `/readyz` are deployment infrastructure, not device APIs. `/readyz` opens a store
transaction and reads `ops.tenant`, so a broken database answers 503 while liveness stays a restart
signal.

## The token and proof on the wire

The access token is a compact JWS with exactly this header:

```json
{"alg":"ES256","typ":"at+jwt","kid":"<RFC 7638 thumbprint of the signing key>"}
```

and these claims: `iss`, `aud`, `sub` = device_id, `tenant_id`, `device_id`, `credential_id`,
`cnf:{"jkt":"<proof jwk thumbprint>"}`, `iat`, `exp` (default 900 s), `jti`.

The DPoP proof is a compact JWS with header:

```json
{"typ":"dpop+jwt","alg":"ES256","jwk":{...}}
```

and claims `htm`, `htu`, `iat`, `jti` (and `ath` = base64url(sha256(ascii(access-token))) when a
request carries an access token). The token endpoint's proof has no access token yet, so it carries no
`ath`; a resource request — and a `dpop` re-enrolment — carries one, and the verifier requires it.

## Configuration and containers

Where a setting has both a flag and an environment variable, **a flag wins**. Deployment vocabulary is
`SAC_*`: `SAC_ROLE`, `SAC_PG_HOST`, `SAC_PG_DATABASE`, `SAC_KEYVAULT_URI` and `SAC_APPINSIGHTS` are
passed by `azure/main.bicep`; `SAC_HTTP_ADDR`, `SAC_STORE`, `SAC_CREDENTIAL_TTL` and
`SAC_ENROLMENT_TOKEN_TTL` are image defaults; `SAC_REGION`, `SAC_CA_CERT_PEM`, `SAC_CA_KEY_PEM`,
`SAC_TOKEN_ISSUER`, `SAC_TOKEN_AUDIENCE`, `SAC_DPOP_TOKEN_KEY_PEM`, `SAC_VAULT_URL` and
`SAC_UPLOAD_SIGNING_KEY` are documented deployment gaps (the binary reads them, no deployment passes
them yet; the local auth lab does).
[`cmd/control-api/infra_agreement_test.go`](cmd/control-api/infra_agreement_test.go) fails if the two
directions disagree.

The binary **refuses to start without `SAC_DPOP_TOKEN_KEY_PEM`** (or `--dpop-token-key-pem`): an
endpoint that issued tokens under an unstated key would be a different authority from the one the
deployment configured. Certificate signing selects the `KeyVaultSigner` when `SAC_KEYVAULT_URI` is
set — which refuses clearly until this build carries a vault client — and otherwise a `LocalCA` that
loads `SAC_CA_CERT_PEM`/`SAC_CA_KEY_PEM` or, if neither is set, generates a fresh development CA.

The default build carries no PostgreSQL driver. `-store sql` then refuses with an actionable message;
the driver is linked only under the `sac_sql_driver` tag (see [sqlpg](sqlpg/README.md)), exactly as
ingest-api does.

## The integration seam

Every SQL statement is a constant in `internal/store/sql.go`, and the tagged test under `sac_sql_driver`
prepares the exact text against a reachable PostgreSQL and drives the store through the real driver.
The order is: set the transaction-local RLS tenant; resolve the token or credential; reconcile the
device by hardware identity; revoke the previous credential and insert the new one in one transaction;
mark the token used. Nothing in the request path re-checks identity with a
read-then-write: single-use is a conditional `UPDATE ... WHERE used_at IS NULL` whose zero-row result
is the reuse.

## What is not built yet

1. **The Key Vault signer is an interface, not an implementation.** `KeyVaultSigner.Sign` returns a
   clear "not configured" error. It is deliberately not faked with a local key.
2. **`/v1/policy` and `/v1/health`** are named in §5.2 and §5.4 and are not implemented here. The
   policy bundle has no writer in this build, which is why `ops.policy_bundle.signed_digest` remains
   the schema checker's one excused digest column — this service does not write that table.
3. **Content grants (§5.5, §10) are built for the lab, not for a deployment.** What differs from the
   design: `not_policy_relevant` is never produced, because the tenant retention criteria it is decided
   on have no stored form, so every M3 event is treated as relevant; `410` is not returned — a request
   after an expired grant is decided afresh, and no job sweeps expired or voided grants; the upload
   credential is an HMAC-signed URL, not a SAS; and the grant path needs three database grants the
   schema does not give `sac_control` (a read of `ingest.observation`, the update of
   `ingest.submission.content_state`, and the write of `ops.usage_daily`). The lab connects as the
   database owner, so none of the three is exercised there. Content grants have only a SQL store: under
   `-store memory` they are disabled. There is no unique index on `(tenant_id, event_id)` for live
   grants, so two concurrent first requests for one event could both be granted.
4. **The enrolment token is minted out of band.** The request path verifies and redeems a token; the
   MDM provisioning path that mints one (`enrol.MintEnrolmentToken` is the primitive) is not wired to
   an operator surface.
5. **Certificate rotation overlap is not implemented.** A rotation revokes the previous credential in
   the same transaction that inserts the new one; §2.2's seven-day overlap where both are accepted is
   not.
6. **The DPoP `jti` is verified present but not persisted for replay.** `ops.dpop_replay` exists and is
   granted to ingest-api, but this build does not grant it to `sac_control` and does not write a jti;
   a proof's `iat` window is the only replay bound. Wiring the window is a schema grant and a scheduled
   sweep, and was left out so the enrolment token is the only schema change in this slice.
7. **Region pinning is inert in Azure** until `SAC_REGION` is passed.

## Running the checks

```
GOPROXY=off GOFLAGS=-mod=mod GOTOOLCHAIN=local go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1
node database/tools/check-schema.mjs
```

The live-schema test needs a reachable PostgreSQL with `database/schema.sql` applied and is built only
under `sac_sql_driver`; it skips loudly when none is reachable, because a skip with a reason is honest
and a red gate on a machine without a database is not:

```
go test -tags sac_sql_driver ./sqlpg/ -v
```

## Test map

The claims above are held by: `internal/enrol` (x509 issues a verifiable leaf; dpop stores the JWK and
thumbprint; possession is required; re-enrolment is idempotent; a revoked device is refused; the token
is single-use; the region mismatch fails closed); `internal/token` (a verifiable `at+jwt` is issued and
bound to the proof's key; a bad assertion, a bad proof and a wrong `htu` are
refused); `internal/httpapi` (the endpoints and the common error envelope end to end, including
re-enrolment with an access token); `internal/signer` (a fresh CA per call, a verifiable client leaf,
and the Key Vault refusal); `internal/content` (a granted decision, the three refusals, the three
denials and that a denial is terminal and mints no key, and the finaliser's digest, object and
second-write rejections, all against a fake store and a fake vault); and `internal/store/memory.go`
mirrors the SQL the tagged test executes. The grant path's SQL (`internal/content/sql.go`) and its HTTP
handlers have no test of their own: they were exercised end to end in the local auth lab, with a
Windows device at M3, and by nothing else.
