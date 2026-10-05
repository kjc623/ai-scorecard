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
| `00-environment-check` | Run everything in `ENVIRONMENT.md`; write `BASELINE.md` | none |
| `01-usage-aggregation` | Fill the usage aggregates and watermarks | none |
| `02-device-liveness-coverage` | Device heartbeat, liveness and fleet coverage | 01 |
| `03-findings` | Raise findings from classified events | 01 |
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

00 is not product work. It checks that `ENVIRONMENT.md` is true and records the test
failures that exist before any work is done. Run it before the first task of a new session if
`BASELINE.md` is missing or older than the harness image.

Tasks are done one at a time, and each is built on top of the one before it. The owner merges
finished tasks to `main` through pull requests, when they choose to, so the earlier work may be on
`main` or still on the previous task's branch. A task is finished when its folder holds a
`REPORT.md`. "Depends on" means that task must already be finished in the tree you start from. If
the `REPORT.md` of a task yours depends on is not there, say so and stop.

The "Depends on" column was written before any task was built and has been wrong: 02 and 03 were
listed as depending on nothing, and both were built inside the module 01 created. Treat "none" as
"none known". If your task needs something an unfinished task was going to provide, say so and
stop.

01 to 04 are finished. 05 and 06 only became visible in the dashboard once aggregates and liveness
existed, which they now do.

## Where things are

Paths are relative to the repository root (`/workspace` inside the OpenCode harness).

| Path | What it is |
|---|---|
| `endpoint/` | The device agent (Go): `capture-core`, the classifier host, `protocol/` |
| `ingestion/ingest-api/` | Accepts device batches (Go) |
| `control/control-api/` | Enrolment, policy bundle, content grants (Go) |
| `vault/content-vault/` | Stores, indexes and serves prompt content (Go) |
| `query/query-api/` | The closed read API (Node, one dependency: `jose`, for verifying the product token); `src/registry.js` lists every source it serves |
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

`backlog/ENVIRONMENT.md` describes the harness you are in: what it can reach, the two tenants in
the lab's database and their dashboards, the lab commands, how to open a dashboard page in the
headless browser, and how to run the tests. Read it before you run anything. It is a separate file
only to keep this one short; it is not optional.

Three things from it matter enough to repeat here:

- **Never run `node localdev/run.mjs`, with any flag.** It can regenerate the lab certificate
  authority, which orphans the agent installed on the owner's Windows machine.
- **Never change the owner's tenant** (`11111111-1111-1111-1111-111111111111`): not with `psql`,
  not with simulated traffic, not through the product's own writes. It holds what the owner's real
  device sent. Simulated data goes in the sample tenant, which is yours to fill and reshape.
- **A claim about what a page shows is checked in the browser**, with
  `query/dashboard/tools/observe.mjs`, not by reading the query API.

## How to work

- One branch per task, named `backlog/<folder-name>`, cut from the branch the session starts on.
  The owner leaves the repository on the branch that holds the most recently finished work, which
  is `main` once that work is merged. Before you cut, check that the `REPORT.md` of the last
  finished task is in the tree. Never commit to `main`. Do not push or open a pull request unless
  asked.
- Never commit `installer/profiles/lab-host.env` (it holds an enrolment token), `.claude/` or
  `skills-lock.json`.
- Read the design section and the schema comments the task names before writing code. Where the
  design and the code disagree, say which you followed and why.
- Check your brief against what has happened since it was written. The briefs were all written on
  2026-10-04, before any task was built, and finished tasks have changed things they rely on.
  Before building, read the entries naming your task in `backlog/FOLLOWUPS.md` and the ADRs under
  `docs/adr` added since that date. Where one of them contradicts your brief, the recorded decision
  wins: follow it and say in your report which sentence of the brief you set aside. If what is
  left of the brief no longer says clearly what to build, stop and ask.
- `backlog/OWNER-TODO.md` lists what earlier tasks left for the owner: commands to run, things to
  check, questions to answer. An unticked line has not happened. If your task relies on one, such
  as a reinstall of the agent on the Windows host, check before building on it. When your task
  leaves something for the owner, add a line there, under Run, Verify or Answer, naming your task.
- Do not write a `DECISIONS.md`, or any other document that argues a decision. A task that says
  "propose" or "raise" names a question the owner wants to answer: ask it in the session, in a few
  lines (the options, your recommendation, and what is hard to change later), and stop for an
  answer before building that part. Build the parts that do not depend on it. Every other choice
  is yours to make. Record a decision, the owner's or yours, in a line or two in the report, and
  what it changes for a later task in `FOLLOWUPS.md`.
