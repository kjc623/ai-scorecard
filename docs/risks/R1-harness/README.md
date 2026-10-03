# `docs/risks/R1-harness/` — the instrument behind the R1 finding

A runnable Go module that measures whether the loopback inference broker sees what the design assumes
it sees. It is kept as a program rather than as a table of results so the measurement can be repeated
instead of believed.

```
cd docs/risks/R1-harness
go run .               # every claim;  go run . a|b|c|d|e for one
```

The directory holds no `_test.go` files, so `go run .` is what exercises it.

| File | What it is |
|---|---|
| `main.go` | The harness: starts the broker, drives traffic through it, reports each claim's verdict |
| `rig.go` | The test rig the claims are built on |
| `run-output.txt` | A recorded run |
| `stability.txt` | Repeated runs, to show the result is stable rather than lucky |
| `go.mod` | Its own module. It reaches `endpoint/` by `replace` directives, which is why those point outside this directory |

## Read the limits before quoting it

The finding it produced is [`../R1-loopback-inference-capture.md`](../R1-loopback-inference-capture.md),
and its "not validated" section is the part to read first. The harness measures the broker; it cannot
measure a real vendor tool being relocated, a real client resolving a substitute port, or behaviour
across a reboot. Those remain unvalidated, and R1 gates a build step only, so the finding being
partial does not block the rest of the system.
