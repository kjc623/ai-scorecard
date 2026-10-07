# Test environment

Every on-device check in these tasks runs on one Windows 11 virtual machine, enrolled into the
Azure **pre-prod** environment's test tenant. The VM is Hyper-V on the owner's PC, Entra-joined and
Intune-managed, set up the way a customer's device is. The local lab (`localdev/`) is not used.
The owner's PC builds nothing for the VM: builds come from CI.

## How a change reaches the VM

1. The implementing agent finishes the code and its tests on the task branch, and reports **ready
   to merge**.
2. The owner merges it to `main`. The push runs the deploy workflow, which builds the signed agent
   release, deploys the services and migrates the database in pre-prod.
3. The agent runs `node tools/testbed/deploy.mjs`. It takes that run's agent release, publishes it
   to the VM through Intune, and waits until the VM runs it.
4. The agent checks the device side with `tools/testbed/invm.ps1`. The owner checks the dashboard
   and changes settings there when a brief asks.

## Values

The owner fills these in once (task 00a for the environment rows, the VM checklist below for the
rest). Agents read this file and never write a secret into it.

| Name | Value | What it is |
|---|---|---|
| Subscription | | Pre-prod's Azure subscription id |
| Resource group | `rg-sac-preprod-eastus` | Pre-prod's resource group |
| Device hostname | | `SAC_DEVICE_FQDN`: the device edge |
| Analyst hostname | | `SAC_ANALYST_FQDN`: the dashboard |
| Test tenant id | | The "Endpoint Test" product tenant (task 00a) |
| Tenant file | `%USERPROFILE%\.sac-testbed\ShadowAICapture.tenant.env` | The test tenant's file from its Intune package; holds the deployment key |
| GitHub repository | | `<org>/<repo>`, for `gh run` |
| VM name | | The Hyper-V VM name, as `Get-VM` shows it |
| Clean checkpoint | `clean-enrolled` | A checkpoint of the VM enrolled in Intune with no agent installed |
| VM admin credential | `%USERPROFILE%\.sac-testbed\vm-admin.xml` | A local administrator on the VM, saved with `Get-Credential \| Export-Clixml` (DPAPI, readable only by the owner's Windows account), for PowerShell Direct |
| Console user | | UPN of the Entra test user signed in at the VM's console |
| Second user | | UPN of a second Entra test user, signed in through fast user switching and left signed in |
| Entra tenant id | | The Microsoft Entra tenant the VM is joined to |
| Intune app registration | | Application (client) id of the publishing app registration |
| Publishing certificate | | Thumbprint of its certificate, in the owner's `Cert:\CurrentUser\My` |
| Test device group | | Object id of the Entra group containing only the VM |
| Intune app id | (task 00b fills this in) | The one Win32 app the testbed publishes to |

## VM checklist (owner, once)

1. **VM.**
   - Create a Windows 11 Pro or Enterprise x64 VM in Hyper-V with Guest Services enabled, on a
     switch with internet access.
   - Join it to Entra and enrol it in Intune. Add it to the test device group.
   - Create a local administrator account for PowerShell Direct. An Intune LAPS-managed account
     works; re-export the credential after a rotation.
2. **Users.**
   - Sign in at the console as the console user.
   - Switch user, sign in as the second user, and switch back. Both sessions stay signed in.
3. **Browser extension.**
   - In Intune, create a Settings catalog profile for Microsoft Edge and Google Chrome, assigned to
     the test device group, as `azure/RUNBOOK.md` §7 describes:
     - `ExtensionInstallForcelist` = `<extension id>;https://<device hostname>/v1/extension/updates.xml`;
     - `NativeMessagingAllowlist` = `com.shadowaicapture.capture_core`.
   - The extension id is in the release's `release.json`.
4. **Publishing app.**
   - Create an Entra app registration with a certificate credential (no secret). Install the
     certificate in the owner's `Cert:\CurrentUser\My` on the PC.
   - Grant Microsoft Graph application permissions, with admin consent:
     - `DeviceManagementApps.ReadWrite.All`
     - `DeviceManagementManagedDevices.Read.All`
     - `DeviceManagementManagedDevices.PrivilegedOperations.All`
5. **Checkpoint.** With both users signed in and no agent installed, take the checkpoint named
   `clean-enrolled`.

## Rules for agents

- **Pre-prod.**
  - Read only. Allowed: `az ... show|list`, `az containerapp logs show`,
    `az monitor log-analytics query`, and `curl` to the public hostnames.
  - Never create, update, delete, restart or run anything in Azure, and never run a workflow.
    Deploys happen only when the owner merges to `main`.
  - Never use the owner's tenant `11111111-1111-1111-1111-111111111111`.
- **The dashboard is the owner's.**
  - Agents have no dashboard sign-in. When a brief needs a setting changed or a page checked, stop
    and tell the owner exactly what to do or look at, and wait.
  - Event delivery is checked on the device side with `invm.ps1 -AgentState`: events emitted,
    spooled, and acknowledged by `/v1/events`. The owner confirms what the dashboard shows.
- **Intune.**
  - Change only the Win32 app whose id is in the table above (or create it once, in task 00b).
  - Assign it only to the test device group.
  - Change no other Intune or Entra object.
- **The VM.**
  - Work in it only through `tools/testbed`.
  - Restore `clean-enrolled` only when a brief says so.
  - Never sign the console user or the second user out.
- **Secrets.** Never print, log or commit:
  - the VM admin credential;
  - the publishing certificate;
  - the deployment key.
- **Timing.** A merge-to-device round takes the deploy workflow plus Intune delivery. Wait with
  `deploy.mjs --wait`, never with fixed sleeps. If a release hasn't arrived 60 minutes after its
  workflow finished, stop and report it.
