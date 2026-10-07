# 55. Linux: attribution, helper and discovery

Needs:
- a Linux desktop (systemd and logind, for example Ubuntu 24.04 or Fedora 41 with GNOME) usable
  as a test device;
- the test tenant's file (`TESTBED.md`, "Tenant file") copied to the desktop by the owner, outside
  the repository;
- the owner's `gh` CLI signed in on the desktop with read access to the repository's Actions;
- Cursor, VS Code with the Copilot, Claude Code and Continue extensions, the `claude`, `codex` and
  `gemini` CLIs, and Ollama with two models, installed.

## Problem

Tasks 09, 10 and 15–21 were built and verified on Windows only. On Linux their providers compile
and report `absent` (`DESIGN.md` §12).

## Goal

On Linux, each of the following works and passes its Windows task's "Done when", adapted to
Linux:
- process attribution;
- the user-session helper;
- the inventory scanner;
- the process monitor;
- the flow monitor.

All of it builds with `CGO_ENABLED=0`.

## Scope

- **A pre-prod release for Linux.** The desktop enrols into pre-prod's test tenant, never the local
  lab. Its package must carry pre-prod's policy public key and a classifier release signed with
  the environment's classifier key, which only CI holds (`SAC_CLASSIFIER_SIGNING_KEY`). So no
  local build can produce it.
  - Until `.github/workflows/deploy.yml` builds a Linux package, add it, scoped to this:
    - one step in the `agent-release` job, after the MSI. It stages with
      `node device/installer/build.mjs --os linux` for amd64 and arm64, using the same key inputs
      `release-msi.mjs` receives. `build.mjs` cross-compiles and packs the tar.gz on Windows.
    - It uploads the artifact `agent-release-linux`.
    - No other step or job changes.
  - This lands in the first merge of this task. After the deploy run, download the artifact on
    the desktop with `gh run download`.
  - **Install.** The owner installs it with the tenant file:
    `sudo ./install.sh --tenant-env <file>`. There is no Linux MDM in scope, so this is the
    install path. Record it in `DECISIONS.md`.
- **Attribution** (`hostinfo`, `_linux.go`):
  - `OwnerOfLocalTCP`: find the socket inode in `/proc/net/tcp` and `/proc/net/tcp6` by local
    and remote address, then scan `/proc/<pid>/fd` for `socket:[inode]`. Cache the inode-to-PID
    map for 1 s.
  - `ProcessInfo`: image from `/proc/<pid>/exe`, uid from `/proc/<pid>/status`, start time from
    `/proc/<pid>/stat`.
  - Publisher: the owning package name (`dpkg -S` or `rpm -qf`, cached per image), recorded as
    the publisher. Linux has no code-signing subject.
  - Also check the noted os/user limitation: pure-Go `os/user` reads only `/etc/passwd`, so
    directory users (SSSD or LDAP) may not resolve. Test with the desktop's account, and record
    the outcome.
- **Helper** (`userhelper`, `_linux.go`):
  - A systemd user unit `/usr/lib/systemd/user/shadow-ai-capture-helper.service`
    (`WantedBy=default.target`, enabled globally with `systemctl --global enable`), running
    `capture-core --user-helper`, added to `device/installer/manifest.mjs` and `install.sh`.
  - Notifications use the `org.freedesktop.Notifications` D-Bus call through the existing
    `godbus/dbus` dependency.
  - The service unit's `ProtectHome=true` doesn't apply to the helper.
- **Inventory** (`inventory`, `_linux.go`):
  - apps: `dpkg-query -W` or `rpm -qa` output, matched on `linux_package`, plus `.desktop` files in
    `/usr/share/applications` and `~/.local/share/applications` for AppImage installs (Cursor);
  - CLIs: npm global, pipx, `~/.local/bin`, `/usr/local/bin`;
  - IDE extensions: the same folders under `~`;
  - local models: `~/.ollama/models/manifests` and `/usr/share/ollama/.ollama/models` for the
    service install.
  - The service unit's `ProtectHome=true` blocks reading home directories. Change it to
    `ProtectHome=read-only` in `generated/linux/shadow-ai-capture.service` (through
    `manifest.mjs`), and record why in `DECISIONS.md`.
- **Process monitor** (`procmon`, `_linux.go`): the netlink process connector
  (`NETLINK_CONNECTOR`, `PROC_EVENT_EXEC` and `PROC_EVENT_EXIT`) through `x/sys/unix`, which needs
  `CAP_NET_ADMIN`, so add it to the unit's capabilities. If the unit can't hold it, poll `/proc`
  every 2 s and record which was used.
- **Flow monitor** (`flowmon`, `_linux.go`): poll `/proc/net/tcp*` every 2 s, with the same domain
  resolution as macOS (task 53), and record the same limitation.

## Done when

- `cd device/capture-core && go test ./hostinfo/ ./userhelper/ ./inventory/ ./procmon/ ./flowmon/`
  passes on the Linux desktop and in CI's Linux job.
- Ready to merge. After merge and deploy (`AGENTS.md`), with `main`'s `agent-release-linux`
  installed on the desktop and enrolled in the test tenant, each Windows task's finish line holds
  for the tools installed:
  - Cursor found;
  - CLIs and extensions with versions;
  - Cursor start and stop;
  - Ollama with two models;
  - a `curl` attributed to `curl` and the user;
  - the helper connected.

  Show each on the desktop from `/var/lib/shadow-ai-capture/health.json` (the collector's
  `emitted` counter) and the agent's `envelope spooled` log lines (task 05, `journalctl -u
  shadow-ai-capture`). Then ask the owner to confirm each on the test tenant's dashboard; wait.
- `node tools/accept.mjs` passes.
