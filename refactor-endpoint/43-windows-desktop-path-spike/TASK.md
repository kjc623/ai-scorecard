# 43. Windows desktop path spike

Needs: Claude Desktop and ChatGPT Desktop installed on the reference host and signed in.

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

- With TLS inspection on for the lab tenant (task 08) and attribution in place (task 09), for each
  app record:
  1. whether its API traffic reaches the PAC-routed proxy (the `egress_proxy` counters, or the
     proxy's debug log at `SAC_LOG_LEVEL=debug` set by hand for the spike only);
  2. whether it trusts the device root or pins, per host;
  3. whether it uses QUIC or HTTP/3 to those hosts (`netstat -ano -p UDP` while sending, or
     `pktmon`), and whether it falls back to TCP when UDP 443 is blocked by a temporary Windows
     Firewall rule;
  4. whether a canary prompt is extracted. Use the existing generic extractor; per-app parsing is
     task 45;
  5. added latency to first byte, median and p95 over 50 requests, with and without the PAC.
- If either app bypasses the PAC, prototype a WFP redirect (an ALE connect-redirect callout is
  kernel-mode, so prototype only the user-mode `FWPM_LAYER_ALE_CONNECT_REDIRECT` options) on a
  scratch branch `spike/windows-wfp` that is never merged, and measure the same five things.
- No product code lands from this task except the `DECISIONS.md` note. Revert any firewall rules
  and log levels you changed; the report lists them.
- Recommendation: either "PAC suffices", or "WFP needed", with the production tasks it implies.
  Also state whether task 47 needs the QUIC firewall rule.

## Done when

- `DECISIONS.md` has the note with the Windows build, both app versions and every measurement.
- The reference host is back to its prior state: firewall rules removed and log level reset. The
  report shows `netsh advfirewall firewall show rule name=all | findstr /i spike` returning nothing.
