# core — the agent's spine

This package holds the parts of `capture-core` that must exist before any provider does: the provider
contract and the coverage row each provider owns, the mode resolution and the content gate, the
envelope builder, and §3.5's startup and shutdown order as data rather than as the order of
statements in a function.

Two properties are structural here rather than documentary, and they are the reason to read this
package first:

- **A provider cannot report success it did not observe.** Health is assembled from a positive
  observation, and the registry overrides any row that contradicts its own bookkeeping — a provider
  that failed to start cannot surface as healthy, and a stopped one cannot either.
- **The M0 gate is not a check someone has to remember.** Content is reachable only through a
  `ContentReader` the pipeline consults *after* the mode is resolved, and the gate refuses to read
  when the mode forbids it. A provider that already holds bytes has already violated the ordering,
  which is why the interface hands over a reader rather than a `[]byte`.

## Files

| File | What it owns |
|---|---|
| `pipeline.go` | `Observation`, the mode-first flow, classifier invocation, envelope minting, the spool write, and the outcome reasons a coverage report groups by. |
| `mode.go` | §11.1's most-restrictive resolution across the tool, population, device, class-ceiling and tenant-default axes, with the contributions kept so an operator can see *which* entry over-restricted a tool. |
| `envelope.go` | Envelope minting: identity from enrolment (never from a provider), contract-shaped attachment descriptors, and the per-kind decision about every field. |
| `registry.go` | The provider set: concurrent start, one failure degrades one row and nothing else, invented states and counters sanitised away. |
| `health.go` | The per-provider health row, the closed detail vocabulary, and counters as cumulative-since-start **plus** a windowed delta. |
| `ordering.go` | §3.5's two columns as literal step names, the `Supervisor` that records every step it performs, and the platform seams (`SystemProxy`, `TrustRoot`, `Releaser`). |

## Mode resolution

The effective mode is the most restrictive applicable value, and **mode is never empty**: an
unresolvable scope entry resolves *downward* to M0 and says so in its reasons. With no bundle the
device is M0; an unacknowledged notice gate lowers to M0; a tampered bundle never widens a mode.
Data class is deliberately absent from the query — it is a *result* of classification, and the mode
has to be chosen before content is read, which is what the class-prior map exists for.

## Pipeline behaviour at the edges

- A non-nil content reader at M0 is a defect and is caught: the gate refuses to call it rather than
  silently reading.
- A classifier that is unavailable, hung or version-mismatched still emits — with `confidence:
  degraded` and `rules-only` labels. "No answer" is never reported as "no labels found".
- An extraction failure degrades to the Tier-S surrogate key rather than dropping the observation,
  because a route that cannot identify the authored boundary must not guess.
- A body over the cap is recorded with its size and a digest of the prefix, classified not at all,
  and emitted degraded — never as a confident clean result.
- With no canonicaliser installed the pipeline takes the degraded path instead of presenting an
  unnormalised digest as the versioned `sac-canon-1` value.
- A refused spool write is counted and carries the request, because a provider with nowhere to write
  must not drop silently.
- At M3 the content goes to the `ContentStore` and never into the envelope. What is handed over is
  the prompt text where the route's extractor identified the user-authored segment, and the body as
  observed where it did not. With no store configured the observation is refused, because an M3
  record whose content does not exist is a false claim; a store that fails to write is a degraded
  outcome (`content_store_unwritable`) and the event still goes.

## Ordering

`StartupOrder()` and `ShutdownOrder()` are §3.5's columns transcribed. Shutdown has one forced
interpretation: the broker's port release happens first of all — it is the one failure that breaks
the user rather than losing data — the proxy stops enforcing immediately after, and `proc.detect`,
which starts first, stops last so it can report tamper signals about the others. The registry never
restarts a provider: crash-loop policy is a supervisor decision, and keeping it out of the registry
is what makes "which provider is running" one question with one answer.

## Tests

The suite is organised around the claims above, not around coverage: `TestPipelineM0NeverCallsTheContentReader`,
`TestBuildEnvelopeRefusesContentDerivedFieldsAtM0`, `TestPipelineWithoutCanonicaliserIsDegradedAndNeverClaimsTierT`,
`TestRegistryNeverReportsHealthyAfterFailedStart`, `TestRegistrySanitisesInventedCounters`,
`TestTamperedBundleNeverWidensAMode`, `TestResolveUnresolvableScopeEntryResolvesDownward`, the
`TestOrdering_3_5_*` sequence tests, and `TestEnvelope_ADR0018_EveryKindDecidesEveryField`.

## What it does not do

- **No policy.** It resolves what the bundle says; it never decides what the bundle should say.
- **No persistence or transport.** The spool is behind a narrow `Sink` interface that
  `protocol.Store` satisfies, the M3 content store is behind `ContentStore` (one method, `Put`),
  and nothing here dials, listens or writes a file.
- **No platform code.** The system proxy, trust root and process enumeration are interfaces; the
  implementations, and their absence in this build, are the providers' problem.
- **No restart, no backoff, no service management.** Those belong to the supervisor and the platform
  service manager respectively.
