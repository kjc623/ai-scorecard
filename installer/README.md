# installer — the endpoint installer

This packages the device tier the way [docs/05-platform-delivery.md](../docs/05-platform-delivery.md)
§6 describes it: **one artefact per platform carrying code only**. The tenant association is one
small file, `ShadowAICapture.tenant.env`, that travels **beside** the artefact in the package an
admin downloads from Settings → Deployment, and the installer copies it in. One signed MSI serves
every tenant, and the install command takes no properties: `msiexec /i ShadowAICapture.msi /qn`.

The release artefacts are the WiX source for Windows, the `pkgbuild` script for macOS, and the
`install.sh` package for Linux. All three read the same payload and the same configuration catalogue
from [`manifest.mjs`](manifest.mjs), and the committed output under [`generated/`](generated) is
checked for drift the way [`contracts/generated`](../contracts/generated) is — one source, generated
output, a `--check` that fails on drift.

> **Linux is a supported endpoint platform.** The `ops.device.os` constraint admits
> `windows | macos | linux`, and `control-api` accepts it. This is not a development accommodation
> behind a flag: a Linux agent enrols as itself.

## What is here

| Path | What it is |
|---|---|
| [`manifest.mjs`](manifest.mjs) | The single source of truth: the payload, the installed layout per platform, the configuration catalogue (each `SAC_*` variable, the `capture-core` flag it becomes, and its scope: vendor, `tenant` or `lab`), the tenant package vocabulary, and the generic vendor-wide profile. |
| [`render.mjs`](render.mjs) | Generates every platform artefact from the manifest. `--check` fails on drift; `--generic --os <os>` prints the vendor-wide `capture-core.env`; `--args FILE` renders a config file into the exact flag list; `--list` prints the catalogue. |
| [`build.mjs`](build.mjs) | Compiles `capture-core` and `classifier-host` for a target and stages `installer/.stage/<os>-<arch>/` with a SHA-256 manifest. `--src DIR` compiles from an exported tree (a release tag) instead of the working tree. |
| [`release-msi.mjs`](release-msi.mjs) | Builds the **generic Windows MSI** into `installer/dist/release/` with `release.json` read back out of the package. This is what a vendor ships and what `control-api` packages (`SAC_AGENT_RELEASE_DIR`). |
| [`lab-msi.mjs`](lab-msi.mjs) | Builds the same MSI for this host against the auth lab, adds the lab's profile files, and puts a lab `ShadowAICapture.tenant.env` beside it. Double-click it: the service enrols, intercepts, classifies, drains and (at M3) uploads content with nothing typed. |
| [`verify.mjs`](verify.mjs) | The installer's gate: flags exist (parsed from `main.go`, not from this manifest), the three platforms agree, generated output has not drifted, the real binary parses the generated argv, and the generic package carries no tenant data (on Windows, read back out of a built MSI). |
| [`dev.mjs`](dev.mjs) | Runs the agent **inside** the lab network: install into a prefix, enrol against the real `control-api`, drain the spool through the edge, and read the rows back out of `ingest.*`. For daemons whose published ports the calling host cannot reach. |
| [`dev-host.mjs`](dev-host.mjs) | Runs the agent **on the host** as a console process against the host-published auth lab (Docker Desktop on Windows/macOS): seeds a token, enrols, feeds the golden frames, and checks the rows. `--print-only` resolves the config and stops. It has a known defect, described under "Running on the host against the lab": its frames expire before they are sent. |
| [`seed.mjs`](seed.mjs) | Mints and seeds one single-use enrolment token in the auth lab and prints only the token. `--ceiling m0..m3` also sets the tenant's collection ceiling; at `m3` it gives the tenant a KEK id (a schema requirement), a 1 GiB/day content budget (without one every content grant is denied `over_budget`) and the `full_text` search tier. |
| [`generated/`](generated) | The committed, `--check`-able render: the catalogue template, the tenant package example, `capture-core-run` (sh and `.cmd`), systemd unit, LaunchDaemon plist, WiX `.wxs`. |
| [`linux/`](linux) | `install.sh` / `uninstall.sh` — the two-mode installer (system or rootless `--prefix`). |
| [`macos/`](macos) | `build-pkg.sh` and the `preinstall` / `postinstall` scripts. |
| [`windows/`](windows) | `Build-Msi.ps1` (runs WiX v5+ over the stage; signing, off by default), `Read-MsiInfo.ps1` (an MSI's codes and tables as JSON, through Windows Installer's own COM object), `msi.mjs` (the build both entry points share). |
| [`profiles/`](profiles) | Example lab profiles. `lab-host.env` holds a token and is never committed. |

