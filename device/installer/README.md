# installer

Builds the agent packages and the browser extension release. Each platform gets one generic package
that carries code only: `capture-core`, `classifier-host`, the signed classifier release, the
vendor-wide `capture-core.env` (the state directory, the classifier release, and the pinned policy
and classifier public keys; everything else comes from the signed policy)
and the extension's native messaging host. A tenant is attached by `ShadowAICapture.tenant.env`
(`SAC_TENANT_ID`, `SAC_DEVICE_ENDPOINT`, `SAC_DEPLOYMENT_KEY`), which control-api writes beside the
package for each download; the installer copies it to `tenant.env`, read after the vendor file.

## Build

| Input | Flag | |
|---|---|---|
| Version | `--version` | `major.minor.build`; the MSI ProductVersion and the extension version. Raise it for every release |
| Policy public key | `--policy-key-file` | public half of `sac-policy-signing-key` (64 hex digits or PEM) |
| Policy key id | `--policy-key-id` | the key id control-api signs policy with |
| Classifier signing key | `--classifier-key` | hex Ed25519 seed the classifier release is signed with |
| Classifier rules and model | `--classifier-rules`, `--classifier-model` | `device/classifier-host/rules/default.json` and `model.json` |
| Extension signing key | `--extension-key` or `SAC_EXTENSION_SIGNING_KEY` | RSA key whose public half is `device/extension/manifest.json`'s `key` |
| Extension update URL | `--extension-update-url` | `https://<analyst-fqdn>/v1/extension/updates.xml`, packaged as the extension's `update_url` |

Windows release, on Windows with Go 1.27, Node 22 and WiX 7 (`dotnet tool install --global wix`):

```
npm ci --prefix extension
node device/installer/release-msi.mjs --version 1.4.0 --policy-key-file policy.pub --policy-key-id policy-key-1 \
  --classifier-key classifier.key --classifier-rules device/classifier-host/rules/default.json \
  --classifier-model device/classifier-host/rules/model.json --extension-key extension.pem \
  --extension-update-url https://<analyst-fqdn>/v1/extension/updates.xml --wix-eula wix7 [--sign]
```

`device/installer/dist/release/` then holds `ShadowAICapture.msi`, `shadow-ai-capture.crx`,
`release.json` (the MSI's version, product, upgrade and package codes, sha256, size, publisher and
signature state, the install and uninstall commands and the detection key, the pinned trust anchors,
and `extension: {id, version, file, sha256, size}`) and `agent-release.json`, the signed statement
agents update from (below). This directory is control-api's agent release (the `agent-release` build
context of its image): control-api wraps the MSI into each tenant's package and serves the agent
update, the CRX and the extension update manifest from it. The build fails closed on a missing input,
on generated files that differ from `manifest.mjs`, and on any check of the built package.

`--sign` Authenticode-signs the executables and the MSI with `signtool` and one of: Trusted Signing
(`SAC_SIGN_DLIB` = `Azure.CodeSigning.Dlib.dll`, `SAC_SIGN_METADATA` = its `metadata.json`), a
certificate thumbprint (`SAC_SIGN_THUMBPRINT`) or a PFX (`SAC_SIGN_PFX`, `SAC_SIGN_PFX_PASSWORD`).

Linux and macOS: `node device/installer/build.mjs --os linux|darwin --arch amd64|arm64` with the same inputs.
Linux produces `device/installer/dist/shadow-ai-capture-<version>-linux-<arch>.tar.gz` (unpack it with the
tenant file beside `install.sh`, then `sudo sh install.sh`); on a Mac,
`device/installer/macos/build-pkg.sh --stage device/installer/.stage/darwin-<arch> --version <v> --out device/installer/dist --sign "Developer ID Installer: …"`
produces `ShadowAICapture.pkg`, which reads the tenant file from beside the `.pkg`.

`node device/installer/verify.mjs` is the gate: the catalogue against capture-core's flags, the generated
files against `manifest.mjs` (`node device/installer/render.mjs` regenerates them), the three service
definitions and host registrations, and `capture-core --print-config` over the generic file and a
tenant file. `--release device/installer/dist/release` also checks a built release; `--no-exec` skips Go.

## Deploying with Intune

**The agent.** In the dashboard, Settings → Deployment → download the Intune package (`.intunewin`
holding the signed MSI and this tenant's `ShadowAICapture.tenant.env`, with a deployment key minted
for the download; treat it as a secret). Intune admin center → Apps → Windows → Add → Windows app
(Win32): install command `msiexec /i ShadowAICapture.msi /qn`, install behaviour System, requirement
Windows 10 21H2 or later x64, assignment Required. The agent updates itself (below), so neither the
detection rule nor the uninstall command names this build's ProductCode (`INSTALLED` in
`manifest.mjs`): detection is the registry value `HKEY_LOCAL_MACHINE\SOFTWARE\ShadowAICapture`
`Version`, version comparison, greater than or equal to the release version, 64-bit view; the
uninstall command removes whichever product carries the UpgradeCode:

