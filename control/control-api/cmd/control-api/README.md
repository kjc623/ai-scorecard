# cmd/control-api — the binary

`main.go` parses configuration, selects the certificate signer and the store, wires the services and
serves `/v1/enrol`, `/v1/token`, `/v1/content/grant`, `/internal/v1/content/finalise`, `/healthz` and
`/readyz`.

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
