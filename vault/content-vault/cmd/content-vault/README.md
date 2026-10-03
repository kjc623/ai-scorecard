# cmd/content-vault — the binary

The `main` package that wires [content-vault](../../README.md) together and puts it on the network. It
owns process concerns — configuration, the key backend, the store, the listener and the probes — and no
authorisation logic, which lives in `internal/vault`.

## Subcommands

| Subcommand | What it does |
|---|---|
| `serve` | Runs the internal HTTP surface. This is what `CMD ["serve"]` in the image invokes |
| `schema-sql` | Prints every SQL statement the service issues as a psql script (`--out FILE` to write one) |
| `version` | Prints the build's identity |

`schema-sql` is not a convenience: it is how `tools/live-schema-check.ps1` executes the **same text** the
service would send, rather than a copy of it. That is what makes the harness evidence instead of
documentation. Statements the schema does not yet have are emitted commented out, with the reason.

## What `serve` decides, in order

1. Resolves every setting as **flag > environment > default**, asking the flag package which flags were
   *passed* so `--addr ""` is a request rather than an absence.
2. Validates the listen address and the two URLs (`SAC_KEYVAULT_URI`, `SAC_BLOB_CIPHERTEXT_ENDPOINT`) at
   boot, so a typo in a deployment parameter is a startup failure rather than a first-request failure.
3. Picks the key backend. `local` uses `keys.NewLocal()`, or `keys.OpenLocal(file)` with `--key-file`.
   `kms` **refuses to start** unless `--allow-unimplemented-kms` was passed, naming the backend, the
   endpoint and the reason (the backend is not implemented in this build). A deployment must not report a KMS it does not
   have, so the refusal is the feature.
4. Refuses `--store sql` with the driver, the DSN it would have used and the evidence that does exist —
   "the one thing this must never do is start in memory and let a deployment believe it is persisting".
   `--store memory` builds `store.NewMemory()`; nothing here constructs the SQL store.
5. Builds the service with `CONTENT_VAULT_SCOPE_TIERS` (`scope=tier` pairs) as the signed bundle's
   per-scope search tiers, and the header authenticator restricted to `query-api`, `control-api` and
   `ops`.
6. Refuses a non-loopback bind unless `--allow-non-loopback` or `SAC_INTERNAL_ONLY=true` acknowledges
   it. Binding a non-loopback address would expose the only component that can unwrap content keys; the
   acknowledgement is the most a process can check about its own ingress, and it is **not** a substitute
   for the network control.

## Configuration

The environment names are the deployment's names, and the agreement is asserted rather than assumed:
[`infra_agreement_test.go`](infra_agreement_test.go) reads `azure/main.bicep` and this package's own
source, and `localdev/tools/check-config-agreement.mjs` adds both Dockerfiles and the lab compose file.

| Setting | Flag | Environment |
|---|---|---|
| identity | `--role` | `SAC_ROLE` |
| database host / name | `--pg-host`, `--pg-database` | `SAC_PG_HOST`, `SAC_PG_DATABASE` |
| listen address | `--addr` | `SAC_HTTP_ADDR` (the image sets `0.0.0.0:8080`) |
| store mode | `--store` | `SAC_STORE` (the image sets `memory`) |
| key backend | `--key-backend` | `SAC_KEY_BACKEND` (the image sets `local`) |
| Key Vault URI | `--keyvault-uri` | `SAC_KEYVAULT_URI` |
| blob endpoint (unused) | `--blob-ciphertext-endpoint` | `SAC_BLOB_CIPHERTEXT_ENDPOINT` |
| internal ingress | — | `SAC_INTERNAL_ONLY` |
| non-loopback acknowledgement | `--allow-non-loopback` | `SAC_ALLOW_NON_LOOPBACK` |
| telemetry | — | `SAC_APPINSIGHTS` |

`SAC_KEYVAULT_URI` supersedes `CONTENT_VAULT_KMS_ENDPOINT` for the `kms` backend; the
`CONTENT_VAULT_*` variables are inputs from the control plane (scope tiers, KMS mode) rather than
deployment parameters, which is why they keep those names. `SAC_BLOB_CIPHERTEXT_ENDPOINT` is
read, validated as a URL and then reported unused; `SAC_APPINSIGHTS` is read and reported unused
without being validated, and its value is never logged. This build performs no blob I/O and
exports no telemetry, and saying so beats leaving a deployment to assume otherwise.

## Probes

`probes.go` adds `/readyz` in front of the handler. It exists because
`azure/modules/container-app.bicep` probes both `/healthz` (liveness) and `/readyz` (readiness), and
before it the second path 404'd, so a container from this binary would have stayed unready forever.
Readiness answers a question this service can genuinely fail — **is the key backend usable?** — and
returns 503 for a process started with an acknowledged-but-unimplemented KMS, which is alive and must
not be sent traffic. The SQL store's connection check belongs there too when that mode is wired.

## Not verified

The `kms` backend, the SQL store and blob serving are all absent rather than untested in passing; the
full list is in [../../README.md](../../README.md#not-verified-and-why).