```
powershell.exe -NoProfile -NonInteractive -Command "foreach ($p in (New-Object -ComObject WindowsInstaller.Installer).RelatedProducts('{7E9C2B7A-6D0E-4C6A-9F2B-1A6E6C2D44A1}')) { $r = (Start-Process msiexec.exe -ArgumentList '/x',$p,'/qn' -Wait -PassThru).ExitCode; if ($r -ne 0) { exit $r } }"
```

An install with no tenant file beside the MSI fails with 1603 before changing anything (`msiexec /l*v`
logs the reason under action `CheckTenantConfig`).

**Updates.** Each release build also writes `agent-release.json`: the MSI's version, file name,
sha256 and size, signed with the classifier signing key, whose public half every package pins
(`SAC_CLASSIFIER_PUBKEY`). control-api serves it and the MSI to enrolled devices
(`GET /v1/agent/release`, `GET /v1/agent/package`). Every 15 minutes the Windows agent verifies the
statement under the pinned key, and when it names a newer version downloads the MSI (resuming over
several requests on a slow link), checks its size and sha256, copies its own `tenant.env` beside it
as `ShadowAICapture.tenant.env`, and runs `msiexec /i … /qn /norestart`, which upgrades it in place
and keeps its enrolment. The download, the installer log and the attempt record are under
`state\update`; a version whose install does not take is retried after 6 hours, and the health
document's `agent_update` says what the last check found. It never installs an older version, an
unsigned statement or a package that differs from it, and a development build never updates.

**The extension.** Force-install it in both browsers with `ExtensionInstallForcelist`, value
`<extension id>;https://<analyst-fqdn>/v1/extension/updates.xml` (the id is `release.json`'s
`extension.id`). In Intune, Devices → Configuration → Create → Windows 10 and later → Settings catalog:

- Google Chrome → Extensions → *Configure the list of force-installed apps and extensions*
- Microsoft Edge → Extensions → *Control which extensions are installed silently*

The policy's URL serves the first install only; the installed extension checks for updates at its
packaged `update_url`, the same manifest, so a deploy reaches browsers at their next update check
(about every five hours). Chrome and Edge install an extension from outside their stores only on a managed device, which an
Intune-enrolled device is, and only a policy-installed extension is granted `webRequestBlocking`.
A tenant that restricts native messaging must allow `com.shadowaicapture.capture_core`
(`NativeMessagingAllowlist`).

**What the MSI changes.** A LocalSystem service `ShadowAICapture` started at install and at boot;
`C:\Program Files\ShadowAICapture` (binaries, classifier release, host manifest);
`C:\ProgramData\ShadowAICapture`, whose `profile` (vendor and tenant files) and `state` (including the
service log `state\capture-core.log`) only SYSTEM and Administrators can read, and whose `cli` (the CLI
shim's CA bundle and proxy script) every user can read; the native messaging host keys under `HKLM\SOFTWARE\Google\Chrome` and
`HKLM\SOFTWARE\Microsoft\Edge`; a *Shadow AI Capture* Start-menu shortcut for all users, whose
AppUserModelID (`ShadowAICapture.Agent`) Windows requires before it shows the agent's notifications.
While the service runs, as the tenant's settings switch them on, the agent also changes:

- the tools' machine-wide managed settings: `C:\Program Files\ClaudeCode\managed-settings.json`,
  `C:\ProgramData\OpenAI\Codex\requirements.toml` and `config.toml`, `C:\ProgramData\Cursor\hooks.json`,
  and Copilot's values under `HKLM\SOFTWARE\Policies\Microsoft\VSCode` and in the machine environment;
  each file or value is backed up before the agent first writes it;
- the machine environment's `OLLAMA_HOST`;
- each signed-in user's `AutoConfigURL` (Internet Settings), pointed at the desktop-app PAC;
- its interception root in the machine Root store, whose key is the CNG machine key
  `ShadowAICapture-DeviceRoot`;
- the CLI shim's proxy and CA variables in the machine environment;
- any `ShadowAICapture QUIC *` firewall rule.

Stopping the service takes the root, the PAC, the tools' settings and the CLI environment back out.
A full uninstall then runs `capture-core --uninstall-cleanup` as SYSTEM, before the files are removed:
it removes or restores each of the locations above from the backups, also after a crash, restores
`OLLAMA_HOST`, deletes the CNG key, and writes each step's outcome to
`%WINDIR%\Temp\ShadowAICapture-uninstall.log`; its exit code is ignored, so it never blocks the
uninstall. Uninstalling then removes the service, the files, the keys, the shortcut and the data
folder; an upgrade keeps the data folder and runs no cleanup.

`windows\snapshot.ps1` checks that an uninstall left the machine as it was: run it as an
administrator before the install and after the uninstall (`-Out before.json`, `-Out after.json`),
then `snapshot.ps1 -Compare before.json after.json` prints every difference and exits 1 if there is
one.
