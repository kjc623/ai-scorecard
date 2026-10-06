# 27. Build and install the macOS package

Needs: a Mac (Apple silicon or Intel) with Go 1.27, Node 22 and the Xcode command-line tools, that
can be used as a test device.

## Problem

The macOS package has never been built or installed. `device/installer/build.mjs --os darwin` stages the
binaries, the classifier release, the vendor configuration and the native messaging host manifests,
and `device/installer/macos/build-pkg.sh` should turn that into a `.pkg` with a launchd service, but it has
only been checked with `sh -n` and a cross-compile on Windows.

## Goal

A `.pkg` built from the repository installs on a Mac, runs capture-core as a launchd service,
registers the native messaging host for Chrome and Edge, and uninstalls cleanly.

## Scope

- Build: stage with `device/installer/build.mjs --os darwin` (inputs as in `device/installer/README.md`, using
  throwaway keys passed explicitly), then `device/installer/macos/build-pkg.sh`. Fix whatever fails.
- Install on the Mac with a tenant file pointing at the local lab or pre-prod (the owner supplies the
  device endpoint and a deployment key; never the owner's tenant in the lab).
- Check: the service is running (`launchctl print system/com.shadowaicapture.capture-core`), the
  state directory `/var/db/shadow-ai-capture/state` is 0700 and root-owned, the host manifests exist
  under `/Library/Google/Chrome/NativeMessagingHosts/` and `/Library/Microsoft Edge/NativeMessagingHosts/`,
  the device enrols, and an event from `curl` through a signed-in terminal reaches the dashboard.
- Uninstall, and check nothing is left behind (service, files, trust store entry).
- Add the macOS steps to `device/installer/README.md` (short). Signing and notarisation are out of scope.

## Done when

- The checks above pass on the Mac, and the report lists the commands and their output.
- `node tools/accept.mjs` still passes on Windows.
