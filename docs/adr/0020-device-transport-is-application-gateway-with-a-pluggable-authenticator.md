# 0020. Device transport is Application Gateway with a pluggable authenticator: mTLS, DPoP, or a development secret

Status: proposed · Amends [0019](0019-the-origin-validates-the-device-certificate-itself.md): the edge is
Application Gateway, not Front Door, and the origin does not terminate the client TLS handshake itself ·
Related: [0005](0005-every-device-holds-its-own-revocable-credential-bound-to-transport.md),
[0007](0007-tenant-residency-is-pinned-and-fails-closed-at-ingest.md),
[0002](0002-postgresql-is-the-server-store-sqlite-is-only-the-device-spool.md), and
[docs/02-ingest-and-transport.md](../02-ingest-and-transport.md) §2
Date: 2026-10-03

## Context

[docs/02](../02-ingest-and-transport.md) §2.1 and
[ADR 0019](0019-the-origin-validates-the-device-certificate-itself.md) assume **Azure Front Door Premium
mTLS** as the device edge, with the origin terminating TLS itself and re-validating the device
certificate. Three facts make that design unsafe to build on:

1. **Azure's own documentation contradicts itself on the exact topology we use.** The Front Door mTLS
   page (`ms.date 2026-08-11`) describes mTLS as a **preview** feature with OCSP-only revocation, two
   CA certificates and no auto-rotation. The Private Link page (updated 2026-05-26) states flatly that
   *"Azure Front Door doesn't support client/mutual authentication (mTLS) for public origins or private
   link enabled origins."* We run a Private Link origin. Whatever the resolution, the feature is
   preview and its support for our topology is denied on a current page.
2. **Front Door is an L7 proxy and always terminates TLS.** The origin's handshake is with Front Door,
   not the device, so the origin can never perform `RequireAndVerifyClientCert` against the device. ADR
   0019's "Mechanism A" is not achievable behind Front Door; only a forwarded-certificate mechanism is.
3. **Certificate-or-nothing is the wrong product shape.** Requiring an MDM/PKI-issued client
   certificate for every deployment makes Intune (or an equivalent CA) a hard prerequisite — including
   for development, where it is a paid dependency on a machine that only needs to exercise the
   ingestion path.

The properties that must survive any change are ADR 0005's: a **per-device, revocable credential**, and
**sender-constraint** — a stolen credential alone must not be enough to impersonate a device. The
origin must remain the authority for per-device status (docs/02 §2.3), and the device-facing service
code must be production code, not a development-only path.

## Decision

**1. Application Gateway is the public device ingress; Front Door is the analyst ingress.**

Azure Application Gateway `WAF_v2` (or `Standard_v2`), GA mTLS since February 2023, terminates the
device TLS connection and reaches the internal Container Apps environment over the VNet. Front Door
remains in front of the analyst surface (`/analyst/*`) only. The device FQDN resolves to Application
Gateway. The listener runs in **passthrough** mode by default: it requests a client certificate when
one is presented and forwards it, and the origin authenticates. Strict mode (edge validates against an
uploaded CA chain) is an optional hardening for deployments that use certificates only; it is not the
default because a single regional gateway serves tenants with different credential modes.

**2. The origin authenticates through one pluggable seam with three modes.**

`auth.Authenticator` selects on what the request presents, and every production mode preserves
sender-constraint:

| Wire mode | Credential | The origin verifies | Production |
|---|---|---|---|
| `x509` | X.509 client certificate, MDM-issued **or** deployment-issued | Chain against the configured trust bundle, `clientAuth` EKU, validity window; SHA-256 SPKI thumbprint equals `ops.device_credential.public_key_thumbprint`; credential/device status in the write transaction | yes |
| `dpop` | Device-held keypair + short-lived proof-of-possession access token (RFC 9449) | Token signature and `cnf.jkt` binding; the per-request `DPoP` proof signature, `htm`/`htu`, `ath`, `iat` and `jti` replay; then the same status check | yes |
| `dev` | Shared development principal | Nothing cryptographic; **refused unless explicitly acknowledged at startup**, and never enabled by a deployment | no |

The `x509` mode accepts the certificate in either of the two forms a real deployment produces: directly
from the connection (`r.TLS.PeerCertificates`, a compose/lab direct-TLS listener) or forwarded by the
edge as PEM in `X-Client-Cert`, set by an Application Gateway rewrite from the `{var_client_certificate}`
server variable. A forwarded certificate is trusted **only** when the origin is reachable solely
through the edge (private network, source restricted to the gateway subnet), and it is re-validated
against the CA bundle regardless — the edge is a filter, the origin is the authority (ADR 0019's
intent, kept).

