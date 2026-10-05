# control-api — the control plane and the product's identity service

The device-facing control plane ([ADR 0020](../../docs/adr/0020-device-transport-is-application-gateway-with-a-pluggable-authenticator.md)
decisions 3 and 4; [docs/02-ingest-and-transport.md](../../docs/02-ingest-and-transport.md) §5.1,
§5.2, §5.5, §10). It turns a bootstrap credential into a per-device credential and a device credential
into a short-lived, sender-constrained access token, serves each tenant's signed policy bundle, and
decides whether one event's content may be uploaded.

It is also the product's one identity service ([docs/06](../../docs/06-security-and-threat-model.md)
§4.1): the OpenID Connect relying party for every customer identity provider, the keeper of sign-in
sessions, the issuer of the short-lived product access tokens `query-api`, `content-vault` and its own
admin API verify, the onboarding pages a customer's admin links their provider through, the SCIM 2.0
endpoint people are provisioned by, and the admin API behind Settings → Deployment. Each of those is
off until its settings are present and says so at startup ([cmd/control-api](cmd/control-api/README.md)
lists them). Go, standard library only in the default build.

## The endpoints, exactly

**`POST /v1/enrol`** takes `protocol.EnrolmentRequest` and answers with `protocol.EnrolmentResponse`,
both consumed from `endpoint/protocol` and never re-declared. It is mode-agnostic:

| Mode | The device sends | The service does |
|---|---|---|
| `x509` | a PKCS#10 CSR (PEM) | signs a leaf through the `CertificateSigner` with CN=device_id, OU=tenant_id, EKU=clientAuth and the configured validity/SANs, and inserts an `x509` credential |
| `dpop` | a public EC P-256 JWK and a proof of possession | verifies the proof, computes the RFC 7638 thumbprint and inserts a `dpop` credential with `public_key_jwk` and `public_key_thumbprint` |

