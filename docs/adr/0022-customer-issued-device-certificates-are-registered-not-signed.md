# 0022. Customer-issued device certificates are registered, not signed

Status: proposed · Amends [0020](0020-device-transport-is-application-gateway-with-a-pluggable-authenticator.md)
decision 3 (control-api is the issuer) by adding a second issuance model · Adds `ops.tenant.device_ca_pem`,
`ops.device_credential.credential_origin`, a live-thumbprint unique index, and a pre-tenant credential
resolver · Related: [0005](0005-every-device-holds-its-own-revocable-credential-bound-to-transport.md),
[0019](0019-the-origin-validates-the-device-certificate-itself.md),
[0021](0021-device-identity-is-clear-by-default.md)
Date: 2026-10-05

## Context

ADR 0020 decision 3 makes the product the certificate authority: an `x509` device submits a PKCS#10
CSR and `control-api` signs a leaf from a `CertificateSigner` (a local CA in development, Key Vault /
Managed HSM in production). `docs/02` §2.2 fixes the leaf's identity in the certificate itself —
subject `CN` is the product `device_id`, an `OU` is the tenant. The authenticator depends on that:
`identityFromCertificate` reads the device and tenant straight out of the subject.

Real customers already run a PKI. The obvious one for this product's market is **Intune Cloud PKI**,
which issues device certificates from Microsoft's template under the customer's Intune. A deployment
that wants to use it — a customer test, or a customer whose security team requires the product to
consume its existing device certificates — hits three facts at once:

1. **The product cannot sign with it.** Intune Cloud PKI exposes no signing API and no CA private key
   to a third party; only the MDM can request a certificate, for a device it manages.
2. **A customer certificate cannot carry the product's identity.** `device_id` is minted *at
   enrolment*, after the certificate already exists, and the product cannot dictate an `OU` into
   Microsoft's subject template. The subject-CN/OU identity model is therefore unavailable for any
   certificate the product did not issue.
3. **There is no path for one.** The wire contract carries a `csr` or a `jwk`; the enrolment service
   always calls `signer.Sign`; no table stores a customer trust anchor; and `ops.device_credential`
   is keyed `(tenant_id, credential_id)` with no way to resolve a credential before the tenant is
   known.

So "use Intune Cloud PKI" is not a configuration of the existing design; it is a second issuance
model, and the product's role in it changes from **issuer** to **verifier**.

## Decision

**1. A tenant's device certificate is issued by the product or by the customer, and which one is
recorded.** `ops.device_credential.credential_origin` is the closed pair `product` | `customer`
(default `product`, so every existing row is unchanged). `credential_type` still says `x509` or
`dpop`; origin says who issued the x509 certificate. The issuance path is an explicit property of the
credential, never inferred per request: a request that presents a CSR for a `customer` tenant, or a
certificate for a `product` tenant, is refused.

**2. The customer's trust anchor lives on the tenant.** `ops.tenant.device_ca_pem` is the PEM bundle
a tenant's device certificates must chain to. `NULL` means the tenant is `product`-issued and the
origin verifies against the product CA bundle it already holds; non-`NULL` means the customer issues
device certificates and the origin verifies against this bundle. It is public trust material (not
sealed), per tenant because each customer's PKI has its own issuing CA, and set by an operator from
the customer's MDM (for Cloud PKI: the issuing CA certificate downloaded from the Intune admin
center).

**3. Enrolment registers a certificate it is shown.** For a `customer` tenant, an `x509` enrolment
carries no CSR. The device's certificate arrives on the transport and is forwarded by Application
Gateway as `X-Client-Cert` — the same header the authenticator already reads. `control-api` resolves
the tenant from the deployment key (as today), verifies the forwarded chain against that tenant's
`device_ca_pem` (`clientAuth` EKU, validity window), binds the device (hardware identity plus the
Intune attestation as today), and registers the credential with
`credential_id = protocol.CredentialID(leaf.Raw)` and `public_key_thumbprint = SPKI(leaf)`. The
signer is never invoked. The private key remains on the device throughout; the product never sees it.

