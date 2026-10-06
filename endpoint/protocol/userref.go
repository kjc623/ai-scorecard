package protocol

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// The pseudonymous user reference, derived the same way on both sides of the product.
//
// The device knows who is signed in to the operating system; the customer's identity provider
// (through SCIM) knows who that person is in the directory. Neither sends the other a name on the
// event path, so both derive user_ref from the identifiers they share (the user principal name,
// the Entra object id, or a bare account name) under a per-tenant key the server issues at
// enrolment. capture-core and control-api both import this function, and the test vectors in
// userref_test.go pin it.
//
// The device prefers the UPN because every identity provider sends one through SCIM, Entra or
// not; the object id is the fallback when the device cannot resolve a UPN, and the directory keeps
// an alias from it. A local account with neither becomes an "acct" reference that no directory
// row matches, which is the explicit unmapped series rather than a dropped person.

// UserRefKind is the closed set of identifiers a user_ref may be derived from.
type UserRefKind string

const (
	UserRefUPN     UserRefKind = "upn"
	UserRefOID     UserRefKind = "oid"
	UserRefAccount UserRefKind = "acct"
)

// UserRefKeySize is the length of a tenant's user-reference key.
const UserRefKeySize = 32

// DeriveUserRef returns "u_" and the first 16 bytes, hex, of HMAC-SHA256(key, kind ":" value),
// where value is trimmed and lower-cased so a UPN typed in another case is the same person.
// It refuses a short key and an empty value rather than producing a reference that every
// unidentified person would share.
func DeriveUserRef(key []byte, kind UserRefKind, value string) (string, error) {
	if len(key) != UserRefKeySize {
		return "", fmt.Errorf("protocol: user_ref key is %d bytes, want %d", len(key), UserRefKeySize)
	}
	switch kind {
	case UserRefUPN, UserRefOID, UserRefAccount:
	default:
		return "", fmt.Errorf("protocol: user_ref kind %q is not upn, oid or acct", kind)
	}
	v := strings.ToLower(strings.TrimSpace(value))
	if v == "" {
		return "", fmt.Errorf("protocol: user_ref %s value is empty", kind)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(string(kind) + ":" + v))
	return "u_" + hex.EncodeToString(mac.Sum(nil)[:16]), nil
}

// DecodeUserRefKey reads the base64url (unpadded) key an enrolment response carries.
func DecodeUserRefKey(encoded string) ([]byte, error) {
	key, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("protocol: user_ref key is not base64url: %w", err)
	}
	if len(key) != UserRefKeySize {
		return nil, fmt.Errorf("protocol: user_ref key is %d bytes, want %d", len(key), UserRefKeySize)
	}
	return key, nil
}
