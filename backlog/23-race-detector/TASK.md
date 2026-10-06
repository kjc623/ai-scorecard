# 23. Run the Go suites under the race detector in CI

## Problem

No Go suite runs under `-race`. The device agent is heavily concurrent (spool, drainer, native
relay, proxies, classifier link) and the services serve concurrent requests, so a data race would
only show up in production. When classifier-host was last run with `go test -race ./...` on Linux it
had no data race, but one end-to-end case in `cmd/classifier-host/main_test.go` failed on timing
alone: under race instrumentation the parser child takes longer to start than the parser's real
250 ms timeout (`parser/isolation/run.go`, `Timeout`).

## Goal

The Linux CI job runs every Go module's tests with `-race`, and they pass.

## Scope

- `tools/accept.mjs`: an option (e.g. `--race`) that adds `-race` to the `go` gate's `go test`;
  `.github/workflows/ci.yml` uses it on Linux (`-race` needs cgo; the hosted Ubuntu runner has gcc).
  Windows keeps the plain run.
- Make timing-dependent tests independent of instrumentation speed — for the classifier-host case,
  let the test set the parser timeout rather than relying on the production default — without
  changing production limits.
- Fix every real race the run finds, in the module where it is found, with a test that would have
  caught it.
- Out of scope: performance tuning, new features.

## Done when

- On Linux (CI, or a `golang:1.27` container locally), `node tools/accept.mjs --only go --race`
  passes.
- `node tools/accept.mjs` still passes on Windows.
