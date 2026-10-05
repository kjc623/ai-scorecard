# installer — the development installer for the endpoint

This packages the device tier the way [docs/05-platform-delivery.md](../docs/05-platform-delivery.md)
§6 describes it: **one signed artefact per platform carrying code only**, with the per-tenant
association delivered as configuration by the customer's MDM, never baked into the installer. It
exists so that progress on the endpoint can be *checked* on one machine, not inferred from green
component suites.

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
| [`manifest.mjs`](manifest.mjs) | The single source of truth: the payload, the installed layout per platform, and the configuration catalogue (each `SAC_*` variable and the `capture-core` flag it becomes). |
| [`render.mjs`](render.mjs) | Generates every platform artefact from the manifest. `--check` fails on drift; `--args FILE` renders a config file into the exact flag list; `--list` prints the catalogue. |
| [`build.mjs`](build.mjs) | Compiles `capture-core` and `classifier-host` for a target and stages `installer/.stage/<os>-<arch>/` with a SHA-256 manifest. |
| [`verify.mjs`](verify.mjs) | The installer's gate: flags exist (parsed from `main.go`, not from this manifest), the three platforms agree, generated output has not drifted, and the real binary parses the generated argv. |
| [`dev.mjs`](dev.mjs) | Runs the agent **inside** the lab network: install into a prefix, enrol against the real `control-api`, drain the spool through the edge, and read the rows back out of `ingest.*`. For daemons whose published ports the calling host cannot reach. |
| [`dev-host.mjs`](dev-host.mjs) | Runs the agent **on the host** as a console process against the host-published auth lab (Docker Desktop on Windows/macOS): seeds a token, enrols, feeds the golden frames, and checks the rows. `--print-only` resolves the config and stops. It has a known defect, described under "Running on the host against the lab": its frames expire before they are sent. |
| [`lab-msi.mjs`](lab-msi.mjs) | Builds the Windows MSI for this host against the auth lab: mints the policy bundle, the classifier release and an enrolment token, sets the lab tenant's ceiling to the bundle's mode, and packages them with the payload. The installed service enrols, intercepts, classifies, drains and (at M3) uploads content with nothing typed. |
| [`seed.mjs`](seed.mjs) | Mints and seeds one single-use enrolment token in the auth lab and prints only the token. `localdev/run.mjs --auth` mints its own and discards them, so a host-run device has none. `--ceiling m0..m3` also sets the tenant's collection ceiling; at `m3` it gives the tenant a KEK id (a schema requirement), a 1 GiB/day content budget (without one every content grant is denied `over_budget`) and the `full_text` search tier. |
| [`generated/`](generated) | The committed, `--check`-able render: env template, `capture-core-run` (sh and `.cmd`), systemd unit, LaunchDaemon plist, WiX `.wxs`. |
| [`linux/`](linux) | `install.sh` / `uninstall.sh` — the two-mode installer (system or rootless `--prefix`). |
| [`macos/`](macos) | `build-pkg.sh` and the `preinstall` / `postinstall` scripts. |
| [`windows/`](windows) | `Build-Msi.ps1` — stages the enrolment profile and the files it points at, and runs WiX (v5 or later: the source harvests the profile directory with `<Files>`; it was built with v7). |
| [`profiles/`](profiles) | Example enrolment profiles. Values are per tenant; the lab one is non-secret. |

## Use it

```sh
# 1. Render (only after editing manifest.mjs) and check it
node installer/render.mjs
node installer/render.mjs --check

# 2. Build and stage a payload for one platform
node installer/build.mjs --os linux
node installer/build.mjs --os windows --arch amd64
node installer/build.mjs --os darwin  --arch arm64

# 3. The installer's own gate (part of `node tools/accept.mjs`, gate `installer`)
node installer/verify.mjs

# 4. Install into a prefix and print the resolved config, no Docker
node installer/dev.mjs --no-lab --prefix /tmp/sac

# 5. Install and drive the whole path against the local auth lab (needs Docker)
node installer/dev.mjs            # agent inside the lab network
node installer/dev-host.mjs       # agent on the host, against the host-published edge
```

Step 3 does not pass in full on a Windows host: its last check runs the staged **Linux** binary
(`.stage/linux-amd64`), which Windows cannot execute. `node installer/verify.mjs --no-exec` skips
that one check and the rest pass.

The result of step 5 with `dev.mjs`, on a working tree, is:

