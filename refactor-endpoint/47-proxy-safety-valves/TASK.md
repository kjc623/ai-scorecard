# 47. Proxy safety valves

## Problem

TLS inspection breaks things, so it needs ways to step out of the way:

- **Pinned apps.** Pinning exclusions are keyed by process and host, but the process name was
  always `unknown` until task 09, so one pinned app could exclude a host for everyone.
- **Unparseable traffic** must reach the server untouched rather than be held or broken.
- **QUIC.** Task 43 recorded whether apps bypass the proxy over QUIC.
- **Kill switch.** The bundle supports `kill_switches`, but the server never emits one. An
  operator has no way to stop interception fleet-wide except the tenant switch, and `DESIGN.md`
  §0 requires a dashboard control.

## Goal

- Pinned traffic is passed through per process and host.
- Unparseable traffic is forwarded unchanged.
- QUIC is handled as task 43 recommended.
- An admin can trip a kill switch per route from the dashboard. Turning off inspection or tripping
  the kill switch restores direct traffic within one policy poll.

## Scope

- **Pinned pass-through** (`proxy/tlsproxy`): the exclusion key is `(process image, host)` from
  task 09's attribution.
  - A handshake failure that looks like pinning adds the key for 24 hours, after which it is
    probed again.
  - While excluded, the connection is blind-tunnelled and counted `blind_tunnelled`.
  - The provider row reports `degraded`/`client_pinned` while any exclusion is active.
- **Unparseable pass-through**: a body that no parser handles, or that is over the body cap, is
  forwarded unchanged, and the event is `confidence: degraded`. Confirm the existing behaviour
  with a test, and change it only if it holds or rewrites the body.
- **QUIC**: follow task 43's note in `DECISIONS.md`.
  - If it found an app falls back to TCP only when UDP 443 is blocked: while interception is on,
    the agent adds one Windows Firewall rule per such app executable (outbound UDP 443, block),
    named `ShadowAICapture QUIC <app_key>`, through the `INetFwPolicy2` COM API with go-ole
    (`DESIGN.md` §11). The executables are the catalog's `windows_exe` signals for those apps.
    The rules are removed when interception is switched off, and at uninstall (task 50).
  - If task 43 found no bypass, record "not needed" and add nothing.
- **Kill switch**:
  - Database: `ops.kill_switch (tenant_id, route, reason_code, effective_at, set_by)`, with
    `route` in the interception routes (`proxy.tls`, `proxy.loopback`), RLS, grants and audit.
  - control-api:
    - `PUT /admin/v1/settings/kill-switch/{route}` with `{on, reason_code}`;
    - `compose()` emits `kill_switches` with `mode: "disable"`.
  - Dashboard: a "Kill switch" control per route on the Settings page, with a required reason.
  - The device already honours `kill_switches` for `proxy.tls`. Make the loopback broker honour
    its own the same way.
  - A tripped switch reports `degraded`/`killed`, and stops enforcement and decryption (blind
    tunnel); it doesn't stop the provider.
- Tests:
  - the exclusion is per process;
  - an excluded connection is tunnelled;
  - a kill switch makes the next request tunnel;
  - compose emits the switch;
  - the dashboard control.

## Done when

- `cd device/capture-core && go test -race ./proxy/...` passes, and the control-api and dashboard
  tests pass.
- On the reference VM, deployed with `node localdev/testbed/deploy.mjs`, with TLS inspection on:
  1. If task 43 called for the QUIC rule:
     `invm.ps1 -Command 'Get-NetFirewallRule -DisplayName "ShadowAICapture QUIC *" | Get-NetFirewallApplicationFilter'`
     lists one rule per app executable. If it didn't, the same command lists none.
  2. Tripping the `proxy.tls` kill switch in the dashboard makes the next Claude Desktop request,
     sent at the VM's console, go direct within one policy poll:
     - the `egress_proxy` row shows `killed`;
     - the `blind_tunnelled` count in the VM's `health.json` rises, read before and after with
       `invm.ps1 -Command`.
  3. Clearing it restores interception.
  4. Turning TLS inspection off removes the PAC and root within one poll, checked with the same
     `invm.ps1` commands as task 08, and removes the QUIC rules (the `Get-NetFirewallRule` command
     above lists none).
  5. Record the times.
- `node tools/accept.mjs` passes.
