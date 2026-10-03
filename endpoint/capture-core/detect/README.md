# detect — `proc.detect`, the process and model detector

This is the provider of [docs/01-collectors.md §4.4](../../../docs/01-collectors.md): it samples the process
table and answers one question — did a local model actually run on this device?

It is the cheapest provider in the product: no ports, no trust configuration, started first and
stopped last so it can report tamper signals about the others (C24). It is also the only provider
whose failure costs nothing user-visible, which is why its failure mode is **failing open by doing
nothing**: it observes passively, and a sampling cycle that goes wrong loses a detection rather than
touching the user's machine.

## R7 is enforced by the shape, not by discipline

§4.4's table has exactly two outputs and one field, and this package has no place to put anything
else:

| Situation | What is emitted |
|---|---|
| evidence a local model ran | one `model_detection`, carrying `detection_basis` and no window |
| a candidate is active with no submission from another route | one `usage_rollup` per device, per tool, per day |
| process identity that belongs to another provider's event | a field on *that* envelope, never a record of its own |

There is no counter and no kind for per-cycle telemetry, so a defect cannot start shipping it. The
distinction that matters is between *installed* and *ran*: a model on disk with no listening socket,
no loaded runtime module and no compute signature is not a model that ran, and the tests pin that
case (`TestDetect_4_4_InstalledButIdleRuntimeIsNotAModelThatRan`).

## How it is configured

Everything policy-shaped lives in the bundle's `ProcDetect` section, so a new runtime signature or a
seed-set change is a bundle change rather than a release. The provider's own `Config` carries seams
and timings: the `Enumerator`, the bundle accessor, the pipeline, the cycle interval, the miss window
and the rollup window.

`Submissions` is the one non-obvious seam: it answers "how many submissions did another provider
record for this tool in this window", and it is the only way a rollup can honestly assert
`submission_count: 0`. Without it the provider records zero and says so in its outcome, rather than
presenting an unasked question as a negative answer.

## Health

- With no enumerator it refuses to start — a route with no way to observe is not started at all.
- A cycle whose enumeration fails degrades to `enumeration_partial` **and emits nothing**, because a
  rollup built on a failed sample would be a fabricated count.
- An empty seed set degrades to `signature_set_stale`: no signatures is not a licence to detect
  everything.
- A cycle that misses its window leaves the provider `absent` rather than stale-healthy.
- After `Stop` it is never healthy, and `Stop` is idempotent.

## What it deliberately does not do

- **No content, ever.** It has no reader and no bytes: mode I is `detection_only` by construction, and
  a prompt that never crosses a socket cannot be reconstructed here.
- **No judgement about who authored a prompt.** Machine-authored traffic is captured like any other;
  the design does not attempt the distinction.
- **No full signature on this host.** The only enumerator wired here is `tasklist` on Windows, which
  gives an image name and a PID — no modules, no listening sockets, no compute signature. It can match
  the candidate rule and emit daily rollups, but it can never produce evidence of use, so it never
  emits a `model_detection`. The provider is off by default; `--proc-detect` turns on that partial
  version and reports it degraded.
- **No ports and no interception.** Starting this provider cannot break anything on the device; that
  is the point of starting it first.
