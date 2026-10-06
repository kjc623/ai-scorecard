# 44. Non-exportable device CA

## Problem

capture-core mints a per-device interception root and keeps its private key as a file in the
protected state directory (`capture-core/proxy/tlsproxy/ca.go`). An administrator, or malware
running as SYSTEM, can copy that file and impersonate any intercepted host to this device. The
plan asks for a key the OS keystore holds and refuses to export.

The plan also says "deliver trust through an MDM profile". A per-device root is different on every
device, so a tenant-wide Intune profile can't carry it. Trust stays installed by the agent, in the
machine Root store, as today (`capture-core/trust`).

## Goal

On Windows, the device root's private key is generated in CNG (Microsoft Software Key Storage
Provider, machine key) with export policy none. Leaf certificates are signed through that key.
Exporting the key fails.

## Scope

- **`capture-core/proxy/tlsproxy/ca.go`**:
  - Behind a `caKeyStore` seam, Windows gets the CNG-backed implementation, using
    `github.com/google/certtostore` (`DESIGN.md` §11):
    - a machine-scope key named `ShadowAICapture-DeviceRoot`;
    - RSA 3072 or P-256, whichever certtostore supports with an explicit non-exportable policy.
      Verify, and record the choice in `DECISIONS.md`;
    - `NCRYPT_ALLOW_EXPORT_FLAG` unset.
  - The root certificate (public) stays a file in the state directory. Leaf signing uses the
    returned `crypto.Signer`.
  - macOS and Linux keep the file key. Tasks 53 and 55 decide their keystores.
- **Migration**: an existing file key is replaced on first start of the new version:
  1. Mint a new root in CNG.
  2. Remove the old root from the trust store.
  3. Delete the key file.
  4. Install the new root.
  Old leaves expire naturally.
- **Lifecycle**: the TLS proxy's toggle (task 08) installs and removes the root's trust, but keeps
  the CNG key across toggles. Uninstall (task 50) deletes the key.
- **Deviation**: add a `DECISIONS.md` entry saying trust is installed by the agent, not by an MDM
  profile, and why.
- **Tests**:
  - a Windows test that mints a root through the seam and confirms `NCryptExportKey` with
    `BCRYPT_PRIVATE_KEY_BLOB` / PKCS#8 fails with `NTE_NOT_SUPPORTED` or
    `NTE_PERM`-equivalent;
  - a leaf signed through it verifies.

## Done when

- `cd device/capture-core && go test -race ./proxy/tlsproxy/` passes on Windows.
- On the reference VM, deployed with `node localdev/testbed/deploy.mjs` over the previous build
  (so the migration runs), with TLS inspection on:
  1. `invm.ps1 -Command 'certutil -store Root'` lists the device root, and only one Shadow AI
     Capture root.
  2. `invm.ps1 -Command 'certutil -csp "Microsoft Software Key Storage Provider" -key'` lists
     `ShadowAICapture-DeviceRoot`.
  3. An export attempt fails, with the error pasted into the report. Run `certutil -exportPFX` of
     the root as SYSTEM, from an `invm.ps1 -Command` script that registers and runs a one-shot
     scheduled task under `NT AUTHORITY\SYSTEM`, captures its output, then deletes the task.
  4. A proxied request still succeeds:
     `invm.ps1 -AsUser console -Command 'curl.exe -s -o NUL -w "%{http_code}" https://api.openai.com/v1/models'`.
- No key file remains in the state directory. Listing it with `invm.ps1 -Command`
  (`Get-ChildItem C:\ProgramData\ShadowAICapture\state -Recurse`) shows no device-root key file.
- `node tools/accept.mjs` passes.
