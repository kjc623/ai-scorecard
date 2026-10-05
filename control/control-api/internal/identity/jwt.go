package identity

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// An identity provider's id_token is verified with the algorithm pinned to RS256: an algorithm the
// token's own header could choose is the classic JWT confusion, and every provider the product
// targets (Entra, Okta, Ping, Google, ADFS) signs id_tokens with RS256 by default.

// jwtParts is a compact JWS split and decoded, before verification.
type jwtParts struct {
	kid     string
	claims  map[string]json.RawMessage
	signing string
	sig     []byte
}

func parseRS256(raw string) (jwtParts, error) {
	segs := strings.Split(raw, ".")
	if len(segs) != 3 {
		return jwtParts{}, errors.New("id_token is not a compact JWS")
	}
	hb, err := base64.RawURLEncoding.DecodeString(segs[0])
	if err != nil {
		return jwtParts{}, errors.New("id_token header is not base64url")
	}
	var header struct {
		Alg  string   `json:"alg"`
		Kid  string   `json:"kid"`
		Crit []string `json:"crit"`
	}
	if err := json.Unmarshal(hb, &header); err != nil {
		return jwtParts{}, errors.New("id_token header is not JSON")
	}
	if header.Alg != "RS256" {
		return jwtParts{}, fmt.Errorf("id_token alg %q, want RS256", header.Alg)
	}
	if len(header.Crit) > 0 {
		return jwtParts{}, errors.New("id_token carries critical header parameters this verifier does not process")
	}
	cb, err := base64.RawURLEncoding.DecodeString(segs[1])
	if err != nil {
		return jwtParts{}, errors.New("id_token payload is not base64url")
	}
	dec := json.NewDecoder(bytes.NewReader(cb))
	dec.UseNumber()
	var claims map[string]json.RawMessage
	if err := dec.Decode(&claims); err != nil {
		return jwtParts{}, errors.New("id_token payload is not a JSON object")
	}
	sig, err := base64.RawURLEncoding.DecodeString(segs[2])
	if err != nil {
		return jwtParts{}, errors.New("id_token signature is not base64url")
	}
	return jwtParts{kid: header.Kid, claims: claims, signing: segs[0] + "." + segs[1], sig: sig}, nil
}

func (p jwtParts) verify(pub *rsa.PublicKey) error {
	sum := sha256.Sum256([]byte(p.signing))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], p.sig); err != nil {
		return errors.New("id_token signature does not verify")
	}
	return nil
}

