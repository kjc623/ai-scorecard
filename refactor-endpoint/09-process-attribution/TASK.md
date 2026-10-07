# 09. Process attribution

Needs: on the reference VM, the second user from `TESTBED.md` signed in alongside the console user
(both sessions stay signed in).

## Problem

capture-core attributes everything it proxies to the console user:
- `tlsproxy.Config.Process` defaults to `"unknown"` (`device/capture-core/proxy/tlsproxy/provider.go`,
  around lines 83–119), and `service.go` never sets it;
- nothing maps a local TCP connection to the process that opened it;
- the only per-process identity is the native endpoint's peer-credential lookup
  (`cmd/capture-core/nativeipc_windows.go` → `hostinfo.UserOfProcess`).

The OTLP receiver (task 24), the flow monitor (task 21), the process monitor (task 19) and the
proxy all need "which process, which user, which executable, signed by whom" for a connection or
a PID.

## Goal

`hostinfo` answers, on Windows, which process owns a given loopback TCP connection, and who runs
that process, from which image, signed by which publisher. The TLS proxy uses this to attribute
each intercepted request to the process owner instead of the console user.

## Scope

- **`device/capture-core/hostinfo`**, with Windows implementations and `_other.go` stubs that
  return `ErrUnsupported`:
  - `OwnerOfLocalTCP(local, remote netip.AddrPort) (pid uint32, err error)`:
    - looks up the connection in `GetExtendedTcpTable` (`TCP_TABLE_OWNER_PID_ALL`, IPv4 and IPv6)
      through `iphlpapi.dll` with `golang.org/x/sys/windows` lazy procs;
    - the caller passes the server side's view (`remote` is the client's address as the server sees
      it), and the function finds the row whose local endpoint is the client's.
  - `ProcessInfo(pid uint32) (Process, error)`:
    - `Process{PID, Image string, Publisher string, User *User, Started time.Time}`;
    - `Image` comes from `QueryFullProcessImageNameW`;
    - `User` from the existing `UserOfProcess`;
    - `Started` from `GetProcessTimes`;
    - `Publisher` is the signer subject's CN from Authenticode (`WinVerifyTrust` through
      `x/sys/windows`, then the signer certificate through `CryptQueryObject`/`CryptMsgGetParam`
      or `WTHelperProvDataFromStateData`). An unsigned image gives `""`, not an error.
  - Cache `ProcessInfo` by `(pid, Started)` for 10 minutes, so a reused PID never returns another
    process's answer, with at most 1,024 entries.
- **TLS proxy**:
  - Set `tlsproxy.Config.Process` in `service.go` to a function that calls `OwnerOfLocalTCP` and
    `ProcessInfo` and returns the image base name. The pinning exclusion then keys on a real
    process name.
  - Each intercepted observation's `Person` (`core.Observation.Person`) is the process owner,
    resolved through the existing `peerPerson` logic. It falls back to the console user only
    when attribution fails, and counts `errors` once per failure.
- **Tests**:
  - unit tests on Windows: `OwnerOfLocalTCP` finds the PID of a test process that dialled a
    listener; `ProcessInfo` of the test binary has its image path and its owner;
    `ProcessInfo` of `C:\Windows\System32\curl.exe`'s process has publisher `Microsoft Windows`;
  - the TLS proxy test: an intercepted request carries the dialling process's user.

## Done when

- `cd device/capture-core && go test -race ./hostinfo/ ./proxy/tlsproxy/` passes on the PC (Windows).
- Ready to merge. After merge and deploy (`AGENTS.md`), ask the owner to switch TLS inspection on
  for the test tenant, and wait one policy poll:
  1. Run `invm.ps1 -AsUser second -Command 'curl.exe -s -o NUL -w "%{http_code}" https://api.openai.com/v1/models'`
     while the console user is at the console.
  2. It produces a `proxy.tls` event attributed to the second user, not the console user:
     - Device side: `invm.ps1 -AgentState` shows the `egress_proxy` `emitted` counter rising and
       the batch acknowledged.
     - The owner confirms on the dashboard's Search page that the newest event to
       `api.openai.com` names the second user. Its `subject_name` is their UPN while device
       identity is clear; the `user_ref` is `protocol.DeriveUserRef` of that UPN with kind `upn`.
     - Run the same command with `-AsUser console`. The owner confirms that event names the
       console user, for contrast.
  3. Ask the owner to switch TLS inspection off again.
- `node tools/accept.mjs` passes.
