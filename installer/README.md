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
| [`dev-host.mjs`](dev-host.mjs) | Runs the agent **on the host** against the host-published auth lab (Docker Desktop on Windows/macOS): seeds a token, enrols, aligns `--device-id` with the value the server mints, feeds the golden frames, and verifies the rows. `--print-only` resolves the config and stops. |
| [`seed.mjs`](seed.mjs) | Mints and seeds one single-use enrolment token in the auth lab and prints only the token. `localdev/run.mjs --auth` mints its own and discards them, so a host-run device has none. |
| [`generated/`](generated) | The committed, `--check`-able render: env template, `capture-core-run` (sh and `.cmd`), systemd unit, LaunchDaemon plist, WiX `.wxs`. |
| [`linux/`](linux) | `install.sh` / `uninstall.sh` — the two-mode installer (system or rootless `--prefix`). |
| [`macos/`](macos) | `build-pkg.sh` and the `preinstall` / `postinstall` scripts. |
| [`windows/`](windows) | `Build-Msi.ps1` — composes the service argv and runs WiX v4. |
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

The result of step 5, on a working tree, is:

```
  ok   auth lab ready at the edge — https://edge:8443
  ok   the drain enrolled and stored a credential — /state/credential.sealed
  ok   ops.device carries the enrolled device — device_id=… region=authlab-region
  ok   the golden-frames run exits zero — exit 0
  ok   ingest.observation holds rows for the tenant — 2 observation(s)
  ok   nothing was rejected on identity — 0 rejected
```

## The artefacts

**Windows.** `installer/windows/Build-Msi.ps1 -ConfigFile installer/profiles/lab.env` composes the
service argv from the profile and runs WiX over `generated/windows/ShadowAICapture.wxs`. The service
is installed as `LocalSystem` with the argv in the public property `SAC_ARGS`; the documented MDM
shape is Intune passing command-line properties to `msiexec`, and `SAC_ARGS` is overridable at
install time. The WiX source is authored to the contract the Linux installer already satisfies, but
it is **NOT VERIFIED**: there is no Windows and no WiX on the host this was written on, so the
script prints the exact commands it would run and exits non-zero rather than pretending.

**macOS.** `installer/macos/build-pkg.sh` stages `/usr/local/opt`, the LaunchDaemon plist and a
postinstall that seeds the config once and loads the daemon. **NOT VERIFIED** for the same reason:
there is no macOS and no `pkgbuild` here. With no `pkgbuild` the script names the commands it would
run and exits non-zero.

**Linux.** `installer/linux/install.sh` is the one that runs here. It has two modes: a system install
(`/opt`, `/etc`, `/var/lib` + the systemd unit) and a rootless `--prefix` install for development. It
never overwrites an existing configuration, because an installer that resets a device's identity is
worse than one that stops.

## How the agent knows where to send data

`capture-core` resolves flags and nothing else, so the configuration file is turned into argv by a
generated wrapper (`capture-core-run`) on Linux/macOS, and into `SAC_ARGS` on Windows. The one flag
that decides the destination is `--device-endpoint`, the regional Application Gateway FQDN. The
per-tenant binding is not in the artefact: the MDM delivers the FQDN, the credential mode
(`x509`/`dpop`), the pinned CA set and a short-lived, single-use enrolment token as the **enrolment
profile** ([docs/05 §6.2](../docs/05-platform-delivery.md)). The device calls `POST /v1/enrol`, and
the **server** resolves the tenant from the token or the credential — never from the request body —
and returns `device_id`, `tenant_id` and `region`. The credential names the FQDN (SAN in `x509`, token
audience in `dpop`), and the region pin fails closed.

## Running on the host against the lab

On a machine where Docker Desktop runs the lab (a Windows host, typically), the published edge is
reachable at `127.0.0.1`, so the agent can run as an ordinary host process rather than inside the lab
network:

```
node localdev/build.mjs --auth
node localdev/run.mjs --auth          # leaves the auth lab running
node installer/dev-host.mjs           # x509; add --auth-mode dpop for DPoP
```

`dev-host.mjs` builds the host payload, seeds a token, runs the enrol pass, reads the server-minted
`device_id` out of `ops.device`, rewrites `--device-id`, feeds the golden frames, and checks
`ingest.observation`. It expects the **auth** lab, not the default memory lab: the endpoint enrols, so
it needs `control-api` and the edge, and the default lab's `ingest-api` has no `/v1/enrol` and accepts
only header-based dev principals the agent does not send.

Use the MSI instead when you want the installed-service shape:

```
dotnet tool install --global wix        # once
pwsh installer/windows/Build-Msi.ps1 -ConfigFile installer/profiles/lab-host.env
msiexec /i installer/dist/ShadowAICapture.msi /qn
```

`profiles/lab-host.env` is the host-facing profile (edge at `127.0.0.1`, native paths);
`node installer/seed.mjs` prints the token to paste in.

## Two gaps this makes visible

1. **The agent stamps its envelopes from `--device-id`, but the server mints `device_id`.** Enrolment
   mints the device, and `capture-core` does not adopt the value the server returned, so its envelope
   `device_id` disagrees with the credential the write path authenticates. `dev.mjs` works around
   this the only way configuration can: it enrols first, reads the minted `device_id` from
   `ops.device`, aligns the flag, and *then* spools anything. The real fix is for the agent to take
   its identity from the issued credential. Until then, an operator must pin `--device-id` to what
   enrolment returned, and a batch minted before alignment is correctly rejected by
   `ingest-api` as `schema_violation` on `/device_id` ([docs/02 §5.3](../docs/02-ingest-and-transport.md)).
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

**Not verified here:** the Windows MSI (no WiX, no Windows) and the macOS PKG (no `pkgbuild`). The
Linux service unit is installed by `install.sh` but `dev.mjs` runs the binary directly rather than
under systemd. Nothing here is signed: the production artefact signs with the 460-day code-signing
certificate ([docs/05 §7](../docs/05-platform-delivery.md)); these are development artefacts and say
so.
