# 53. macOS: attribution, helper and discovery

Needs:
- a Mac (Apple silicon) usable as a test device, with Go 1.27, Node 22 and the Xcode command-line
  tools;
- backlog task 27 done (the macOS package builds and installs on a Mac);
- the test tenant's file (`TESTBED.md`, "Tenant file") copied to the Mac by the owner, outside the
  repository;
- the owner's `gh` CLI signed in on the Mac with read access to the repository's Actions;
- Claude Desktop, ChatGPT Desktop, Cursor, VS Code with the Copilot, Claude Code and Continue
  extensions, the `claude` and `codex` CLIs, and Ollama with two models, installed.

## Problem

Tasks 09, 10 and 15–21 were built and verified on Windows only. On macOS their providers compile
and report `absent` (`DESIGN.md` §12). The plan's discovery finish lines ("on each OS") are not
met for macOS.

## Goal

On macOS, each of the following works and passes its Windows task's "Done when", adapted to
macOS:
- process attribution;
- the user-session helper;
- the inventory scanner (apps, CLIs, IDE extensions, local models);
- the process monitor;
- the flow monitor.

All of it builds with `CGO_ENABLED=0`.

## Scope

- **A pre-prod release for macOS.** The Mac enrols into pre-prod's test tenant, never the local
  lab. Its package must carry pre-prod's policy public key and a classifier release signed with
  the environment's classifier key, which only CI holds (`SAC_CLASSIFIER_SIGNING_KEY`). So no
  local build can produce it.
  - Until `.github/workflows/deploy.yml` builds a macOS package, add one, scoped to this:
    - a job `agent-release-macos` on `macos-latest`, beside `agent-release`, with the same
      environment, variables and secrets;
    - it stages with `node device/installer/build.mjs --os darwin`, using
      `SAC_POLICY_PUBLIC_KEY`, `SAC_CLASSIFIER_SIGNING_KEY` and `SAC_EXTENSION_SIGNING_KEY`
      exactly as `agent-release` passes them to `release-msi.mjs`;
    - it runs `device/installer/macos/build-pkg.sh`, unsigned (signing and notarisation are
      E38);
    - it uploads the artifact `agent-release-macos`;
    - no other job changes, and the images and deploy jobs don't wait on it.
  - This lands in the first merge of this task. After the deploy run, download the artifact on
    the Mac with `gh run download`.
  - **Install.** The owner installs it with the tenant file beside the `.pkg`
    (`sudo installer -pkg ShadowAICapture.pkg -target /`). The package needs no MDM profile on
    macOS until E38's network extension and Full Disk Access work, so this hand install is the
    interim install path. Record it in `DECISIONS.md` as a deviation from "Intune installs".
- **Attribution** (`hostinfo`, `_darwin.go`):
  - `ProcessInfo`:
    - image path via `sysctl kern.procargs2`, or `proc_pidpath` through `golang.org/x/sys/unix`
      syscalls if reachable without cgo;
    - user from the process's uid (`kern.proc.pid`);
    - publisher: the code-signing team id, via `codesign -dv --verbose=2 <path>` output parsed
      once per image and cached. A child process is acceptable here; record it.
  - `OwnerOfLocalTCP`: choose a cgo-free method and record the choice and why in `DECISIONS.md`.
    In order of preference:
    1. the `net.inet.tcp.pcblist_n` sysctl (undocumented; verify on the installed macOS version);
    2. parsing `/usr/sbin/lsof -nP -iTCP -sTCP:ESTABLISHED -F pn` for the one port, with a 200 ms
       timeout.
- **Helper** (`userhelper`, `_darwin.go`):
  - A LaunchAgent `/Library/LaunchAgents/com.shadowaicapture.user-helper.plist` running
    `capture-core --user-helper` in each GUI session, added to `device/installer/manifest.mjs`
    and the macOS package scripts. It is installed with the package, not per feature.
  - The service doesn't spawn it; it checks each console user has a connected helper.
  - Notifications use `osascript -e 'display notification ...'`. Record whether that suffices
    without an app bundle; if not, record it as an E38 dependency.
- **Inventory** (`inventory`, `_darwin.go`):
  - apps: `/Applications` and `~/Applications` bundles' `Info.plist` (`CFBundleIdentifier`,
    `CFBundleShortVersionString`), matched against `macos_bundle_id`;
  - CLIs: Homebrew (`/opt/homebrew/bin`, `/usr/local/bin`, the `Cellar` versions), npm global,
    pipx, `~/.local/bin`;
  - IDE extensions: the same folders as Windows under `~`;
  - local models: `~/.ollama/models/manifests`.
  - Users' home folders are read as root. Respect TCC: record which locations need Full Disk
    Access and degrade (`enumeration_partial`) rather than fail.
- **Process monitor** (`procmon`, `_darwin.go`): Endpoint Security needs an Apple entitlement, so
  poll `sysctl kern.proc.all` every 2 s and diff by `(pid, start time)` for start and stop.
  Record the polling interval's effect on short-lived processes.
- **Flow monitor** (`flowmon`, `_darwin.go`): poll the TCP table by the method chosen for
  `OwnerOfLocalTCP` every 2 s. Resolve catalog inference domains to addresses, refreshing every 5
  minutes and honouring the DNS TTL. Match on remote address. Record the CDN shared-address
  limitation in `DECISIONS.md`.
- Each provider's macOS health replaces `absent` with real states.

## Done when

- `cd device/capture-core && go test ./hostinfo/ ./userhelper/ ./inventory/ ./procmon/ ./flowmon/`
  passes on the Mac.
- Ready to merge. After merge and deploy (`AGENTS.md`), and with `main`'s `agent-release-macos`
  installed on the Mac and enrolled in the test tenant, each Windows task's finish line holds:
  - Claude Desktop, ChatGPT Desktop and Cursor found (16);
  - `claude`, `codex`, `gemini` and `copilot` with versions, for those installed (17);
  - the three extensions with versions (18);
  - Cursor start and stop (19);
  - Ollama with two models (20);
  - a `curl` to `api.openai.com` attributed to `curl` and the user (21);
  - the helper connected (10).
  Show each on the Mac from `/var/db/shadow-ai-capture/state/health.json` (the collector's
  `emitted` counter) and the agent log's `envelope spooled` lines (task 05) in
  `/var/log/shadow-ai-capture/`. Then ask the owner to confirm each on the test tenant's
  dashboard (the Mac's device view and the event list); wait.
- `node tools/accept.mjs` passes on Windows. The Go suites also pass on the Mac.
