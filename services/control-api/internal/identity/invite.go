package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/shadow-ai-capture/control-api/internal/session"
)

// The onboarding invite token. The tenant id rides in the clear so the RLS session can be opened before the lookup; the 256-bit tail
// is the secret, and only sha256 of the whole token is stored.
//
//	sacinv_<tenant uuid>.<base64url 32 bytes>
//
// The token is the only credential the customer's admin holds before their first sign-in, so it is
// single-use, it expires, and it binds to the tenant in its own text: a token presented for another
// tenant's onboarding cannot be looked up there.
const InvitePrefix = "sacinv_"

// ErrMalformedInvite is a string that is not an invite token at all.
var ErrMalformedInvite = errors.New("identity: malformed invite token")

// MintInviteToken returns a fresh plaintext invite for a tenant. The plaintext is shown once.
func MintInviteToken(tenantID string) (string, error) {
	if !session.IsUUID(tenantID) {
		return "", fmt.Errorf("identity: tenant %q is not a uuid", tenantID)
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", fmt.Errorf("identity: no entropy for an invite: %w", err)
	}
	return InvitePrefix + strings.ToLower(tenantID) + "." + base64.RawURLEncoding.EncodeToString(secret[:]), nil
}

// ParseInviteToken returns the tenant an invite names. It checks the shape, not the secret; the hash
// lookup does that.
func ParseInviteToken(token string) (string, error) {
	rest, ok := strings.CutPrefix(token, InvitePrefix)
	if !ok {
		return "", ErrMalformedInvite
	}
	tenant, secret, ok := strings.Cut(rest, ".")
	if !ok || !session.IsUUID(tenant) {
		return "", ErrMalformedInvite
	}
	raw, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || len(raw) != 32 {
		return "", ErrMalformedInvite
	}
	return strings.ToLower(tenant), nil
}

// HashToken is the stored form of a token: sha256 in the repository's one digest spelling.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:])
}
