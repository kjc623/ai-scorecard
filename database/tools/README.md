# `database/tools/` — the schema's checkers and its live-server runner

Nothing here is part of the product. These are the instruments that decide whether `../schema.sql`
still holds the properties the architecture relies on.

| File | What it does |
|---|---|
| `run-invariants.ps1` | Starts a throwaway PostgreSQL, applies `../schema.sql`, runs `../invariants.test.sql`, and reports the assertion tally. Gate 8 of `node tools/accept.mjs` |
| `check-schema.mjs` | Static checks with **no server**: structural properties of the DDL, the reason-code vocabulary across the SQL/Go seam, and the assertion count quoted in prose |
| `check-schema.test.mjs` | Proves the checker can fail — every negative case perturbs a copy of the repository and expects a named check to go red |
| `tally-log.mjs` | Parses a psql run's log into `pass`/`fail`/`distinct_ids`, so the runner judges on the tally rather than on an exit code |
| `tally-log.test.mjs` | The tally's own tests, including the exact log of a healthy run that the old inline counter mis-read |
| `probe-rls-nonsuperuser.sql` | Independently proves row-level security holds for a role that is not a superuser |
| `repro-equal-rank-adopt.sql` | The reproduction for the equal-fidelity dedup defect, kept so the case stays visible |

## Two rules these tools follow

**Judge on the tally, not the exit code.** The runner's exit code once reported failure for a healthy
run because its error counter matched the substring `ERROR` inside `ON_ERROR_STOP` in the echoed
command line. It is fixed, and `tools/accept.mjs` still parses the TALLY line instead — a gate that
trusts an exit code it has seen lie once will lie again.

**A checker must be able to fail.** `check-schema.test.mjs` exists because a checker that only ever
runs against a correct tree cannot show that it checks anything. Each negative case builds a fixture
tree from the real repository, makes exactly one deliberate change, and asserts the specific check
goes red.

## Running them

```
powershell -NoProfile -ExecutionPolicy Bypass -File database/tools/run-invariants.ps1
node database/tools/check-schema.mjs
node --test database/tools/
```

The runner's default host port is `55434`; it leaves that server running for inspection after every
run, pass or fail, and says so. If something else already holds the port it stops with
`docker run failed for image …` rather than silently testing nothing — `localdev/README.md` explains
the port layout and why it is not shared.
