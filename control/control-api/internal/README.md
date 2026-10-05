# internal/ — the service's seams

| Package | What it is |
|---|---|
| `store` | the persistence seam: an interface, the in-memory double, and the SQL implementation whose statement text the tagged test executes |
| `signer` | the `CertificateSigner` interface, the development `LocalCA`, and the `KeyVaultSigner` that refuses clearly until the vault client lands |
| `enrol` | `POST /v1/enrol`: enrolment-token, deployment-key or credential validation, the per-key rate limit and the Intune check (`deployment.go`), region pin, C11 idempotency, credential issuance, and the tenant's `user_ref_key` in the response |
| `token` | `POST /v1/token`: RFC 7523 assertion + RFC 9449 proof, and the ES256 `at+jwt` it issues; the `Verifier` is exposed for re-enrolment |
| `jose` | the stdlib-only compact JWS / EC JWK subset shared by the proof and the token |
| `dpop` | the RFC 9449 proof verifier, including `htu` reconstruction behind a gateway |
| `content` | `POST /v1/content/grant` and the finaliser: the grant decision over server-side state, the signed upload URL, the HTTP client for content-vault, and its own SQL statements (`sql.go`). It has a SQL store only |
| `httpapi` | the HTTP transport and §5's common error envelope. It also resolves the current credential a request presents; a certificate forwarded by the edge arrives percent-encoded in `X-Client-Cert` and is decoded here |
| `policyserve` | `GET /v1/policy` and the writer of `ops.policy_bundle`: composes a tenant's bundle from the database, signs it with the vendor's Ed25519 key, stores the exact envelope, and serves it with `ETag`/`304`/`404` |
| `session` | the product's token authority: the ES256 product access token and its verifier, the JWKS and discovery document under `/.well-known/`, and the server-side session store over `ops.auth_session` (8 h maximum, 1 h idle) |
| `identity` | the OIDC relying party for Entra (the vendor's multi-tenant app; the token's own `tid` decides the connection) and any OIDC provider (found by email domain, exact issuer and client id); PKCE, state and nonce in `ops.auth_signin`; roles from `role_map` and `ops.role_grant`; the internal sign-in API the dashboard's server calls; re-mint checks of connection, SCIM status and provider refresh |
| `onboard` | the `/onboard/*` pages (Entra admin consent with a consent probe, or the OIDC provider form) that create a pending connection, and the vendor's `tenant create` / `tenant invite` writes |
| `entraapp` | the vendor's Entra application as a client: exactly one credential (secret for the lab, certificate, or managed identity as a federated credential) and app-only tokens in a customer's tenant |
| `intune` | the Graph `managedDevices` check a deployment-key enrolment passes when the tenant requires Intune verification; its refusals carry a reason |
| `scim` | the SCIM 2.0 service provider at `/scim/v2` (Users, Groups, discovery, filters, PATCH as Entra and Okta send it), the `sacscim_` bearer, canonical `user_ref` and aliases, retire-not-delete, and the `ops.user_dim` row |
| `deploy` | the admin API behind Settings → Deployment (summary, package download minting a deployment key, key revocation, verification setting, SCIM tokens) and the package builders: `.zip` and Microsoft's `.intunewin` format |
| `directory` | the `Cipher` that seals every `*_enc` column under per-tenant keys derived from `SAC_DIRECTORY_KEY`, the tenants' user-reference keys (`userrefkey.go`), and the lab's file-based directory sync |

The dependency direction is one way: `httpapi` calls `enrol`, `token`, `content`, `policyserve` and
`deploy`'s routes; none knows the others; `enrol` and `token` call `store` and `dpop`; `enrol` calls
`intune`, whose token source is an interface `entraapp` satisfies without either importing the other;
`deploy` calls `enrol` (to mint keys) and `store`; `signer` is consumed only by `enrol`; `content` has its
own store interface and reaches the vault over HTTP. On the identity side `onboard` calls `identity`,
which calls `session` and `directory`; `scim` calls `directory` and `store`. `cmd/control-api/enterprise.go`
assembles them. The device-side shapes come from
`github.com/shadow-ai-capture/device/protocol` and are consumed, never forked.