// str returns a string claim, or "" when absent or not a string.
func str(c map[string]json.RawMessage, name string) string {
	var s string
	if raw, ok := c[name]; ok && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// num returns a numeric claim.
func num(c map[string]json.RawMessage, name string) (int64, bool) {
	raw, ok := c[name]
	if !ok {
		return 0, false
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	if v, err := n.Int64(); err == nil {
		return v, true
	}
	if f, err := n.Float64(); err == nil {
		return int64(f), true
	}
	return 0, false
}

// strs returns a claim that is a string or an array of strings (Okta's groups, Entra's roles).
func strs(c map[string]json.RawMessage, name string) []string {
	raw, ok := c[name]
	if !ok {
		return nil
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	var one string
	if json.Unmarshal(raw, &one) == nil && one != "" {
		return []string{one}
	}
	return nil
}

// checkTimes applies exp (required), nbf and iat with the leeway.
func checkTimes(c map[string]json.RawMessage, now time.Time, leeway time.Duration) error {
	exp, ok := num(c, "exp")
	if !ok {
		return errors.New("id_token has no exp")
	}
	if !now.Before(time.Unix(exp, 0).Add(leeway)) {
		return errors.New("id_token has expired")
	}
	if nbf, ok := num(c, "nbf"); ok && now.Add(leeway).Before(time.Unix(nbf, 0)) {
		return errors.New("id_token is not yet valid")
	}
	if iat, ok := num(c, "iat"); ok && now.Add(leeway).Before(time.Unix(iat, 0)) {
		return errors.New("id_token was issued in the future")
	}
	return nil
}

// checkAudience requires aud to name clientID, and azp to be clientID when there are several.
func checkAudience(c map[string]json.RawMessage, clientID string) error {
	auds := strs(c, "aud")
	if !contains(auds, clientID) {
		return errors.New("id_token audience is not this client")
	}
	if len(auds) > 1 {
		if azp := str(c, "azp"); azp != clientID {
			return errors.New("id_token has several audiences and azp is not this client")
		}
	}
	return nil
}

// keyCache holds each provider's JWKS. A token whose kid is not cached triggers a refetch, but at
// most one per minRefetch per JWKS URL: a flood of tokens with invented kids must not become a flood
// of requests to the provider.
type keyCache struct {
	client     *http.Client
	now        func() time.Time
	minRefetch time.Duration
	maxAge     time.Duration

	mu      sync.Mutex
	entries map[string]*keyEntry
}

type keyEntry struct {
	keys        map[string]*rsa.PublicKey
	anonymous   []*rsa.PublicKey // keys published without a kid
	fetchedAt   time.Time
	lastAttempt time.Time
}

func newKeyCache(client *http.Client, now func() time.Time, minRefetch time.Duration) *keyCache {
	return &keyCache{client: client, now: now, minRefetch: minRefetch, maxAge: 24 * time.Hour, entries: map[string]*keyEntry{}}
}

// key returns the public key for kid at jwksURL.
func (k *keyCache) key(ctx context.Context, jwksURL, kid string) (*rsa.PublicKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	e := k.entries[jwksURL]
	if e == nil {
		e = &keyEntry{}
		k.entries[jwksURL] = e
	}
	found := func() *rsa.PublicKey {
		if kid != "" {
			return e.keys[kid]
		}
		if len(e.keys)+len(e.anonymous) == 1 {
			for _, v := range e.keys {
				return v
			}
			return e.anonymous[0]
		}
		return nil
	}
	stale := e.fetchedAt.IsZero() || now.Sub(e.fetchedAt) >= k.maxAge
	if pub := found(); pub != nil && !stale {
		return pub, nil
	}
	if stale || now.Sub(e.lastAttempt) >= k.minRefetch {
		e.lastAttempt = now
		keys, anon, err := fetchJWKSDetailed(ctx, k.client, jwksURL)
		if err != nil {
			if pub := found(); pub != nil {
				return pub, nil // a provider outage does not invalidate keys we already hold
			}
			return nil, err
		}
		e.keys, e.anonymous, e.fetchedAt = keys, anon, now
	}
	if pub := found(); pub != nil {
		return pub, nil
	}
	if kid == "" {
		return nil, errors.New("id_token names no kid and the provider publishes several keys")
	}
	return nil, errors.New("id_token kid is not in the provider's JWKS")
}

func fetchJWKS(ctx context.Context, client *http.Client, address string) ([]*rsa.PublicKey, error) {
	keys, anon, err := fetchJWKSDetailed(ctx, client, address)
	if err != nil {
		return nil, err
	}
	out := append([]*rsa.PublicKey(nil), anon...)
	for _, v := range keys {
		out = append(out, v)
	}
	return out, nil
}

// fetchJWKSDetailed reads the RSA signing keys of a JWKS. Keys for another use or algorithm are
// skipped rather than refused, because providers publish encryption keys and EC keys alongside.
func fetchJWKSDetailed(ctx context.Context, client *http.Client, address string) (map[string]*rsa.PublicKey, []*rsa.PublicKey, error) {
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Use string `json:"use"`
			Alg string `json:"alg"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := getJSON(ctx, client, address, &doc); err != nil {
		return nil, nil, fmt.Errorf("jwks: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	var anon []*rsa.PublicKey
	for _, j := range doc.Keys {
		if j.Kty != "RSA" || (j.Use != "" && j.Use != "sig") || (j.Alg != "" && j.Alg != "RS256") {
			continue
		}
		nb, err1 := base64.RawURLEncoding.DecodeString(j.N)
		eb, err2 := base64.RawURLEncoding.DecodeString(j.E)
		if err1 != nil || err2 != nil || len(eb) == 0 || len(eb) > 4 {
			continue
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(new(big.Int).SetBytes(eb).Int64())}
		if pub.N.BitLen() < 2048 || pub.E < 3 {
			continue
		}
		if j.Kid == "" {
			anon = append(anon, pub)
			continue
		}
		keys[j.Kid] = pub
	}
	return keys, anon, nil
}
