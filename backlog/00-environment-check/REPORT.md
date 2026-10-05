# 00. Environment check — report

## Verdict

| Clause (from "Done when") | Verdict | Tenant | Evidence |
|---|---|---|---|
| "Every command in `ENVIRONMENT.md` has a recorded result, and the report lists the ones that did not work as written with what was done about each." | met | harness/lab | Command-by-command results and timings under **Evidence**. No command failed as written, so there is nothing to correct or leave. |
| "`backlog/BASELINE.md` exists with the contents above." | met | harness/lab | `backlog/BASELINE.md`: header (date, commit, image id, lab state), the four-row failure table with groups, the passing suites with counts, and the run-to-run note. |
| "The simulator ran once from the harness and the report gives the working invocation and what it added to the lab." | met | sample (added); owner's tenant read-only | `node localdev/tools/simulate-devices.mjs --edge https://edge:8443 --devices 8 --events 240` → accepted 241/241. Sample tenant: submissions 201→406, observations 240→481, devices 8→8, collector-state 48→48. Owner's tenant: submissions 1373, observations 1711, devices 10, submission digest `fbd4ff3767f2`, identical before and after. |
| "`observe.mjs` opened a dashboard page from the harness." | met | owner's tenant and sample | `.integration/observe/2026-10-05T03-16-40-735Z-index-html-transport-live-devices.{txt,png,html}` (owner, `--expect Reporting` held), `2026-10-05T03-16-44-854Z-index-html-transport-live-tools.{txt,png,html}` (sample), `2026-10-05T03-16-48-403Z-explore-html-transport-live-events.{txt,png,html}` (owner Search). Each header: settled yes, console errors 0, failed requests 0, other-host requests 0. |
| "`git status` shows changes only under `backlog/`." | not met as written | — | The tree carries three entries that predate this task and that task 00 does not own: ` M installer/profiles/lab-host.env` (the owner's enrolment profile: local CA path and a token, which `AGENTS.md` says must never be committed), `?? .claude/` and `?? skills-lock.json`. Every change task 00 authored is under `backlog/`; see **Not finished, not verified**. |
| "The decisions the report leaves open: for each broken command or failing suite, whether the repository is fixed or the failure stays in the baseline as known." (owner) | owner | — | Read the failure table in `backlog/BASELINE.md`, then answer the task-00 line in `backlog/OWNER-TODO.md`. Before deciding, `node tools/accept.mjs` shows all four failures, and `backlog/BASELINE.md` names each one's cause and group. |

## For the owner to do

One line was added to `backlog/OWNER-TODO.md` (Answer): **Decide the disposition of the four
baseline failures** — for each, whether the repository is fixed or the failure stays in the
baseline as known. To see them first:

```
cd /workspace && node tools/accept.mjs                                   # packages, seams and db fail; browser skips
cd /workspace/endpoint/classifier-host && go test ./...                  # 2 failures, peak-memory assertions
cd /workspace/ingestion/ingest-api && go test ./...                      # 5 failures, tenant_pkey collision
cd /workspace && node tools/check-seams.mjs                              # the policy_rule_id finding
```

## Decisions

- **Branch.** `backlog/00-environment-check`, cut from `main` at
  `78e85d8f2be360801b20b12bb4f46bc02fb9b559`. Task 00 changes no product code.
- **The one command not run literally as written.** `ENVIRONMENT.md` says
  `up -d <service>` after rebuilding; because `build.mjs --auth` rebuilt every image at once, I ran
  `up -d` with no service to recreate them all. Same for the dashboard step (`up -d dashboard
  dashboard-sample`). Both worked.
- **Suite invocation.** I ran `node --test` directly for the two packages `ENVIRONMENT.md` names
  (`query/dashboard`, `query/query-api`) and `go test ./...` directly for every Go module, then
  `tools/verify-all.mjs` and `tools/accept.mjs`. The direct Node runs count one more "test" per
  package than `verify-all.mjs` because default discovery loads `test/helpers.mjs`; `BASELINE.md`
  records both.
- **Classification** of the four failures into the brief's three groups is in `BASELINE.md`; the
  `seams` finding is grouped "the code is wrong" because that is what the gate decides, though the
  field it flags belongs to the content-grant request, not the envelope. If the owner prefers that
  read differently, the report and `BASELINE.md` change with it.
- **Restored artifacts.** The `endpoint/classifier-host` suite rewrites its tracked `reports/`
  files; I restored them with `git checkout -- endpoint/classifier-host/reports/` so the tree
  carries no test byproduct.
- **Where the simulator fact went.** Re-running `--devices 8` adds events but no device rows
  (fixed hardware-identity hash per index). That belongs next to the simulator, but task 00's
  Done-when keeps changes under `backlog/`, so it is recorded in `BASELINE.md` instead.
- **`ENVIRONMENT.md` pruning.** Trimmed the simulator's history clause and the "the one with the
  dashboard" clause; added one sentence that `git status` is never clean (the three foreign
  entries), which a starting agent needs so it does not think it caused them. The file is the same
  length as before. Nothing in it was found to be wrong.

## Not finished, not verified

