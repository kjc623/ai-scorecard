# 14. Group the cloud components under `services/`

## Problem

Three top-level directories exist only to hold one component each (`control/control-api`,
`ingestion/ingest-api`, `vault/content-vault`), `query/` holds two, and `jobs/`, `database/` and
`platform/` sit beside them. The root has more folders than the system has parts, and nothing in the
layout says which ones run in Azure.

## Goal

Every component deployed to Azure, and the library they share, under one `services/` directory:

```
services/
  ingest-api/      ← ingestion/ingest-api/
  control-api/     ← control/control-api/
  content-vault/   ← vault/content-vault/
  query-api/       ← query/query-api/
  dashboard/       ← query/dashboard/
  jobs/            ← jobs/
  database/        ← database/
  platform/        ← platform/
```

`ingestion/`, `control/`, `vault/` and `query/` disappear. Move with `git mv` so history follows.

## Scope

- Go module paths (`github.com/shadow-ai-capture/...`) do **not** change; only the relative paths in
  `replace` directives do.
- Update every reference to an old path: Dockerfiles (`COPY` lines; the build context stays the
  repository root), `.github/workflows/*.yml`, `tools/` (`check-config.mjs` `COMPONENTS`,
  `check-invariants.mjs`, `check-vocab.mjs`), `localdev/` (`build.mjs`, `compose.yaml`, scripts),
  tests that read another component's files (e.g. the dashboard's parity test reads query-api's
  source), `README.md`, `AGENTS.md`, `docs/architecture.md`, `azure/README.md`, component READMEs and
  the other briefs in `backlog/`.
- Out of scope: the device side (task 15), renaming modules, changing any code.

## Done when

- `git grep -n -E "control/control-api|ingestion/ingest-api|vault/content-vault|query/query-api|query/dashboard|(^|[^a-z/])(jobs|database|platform)/"`
  finds no stale path (check each hit; `services/jobs/` and the like are correct).
- `node tools/accept.mjs` passes.
- `docker build -f services/<name>/Dockerfile .` succeeds for all seven images (`ingest-api`,
  `control-api`, `content-vault`, `query-api`, `dashboard`, `jobs`, and `migrate` from
  `services/database/Dockerfile`).
- `docker compose -f localdev/compose.yaml config` succeeds.
