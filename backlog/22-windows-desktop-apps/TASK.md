# 22. Capture desktop AI apps on Windows

## Problem

`proxy.tls` sees only traffic that is sent to it. Today that is CLI tools, which `cli.shim` points at
the proxy through environment variables, and browsers, which the extension covers. Desktop apps that
use the system proxy settings — the ChatGPT and Claude desktop apps and other Chromium/Electron apps —
are not routed through `proxy.tls`, so their prompts are not captured at all.

## Goal

On Windows, requests from desktop apps to the destinations the policy bundle names
(`interception.seed_hosts`) go through `proxy.tls`; everything else is untouched.

## Decide first (ask the owner)

How the system proxy is set, with your recommendation. The options include a PAC script served by
capture-core and set per user (`AutoConfigURL` in each signed-in user's Internet Settings), or set
machine-wide by policy. Cover what happens on devices where the customer already sets a proxy or PAC
through Intune or Group Policy: that setting must keep working for everything we do not intercept.

## Requirements

- Only the bundle's interception hosts are sent to the proxy; every other request keeps the device's
  existing proxy behaviour.
- Fail open: if capture-core stops or the proxy is unhealthy, the setting is removed or the PAC
  returns the original route, so the user's traffic never breaks. Uninstall removes every change.
- Observations are attributed to the signed-in user, as for the proxy's other traffic.
- Windows only; code in `endpoint/capture-core` (and `installer/` only if the MSI must change).
- Unit tests for the PAC logic and the settings writer; no test may change the test machine's real
  proxy settings.

## Done when

- `go vet` (GOOS=windows, linux, darwin) and `go test ./...` pass in `endpoint/capture-core`;
  `node tools/accept.mjs` passes.
- The owner verifies on a Windows device with the agent installed: a prompt sent from the ChatGPT or
  Claude desktop app appears on the dashboard; browsing a site not in the bundle is unaffected;
  stopping the service restores the previous proxy behaviour.
