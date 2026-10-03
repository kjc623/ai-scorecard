# `tools/` — the acceptance gates

One command decides whether this repository still works:

```
node tools/accept.mjs
```

It exits 0 only when every gate passes, and it says which gate failed and why. Nothing here is a
component; these are the checks that compare components against something *outside* themselves.

## Why this directory exists

Every component in this repository has its own test suite, and every one of those suites can only
confirm its author's model of the world. A component's tests passing is necessary and not sufficient:
they cannot see that the wire contract drifted, that two components spell the same closed enum
differently, or that the database's assertions no longer hold. This directory holds the checks that
can, and `accept.mjs` runs them in one place so "the build is green" is one command with one exit
code rather than five results someone has to hold in their head.

The pattern that matters: **every check here compares a component against an artefact it does not
own** — the contract, the live PostgreSQL catalog, another component's enum, or a real browser.
That is deliberate. A check that reads a component's own files can only agree with them.

## The commands

| Command | What it decides | Exit |
|---|---|---|
| `accept.mjs` | all gates below, plus a JSON evidence record under `.integration/` | non-zero if any gate fails |
| `accept.mjs --list` | the gates and, for each, what a failure means | 0 |
| `accept.mjs --skip db` | the same run with a gate skipped. A skipped gate is reported as SKIPPED with a reason and is **not** a pass | — |
| `verify-all.mjs` | every declared package's own suite. `MISSING` (no directory) and `NO-TESTS` are failures, not passes. Each package has a hard time budget; `TIMEOUT` is its own status | non-zero |
| `check-seams.mjs` | envelope field names used by six components, against `contracts/event-envelope.schema.json`. A device carrying a server-assigned field is a finding | non-zero |
| `check-vocab.mjs` | eight closed vocabularies, compared **by value** between `endpoint/protocol` (Go) and `extension/src/messages.js` (JS) | non-zero |
| `check-invariants.mjs` | the six architecture invariants, mechanically. Reports BLOCKED when a component does not exist rather than passing | non-zero |
| `update-cockpit.mjs` | refreshes `.cockpit/project.json` component paths and statuses from what is on disk. `--check` reports drift and changes nothing | non-zero on drift with `--check` |
| `update-cockpit-tasks.mjs` | writes the task completion notes into the same manifest | — |

The `db` gate is `database/tools/run-invariants.ps1`, and the browser and endpoint gates live with
their components (`extension/tools/in-browser-check.mjs`, and `endpoint/capture-core --selftest`).
`accept.mjs` invokes them rather than duplicating them.

## What these checks do NOT prove

Each tool states its own limits in its output, and the honest summary is:

- they read **source text and live catalogs**, never runtime behaviour across a seam;
- `check-seams.mjs` compares field **names**, not nesting, optionality or types;
- `check-vocab.mjs` compares values, not whether either side uses them correctly;
- `check-invariants.mjs` exits 0 while individual invariants are BLOCKED, so a green `invariants`
  gate means "no FAIL", not "all six verified";
- a gate that could not run is `SKIPPED`, which is the opposite of a green tick.

`.integration/LEAD-VERIFICATION.md` is where the Lead records the checks it reproduced personally,
including the ones that failed. `.integration/REPORT.md` is a separate, independent verifier's report.

## Adding a gate

Add it to the `GATES` array in `accept.mjs` with a `decides` line — the sentence a reader needs when
the gate goes red. A gate whose failure mode is unclear is a gate people route around.