```
  ok   auth lab ready at the edge — https://edge:8443
  ok   the drain enrolled and stored a credential — /state/credential.sealed
  ok   ops.device carries the enrolled device — device_id=… region=authlab-region
  ok   the golden-frames run exits zero — exit 0
  ok   ingest.observation holds rows for the tenant — 2 observation(s)
  ok   nothing was rejected on identity — 0 rejected
```

## The artefacts

**Windows.** `installer/windows/Build-Msi.ps1 -ConfigFile installer/profiles/lab.env` stages the
profile and runs WiX over `generated/windows/ShadowAICapture.wxs`. capture-core is registered as a
`LocalSystem` service whose arguments are `--service --service-name ShadowAICapture --config-file
C:\ProgramData\ShadowAICapture\capture-core.env`. `--service` makes the agent implement the SCM
contract itself (it is otherwise a console application), so no external wrapper (NSSM/WinSW) is
needed; the MSI installs the enrolment profile as that file **merged over the platform defaults**
(`render.mjs --env`, so the file is complete and capture-core resolves only what it contains), which
makes reconfiguration a file edit rather than an MSI rebuild and keeps the command line secret-free. The files
the profile points at (the signed policy bundle, the device CA, the classifier release, the pinned
edge CA) are passed as `-ProfileDir` and installed under `C:\ProgramData\ShadowAICapture\profile`.
The service is registered with `Start="auto"` and started by a custom action the installer does not
wait for and whose outcome it ignores, so a start failure cannot abort and roll back the install.
The action is `cmd /c sc.exe start … >nul 2>&1`, and both halves of that are deliberate: run bare
from a custom action, `sc.exe` was seen to start the service and then block for good on its own
console output, which held the installer open until the process was killed. With its output sent
to `nul` it exits, and nothing is left behind. A rebuilt MSI carries the same version, so the
package allows a same-version upgrade; without that a second install registers a second product
beside the first. A full uninstall removes `C:\ProgramData\ShadowAICapture` (spool, sealed
credential, held content, logs); an upgrade keeps it, unless the MSI was built with
`-FreshEnrolment`, which clears the state so the device enrols again. Built and installed with
WiX 7 on Windows 11 through `lab-msi.mjs` (below); with no `wix` on PATH the script prints the
commands it would run and exits non-zero rather than pretending. The dialog is Windows Installer's
basic progress box: the source declares no UI, so there is nothing to choose at install time.

**macOS.** `installer/macos/build-pkg.sh` stages `/usr/local/opt`, the LaunchDaemon plist and a
postinstall that seeds the config once and loads the daemon. **NOT VERIFIED** for the same reason:
there is no macOS and no `pkgbuild` here. With no `pkgbuild` the script names the commands it would
run and exits non-zero.

**Linux.** `installer/linux/install.sh` is the one that runs here. It has two modes: a system install
(`/opt`, `/etc`, `/var/lib` + the systemd unit) and a rootless `--prefix` install for development. It
never overwrites an existing configuration, because an installer that resets a device's identity is
worse than one that stops.

## How the agent knows where to send data

`capture-core` reads the enrolment profile directly with `--config-file` (a `KEY=VALUE` file in the
same `SAC_*` vocabulary the manifest lists), so the flag names live in one place and a secret or a
path with spaces needs no quoting. The generated wrapper on Linux/macOS and the Windows service both
just point the agent at that file. The one flag that decides the destination is `--device-endpoint`,
the regional Application Gateway FQDN. The
per-tenant binding is not in the artefact: the MDM delivers the FQDN, the credential mode
(`x509`/`dpop`), the pinned CA set and a short-lived, single-use enrolment token as the **enrolment
profile** ([docs/05 §6.2](../docs/05-platform-delivery.md)). The device calls `POST /v1/enrol`, and
the **server** resolves the tenant from the token or the credential — never from the request body —
and returns `device_id`, `tenant_id` and `region`. The credential names the FQDN (SAN in `x509`, token
audience in `dpop`), and the region pin fails closed.

The profile decides more than the destination. Two pairs in the catalogue switch on things the agent
otherwise does not do, and both default to empty:

| Variables | What they turn on |
|---|---|
| `SAC_CLASSIFIER_RELEASE`, `SAC_CLASSIFIER_PUBKEY` | The agent runs the installed `classifier-host` as a child on stdio, loading that signed release. With neither these nor `SAC_CLASSIFIER_ADDRESS`, classification is rules-only. A Windows service has no other way to have a classifier: the host has no named-pipe listener and is not a service itself. |
| `SAC_CONTENT_DIR`, `SAC_CONTENT_KEY` | The M3 local content store, and the key file it is sealed under (outside the directory). Without them an M3 observation is refused. |

