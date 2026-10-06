# content-vault

Stores prompt content (collection mode M3) encrypted in PostgreSQL, and is the only component that
can decrypt it. It has **internal ingress only**: no device, browser or edge route reaches it.

## How it works

- **Upload.** A device uploads a granted content object to control-api, which checks the grant and
  forwards the body to `PUT /internal/v1/tenants/{tenant_id}/content/{event_id}` with headers
  `X-Sac-Grant-Id`, `X-Sac-Raw-Digest` (`sha256:<hex>` of the body) and its service token. In one
  transaction the vault claims the grant (`ops.grant.used_at`), encrypts the body, inserts
  `ops.content` with the tenant's content retention, writes `ingest.search_text` as the tenant's
  `content_search` tier allows (typed prompt text at `full_text`, attachment names at
  `attachment_names` and above, nothing for a `client_generated` request), and writes `ops.audit`.
  Repeating an upload already stored under the same grant with the same digest answers 200.
- **Retrieval.** query-api forwards an analyst's `POST /v1/content/retrieval` with their token
  (role `content_reader`). The vault records a single-use `ops.retrieval_grant` and answers with a
  five-minute URL, `GET /v1/content/retrieval/{tenant_id}/{grant_id}`, which the dashboard server
  forwards from the browser. Redeeming it claims the grant, decrypts and audits in one transaction.
- **Search.** query-api forwards `POST /v1/content-search` (role `analyst` or `content_reader`). The
  tier comes from `ops.tenant.content_search`; the audit row commits with the results.

Every read is audited in the transaction that serves it. Content past its retention, or sealed
under a key version the keyring no longer holds, is answered as `no_longer_available`.

## Authentication

Bearer JWTs signed ES256 by `SAC_AUTH_ISSUER` (control-api), verified against its JWKS, audience
`sac-vault`. Upload needs a service token with `"svc": "control-api"`; search and retrieval need a
person's token (`sac_tenant`, `actor`, `roles`). The retrieval URL needs no token: the single-use,
short-lived grant it names is the credential.

## Encryption

`SAC_CONTENT_KEYS` is `v2:<base64 32 bytes>,v1:<base64 32 bytes>` (Key Vault secret
`sac-content-keys`). The first entry encrypts; all decrypt, so a key is rotated by prepending a new
version and retired once no `ops.content.key_version` names it. Per tenant, the key is
HKDF-SHA256(master, no salt, info `sac/content/v1/<tenant_id>`); content is AES-256-GCM with a
random 12-byte nonce prefix and additional data `<tenant_id>/<object_id>`.

## Configuration

| Variable | Required | Meaning |
|---|---|---|
| `SAC_HTTP_ADDR` | no | Listen address; `127.0.0.1:8080` by default, `0.0.0.0:8080` in the image |
| `SAC_PG_HOST`, `SAC_PG_DATABASE`, `SAC_PG_USER` | yes | Database; the user is `content-vault` (role `sac_vault`) |
| `SAC_PG_PORT`, `SAC_PG_SSLMODE`, `SAC_PG_PASSWORD` | no | 5432, `require`; with no password each connection uses the managed identity's Entra token |
| `SAC_CONTENT_KEYS` | yes | The keyring; startup fails if a key is not 32 bytes or a version repeats |
| `SAC_AUTH_ISSUER` | yes | Token issuer, compared exactly |
| `SAC_AUTH_AUDIENCE` | no | `sac-vault` |
| `SAC_AUTH_JWKS_URL` | no | `<issuer>/.well-known/jwks.json` |
| `SAC_RETRIEVAL_URL_BASE` | no | Origin minted retrieval URLs start with; empty mints a path |

`GET /healthz` is liveness; `GET /readyz` makes a database round trip. Logs are JSON on stdout.

## Build and test

```sh
go build ./... && go vet ./... && go test ./...
docker build -f vault/content-vault/Dockerfile -t content-vault .   # from the repository root
```

The tests in `internal/store` run against a real database only when `SAC_TEST_PG_DSN` names one
(`postgres://…`) with `database/schema.sql` applied; they create a random tenant and run the
vault's statements as `sac_vault`.
