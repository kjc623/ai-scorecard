# 48. Supervised components

## Problem

The plan has the agent install, start and monitor an MCP gateway as a supervised component. No
gateway exists in this repository (`DESIGN.md` §1). The agent already supervises one child
process, `classifier-host`, through code specific to it:
- `classifierHostController` in `cmd/capture-core/service.go`, around lines 701–738;
- `classifierlink/child.go`.

That code can't supervise anything else. Its health is a synthetic row built in `health.go`
(around lines 286–292) rather than a registered provider.

## Goal

A generic supervised-component package starts a child process from the install directory and
keeps it running with bounded restarts. It reports a health row, and stops it in the right
shutdown step. `classifier-host` becomes its first user, with unchanged behaviour. A future MCP
gateway would be a second user. No gateway code is written here.

## Scope

- **New package `device/capture-core/component`**:
  - `Spec{Collector protocol.Collector, Path string, Args []string, Stdio bool, Ready func(ctx) error}`.
  - `Supervisor` with `Start`, `Stop`, `Health` and `Restarts()`, implementing `core.Provider`.
  - Restart with exponential backoff (1 s doubling to 60 s), at most 5 restarts in 10 minutes,
    then `degraded` with a new detail `component_crash_loop` (add it to the vocabulary and
    `check-vocab`). It tries again after 10 minutes.
  - On Windows the child is put in a job object with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, so it
    dies with the service. On Unix it gets `Pdeathsig` where available, and a process-group kill
    on Stop.
  - Stdout and stderr, when not used as the protocol channel, are discarded. A child's output is
    never logged, because it could carry content.
- **Move `classifier-host` onto it**:
  - `classifierlink` keeps the protocol (handshake, frames, the serialised `Classify` from task
    02).
  - `component.Supervisor` owns the process.
  - The `classifier_host` health row becomes the supervisor's row, and the synthetic row in
    `health.go` is removed.
  - The supervisor still starts it at `StepStartClassifierHost` and stops it at
    `StepStopClassifierHost` (`core/ordering.go`), so the order tests stay unchanged.
  - Its behaviour when the release is missing stays the same: it isn't started, and classification
    degrades to rules-only.
- **No gateway**: don't add an MCP gateway spec, path, setting or placeholder. Add one sentence to
  `device/README.md`'s component table saying `component` supervises child processes.
- **Tests**:
  - a fake child that exits is restarted with backoff, on a fake clock;
  - the crash-loop limit gives `degraded`;
  - Stop kills the child;
  - the existing `classifierlink` and `cmd/capture-core` tests pass unchanged.

## Done when

- `cd device/capture-core && go test -race ./component/ ./classifierlink/ ./cmd/capture-core/ ./core/`
  passes.
- `node tools/accept.mjs` passes.

## On the device

On the reference VM, running `main`'s release:
1. `invm.ps1 -Command 'Stop-Process -Name classifier-host -Force; Start-Sleep -Milliseconds 2000; Get-Process classifier-host'`
   shows a new process within 2 s.
2. The `classifier_host` row stays `healthy` after the restart (`invm.ps1 -AgentState`), and the
   owner confirms the same on the dashboard's device view.
3. Killing it six times in a row, in one `invm.ps1 -Command` loop that waits for each restart,
   shows `degraded`/`component_crash_loop` in `invm.ps1 -AgentState`. The owner confirms it on
   the device view after the next health report.
