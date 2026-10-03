# 0005. Every device holds its own revocable credential, bound to its transport

Status: proposed
Date: 2026-10-02

Supersedes: ADR 0005 of the pre-brief package (same decision, re-derived against the Shadow AI Capture
brief and its 70–85% management-coverage reality).

## Context

Brief §4.2 requires: enrolment is one-shot and mutually authenticated; re-enrolment after a re-image is
idempotent and returns the existing identity rather than creating a duplicate; device identity is
revocable per device; a revoked device is rejected and marked accordingly.

The estate is 70–85% managed (brief §5.5), so some devices are not reachable by MDM at all. A design
that assumes it can always push a removal has an unpatched hole in exactly the population it knows least
about.

## Decision

Every device completes a one-shot mutually authenticated enrolment and receives its **own** credential
and identity. Enrolment is idempotent on a hardware-derived device key: re-imaging a machine returns the
existing `device_id` and rotates the credential rather than minting a second device.

**As built.** The schema does not yet carry that key: `ops.device` has no hardware-identity column and
no uniqueness constraint beyond `(tenant_id, device_id)`, and the enrolment service is not implemented,
so the idempotency above is a requirement nothing enforces today.

Credentials are per-device, time-bounded (90 days, rotated at 60, seven-day overlap) and bound to a
client certificate on the transport, so a stolen token alone is not sufficient to impersonate a device.

Revocation is an API operation with an actor and a reason (`ops.device.revoked_by`, `revoked_reason`). A
revoked device's in-flight batch is rejected, its spool is retained rather than discarded, and its
`collector_state` becomes visible in the device inventory with the actor who revoked it.

Validation happens per request at the origin, not only at the edge: Azure Front Door's mTLS support has
an OCSP-only revocation story with no automatic CA rotation, so the edge is treated as a filter and the
origin as the authority.

## Alternatives considered

- **A shared per-tenant credential.** Rejected: one leaked secret compromises every device of that
  tenant, and revocation becomes an all-or-nothing operation no customer would accept.
- **MDM identity alone, with no product-owned credential.** Rejected because brief §5.5 puts 15–30% of
  devices outside management, and because MDM enrolment state is not the same fact as "this collector is
  authorised to send me data". It is an input to enrolment, not the authorisation.
- **Long-lived credentials with no rotation.** Rejected: a credential that outlives the laptop's
  re-imaging is the attack path this ADR exists to close.

## Consequences

Easier: a compromised device costs one credential and one device row. Revocation is a first-class,
auditable action. Device-scoped rate limiting and coverage reporting both have a stable key.

Harder: the credential lifecycle is real operational work — issuance, rotation with overlap, expiry
monitoring, and a support path for re-imaged machines. Access tokens must carry the device binding, or
the mTLS requirement becomes decorative.

We now maintain: an enrolment service, a rotation schedule, and a documented procedure for a device whose
certificate was issued to the wrong trust store, which brief E7 warns fails silently.

Revisit if: a platform vendor offers hardware-attested device identity the whole estate already has, at
which point the enrolment ceremony could be replaced rather than supplemented.
