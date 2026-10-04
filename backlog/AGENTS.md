# Backend backlog: instructions for agents

This directory holds the backend work that the dashboard is waiting on. Each numbered folder is one
task; its `TASK.md` is the brief. This file is the context every task assumes. Read it first, then
the task you were given.

OpenCode loads this file when a session is started in `backlog/` or in any task folder beneath it.
If your session was started at the repository root, read it yourself before starting.

## The product, in one paragraph

Shadow AI Capture records what an organisation's people send to AI tools. An agent on each device
(`endpoint/capture-core`) intercepts AI traffic, classifies it on the device and sends events to
`ingestion/ingest-api`. Prompt content, when the collection mode allows it (M3), is sealed on the
device and stored through `vault/content-vault`. `control/control-api` enrols devices and signs the
policy bundle. `query/query-api` is the one read path, a closed query API over PostgreSQL
(`database/schema.sql`). `query/dashboard` is the customer-facing web app.

## Why this backlog exists

The dashboard was walked page by page on live lab data on 2026-10-04. Capture, ingest, content
storage and prompt search work end to end. Most other pages are empty, because nothing computes the
data they read: with 112 submissions captured, the aggregate tables (`mart.agg_*`),
`ops.aggregate_watermark`, `ops.coverage_snapshot`, `ops.collector_state`, `mart.finding`,
`ops.tool` and `ops.user_dim` all had zero rows, and no component in the repository writes them.

## The tasks

| Folder | Task | Depends on |
|---|---|---|
| `01-usage-aggregation` | Fill the usage aggregates and watermarks | none |
| `02-device-liveness-coverage` | Device heartbeat, liveness and fleet coverage | none |
| `03-findings` | Raise findings from classified events | none |
| `04-device-identity` | Hostname, user, agent version and mode per device | 02 |
| `05-tool-catalogue` | Readable tool names and sanction state | 01 |
| `06-directory-sync` | People and departments from the identity provider | 01 |
| `07-class-filter-bug` | Data class filter on events returns nothing | none |
| `08-client-generated-requests` | Mark requests nobody typed; classify typed text only | none |
| `09-prompt-search-filters` | Prompt-text search narrowed by person, tool, device, time | none |
| `10-content-retrieval-path` | Retrieval URL and authenticated blob reads | none |
| `11-sign-in-and-roles` | Authentication and role-based access | none |
| `12-settings` | Collection mode, retention and sanction settings | 04, 05, 11 |
| `13-exports` | List export, subject export and erasure | 06, 11 |

Do 01 to 03 first. 04 to 06 only become visible in the dashboard once aggregates and liveness exist.
Do not start a task whose dependencies are not merged; say so and stop.

## Where things are

Paths are relative to the repository root (`/workspace` inside the OpenCode harness).

| Path | What it is |
|---|---|
| `endpoint/` | The device agent (Go): `capture-core`, the classifier host, `protocol/` |
| `ingestion/ingest-api/` | Accepts device batches (Go) |
| `control/control-api/` | Enrolment, policy bundle, content grants (Go) |
| `vault/content-vault/` | Stores, indexes and serves prompt content (Go) |
| `query/query-api/` | The closed read API (Node, no dependencies); `src/registry.js` lists every source it serves |
| `query/dashboard/` | The web app (zero-dependency ES modules); `README.md` explains its layout |
| `database/schema.sql` | The whole schema, with comments that state intent |
| `contracts/` | The device envelope contract and its generated code |
| `docs/00` to `docs/06`, `docs/adr/` | The design. `docs/04` is the dashboard and query design; `docs/03` the data platform; `docs/06` security |
| `localdev/` | The Docker labs and the harness |
| `tools/accept.mjs` | The repository's acceptance gates |

## Rules that hold in every task

These are product invariants. Tests enforce several of them; do not weaken a test to pass.

- Every table is tenant-scoped under row-level security. A read sets `app.tenant_id` on the session.
- A per-person figure covering fewer than k = 5 people is suppressed, never shown or exported.
- Nothing ranks people by volume. There is no leaderboard, and no list of people sorted by usage.
- The dashboard never shows a fleet percentage. Counts are shown with their denominator.
- A subject-level read is written to the audit trail before it is served.
- `query-api` cannot read prompt content or the search index; only `content-vault` can. Keep that
  separation (`docs/06`).
- The dashboard loads nothing from another host: no CDN, no remote fonts.
- No new third-party dependency without saying so in your report and why it was unavoidable.

Two recent product decisions that differ from the design documents. Follow the product, and update
the document section you touch:

