# AGENTS.md

Instructions for agents working in this repository. Read `README.md` and `docs/architecture.md`
first; `backlog/AGENTS.md` adds instructions for the product tasks under `backlog/`.

## Current focus: the first pre-production deployment

The goal is a pre-prod environment in Azure and a real-device test: Intune installs the agent, the
device enrols, its events reach PostgreSQL, and they appear on the dashboard. The code, the template
and the pipelines are ready for it; nothing has been deployed yet. What remains are owner actions,
in order, in `azure/RUNBOOK.md`: subscription and resource group, the deployment identity and GitHub
environment, DNS names and the device TLS certificate, the vendor Entra application, Intune
licensing, then the bootstrap deployment, `azure/scripts/create-secrets.sh`, and the deploy workflow.

## Rules

- `node tools/accept.mjs` passes before you say the repository works. A gate reported `SKIPPED`
  was not checked; say so.
- Work on a branch; never commit to `main`; do not push or open a pull request unless asked. Never
  commit `.claude/` or `skills-lock.json`.
- Never run `node localdev/run.mjs` with any flag: the running lab is the owner's, and a device may be
  enrolled against it. Never change the owner's tenant (`11111111-1111-1111-1111-111111111111`).
- Tests never touch a database they did not create. Live database tests run only when
  `SAC_TEST_PG_DSN` names a database, and use a random tenant.

## Conventions

- Production code only: no feature, flag or file the deployment does not use; the lab adapts to the
  product, not the other way round.
- Well-known libraries over hand-rolled ones; no build tags or stand-ins.
- Comments explain what is not obvious, briefly, in the present tense. No references to decisions,
  tasks, documents or history; the code and its tests are the record.
- One short README per component: what it is, how it runs in production (its configuration), how to
  build and test it.
- A schema change goes in `services/database/schema.sql` and, once an environment exists, also in a numbered
  migration (`services/database/migrations/README.md`). A device envelope change starts in `contracts/`.
- A setting the deployment passes must be read by its component (`tools/check-config.mjs` checks
  this).
