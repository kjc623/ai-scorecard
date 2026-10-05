package scim

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

// The SCIM token format: the enrolment token's spelling (internal/enrol/token.go) with its own
// prefix, so a SCIM token, a deployment key and an enrolment token can never be mistaken for one
// another in a log or a support ticket. The tenant id is in the clear because the RLS session must be
// opened before the token's row can be read; only the 256-bit tail is secret, and only the sha256 of
// the whole plaintext is stored.
const (
	TokenPrefix      = "sacscim_"
	tokenSecretBytes = 32
)

// ErrMalformedToken is a presented string that is not a SCIM token at all.
var ErrMalformedToken = errors.New("scim: malformed token")

// MintToken returns a fresh plaintext token for a tenant and its stored hash. The plaintext is shown
// to the admin once and never stored.
func MintToken(tenantID string) (plaintext, hash string, err error) {
	if !store.IsUUID(tenantID) {
		return "", "", fmt.Errorf("scim: tenant %q is not a uuid", tenantID)
	}
	var secret [tokenSecretBytes]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", "", fmt.Errorf("scim: no entropy for a token: %w", err)
	}
	plaintext = TokenPrefix + strings.ToLower(tenantID) + "." + base64.RawURLEncoding.EncodeToString(secret[:])
	return plaintext, HashToken(plaintext), nil
}

// ParseToken returns the tenant a token claims. It checks the shape only; the hash lookup decides.
func ParseToken(plaintext string) (string, error) {
	rest, ok := strings.CutPrefix(plaintext, TokenPrefix)
	if !ok {
		return "", ErrMalformedToken
	}
	tenant, secret, ok := strings.Cut(rest, ".")
	if !ok || !store.IsUUID(tenant) {
		return "", ErrMalformedToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || len(raw) != tokenSecretBytes {
		return "", ErrMalformedToken
	}
	return strings.ToLower(tenant), nil
}

// HashToken is the stored form: sha256 over the plaintext, in the repository's digest spelling.
func HashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return "sha256:" + hex.EncodeToString(sum[:])
}
