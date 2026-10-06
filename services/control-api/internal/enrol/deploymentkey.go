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

// A deployment key is `sacdk_<tenant uuid>.<256-bit base64url secret>`. The tenant rides in the
// clear so the row-level-security session can be opened before the key's row is read; the secret
// is the credential, and only the SHA-256 of the whole key is stored. One tenant package carries
// it to every device, so it is reusable; revocation, an optional expiry, a per-key rate limit and,
// for an Intune tenant, the MDM check bound what a copied package can do.
const (
	deploymentKeyPrefix = "sacdk_"
	secretBytes         = 32
)

// ErrMalformedKey is a presented string that is not a deployment key at all.
var ErrMalformedKey = errors.New("enrol: malformed deployment key")

// MintDeploymentKey returns a fresh plaintext deployment key for a tenant. The plaintext goes into
// one package and is never stored or logged.
func MintDeploymentKey(tenantID string) (string, error) {
	if !store.IsUUID(tenantID) {
		return "", fmt.Errorf("enrol: tenant %q is not a uuid", tenantID)
	}
	var secret [secretBytes]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", fmt.Errorf("enrol: no entropy for a deployment key: %w", err)
	}
	return deploymentKeyPrefix + strings.ToLower(tenantID) + "." + base64.RawURLEncoding.EncodeToString(secret[:]), nil
}

// ParseDeploymentKey returns the tenant a deployment key names, checking its shape only.
func ParseDeploymentKey(plaintext string) (string, error) {
	rest, ok := strings.CutPrefix(plaintext, deploymentKeyPrefix)
	if !ok {
		return "", ErrMalformedKey
	}
	tenantID, secretPart, ok := strings.Cut(rest, ".")
	if !ok || !store.IsUUID(tenantID) {
		return "", fmt.Errorf("%w: tenant segment is not a uuid", ErrMalformedKey)
	}
	secret, err := base64.RawURLEncoding.DecodeString(secretPart)
	if err != nil || len(secret) < secretBytes {
		return "", fmt.Errorf("%w: secret segment is not a 256-bit base64url value", ErrMalformedKey)
	}
	return strings.ToLower(tenantID), nil
}

// HashDeploymentKey is the stored form of a deployment key: sha256:<hex> of the plaintext.
func HashDeploymentKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return "sha256:" + hex.EncodeToString(sum[:])
}
