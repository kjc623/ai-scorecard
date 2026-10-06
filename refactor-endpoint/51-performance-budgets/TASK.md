# 51. Performance budgets

## Problem

The agent sits on the user's critical path (hooks), receives telemetry continuously (OTLP), and
runs all day on every managed laptop. `DESIGN.md` §13 sets budgets for each, but nothing measures
them repeatedly, so a regression would ship unnoticed.

## Goal

Each budget in §13 has a repeatable measurement. The ones that are stable on a CI runner fail the
build when exceeded. The ones that need a real desktop have a script whose result goes in the
report and in `DECISIONS.md`.

## Scope

- **Hook latency**: task 36's benchmark (`SAC_HOOK_BENCH=1`) stays the measurement on the
  reference host. For CI, add a Go benchmark of the service-side decision alone, without process
  spawn: frame in, classify, evaluate, frame out, over a loopback pipe or socket with a 4 KB
  prompt.
  - It fails at p99 ≥ 20 ms. That is a CI-stable fraction of the 50 ms whole-process budget;
    record the ratio and why in `DECISIONS.md`.
  - It runs in `node tools/accept.mjs`'s `go` gate as a normal test (`TestHookDecisionBudget`)
    with 2,000 iterations. Use a percentile check, not `testing.B`.
- **OTLP throughput** (`capture-core/otlp`, `TestOTLPBudget`):
  - Send 2,000 log records per second for 10 s through the official OTLP/HTTP log exporter
    (`DESIGN.md` §11) to the receiver, with a fake pipeline.
  - Assert none are dropped and the p99 request handling time is under 20 ms.
  - On a CI runner, scale by the measured single-core speed only if the raw test is flaky in 5
    consecutive runs. Record what you did.
- **Idle CPU and memory** (`device/installer/windows/measure-idle.ps1`):
  - Samples `capture-core` and its children (`classifier-host`, helpers) for 10 minutes with
    `Get-Counter '\Process(*)\% Processor Time'` and private working set.
  - Prints the average CPU as a share of one core and the peak private working set.
  - Exits 1 above §13's limits.
  - Run it on the reference host with the lab MSI and every collector on, the user idle.
- Don't change any budget. A budget that can't be met is reported, not loosened.

## Done when

- `TestHookDecisionBudget` and `TestOTLPBudget` pass locally and in `node tools/accept.mjs`. The
  report shows each one failing when a 30 ms sleep is injected into the measured path (then
  reverted).
- `measure-idle.ps1` passes on the reference host. Its output is in the report and its numbers are
  in `DECISIONS.md`.
- Task 36's whole-process benchmark is rerun on the reference host, with p99 under 50 ms.
- `node tools/accept.mjs` passes.
