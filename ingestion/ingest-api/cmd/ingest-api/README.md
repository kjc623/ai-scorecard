# cmd/ingest-api — the binary

The `main` package that wires [ingest-api](../../README.md) together and puts it on the network. It
owns process concerns only: configuration, the store and authenticator it selects, the listener, and
the two probe paths. Every validation decision lives in `internal/ingest`; this package cannot invent
an outcome the write path did not make.

## What it does

- Registers every flag and resolves each setting as **flag > environment > default**. Precedence asks
  the flag package which flags were actually *passed*, so `--region ""` is a request rather than an
  absence. Flag defaults are the laptop values (`127.0.0.1:8443`, `-store memory`); the deployment
  supplies environment instead, because the Container Apps module has no `command`/`args` parameter.
- Loads the contract schema (`--schema`, else discovered above the working directory) and derives the
  repository root from its own location, which is what lets the route-table default stay
  module-relative and work from any working directory.
- Selects the store: `memory` (a test double, loudly logged) or `sql`. `--store sql` in the default
  build **refuses to start** with a message naming the driver, the variables it read and the two
  commands that would make it real, rather than starting in memory while a deployment believes it is
  persisting.
- Selects the authenticator: the mTLS authenticator when TLS material is present,
  `-dev-trust-principal` when explicitly asked for, and a hard refusal otherwise — "refusing to serve
  without device authentication". The dev flag is loudly logged and is mutually exclusive with TLS
  material.
- Serves `POST /v1/events` and `/healthz` through `internal/httpapi`, and `/readyz` through its own
  wrapper.

## Configuration

The environment names are the deployment's names, and the agreement is asserted rather than assumed:
[`infra_agreement_test.go`](infra_agreement_test.go) reads `azure/main.bicep` and this package's own
source and fails if a name is passed to a process that never reads it or read without being accounted
for; `node localdev/tools/check-config-agreement.mjs` adds both Dockerfiles and the lab compose file.

| Setting | Flag | Environment |
|---|---|---|
| identity | `--role` | `SAC_ROLE` |
| database host / name | `--pg-host`, `--pg-database` | `SAC_PG_HOST`, `SAC_PG_DATABASE` |
| listen address | `--addr` | `SAC_HTTP_ADDR` (the image sets `0.0.0.0:8080`) |
| store mode | `--store` | `SAC_STORE` (the image sets `memory`) |
| contract schema | `--schema` | `SAC_SCHEMA` |
| route ranking | `--routes-file` | `SAC_ROUTES_FILE` |
| region | `--region` | `SAC_REGION` |
| blob endpoint (unused) | `--blob-ciphertext-endpoint` | `SAC_BLOB_CIPHERTEXT_ENDPOINT` |
| telemetry | — | `SAC_APPINSIGHTS` |
| server certificate / key / client CA | `--tls-cert`, `--tls-key`, `--tls-client-ca` | `SAC_TLS_CERT_PEM`, `SAC_TLS_KEY_PEM`, `SAC_TLS_CLIENT_CA_PEM` |

Two entries are deliberately inert. `SAC_BLOB_CIPHERTEXT_ENDPOINT` is read, validated and reported
unused, because the ingest path performs no blob I/O and a deployment parameter nobody reads is worse
than one read and declared inert. `SAC_APPINSIGHTS` is read, validated and never logged: it is a
credential, and this build exports no telemetry to it. `--region` is passed by nobody today, so §12
region pinning is inert in Azure — a gap the agreement test prints on every run rather than a silent
omission.

TLS material arrives either as three files on a laptop or as PEM in the environment in a container
(the Container Apps module has `keyVaultEnv` but no volume mount). A flag wins; a partial set is a
startup error rather than a handshake failure on the first device request. The listener is TLS 1.3
with `RequireAndVerifyClientCert`, and the per-device credential status is still re-checked inside the
write transaction.

## Probes

`probes.go` adds `/readyz` in front of the service handler and passes everything else through
untouched. It exists because `azure/modules/container-app.bicep` probes both `/healthz` (liveness) and
`/readyz` (readiness), and before it the second path 404'd — so a container built from this binary
would have stayed unready forever. Readiness asks a real dependency: the store reads
`ref.route_fidelity`, a query in SQL mode and a state check in memory mode, and answers 503 when it
fails. The failure reason is logged, not returned, because a dependency error can carry a hostname.

## Building and running

```powershell
# default build: standard library only, what CI and the acceptance gate run
go build ./... && go test ./...

# tagged build: the real PostgreSQL driver (see ../sqlpg/README.md)
go build -tags sac_sql_driver -o ingest-api-sql ./cmd/ingest-api

# local run against the in-memory store
go build -o ingest-api.exe ./cmd/ingest-api
.\ingest-api.exe -addr 127.0.0.1:18449 -store memory -dev-trust-principal `
  -dev-seed-principal "11111111-1111-1111-1111-111111111111:22222222-2222-2222-2222-222222222222"
```

Then POST with `X-Dev-Tenant-Id` / `X-Dev-Device-Id`. `-dev-seed-principal` is refused with
`-store sql`, where the principal comes from `ops.*` instead.

## Not verified

The `database/sql` path is only reachable under the `sac_sql_driver` tag, and the region check is a
string comparison. Both are stated in full in [../../README.md](../../README.md#not-verified).
