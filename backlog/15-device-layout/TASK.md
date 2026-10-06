# 15. Group the device components under `device/`

Depends on: 14 (both edit the same references; doing them in order avoids conflicts).

## Problem

What runs on a managed device is spread over three top-level directories: `endpoint/` (the agent),
`extension/` (the browser extension) and `installer/` (the packages that install both). The Go
modules already call themselves `github.com/shadow-ai-capture/device/...`; the directories do not.

## Goal

```
device/
  capture-core/     ← endpoint/capture-core/
  capture-spool/    ← endpoint/capture-spool/
  classifier-host/  ← endpoint/classifier-host/
  protocol/         ← endpoint/protocol/
  integration/      ← endpoint/integration/
  extension/        ← extension/
  installer/        ← installer/
  README.md         ← endpoint/README.md, updated to also introduce the extension and installer
```

`endpoint/`, `extension/` and `installer/` disappear from the root. Move with `git mv`.

## Scope

- Go module paths do **not** change; update every `replace` directive that points into `endpoint/`
  (the services' modules point at `endpoint/protocol`, and `endpoint/integration` points at its
  siblings and at `contracts/generated/go`).
- Update every reference: `installer` build scripts (paths to capture-core, classifier-host, the
  extension), the extension's tests and tools that read `endpoint/protocol` or emit golden frames into
  `endpoint/integration/testdata`, `.github/workflows/*.yml`, `tools/check-vocab.mjs`,
  `tools/check-invariants.mjs`, `localdev/` (`lab-msi.mjs`, `build.mjs`), `.gitignore` entries (e.g.
  `extension/tools/extension-key.pem` → `device/extension/tools/extension-key.pem`), `README.md`,
  `AGENTS.md`, `docs/architecture.md`, `azure/RUNBOOK.md` (the extension key path) and READMEs.
- The extension's signing key file is local and git-ignored: move it on disk too if it exists, and
  say so in your report.
- Out of scope: renaming modules, changing any code.

## Done when

- `git grep -n -E "(^|[^a-z/-])(endpoint|extension|installer)/"` finds no stale path (check each hit).
- `node tools/accept.mjs` passes, including the `installer` and `browser` gates on Windows.
- `node device/installer/verify.mjs` passes.
