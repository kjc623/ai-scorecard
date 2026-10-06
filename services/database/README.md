# database

The product's PostgreSQL 16 schema and the `migrate` job that applies it.

| Path | What it is |
|---|---|
| `schema.sql` | The whole schema: roles, tables, row-level security, grants, functions, triggers, seed data. Schema version 1 |
| `migrations/` | Numbered changes for databases that already exist (see its README) |
| `database.go`, `cmd/migrate/` | The migrator (Go module `github.com/shadow-ai-capture/database`) |
| `Dockerfile` | The `migrate` image (build context: the repository root) |
| `invariants.test.sql` | Assertions that the schema's properties hold on a real server, run as the runtime roles |
| `tools/` | `test-database.mjs` (the database gate) and `check-schema.mjs` (static checks) |

## In production

The deployment runs the `migrate` image as a job before the services start. It holds one
connection under an advisory lock, creates `public.schema_migration` if needed, applies
`schema.sql` to an empty database and then each pending migration (each in its own transaction with
its ledger row), and makes every component login a member of exactly its runtime role, creating the
login with `pgaadauth_create_principal_with_oid` when it does not exist. A re-run changes nothing.
It logs JSON to stdout and exits non-zero on failure.

| Variable | Meaning |
|---|---|
| `SAC_PG_HOST`, `SAC_PG_PORT` (5432), `SAC_PG_DATABASE` | The server and database |
| `SAC_PG_USER` | The migration identity: in Azure a Microsoft Entra administrator of the server (member of `azure_pg_admin`, `CREATEROLE`, not a superuser), with `CREATE` on the database and on schema `public` |
| `SAC_PG_PASSWORD` | Lab only. Without it each connection authenticates with an Entra token for the managed identity |
| `SAC_PG_SSLMODE` (`require`) | TLS mode |
| `SAC_DB_LOGINS` | JSON array of `{"login", "role", "objectId"}`: the login name, one of `sac_ingest`, `sac_control`, `sac_vault`, `sac_query`, `sac_ops`, and the managed identity's object id. Empty skips the step |

```
SAC_DB_LOGINS='[{"login":"ingest-api","role":"sac_ingest","objectId":"<guid>"},{"login":"control-api","role":"sac_control","objectId":"<guid>"},{"login":"content-vault","role":"sac_vault","objectId":"<guid>"},{"login":"query-api","role":"sac_query","objectId":"<guid>"},{"login":"jobs","role":"sac_ops","objectId":"<guid>"}]'
```

Every tenant-scoped query runs in a transaction that first does
`SELECT set_config('app.tenant_id', $1, true)`; without it row-level security returns nothing.

## Changing the schema

Edit `schema.sql` so a new database gets the change, and add the same change as the next
`migrations/NNNN-name.sql` so existing databases get it. Then run the checks below.

## Build and test

```
cd database && go vet ./... && go test ./...      # unit tests; set SAC_TEST_PG_DSN to an empty database for the live test
node services/database/tools/check-schema.mjs               # static checks, no server
node --test services/database/tools/check-schema.test.mjs   # proves each static check can fail
node services/database/tools/test-database.mjs              # the gate (Docker and Go)
docker build -f services/database/Dockerfile -t migrate .   # the image, from the repository root
```

The gate starts a throwaway `postgres:16` container set up like Azure (a NOLOGIN `azure_pg_admin`, a
non-superuser administrator with `CREATEROLE` in it, a database it owns, and an emulation of
`pgaadauth_create_principal_with_oid`), runs the migrator twice as that administrator, checks the
component logins, runs `invariants.test.sql`, and removes the container. It prints PASS or FAIL;
without Docker or Go it prints SKIPPED and exits 3, and a skipped gate is not a pass.
