# classifierlink — capture-core's side of the classifier socket

This is the client half of the local socket described in
[docs/01-collectors.md §3.4](../../../docs/01-collectors.md): `capture-core` sends bytes and a mode, and
`classifier-host` sends labels back. One observation or document per request, request/response.

The framing and the handshake shapes come from [`protocol`](../../protocol/README.md), which is the
single source of truth for the wire. This package defines no second format. What it does own is the
failure contract, and that is the reason it is a package rather than thirty lines in the pipeline:

> A version mismatch, a hung host or a crashed host marks the host `degraded` and falls back to
> rules-only with `confidence: degraded`. **"No answer" is never reported as "no labels found"**, and
> a classifier outage never fails the submission (C21).

That distinction is the product's honesty requirement: a submission the device could not classify must
not be recorded as one that was classified and found clean.

## Addressing

`Address` is `unix` (a socket path), `pipe` (a pipe name) or `stdio` (below), and anything else is
refused rather than guessed at. `tcp` is accepted **only** for a loopback address, and only because a platform without
`AF_UNIX`, or a lab running the two components on separate loopback interfaces, has no other
transport; a non-loopback TCP address is refused rather than silently becoming a network listener for
a component that is supposed to be local. `DefaultAddress` supplies the platform's form —
`\\.\pipe\NAME` on Windows, a socket under the service directory elsewhere.

`stdio` is the fourth form, and the only one that is not dialled. Its path is the host executable,
and `Dial` refuses it: the connection is made by `ChildDialer`, which starts the host as a child
process and speaks the same frames on its stdin and stdout. It exists because `classifier-host` has
no named-pipe listener on Windows and serves `stdio` there by default, and because it is the one
transport under which the host's lifetime is the core's: closing the connection closes the child's
stdin, which is its normal end of connection, and then kills it. A child that dies is replaced the
next time `Classify` re-dials. An anonymous pipe has no deadlines, so the connection accepts and
ignores them; `Classify`'s own timer bounds each request, and a timed-out request closes the
connection.

## The answer's shape

`classifier-host` frames a verdict: the `protocol.ClassifyResponse` under `response`, with the
enforcement half (`action`, `rule_id`, `shadowed`) beside it. `protocol` defines the bare response.
The client accepts both and takes only the response. Before it did, every answer from the real host
failed `Validate` for want of a `classifier_version` and was recorded as `version_mismatch`, so a
connected, working host looked exactly like an incompatible one. The enforcement half is read by
nothing on this side yet.

`RulesOnlyVersion` is `rules-only`, the §13.3 rule 6 baseline. It must match
`core.RulesOnlyVersion`, because the value is part of the envelope contract: the pipeline uses the
same constant for the same fallback, and two different spellings would be two different claims about
how an event was classified.

## What the tests pin

`TestLink_3_4_HandshakeAndClassifyRoundTrip` proves the happy path over the real framing;
`TestLink_3_4_VersionMismatchDegradesToRulesOnly` and `TestLink_3_4_HungHostTimesOutToDegraded` prove
the two failure modes above end in a degraded answer rather than an error or an empty one;
`TestLink_11_2_RefusesAModeViolatingRequest` proves the client does not send content at a mode that
forbids reading; `TestLink_Addresses` covers the address rules; `TestLink_CloseIsIdempotent` covers
shutdown.

## What it deliberately does not do

- **No wire format of its own.** Frames, handshake shapes and version bytes come from `protocol`.
- **No process management beyond the one child.** `ChildDialer` starts the host and ends it with
  the connection, and that is all. Restarting with backoff and the crash-loop policy belong to the
  supervisor and the platform service manager, and neither is implemented for the host: a host that
  crashes on every request is re-spawned on every request. `child.go` has no test of its own; it was
  exercised through the installed Windows service.
- **No retry storm.** A failed call degrades the observation and returns; the next observation tries
  again, and nothing here blocks a submission waiting for the host to recover.
- **No labels of its own.** It never synthesises a verdict. When the host cannot answer, the answer is
  the rules-only baseline plus `degraded`, and both are labelled as such.
