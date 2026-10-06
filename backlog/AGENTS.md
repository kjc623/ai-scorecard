# Backlog: instructions for agents

Each numbered folder is one product task; its `TASK.md` is the brief. Read the repository's
`AGENTS.md`, `README.md` and `docs/architecture.md` first — the product rules there hold in every
task — then the brief you were given.

| Folder | Task |
|---|---|
| `12-settings` | Collection mode, retention, sanction and content search settings |
| `13-exports` | List export, subject export and erasure |

## How to work

- One branch per task, named `backlog/<folder-name>`. Never commit to `main`; do not push or open a
  pull request unless asked. Never commit `.claude/` or `skills-lock.json`.
- Verify on the local lab (`localdev/README.md`). Never run `node localdev/run.mjs` with any flag,
  and never change the owner's tenant (`11111111-1111-1111-1111-111111111111`); simulated data goes in
  the lab's sample tenant.
- A schema change goes in `database/schema.sql` and, for databases that already exist, in a numbered
  migration (`database/migrations/README.md`). A change to the device envelope starts in `contracts/`.
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
