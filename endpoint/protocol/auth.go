package protocol

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"
)

// The device authentication and enrolment vocabulary: what the agent sends to authenticate
// itself and to obtain its per-device credential (ADR 0020; docs/02-ingest-and-transport.md
// §2, §5.1).
//
// ADR 0020 makes device transport "Application Gateway with a pluggable authenticator": the edge
// terminates TLS and forwards the leaf certificate in X-Client-Cert, and the origin selects
// x509, dpop or the development-only dev mode from what the request presents. Every device
// component consumes this package and none redefines its shapes (contracts/README.md), so the
// two production modes and the enrolment exchange are defined here once.
//
// Two properties this file exists to keep honest:
//
//   - AuthMode and the token type are closed vocabularies. A mode the origin does not implement
//     is refused by AuthMode.Valid rather than defaulted to x509, because a default would turn
//     "this credential type is unknown" into "authenticate by certificate", the one failure a
//     pluggable authenticator must not have.
//   - The private key never leaves the device. Enrolment carries a CSR (x509) or a public JWK
//     (dpop) and never key material; JWK.Thumbprint names the public key that becomes the single
//     transport binding for both modes (ADR 0020 decisions 3 and 4).

// HeaderClientCert is the edge-forwarded leaf certificate, PEM-encoded. Application Gateway sets
// it from the {var_client_certificate} server variable via a rewrite; the origin trusts it only
// when it is reachable solely through the edge, and re-validates it against the CA bundle anyway
// (ADR 0020 decision 2).
const HeaderClientCert = "X-Client-Cert"

// HeaderDPoP carries the RFC 9449 proof of possession. It is a per-request signature over the
// request, not a bearer credential.
const HeaderDPoP = "DPoP"

// HeaderAuthorization carries "DPoP <access-token>": the scheme names the token type and the value
// is the short-lived, sender-constrained token the DPoP proof is bound to (RFC 9449).
const HeaderAuthorization = "Authorization"

// EnrolmentSchemaVersion is the document version of the /v1/enrol exchange, independent of the
// /v1 API major version (docs/02-ingest-and-transport.md §5).
const EnrolmentSchemaVersion = "1.0"

// TokenTypeDPoP is the only value a TokenResponse may name: the access token is proof-of-
// possession bound, never a bearer token replayable from any host (ADR 0005, ADR 0020).
const TokenTypeDPoP = "DPoP"

// AuthMode is the closed set of production authenticators. `dev` exists in the deployment only
// behind an explicit startup acknowledgement and is deliberately not a wire mode, so it is not a
// member here (ADR 0020 decision 2).
type AuthMode string

const (
	AuthModeX509 AuthMode = "x509"
	AuthModeDPoP AuthMode = "dpop"
)

// Valid rejects anything outside the closed set rather than defaulting it. A default here would
// silently downgrade an unknown credential type to a known one, which is exactly the failure a
// pluggable authenticator must make impossible.
func (m AuthMode) Valid() bool {
	switch m {
	case AuthModeX509, AuthModeDPoP:
		return true
	default:
		return false
	}
}

// DeviceInfo is what the device asserts about itself at enrolment. hardware_identity_hash is the
// idempotency key: per-tenant unique, it makes a re-image return the existing device_id rather
// than mint a duplicate (C11), and a revoked device cannot re-enrol into a fresh identity through
// it.
//
// hostname and subject_name are the clear identity fields. As built (ADR 0021) a device whose
// tenant's device_identity is 'clear' sends the hostname (and, on each event, a subject_name);
// when the setting is 'hashed' it sends hostname_hash and no name. The setting reaches the device
// on the enrolment and health responses; until it has been told, the device uses its configured
// default, which is 'clear'.
type DeviceInfo struct {
	OS                   string `json:"os"`
	OSVersion            string `json:"os_version,omitempty"`
	AgentVersion         string `json:"agent_version"`
	Hostname             string `json:"hostname,omitempty"`
	HostnameHash         string `json:"hostname_hash,omitempty"`
	ManagedState         string `json:"managed_state,omitempty"`
	MDMID                string `json:"mdm_id,omitempty"`
	HardwareIdentityHash string `json:"hardware_identity_hash"`
}

// DeviceIdentity is the tenant's device-identity setting as the server states it to a device
// (ADR 0021). 'clear' means send the hostname and the submitting account name; 'hashed' means send
// only hostname_hash and no name. It is a closed pair, not a boolean, so a future third mode does
// not silently become one of the two.
type DeviceIdentity string

const (
	DeviceIdentityClear  DeviceIdentity = "clear"
	DeviceIdentityHashed DeviceIdentity = "hashed"
)

