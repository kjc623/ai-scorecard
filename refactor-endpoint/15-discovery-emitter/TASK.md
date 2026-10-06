# 15. Discovery emitter

## Problem

Five collectors produce discovery records:
- installed apps, CLIs and IDE extensions (16–18);
- running apps (19);
- local models (20);
- inference connections (21).

Each needs the same handling: per-day de-duplication that survives a restart, a daily budget,
conversion to a `discovery` envelope through `core.Pipeline.Record` (task 05), and counting
against its own health row. Written five times, these would drift.

## Goal

One package, `device/capture-core/discovery`, takes a `Record` from any collector and emits at
most one `discovery` envelope per key per UTC day, within the bundle's daily budget, exactly as in
`DESIGN.md` §6.

## Scope

- **`device/capture-core/discovery`**:
  - `Record{Type protocol.DiscoveryType, Basis protocol.DetectionBasis, AppKey, Version, Publisher, HostApp, DestinationHost string, ModelNames []string, UserRef string, Person *core.Person, OccurredAt time.Time}`.
    `UserRef` is `unattributed` for machine-wide facts.
  - `Emitter`, built with:
    - the pipeline (an interface with `Record(ctx, core.Fact) error`);
    - the state directory;
    - a clock;
    - a function returning the bundle in force;
    - the device id.
  - `Emit(ctx, collector *core.CounterSet, r Record) error`:
    1. Computes the §6 key, `sha256(device|user_ref|type|app_key|version|destination_host|day)`
       over the UTC day of `OccurredAt`.
    2. Drops a key already seen today, counting nothing.
    3. Refuses past `endpoint.discovery_daily_budget` (counting `dropped` on the collector's set).
    4. Otherwise calls `Pipeline.Record` with `tool_fingerprint` `app:<AppKey>` and route
       `inv.scan`, `proc.detect` or `net.flow` (passed by the collector), and the `dedup_key` set
       to the same sha256 in `sha256:` form. It counts `emitted`, and `errors` on failure.
  - `Stop(ctx, Record)`, for the process monitor: records an `app_running` stop, which never
    becomes an envelope (§6). It is counted `observed` only.
  - Persistence:
    - `discovery-seen.json` in the state directory holds today's day and keys;
    - it is written atomically (temp file and rename, as `state` does elsewhere);
    - it is loaded at construction and reset when the day changes;
    - a corrupt file is replaced (counting `errors`), never fatal.
  - Concurrency-safe: collectors call it from their own goroutines.
- **Tests**:
  - same record twice in a day gives one envelope;
  - a new day emits again;
  - a restart (new `Emitter` on the same directory) doesn't re-emit;
  - the 201st distinct record with a budget of 200 is dropped and counted;
  - a stop is never emitted;
  - every emitted fact validates through `BuildEnvelope`;
  - run under `-race`.
- No collector uses it yet; task 16 is the first.

## Done when

- `cd device/capture-core && go test -race ./discovery/` passes.
- `cd device/integration && go test ./...` passes, with one emitted `discovery` record per
  `discovery_type` validated against `envelope.Schema`.
- `node tools/accept.mjs` passes.
