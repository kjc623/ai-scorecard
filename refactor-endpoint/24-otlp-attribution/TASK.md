# 24. OTLP sender attribution

Needs: on the reference VM, the second user from `TESTBED.md` signed in alongside the console user.

## Problem

OTLP requests arrive on a loopback socket shared by everyone on the machine. A prompt from one
user's Claude Code must not be attributed to another user, or to whoever is at the console. The
receiver (task 23) knows only the client's remote address.

## Goal

Every OTLP request is attributed to the process that sent it and that process's user. The
attribution comes from the TCP connection's local peer port (task 09), and every normalized
record carries that person.

## Scope

- **`device/capture-core/otlp`**:
  - `Sender{PID uint32, Image, Publisher string, Person *core.Person, Resolved bool}`.
  - It is resolved once per connection, not per request:
    - **HTTP**: in `http.Server.ConnContext`, from the `net.Conn`'s local and remote addresses,
      through `hostinfo.OwnerOfLocalTCP` and `hostinfo.ProcessInfo`. Stored in the request
      context.
    - **gRPC**: a `stats.Handler` `TagConn` or a server option that sees the raw `net.Conn` does
      the same lookup. Read the result in the interceptor with `peer.FromContext` plus the
      connection's tag.
  - The person is the process owner, resolved through the same `peerPerson` path the native
    endpoint uses (`cmd/capture-core/person.go`). If the shared helper isn't reachable from
    `otlp`, move it into a package both can use, without changing its behaviour.
  - When attribution fails:
    - the request is still accepted;
    - `Resolved` is false;
    - normalizers attribute to `unattributed`, never to the console user;
    - the provider counts one `errors` per connection.
  - Normalizers receive the `Sender` (the interface from task 23 already carries it).
- **Tests**:
  - a test client dialling the HTTP and gRPC listeners is attributed to the test process's own
    PID and user;
  - a failed lookup gives `unattributed`.

## Done when

- `cd device/capture-core && go test -race ./otlp/` passes.
- The provider logs one `info` line per new connection, with the sender's PID, image base name
  and `user_ref` only (never a body or attribute value).
- Ready to merge. After merge and deploy (`AGENTS.md`):
  1. Copy `telemetrygen` (the same release as task 23's) into `C:\ProgramData\SacTestbed\otel\`
     with `invm.ps1 -CopyTo`.
  2. Start it with the device token header in both users' sessions at the same time:
     `invm.ps1 -AsUser console -Command '<telemetrygen logs ... --duration 60s>'` and
     `invm.ps1 -AsUser second -Command '<same>'`, started back to back so their runs overlap.
  3. The log (`invm.ps1 -Command 'Get-Content C:\ProgramData\ShadowAICapture\state\capture-core.log -Tail 100'`)
     shows two connections with two distinct `user_ref`s. They match the refs derived from the
     two users' UPNs.

  Events carrying the person follow in task 26.
- `node tools/accept.mjs` passes.
