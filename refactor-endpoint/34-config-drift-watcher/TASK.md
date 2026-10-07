# 34. Config drift watcher

## Problem

The tool config writers (tasks 27, 29 and 31) apply configuration on start, on policy change and
on each health check. Between those, an administrator, a script or a tool's own updater can
change or delete the managed file. Telemetry then stops silently until the next re-apply, and
nothing records that it happened.

## Goal

When a file or registry value a config writer owns changes, the agent re-applies its keys within
5 seconds (`DESIGN.md` §13). The tool's health row reports `tampered` with detail
`config_tampered`, so the change is visible on the dashboard as a security signal.

## Scope

- **`device/capture-core/toolconfig/watch.go`**, using `github.com/fsnotify/fsnotify`
  (`DESIGN.md` §11):
  - Each enabled writer registers its `Path()`. The watcher watches the parent directory (so a
    delete and re-create is seen) and filters to the file.
  - On a write, create, rename or remove event, after a 250 ms debounce:
    1. Read the file.
    2. If the agent's keys aren't exactly the desired values, call the writer's `Apply`, count
       `errors` on failure, and mark the row tampered.
    3. If they are (including the agent's own write echoing back), do nothing.
  - **Registry-backed writers** (Copilot's policy values and machine environment variables, task
    31): a `RegNotifyChangeKeyValue` watch on the keys involved, through `x/sys/windows`, with the
    same compare-then-apply rule.
  - On start, and every 60 s as a backstop, every writer compares and re-applies, in case an
    event was missed.
- **Health** (in each writer's provider):
  - a re-apply caused by an outside change sets the row to `tampered` with `config_tampered` for
    the rest of the health interval and until the next clean comparison, then returns to
    `healthy`;
  - every re-apply logs one line with the tool and path, never the file contents.
- **Tests**:
  - an external edit to a watched temp file is reverted within 5 s;
  - delete and re-create is reverted;
  - the agent's own write doesn't flag tamper;
  - a registry value change through a fake seam is reverted;
  - the tampered state is reported and clears after a clean comparison;
  - run under `-race`.

## Done when

- `cd device/capture-core && go test -race ./toolconfig/` passes.
- After merge and deploy (`AGENTS.md`), with Claude Code OTel on for the test tenant (ask the
  owner to check the Settings page; wait):
  1. With one `invm.ps1 -Command` script, as the VM administrator: delete the agent's
     `OTEL_EXPORTER_OTLP_ENDPOINT` entry from the managed settings file and save it, then poll the
     file every 250 ms for 10 s.
  2. Within 5 s the entry is back. Show the timestamps from the polling output and from the agent
     log (`C:\ProgramData\ShadowAICapture\state\capture-core.log`, read with `invm.ps1 -Command`).
  3. `invm.ps1 -AgentState` shows the `tool_config_claude_code` row `tampered` /
     `config_tampered`, and the owner confirms the same on the dashboard's device view after the
     next health report.
- `node tools/accept.mjs` passes.
