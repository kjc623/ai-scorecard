# sqlpg — the one third-party dependency, behind a build tag

This package binds [ingest-api](../README.md) to PostgreSQL. It is four small files and its whole
reason for existing is containment: `github.com/jackc/pgx/v5/stdlib` is imported **here and nowhere
else** in the module, so `go build ./...` and `go test ./...` can stay standard-library-only while the
production path still uses a real driver.

## Why a build tag

The gates run with `GOPROXY=off` by choice, so nothing is fetched during a gate run, and the acceptance
harness runs every Go package with `GOMODCACHE` pointed at its own directory (`.tools/gopath/pkg/mod`).
A dependency the default build needs would turn the packages gate red on a machine whose cache does not
hold it. So the driver is imported only under `sac_sql_driver`, and that
tag is never set by the default build, the gate, or the lab images. The `require` lines in `go.mod` are
inert without the tag: module-graph pruning means an unused requirement is never loaded, so the default
build needs nothing from the module cache.

The two commands, and the difference between them:

```powershell
# default: no third-party code compiled; what CI and the gate run
go build ./... && go test ./...

# tagged: the production persistence path; needs a module cache or a network
go build -tags sac_sql_driver ./... && go test -tags sac_sql_driver ./sqlpg/ -v
```

`--store sql` on the untagged binary refuses to start and names the driver, the variables it read and
these commands, because starting in memory while a deployment believes it is persisting is the worse
failure. The tagged binary links the driver in itself, so it needs no `-driver` flag, and the untagged
one cannot pretend to have one.

## What is in it

- `driver.go` (`//go:build sac_sql_driver`) is the blank import that registers `pgx` with
  `database/sql`, plus `DriverName`, `DefaultMaxOpenConns` (16 — the pool is sized for concurrency, not
  throughput, against the design's 0.14 events/s mean) and two openers: `Open`, which wraps the pool in
  `store.SQLStore`, and `OpenDB`, the raw pool for callers that need SQL, such as the integration test
  seeding a tenant.
- `integration_test.go` connects to the lab's PostgreSQL and exercises the statements
  `internal/store` already holds — pooling, the transaction boundary, `ingest.record_event()`, driver
  error mapping. It **skips loudly** when no server is reachable, with the command that starts one,
  because a skip with a reason is honest and a red gate for a missing container is not.

Neither opener pings. Whether the database is reachable is a readiness question, answered by `/readyz`,
which reads `ref.route_fidelity` through this store; an `Open` that pinged would turn a slow database
into a startup failure instead of an unready replica.

## Verified only under the tag

Everything above is exercised only in the tagged build and only against the lab's server. Concurrency
on the SQL path was not stressed, and no server other than the lab's was used — the same caveats as
[../README.md § Not verified](../README.md#not-verified), which also explains why the tag exists
rather than a vendored driver.
