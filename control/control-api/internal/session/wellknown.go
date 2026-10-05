package session

import (
	"encoding/json"
	"net/http"
)

// Well-known paths. Verifiers default SAC_AUTH_JWKS_URL to {issuer}/.well-known/jwks.json.
const (
	PathJWKS      = "/.well-known/jwks.json"
	PathDiscovery = "/.well-known/openid-configuration"
)

// WellKnownHandler serves the JWKS and a minimal discovery document. Both are public by nature: they
// hold public keys and the issuer name. The discovery document is not a claim that control-api is a
// browser-facing OpenID provider; it exists so a verifier that discovers keys the standard way finds
// them.
func (i *Issuer) WellKnownHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		var body any
		switch r.URL.Path {
		case PathJWKS:
			body = i.keys.JWKS()
		case PathDiscovery:
			body = map[string]any{
				"issuer":                                i.issuer,
				"jwks_uri":                              i.issuer + PathJWKS,
				"id_token_signing_alg_values_supported": []string{"ES256"},
				"subject_types_supported":               []string{"public"},
				"response_types_supported":              []string{},
			}
		default:
			writeJSONError(w, http.StatusNotFound, "not_found")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Short enough that a rotation reaches verifiers well inside a token lifetime, long enough
		// that a verifier's kid-miss refetch is not the steady state.
		w.Header().Set("Cache-Control", "public, max-age=300")
		_ = json.NewEncoder(w).Encode(body)
	})
}