The collection mode is not in the profile at all. It comes from the signed policy bundle the profile
points at (`SAC_BUNDLE`, verified under `SAC_POLICY_KEY`), and with no valid bundle the agent runs
at M0.

## Running on the host against the lab

On a machine where Docker Desktop runs the lab (a Windows host, typically), the published edge is
reachable at `127.0.0.1`, so the agent can run as an ordinary host process rather than inside the lab
network:

```
node localdev/build.mjs --auth
node localdev/run.mjs --auth          # leaves the auth lab running
node installer/dev-host.mjs           # x509; add --auth-mode dpop for DPoP
```

`dev-host.mjs` builds the host payload, seeds a token, runs the enrol pass, confirms the device row
in `ops.device`, feeds the golden frames, and checks `ingest.observation`. It no longer rewrites
`--device-id`: the agent takes its identity from the issued credential. It expects the **auth** lab,
not the default memory lab: the endpoint enrols, so it needs `control-api` and the edge, and the
default lab's `ingest-api` has no `/v1/enrol` and accepts only header-based dev principals the agent
does not send.

**Known defect, not fixed: `dev-host.mjs` does not deliver its frames, and can report success
anyway.** It sets `SAC_RETENTION=1h`, and the golden frames carry an `occurred_at` of 2026-10-02, so
the drain's retention sweep drops all six before sending (`retention dropped 6 expired record(s)`,
`delivered=0`). Two of its checks then mislead. "The endpoint spooled and drained observations"
counts acks, which only mean the frames were spooled. "ingest.observation holds rows for the
tenant" and "nothing was rejected" count the whole tenant, not this device, so they pass or fail on
rows left by earlier runs. On a fresh database the run ends with one honest failure, `0
observation(s)`. Passing `--retention 720h` to the staged binary by hand delivers the frames.
`dev.mjs` sets the same retention and has not been re-run. To see a host device working, use the
lab MSI below.

### The lab MSI: the product's install, on this host

```
node localdev/build.mjs --auth        # the lab images, once and after a change to a service
node localdev/run.mjs --auth          # the lab the device enrols against
node installer/lab-msi.mjs            # builds installer/dist/ShadowAICapture.msi
```

Double-click the MSI and accept the elevation prompt. It installs the agent as the
`ShadowAICapture` service and starts it; the device enrols, trusts its interception CA, points new
processes at `proxy.tls`, runs the classifier host as a child, and drains to the lab. Nothing is
typed, and a prompt sent from a terminal opened afterwards lands in `ingest.observation`. Uninstall
from Apps & features.

What the install changes on the machine, all of it undone by the uninstall: a LocalSystem service
that starts at boot; the device root CA in the machine `Root` store; and the proxy and CA variables
in the machine environment, so every process started afterwards — not only AI tools — sends its
HTTPS through the agent, which blind-tunnels what is outside the bundle's scope. Stopping the
service removes the root and the variables as well, because the lab profile sets
`SAC_TRUST_REMOVE_ON_STOP`. Only terminal and CLI traffic is captured this way: a browser ignores
the environment variables, and the system proxy is not set.

`lab-msi.mjs` stands in for the MDM: it mints what an enrolment profile carries (a signed policy
bundle, a classifier release, a single-use token, the lab edge and its CA) and hands it to
`Build-Msi.ps1` as `-ConfigFile` / `-ProfileDir` / `-FreshEnrolment`. `--mode` (default `m3`) and
`--hosts` (default `api.anthropic.com`) set the bundle's collection mode and interception scope, and
the lab tenant's ceiling is set to the same mode.

At `m3` the device holds each prompt locally, asks `control-api` for a per-event grant once the event
is delivered, and uploads the sealed content. Nothing in `ingest.observation` carries the text — the
envelope never does. To read a prompt, open the dashboard's Explore page, which the lab serves at
**http://127.0.0.1:8787/explore.html?transport=live**. Open an event whose content is `uploaded` and
request a retrieval in its panel: `content-vault` requires a case reference and a second approver who
is not the requester, issues a single-use grant, and writes the audit rows first. The panel shows what
the user typed apart from what the client added around it (Claude Code's injected context, the resent
conversation, telemetry batches), with the full capture one click away. "Search prompt text" on the
same page is the vault's content search: the vault indexes a prompt's text as it stores the object
(the lab tenant is seeded at the `full_text` tier, scope `lab`), and a search returns bounded snippets
and is audited like a retrieval. Both reach the vault through `query-api`; the analyst is the
development principal the dashboard's server names (`LAB_ANALYST`, default `analyst@lab.test`).