// Valid rejects anything outside the closed pair rather than defaulting it. A default here would
// silently pick a privacy posture the tenant did not choose.
func (d DeviceIdentity) Valid() bool {
	return d == DeviceIdentityClear || d == DeviceIdentityHashed
}

// ManagedState is the closed vocabulary for whether the device is under MDM. 'unknown' is a
// first-class value: it is what a device without an MDM integration reports, and conflating it
// with 'unmanaged' would assert a fact nobody established.
type ManagedState string

const (
	ManagedStateManaged   ManagedState = "managed"
	ManagedStateUnmanaged ManagedState = "unmanaged"
	ManagedStateUnknown   ManagedState = "unknown"
)

// Valid rejects anything outside the closed set.
func (m ManagedState) Valid() bool {
	return m == ManagedStateManaged || m == ManagedStateUnmanaged || m == ManagedStateUnknown
}

// EnrolmentRequest is the /v1/enrol body (docs/02-ingest-and-transport.md §5.1). It is mode-
// agnostic: an x509 device carries a CSR, a dpop device carries its public JWK, and the unused
// field is omitted. It carries a hostname and (via DeviceInfo) a managed state; it does not carry
// a subject name, which rides on each event, not on enrolment.
//
// A first enrolment presents exactly one bootstrap credential: EnrolmentToken, the short-lived
// single-use token the lab mints per device, or DeploymentKey, the reusable per-tenant key a
// customer's deployment package carries, since an MDM such as Intune delivers one package to every
// device and cannot hand each its own token. A deployment key alone is a shared secret, so a tenant
// may require Attestation as well: the identifiers the device's MDM enrolment gave it, which the
// server checks against the customer's MDM before it issues a credential.
type EnrolmentRequest struct {
	SchemaVersion  string             `json:"schema_version"`
	EnrolmentToken string             `json:"enrolment_token,omitempty"`
	DeploymentKey  string             `json:"deployment_key,omitempty"`
	Mode           AuthMode           `json:"mode"`
	CSR            string             `json:"csr,omitempty"`
	JWK            *JWK               `json:"jwk,omitempty"`
	Device         DeviceInfo         `json:"device"`
	Attestation    *DeviceAttestation `json:"attestation,omitempty"`
	ClaimedRegion  string             `json:"claimed_region,omitempty"`
}

// DeviceAttestation is what the device can say about its own management, read from the operating
// system rather than configured. None of it is secret; its value is that the server can look each
// identifier up in the customer's MDM and refuse a device the customer does not manage. Empty
// fields are absent facts, never guesses: a device that is not Intune-enrolled sends no Intune id.
type DeviceAttestation struct {
	// IntuneDeviceID is the Intune managed-device id (Windows: the enrolment's EntDMID).
	IntuneDeviceID string `json:"intune_device_id,omitempty"`
	// EntraDeviceID is the Microsoft Entra device object's deviceId, from the device's join state.
	EntraDeviceID string `json:"entra_device_id,omitempty"`
	// SerialNumber is the hardware serial the MDM inventories, compared case-insensitively.
	SerialNumber string `json:"serial_number,omitempty"`
}

// IssuedCredential is the credential half of the enrolment response. In x509 mode it is a leaf
// and its chain; in dpop mode CertPEM and ChainPEM are empty and JWK is the registered public key
// whose RFC 7638 thumbprint is the transport binding.
type IssuedCredential struct {
	Mode     AuthMode  `json:"mode"`
	CertPEM  string    `json:"cert_pem,omitempty"`
	ChainPEM []string  `json:"chain_pem,omitempty"`
	NotAfter time.Time `json:"not_after,omitempty"`
	JWK      *JWK      `json:"jwk,omitempty"`
}

// EnrolmentResponse is the /v1/enrol response (docs/02-ingest-and-transport.md §5.1). Reenrolled is
// true when the hardware identity already existed and the existing device_id is being returned
// rather than a new row created. PolicyETag lets the device skip its first policy GET when the
// bundle has not changed since enrolment.
type EnrolmentResponse struct {
	SchemaVersion string           `json:"schema_version"`
	DeviceID      string           `json:"device_id"`
	TenantID      string           `json:"tenant_id"`
	Region        string           `json:"region"`
	Reenrolled    bool             `json:"reenrolled"`
	Credential    IssuedCredential `json:"credential"`
	PolicyETag    string           `json:"policy_etag,omitempty"`
	// DeviceIdentity is the tenant's identity setting, so a device learns whether to send clear
	// values (ADR 0021). Empty means the server did not state one and the device keeps its default.
	DeviceIdentity DeviceIdentity `json:"device_identity,omitempty"`
	// UserRefKey is the tenant's user-reference key, base64url without padding (32 bytes). The
	// device derives each person's pseudonymous user_ref with it (DeriveUserRef), and the
	// directory side derives the same value from what the customer's identity provider sends, so
	// the two meet without either sending a name. Empty means the device keeps a configured ref.
	UserRefKey string    `json:"user_ref_key,omitempty"`
	ServerTime time.Time `json:"server_time"`
}

