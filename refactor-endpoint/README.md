# Endpoint agent refactor

This folder breaks the owner's plan (`PLAN.md`) into tasks that each fit one agent session.
`DESIGN.md` fixes how the plan maps onto this repository: names, wire shapes, settings, ports and
dependencies. `DECISIONS.md` records what has been settled and why. `AGENTS.md` holds the rules for
the agents doing the work. `TESTBED.md` describes the test environment of the device phase: pre-prod
(Fly.io and Supabase) with a test tenant, and the reference VM where every on-device check runs
(Hyper-V, Entra-joined, Intune-managed). The local lab is not used.

The work runs in two phases (`AGENTS.md`):

1. **Build** (tasks 01–57). Each task is built and verified on the PC with its tests and
   `node tools/accept.mjs`. No pre-prod and no device exist yet; a brief's "On the device" section
   waits.
2. **Device** (tasks 58–60). When the build is far enough along, the owner brings up pre-prod (58)
   and the reference VM tooling (59), and task 60 runs every brief's "On the device" section in
   order, fixing what it finds.

Tasks run in number order unless "Depends on" allows otherwise. Tasks 13, 30–32, 37–41, 43, 45–47
and 52 include steps the owner does at the VM's console in a desktop app (`AGENTS.md`); the agent
stops and asks at those points. Every task leaves dashboard settings and checks to the owner.
"Needs" lists what the owner must supply for the task's device checks: on the reference VM,
installed and signed in as the console user unless it says otherwise. Phase 7's machines are needed
for the build too. Three tasks have no build step (22, 42, 43) and run only in the device phase.
The Plan column gives the plan's task id. "Built" is ticked when the task's "Done when" is met;
"Device" when task 60 has run its "On the device" section.