**3. Enrolment is mode-agnostic and the private key never leaves the device.**

The device always generates its keypair and never exports the private half. In `x509` mode it submits a
PKCS#10 CSR and receives a leaf issued by a `CertificateSigner` (a local CA in development; Key
Vault / Managed HSM in production). In `dpop` mode it submits its public JWK and a proof of possession
and receives a token-endpoint identity. Re-enrolment stays idempotent on `hardware_identity_hash` and
returns the existing `device_id` (C11); a revoked device still cannot re-enrol into a fresh identity.

**4. The credential table records the mode; the binding is one thumbprint.**

`ops.device_credential` gains `credential_type` (`x509` | `dpop`) and, for DPoP, the registered public
key as a JWK. `public_key_thumbprint` becomes SHA-256 (over the certificate's SPKI for `x509`, over the
RFC 7638 JWK thumbprint for `dpop`) and is the single transport-binding value for both modes.
`ops.device` gains `hardware_identity_hash` with a per-tenant uniqueness constraint, which is the
idempotency key C11 requires and A17 assumes.

**5. The local lab simulates the edge faithfully; the services do not know it is a lab.**

The `$0.00` lab runs a real TLS-terminating proxy that forwards the client certificate in
`X-Client-Cert` — the same interface Application Gateway provides — plus PostgreSQL with the real
schema. The only development-only code in any service is the acknowledged `dev` authenticator, which is
refused in a deployment. No service gains a "if lab then" branch; the code that runs in the lab is the
code that runs in production.

## Alternatives considered

- **Front Door Premium mTLS (preview), origin reads `X-Azure-ClientCertificate`.** Rejected: a preview
  feature, OCSP-only revocation, no CA auto-rotation, and its support for Private Link origins is
  denied by Azure's own documentation. Building device authentication on it makes correctness depend on
  a preview's semantics.
- **External Container Apps ingress with native client-certificate mode.** Rejected: it exposes a
  public PaaS endpoint outside the private environment and duplicates the edge's job, for a narrower
  feature. Kept as a fallback if Application Gateway proves unusable.
- **API Management as the terminator (`validate-client-certificate`).** Rejected for v1 as a heavier
  component than the need requires; a legitimate alternative if policy/quotas are wanted at the edge.
- **Certificates only.** Rejected: it makes Intune or a private CA a hard prerequisite for every
  deployment and every development machine.
- **Bearer tokens only.** Rejected: replayable from any host, which breaks ADR 0005's sender-constraint.
- **SPIFFE/SPIRE workload identity.** Rejected: its attestation model targets orchestrator workloads,
  not internet-facing endpoint agents, and the Workload API is a localhost primitive.

## Consequences

**Easier.** Every component is GA and independently testable; a developer can stand the whole device
path up with no Intune and no PKI, because `dpop` and `dev` need no CA; an MDM certificate is optional
rather than structural; and there is exactly one authenticator seam, so a fourth mode is additive.

**Harder.** Application Gateway is a new regional component to operate and a new fixed cost line in
[05-platform-delivery](../05-platform-delivery.md) §11. A forwarded certificate is only trustworthy
behind an origin lock, so the network restriction is a correctness requirement, not hardening. DPoP
brings a token endpoint, a signing key and a `jti` replay store. A mixed fleet runs both modes on one
gateway, so the edge cannot enforce a single credential type. Three subsystem documents and the Azure
Bicep must be updated to match this record:
[docs/02](../02-ingest-and-transport.md) §2.1, §5.1, §13; [docs/05](../05-platform-delivery.md) §2,
§3.5; `azure/` (a new Application Gateway module; `/v1/*` moves off the Front Door routes).

**We now maintain.** A certificate-signing service behind a `CertificateSigner` interface (local CA in
development, Key Vault in production); a DPoP token endpoint and replay cache; and the origin-lock
configuration that makes a forwarded certificate meaningful.

**Revisit if:** Front Door mTLS leaves preview *and* a real subscription demonstrates it on a Private
Link origin, at which point the terminator could collapse back to one edge; or Application Gateway
passthrough proves too weak an admission filter and strict mode is adopted for certificate-only
tenants.
