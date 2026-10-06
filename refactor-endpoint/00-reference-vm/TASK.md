# 00. Reference VM tooling

Needs: the owner has completed the setup checklist in `refactor-endpoint/TESTBED.md` and filled in
its values, and the lab is running with `LAB_PUBLIC_HOST` set to the lab address.

## Problem

Every later task checks its work on the reference VM (`TESTBED.md`): a Hyper-V VM, Entra-joined
and Intune-managed, that receives the agent the way a customer device does, through Intune. There
is no tooling for any of the three steps:
- **Publishing:** `localdev/lab-msi.mjs` builds an MSI for double-clicking on the PC.
- **Versioning:** its version (`1.0.<hours since 2026>`) repeats within an hour, so Intune would
  not see a second build in the same hour as an update.
- **Reaching the VM:** nothing publishes to Intune, waits for the VM to install, or runs a check
  inside the VM.

## Goal

One command publishes the current tree to the VM through Intune and waits until the VM runs it.
A second command runs a check inside the VM: as an administrator, as either signed-in Entra user
in their own session, or as a screenshot of the console session.

## Scope

All of this is lab tooling under `localdev/testbed/`. It changes no product code.

- **Monotonic lab version** (`localdev/lab-msi.mjs`):
  - Replace `labVersion` with a persisted build counter: `localdev/.msi/build-number`, git-ignored,
    starting at 1.
  - The version is `1.1.<n>`, which is higher than every `1.0.x` already installed.
  - Each build increments `n`. Update `localdev/lab.test.mjs`.
- **`localdev/testbed/testbed.mjs`**: reads `refactor-endpoint/TESTBED.md`'s value table into a
  config object. It refuses to run when a required value is empty, naming the missing value.
- **`localdev/testbed/deploy.mjs`**: builds, publishes and waits, in this order.
  1. **Build**: run `localdev/lab-msi.mjs`.
  2. **Tenant file for the VM**: write `localdev/.testbed/package/ShadowAICapture.tenant.env` with:
     - the lab tenant id;
     - `SAC_DEVICE_ENDPOINT=https://<lab address>:<edge port>`;
     - the lab deployment key, read as `lab-msi.mjs` reads it;
     - no `SAC_CA_FILE`, because the VM trusts the lab CA through the Intune certificate profile.

     Copy the MSI beside it.
  3. **Wrap**: run Microsoft's Win32 Content Prep Tool (`IntuneWinAppUtil.exe`).
     - Download a pinned release from `github.com/microsoft/Microsoft-Win32-Content-Prep-Tool`
       into `localdev/.testbed/tools/`, checking a SHA-256 recorded in the script.
     - Wrap `localdev/.testbed/package/` with setup file `ShadowAICapture.msi`.
  4. **Publish**: `localdev/testbed/Publish-IntuneBuild.ps1`, run with Windows PowerShell 5.1.
     - Authenticate with `Microsoft.Graph.Authentication` (`Connect-MgGraph -ClientId -TenantId -CertificateThumbprint`)
       and call Graph only through `Invoke-MgGraphRequest`.
     - Follow the documented Win32 LOB app upload sequence:
       1. create a content version;
       2. create the file, with encryption info taken from the `.intunewin`'s `Detection.xml`;
       3. upload the encrypted payload in blocks to the returned Azure Storage URI;
       4. commit;
       5. set `committedContentVersion`.
     - **First run:** create the app.
       - Display name `Shadow AI Capture (lab)`.
       - Install command `msiexec /i "ShadowAICapture.msi" /qn`, uninstall command
         `msiexec /x "{product code}" /qn`.
       - Install for the system, Windows 10 21H2+ x64.
       - Detection: the MSI product code with the product version, operator "greater than or
         equal".

       Write its id into `TESTBED.md`'s "Intune app id" row (the one write to that file).
     - **Later runs:** update that app only. Upload the new content version, and update the
       detection rule's product code and version to the new build's.
     - Assign the app as **required** to the test device group, if not already assigned.
     - Refuse any app id other than the recorded one.
  5. **Nudge the VM**:
     - Graph `POST /deviceManagement/managedDevices/{id}/syncDevice` for the VM. Find the
       device by its Entra device id, read inside the VM with `dsregcmd /status`.
     - Then restart the `IntuneManagementExtension` service inside the VM (PowerShell Direct).
  6. **Wait** (`--wait`): poll the lab database read-only, through the lab's PostgreSQL address,
     until the VM's device row (`ops.device`, lab tenant) reports `agent_version` equal to the
     new version. Print the elapsed time.
     - Time out after 60 minutes, printing the VM's Intune Management Extension log lines for the
       app (`C:\ProgramData\Microsoft\IntuneManagementExtension\Logs\AppWorkload*.log`).
  - **Flags:** `--wait` (default on), `--no-build` (republish the last build), `--uninstall`
    (assign as "uninstall" instead, for task 50).
  - Never print the deployment key or the credential.
- **`localdev/testbed/invm.ps1`**: runs a check in the VM, through PowerShell Direct
  (`Invoke-Command -VMName`, credential imported from the `TESTBED.md` path).
  - `-Command '<script>'` runs as the VM administrator and prints the output.
  - `-AsUser console|second -Command '<script>'` runs inside that user's interactive session:
    1. register a one-shot scheduled task with an `Interactive` logon principal (no password),
       run it, and wait for it to finish;
    2. return its output through a file under `C:\ProgramData\SacTestbed\`;
    3. delete the task.

    Use this mode for anything about HKCU, the user's environment, or a process that must belong
    to that user.
  - `-Screenshot <out.png>` captures the console user's screen (`System.Drawing` `CopyFromScreen`,
    in the console session through `-AsUser console`) and copies the PNG to the PC.
  - `-CopyFrom <vm path> <pc path>` and `-CopyTo <pc path> <vm path>`.
  - `-RestoreCheckpoint` restores `clean-enrolled` and waits for the VM to answer PowerShell Direct.
- **`localdev/testbed/README.md`**: short. Covers:
  - what the VM is;
  - the commands above;
  - that `TESTBED.md` is the configuration;
  - that the scripts change only the one Intune app.
- `localdev/.gitignore`: `.testbed/` and `.msi/build-number`.
- **Tests**: `node --test` covers:
  - the version counter;
  - `TESTBED.md` table parsing (missing values refused);
  - the tenant file contents (no `SAC_CA_FILE`, no key in any log line);
  - the refusal of a foreign app id.

  The Graph and Hyper-V calls themselves are proven by the Done when, not mocked.

## Done when

- `node localdev/testbed/deploy.mjs` installs the current tree on the VM through Intune, from an
  empty VM restored to `clean-enrolled`. The VM appears in the lab tenant on the dashboard
  (observed in the browser) with `managed_state` `managed` and the new agent version. Report the
  elapsed time.
- Running it again without changes produces a version one higher, which replaces the first through
  Intune.
- `powershell -File localdev/testbed/invm.ps1 -Command 'Get-Service ShadowAICapture'` shows it
  running.
- `-AsUser second -Command 'whoami /upn'` prints the second user's UPN.
- `-Screenshot` returns an image of the console session.
- `cd localdev && npm test` passes, and `node tools/accept.mjs` passes.
