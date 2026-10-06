package protocol

import (
	"crypto/sha256"
	"fmt"
	"time"
)

// Device enrolment and authentication.
//
// A device authenticates with an X.509 certificate that control-api issues at enrolment. The
// device generates its key pair locally and sends only a CSR; the private key never leaves the
// device. Application Gateway terminates the device's TLS connection and forwards the presented
// leaf certificate to the origin in X-Client-Cert, and the origin verifies it against the device
// CA before trusting it.

// HeaderClientCert carries the edge-forwarded leaf certificate, PEM-encoded (URL-encoded by
// Application Gateway).
const HeaderClientCert = "X-Client-Cert"

// EnrolmentSchemaVersion is the document version of the /v1/enrol exchange.
const EnrolmentSchemaVersion = "1.0"

// DeviceInfo is what the device asserts about itself at enrolment. HardwareIdentityHash is the
// per-tenant idempotency key: a re-imaged device gets its existing device_id back instead of a
// duplicate, and a revoked device cannot re-enrol into a fresh identity.
//
// Hostname is sent when the tenant's device identity setting is clear; HostnameHash replaces it
// when the setting is hashed.
type DeviceInfo struct {
	OS                   string `json:"os"`
	OSVersion            string `json:"os_version,omitempty"`
	AgentVersion         string `json:"agent_version"`
	Hostname             string `json:"hostname,omitempty"`
	HostnameHash         string `json:"hostname_hash,omitempty"`
	ManagedState         string `json:"managed_state,omitempty"`
	HardwareIdentityHash string `json:"hardware_identity_hash"`
}

// DeviceIdentity is the tenant's device identity setting: clear sends the hostname and the
// submitting account name, hashed sends only a hostname hash and no name.
type DeviceIdentity string

const (
	DeviceIdentityClear  DeviceIdentity = "clear"
	DeviceIdentityHashed DeviceIdentity = "hashed"
)

// Valid reports whether d is one of the two settings.
func (d DeviceIdentity) Valid() bool {
	return d == DeviceIdentityClear || d == DeviceIdentityHashed
}

// ManagedState says whether the device is under MDM. unknown is what a device without an MDM
// integration reports; it is not the same fact as unmanaged.
type ManagedState string

const (
	ManagedStateManaged   ManagedState = "managed"
	ManagedStateUnmanaged ManagedState = "unmanaged"
	ManagedStateUnknown   ManagedState = "unknown"
)

// Valid reports whether m is in the closed set.
func (m ManagedState) Valid() bool {
	return m == ManagedStateManaged || m == ManagedStateUnmanaged || m == ManagedStateUnknown
}

// EnrolmentRequest is the POST /v1/enrol body.
//
// A first enrolment presents the tenant's DeploymentKey, which the MDM-delivered tenant package
// carries. A re-enrolment (certificate rotation) presents the current certificate on the
// transport instead and omits the key. CSR is the PKCS#10 request, PEM-encoded. A tenant may
// require Attestation as well, which the server checks against the customer's MDM.
type EnrolmentRequest struct {
	SchemaVersion string             `json:"schema_version"`
	DeploymentKey string             `json:"deployment_key,omitempty"`
	CSR           string             `json:"csr"`
	Device        DeviceInfo         `json:"device"`
	Attestation   *DeviceAttestation `json:"attestation,omitempty"`
}

// DeviceAttestation is what the device reads about its own management from the operating
// system. None of it is secret; the server looks each identifier up in the customer's MDM and
// refuses a device the customer does not manage. Empty fields are absent facts.
type DeviceAttestation struct {
	// IntuneDeviceID is the Intune managed-device id (Windows: the enrolment's EntDMID).
	IntuneDeviceID string `json:"intune_device_id,omitempty"`
	// EntraDeviceID is the Microsoft Entra device object's deviceId, from the device's join state.
	EntraDeviceID string `json:"entra_device_id,omitempty"`
	// SerialNumber is the hardware serial the MDM inventories, compared case-insensitively.
	SerialNumber string `json:"serial_number,omitempty"`
}

// IssuedCredential is the device certificate and its chain.
type IssuedCredential struct {
	CertPEM  string    `json:"cert_pem"`
	ChainPEM []string  `json:"chain_pem,omitempty"`
	NotAfter time.Time `json:"not_after"`
}

// EnrolmentResponse is the POST /v1/enrol response. Reenrolled is true when the hardware identity
// already existed and its device_id is returned. PolicyETag lets the device skip its first policy
// fetch when the bundle is unchanged.
type EnrolmentResponse struct {
	SchemaVersion string           `json:"schema_version"`
	DeviceID      string           `json:"device_id"`
	TenantID      string           `json:"tenant_id"`
	Region        string           `json:"region"`
	Reenrolled    bool             `json:"reenrolled"`
	Credential    IssuedCredential `json:"credential"`
	PolicyETag    string           `json:"policy_etag,omitempty"`
	// DeviceIdentity is the tenant's identity setting. Empty means the device keeps its default.
	DeviceIdentity DeviceIdentity `json:"device_identity,omitempty"`
	// UserRefKey is the tenant's user-reference key, base64url without padding (32 bytes). The
	// device derives each person's pseudonymous user_ref with it (DeriveUserRef), and the directory
	// side derives the same value from the identity provider's data, so neither sends a name.
	UserRefKey string    `json:"user_ref_key,omitempty"`
	ServerTime time.Time `json:"server_time"`
}

// CredentialID derives the stable identifier of a device certificate from its DER bytes, so the
// issuer (control-api) and the verifiers (ingest-api, control-api) compute the same id without
// sharing state. A re-issued certificate is a different credential, which makes revocation per
// credential rather than per device.
func CredentialID(certDER []byte) string {
	sum := sha256.Sum256(append([]byte("sac-credential\x1f"), certDER...))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
