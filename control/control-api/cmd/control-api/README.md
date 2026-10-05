# cmd/control-api — the binary

`main.go` parses configuration, selects the certificate signer and the store, wires the services and
serves `/v1/enrol`, `/v1/token`, `/v1/health`, `/v1/policy`, `/v1/content/grant`,
`/internal/v1/content/finalise`, `/healthz` and `/readyz`. `enterprise.go` wires the rest on the same
listener — `/internal/v1/auth/*`, `/onboard/*`, `/.well-known/*`, `/admin/v1/*` and `/scim/v2` — each
part only when its settings below are present, with one startup log line for each part that is off.
`tenant.go` is the vendor operator's `control-api tenant create|invite`, and `sync_directory.go` the
lab's `control-api sync-directory`.

## Flags and environment

A flag wins over the environment, decided by asking the flag package which flags were actually passed,
so `--region ""` is a request rather than an absence.

| Setting | Flag | Environment | Notes |
|---|---|---|---|
| Listen address | `--addr` | `SAC_HTTP_ADDR` | the image sets `0.0.0.0:8080` |
| Store | `--store` | `SAC_STORE` | `memory` \| `sql` |
| Database | `--dsn`, `--driver`, `--pg-host`, `--pg-port`, `--pg-database` | `SAC_PG_HOST`, `SAC_PG_DATABASE` | DSN wins when set |
| Identity | `--role` | `SAC_ROLE` | `control-api` |
| Region | `--region` | `SAC_REGION` | empty disables only the region check |
| LocalCA (files) | `--ca-cert`, `--ca-key` | — | both or neither |
| LocalCA (PEM) | — | `SAC_CA_CERT_PEM`, `SAC_CA_KEY_PEM` | the container form |
| Access-token key | `--dpop-token-key-pem` | `SAC_DPOP_TOKEN_KEY_PEM` | PEM text **or** a file path; required |
| Token identity | `--token-issuer`, `--token-audience` | `SAC_TOKEN_ISSUER`, `SAC_TOKEN_AUDIENCE` | `iss`/`aud` |
| Credential life | `--credential-ttl` | `SAC_CREDENTIAL_TTL` | default 2160h (90 days) |
| Token life | — | `SAC_ENROLMENT_TOKEN_TTL` | operator-minting primitive only |
| SANs | `--sans` | — | comma-separated DNS names / IPs |
| Content vault | `--vault-url` | `SAC_VAULT_URL` | content-vault's internal base URL; empty disables content grants |
| Upload signing key | — | `SAC_UPLOAD_SIGNING_KEY` | signs the upload URL and authenticates the finalise call; at least 16 bytes; a secret, so no flag |
| Key Vault | — | `SAC_KEYVAULT_URI` | selects the KeyVaultSigner, which refuses clearly |
| Telemetry | — | `SAC_APPINSIGHTS` | read, validated, never logged, not exported |

### The enterprise surface (environment only)

| Environment | What it does |
|---|---|
| `SAC_AUTH_ISSUER` | Turns the identity service on: the exact `iss` of product tokens, which query-api and content-vault pin. Needs `--store sql`, `SAC_DIRECTORY_KEY` and `SAC_PUBLIC_URL`, or the binary refuses to start |
| `SAC_SESSION_SIGNING_KEY_FILE` | PEM file of the P-256 key that signs product tokens; further PEM blocks are published in the JWKS for rotation. Not the device token key |
| `SAC_INTERNAL_TOKEN` | The bearer the dashboard's server presents on `/internal/v1/auth/*`; at least 32 characters |
| `SAC_PUBLIC_URL` | The browser-facing origin: sign-in redirect URIs, onboarding links, the SCIM base URL (`{SAC_PUBLIC_URL}/scim/v2`) |
| `SAC_PUBLIC_DEVICE_ENDPOINT` | The device edge written into every tenant package as `SAC_DEVICE_ENDPOINT` |
| `SAC_DIRECTORY_KEY` | Base64 32-byte key that seals every `*_enc` column and the tenants' user-reference keys. Without it SCIM and `user_ref_key` are off |
| `SAC_AUTH_ALLOW_INSECURE_IDP` | `1` admits `http` issuers and private addresses for identity providers: the lab's stand-in only |
| `SAC_AUTH_REDIRECT_URIS` | Comma-separated exact redirect URIs sign-in accepts; empty accepts any absolute http(s) URI |
| `SAC_AUTH_TOKEN_TTL` | Product token life; default 5m, clamped to 10m |
| `SAC_ENTRA_CLIENT_ID` | The vendor's multi-tenant Entra application, for Entra sign-in, admin consent and the Intune check. Empty turns Entra off |
| `SAC_ENTRA_CLIENT_SECRET` | The app's client secret: the lab's credential only |
| `SAC_ENTRA_CERT_FILE` | A certificate whose key signs a client assertion |
| `SAC_ENTRA_FIC` | `managed`: present this container's managed identity token as a federated credential (the production choice; no secret anywhere) |
| `SAC_ENTRA_MI_CLIENT_ID` | Which user-assigned managed identity to use for `SAC_ENTRA_FIC`; empty uses `AZURE_CLIENT_ID` |
| `SAC_ENTRA_LOGIN_BASE` | The identity platform host; default `https://login.microsoftonline.com` (a stand-in in tests) |
| `SAC_GRAPH_URL` | Points the Intune check at a Graph stand-in; empty is Microsoft Graph |
| `SAC_POLICY_SIGNING_KEY_FILE` | The Ed25519 policy-bundle signing key; its public half is the trust anchor the generic MSI pins. Without it `GET /v1/policy` answers 503 |
| `SAC_POLICY_SIGNING_KEY_ID` | The key id bundles name; must equal the one the MSI pins; default `policy-key-1` |
| `SAC_POLICY_RECHECK` | How long a tenant's served bundle is reused before its inputs are re-read; default 30s, negative re-reads on every request |
| `SAC_AGENT_RELEASE_DIR` | The folder holding the generic `ShadowAICapture.msi` and `release.json` that packages are built from |
| `SAC_DEPLOYMENT_KEY_TTL` | Life of a newly minted deployment key; 0 (default) never expires, and revocation is the control |
| `SAC_DEPLOYMENT_KEY_RATE` | Sustained enrolments per second one deployment key may make; default 5, negative disables the limit |
| `SAC_DEPLOYMENT_KEY_BURST` | The bucket that absorbs a rollout wave per key; default 300 |
| `SAC_SCIM_POPULATION_ATTRIBUTE` | The SCIM attribute path copied to `ops.user_dim.population`; empty writes none |

## When content grants are on

The grant path needs three things: a vault to mint the object key (`--vault-url`), a database to decide
against (`--store sql`), and the upload signing key. With no vault URL it is disabled and says so at
startup; with the in-memory store it is disabled with a warning; with a vault URL and the SQL store but
no signing key the binary **refuses to start**, because an upload URL nobody can verify is an upload
path with no decision behind it. Disabled, both content routes answer 503.

## Why there are two probes

`/healthz` is liveness. `/readyz` opens a store transaction and reads `ops.tenant`; an unknown tenant
is a healthy database, so only a transport failure answers 503. A service holding a broken database
connection should be taken out of rotation, not killed.

## Why `-store sql` refuses in the default build

The default build has no PostgreSQL driver, because the gate runs with `GOPROXY=off` and an empty
module cache. Rather than start in memory while a deployment believes it is persisting, the binary
names the tagged build and the commands that verify it. The tagged binary links the driver in itself
and needs no `--driver`.