## Use it

```sh
# 1. Render (only after editing manifest.mjs) and check it
node installer/render.mjs
node installer/render.mjs --check

# 2. The Windows release: installer/dist/release/{ShadowAICapture.msi, release.json}
node installer/release-msi.mjs

# 3. The installer's own gate (part of `node tools/accept.mjs`, gate `installer`)
node installer/verify.mjs

# 4. Stage a payload for another platform, install into a prefix, drive the lab
node installer/build.mjs --os linux
node installer/dev.mjs --no-lab --prefix /tmp/sac
node installer/dev.mjs            # agent inside the lab network (needs Docker)
```

Step 3 does not pass in full on a Windows host: its print-config check runs the staged **Linux**
binary (`.stage/linux-amd64`), which Windows cannot execute. `node installer/verify.mjs --no-exec`
skips that one check. Off Windows, the checks against a built MSI are skipped.

## How a device is configured

`capture-core` reads `KEY=VALUE` files in the `SAC_*` vocabulary with `--config-file`, which may be
repeated; **a later file wins**, and a flag on the command line wins over both. An installed device
has two:

| File (Windows path) | Who writes it | What it holds |
|---|---|---|
| `C:\ProgramData\ShadowAICapture\profile\capture-core.env` | The package (vendor-wide, replaced on upgrade) | Layout, product defaults, and the trust anchors: the policy public key and key id every bundle must verify under, the classifier release's public key. `render.mjs --generic`. No `tenant` or `lab` key. |
| `C:\ProgramData\ShadowAICapture\profile\tenant.env` | Copied at install from `ShadowAICapture.tenant.env` beside the package | `SAC_TENANT_ID`, `SAC_DEVICE_ENDPOINT`, `SAC_DEPLOYMENT_KEY`, `SAC_AUTH_MODE` — nothing else ([example](generated/ShadowAICapture.tenant.env.example)). `control-api` writes it, minting a deployment key per download. |

The service runs `capture-core --service --service-name ShadowAICapture --config-file <profile>\capture-core.env --config-file <profile>\tenant.env`.
The device enrols with the deployment key (and, for an Intune tenant, its Intune device id, which
`control-api` checks with Graph); the **server** resolves the tenant from the key, never from the
request body, and returns `device_id`, `tenant_id`, `region` and the tenant's user-reference key.
With no `SAC_BUNDLE` the agent fetches its policy from `GET /v1/policy` and verifies it under the
pinned key; with no CA pair it mints a per-device interception CA and installs it. Linux and macOS
use the same two files under `/etc/shadow-ai-capture` and `/usr/local/etc/shadow-ai-capture`.

Catalogue scopes (`manifest.mjs`): an entry with no scope is vendor-wide and may be in the generic
file; `tenant` entries come only from the tenant file; `lab` entries (`SAC_USER_REF`,
`SAC_DEVICE_ID`, `SAC_ENROLMENT_TOKEN`, `SAC_BUNDLE`, `SAC_CA_KEY`, `SAC_CA_CERT`, `SAC_CA_FILE`,
`SAC_MDM_ID`) are optional overrides a lab profile or a local run may set, which neither a generic
file nor a tenant package carries; the agent resolves each itself when it is empty.

## The Windows MSI

**Generic and code-only.** `release-msi.mjs` stages the binaries, signs a classifier release into
`C:\Program Files\ShadowAICapture\classifier` (payload, as built: docs/05 §6.1), writes the generic
`capture-core.env`, runs WiX over `generated/windows/ShadowAICapture.wxs`, and reads the package back
into `release.json`: `version`, `product_code`, `upgrade_code`, `package_code`, `sha256`, `size`,
`publisher`, `signed`, plus the trust anchors it pinned and the source tree it compiled. WiX gives
every build a new ProductCode; the UpgradeCode never changes, so a newer MSI replaces the old one
(major upgrade). Raise `--version` for every release an MDM should upgrade to.

