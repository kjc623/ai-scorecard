# jobs

The scheduled database jobs, one binary with a subcommand per job. Each run makes one pass over
every tenant and exits: zero when every tenant succeeded, non-zero when any failed. Every tenant's
work is one transaction that first sets `app.tenant_id`, so row-level security confines it to that
tenant; a failed tenant is rolled back, logged, and the remaining tenants still run. The tenant list
comes from `ops.tenant_ids()`.

| Subcommand | Schedule | What it does |
|---|---|---|
| `jobs aggregate` | every 5 minutes | Recomputes the trailing 48 hour buckets and 7 day buckets of the `mart.agg_*_period` usage aggregates from `ingest.submission`, replacing each bucket rather than incrementing it, and writes an `ops.aggregate_watermark` row per aggregate and bucket size. Also derives `mart.finding` (insert-only) over the trailing 7 days and recomputes 7 days of `ops.coverage_snapshot`. |
| `jobs expire` | daily | Deletes rows whose `expires_at` has passed from `ingest.search_text`, `ops.content`, `ingest.observation`, `ingest.submission` and `ingest.rejected`, in batches of at most 5,000 rows per statement. A submission that outlives its expired stored content is marked `content_state = 'shredded'`, `shredded_reason = 'retention_expired'`. When anything was removed, writes one `ops.erasure_receipt` (`scope_kind = 'retention'`) with the per-table counts, in the same transaction. |

Logs are JSON on stdout: one line per tenant with its per-table row counts (`written` or
`removed`), and a summary line per pass.

## Configuration

Both jobs run as the database login `jobs` (role `sac_ops`).

| Variable | Default | Meaning |
|---|---|---|
| `SAC_PG_HOST` | (required) | PostgreSQL server host |
| `SAC_PG_PORT` | `5432` | PostgreSQL port |
| `SAC_PG_DATABASE` | (required) | Database name |
| `SAC_PG_USER` | (required) | Database login: `jobs` |
| `SAC_PG_PASSWORD` | (none) | Password authentication, used by the local lab. When unset, each connection authenticates with a Microsoft Entra token for the managed identity. |
| `SAC_PG_SSLMODE` | `require` | libpq `sslmode` |
| `AZURE_CLIENT_ID` | (none) | Which user-assigned managed identity to request tokens for. Container Apps injects `IDENTITY_ENDPOINT` and `IDENTITY_HEADER`. |

## Build and test

```
docker build -f services/jobs/Dockerfile -t jobs .     # from the repository root
cd jobs && go vet ./... && go test ./...
```

The database tests run only when `SAC_TEST_PG_DSN` names a PostgreSQL database with
`services/database/schema.sql` applied, connecting as a role that may `SET ROLE sac_ops`, for example
`postgres://postgres:secret@localhost:5432/postgres?sslmode=disable`. Each test creates a tenant with
a random id inside a transaction and rolls it back. Without the variable they skip.
