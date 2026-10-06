# 10. User-session helper

## Problem

capture-core runs as LocalSystem. Some work has to happen inside the signed-in user's session:
- showing a notification when the proxy blocks a prompt (task 46);
- reading per-user state that only exists while the user is signed in.

There is no per-user process today. The native endpoint's transport (pipe or socket server, peer
credentials, framing) lives in `package main` (`cmd/capture-core/nativeipc*.go`, `native.go`), so
nothing else can reuse it.

## Goal

The service starts one helper process in each interactive user session and keeps it running. The
service and the helper talk over the native endpoint with peer-credential checks on both sides.
The helper's first job is to show a notification the service asks for.

## Scope

- **`device/capture-core/localipc`**:
  - Move the endpoint server and client out of `cmd/capture-core`:
    - Windows pipe and Unix socket listeners, with their security descriptors and modes;
    - the server-ownership check;
    - peer identification;
    - the length-prefixed frame reader and writer.
  - `native.go`, `relay.go` and the extension's message handling keep their behaviour and call the
    package. This is a move, not a redesign: existing tests keep passing unchanged except for
    imports.
- **Protocol** (`device/protocol/native.go`):
  - `helper_hello` (helper → service): `{session_id, pid}`;
  - `notify` (service → helper): `{title ≤ 64, body ≤ 280, link?}`;
  - `notify_result` (helper → service): `{shown bool, error?}`.

  The service checks that the peer's user owns `session_id`. Add the types to `check-vocab` as
  device-only types.
- **Helper mode**: `capture-core --user-helper`.
  1. It connects to the endpoint, checking the server is SYSTEM-owned (the existing client
     check), and sends `helper_hello`.
  2. It shows each `notify` as a Windows toast through `Windows.UI.Notifications.ToastNotificationManager`,
     called over WinRT with `github.com/go-ole/go-ole` (`DESIGN.md` §11).
  3. Use the Start-menu shortcut AppUserModelID that the MSI installs. Add the shortcut and its
     `System.AppUserModel.ID` property to `device/installer/manifest.mjs` and regenerate the
     `.wxs`.
- **Service side** (`capture-core/userhelper`, a `core.Provider` with collector `user_helper`):
  - Every 15 seconds, on the existing `watchPeople` tick, enumerate active interactive sessions
    (`WTSEnumerateSessionsW`, state `WTSActive`).
  - Start `capture-core.exe --user-helper` in each session that has no live helper, with
    `WTSQueryUserToken` and `CreateProcessAsUserW` (no window, `lpDesktop` `winsta0\default`).
  - Restart a helper that exits, at most 5 times in 10 minutes per session, then report
    `degraded`/`helper_unavailable`.
  - Health is `healthy` when every active session has a connected helper.
  - Expose `Notify(sessionID, n) error` for later tasks.
  - Not `Toggled`: the helper always runs, because it collects nothing.
- `ref.collector` row `user_helper`, and the detail `helper_unavailable`.
- On macOS and Linux the provider reports `absent` with `helper_unavailable` (tasks 53 and 55
  port it).

## Done when

- `cd device/capture-core && go test -race ./localipc/ ./userhelper/ ./cmd/capture-core/` passes,
  including a test where a client that is not the session's owner is refused for `helper_hello`.
- On the reference host with a rebuilt lab MSI:
  1. `Get-Process capture-core` shows a helper in the owner's session.
  2. Ending it brings it back within 15 s.
  3. Prove `Notify` with the userhelper package's Windows integration test (not with production
     code added for the purpose). The test starts a helper in the current session and shows a
     toast. Take a screenshot.
- `node tools/accept.mjs` passes.
