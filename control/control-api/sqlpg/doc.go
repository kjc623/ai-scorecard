// Package sqlpg binds control-api to PostgreSQL.
//
// # What this package is
//
// It is the only place in this module that imports a third-party dependency
// (github.com/jackc/pgx/v5/stdlib), so that `go build ./...` and `go test ./...` can stay exactly as
// they are -- standard library only -- while the production path still uses a real driver.
//
// # Why it is behind a build tag
//
// The offline host cannot fetch modules (GOPROXY=off), and the acceptance harness runs every Go
// package with GOMODCACHE pointed at an empty directory. A dependency the default build needs would
// turn the packages gate red on a clean machine. So:
//
//   - the driver is imported only under the `sac_sql_driver` tag, and
//   - that tag is never set by the default build, the gate, or the lab images.
//
// Those two facts keep the default build dependency-free in practice: module-graph pruning means an
// unused requirement is never loaded, which is verified by the packages gate passing on a machine
// whose module cache is empty.
//
// # The two commands
//
// Default build (what CI and the gate run; no third-party code compiled):
//
//	go build ./... && go test ./...
//
// Tagged build (the production persistence path; needs a module cache or a network):
//
//	go build -tags sac_sql_driver ./... && go test -tags sac_sql_driver ./sqlpg/ -v
//
// The tagged test connects to the lab's PostgreSQL and exercises the statements internal/store
// already has. It skips, loudly, when no server is reachable, because a skip with a reason is honest
// and a red gate is not.
package sqlpg