| # | Task | Plan | Depends on | Needs | Built | Device |
|---|---|---|---|---|---|---|
| **Phase 0: Foundation** | | | | | | |
| 01 | [Make the policy drift test run](01-bundle-drift-test/TASK.md) | E03 | | | [x] | — |
| 02 | [Serialise classifier requests](02-classifier-link-serialise/TASK.md) | E24, E25 | | | [x] | — |
| 03 | [Envelope: endpoint routes and kinds](03-envelope-endpoint-kinds/TASK.md) | E02 | | | [x] | — |
| 04 | [Server: store the new kinds](04-ingest-endpoint-kinds/TASK.md) | E02 | 03 | | [x] | [ ] |
| 05 | [Device: mint the new kinds](05-device-envelope-kinds/TASK.md) | E02 | 03, 04 | | [x] | — |
| 06 | [Collector lifecycle and policy toggles](06-collector-lifecycle/TASK.md) | E01, E03 | 05 | | [x] | — |
| 07 | [Policy: the endpoint section](07-endpoint-policy-section/TASK.md) | E03 | 01, 06 | | [x] | [ ] |
| 08 | [TLS inspection becomes opt-in](08-tls-inspection-opt-in/TASK.md) | E36 | 07 | | [x] | [ ] |
| 09 | [Process attribution](09-process-attribution/TASK.md) | E04, E15 | 06 | | [x] | [ ] |
| 10 | [User-session helper](10-user-session-helper/TASK.md) | E05 | 09 | | [x] | [ ] |
| 11 | [Rules in policy](11-rules-policy/TASK.md) | E03, E30 | 07 | | [x] | [ ] |
| 12 | [Device rule engine](12-device-rule-engine/TASK.md) | E25, E35 | 11 | | [x] | [ ] |
| 13 | [Extension policy hand-off](13-extension-policy-handoff/TASK.md) | E30 | 12 | Chrome or Edge | [x] | [ ] |
| **Phase 1: Discovery** | | | | | | |
| 14 | [App catalog](14-app-catalog/TASK.md) | E06 | 07 | | [x] | [ ] |
| 15 | [Discovery emitter](15-discovery-emitter/TASK.md) | E13 | 05, 06, 14 | | [x] | — |
| 16 | [Installed app scanner](16-installed-app-scanner/TASK.md) | E07 | 15 | Claude Desktop, ChatGPT Desktop, Cursor installed | [x] | [ ] |
| 17 | [CLI and package scanner](17-cli-scanner/TASK.md) | E08 | 16 | claude, codex, gemini, copilot CLIs installed | [x] | [ ] |
| 18 | [IDE extension scanner](18-ide-extension-scanner/TASK.md) | E09 | 16 | VS Code with Copilot, Claude Code and Continue extensions | [x] | [ ] |
| 19 | [Process monitor](19-process-monitor/TASK.md) | E10 | 09, 15 | | [x] | [ ] |
| 20 | [Local model detection](20-local-model-detection/TASK.md) | E11 | 16 | Ollama with two models | [x] | [ ] |
| 57 | [Local model capture in policy](57-loopback-policy/TASK.md) | E11 | 08, 20 | Ollama with two models | [ ] | [ ] |
| 21 | [Connection monitor](21-connection-monitor/TASK.md) | E12 | 19 | | [x] | [ ] |
| 22 | [Discovery volume check](22-discovery-volume-check/TASK.md) | E13 | 16–21 | 24 h of ordinary use on the reference VM | — | [ ] |
| **Phase 2: Native telemetry** | | | | | | |
| 23 | [Local OTLP receiver](23-otlp-receiver/TASK.md) | E14 | 06, 07 | | [x] | [ ] |
| 24 | [OTLP sender attribution](24-otlp-attribution/TASK.md) | E15 | 09, 23 | A second local Windows account | [x] | [ ] |
| 25 | [Claude Code telemetry fixtures](25-claude-code-otel-fixtures/TASK.md) | E16 | 23 | Claude Code signed in | [x] | [ ] |
| 26 | [Claude Code normalizer](26-claude-code-normalizer/TASK.md) | E17 | 25 | | [x] | [ ] |
| 27 | [Claude Code config writer](27-claude-code-config-writer/TASK.md) | E18 | 26 | Claude Code signed in | [x] | [ ] |
| 28 | [Codex fixtures and normalizer](28-codex-otel-normalizer/TASK.md) | E19 | 27 | Codex CLI signed in | [x] | [ ] |
| 29 | [Codex config writer](29-codex-config-writer/TASK.md) | E19 | 28 | Codex CLI signed in | [ ] | [ ] |
| 30 | [Copilot fixtures and normalizer](30-copilot-otel-normalizer/TASK.md) | E20 | 27 | VS Code with Copilot, Copilot CLI, a Copilot licence | [x] | [ ] |
| 31 | [Copilot config writer](31-copilot-config-writer/TASK.md) | E20 | 30 | as 30 | [ ] | [ ] |
| 32 | [Claude Cowork spike](32-claude-cowork-spike/TASK.md) | E21 | 27 | Claude Desktop with Cowork | [x] | [ ] |
| 33 | [Generic GenAI normalizer](33-genai-normalizer/TASK.md) | E22 | 24 | | [x] | [ ] |
| 34 | [Config drift watcher](34-config-drift-watcher/TASK.md) | E23 | 27, 29, 31 | | [ ] | [ ] |
| 35 | [OTel content privacy](35-otel-content-privacy/TASK.md) | E24 | 02, 26 | | [x] | — |
| **Phase 3: Hooks** | | | | | | |
| 36 | [Hook relay](36-hook-relay/TASK.md) | E25 | 02, 10, 12 | | [x] | [ ] |
| 37 | [Claude Code hooks](37-claude-code-hooks/TASK.md) | E26 | 27, 36 | Claude Code signed in | [x] | [ ] |
| 38 | [Cursor hooks](38-cursor-hooks/TASK.md) | E27 | 36 | Cursor signed in | [x] | [ ] |
| 39 | [Other tools' hooks spike](39-other-hooks-spike/TASK.md) | E28 | 36 | Codex, Copilot CLI, Gemini CLI | [ ] | [ ] |
| 40 | [Hook and OTel merge](40-hook-otel-merge/TASK.md) | E29 | 26, 37 | Claude Code signed in | [x] | [ ] |
| 41 | [Coaching messages](41-hook-user-messaging/TASK.md) | E30 | 37, 38 | Claude Code, Cursor | [ ] | [ ] |
| **Phase 4: Network inspection (opt-in)** | | | | | | |
| 42 | [macOS transparent proxy spike](42-macos-transparent-proxy-spike/TASK.md) | E31 | 08 | A Mac, Xcode, an Apple developer account | — | [ ] |
| 43 | [Windows desktop path spike](43-windows-desktop-path-spike/TASK.md) | E32 | 08, 09 | Claude Desktop, ChatGPT Desktop | — | [ ] |
| 44 | [Non-exportable device CA](44-device-ca-non-exportable/TASK.md) | E33 | 08 | | [x] | [ ] |
| 45 | [Per-app protocol parsers](45-app-parsers/TASK.md) | E34 | 43 | Claude Desktop, ChatGPT Desktop | [x] | [ ] |
| 46 | [Inline policy in the proxy](46-proxy-inline-policy/TASK.md) | E35 | 10, 12, 45 | Claude Desktop | [x] | [ ] |
| 47 | [Proxy safety valves](47-proxy-safety-valves/TASK.md) | E36 | 46 | | [x] | [ ] |
| **Phase 5: Components, health, uninstall** | | | | | | |
| 48 | [Supervised components](48-supervised-components/TASK.md) | E37 | 06 | | [x] | [ ] |
| 49 | [Per-tool health in the cloud](49-collector-health/TASK.md) | E39 | 27, 29, 31, 38 | | [ ] | [ ] |
| 50 | [Uninstall restores the machine](50-uninstall-cleanup/TASK.md) | E40 | 37, 38, 44 | | [ ] | [ ] |
| **Phase 6: Verification** | | | | | | |
| 51 | [Performance budgets](51-performance-budgets/TASK.md) | E42 | 36, 23 | | [x] | [ ] |
| 52 | [Privacy canaries](52-privacy-canaries/TASK.md) | E43 | 40, 46 | | [ ] | [ ] |
| **Phase 7: macOS and Linux** | | | | | | |
| 53 | [macOS: attribution, helper and discovery](53-macos-discovery/TASK.md) | E04, E05, E07–E12 | 21, backlog 27 | A Mac | [ ] | [ ] |
| 54 | [macOS: native telemetry and hooks](54-macos-native-telemetry/TASK.md) | E18–E20, E26 | 53, 41 | A Mac with Claude Code, Codex, Cursor | [ ] | [ ] |
| 55 | [Linux: attribution, helper and discovery](55-linux-discovery/TASK.md) | E04, E05, E07–E12 | 21 | A Linux desktop (systemd, logind) | [ ] | [ ] |
| 56 | [Linux: native telemetry and hooks](56-linux-native-telemetry/TASK.md) | E18–E20, E26 | 55, 41 | A Linux desktop with Claude Code, Codex | [ ] | [ ] |
| **Phase 8: Pre-prod and device verification** | | | | | | |
| 58 | [Pre-prod environment and test tenant](58-preprod-environment/TASK.md) | | the build tasks to verify, merged to `main` | Fly.io and Supabase accounts; `fly/RUNBOOK.md` owner steps | [ ] | — |
| 59 | [Reference VM tooling](59-reference-vm/TASK.md) | | 58 | `TESTBED.md` VM checklist done | [ ] | — |
| 60 | [Device verification](60-device-verification/TASK.md) | | 59 | each brief's "Needs", on the VM | — | [ ] |

Deferred by the owner: E38 (signed and notarised packages, MDM profiles, Jamf) and E41 (nightly
vendor-tool compatibility matrix).

Not in the plan, so not here: dashboard pages for discovery data and agent activity. Events are
stored and queryable after task 04, but no new page shows them.
