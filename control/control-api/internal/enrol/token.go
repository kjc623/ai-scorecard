package enrol

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/shadow-ai-capture/control-api/internal/store"
)

// The enrolment-token format. The token is tenant-scoped and carries the tenant id in the clear, so
// the server can set the row-level-security session before it reads the token's row: the tenant must
// come from the token and never from the request body (docs/02 §12), and the token is the only thing
// presented. The tenant id is not a secret; the 256-bit random tail is, and only its SHA-256 hash is
// stored. A token is delivered by the MDM enrolment profile (A5) and is short-lived and single-use.
const (
	tokenPrefix = "sac1"
	// tokenSecretBytes is the entropy of the random tail: 256 bits, so a token cannot be guessed.
	tokenSecretBytes = 32
)

// ErrMalformedToken is returned when a presented string is not an enrolment token at all. It is a
// distinct condition from a well-formed token that is unknown, used, revoked or expired.
var ErrMalformedToken = errors.New("enrol: malformed enrolment token")

// MintEnrolmentToken returns a fresh plaintext token for a tenant. It is used by tests and by an
// operator-side provisioning path; the plaintext is delivered out of band and never stored.
func MintEnrolmentToken(tenantID string) (string, error) {
	if !store.IsUUID(tenantID) {
		return "", fmt.Errorf("enrol: tenant %q is not a uuid", tenantID)
	}
	var secret [tokenSecretBytes]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", fmt.Errorf("enrol: no entropy for an enrolment token: %w", err)
	}
	return strings.Join([]string{tokenPrefix, tenantID, base64.RawURLEncoding.EncodeToString(secret[:])}, "."), nil
}

// ParseEnrolmentToken extracts the tenant from a plaintext token. It validates the shape but not the
// hash; the store lookup and the constant-time comparison do that.
func ParseEnrolmentToken(plaintext string) (string, error) {
	parts := strings.Split(plaintext, ".")
	if len(parts) != 3 || parts[0] != tokenPrefix {
		return "", ErrMalformedToken
	}
	if !store.IsUUID(parts[1]) {
		return "", fmt.Errorf("%w: tenant segment is not a uuid", ErrMalformedToken)
	}
	secret, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(secret) < 16 {
		return "", fmt.Errorf("%w: secret segment is not base64url", ErrMalformedToken)
	}
	return parts[1], nil
}

// HashEnrolmentToken is the stored form of a token: sha256 over the plaintext, in the one spelling
// the repository uses for digests. The plaintext is never written.
func HashEnrolmentToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// The deployment-key format (contract §1, §5): the enrolment token's spelling with its own prefix,
// `sacdk_<tenant uuid>.<256-bit base64url secret>`. The tenant is in the clear for the token's
// reason, so the RLS session is set before the key's row is read. Unlike a token it is reusable and
// long-lived, because one customer package carries it to every device; what bounds it is
// revocation, an optional expiry, a per-key rate limit and, for an Intune tenant, the MDM check.
const deploymentKeyPrefix = "sacdk_"

// MintDeploymentKey returns a fresh plaintext deployment key for a tenant. The plaintext goes into
// one package and is never stored or logged.
func MintDeploymentKey(tenantID string) (string, error) {
	if !store.IsUUID(tenantID) {
		return "", fmt.Errorf("enrol: tenant %q is not a uuid", tenantID)
	}
	var secret [tokenSecretBytes]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", fmt.Errorf("enrol: no entropy for a deployment key: %w", err)
	}
	return deploymentKeyPrefix + strings.ToLower(tenantID) + "." + base64.RawURLEncoding.EncodeToString(secret[:]), nil
}

// ParseDeploymentKey extracts the tenant from a plaintext deployment key, validating its shape only.
func ParseDeploymentKey(plaintext string) (string, error) {
	rest, ok := strings.CutPrefix(plaintext, deploymentKeyPrefix)
	if !ok {
		return "", ErrMalformedToken
	}
	tenantID, secretPart, ok := strings.Cut(rest, ".")
	if !ok || !store.IsUUID(tenantID) {
		return "", fmt.Errorf("%w: tenant segment is not a uuid", ErrMalformedToken)
	}
	secret, err := base64.RawURLEncoding.DecodeString(secretPart)
	if err != nil || len(secret) < tokenSecretBytes {
		return "", fmt.Errorf("%w: secret segment is not a 256-bit base64url value", ErrMalformedToken)
	}
	return tenantID, nil
}

// HashDeploymentKey is the stored form of a deployment key, in the enrolment token's spelling.
func HashDeploymentKey(plaintext string) string { return HashEnrolmentToken(plaintext) }
