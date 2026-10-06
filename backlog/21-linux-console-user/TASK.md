# 21. Attribute Linux CLI traffic to the signed-in user

## Problem

capture-core attributes what `proxy.tls` observes to the person signed in at the device. On Windows
it asks the session manager and on macOS it reads the owner of `/dev/console`
(`device/capture-core/hostinfo/console_darwin.go`). On Linux `consoleUser()` returns
`ErrUnsupported` (`hostinfo/console_other.go`), and because the service runs as root, every CLI-tool
observation on a Linux device is recorded as `unattributed`. (The browser path is already attributed
through the native host relay.)

## Goal

On a Linux desktop, observations are attributed to the user of the active local session, the same way
as on Windows and macOS.

## Scope

- Implement `consoleUser()` for Linux in a new `hostinfo/console_linux.go` (and narrow
  `console_other.go`'s build tag to what remains, or delete it if nothing does): ask systemd-logind
  for the active session on `seat0` and return its user (`UserOfUID`). Use the logind D-Bus API
  (`org.freedesktop.login1`) with a maintained client such as `github.com/godbus/dbus/v5`.
- No active session, or a session owned by root → `ErrNoConsoleUser`; no logind (e.g. a server
  without systemd) → `ErrUnsupported`. Keep the existing resolver behaviour for both.
- Unit tests with the D-Bus calls behind a small interface; no test may need a real logind.
- Out of scope: Windows and macOS, SSH sessions without a seat, multi-seat machines beyond `seat0`.

## Done when

- `go vet` (GOOS=linux, windows, darwin) and `go test ./...` pass in `device/capture-core`;
  `node tools/accept.mjs` passes.
- The owner verifies on a Linux desktop with the agent installed: a `curl` to an intercepted AI
  endpoint from a signed-in user's terminal shows that user (not `unattributed`) on the dashboard.
