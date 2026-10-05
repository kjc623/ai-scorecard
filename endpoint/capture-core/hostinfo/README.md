# hostinfo — what the operating system says about the device and the person at it

The enterprise path (contract §4, §5) needs three things the agent cannot be configured with,
because one tenant package goes to every device: an attestation the server checks against the
customer's MDM, a hardware seed so each device enrols under its own idempotency key, and the person
at the console, from whom `user_ref` is derived. This package reads them. Every reader sits behind a
seam, so the selection rules are tested on any OS; only `windows.go` touches the real machine, through
the standard library's `syscall` and lazily loaded system DLLs (no `golang.org/x/sys`).

| Fact | Where Windows states it | Rule |
|---|---|---|
| Intune device id | `HKLM\SOFTWARE\Microsoft\Enrollments\<guid>` with `ProviderID = MS DM Server`, value `DMClient\MS DM Server\EntDMID` | An active enrolment (`EnrollmentState = 1`) is preferred over a lingering one; two different ids send none |
| Entra device id | `HKLM\SYSTEM\CurrentControlSet\Control\CloudDomainJoin\JoinInfo\<thumbprint>` names the device certificate in `LocalMachine\My`, issued by `MS-Organization-Access`, whose subject CN is the id | With no join state, one such certificate is used; more than one sends none. Entra-joined and hybrid-joined devices both have it |
| Serial number | SMBIOS structure type 1, read with `GetSystemFirmwareTable('RSMB')` | The field `Win32_BIOS.SerialNumber` and Intune's `serialNumber` read. `HKLM\HARDWARE\DESCRIPTION\System\BIOS` does not carry it. Sent as stated, OEM placeholder included |
| Hardware seed | SMBIOS system UUID (and the serial when it is real), else `MachineGuid` | Never a placeholder: an all-zero UUID, a known board constant or "To be filled by O.E.M." would collide across devices |
| Console user | `WTSGetActiveConsoleSessionId` → `WTSQueryUserToken` → the token's SID and `DOMAIN\user` | Only a service may ask; a console run falls back to its own user, never to `NT AUTHORITY\SYSTEM` |
| UPN | `HKLM\SOFTWARE\Microsoft\IdentityStore\Cache\<SID>\IdentityCache\<SID>` value `UserName`, else `GetUserNameEx(NameUserPrincipal)` / `TranslateName` while impersonating the user | Asked once per account, retried at most every 10 minutes; a value without `@` is not a UPN |
| Entra object id | An `S-1-12-1-a-b-c-d` SID: the four sub-authorities are the GUID's bytes, little-endian | Any other SID carries none |

`User.Ref` applies contract §4's order: UPN, else object id, else `DOMAIN\user`, under the tenant's
key (`protocol.DeriveUserRef`).

On this build host (Windows 11, not joined, not enrolled, not elevated) the readers return the
SMBIOS UUID and serial WMI reports, `MachineGuid`, no Intune or Entra id, and the process user (the
console query is refused without the service's privilege). What only a joined and enrolled device
shows — the Intune and Entra ids, a cached UPN, the console query under LocalSystem — is not
verified here.
