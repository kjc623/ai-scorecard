# Endpoint agent refactor: macOS and Linux

The endpoint refactor (`refactor-endpoint/`) runs against Windows only: its build tasks, pre-prod,
the reference VM and the device verification. The tasks here port the Windows work to macOS and
Linux, and they wait until Windows is tested end to end (task 60 done). They keep their task
numbers; the briefs in the Windows tasks refer to them by those numbers.

The rules are the ones in `refactor-endpoint/AGENTS.md`; `refactor-endpoint/DESIGN.md` (§12,
Platforms) and `refactor-endpoint/DECISIONS.md` apply unchanged. Each task needs the machine its
"Needs" line names, for the build as well as for the device checks. Task 42 has no build step.

| # | Task | Plan | Depends on | Needs | Built | Device |
|---|---|---|---|---|---|---|
| 42 | [macOS transparent proxy spike](42-macos-transparent-proxy-spike/TASK.md) | E31 | 08 | A Mac, Xcode, an Apple developer account | — | [ ] |
| 53 | [macOS: attribution, helper and discovery](53-macos-discovery/TASK.md) | E04, E05, E07–E12 | 21, backlog 27 | A Mac | [ ] | [ ] |
| 54 | [macOS: native telemetry and hooks](54-macos-native-telemetry/TASK.md) | E18–E20, E26 | 53, 41 | A Mac with Claude Code, Codex, Cursor | [ ] | [ ] |
| 55 | [Linux: attribution, helper and discovery](55-linux-discovery/TASK.md) | E04, E05, E07–E12 | 21 | A Linux desktop (systemd, logind) | [ ] | [ ] |
| 56 | [Linux: native telemetry and hooks](56-linux-native-telemetry/TASK.md) | E18–E20, E26 | 55, 41 | A Linux desktop with Claude Code, Codex | [ ] | [ ] |