`node localdev/run.mjs --auth` regenerates the lab's development CA every time it runs, which
orphans an installed device (it pins the old CA and holds a leaf the new one did not sign). Rebuild
and reinstall the MSI after running it. To restart lab services without that, use
`docker compose -f localdev/authlab.compose.yaml up -d`.

Each build seeds a new token, and one install consumes it: rebuild before installing on a second
host or after the lab database is recreated. The device CA and the classifier signing key are kept
in `installer/.lab/msi/keys/` so a rebuild does not mint a new trusted root. The classifier release
is built from the development rules (`endpoint/classifier-host/testdata/dev-rules.json`), so its
labels are a demonstration of the mechanism, not a judgement worth acting on.

Other modes build the same way (`--mode m0|m1|m2`); only `m3`, the default, and `m2` have been
installed and run. At `m2` nothing is uploaded: the envelope carries a minimised excerpt, which is
the fragment a classifier rule matched and is empty when none did.

It needs WiX on the build host, one of:

- the standalone CLI, no .NET SDK: `wix-cli-x64.msi` from https://github.com/wixtoolset/wix/releases
- the .NET tool, needs the .NET SDK 6+: `dotnet tool install --global wix`

`profiles/lab-host.env` remains as a minimal hand-filled profile (drain only, no interception) for
`Build-Msi.ps1 -ConfigFile`; `node installer/seed.mjs` prints the token to paste in.

## Two gaps this makes visible

1. **RESOLVED — the agent now adopts the issued credential as its envelope identity.** Enrolment
   mints the device, and `capture-core` takes its `tenant_id`/`device_id` from the sealed credential
   (loaded at startup, or a bounded synchronous enrolment on first start) rather than the flags. The
   write path authenticates the credential, so the envelope identity and the credential can no longer
   disagree. `--device-id`/`--tenant-id` are demoted to a local/offline fallback (required only with
   no `--device-endpoint`); when they are present alongside a credential and disagree, the mismatch is
   logged and the credential wins. `dev.mjs` no longer reads the minted `device_id` back out of
   `ops.device` and rewrites the flag — it relies on the adopted credential. A record minted by an
   older build under the flag identity is quarantined at drain (rejected, reason `stale_identity`),
   never delivered.
2. **The lab runs the device inside the lab network, not on the host.** Some Docker daemons publish
   ports the host shell cannot reach, and present a host bind mount as an empty directory. `dev.mjs`
   therefore streams the payload to the daemon as a build context and shares state through a named
   volume — the same technique `localdev/run.mjs` uses for the PKI.

## What is verified, and what is not

**Verified here:** the manifest/flag agreement (against `main.go`), the three platform renderings, the
generated-output drift check, the shell wrapper's syntax, `capture-core --print-config` over the
generated argv, the Linux system/prefix install, and — with Docker — Linux enrolment through the real
`control-api`, a spool drain through the Application Gateway stand-in, and rows in the real
`ingest.observation` / `ingest.submission` schema.

**Verified on a Windows 11 host with WiX 7,** by hand, on one machine: the lab MSI installs and
upgrades over itself; the installed service enrols; a Claude Code prompt from a new process reaches
`ingest.observation` with labels from the classifier child, at M2 and at M3; at M3 its content is
granted, uploaded, found by the dashboard's prompt-text search and read back through
`content-vault`; and with the service installed, Node, `curl.exe`, `git` and Python `urllib` and
`requests` still reach hosts outside the interception scope. A full uninstall was checked once and
left nothing behind (service, both directories, machine environment and trusted root all removed).
That check was made on an earlier build of the same source, before the content store and the
quiet start step were added; the uninstall has not been repeated since. No install has been done
through the dialog: every one was `msiexec /qn`.

**Not verified here:** the macOS PKG (no `pkgbuild`); the Windows MSI on any other machine or
Windows version; and anything under a real MDM. The
Linux service unit is installed by `install.sh` but `dev.mjs` runs the binary directly rather than
under systemd. Nothing here is signed: the production artefact signs with the 460-day code-signing
certificate ([docs/05 §7](../docs/05-platform-delivery.md)); these are development artefacts and say
so.
