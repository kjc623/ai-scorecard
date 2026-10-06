# 50. Uninstall restores the machine

## Problem

The agent now changes things outside its own folders:
- tool managed settings (Claude Code, Codex, Copilot, Cursor);
- `OLLAMA_HOST`;
- the device root and its CNG key;
- the PAC in each user's Internet Settings;
- QUIC firewall rules;
- the Start-menu shortcut for notifications.

Today the MSI's uninstall removes the service and `ProgramData` (`device/installer/generated/windows/ShadowAICapture.wxs`,
`RemoveData`). Stopping the service removes the root and the CLI shim, but nothing restores the
tool configs. A machine whose agent is uninstalled must be left as it was found.

## Goal

Uninstalling the MSI, through Intune or Settings → Apps, returns the machine to its pre-install
state for every location the agent touched. A script proves it by comparing snapshots.

## Scope

- **Cleanup mode** (`cmd/capture-core`): `capture-core --uninstall-cleanup`, run as SYSTEM, does
  the following:
  1. For each `toolconfig` writer: remove exactly the agent's keys, restore any keys it replaced
     from `toolconfig/<tool>/original` (`DESIGN.md` §8), and delete a file the agent created
     (rather than modified) if it is now empty of others' keys.
  2. Restore `OLLAMA_HOST` (task 57).
  3. Remove every PAC `AutoConfigURL` the agent set, restoring each user's original
     (`winproxy`'s existing restore).
  4. Remove the root from the trust store, and delete the CNG key (task 44).
  5. Remove the `ShadowAICapture QUIC *` firewall rules (task 47).
  6. Remove the CLI shim's machine environment.

  It reports each step and exits 0 even if a step fails: uninstall must not be blocked. Each
  step's outcome is written to `%WINDIR%\Temp\ShadowAICapture-uninstall.log`, outside
  `ProgramData`, so it survives that folder's removal.

  This is the uninstall path, not a feature switch: the MSI is its only caller.
- **MSI** (`device/installer/manifest.mjs` → regenerate `generated/windows/ShadowAICapture.wxs`):
  - A deferred, no-impersonate custom action runs the cleanup:
    - after `StopServices`;
    - before `RemoveFiles`;
    - only when `REMOVE="ALL" AND NOT UPGRADINGPRODUCTCODE`.
  - The action's return is ignored.
  - Update `installer/verify.mjs` `releaseChecks` and the generated-file checks to assert the
    action and its condition.
  - Update `device/installer/README.md`'s "what the MSI changes" list.
- **Snapshot script** (`device/installer/windows/snapshot.ps1`): writes a JSON snapshot of every
  location above:
  - the files' hashes;
  - the registry values under each loaded user hive's Internet Settings;
  - the machine environment;
  - Root store thumbprints;
  - CNG key names;
  - firewall rule names;
  - the Start-menu shortcut.

  `-Compare a.json b.json` prints differences and exits 1 if any. It is a release check, so it
  lives with the installer scripts.
- Tests: a Go test of the cleanup over fake seams (a tool config with customer keys and a backup,
  a PAC, an environment variable): after cleanup, each equals its original.

## Done when

- `cd device/capture-core && go test -race ./cmd/capture-core/ ./toolconfig/` passes.
- `node device/installer/verify.mjs` passes.
- On the reference host:
  1. `snapshot.ps1 > before.json`.
  2. Install the lab MSI, enable every collector, hooks and TLS inspection for the lab tenant,
     and let one policy poll apply them.
  3. Uninstall from Settings → Apps.
  4. `snapshot.ps1 > after.json`, then `snapshot.ps1 -Compare before.json after.json` exits 0.
  5. Paste both commands' output.
  Afterwards, reinstall the lab MSI so the owner's lab device is enrolled again.
- `node tools/accept.mjs` passes.
