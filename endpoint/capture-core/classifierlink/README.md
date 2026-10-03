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

`Address` is `unix` (a socket path) or `pipe` (a pipe name), and anything else is refused rather than
guessed at. `tcp` is accepted **only** for a loopback address, and only because a platform without
`AF_UNIX`, or a lab running the two components on separate loopback interfaces, has no other
transport; a non-loopback TCP address is refused rather than silently becoming a network listener for
a component that is supposed to be local. `DefaultAddress` supplies the platform's form —
`\\.\pipe\NAME` on Windows, a socket under the service directory elsewhere.

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
- **No process management.** Spawning, restarting with backoff, kill-on-timeout and the crash-loop
  policy belong to the supervisor and the platform service manager, not to the client.
- **No retry storm.** A failed call degrades the observation and returns; the next observation tries
  again, and nothing here blocks a submission waiting for the host to recover.
- **No labels of its own.** It never synthesises a verdict. When the host cannot answer, the answer is
  the rules-only baseline plus `degraded`, and both are labelled as such.
