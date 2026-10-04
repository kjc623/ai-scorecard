# aggregator

The scheduled job that rolls `ingest.submission` and `ingest.observation` up into the `mart`
usage aggregates and writes the `ops.aggregate_watermark` rows that tell the read path how current
each aggregate is. It is the job described by
[docs/03-data-platform.md](../../docs/03-data-platform.md) §5 and
[docs/04-dashboard-and-query.md](../../docs/04-dashboard-and-query.md) §4.

It replaces the bucket, never increments it (brief C28). That is what makes a re-run idempotent,
what absorbs a device that was offline and flushes late, and what lets a subject erasure make a
count fall.

## What it writes

| Table | Answers | Source |
|---|---|---|
| `mart.agg_tool_period` | Q1, part of Q2 | `ingest.submission` by bucket, tool |
| `mart.agg_tool_user_period` | Q2 | prompts by bucket, tool, `user_ref` |
| `mart.agg_class_period` | Q4 | labels unnested; severity from `ref.rule` falling back to `ref.data_class` |
| `mart.agg_org_period` | Q3 | prompts joined to `ops.user_dim`; empty until a directory sync exists |
| `mart.agg_user_period` | Q6 | prompts by bucket, `user_ref` |

And one `ops.aggregate_watermark` row per `(tenant, aggregate, bucket_size)` after each statement.

## How it runs

For each tenant, in **one transaction**: for the `hour` and `day` bucket sizes, recompute the
trailing window and upsert every bucket in it, then write the watermark. A failure rolls the whole
tenant back and advances no watermark; other tenants still run. The tenant is set on the session
with `set_config('app.tenant_id', …, true)` before any statement, so in production the job runs as
the non-superuser `sac_ops` role and forced row-level security isolates it.

The trailing window is computed from `received_at`, the server clock (brief C26), so a device
offline for a month flushes into the current bucket and the window never grows with downtime.

| Flag | Default | Meaning |
|---|---|---|
| `--dsn` / `SAC_PG_DSN` | — | PostgreSQL DSN (required) |
| `--interval` / `SAC_INTERVAL` | `5m` | how often a full pass runs |
| `--day-lookback` | `7` | trailing day buckets recomputed each pass |
| `--hour-lookback` | `48` | trailing hour buckets recomputed each pass |
| `--tenant` | all | roll up only this tenant; repeatable |
| `--once` | false | run one pass and exit |
| `--addr` / `SAC_HTTP_ADDR` | `127.0.0.1:8080` | `/healthz` and `/readyz` |

## Build

The default build carries no third-party dependency and refuses `--store sql` with a message
naming the tag. The driver is compiled only under `sac_sql_driver`, exactly as ingest-api,
control-api and content-vault do it:

```
go test ./...                                              # unit tests, no database needed
go build -tags sac_sql_driver -o aggregator-sql ./cmd/aggregator
./aggregator-sql -dsn "$SAC_PG_DSN" --once
```

`internal/rollup`'s integration test discovers a running PostgreSQL container that has
`database/schema.sql` applied (preferring `sac-authlab-postgres-1`, `shadowpg`,
`shadowpg-invariants`, overridable with `SHADOWPG_CONTAINER`) and skips when there is none. It
drives events through `ingest.record_event`, runs the frozen statements as text with
`PREPARE`/`EXECUTE`, and proves idempotency and late-arrival absorption. Everything runs in one
transaction ending in `ROLLBACK`.

## The lab

`localdev/authlab.compose.yaml` runs it as `sac/aggregator:lab-auth`, built by
`node localdev/build.mjs --auth`, with a 30-second interval. It connects as the same `postgres`
role every other lab service uses, which also lets it enumerate tenants; under forced row-level
security a constrained runtime role cannot, so a deployment either grants the job a role that can
see the tenant list or passes `--tenant` for each tenant.
