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

- **Hook latency**: task 36's benchmark (`SAC_HOOK_BENCH=1`) stays the measurement, run in the
  reference VM as the console user in the device phase, exactly as task 36's On the device
  describes. The budgets are the
  VM's numbers. For CI, add a Go benchmark of the service-side decision alone, without process
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
  - It runs on the reference VM in the device phase (On the device). For the build, run it
    once on the PC against a locally started `capture-core` to prove the script works; that
    number is not the measurement.
- Don't change any budget. A budget that can't be met is reported, not loosened.

## Done when

- `TestHookDecisionBudget` and `TestOTLPBudget` pass locally and in `node tools/accept.mjs`. The
  report shows each one failing when a 30 ms sleep is injected into the measured path (then
  reverted).
- `measure-idle.ps1` runs on the PC and prints its two numbers.
- `node tools/accept.mjs` passes.

## On the device

On the reference VM, running `main`'s release, with every endpoint collector on for the test
tenant (ask the owner to check the Settings page; wait) and both users idle:
- `measure-idle.ps1` is a measurement tool, not part of the install: copy it in with
  `invm.ps1 -CopyTo` and run it with `invm.ps1 -Command`. It passes. Its output is in the report,
  and its numbers are in `DECISIONS.md` with the VM's vCPU count and memory.
- Task 36's whole-process benchmark is rerun on the reference VM as the console user
  (`invm.ps1 -AsUser console`), with p99 under 50 ms.