- A schema change goes in `database/schema.sql`, the lab must come up clean from an empty database,
  and your report says how an existing database is brought up to it.
- A change to the device envelope is a change to `contracts/` first; regenerate the generated code.
- Match the code around you: comment density, naming and idiom. The comments in this repository
  explain why, not what.
- When the lead splits a task across teammates, keep one owner per module and merge through the
  lead. Reviewers are read-only.

## Passing on what you learned

The next agent starts from this file and its own brief, and does not read your report. If you
worked out something it would otherwise work out again, write it where it will be found:

- A fact about one script, service or test goes in that file's header or its README, next to the
  thing. Most of what you learn belongs there.
- A fact about the harness or the lab (a command, an address, what a tool needs) goes in
  `ENVIRONMENT.md`.
- A rule that holds in every task, whatever it touches, goes in this file, in the section it
  belongs to.

For this file and `ENVIRONMENT.md` the bar is higher, because every agent reads all of both before
every task and each added line makes the others easier to miss:

- Write only what you ran and saw. An inference goes in your report, marked as one.
- Correct or replace the sentence that was wrong. Do not add a second sentence beside it.
- If the next agent on an unrelated task would lose nothing without it, it does not belong here.
- Say it in a line or two. If it needs more, put the detail next to the thing and point at it.

List each such edit in your report, here or elsewhere, so the owner can see it and take it out.

Two kinds of thing go in `backlog/FOLLOWUPS.md` instead, one line each:

- **A later brief your work has made wrong.** Read the briefs of the tasks not yet done. Where
  what you built or decided contradicts a sentence in one, add a row: the brief, what it says, what
  is now true. Do not edit the brief; it is the owner's instruction and changes by the owner's
  hand. The agent that picks that task up reads your row before it starts.
- **Work you found and did not do.** Anything your report would call a follow-up, a later task or
  "not built". Say why it was left.

## Done

A task is done when all of these hold:

1. Every clause under "The agent verifies" in the task's "Done when" has been observed on the
   device-auth lab. A clause about what a page shows is observed in the browser, with
   `tools/observe.mjs`.
2. Tests cover the new behaviour, and every suite you touched passes.
3. The design section whose behaviour you changed is updated.
4. `REPORT.md` exists in the task folder, as described below.
5. Everything the task leaves for the owner is a line in `backlog/OWNER-TODO.md`.

The clauses under "The owner verifies" need the real device or a person looking, so they are not
yours to claim. Your part is to make them checkable: build what the device needs and write the
hand-off.

## The report

The owner reads `REPORT.md` to find out whether the task is finished and what is now waiting on
them. Write it for that reader, in this order, so they can stop reading as soon as they have what
they came for:

1. **Verdict.** A table with one row for each clause of the task's "Done when", from both lists:
   the clause quoted, a verdict, the tenant it was observed in, and the evidence. The verdicts are:
   - **met**: observed as the clause is written. Name the evidence: the file under
     `.integration/observe/`, the test, or the query and its result.
   - **met by substitute**: something else stood in for what the clause names, for example a
     direct call to `ingest.record_event` in place of the simulator. Say what stood in for what.
   - **not met**: say why, in a line.
   - **owner**: a clause from "The owner verifies". Say what the owner must do first, if anything.
2. **For the owner to do.** The lines you added to `OWNER-TODO.md`, with the exact commands.
3. **Decisions.** Each choice the owner would want to know about, in a line or two: what you
   chose, and what it would take to change.
4. **Not finished, not verified.** What the brief asked for that is not there, and what is there
   but was not seen working.
5. **Downstream impact.** The rows you added to `FOLLOWUPS.md`, with the wording you propose for
   each brief that is now wrong, or a line saying you read the remaining briefs and found none.
6. **What was built.** The branch it was cut from; the earlier tasks whose work it used, one line
   each; then the modules touched and what each now does. Name modules, not files: the commit
   lists the files.
7. **Evidence.** What you ran and what it showed, for anything the verdict table did not already
   cover: test counts that changed against `BASELINE.md`, the schema applied from empty, and edits
   you made to this file or to a script's header.

Leave out the brief restated, the baseline explained, and the reasoning behind a choice the owner
did not ask about. A report that covers its task in about 80 lines is the usual size; a longer one
should be longer because the task left more for the owner, not because more was written down.

The verdict table has to be true on its own. State plainly what is not finished. A report that
says a thing works when it was not run is worse than one that says it was not run.
