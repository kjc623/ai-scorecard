package identity

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
)

// An identity provider's id_token is verified with the algorithm pinned to RS256: an algorithm the
// token's own header could choose is the classic JWT confusion, and every provider the product
// targets (Entra, Okta, Ping, Google, ADFS) signs id_tokens with RS256 by default.

// verifyIDTokenSignature checks raw's signature against the provider's JWKS at jwksURL and its
// audience against clientID, and returns its claims. The issuer, the times and the nonce are the
// caller's: Entra's issuer depends on the token's own tenant, and the time checks use the product's
// leeway.
func (s *Service) verifyIDTokenSignature(ctx context.Context, jwksURL, clientID, raw string) (map[string]json.RawMessage, error) {
	v := oidc.NewVerifier("", s.keySet(jwksURL), &oidc.Config{
		ClientID:             clientID,
		SupportedSigningAlgs: []string{oidc.RS256},
		SkipIssuerCheck:      true,
		SkipExpiryCheck:      true,
	})
	tok, err := v.Verify(ctx, raw)
	if err != nil {
		return nil, err
	}
	var claims map[string]json.RawMessage
	if err := tok.Claims(&claims); err != nil {
		return nil, errors.New("id_token payload is not a JSON object")
	}
	return claims, nil
}

// keySet returns the cached key set for a JWKS URL. A RemoteKeySet caches the provider's keys and
// refetches them when a token names a key it has not seen, which is how a provider's rotation is
// picked up.
func (s *Service) keySet(jwksURL string) *oidc.RemoteKeySet {
	s.mu.Lock()
	defer s.mu.Unlock()
	ks, ok := s.keySets[jwksURL]
	if !ok {
		ks = oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), s.client), jwksURL)
		s.keySets[jwksURL] = ks
	}
	return ks
}

// rsaSigningKeys fetches a JWKS and counts its RSA signing keys of at least 2048 bits. Keys for
// another use or algorithm are skipped rather than refused, because providers publish encryption
// keys and EC keys alongside.
func rsaSigningKeys(ctx context.Context, client *http.Client, address string) (int, error) {
	var doc jose.JSONWebKeySet
	if err := getJSON(ctx, client, address, &doc); err != nil {
		return 0, errors.Join(errors.New("jwks"), err)
	}
	n := 0
	for _, k := range doc.Keys {
		pub, ok := k.Key.(*rsa.PublicKey)
		if !ok || (k.Use != "" && k.Use != "sig") || (k.Algorithm != "" && k.Algorithm != string(jose.RS256)) {
			continue
		}
		if pub.N.BitLen() >= 2048 {
			n++
		}
	}
	return n, nil
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

// checkAuthorizedParty requires azp to be clientID when the token names several audiences.
func checkAuthorizedParty(c map[string]json.RawMessage, clientID string) error {
	if len(strs(c, "aud")) > 1 && str(c, "azp") != clientID {
		return errors.New("id_token has several audiences and azp is not this client")
	}
	return nil
}