- **`git status` is not clean of non-`backlog/` entries.** I did not touch
  `installer/profiles/lab-host.env`, `.claude/` or `skills-lock.json`: the first holds the owner's
  enrolment token, and none is a task-00 change. I did not stash or delete them.
- **The `browser` accept gate was skipped**, not passed: it looks for a Windows Edge/Chrome path.
  The real-browser half of acceptance was therefore not exercised here.
- **The `db` gate's own assertions were not run** (no `powershell`). Its FAIL is recorded as a
  baseline failure, not as evidence about the schema.
- **The owner's verdict clause is open** (above). Nothing was fixed, per "Do not fix what you
  find".
- **The default lab** (`localdev/docker-compose.yml`) was not touched; no task uses it.
- **No schema was applied from empty.** Task 00 changes no schema; the lab's schema one-shot
  reported "already applied (schema 'ingest' exists)".

## Downstream impact

I read the briefs of tasks 05–13. None names a suite, `accept.mjs`, `powershell` or the baseline,
so no brief is made wrong by this work and no "brief now wrong" row was added. Four rows were added
to the **Deferred work** list in `backlog/FOLLOWUPS.md`, one per baseline failure: the
classifier-host peak-memory tests, the ingest-api tests binding to the live lab database, the `db`
gate failing instead of skipping without `powershell`, and the `seams` checker reading the
content-grant request as the envelope. Each says task 00 recorded the baseline and left the fix to
the owner.

## What was built

- Branch `backlog/00-environment-check`, cut from `main` at `78e85d8`.
- Earlier tasks used: none. Task 00 changes no product code and used no earlier task's work beyond
  the lab the backlog runs on.
- `backlog/BASELINE.md` — new; the reference for later tasks.
- `backlog/ENVIRONMENT.md` — pruned (two clauses), plus the `git status` fact.
- `backlog/OWNER-TODO.md` — one Answer line.
- `backlog/FOLLOWUPS.md` — four Deferred-work rows.
- `backlog/00-environment-check/REPORT.md` — this file.

## Evidence

Command results, in the order `ENVIRONMENT.md` gives them (all from `/workspace`; no command
failed):

| Command | Result | Time |
|---|---|---|
| `docker compose -f localdev/authlab.compose.yaml up -d` | ok; 11 containers, schema one-shot exited 0 | 2.1 s |
| `node localdev/build.mjs --auth` | ok; 6 lab + 2 plain + 4 tagged SQL images built | 7 m 0 s |
| `docker compose -f localdev/authlab.compose.yaml up -d` | ok; services recreated on the new images | 7.4 s |
| `docker build -f query/dashboard/Dockerfile -t sac/dashboard:lab .` | ok (cached) | 19.8 s |
| `docker compose -f localdev/authlab.compose.yaml up -d dashboard dashboard-sample` | ok | 1.5 s |
| `node tools/build-index.mjs` (in `query/dashboard`) | ok; `index.html` 180692 B, `explore.html` 248402 B; no drift | 0.2 s |
| `psql -Atc '…'` | ok; `shadow / postgres / tenants=2` | 17 ms |
| `curl http://query-api:8080/healthz` | 200 `{"status":"ok","role":"sac_query"}` | 10 ms |
| `curl http://ingest-api:8080/healthz` | 200 `{"status":"ok"}` | 10 ms |
| `curl http://content-vault:8080/healthz` | 200 | 9 ms |
| `curl --cacert localdev/.authlab/dev-ca.crt https://edge:8443/` | TLS verified; HTTP 404 (no root route) | 21 ms |
| `curl http://dashboard:8787/index.html` | 200 | 11 ms |
| `curl http://dashboard-sample:8787/index.html` | 200 | 11 ms |
| `node localdev/tools/simulate-devices.mjs --edge https://edge:8443 --devices 8 --events 240` | ok; accepted 241 | 1.2 s |
| `node query/dashboard/tools/observe.mjs 'index.html?transport=live#devices' --expect Reporting` | ok; check held | 4.2 s |
| `node query/dashboard/tools/observe.mjs 'http://dashboard-sample:8787/index.html?transport=live#tools'` | ok | 4.0 s |
| `node query/dashboard/tools/observe.mjs 'explore.html?transport=live#events'` | ok | 3.6 s |
| `node tools/verify-all.mjs` | 16 pass, 2 fail | 1 m 58 s |
| `node tools/accept.mjs` | 5 pass, 3 fail, 1 skip | 2 m 18 s |

Test counts against `BASELINE.md`: this task writes the baseline, so there is no delta to report.
The measured state is 16/18 package suites passing; both Node packages ENVIRONMENT names pass
(`query/dashboard` 145 tests/139 pass/6 skip, `query/query-api` 214/199/15). The two failing package
suites and the `seams`/`db` gate failures and the skipped `browser` gate are in `BASELINE.md`.

Edits to `ENVIRONMENT.md` (for the owner to strike if unwanted): one sentence added about
`git status` never being clean; the simulator history clause and the "the one with the dashboard"
clause removed. No script header or `AGENTS.md` was edited, so `git status` shows no change there.
