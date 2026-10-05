# 00. Environment check

Depends on: nothing. Do this before any other task, and again whenever the harness image, the lab's
services or the set of test suites changes.

## Problem

The environment instructions, at the time a section of `AGENTS.md`, were written from memory on
the owner's Windows host, and were not run in the harness before tasks 01 to 04 used them. They
were wrong in ways each of those tasks paid for separately:

- It named baseline failures (`endpoint/capture-core/cli`, `sac-bundle`, `trust`) that are Windows
  failures. The harness is Fedora and fails a different set, which all four reports rediscovered
  and explained again, and two of them explained differently.
- `node localdev/build.mjs --auth` did not work until task 01 fixed a Dockerfile on the way past.
- The harness was not on the device-auth lab's network. Task 03 concluded the device simulator
  cannot run from the harness; 01, 02 and 04 ran it.

The network has since been corrected, a headless browser and a sample tenant added, and the
instructions rewritten as `backlog/ENVIRONMENT.md`. That file has not been run end to end either.

## Goal

Every command in `backlog/ENVIRONMENT.md` has been run in the harness as written, and the file
says only things that were seen to work. The test failures that exist before any work
is done are written down once, in `backlog/BASELINE.md`, so that later tasks report differences from
it instead of rediscovering it.

This task changes no product code. Its outputs are `backlog/BASELINE.md`, corrections to
`backlog/ENVIRONMENT.md`, and its report.

## Do

Work on a clean tree at the current branch head. Record the commit.

1. **The commands.** Run each command `ENVIRONMENT.md` gives, in the order it gives them:
   starting the lab, rebuilding the lab images, rebuilding the dashboard image, regenerating the
   dashboard's `index.html`, and each address and variable it says is reachable (`psql`, the three
   `$LAB_*_URL`s, the edge, the dashboard by both names). For each, record the exact command,
   whether it worked, how long it took, and the error if it did not.
2. **The device simulator.** Run the invocation `ENVIRONMENT.md` gives, into the sample tenant, and
   record what it added: the device and event counts for that tenant before and after. Confirm the
   owner's tenant gained nothing.
3. **The browser.** Run `node query/dashboard/tools/observe.mjs` against one page of each
   dashboard, the owner's and the sample tenant's, and one Search address. Record what each header
   reported.
4. **The tests.** Run every suite `ENVIRONMENT.md` names (`node --test` in each Node package, `go test
   ./...` in each Go module) and then `node tools/accept.mjs`. For each failure, find the cause and
   put it in one of three groups: the test needs something the harness does not have; the test
   collides with the running lab; or the code is wrong. Run a failing suite a second time before
   classifying it, and say if the result changed.

## Do not fix what you find

A command that does not work as written, and a test that fails, are findings. Correct the sentence
in `ENVIRONMENT.md` when the instruction was wrong and you found what works. Do not change a service, a
test, a Dockerfile or a script to make a command pass: write it down, and the owner decides whether
the repository or the instruction changes. If a broken command stops you from running the ones
after it, say which were not run.

## What to write

`backlog/BASELINE.md`:

- The date, the commit, the harness image id and the state of the lab it was measured against.
- One table of the suites and gates that fail before any work: the suite, the failing tests, the
  group from step 4, and the cause in a line.
- The suites that pass, with their counts, so a later task can see a count change.
- Anything that passed on one run and failed on another.

`backlog/ENVIRONMENT.md`: correct what was wrong, including the simulator
invocation if it did not work as written. Keep it to what an agent needs before starting; the measurements belong in
`BASELINE.md`.

This task is also when that file is pruned. Earlier tasks add to it as they learn things. For
each sentence in it, ask whether it is still true and whether an agent starting an unrelated task
needs it. Take out what fails either test, moving it to the header or README of the thing it
describes if it is true but local. The file should not be longer after this task than before it
unless the environment itself gained something.

## Done when

The agent verifies, in the harness:

- Every command in `ENVIRONMENT.md` has a recorded result, and the report lists the ones
  that did not work as written with what was done about each: instruction corrected, or left for
  the owner.
- `backlog/BASELINE.md` exists with the contents above.
- The simulator ran once from the harness and the report gives the working invocation and what it
  added to the lab, or says exactly why it could not run.
- `observe.mjs` opened a dashboard page from the harness.
- `git status` shows changes only under `backlog/`.

The owner verifies:

- The decisions the report leaves open: for each broken command or failing suite, whether the
  repository is fixed or the failure stays in the baseline as known.

## Out of scope

Fixing the baseline failures. The duplicate rows on the Devices page (20 rows for 10 devices), which
the first browser observation showed; note it if you see it, and leave it.