The bootstrap is one of three: a single-use enrolment token (the lab's), a per-tenant **deployment
key** (`sacdk_<tenant>.<secret>`, the one a tenant package carries; `internal/enrol/deployment.go`), or
the current credential. A deployment key is reusable, so it is stored only as its hash, can be revoked
or expire, and is rate-limited per key (default 5/s, burst 300); for a tenant whose
`device_verification` is `intune` the request must also carry `attestation`, which `internal/intune`
checks against the customer's Intune through Microsoft Graph, refusing with `403 device_not_managed`
and a `detail.reason`, and one Intune device id binds to one product device. The response carries the
tenant's `user_ref_key`, minted and sealed on first need.

The tenant always comes from the enrolment token, the deployment key or the current credential,
**never the body** (§12); the region pin is checked and fails closed; and `hardware_identity_hash` is the C11
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

**`GET /v1/policy`** (§5.2, `internal/policyserve`) authenticates the device exactly as `/v1/health`
does and answers `protocol.PolicyResponse`: the signed envelope byte for byte, with an `ETag`, or `304`
for a matching `If-None-Match`. The bundle is composed from the tenant's ceiling, the tool catalogue's
TLS hosts and the servable classifier release, signed with the Ed25519 key in
`SAC_POLICY_SIGNING_KEY_FILE`, and stored in `ops.policy_bundle` with `signed_envelope`; a new version
is minted only when that composition or the key changes. `404 no_policy_bundle` when no classifier
release is servable and none was ever minted (the device stays at M0); `503` with no signing key.

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

## Sign-in, onboarding and the admin API

Wired in [`cmd/control-api/enterprise.go`](cmd/control-api/enterprise.go); none of it is a device route.

| Route | Who calls it | What it does |
|---|---|---|
| `POST /internal/v1/auth/begin`, `complete`, `token`, `revoke` | the dashboard's server, with `Authorization: Bearer <SAC_INTERNAL_TOKEN>` | `internal/identity`: begin a sign-in (by work-email domain, or `provider: entra` for the Microsoft button), complete it into an opaque session id and a product token, re-mint a token for a live session, end a session. Refusals are `no_sso_connection`, `tenant_not_onboarded`, `no_role`, `connection_disabled`, `user_deactivated`, `session_ended` and a few "start again" codes |
| `GET /.well-known/jwks.json`, `/.well-known/openid-configuration` | `query-api`, `content-vault` | `internal/session`: the product token issuer's keys (two during a rotation) |
| `/onboard/*` | a customer's admin, through the dashboard's origin | `internal/onboard`: the one-time invite page, Entra admin consent (bound to the invite and a same-browser cookie, then proved by an app-only token in the consenting tenant), or the OIDC provider form (validated by discovery). Either creates a **pending** connection; the first successful sign-in through it activates it and makes that person admin |
| `/admin/v1/deployment`, `…/package`, `…/keys/{id}/revoke`, `…/verification`, `/admin/v1/scim/tokens…` | the dashboard's server, with the person's product token (audience `sac-control`, role `admin`) | `internal/deploy`: the Settings → Deployment summary; a tenant package (`.intunewin` or `.zip`, the generic MSI from `SAC_AGENT_RELEASE_DIR` plus `ShadowAICapture.tenant.env`), minting a new deployment key per download; key revocation; the Intune verification switch (only with an active Entra connection); SCIM tokens, shown once. Every write is audited with the token's actor |
| `/scim/v2/*` | the customer's identity provider, with a `sacscim_` bearer | `internal/scim`, below |

Sign-in: PKCE, state and nonce are generated here and held in `ops.auth_signin` for ten minutes. An
Entra token's issuer must be `https://login.microsoftonline.com/{tid}/v2.0` for its own `tid`, and that
`tid` must map to an active `ops.identity_connection`; an OIDC connection is chosen by the email domain
the vendor registered and pinned to its exact issuer and client id. Roles are the connection's
`role_map` over the provider's roles claim, united with `ops.role_grant`; no role is a refusal. The
session lives in `ops.auth_session` (8 h at most, 1 h idle); each re-mint re-checks the connection, the
person's SCIM `active` flag and, at most every 30 minutes, the provider's refresh token. The product
token is ES256 under `SAC_SESSION_SIGNING_KEY_FILE`, at most ten minutes, audiences `sac-query`,
`sac-vault` and `sac-control`, with `sac_tenant`, `actor`, `roles`, `idp` and `sid`.

The vendor operator creates customers from the command line, never over HTTP, because nothing a
customer can reach may create a tenant or name its email domains:

```
control-api tenant create --dsn "$SAC_PG_DSN" --name "Contoso" --region eu-west --key-custody vendor --ceiling m1
control-api tenant invite --dsn "$SAC_PG_DSN" --tenant <id> --domain contoso.com [--domain …] [--expires 168h] [--public-url https://…]
```

`create` prints the tenant id; `invite` writes the domains and prints the one-time onboarding URL,
whose hash is all that is stored.

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
them yet; the local auth lab does). The enterprise settings (`SAC_AUTH_ISSUER`, the session and policy
keys, the Entra application, deployment-key limits and the rest) are environment-only and listed one per
line in [cmd/control-api](cmd/control-api/README.md).
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

## People: SCIM, and the lab's directory file

A customer's people reach `ops.user_dim` one way: their identity provider pushes them to
**`/scim/v2`** (`internal/scim`, SCIM 2.0, built to what Entra's provisioning service and Okta actually
send). The bearer is a per-tenant `sacscim_<tenant>.<secret>` token an admin mints on the deployment
page; only its hash is stored, and the tenant the database resolves for it must equal the one in its
prefix. There is **no pull from Microsoft Graph**: the Entra application asks for no directory-read
permission, and the Graph source this package once had is gone.