**4. Authentication resolves the credential by its transport binding, then verifies the chain per
tenant.** The origin computes the certificate's SPKI thumbprint, resolves the live credential through
`ops.device_credential_for_thumbprint(text)` — a pre-tenant `SECURITY DEFINER` function in the
existing section-5c pattern, owned by `sac_resolver` — which returns `(tenant_id, device_id,
credential_id, credential_type, expires_at, device_ca_pem)`, then verifies the chain against the
returned anchor and runs the existing binding/status/expiry checks. The subject no longer has to
carry product UUIDs; a certificate that was never registered at enrolment resolves to nothing. The
resolver verifies against the tenant the credential belongs to, so one customer's certificate never
validates against another's CA.

**5. A live public key is globally unique.** A partial unique index on
`ops.device_credential (public_key_thumbprint) WHERE revoked_at IS NULL` makes the thumbprint a key of
a *live* credential across all tenants, so the resolver answers with at most one row. It is safe
because a key belongs to one device, and it preserves history and rotation: `IssueCredential` revokes
the previous row before inserting the new one in one transaction, so a re-registration with the same
key never trips it.

## Alternatives considered

- **The product signs every certificate; ignore customer PKI.** Rejected: it makes the vendor CA a
  hard prerequisite for exactly the customers (large, Intune-managed) best able to issue their own,
  which is the coupling ADR 0020 decision 3 was written to keep optional.
- **Put the product identity into the customer's certificate template.** Rejected: `device_id` is
  minted at or after enrolment, so no certificate issued before then can carry it. Using the
  Entra/Intune device id as the subject and mapping it is possible but still needs the same
  resolver, with less generality and a new cross-system identifier on the wire.
- **A union trust bundle at the edge instead of a per-tenant anchor.** Rejected as the primary
  model: it loses per-tenant trust isolation (one customer's CA would verify another's certificate)
  and does not say which tenant a certificate belongs to. A deployment-level bundle remains, but
  only for `product`-issued certificates.
- **A new wire mode (`byoc`).** Rejected: the certificate is still x509 and the transport is
  unchanged; a fourth mode would multiply the pluggable seam and the schema's closed sets for no new
  authentication property. `credential_origin` records the difference in the data instead.

## Consequences

**Easier.** A customer with Intune, ADCS or Jamf can enrol without the product holding any CA key,
which is where its security interest actually is. The product is a verifier, and the durable binding
is one SHA-256 over a public key it already stores. The test uses the real thing.

**Harder.** Two issuance models to keep straight, and the invariant that they must not mix per
request. A per-tenant trust-anchor lifecycle: set at onboarding, rotated when the customer rotates
its issuing CA, with the overlap the customer's rotation provides. A second cross-tenant lookup whose
grader must be as tight as the identity resolvers' — revoked from `PUBLIC`, granted only to
`sac_control` and `sac_ingest`. The agent, on a `customer` tenant, must load a certificate and key the
OS already holds (Intune provisions it into the machine store) rather than generating a CSR. And the
origin no longer learns the tenant from the certificate; it learns it from the credential row, so an
unregistered certificate — however valid its chain — authenticates nothing.

**Not built by this record.** This ADR is the design and the schema foundation. The enrolment path,
the store methods, the ingest authenticator's thumbprint resolution, the agent's certificate loading,
and the operator step that sets `device_ca_pem` are follow-on work; until they land, a `customer`
tenant cannot enrol and the existing subject-CN/OU product path is unaffected.

**We now maintain.** The rule that `credential_origin` and `device_ca_pem` agree — a `customer`
credential only exists for a tenant with an anchor, and a `product` credential for a tenant without
one — as a validation the enrolment service enforces, not just a convention.

**Revisit if:** Microsoft exposes a per-tenant signing API the product could call, or a customer wants
the product to hold the issuing CA; both are already served by the `product` path and would not change
this record.
