# 19. Process monitor

## Problem

Being installed is not being used. The product can't tell when an AI app actually runs, for which
user, or whether the running binary is the vendor's signed one:
- process detection was deleted from capture-core;
- the `proc.detect` route and the `process_detector` collector row are left over with no
  implementation.

Polling the process list misses short-lived processes. Windows reports process start and stop in
real time through ETW (Event Tracing for Windows).

## Goal

A new process monitor watches process start and stop in real time and matches each process to the
catalog by executable name and publisher. It reports start and stop to the discovery emitter
(task 15), so the first start per app, user and day becomes an `app_running` record with the
version and signer. An admin can switch it on and off from the dashboard.

## Scope

- **`device/capture-core/etwsession`**: a small shared helper for real-time ETW sessions, used here
  and by the flow monitor (task 21). It is built on `github.com/0xrawsec/golang-etw`
  (`DESIGN.md` §11).
  - `Open(name string, providers []Provider) (*Session, error)`.
  - `Events() <-chan Event`.
  - `Close()`.
  - Session names are prefixed `ShadowAICapture-`. A stale session with the same name, left by a
    crash, is stopped and re-created.
  - `_other.go` returns `ErrUnsupported`.
- **`device/capture-core/procmon`**, a `core.Provider`:
  - collector `process_detector` (existing `ref.collector` row; add the `protocol.Collector`
    constant if task 06 didn't), route `proc.detect`;
  - `core.Toggled` on `endpoint.processes.enabled`;
  - subscribes to `Microsoft-Windows-Kernel-Process` (`{22FB2CD6-0E7B-422B-A0C7-2FAD1FD0E716}`),
    keyword `WINEVENT_KEYWORD_PROCESS` (0x10). Event 1 (ProcessStart) gives PID, image name and
    start time; event 2 (ProcessStop) gives PID. Verify the event ids and fields on the
    reference host and record the Windows build checked in `DECISIONS.md`.
  - **On start**: match the image's base name with `Bundle.AppByExe("windows", …)`. On a match,
    resolve the process through `hostinfo.ProcessInfo` (task 09: image, user, publisher). Keep a
    running-set entry `pid → (app_key, user, started)`, and send a `discovery.Record{Type: app_running, Basis: process_event, AppKey, Version, Publisher, Person}`
    to the emitter.
    - The version comes from the image's PE file version resource (as in task 17).
    - A publisher that doesn't match the app's `publisher` signal, when the catalog lists one, is
      still emitted, with the observed publisher, so an impostor binary is visible.
  - **On stop**: if the PID is in the running set, call `Emitter.Stop` and remove the entry.
  - **At start-up**: enumerate the current processes once (`CreateToolhelp32Snapshot`), so apps
    already running are seen.
  - **Health**:
    - `healthy` while the session delivers events;
    - `degraded` with a new detail `etw_session_failed` if the session can't be opened or stops
      delivering (add it to `device/protocol` and `check-vocab`);
    - `absent` on other platforms (§12).
- Wire it into `buildProviders`.
- **Tests**:
  - `procmon` with a fake event source: start then stop of a catalog app gives one emitter record
    and one stop;
  - a non-catalog app gives nothing;
  - two starts in a day give one record;
  - a mismatched publisher is emitted with the observed publisher;
  - a Windows-only test opens a real session for a few seconds and sees the test's own child
    process start (`go test` runs elevated on the reference host; skip with a clear message when
    it isn't elevated).

## Done when

- `cd device/capture-core && go test -race ./etwsession/ ./procmon/` passes. On the reference
  host, run it elevated and show that the real-session test ran rather than skipped.
- On the reference host with the lab MSI rebuilt:
  1. Launching and quitting Claude Desktop produces, within seconds, an `app_running` record for
     `app:claude_desktop` with its publisher and the owner's `user_ref` in the lab tenant.
  2. The service log (`capture-core.log` in the state directory, at the default `info` level)
     shows one line for the start and one for the stop. The provider logs, for each catalog start
     and stop it handles, the app key and PID only, never a command line.
- `node tools/accept.mjs` passes.
