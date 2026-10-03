# proxy — the two interception providers

The two providers here are grouped because they are the pair whose failure modes are inverted, and
reading either one without the other invites the wrong reflex.

| Provider | Route | Modes | If it fails | Blast radius |
|---|---|---|---|---|
| `tlsproxy` | `proxy.tls` | D, E (GUI), G, H | it must **fail open**: carry the request | largest in the product — it can break egress while in the path |
| `loopback` | `proxy.loopback` | F | it must **refuse to serve**: release the port | inverted (E14) — holding the port without a working upstream breaks the user's local model server |

Both resolve the effective mode through the pipeline *before* buffering a body, so at M0 the body is
forwarded without ever being retained. Both treat an over-cap body the same way §5.3 requires: forward
it, record its size and a digest of the prefix, classify nothing, and emit with `confidence: degraded`
rather than a confident clean result.

## `tlsproxy` — the egress interceptor

It observes TLS-terminated request bodies on an **enumerated** set of destinations and connection
metadata for everything else. It is the only provider that reads content outside the browser, and
therefore the only one that can reach modes D, G and H at all — which is also why it is the one with
the largest blast radius.

- **Fail open is the acceptance criterion, not an aspiration.** The user's traffic is never blocked,
  degraded or delayed by this provider's inability to do its job. Every branch prefers "carry the
  request" over "report an error", and the only thing that stops interception is signed policy.
- **Interception is scope, not discovery.** A destination not in the bundle's sets is blind-tunnelled.
- **No bundle means nothing is decrypted.** With no bundle in force the provider stops reading
  content — it never widens.
- **The CA is per device**, never per tenant and never per fleet: a stolen key covers only that
  device's minted leaves, leaves are short-lived, and the private key never leaves the process in
  anything but sealed form. Nothing in the package writes it anywhere.
- **Health requires an end-to-end probe.** The canary handshake through a minted leaf is what makes
  the row healthy; a successful file write is not evidence that interception works, so without a
  canary the provider reports `tls_probe_failed` instead of claiming health.
- **Pinned clients are excluded on a ladder that is re-probed**, never a permanent silent omission.
- **The kill switch stops enforcement before the system proxy is restored**, so a device is never left
  pointing at a proxy that has stopped serving.

Its tests are named for the clauses they encode: `TestTLS_5_3_InterceptsEligibleDestination`,
`TestTLS_5_1_BlindTunnelsUnlistedDestination`, `TestTLS_13_3_NoBundleMeansNothingIsDecrypted`,
`TestTLS_5_4_FailOpenTable`, `TestTLS_5_3_OverCapBodyIsForwardedButNotHeld`,
`TestTLS_5_5_KillSwitchStopsEnforcementBeforeRestoringTheProxy`,
`TestTLS_5_5_PinnedClientExclusionLadder`, `TestTLS_5_6_HealthNeverHealthyWithoutTheProbe`,
`TestTLS_5_2_CAIsPerDeviceAndOnlyTheSealedFormLeaves`.

## `loopback` — the local inference broker

It holds the port a local inference server would otherwise bind, forwards to the relocated upstream,
and observes the plaintext bodies that pass through it. This is the one surface appliances cannot see,
and the design's most fragile assumption (R1): if a tool cannot be moved off its default port, mode F
degrades to `detection_only`.

§6.2's shape follows from the inverted failure mode. A state machine (`RELEASED`, `BINDING`,
`HOLDING`, `ORPHAN`) is the **single writer** of the bind/release decision; the watchdog, the preflight
and the shutdown path produce events, and only the machine decides. The machine never binds, closes or
sleeps: it returns an action and one runtime goroutine per port performs it. Binding happens only
after a preflight against the upstream path the tool documents as safe, so the port is never held
without a serving upstream, and repeated failures cool down rather than fighting for the port. A port
held by another process is reported as `tampered` and never contested.

Its tests: `TestBroker_6_2_NeverBoundWithoutServingUpstream`,
`TestBroker_6_2_BindsAfterPreflightAndBrokersRequests`,
`TestBroker_11_2_M0ForwardsWithoutRetainingTheBody`,
`TestBroker_6_4_UpstreamCrashReleasesAndRecoveryRebinds`,
`TestBroker_6_2_PortHeldByOtherIsTamperedAndNeverFoughtFor`,
`TestBroker_3_5_ReleaseIsSeparateFromStop`, `TestMachine_6_2_TransitionTable`.

## What is deliberately not here

- **No platform facilities.** `core.SystemProxy`, `core.TrustRoot` and the CA `Sealer` are interfaces;
  this build wires none of them, so `proxy.tls` reports `degraded detail=tls_probe_failed` rather than
  health, and the interceptor itself is exercised with in-process root pools.
- **No `cli.shim`.** Modes E (CLI) and G are unrouted, so nothing sets the proxy environment per
  runtime and nothing injects an added CA bundle path.
- **No document parsing.** Content that needs parsing belongs to `classifier-host`'s isolated child,
  never to a provider holding a live connection.
- **No content retention.** Neither provider writes content anywhere; the envelope carries a digest,
  and M3's local store is a separate component.
