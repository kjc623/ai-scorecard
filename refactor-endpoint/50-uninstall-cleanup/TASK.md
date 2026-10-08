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
- `node tools/accept.mjs` passes.

## On the device

On the reference VM, through Intune end to end:
1. Restore the clean checkpoint with `invm.ps1 -RestoreCheckpoint` (no agent installed).
2. Copy `snapshot.ps1` in with `invm.ps1 -CopyTo`, and take the "before" snapshot with
   `invm.ps1 -Command` (as administrator; it reads every loaded user hive, so both signed-in
   users' Internet Settings are covered). Copy `before.json` back with `invm.ps1 -CopyFrom`.
3. Deploy `main`'s release with `node tools/testbed/deploy.mjs`. Ask the owner to enable every
   endpoint collector, hooks, TLS inspection and Ollama capture for the test tenant on the
   Settings page; wait until `invm.ps1 -AgentState` shows the new bundle version in force.
4. Exercise the features: one `claude -p` prompt run with `invm.ps1 -AsUser console -Command`,
   one `curl.exe` through the proxy run with `-AsUser second`, so the second user gets a PAC
   too, and one notification.
5. Uninstall through Intune with `node tools/testbed/deploy.mjs --uninstall`. Wait until
   `invm.ps1 -Command 'Get-Service ShadowAICapture'` reports no such service.
6. Take the "after" snapshot the same way, then run
   `snapshot.ps1 -Compare before.json after.json` on the PC; it exits 0.
7. Paste both commands' output, and the VM's `%WINDIR%\Temp\ShadowAICapture-uninstall.log`
   (read with `invm.ps1 -Command`).

Afterwards, run `node tools/testbed/deploy.mjs` again, so the app is assigned as required and
the VM is enrolled for the next task. The re-enrolled VM may appear as a new device in the test
tenant; tell the owner, so they can revoke the old one if they want.
