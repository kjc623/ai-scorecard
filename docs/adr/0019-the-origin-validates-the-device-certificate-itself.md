# 0019. The origin validates the device certificate itself; the edge is a filter

Status: proposed · Amends nothing; makes explicit what [02-ingest-and-transport.md §2.1](../02-ingest-and-transport.md)
already requires and what the deployment does not yet implement ·
Related: [0005](0005-every-device-holds-its-own-revocable-credential-bound-to-transport.md),
[0007](0007-tenant-residency-is-pinned-and-fails-closed-at-ingest.md)
Date: 2026-10-02

## Context

`docs/02-ingest-and-transport.md` §2.1 states the requirement in three sentences that the code and the
infrastructure have not caught up with:

> Front Door Premium mTLS is the edge control … **the edge is treated as a *filter*, never as the
> authority**: the origin re-validates the certificate chain and, decisively, the **per-device
> credential status** on every request (§2.3).
>
> **Origin reachability.** The origin must be reachable only through the edge … The platform documents
> that mTLS can be bypassed by calling the origin directly, so this is a **correctness requirement of
> the authentication design**, not a hardening nicety.

Three facts about the build make this a live decision rather than a restatement:

1. **The binary does the right thing already.** `ingestion/ingest-api` serves TLS with
   `RequireAndVerifyClientCert`, validates the chain against `--tls-client-ca`, and re-checks the
   per-device credential status *inside the write transaction* (§2.3). With no TLS material and no dev
   flag it **refuses to start** rather than serving unauthenticated — observed directly:
   `refusing to serve without device authentication: supply -tls-cert, -tls-key and -tls-client-ca, or
   -dev-trust-principal for a local test`.
2. **The deployment cannot supply that material.** Every app in `azure/main.bicep` passes
   `keyVaultEnv: []`, and `azure/modules/container-app.bicep` probes `scheme: 'HTTP'` on the serving
   port. So a deployed container is reachable and probeable and **cannot authenticate a device**.
3. **Two mechanisms could satisfy §2.1, and they are not equivalent.**

| Mechanism | How the origin gets the device certificate | What it depends on |
|---|---|---|
| **A. End-to-end TLS** | The origin terminates TLS itself; Front Door passes the connection through | The origin holding a certificate for the public name, and Front Door not terminating that layer. Nothing preview. |
| **B. Edge forwards the certificate** | Front Door validates at the edge and forwards the client certificate to the origin (a header or a platform feature) | A Front Door capability that **the repository cannot verify**: there is no subscription, no `az` CLI and no network on the machine this was written on. |

## Decision

**Mechanism A, end-to-end TLS, is the target.** The origin terminates TLS, presents a certificate for
the public name, requires and verifies the client certificate against the device CA, and re-checks the
per-device credential status in the write transaction — all of which it already does.

**Mechanism B is not adopted, and must not be adopted on the strength of a document.** Two reasons, and
the second is the one that matters:

- It would make the edge the authority rather than a filter, which §2.1 explicitly refuses. Front Door's
  mTLS is *in preview*, its revocation check is OCSP-only, and it validates against up to two CA
  certificates with no auto-rotation — so treating it as authoritative imports a preview feature's
  semantics into the correctness of device authentication.
- **The forwarding behaviour is unverified here.** Whether the edge passes a client certificate to a
  Private Link origin, in what form, and whether it is trustworthy against a direct caller, are claims
  this repository cannot test. Adopting B would mean writing a header-parsing trust path whose
  security rests on a platform behaviour nobody has observed.

**Consequently, `keyVaultEnv` must be populated for the app that terminates TLS before its probe moves
to `HTTPS`.** Ingestor's `probeScheme` parameter exists and defaults to `HTTP`, which is the correct
default: an HTTPS probe against a still-plaintext listener is strictly worse than an HTTP probe against
a plaintext one. The two changes are one change.

**And the origin-reachability requirement follows from the decision, not from hardening taste.** If the
origin terminates TLS itself, then anything that can reach the origin directly can complete a TLS
handshake — so the guarantee that only the edge reaches it is what makes the edge's filtering
meaningful at all. §2.1 already says this; this record is the reason it cannot be quietly relaxed for
convenience in a lab.

## Alternatives considered

- **Mechanism B, edge-forwarded certificate, with the origin parsing a header.** Rejected above: it
  transfers authority to a preview feature and rests on unverified platform behaviour. Note it is not
  rejected *forever* — if a future workstream can demonstrate the forwarding on a real subscription, and
  can demonstrate that a direct caller cannot forge the header (which requires the origin be
  unreachable except through the edge), it becomes a legitimate option and this record should be
  superseded rather than ignored.
- **mTLS at the edge only, origin plaintext.** The simplest deployment and the one the current
  infrastructure accidentally implies. Rejected outright: it is precisely the state §2.1 forbids, and it
  would mean the per-device credential re-check inside the write transaction is the only device
  authentication — which is necessary but not sufficient, because it authenticates a *claimed* identity
  rather than a proven one.
- **A separate mTLS-terminating ingress in front of the container app.** Rejected for v1 as a new
  component to operate, when the binary already terminates TLS correctly.

## Consequences

**Easier.** The deployment becomes a configuration task rather than a code task: the binary already
refuses to serve unauthenticated, so the failure mode of getting this wrong is a container that will not
start, not a container that serves everyone.

**Harder, and this is the real cost.** Front Door must be configured so it does not terminate the client
TLS layer the origin depends on, and the origin certificate needs a lifecycle — issued for the
per-region FQDN, rotated, and reachable by the platform's probe. Certificate lifecycle is now something
the deployment owns rather than something the edge absorbed, and the probes must become `HTTPS` with
whatever trust the platform requires for a private CA. **That last point is explicitly NOT VERIFIED**:
whether Container Apps accepts a privately-issued server certificate on a probe has not been tested, and
it is the first thing to check on a real subscription.

**We now maintain.** The requirement that the origin is unreachable except through the edge, as a
deployment property rather than a note. It is already asserted in `azure/` for the vault and the
database; device authentication now depends on it too.

**Revisit if:** a real subscription shows that end-to-end TLS through Front Door is not achievable with
Private Link origins. Then mechanism B returns, with the workstream above as its precondition — and this
record is superseded rather than quietly dropped.
