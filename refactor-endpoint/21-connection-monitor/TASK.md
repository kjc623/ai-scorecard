# 21. Connection monitor

## Problem

Scripts, SDKs and unknown tools that call inference APIs directly are invisible to every
collector that doesn't decrypt, and TLS inspection is now off by default (task 08). The
operating system already knows which process connected where. The product needs that metadata,
without decrypting anything:

> process P, run by user U, connected to `api.openai.com`.

## Goal

A new flow monitor records connections to catalog inference domains, with the process and user
that opened them, from OS flow metadata only. It emits `inference_connection` discovery records
through the emitter (task 15).

## Scope

- **`device/capture-core/flowmon`**, a `core.Provider`:
  - collector `flow_monitor` (new `protocol.Collector` constant and `ref.collector` row, component
    `capture_core`, modes `m0`–`m3`);
  - route `net.flow`;
  - `core.Toggled` on `endpoint.flows.enabled`.
- **Sources**, through the ETW session helper from task 19 (`etwsession`), one session with two
  providers. Verify the event ids and field names on the reference host and record the Windows
  build checked in `DECISIONS.md`.
  - `Microsoft-Windows-DNS-Client` (`{1C95126E-7EEA-49A9-A3FE-A378B03DDB4D}`): query-completed
    events (3008) give the query name, the results (addresses) and the PID. Keep a per-PID map
    `address → name` for names matching `Bundle.AppByDomain`, kept 10 minutes.
  - `Microsoft-Windows-Kernel-Network` (`{7DD42A49-5329-4832-8DFD-43D979153A88}`): TCP connect
    events (IPv4 12, IPv6 28) give the PID, destination address and port.
- **Attribution**:
  1. A connect whose destination address appears in that PID's DNS map, or failing that in any
     PID's map from the last 10 minutes, is attributed to that domain's `app_key`.
  2. The process comes from `hostinfo.ProcessInfo` (task 09), and the person is the process
     owner.
  3. Emit `inference_connection` with:
     - basis `flow_metadata`;
     - `app_key` from the domain (for example `app:openai_api`);
     - `destination_host` (the matched name, lower case);
     - `publisher`;
     - the person.
  4. A connect with no name match is ignored. The monitor never records non-catalog destinations.
- **Process context**:
  - The process's own image name is not a contract field. Don't add one.
  - When the process image is itself a catalog app (for example `Cursor.exe`), the record still
    carries the domain's app (`app:anthropic_api`); the app itself is reported by the process
    monitor.
- **Never inspect payloads**; no packet capture.
- **Health**: as in the process monitor (`etw_session_failed`), and `absent` on other platforms.
- **Tests**:
  - with fake DNS and connect event streams:
    - a curl-shaped sequence (DNS, then connect) gives one record with the right user;
    - a connect to an address resolved by another PID within 10 minutes is still attributed;
    - an expired mapping is not;
    - a non-catalog name gives nothing;
    - two connects in a day give one record.
  - a Windows-only elevated test as in task 19.

## Done when

- `cd device/capture-core && go test -race ./flowmon/ ./etwsession/` passes, with the elevated
  test run on the reference host.
- On the reference host with the lab MSI rebuilt and TLS inspection off for the lab tenant:
  1. As the owner, run `curl.exe -s https://api.openai.com/v1/models` (a 401 is fine).
  2. Within seconds an `inference_connection` record arrives for `app:openai_api` with
     `destination_host` `api.openai.com`, the owner's `user_ref`, and publisher `Microsoft
     Windows` (the curl.exe signer).
  3. Repeat as the second local account (task 09) and show the second `user_ref`.
- `node tools/accept.mjs` passes.
