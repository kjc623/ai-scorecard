# sqlpg — the one third-party dependency, behind a build tag

This package binds [control-api](../README.md) to PostgreSQL, exactly as
[ingest-api's sqlpg](../../ingestion/ingest-api/sqlpg/README.md) does. It is four small files and its
whole reason for existing is containment: `github.com/jackc/pgx/v5/stdlib` is imported **here and
nowhere else** in the module, so `go build ./...` and `go test ./...` stay standard-library-only while
the production path still uses a real driver.

The driver is imported only under `sac_sql_driver`, which the default build, the gate and the lab
images never set, so the default build needs nothing from the module cache. The two commands:

```
# default: no third-party code compiled; what CI and the gate run
go build ./... && go test ./...

# tagged: the production persistence path; needs a module cache or a network
go build -tags sac_sql_driver ./... && go test -tags sac_sql_driver ./sqlpg/ -v
```

`--store sql` on the untagged binary refuses to start and names the driver, the variables it read and
these commands, because starting in memory while a deployment believes it is persisting is the worse
failure.

`integration_test.go` connects to the lab's PostgreSQL and prepares every constant in
`internal/store` against the live schema, then drives the store through the real driver: token
resolution, device upsert, credential rotation in one transaction, single-use settlement and the DPoP
`jti` window. It **skips loudly** when no server is reachable, with the command that starts one.

Neither opener pings. Whether the database is reachable is a readiness question, answered by
`/readyz`; an `Open` that pinged would turn a slow database into a startup failure instead of an
unready replica.