// JWK is the RFC 7517 public-key subset the DPoP mode uses. E and N carry an RSA modulus and
// exponent so the RSA branch of Thumbprint is representable; Crv, X and Y carry an EC point. Only
// the public half belongs here: the private key never leaves the device (ADR 0020 decision 3).
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`
	E   string `json:"e,omitempty"`
	N   string `json:"n,omitempty"`
	Alg string `json:"alg,omitempty"`
	Use string `json:"use,omitempty"`
	Kid string `json:"kid,omitempty"`
}

// Thumbprint returns the RFC 7638 JWK thumbprint: SHA-256 over the canonical JSON of only the
// required members, in lexicographic order, with no whitespace, encoded base64url without
// padding. For EC the members are crv, kty, x, y; for RSA they are e, kty, n. Optional members
// (alg, use, kid) are deliberately excluded, so adding a kid does not change the key's identity.
//
// It returns an error for an unsupported or missing kty and for missing required members rather
// than hashing a partial key, because a wrong thumbprint would bind a credential to the wrong key
// and be undetectable until impersonation succeeded.
func (k JWK) Thumbprint() (string, error) {
	// Member values here are base64url or short registry strings, which RFC 7638 §3.3 requires be
	// represented without escaping, so the canonical form is built directly rather than through a
	// JSON encoder that might escape or reorder.
	var canonical string
	switch k.Kty {
	case "EC":
		if k.Crv == "" || k.X == "" || k.Y == "" {
			return "", fmt.Errorf("protocol: EC JWK thumbprint needs crv, x and y")
		}
		canonical = `{"crv":"` + k.Crv + `","kty":"` + k.Kty + `","x":"` + k.X + `","y":"` + k.Y + `"}`
	case "RSA":
		if k.E == "" || k.N == "" {
			return "", fmt.Errorf("protocol: RSA JWK thumbprint needs e and n")
		}
		canonical = `{"e":"` + k.E + `","kty":"` + k.Kty + `","n":"` + k.N + `"}`
	default:
		return "", fmt.Errorf("protocol: JWK thumbprint does not support kty %q", k.Kty)
	}
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

// CredentialID derives the stable identifier of an X.509 device credential from the certificate's
// DER bytes. It is a name-based UUID over a fixed namespace and the DER, so the issuing service
// (control-api) and the verifying service (ingest-api) compute the same id from the same
// certificate without sharing state — and a re-issued certificate is a different credential, which
// is what makes revocation per credential rather than per device (ADR 0005).
//
// The value is the first 128 bits of SHA-256 over the namespace and the DER, rendered in the
// version-4/variant form the repository's UUID helper uses. It is deterministic, never random: a
// random id would have to be shared, and the verifier only ever sees the certificate.
func CredentialID(certDER []byte) string {
	sum := sha256.Sum256(append([]byte("sac-credential\x1f"), certDER...))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// TokenRequest is the DPoP token-endpoint request (RFC 9449). Assertion is the signed proof of
// possession of the device key; DeviceID is optional because a device may present a token-endpoint
// identity rather than a device registration in some deployments.
type TokenRequest struct {
	GrantType string `json:"grant_type"`
	Assertion string `json:"assertion"`
	DeviceID  string `json:"device_id,omitempty"`
}

// TokenResponse is the DPoP token-endpoint response. TokenType is fixed: a token that is not
// DPoP-bound is a bearer token, which ADR 0005 forbids.
type TokenResponse struct {
	AccessToken string    `json:"access_token"`
	TokenType   string    `json:"token_type"`
	ExpiresIn   int       `json:"expires_in"`
	ServerTime  time.Time `json:"server_time,omitempty"`
}

// Validate refuses a token response whose type is outside the closed vocabulary rather than
// letting a caller treat an unbound token as a DPoP one.
func (r TokenResponse) Validate() error {
	if r.AccessToken == "" {
		return fmt.Errorf("protocol: token response carries no access_token")
	}
	if r.TokenType != TokenTypeDPoP {
		return fmt.Errorf("protocol: token response token_type is %q, want %q", r.TokenType, TokenTypeDPoP)
	}
	if r.ExpiresIn <= 0 {
		return fmt.Errorf("protocol: token response expires_in is %d, want a positive lifetime", r.ExpiresIn)
	}
	return nil
}
