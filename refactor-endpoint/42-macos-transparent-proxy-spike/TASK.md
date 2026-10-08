# 42. macOS transparent proxy spike

This task has no build step: it runs in the device phase (task 60), on the Mac. It needs no
pre-prod.

Needs, in the device phase (task 60):
- a Mac (Apple silicon) with Xcode, Go 1.27, and Claude Desktop and ChatGPT Desktop signed in;
- an Apple developer account that can sign a Network Extension (system extension) for local
  development, with SIP and system-extension developer mode as Apple's documentation requires.

## Problem

On Windows, desktop apps reach the proxy through a per-user proxy auto-config file (PAC). On macOS
the agent has no desktop-app path at all. The plan proposes a transparent proxy network
extension (`NETransparentProxyProvider`) that takes only connections to allow-listed AI domains
and hands them to a local TLS-terminating proxy. Nobody knows yet whether it works with Claude
Desktop and ChatGPT Desktop, or what it costs in latency.

## Goal

A go/no-go note in `DECISIONS.md`, with measured numbers, on building a macOS transparent proxy
for desktop AI apps.

## Scope

- Build a throwaway prototype on a scratch branch named `spike/macos-transparent-proxy`, which is
  never merged:
  - a Swift system extension with a `NETransparentProxyProvider` whose rules match only TCP 443 to
    the bundle's `interception.seed_hosts` for Claude and ChatGPT;
  - a containing app that activates it;
  - claimed flows forwarded to the existing Go TLS proxy (`capture-core/proxy/tlsproxy`), run by
    hand on the Mac, with its per-device root installed in the System keychain by hand.
- No product code, installer or repository change lands from this task, except the
  `DECISIONS.md` note. The repository rule is production code only.
- Measure and record:
  1. whether a prompt typed in each app is read by the proxy (paste the extracted text of a canary
     prompt, not a real one);
  2. whether each app trusts the device root or pins (and which hosts);
  3. whether each app uses QUIC or HTTP/3 to those hosts, and whether denying UDP 443 in the
     extension makes it fall back;
  4. added latency to first byte, median and p95 over 50 requests, with and without the
     extension;
  5. what an administrator must approve through MDM: the system extension, the network extension
     content-filter or proxy payload, and Full Disk Access if needed. Name the exact payload types.
- Recommendation: go or no-go, and if go, the list of production tasks it implies (packaging,
  signing, MDM profiles, and an agent ↔ extension IPC).

## Done when

- `DECISIONS.md` has the note, with the macOS version, both app versions and every measurement
  above.
- The scratch branch is pushed nowhere and isn't merged. The report states its name and that it
  stays local.
