# tools/testbed — the reference VM

The reference VM is a Windows 11 Hyper-V VM on the owner's PC, Entra-joined, Intune-managed and
enrolled into pre-prod's test tenant, set up as a customer's device is. The agent reaches it only as
the deploy workflow's signed release: installed once through Intune, then updating itself from
pre-prod. These tools install it, follow each release onto the VM and run checks inside it. They run on the owner's PC: Node 22, the `gh` CLI signed in, Windows
PowerShell 5.1 with `Microsoft.Graph.Authentication`, and an elevated prompt for Hyper-V.

## Configuration

`refactor-endpoint/TESTBED.md`'s value table, read by `testbed.mjs`; a tool refuses to run while a
value it needs is empty. The table names files that hold secrets (the VM administrator credential,
the tenant file with the deployment key); no tool prints them.

## Commands

```
node tools/testbed/deploy.mjs [--commit <sha>]
node tools/testbed/deploy.mjs --install [--commit <sha>] [--no-wait]
node tools/testbed/deploy.mjs --uninstall [--no-wait]
& .\tools\testbed\invm.ps1 -Command '<script>'
& .\tools\testbed\invm.ps1 -AsUser console|second -Command '<script>'
& .\tools\testbed\invm.ps1 -Screenshot <out.png>
& .\tools\testbed\invm.ps1 -CopyFrom <vm path> <pc path>
& .\tools\testbed\invm.ps1 -CopyTo <pc path> <vm path>
& .\tools\testbed\invm.ps1 -RestoreCheckpoint
& .\tools\testbed\invm.ps1 -AgentState
```

`deploy.mjs` finds the run of `deploy.yml` that ci started on `main` for the commit (default:
`origin`'s `main`), waits for it and downloads its `agent-release` artifact: the release pre-prod
serves after that run, built by it when `device/` changed and otherwise the one pre-prod already
served. The VM's agent updates itself to that release from pre-prod, so by default `deploy.mjs` publishes nothing: it waits (up to 60 minutes)
until the VM runs the release and reports to pre-prod, printing the elapsed time and, while it
waits, the agent's last update check. `--install` is for a VM with no agent, or one older than the
self-updating agent: it wraps the MSI and the tenant file with the pinned `IntuneWinAppUtil.exe`,
publishes it with `Publish-IntuneBuild.ps1` (detected by the registry version at this release or
later, as the dashboard's Intune steps say), syncs the VM, restarts its Intune Management
Extension, and waits the same way. `--uninstall` assigns the app as uninstall and waits until the
agent is gone.

`invm.ps1` runs from an elevated Windows PowerShell prompt at the repository root, called as
`& .\tools\testbed\invm.ps1`; `powershell -File` from a PowerShell prompt would split a quoted
`-Command` argument at its spaces. It works through PowerShell Direct. `-AsUser` runs the script in
that Entra user's own session (HKCU, the user's environment, processes that must be theirs);
`-AgentState` prints the agent's health rows and counters, spool, delivery, policy bundle and last
health acknowledgement.

## What it changes

Intune (`--install` and `--uninstall` only): the Win32 app `Shadow AI Capture (pre-prod test)`, created on the first run (its id is
written to `TESTBED.md`), and only its assignment to the test device group. Any other app id is
refused. The VM: only through these scripts.

The device side is checked here. The dashboard (the Devices page, settings) is the owner's to
check and change.

## Test

```
node --test "tools/testbed/*.test.mjs"
```

The tests cover the `TESTBED.md` parsing, the run selection from `gh` JSON, the log redaction of the
deployment key and the refusal of a foreign app id. Intune, Graph and Hyper-V are exercised only by
running the tools against the VM.
