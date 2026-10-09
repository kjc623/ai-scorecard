# Test environment

Every on-device check in these tasks runs in the device phase (tasks 58–60, `AGENTS.md`), on one
Windows 11 virtual machine, enrolled into the **pre-prod** environment's test tenant. Pre-prod
runs on SaaS: the services on Fly.io and PostgreSQL on Supabase (task 58). The VM is Hyper-V on
the owner's PC, Entra-joined and Intune-managed, set up the way a customer's device is. The local
lab (`localdev/`) is not used.
The owner's PC builds nothing for the VM: builds come from CI.

## How a change reaches the VM

1. The agent finishes the code and its tests on the branch (a fix branch, in the device phase),
   and reports **ready to merge**.
2. The owner merges it to `main`. The push runs the deploy workflow, which builds the signed agent
   release, deploys the services and migrates the database in pre-prod.
3. The agent runs `node tools/testbed/deploy.mjs`. It takes that run's agent release, publishes it
   to the VM through Intune, and waits until the VM runs it.
4. The agent checks the device side with `tools/testbed/invm.ps1`. The owner checks the dashboard
   and changes settings there when a brief asks.

## Values

The owner fills these in once (task 58 for the environment rows, the VM checklist below for the
rest). Agents read this file and never write a secret into it.

| Name | Value | What it is |
|---|---|---|
| Fly.io organization | `personal` | The Fly.io organization pre-prod's apps run in |
| Fly.io app prefix | `sac-preprod` | Pre-prod's apps are `<prefix>-<component>`, e.g. `sac-preprod-ingest-api` |
| Fly.io read-only token | `%USERPROFILE%\.sac-testbed\fly-readonly.token` | A `fly tokens create readonly` token for that organization, for the read-only checks below |
| Supabase project ref | `hbrvtbiifkmgdcmozxcx` | Pre-prod's Supabase project, which holds its PostgreSQL database |
| Device hostname | `devices.preprod.sundial.solutions` | `SAC_DEVICE_FQDN`: the device edge |
| Analyst hostname | `console.preprod.sundial.solutions` | `SAC_ANALYST_FQDN`: the dashboard |
| Test tenant id | `84beb829-b508-437c-9cd2-501f36e28b81` | The "Endpoint Test" product tenant (task 58) |
| Tenant file | `%USERPROFILE%\.sac-testbed\ShadowAICapture.tenant.env` | The test tenant's file from its Intune package; holds the deployment key |
| GitHub repository | `kjc623/ai-scorecard` | `<org>/<repo>`, for `gh run` |
| VM name | `WIN11-TEST` | The Hyper-V VM name, as `Get-VM` shows it |
| Clean checkpoint | `clean-enrolled` | A checkpoint of the VM enrolled in Intune with no agent installed |
| VM admin credential | `%USERPROFILE%\.sac-testbed\vm-admin.xml` | A local administrator on the VM, saved with `Get-Credential \| Export-Clixml` (DPAPI, readable only by the owner's Windows account), for PowerShell Direct |
| Console user | `kyle@sundial.solutions` | UPN of the Entra test user signed in at the VM's console |
| Second user | | Optional: UPN of a second Entra test user, signed in through fast user switching and left signed in. Empty: one user, and the checks that need a second session are reported as not run |
| Entra tenant id | `5ac3954b-f7e1-47af-bbab-2ca027d1948f` | The Microsoft Entra tenant the VM is joined to |
| Intune app registration | `6013fcd8-b0fe-4201-9e0c-5bb4a78e7b57` | Application (client) id of the publishing app registration |
| Publishing certificate | `300591A257424963C75D23327A40BED95F0156C5` | Thumbprint of its certificate, in the owner's `Cert:\CurrentUser\My` |
| Test device group | `6a3c290c-2ff1-4c44-bba9-ca6ed29caed6` | Object id of the Entra group containing only the VM |
| Intune app id | (task 59 fills this in) | The one Win32 app the testbed publishes to |

## VM checklist (owner, once)

1. **VM.**
   - Create a Windows 11 Pro or Enterprise x64 VM in Hyper-V with Guest Services enabled, on a
     switch with internet access.
   - Join it to Entra and enrol it in Intune. Add it to the test device group.
   - Create a local administrator account for PowerShell Direct. An Intune LAPS-managed account
     works; re-export the credential after a rotation.
2. **Users.**
   - Sign in at the console as the console user.
   - With a second user: switch user, sign in as them, and switch back. Both sessions stay signed in.
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
  - Read only. Allowed: `fly status`, `fly machine list`, `fly ips list` and `fly logs` for
    pre-prod's apps, with `FLY_API_TOKEN` set from the read-only token file; `gh run view` for the
    deploy workflow's runs; and `curl` to the public hostnames.
  - Service logs: `fly logs --no-tail` returns only the latest lines. To search a window, start
    `fly logs -a <app>` writing to a file before the window opens and stop it after it closes. If a
    window was missed, ask the owner to search it in Fly.io's log search, which keeps 7 days.
  - Never create, update, delete, restart or run anything on Fly.io or Supabase, and never run a
    workflow. Deploys happen only when the owner merges to `main`.
  - The database is the owner's: agents have no Supabase access (dashboard, SQL or logs).
  - Never use the owner's tenant `11111111-1111-1111-1111-111111111111`.
- **The dashboard is the owner's.**
  - Agents have no dashboard sign-in. When a brief needs a setting changed or a page checked, stop
    and tell the owner exactly what to do or look at, and wait.
  - Event delivery is checked on the device side with `invm.ps1 -AgentState`: events emitted,
    spooled, and acknowledged by `/v1/events`. The owner confirms what the dashboard shows.
- **Intune.**
  - Change only the Win32 app whose id is in the table above (or create it once, in task 59).
  - Assign it only to the test device group.
  - Change no other Intune or Entra object.
- **The VM.**
  - Work in it only through `tools/testbed`.
  - Restore `clean-enrolled` only when a brief says so.
  - Never sign the console user, or the second user if there is one, out.
- **Secrets.** Never print, log or commit:
  - the VM admin credential;
  - the publishing certificate;
  - the Fly.io read-only token;
  - the deployment key.
- **Timing.** A merge-to-device round takes the deploy workflow plus Intune delivery. Wait with
  `deploy.mjs --wait`, never with fixed sleeps. If a release hasn't arrived 60 minutes after its
  workflow finished, stop and report it.
