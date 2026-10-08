# 59. Reference VM tooling

Needs:
- task 58 finished;
- the owner has completed the VM checklist in `refactor-endpoint/TESTBED.md` and filled in its
  values;
- the owner's `gh` CLI is signed in on the PC with read access to the repository's Actions.

## Problem

Task 60 checks every build task's work on the reference VM (`TESTBED.md`): a Hyper-V VM,
Entra-joined and Intune-managed. It enrols into pre-prod's test tenant and receives the agent the
way a customer device does, through Intune.

Builds come from CI. A push to `main` runs `.github/workflows/deploy.yml`:
- it builds the signed agent release (artifact `agent-release`, version `1.0.<run number>`);
- it deploys the services and migrates the database.

Nothing yet takes that release to the VM, waits for the VM to run it, or runs a check inside the
VM.

## Goal

One command takes the agent release that `main` deployed, publishes it to the VM through Intune,
and waits until the VM runs it and reports to pre-prod. A second command runs a check inside the
VM: as an administrator, as either signed-in Entra user in their own session, or as a screenshot of
the console session.

## Scope

All of this is test tooling under `tools/testbed/`. It changes no product code and uses nothing in
`localdev/`.

- **`tools/testbed/testbed.mjs`**: reads `refactor-endpoint/TESTBED.md`'s value table into a config
  object. It refuses to run when a required value is empty, naming the missing value.
- **`tools/testbed/deploy.mjs`**:
  1. **Release.** Find the successful `deploy.yml` run on `main` for a commit (`--commit <sha>`,
     default `origin/main`'s head) with `gh run list --workflow deploy.yml --branch main --json`.
     - If it is still running, wait for it.
     - If it failed, stop and print its URL.
     - Download its `agent-release` artifact with `gh run download` into
       `%LOCALAPPDATA%\sac-testbed\release\<run>\`.
     - Read the version and the MSI product code from `release.json`.
  2. **Package.** Copy the MSI and the saved tenant file
     (`%USERPROFILE%\.sac-testbed\ShadowAICapture.tenant.env`) into a package folder.
     - Wrap it with Microsoft's Win32 Content Prep Tool (`IntuneWinAppUtil.exe`), downloaded from
       a pinned release of `github.com/microsoft/Microsoft-Win32-Content-Prep-Tool` into
       `%LOCALAPPDATA%\sac-testbed\tools\`, with a SHA-256 recorded in the script checked.
     - This is the same pair the dashboard's Intune package carries.
  3. **Publish** (`tools/testbed/Publish-IntuneBuild.ps1`, Windows PowerShell 5.1):
     - Authenticate with `Microsoft.Graph.Authentication`
       (`Connect-MgGraph -ClientId -TenantId -CertificateThumbprint`), and call Graph only
       through `Invoke-MgGraphRequest`.
     - Follow the documented Win32 LOB app upload sequence:
       1. create a content version;
       2. create the file, with encryption info from the `.intunewin`'s `Detection.xml`;
       3. upload in blocks to the returned Azure Storage URI;
       4. commit;
       5. set `committedContentVersion`.
     - **First run:** create the app, as `azure/RUNBOOK.md` §7 describes.
       - Display name `Shadow AI Capture (pre-prod test)`.
       - Detection: MSI product code with product version, "greater than or equal".

       Write its id into `TESTBED.md`'s "Intune app id" row (the one write to that file).
     - **Later runs:** update that app only. Upload the new content version, and update the
       detection rule's product code and version.
     - Assign the app as **required** to the test device group, if not already assigned.
     - Refuse any app id other than the recorded one.
  4. **Nudge.**
     - Graph `POST /deviceManagement/managedDevices/{id}/syncDevice` for the VM. Find it by its
       Entra device id, read inside the VM with `dsregcmd /status`.
     - Then restart the `IntuneManagementExtension` service inside the VM.
  5. **Wait** (`--wait`, default on). Poll inside the VM until all of these hold:
     - the installed product version (the uninstall registry key) equals the release's version;
     - the `ShadowAICapture` service is running;
     - the agent's `health.json` shows a health report acknowledged by pre-prod after the install
       time, and a resolved device credential.

     Print the elapsed time. Time out after 60 minutes, printing the VM's Intune Management
     Extension log lines for the app (`C:\ProgramData\Microsoft\IntuneManagementExtension\Logs\AppWorkload*.log`).
  - **Flags:** `--commit`, `--wait`, `--uninstall` (assign as "uninstall" instead, for task 50).
  - Never print the deployment key or a credential.
- **`tools/testbed/invm.ps1`**: runs a check in the VM, through PowerShell Direct
  (`Invoke-Command -VMName`, credential imported from the `TESTBED.md` path).
  - `-Command '<script>'` runs as the VM administrator and prints the output.
  - `-AsUser console|second -Command '<script>'` runs inside that user's interactive session:
    1. register a one-shot scheduled task with an `Interactive` logon principal (no password),
       run it, and wait for it;
    2. return its output through a file under `C:\ProgramData\SacTestbed\`;
    3. delete the task.

    Use this mode for anything about HKCU, the user's environment, or a process that must belong
    to that user.
  - `-Screenshot <out.png>` captures the console user's screen (`System.Drawing` `CopyFromScreen`,
    through `-AsUser console`) and copies the PNG to the PC.
  - `-CopyFrom <vm path> <pc path>` and `-CopyTo <pc path> <vm path>`.
  - `-RestoreCheckpoint` restores `clean-enrolled` and waits for PowerShell Direct to answer.
  - `-AgentState` prints the agent's state that later tasks check most often, without any key
    material:
    - the health rows and counters from `health.json`;
    - the spool depth, `dropped_total` and `rejected_total`;
    - the policy bundle version in force;
    - the last health acknowledgement.
- **`tools/testbed/README.md`**: short. Covers:
  - what the VM is;
  - the commands;
  - that `TESTBED.md` is the configuration;
  - that the scripts change only the one Intune app;
  - that the device side is checked here, and the dashboard is checked by the owner.
- **Tests**: `node --test tools/testbed/` covers:
  - `TESTBED.md` table parsing (missing values refused);
  - run selection from `gh` JSON;
  - that no log line contains the deployment key;
  - the refusal of a foreign app id.

  Graph and Hyper-V are proven by the Done when, not mocked. Add the test directory to the
  `static` gate's `node --test` list in `tools/accept.mjs`.

## Done when

- `node tools/testbed/deploy.mjs` installs `main`'s release on the VM through Intune, from the VM
  restored to `clean-enrolled`, and `invm.ps1 -AgentState` shows the device enrolled and
  reporting.
  - Report the elapsed time.
  - The owner confirms the VM appears on the test tenant's Devices page with `managed_state`
    `managed` and the release's version.
- After the next `main` deploy, running it again replaces the first version through Intune.
- `powershell -File tools/testbed/invm.ps1 -Command 'Get-Service ShadowAICapture'` shows it running.
- `-AsUser second -Command 'whoami /upn'` prints the second user's UPN.
- `-Screenshot` returns an image of the console session.
- `node tools/accept.mjs` passes.
