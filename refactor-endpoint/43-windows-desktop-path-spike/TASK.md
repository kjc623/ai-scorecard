# 43. Windows desktop path spike

Needs: Claude Desktop and ChatGPT Desktop installed on the reference VM and signed in as the
console user.

## Problem

The plan's Windows spike assumes a WFP (Windows Filtering Platform) redirect to a local proxy.
The agent already routes Windows desktop apps through a per-user proxy auto-config file (PAC)
(`capture-core/winproxy`), which is now behind the tenant's TLS inspection switch (task 08). No
one has checked whether Claude Desktop and ChatGPT Desktop actually:
- honour the PAC;
- trust the device root;
- avoid QUIC.

If they do, WFP is not needed. If they don't, the gap has to be measured before anyone builds a
kernel-adjacent redirect.

## Goal

A go/no-go note in `DECISIONS.md`, with measured numbers, on whether Windows needs a WFP-based
redirect in addition to the PAC path.

## Scope

- Everything runs on the reference VM, running `main`'s release (deployed with
  `node tools/testbed/deploy.mjs`), with TLS inspection on for the test tenant (task 08; ask the
  owner to switch it on on the Settings page, and wait for one policy poll) and attribution in
  place (task 09).
- The apps are used at the VM's console as the console user (owner steps, `AGENTS.md`). Captures are taken with
  `invm.ps1 -Screenshot`, and machine-level observations with `invm.ps1 -Command`.
- For each app, record:
  1. whether its API traffic reaches the PAC-routed proxy: the `egress_proxy` counters in the VM's
     `health.json`, or the proxy's debug log. For debug, set `SAC_LOG_LEVEL=debug` by hand in the
     VM's installed `capture-core.env` with `invm.ps1 -Command`, for the spike only, and restart the
     service;
  2. whether it trusts the device root or pins, per host;
  3. whether it uses QUIC or HTTP/3 to those hosts:
     - `invm.ps1 -Command 'netstat -ano -p UDP'` while sending, or a `pktmon` capture started and
       stopped with `invm.ps1 -Command` and copied back with `-CopyFrom`;
     - whether it falls back to TCP when UDP 443 is blocked by a temporary Windows Firewall rule,
       named with the prefix `spike-` and created with `invm.ps1 -Command`;
  4. whether a canary prompt is extracted. Use the existing generic extractor; per-app parsing is
     task 45;
  5. added latency to first byte, median and p95 over 50 requests, with and without the PAC. Time
     the app's own request with the `pktmon` capture, or replay the app's captured request with
     `curl.exe` through and around the proxy using `invm.ps1 -AsUser console -Command`. Say which
     method was used.
- If either app bypasses the PAC, prototype a WFP redirect (an ALE connect-redirect callout is
  kernel-mode, so prototype only the user-mode `FWPM_LAYER_ALE_CONNECT_REDIRECT` options) on a
  scratch branch `spike/windows-wfp` that is never merged. Run it in the VM (copied in with
  `invm.ps1 -CopyTo`), and measure the same five things.
- No product code lands from this task except the `DECISIONS.md` note. Revert any firewall rules
  and log levels you changed; the report lists them.
- Recommendation: either "PAC suffices", or "WFP needed", with the production tasks it implies.
  Also state whether task 47 needs the QUIC firewall rule.

## Done when

- `DECISIONS.md` has the note with the Windows build, both app versions and every measurement.
- The reference VM is back to its prior state: firewall rules removed, log level reset, and any
  prototype removed. The report shows:
  - `invm.ps1 -Command 'netsh advfirewall firewall show rule name=all | findstr /i spike'`
    returning nothing;
  - `invm.ps1 -Command` reading `SAC_LOG_LEVEL` from the installed `capture-core.env` and showing
    the shipped value.