- Reading a stored prompt no longer requires a case reference or a second approver. Opening an event
  in Search shows its prompt. Do not reintroduce the approval.
- The persistent coverage and freshness strip was removed from the dashboard. Degraded coverage is
  said by a banner under the page title.

## The environment

You are in the OpenCode harness: Fedora, workspace at `/workspace`, with Go, Node, `psql`, `rg` and
the host's Docker engine through the mounted socket.

There are two labs, and they are easy to confuse:

- **The default lab** (`node localdev/run.mjs`): network `scorecard`. The harness is on this network,
  so `$LAB_QUERY_URL`, `$LAB_INGEST_URL`, `$LAB_VAULT_URL` and plain `psql` reach **this** lab.
- **The device-auth lab** (`localdev/authlab.compose.yaml`): network `scorecard-authlab`, containers
  named `sac-authlab-*`. This is the one a real endpoint agent is enrolled against, the one with the
  dashboard, and the one the backlog was observed on. Reach it with Docker:
  `docker exec sac-authlab-postgres-1 psql -U postgres -d shadow -c "..."`. Its tenant is
  `11111111-1111-1111-1111-111111111111`. The dashboard is on the host at port 8787
  (`http://host.docker.internal:8787/index.html?transport=live` from inside the harness).

Lab commands:

- Start the device-auth lab: `docker compose -f localdev/authlab.compose.yaml up -d`.
- **Never run `node localdev/run.mjs --auth`.** It regenerates the lab certificate authority, which
  orphans the agent installed on the owner's Windows machine.
- Rebuild the lab images after changing a service: `node localdev/build.mjs --auth`, then
  `docker compose -f localdev/authlab.compose.yaml up -d <service>`.
- Rebuild the dashboard image: `docker build -f query/dashboard/Dockerfile -t sac/dashboard:lab .`
  from the repository root, then `up -d dashboard`.
- After editing `query/dashboard/src/` or its templates, run `node tools/build-index.mjs` in
  `query/dashboard/`; the generated `index.html` and `explore.html` are checked for drift by tests.
- The endpoint agent runs on the owner's Windows host, installed from an MSI
  (`installer/lab-msi.mjs`). You cannot reinstall it from the harness. If a task needs a new agent
  build on the device, build it, say exactly what the owner must run, and verify what you can with
  the device simulator (`localdev/tools/simulate-devices.mjs`).

Tests:

- Node packages: `node --test` inside the package directory (`query/dashboard`, `query/query-api`).
  Do not pass a directory argument.
- Go modules: `go test ./...` inside each module you touched. The modules build offline on the host
  with `GOFLAGS=-mod=mod GOPROXY=off`; if a build here wants to download a module, stop and report it
  rather than adding or upgrading a dependency.
- Whole repository: `node tools/accept.mjs`.

Some suites had failures before this backlog began (three tests in `endpoint/capture-core/cli`, plus
`sac-bundle` and `trust`, fail on Windows). Record the baseline before you change anything, and
report only what you changed.

## How to work

- Branch from the current working branch, one branch per task, named `backlog/<folder-name>`. Never
  commit to `main`. Do not push or open a pull request unless asked.
- Never commit `installer/profiles/lab-host.env` (it holds an enrolment token), `.claude/` or
  `skills-lock.json`.
- Read the design section and the schema comments the task names before writing code. Where the
  design and the code disagree, say which you followed and why.
- A task that says "propose" or "raise" means: write the options and your recommendation into
  `DECISIONS.md` in the task folder and stop for an answer before building that part. Build the
  parts that do not depend on it.
- A schema change goes in `database/schema.sql`, the lab must come up clean from an empty database,
  and your report says how an existing database is brought up to it.
- A change to the device envelope is a change to `contracts/` first; regenerate the generated code.
- Match the code around you: comment density, naming and idiom. The comments in this repository
  explain why, not what.
- When the lead splits a task across teammates, keep one owner per module and merge through the
  lead. Reviewers are read-only.

## Done

A task is done when all of these hold:

1. The behaviour in the task's "Done when" is observed on the device-auth lab, in the dashboard.
2. Tests cover the new behaviour, and every suite you touched passes.
3. The design section whose behaviour you changed is updated.
4. `REPORT.md` exists in the task folder, with: what was built and where; what you verified and how;
   what you could not verify; decisions you made that the owner should know about; anything the
   owner must run by hand.

State plainly what is not finished. A report that says a thing works when it was not run is worse
than one that says it was not run.
