# policy — the signed bundle, and the rule that failure never widens

This package owns the shape of the signed policy bundle, its verification chain
([docs/01-collectors.md §13.2](../../../docs/01-collectors.md)), and §13.3's rule that a bundle which fails
verification never changes what the device is enforcing.

The one sentence it exists to enforce:

> **No code path lets the absence of a valid bundle widen what the device may do.**

A rejected bundle leaves the previous one in force. With no previous bundle the device runs at M0 —
metadata only. There is no third branch, and no "skip verification" flag anywhere in the package.

## The bundle is data, not code

| Field group | What it decides |
|---|---|
| `Interception` | Which destinations we are willing to **decrypt** — tenant hosts, seed hosts, TLS ports, the body cap, A5's promotion window. A destination in none of these sets is blind-tunnelled. |
| `LoopbackPort` rows | §6's port map: which tool, which port the broker holds, where the relocated upstream listens, the read-only preflight path, and the mode. Ports are policy data, never a compiled-in list. `OriginalPort` records what the tool had before relocation so uninstall restores it exactly. |
| Scope matrix | The per-tool, per-population, per-device modes and class ceilings that `core` resolves to one effective mode. |
| `KillSwitch` | §5.5's switch. `disable` is the only mode defined, and it exists so a fleet regression can stop enforcement without a software release. `ReasonCode` rides the next health report so an operator can attribute a coverage cliff in one step. |
| `ProcDetect` | The detector's runtime signatures and seed set. |

Interception is a **scope**, not a discovery mechanism: it says what the device may decrypt, never
what counts as generative. A hostname allowlist that decided "this is an AI tool" would be exactly
the brand-list discovery C7 forbids.

## Verification

The chain checks four causes in order, all in the closed set §13.3 defines:
`bundle_signature_invalid`, `bundle_schema_invalid`, `bundle_version_regression`,
`bundle_artefact_missing`. Each maps to a name in `protocol.Detail`'s closed vocabulary — a cause
string invented locally would be a coverage cause the reporting layer could not group.

`Store` is the only writer of the bundle in force, so "which policy is the device enforcing" has
exactly one answer at any instant. `InForce()` returning nil is what every caller must read as M0,
never as unrestricted. A successful poll with an unchanged version is idempotent. Repeated failures
escalate rather than becoming a request storm: severity moves through `info`, `warning` and
`critical`, reaching the last at three consecutive failures (`escalationThreshold`), the cause is retained, and the health row carries it.

`NewStore` refuses to exist without a verifier, and `NewVerifier` needs a pinned key: an unverified
bundle must never be enforceable, and that is a programming error rather than a runtime condition.

## Tests

`TestVerify_SignatureFailuresAreNamed`, `TestVerify_SchemaFailures`, `TestVerify_VersionRegressionIsRefused`
and `TestVerify_ArtefactDigestsMustResolve` cover the four causes; `TestStore_RetainsPreviousOnFailure`
and `TestStore_NoPreviousBundleMeansM0` cover the two permitted outcomes of a bad poll;
`TestStore_EscalatesAndBacksOff` covers the escalation; `TestCause_DetailUsesTheClosedVocabulary`
pins the mapping; `TestBundle_InterceptsIsAScopeNotADiscoveryMechanism` pins the distinction above;
`TestNewStore_RefusesToExistWithoutAVerifier` pins the refusal.

## What it deliberately does not do

- **No signing and no key custody.** Bundles are signed elsewhere; this package verifies against a
  pinned Ed25519 public key it is given.
- **No polling loop.** Applying a bundle is a call; the schedule, the fetch and the retry live in the
  binary and the health channel.
- **No partial acceptance.** A bundle is accepted whole or not at all, and validation rejects what the
  device could not enforce — an unenforceable entry is refused at verification rather than ignored at
  use.
- **No widening path.** There is no flag, environment variable or code branch that turns verification
  down; the absence of a bundle is M0, and M0 reads nothing.
