# ingest-api

The device event write path. Devices post batches to `POST /v1/events` through Application
Gateway; ingest-api authenticates the device, validates every envelope against the contract
(`contracts/event-envelope.schema.json`, embedded through `github.com/shadow-ai-capture/contracts`),
and records the batch in PostgreSQL with `ingest.record_event()`. Collectors hold no database
credential.

## How it works

- **Authentication.** Application Gateway terminates the device's TLS connection and forwards the
  leaf certificate in `X-Client-Cert` (URL-encoded PEM). ingest-api verifies the chain against the
  device CA (client-auth usage), takes the device id from the subject CN and the tenant id from the
  OU, derives the credential id from the certificate, and reads the tenant, device and credential
  status on every request. A tenant whose `residency_region` is not `SAC_REGION` is refused.
- **Validation.** Each envelope is checked in memory before the transaction opens: schema version,
  kind registry, then the JSON Schema (formats asserted). A failure is that event's rejection —
  `mode_violation` for a collection-mode rule, otherwise `schema_violation`, with a JSON Pointer and
  the violated constraint, never the value. The body's tenant and device must match the certificate.
- **Write.** One transaction per batch: the credential is re-checked, each accepted envelope goes to
  `ingest.record_event()` exactly as sent (it decides inserted / merged / duplicate), rejections are
  quarantined content-stripped in `ingest.rejected`, and `ops.device.last_seen_at` moves forward.
  Idempotency is the database's per-event key, so a retried batch reports `duplicate`.
- **Responses.** A batch that parses returns 200 with one result per event. Batch-level failures use
  `{"error":{"code","detail","server_time","message"}}`: 400/413 for the batch's shape and size caps
  (8 MiB compressed, 32 MiB decompressed, 256 KiB per envelope, 1–500 events), 401/403 for
  authentication, 503 (with `Retry-After`) when nothing could be committed.

`GET /healthz` is liveness; `GET /readyz` makes a database round trip.

## Configuration

| Variable | Meaning |
|---|---|
| `SAC_HTTP_ADDR` | Listen address. Default `127.0.0.1:8080`; the image sets `0.0.0.0:8080` |
| `SAC_REGION` | Required. The Azure location this deployment serves, e.g. `eastus` |
| `SAC_CA_CERT_PEM` | Required. The device CA certificate (Key Vault secret `sac-device-ca-cert`) |
| `SAC_PG_HOST`, `SAC_PG_DATABASE` | Required. The PostgreSQL server and database |
| `SAC_PG_USER` | Required. `ingest-api`, the login mapped to the managed identity (role `sac_ingest`) |
| `SAC_PG_PORT`, `SAC_PG_SSLMODE` | Default `5432` and `require` |
| `SAC_PG_PASSWORD` | Local lab only. Without it each connection uses an Entra token for the managed identity (`AZURE_CLIENT_ID`) |

Logs are JSON on stdout.

## Build and test

```sh
docker build -f ingestion/ingest-api/Dockerfile -t ingest-api .   # from the repository root
cd ingestion/ingest-api
gofmt -l . && go vet ./... && go test ./...
SAC_TEST_PG_DSN='postgres://user:pass@host:port/db?sslmode=disable' go test ./internal/store/
```

The store tests against a live database run only when `SAC_TEST_PG_DSN` names one with
`database/schema.sql` applied; they create and delete their own tenant and connect as `sac_ingest`.