**Trust anchors are build inputs.** `--policy-key-file` (or `--policy-key`) names the public half of
the key `control-api` signs bundles with (`SAC_POLICY_SIGNING_KEY_FILE`): hex, or a PEM key, private
or public. With none, it is the lab's vendor key (`localdev/.authlab-identity/policy-signing.pub.hex`,
whose private half the lab `control-api` signs with) when this checkout has a lab, else a development
pair minted into `installer/.release/keys/` (PKCS#8 PEM and the 64-byte hex `sac-bundle` takes) and
reused by later builds. The classifier key is `installer/.release/keys/classifier.key.hex` unless `--classifier-key` names
the vendor's; the rules are the development rules unless `--classifier-rules` names others.

**The tenant file.** At install the MSI copies `ShadowAICapture.tenant.env` from the folder it runs
from (`[SourceDir]`: the Intune content folder, a ConfigMgr cache, an extracted ZIP, a share) to
`profile\tenant.env`. If there is none there **and** none is installed, the install fails before
anything changes (exit 1603). An upgrade with a new file replaces the installed one; an upgrade
without one keeps it; a full uninstall removes it with the rest of `ProgramData\ShadowAICapture`.
How, and why each piece is the way it is:

- `ResolveSource` runs for a first install or a major upgrade only (`NOT Installed`): it is what sets
  `SourceDir`, and run at a removal it would prompt for media that is gone.
- The check is an immediate custom action after `CostFinalize`, not a launch condition. A launch
  condition can only test what `AppSearch` found, `AppSearch` must run before costing (ICE27), and
  the source folder is known only after `ResolveSource`. Both alternatives were tried in a probe
  package: an `AppSearch` over `[SourceDir]` finds nothing, and one over `[OriginalDatabase]\..` is
  refused (MSI note 1324). The action runs `cmd.exe` with `if exist`, and on failure Windows Installer
  logs error 1722 with the command line, which carries the reason in plain words.
- The copy is a `MoveFile` row (`<CopyFile>`), source folder `TENANTENV_DIR`, which a type-51 action
  sets from `[SourceDir]`: `MoveFiles` rejects `SourceDir` itself as a source folder (error 2706).
  `MoveFiles` overwrites an existing destination, and files it copied are not removed by an
  uninstall, which is what keeps the file across an upgrade; `CaRemoveData` removes it on a full one.

Those behaviours were each observed with a per-user probe package built from the same elements
(fresh install without the file: 1603; with it, from a folder with spaces: copied; upgrade without:
kept; upgrade with a new one, launched by relative path from its folder as Intune does: replaced).

**The service.** `capture-core` is registered as a `LocalSystem` service with `Start="auto"`.
`--service` makes the agent implement the SCM contract itself (it is otherwise a console
application), so no external wrapper (NSSM/WinSW) is needed. It is started by a custom action the
installer does not wait for and whose outcome it ignores, so a start failure cannot abort and roll
back the install. The action is `cmd /c sc.exe start … >nul 2>&1`, and both halves of that are
deliberate: run bare from a custom action, `sc.exe` was seen to start the service and then block
for good on its own console output, which held the installer open until the process was killed.
A same-version rebuild upgrades in place (`AllowSameVersionUpgrades`). A full uninstall removes
`C:\ProgramData\ShadowAICapture` (spool, sealed credential, held content, logs, tenant file); an
upgrade keeps it. The dialog is Windows Installer's basic progress box: the source declares no UI.

**Signing is off by default.** `release-msi.mjs --sign` (or `SAC_SIGN=1`) makes `Build-Msi.ps1` sign
`capture-core.exe` and `classifier-host.exe` before they are packaged and the MSI after, with
`signtool` and exactly one of: Trusted Signing (`SAC_SIGN_DLIB` = `Azure.CodeSigning.Dlib.dll`,
`SAC_SIGN_METADATA` = the account/profile `metadata.json`), a certificate in the store
(`SAC_SIGN_THUMBPRINT`), or a PFX (`SAC_SIGN_PFX`, password in `SAC_SIGN_PFX_PASSWORD`).
`SAC_SIGNTOOL` and `SAC_SIGN_TIMESTAMP_URL` override the tool and the timestamp server.
`release.json.signed` is read from the built file's Authenticode signature, not from the switch.
Nothing has been signed yet; the argument assembly was checked with a stand-in `signtool`. The
production certificate and its 460-day clock are [docs/05 §7](../docs/05-platform-delivery.md).
`ShadowAICapture.tenant.env` is outside the signature by design: it is tenant data, not code.

It needs WiX v5 or later on the build host (built with WiX 7), one of:

- the standalone CLI, no .NET SDK: `wix-cli-x64.msi` from https://github.com/wixtoolset/wix/releases
- the .NET tool, needs the .NET SDK 6+: `dotnet tool install --global wix`

With no `wix` on PATH, `Build-Msi.ps1` prints the build it would run and exits non-zero.

## Deploying it

Customer-facing. The admin downloads a package from **Settings → Deployment**: an `.intunewin`
(Intune) or a `.zip` (ConfigMgr, Group Policy, other MDM). Both hold the same signed
`ShadowAICapture.msi` and this tenant's `ShadowAICapture.tenant.env`, which carries a deployment key
minted for that download: treat the package as a secret and revoke the key from the same page if it
leaks. Keep the two files together; the MSI reads the tenant file from its own folder.

**Requirements.** Windows 10 21H2 or later, or Windows 11, x64. Installed as SYSTEM. No reboot.

### Microsoft Intune (Win32 app)

1. Intune admin center → **Apps → Windows → Add → Windows app (Win32)**; upload the `.intunewin`.
   (From the ZIP instead: wrap its folder with Microsoft's Win32 Content Prep Tool,
   `IntuneWinAppUtil.exe -c <folder> -s ShadowAICapture.msi -o <out>`.)
2. **App information**: name *Shadow AI Capture*, publisher as shown in Settings → Deployment.
3. **Program**:
   - Install command: `msiexec /i ShadowAICapture.msi /qn`
   - Uninstall command: `msiexec /x {ProductCode} /qn` — the product code is shown in Settings →
     Deployment (Intune fills both in from the package).
   - Install behaviour: **System**. Device restart behaviour: **No specific action**.
   - Return codes: keep the defaults — `0` success, `1707` success, `3010` soft reboot, `1641` hard
     reboot, `1618` retry (another installation was in progress). Any other code is a failure;
     `1603` is the one to expect, for example when the tenant file was not beside the MSI.
4. **Requirements**: operating system architecture **x64**; minimum operating system **Windows 10
   21H2**.
5. **Detection rules**: *Manually configure* → rule type **MSI** → the MSI product code; **MSI
   product version check: Yes**, operator **Greater than or equal to**, value the version shown in
   Settings → Deployment.
6. **Assignments**: *Required* for a device group. For a later version, add it as a new app with
   **supersedence** on this one and *Uninstall previous version* **off**: the new MSI upgrades in
   place and keeps the device's enrolment.

For an Entra tenant with Intune device verification on, a device enrols only if Intune manages it,
so deploy to Intune-managed devices.

### Configuration Manager

Create an **Application** with a *Windows Installer (\*.msi file)* deployment type pointing at
`ShadowAICapture.msi` in the extracted ZIP folder; the content location is that folder, so the
tenant file travels with it into the client cache. Installation program
`msiexec /i "ShadowAICapture.msi" /qn`, uninstall `msiexec /x {ProductCode} /qn`, detection the MSI
product code (filled in from the file), *Install for system*, *Whether or not a user is logged on*.

### Group Policy

Put both files in a share that **Domain Computers** can read. Computer Configuration → Policies →
Software Settings → **Software installation** → New → Package → the MSI by its UNC path →
**Assigned**. It installs at the next start-up, as SYSTEM, reading the tenant file from the share.

### Any other tool

Copy both files into one folder on the device and run `msiexec /i ShadowAICapture.msi /qn` as
SYSTEM (or from an elevated prompt) in that folder.

### When it fails

Add `/l*v C:\Windows\Temp\ShadowAICapture-install.log` to the command for a full log. A missing
tenant file shows there, and in the Application event log (source *MsiInstaller*, event 11722), as
error 1722 for action `CheckTenantConfig`, whose command line reads *"Shadow AI Capture needs
ShadowAICapture.tenant.env beside ShadowAICapture.msi …"*. Intune's own record is under
`C:\ProgramData\Microsoft\IntuneManagementExtension\Logs`. Once installed, the agent writes
`C:\ProgramData\ShadowAICapture\state\service.log` and `health.jsonl`.

### What it changes on a device, and uninstalling

A LocalSystem service that starts at boot; the binaries under `C:\Program Files\ShadowAICapture`;
`C:\ProgramData\ShadowAICapture`; the device root CA in the machine `Root` store; and the proxy and CA
variables in the machine environment, which the agent sets so CLI runtimes send their HTTPS through
it. Stopping the service removes the root and the variables. Uninstalling (Apps & features, or the
uninstall command) stops and removes the service and deletes both folders.

## The lab MSI: the product's install, on this host

```
node localdev/build.mjs --auth                        # the lab images, once and after a change to a service
docker compose -f localdev/authlab.compose.yaml up -d # the lab the device enrols against
node installer/lab-msi.mjs                            # installer/dist/ShadowAICapture.msi + ShadowAICapture.tenant.env
```

Double-click `installer\dist\ShadowAICapture.msi` and accept the elevation prompt. It is the generic
MSI (same stage, vendor file and classifier release as `release-msi.mjs`) with two lab additions:
the files the lab profile points at are installed under `profile\` (the signed policy bundle, the
device CA pair kept in `installer/.lab/msi/keys/` so a rebuild does not mint a new trusted root, the
lab edge's CA), and the tenant file beside it is a full lab profile rather than a package's four
keys: the lab edge, `x509`, a fresh single-use enrolment token in place of a deployment key, a
local bundle in place of `GET /v1/policy` (signed with the lab's vendor key, so the vendor file's
pin verifies it), the device CA, `SAC_USER_REF=lab-user`, and the interception settings. It is copied in exactly as a tenant package's is, so every lab install
exercises the product's copy path. The MSI clears the agent's state on upgrade (`FreshEnrolment`),
because each build's token is new to a server that does not know the old credential.

The service enrols, trusts its interception CA, points new processes at `proxy.tls`, runs the
classifier host as a child, and drains to the lab. A prompt sent from a terminal opened afterwards
lands in `ingest.observation`. Uninstall from Apps & features. Only terminal and CLI traffic is
captured: a browser ignores the environment variables, and the system proxy is not set.

`--mode` (default `m3`) and `--hosts` (default `api.anthropic.com`) set the bundle's collection mode
and interception scope, and the lab tenant's ceiling is set to the same mode. Only `m3` and `m2` have
been installed and run. At `m3` the device holds each prompt locally, asks `control-api` for a
per-event grant once the event is delivered, and uploads the sealed content; at `m2` nothing is
uploaded and the envelope carries a minimised excerpt. The lab dashboard reads them at
**http://127.0.0.1:8787**.

`node localdev/run.mjs --auth` regenerates the lab's development CA every time it runs, which
orphans an installed device (it pins the old CA and holds a leaf the new one did not sign). Rebuild
and reinstall the MSI after running it. Each build seeds a new token and one install consumes it:
rebuild before installing on a second host or after the lab database is recreated. The classifier
release is built from the development rules, so its labels demonstrate the mechanism; they are not
a judgement worth acting on.

`profiles/lab-host.env` remains a minimal hand-filled profile (drain only, no interception):
`Build-Msi.ps1 -TenantEnv installer/profiles/lab-host.env` places it beside a build as its tenant
file; `node installer/seed.mjs` prints a token to paste in.

## The other platforms

**macOS.** `installer/macos/build-pkg.sh` stages `/usr/local/opt`, the LaunchDaemon plist and the
scripts. `preinstall` refuses an install with no `ShadowAICapture.tenant.env` beside the `.pkg` and
none installed (a pkg cannot roll its payload back, so the check comes first); `postinstall` copies
it to `tenant.env`, seeds `capture-core.env` once from the template and loads the daemon, whose
wrapper reads both files. **NOT VERIFIED**: there is no macOS and no `pkgbuild` here. With no
`pkgbuild` the script names the commands it would run and exits non-zero. The PKG does not yet
install the generic vendor file; it installs the template.

**Linux.** `installer/linux/install.sh` is the one that runs here. It has two modes: a system install
(`/opt`, `/etc`, `/var/lib` + the systemd unit) and a rootless `--prefix` install for development. It
installs `ShadowAICapture.tenant.env` from beside the script (or `--tenant-env FILE`) as `tenant.env`;
a system install with neither that, an installed one, nor a complete `--config` profile stops before
installing anything. It never overwrites an existing `capture-core.env`, because an installer that
resets a device's identity is worse than one that stops.

## Running on the host against the lab

On a machine where Docker Desktop runs the lab (a Windows host, typically), the published edge is
reachable at `127.0.0.1`, so the agent can run as an ordinary host process rather than inside the lab
network: `node installer/dev-host.mjs` (x509; add `--auth-mode dpop` for DPoP) builds the host
payload, seeds a token, runs the enrol pass, confirms the device row in `ops.device`, feeds the
golden frames, and checks `ingest.observation`. It expects the **auth** lab: the endpoint enrols, so
it needs `control-api` and the edge.

**Known defect, not fixed: `dev-host.mjs` does not deliver its frames, and can report success
anyway.** It sets `SAC_RETENTION=1h`, and the golden frames carry an `occurred_at` of 2026-10-02, so
the drain's retention sweep drops all six before sending (`retention dropped 6 expired record(s)`,
`delivered=0`). Two of its checks then mislead. "The endpoint spooled and drained observations"
counts acks, which only mean the frames were spooled. "ingest.observation holds rows for the
tenant" and "nothing was rejected" count the whole tenant, not this device, so they pass or fail on
rows left by earlier runs. On a fresh database the run ends with one honest failure, `0
observation(s)`. Passing `--retention 720h` to the staged binary by hand delivers the frames.
`dev.mjs` sets the same retention and has not been re-run. To see a host device working, use the
lab MSI above.

`dev.mjs` runs the device inside the lab network because some Docker daemons publish ports the host
shell cannot reach, and present a host bind mount as an empty directory; it streams the payload to
the daemon as a build context and shares state through a named volume — the same technique
`localdev/run.mjs` uses for the PKI.

## What is verified, and what is not

**Verified here:** the manifest/flag agreement (against `main.go` and the agent's config-file
catalogue), the three platform renderings, the generated-output drift check, the shell wrapper's
syntax, `capture-core --print-config` over the generated argv, the Linux system/prefix install, and —
with Docker — Linux enrolment through the real `control-api`, a spool drain through the Application
Gateway stand-in, and rows in the real `ingest.observation` / `ingest.submission` schema.

**Verified on the Windows build host, without installing it:** the generic MSI builds with WiX 7;
its Property table and summary stream give the ProductCode, UpgradeCode, version and package code
`release.json` records; its File table holds the binaries, the wrapper, the vendor file and the
classifier release and no tenant or lab file; its `MoveFile` row, sequence and `ServiceInstall`
arguments are as above; an administrative extraction (`msiexec /a`, which installs nothing) shows a
vendor file with no tenant, lab or secret key and a pinned policy key. `verify.mjs` repeats these
whenever `installer/dist/release` holds a build. The tenant-file mechanism was exercised end to end
with a per-user probe package (above). The lab variant compiles with its profile files and state
reset; the signing step's argument assembly was checked with a stand-in `signtool`.

**Verified on a Windows 11 host with WiX 7, by hand, on one machine, with the earlier single-file
build:** the lab MSI installs and upgrades over itself; the installed service enrols; a Claude Code
prompt from a new process reaches `ingest.observation` with labels from the classifier child, at M2
and at M3; at M3 its content is granted, uploaded, found by the dashboard's prompt-text search and
read back through `content-vault`; with the service installed, Node, `curl.exe`, `git` and Python
`urllib` and `requests` still reach hosts outside the interception scope; a full uninstall left
nothing behind. None of that has been repeated with the two-file build.

**Not verified:** the generic MSI installed on any machine, through Intune, ConfigMgr or Group
Policy; a signed build; the macOS PKG; anything under a real MDM. The Linux service unit is
installed by `install.sh` but `dev.mjs` runs the binary directly rather than under systemd.
