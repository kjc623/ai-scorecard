# Reference VM

Every on-device check in these tasks runs on one Windows 11 virtual machine. It is Hyper-V on the
owner's PC, Entra-joined and Intune-managed, so it is set up the way a customer's device is. The
owner's own PC runs the lab and the dashboard browser, and is never a test device.

The owner fills in the values below once, then completes the setup checklist. Task 00 builds the
tooling that uses them. Agents read this file and never write a secret into it.

## Values

| Name | Value | What it is |
|---|---|---|
| VM name | | The Hyper-V VM name, as `Get-VM` shows it |
| Clean checkpoint | `clean-enrolled` | A checkpoint of the VM enrolled in Intune with no agent installed |
| VM admin credential | `%USERPROFILE%\.sac-testbed\vm-admin.xml` | A local administrator on the VM, saved with `Get-Credential \| Export-Clixml` (DPAPI, readable only by the owner's Windows account), used for PowerShell Direct |
| Console user | | UPN of the Entra test user signed in at the VM's console |
| Second user | | UPN of a second Entra test user, signed in on the VM through fast user switching and left signed in |
| Entra tenant id | | The Microsoft Entra tenant the VM is joined to |
| Intune app registration | | Application (client) id of the publishing app registration |
| Publishing certificate | | Thumbprint of its certificate, in the owner's `Cert:\CurrentUser\My` |
| Test device group | | Object id of the Entra group containing only the VM |
| Intune app id | (task 00 fills this in) | The one Win32 app the agent publishes to |
| Lab address | | `LAB_PUBLIC_HOST`: the PC's address as the VM reaches it, stable across reboots |

## Owner setup checklist

1. **VM.**
   - Create a Windows 11 Pro or Enterprise x64 VM in Hyper-V with Guest Services enabled.
   - Join it to Entra and enrol it in Intune. Add it to the test device group.
   - Create the local administrator account for PowerShell Direct. An Intune LAPS-managed account
     works; re-export the credential after a rotation.
2. **Network.**
   - Connect the VM to a Hyper-V switch where it reaches the PC at a fixed address: an external
     switch, or an internal switch with a static host IP. The Default Switch's address changes on
     reboot.
   - Set `LAB_PUBLIC_HOST` in `localdev/.env` to that address, then restart the lab.
   - Check from the VM: `Test-NetConnection <lab address> -Port 8443`.
3. **Users.**
   - Sign in at the console as the console user.
   - Switch user, sign in as the second user, and switch back. Both sessions stay signed in.
4. **Lab CA trust.**
   - In Intune, create a Trusted certificate profile (Windows, Computer root) from
     `localdev/.authlab/dev-ca.crt`, assigned to the test device group.
   - This is what a customer would never need, since pre-prod uses a public certificate. It
     replaces the lab installer's `SAC_CA_FILE`, which points at a file on the PC.
5. **Browser extension.**
   - In Intune, create a Settings catalog profile for Microsoft Edge and Google Chrome, assigned to
     the test device group:
     - `ExtensionInstallForcelist` = `<extension id>;https://<lab address>:8443/v1/extension/updates.xml`;
     - `NativeMessagingAllowlist` = `com.shadowaicapture.capture_core`.
   - The extension id is in `device/installer/README.md`.
6. **Publishing app.**
   - Create an Entra app registration with a certificate credential (no secret). Install the
     certificate in the owner's `Cert:\CurrentUser\My` on the PC.
   - Grant Microsoft Graph application permissions, with admin consent:
     - `DeviceManagementApps.ReadWrite.All`
     - `DeviceManagementManagedDevices.Read.All`
     - `DeviceManagementManagedDevices.PrivilegedOperations.All`
7. **Checkpoint.** With both users signed in and no agent installed, take the checkpoint named
   `clean-enrolled`.

## Rules for agents

- **Intune.**
  - Change only the Win32 app whose id is in the table above (or create it once, in task 00).
  - Assign it only to the test device group.
  - Change no other Intune or Entra object, and never read or change device policy.
  - The publish script enforces this and refuses any other app id.
- **The VM.**
  - Work in it only through `localdev/testbed`.
  - Restore `clean-enrolled` only when a brief says so: task 50, or a task whose first install
    must start clean.
  - Never sign the console user or the second user out.
- **Secrets.** Never print, log or commit:
  - the VM admin credential;
  - the publishing certificate;
  - the lab deployment key.
- **Timing.** Intune delivery is slow. Wait with `deploy.mjs --wait`, which polls the agent's own
  health report, never with fixed sleeps. If a build hasn't arrived within 60 minutes, stop and
  report it.
