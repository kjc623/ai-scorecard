# Backlog: instructions for agents

Each numbered folder is one product task; its `TASK.md` is the brief. Read the repository's
`AGENTS.md`, `README.md` and `docs/architecture.md` first — the product rules there hold in every
task — then the brief you were given.

| Folder | Task | Depends on / needs |
|---|---|---|
| `12-settings` | Collection mode, retention, sanction and content search settings | |
| `13-exports` | List export, subject export and erasure | |
| `14-services-layout` | Group the cloud components under `services/` | |
| `15-device-layout` | Group the device components under `device/` | 14 |
| `16-local-clutter` | Remove untracked local state from the working copy | The owner's machine and confirmation |
| `17-browser-tool-names` | Name the tools the browser extension observes | An owner decision (in the brief) |
| `18-dashboard-example-values` | Real identifiers in the dashboard's hints and tests | Better after 17 |
| `19-dashboard-tenant-closed` | Explain a sign-in refused because the tenant is closed | |
| `20-lab-tools-to-localdev` | Move the dashboard's lab tool into `localdev/` | |
| `21-linux-console-user` | Attribute Linux CLI traffic to the signed-in user | A Linux desktop for the owner's check |
| `22-windows-desktop-apps` | Capture desktop AI apps on Windows | An owner decision (in the brief) |
| `23-race-detector` | Run the Go suites under the race detector in CI | |
| `24-job-alerts` | Alert when a scheduled job fails or stops running | |
| `25-ci-green` | Make CI pass on GitHub Actions | The owner pushes the branch |
| `26-azure-validate` | Validate the template against a real subscription | Subscription access |
| `27-macos-package` | Build and install the macOS package | A Mac |

Tasks without a dependency can run in parallel, except that 14 and 15 rewrite paths everywhere: start
other tasks after both have merged, or rebase onto them.

## How to work

- One branch per task, named `backlog/<folder-name>`. Never commit to `main`; do not push or open a
  pull request unless asked. Never commit `.claude/` or `skills-lock.json`.
- Verify on the local lab (`localdev/README.md`). Never run `node localdev/run.mjs` with any flag,
  and never change the owner's tenant (`11111111-1111-1111-1111-111111111111`); simulated data goes in
  the lab's sample tenant.
- A schema change goes in `services/database/schema.sql` and, for databases that already exist, in a numbered
  migration (`services/database/migrations/README.md`). A change to the device envelope starts in `contracts/`.
- A question only the owner can answer is asked in the session — the options, your recommendation,
  and what is hard to change later — and that part waits for the answer. Every other choice is yours.
- Match the code around you. Comments explain what is not obvious, briefly; no references to tasks,
  decisions or history.
- `node tools/accept.mjs` passes before you report.

## Done

1. Every clause under "Done when" that the agent verifies has been observed on the lab; a clause
   about what a page shows is observed in the browser.
2. Tests cover the new behaviour, and every suite passes.
3. Your report (in the session, not a file) says what you built, what you decided, and the commands
   that verify it. Then remove the task's folder from `backlog/`.
