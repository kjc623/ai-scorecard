# `docs/risks/` — the two brief risks closed by measurement

The brief listed risks that are validation tasks rather than design tasks: no amount of careful
architecture settles them, only an instrument does. Both of the ones recorded here were answered by
building something and running it, and both answers were less convenient than the design assumed.

| Finding | What was asked | What came back |
|---|---|---|
| [`R1-loopback-inference-capture.md`](R1-loopback-inference-capture.md) | Can the loopback inference broker capture local model traffic on both operating systems? Measurements in [`R1-harness/`](R1-harness/) | Measured against the real broker, with stated limits. The broker's own half behaves as documented; the one defect the run found — a `tampered` health row that outlived a port conflict — is fixed in `endpoint/capture-core/proxy/loopback/machine.go`. The vendor-tool, vendor-client, reboot and upgrade checks remain unvalidated, and the conclusion says so |
| [`Q2-organisational-dimension.md`](Q2-organisational-dimension.md) | Where does the organisational dimension come from, without which three of the ten headline questions cannot be answered? | The tables and the query dimension are built and plumbed, and **nothing writes them** — `control-api` does not exist. An empty answer renders as a data state rather than as a missing component |

## `R1-harness/` is a program, not prose

It is a runnable Go module — the instrument that produced the finding — kept so the measurement can be
repeated rather than believed. `run-output.txt` and `stability.txt` are its recorded runs.

Each finding file states its own verdicts and, more importantly, its own limits: which claims were
measured, which were reasoned, and which remain unvalidated. Read the "not validated" sections before
quoting either one. R1 in particular gates a build step only, so it can be unresolved without
blocking the rest.