A person is keyed the way devices key them ([docs/03](../../docs/03-data-platform.md) §3.3): the
canonical `user_ref` is `protocol.DeriveUserRef(upn, userName)` under the tenant's user-reference key,
fixed when the user is created. Every upn-ref the person has held, and the oid-ref of a GUID
`externalId`, go in `ops.user_ref_alias`, which `ingest.record_event` resolves before it stores an
event. The resource is stored sealed, with keyed hashes for the provider's `userName`/`externalId`
filters. `DELETE` retires (inactive, still readable), never deletes, and deactivation ends the
person's sessions at their next token re-mint. `ops.user_dim` gets the department from the enterprise
extension, a population from `SAC_SCIM_POPULATION_ATTRIBUTE` (none by default), the display name only
while the tenant's `device_identity` is `clear` (ADR 0021), and `directory_object_id_enc` sealed with
AES-256-GCM under a per-tenant key derived from `SAC_DIRECTORY_KEY`. Groups are stored and not yet
mapped to roles. For Entra, the customer provisions through a separate non-gallery enterprise
application pointed at the SCIM base URL until the product is in the Entra app gallery, and maps
`objectId` to `externalId`. The package comment in [internal/scim](internal/scim/doc.go) is the
reference.

The lab's sample tenant has no identity provider pushing to it, so `control-api sync-directory` loads
a JSON directory export into `ops.user_dim` (`-provider file`, the only provider):

```
control-api sync-directory -store sql -dsn "$SAC_PG_DSN" \
  -file backlog/06-directory-sync/sample-directory.json -tenant <uuid> -directory-key <base64-32b>
```

Without `-tenant` it syncs every tenant the session can see, which a role constrained by forced
row-level security cannot enumerate (the same caveat as the aggregator: pass `-tenant`, or run it as
a role that can).

## What is not built yet

1. **The Key Vault signer is an interface, not an implementation.** `KeyVaultSigner.Sign` returns a
   clear "not configured" error. It is deliberately not faked with a local key.
2. **The policy bundle carries no `not_after` and no `upgrade_required`.** `GET /v1/policy` is built
   and is the writer of `ops.policy_bundle`; it composes on the read path, because no settings writer
   exists yet, so a changed input reaches devices at the first poll after it (within
   `SAC_POLICY_RECHECK`, 30 s by default).
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
4. **The single-use enrolment token is minted out of band.** The request path verifies and redeems a
   token, and the lab seeds one per install (`enrol.MintEnrolmentToken` is the primitive); nothing
   customer-facing mints one. A customer's fleet enrols with deployment keys, which the package
   download mints.
5. **Certificate rotation overlap is not implemented.** A rotation revokes the previous credential in
   the same transaction that inserts the new one; §2.2's seven-day overlap where both are accepted is
   not.
6. **The DPoP `jti` is verified present but not persisted for replay.** `ops.dpop_replay` exists and is
   granted to ingest-api, but this build does not grant it to `sac_control` and does not write a jti;
   a proof's `iat` window is the only replay bound. Wiring the window is a schema grant and a scheduled
   sweep, and was left out so the enrolment token is the only schema change in this slice.
7. **Region pinning is inert in Azure** until `SAC_REGION` is passed.
8. **The dashboard authenticates to `/internal/v1/auth/*` with a shared secret** (`SAC_INTERNAL_TOKEN`,
   at least 32 characters), not yet with its managed identity. A product token, once minted, is not
   revocable before it expires (at most ten minutes): verifiers check its signature and lifetime, not
   the session.

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

The identity and deployment surface is held by its own packages' tests: `internal/session`,
`internal/identity`, `internal/onboard`, `internal/entraapp`, `internal/scim` (with suites written to
Entra's and Okta's provisioning behaviour), `internal/deploy`, `internal/intune`, `internal/policyserve`
and `internal/enrol/deployment_test.go`. `internal/session`, `identity`, `onboard`, `scim` and
`directory` also have live-database tests under `sac_sql_driver`.
