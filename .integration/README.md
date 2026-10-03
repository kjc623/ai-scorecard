# `.integration/` — the verification trail

Where the claims made about this repository are checked by someone other than the component's author,
and where the disagreements are kept.

| File | Author | What it is |
|---|---|---|
| [`LEAD-VERIFICATION.md`](LEAD-VERIFICATION.md) | The Lead | Checks the Lead reproduced **personally**, with the exact command, what it returned, and what it does and does not prove |
| [`REPORT.md`](REPORT.md) | An independent verifier | That verifier's report on the components they did *not* author |

The split is deliberate. A verification log written by the same person who wrote the code is a
statement of what that person believed; keeping the two documents separate is what makes it possible
to see where they disagree. Where they do, the disagreement is itself a finding — and there is at
least one recorded.

Both are worth reading before trusting any summary, including the one in the root README. They record
the checks that **failed** as well as the ones that passed, and several of the defects found in this
project were found by exactly this arrangement: an author's suite passing while an independent check
of the same property went red.

## `repro/` — the reproduction cases

Small, self-contained programs and fixtures that make a specific defect reproducible from the
document alone. Each exists because a finding that cannot be re-run is an anecdote:

| Path | Reproduces |
|---|---|
| `repro/goconsume/` | A real Go consumer decoding an extension frame — the native-content encoding seam |
| `repro/inv4/`, `repro/inv6/` | The INV-4 (append-only spool) and INV-6 (honest coverage) properties, from outside the components that implement them |
| `repro/native-content-seam.mjs` | The framing and encoding mismatch between the extension and a Go reader |
| `repro/inv3b-probe.mjs` | The query-DSL interpolation probe behind INV-3b |
| `repro/frame-*.json` | The three frame shapes the seam is tested against |

Each has its own `go.mod` with `replace` directives pointing into `endpoint/`. They are not part of
any package's suite and `tools/verify-all.mjs` does not collect them: they are kept runnable for a
human who wants to check a claim.

## `browser-probe/`

Raw output from the browser load probe — evidence that a specific Chromium build does or does not load
the unpacked extension, kept as captured text rather than summarised.

## The generated files

Running `node tools/accept.mjs` or `node tools/verify-all.mjs` writes a timestamped JSON record here
for every run. Those are **gitignored**: a run produces evidence, and committing it would add dozens
of files per acceptance run to a repository whose evidence is the prose above. The tracked files are
the ones a person wrote.
