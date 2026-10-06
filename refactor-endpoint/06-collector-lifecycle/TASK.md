# 06. Collector lifecycle and policy toggles

## Problem

The registry and the provider contract exist (`device/capture-core/core/registry.go`,
`core/health.go`), but four things block new collectors:

- **Startup and shutdown are hard-coded.** `core.Supervisor.Startup` and `Shutdown`
  (`core/ordering.go`) start and stop only `cli.shim`, `proxy.tls` and `proxy.loopback`. A newly
  registered provider is never started.
- **Policy can't switch a collector on or off.** `Registry.ApplyPolicy` only passes the bundle to
  each provider as a diff, so a policy change can't start or stop a collector.
- **The registry is keyed by route, but a collector is not a route.** Some collectors emit no
  route (the tool config writers). The route-to-collector map (`collectorCodeByRoute`,
  `cmd/capture-core/health.go` around line 232) is a second list to keep in step.
- **A collector switched off by policy would look like a coverage gap.** The coverage job expects
  every `ref.collector` row on every device (`services/jobs/internal/rollup/coverage.go`).

## Goal

Any provider added to the registry starts after the fixed providers and stops before them. A
signed policy change starts or stops a provider without restarting the service. A provider
switched off by policy reports `absent` with detail `disabled_by_policy`, and the coverage job
does not count it as a gap.

## Scope

- **`device/protocol`**:
  - Add `type Collector string`, with constants for every `ref.collector` code in `DESIGN.md` §2
    that exists today (`egress_proxy`, `loopback_broker`, `cli_shim`, `process_detector`,
    `classifier_host`, `capture_extension`) and `Valid()`. Later tasks add their own constants.
  - Add `DetailDisabledByPolicy = "disabled_by_policy"` to the detail vocabulary and to
    `check-vocab`.
- **`core.Provider`**:
  - `Name()` returns `protocol.Collector` instead of `protocol.Route`. Update the three existing
    providers and their tests. Kill switches stay keyed by route: each provider checks the switch
    for its own route constant, as now.
  - Add an optional interface:

    ```go
    type Toggled interface {
        Enabled(b *policy.Bundle) bool
    }
    ```

    A provider that doesn't implement it is always enabled.
- **`core.Registry`**:
  - Key by `protocol.Collector`.
  - `ApplyPolicy` calls each provider's `ApplyPolicy`, then compares each `Toggled` provider's
    `Enabled(bundle)` with whether it is running. It calls `Start` on false→true and `Stop` on
    true→false, with the existing panic recovery and bookkeeping.
  - A provider stopped by policy reports `absent` with `disabled_by_policy`. This is not `tampered`,
    because its Stop was requested.
- **`core.Supervisor`**:
  - Add a step `StepStartCollectors = "start_collectors"` after `StepStartProxyLoopback`. It starts,
    concurrently, every registered provider not started by an earlier step that is enabled by the
    bundle in force.
  - The existing `StepStopProviders` stops them, concurrently, in shutdown.
  - Update `StartupOrder()` and the order tests.
- **`cmd/capture-core/health.go`**: the health row's collector is the provider's `Name()`. Remove
  `collectorCodeByRoute`; keep the normalisation of extension-reported names.
- **`services/jobs/internal/rollup/coverage.go`**: a `(device, collector)` whose latest row in the
  day has detail `disabled_by_policy` gets `expected = false`. Check how `ops.collector_state`
  stores the detail column, and use it. Add a case to the coverage integration test.
- **Tests** (`core`): fake providers prove that:
  - a provider registered after the fixed three is started by `start_collectors` and stopped in
    shutdown;
  - `ApplyPolicy` with a bundle that disables a `Toggled` provider stops it, and a later bundle
    that enables it starts it again, without `Supervisor.Startup` running twice;
  - a provider that panics in `Start` during a toggle is `absent` and doesn't affect the others.

## Done when

- `cd device/capture-core && go test -race ./core/ ./cmd/capture-core/` passes with the new tests.
  The toggle test is the plan's E03 finish line, at unit level; task 07 shows it with a real
  bundle.
- The coverage test shows a `disabled_by_policy` row as not expected.
- `node tools/accept.mjs` passes.
