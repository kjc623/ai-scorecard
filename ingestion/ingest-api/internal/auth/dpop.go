package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/ingest-api/internal/contract"
	"github.com/shadow-ai-capture/ingest-api/internal/store"
)

const (
	// DefaultDPoPReplayTTL bounds how long a proof jti is refused a second time. It must exceed the
	// proof clock skew, so a proof cannot be replayed after its replay row has expired but while its
	// iat is still inside the accepted window.
	DefaultDPoPReplayTTL = 5 * time.Minute
	// dpopMaxSkew is the tolerance on the access token's and the proof's iat, covering ordinary clock
	// drift between the token endpoint, the device and this deployment. The token's exp is enforced
	// without any tolerance: an expired token is refused, full stop.
	dpopMaxSkew = 30 * time.Second
	// dpopProofType is the RFC 9449 media type a proof's protected header must carry.
	dpopProofType = "dpop+jwt"
	// accessTokenType is the at+jwt media type the shared token format carries.
	accessTokenType = "at+jwt"
)

// DPoPAuthenticator authenticates the dpop mode of ADR 0020 decision 2 (RFC 9449). It verifies
// three things and binds them to one identity:
//
//   - the access token is a compact ES256 JWS signed by the deployment token public key, with the
//     configured issuer and audience and a cnf.jkt binding;
//   - the per-request DPoP proof is a compact ES256 JWS over this exact request (htm/htu/ath), its
//     embedded public key's RFC 7638 thumbprint equals the token's cnf.jkt;
//   - that thumbprint equals the stored ops.device_credential.public_key_thumbprint, and the row's
//     credential_type is dpop.
//
// Tenant, device and credential come only from the signed token claims, never from the proof or the
// body, so a proof cannot name a different principal than the token bound it to. The proof's jti is
// recorded once and a second presentation is refused.
type DPoPAuthenticator struct {
	Store          store.Store
	TokenPublicKey crypto.PublicKey
	Issuer         string
	Audience       string
	Region         string
	Now            func() time.Time
	// ReplayTTL is how long a proof jti stays refused. Zero means DefaultDPoPReplayTTL.
	ReplayTTL time.Duration
}

// Presents reports whether the request carries a DPoP access token. A missing or non-DPoP
// Authorization scheme is not this authenticator's credential.
func (a *DPoPAuthenticator) Presents(r *http.Request) bool {
	_, ok := dpopAccessToken(r)
	return ok
}

// Authenticate implements Authenticator.
func (a *DPoPAuthenticator) Authenticate(ctx context.Context, r *http.Request) (Principal, error) {
	accessToken, ok := dpopAccessToken(r)
	if !ok {
		return Principal{}, ErrNoCredential
	}
	proof := strings.TrimSpace(r.Header.Get(protocol.HeaderDPoP))
	if proof == "" {
		return Principal{}, fmt.Errorf("%w: the DPoP proof header is absent", ErrBadProof)
	}
	now := nowFrom(a.Now)

	claims, err := a.verifyAccessToken(accessToken, now)
	if err != nil {
		return Principal{}, err
	}
	jkt, jti, err := a.verifyProof(r, accessToken, proof, claims.CNF.Jkt, now)
	if err != nil {
		return Principal{}, err
	}

	ttl := a.ReplayTTL
	if ttl <= 0 {
		ttl = DefaultDPoPReplayTTL
	}
	// The jti is recorded only after the token and proof verify and the thumbprint binds to the
	// token, so a forged proof cannot fill the replay table. A replay is an authentication failure.
	seen, err := a.Store.DPoPReplaySeen(ctx, claims.TenantID, jti, now.Add(ttl))
	if err != nil {
		return Principal{}, fmt.Errorf("auth: DPoP replay store: %w", err)
	}
	if seen {
		return Principal{}, ErrReplay
	}

	return resolveCredential(ctx, a.Store, a.Region, now, credentialProof{
		TenantID:     claims.TenantID,
		DeviceID:     claims.DeviceID,
		CredentialID: claims.CredentialID,
		Mode:         protocol.AuthModeDPoP,
		Thumbprint:   jkt,
	})
}

// accessTokenClaims is the shared compact-JWS claim set. It is the same vocabulary the control-api
// slice mints: tenant_id, device_id and credential_id name the principal; cnf.jkt is the
// sender-constraint binding.
type accessTokenClaims struct {
	Iss          string          `json:"iss"`
	Aud          json.RawMessage `json:"aud"`
	Sub          string          `json:"sub"`
	TenantID     string          `json:"tenant_id"`
	DeviceID     string          `json:"device_id"`
	CredentialID string          `json:"credential_id"`
	CNF          struct {
		Jkt string `json:"jkt"`
	} `json:"cnf"`
	Iat int64  `json:"iat"`
	Exp int64  `json:"exp"`
	Jti string `json:"jti"`
}

// verifyAccessToken checks the token's signature, registered claims and binding claim.
func (a *DPoPAuthenticator) verifyAccessToken(token string, now time.Time) (accessTokenClaims, error) {
	var claims accessTokenClaims
	pub, ok := a.TokenPublicKey.(*ecdsa.PublicKey)
	if !ok || pub == nil || pub.Curve == nil {
		return claims, fmt.Errorf("%w: the deployment token public key is not an ECDSA key", ErrBadAccessToken)
	}
	parts, err := splitCompactJWS(token)
	if err != nil {
		return claims, fmt.Errorf("%w: %v", ErrBadAccessToken, err)
	}
	var header jwsHeader
	if err := json.Unmarshal(parts.header, &header); err != nil {
		return claims, fmt.Errorf("%w: unreadable token header: %v", ErrBadAccessToken, err)
	}
	if header.Alg != "ES256" {
		return claims, fmt.Errorf("%w: token alg is %q, want ES256", ErrBadAccessToken, header.Alg)
	}
	if header.Typ != accessTokenType {
		return claims, fmt.Errorf("%w: token typ is %q, want %q", ErrBadAccessToken, header.Typ, accessTokenType)
	}
	if err := verifyES256(pub, parts.signingInput, parts.signature); err != nil {
		return claims, fmt.Errorf("%w: %v", ErrBadAccessToken, err)
	}
	if err := json.Unmarshal(parts.payload, &claims); err != nil {
		return claims, fmt.Errorf("%w: unreadable token claims: %v", ErrBadAccessToken, err)
	}
	if claims.Iss != a.Issuer {
		return claims, fmt.Errorf("%w: token issuer is %q, want %q", ErrBadAccessToken, claims.Iss, a.Issuer)
	}
	if !audienceContains(claims.Aud, a.Audience) {
		return claims, fmt.Errorf("%w: token audience does not include %q", ErrBadAccessToken, a.Audience)
	}
	if claims.Exp == 0 || !now.Before(time.Unix(claims.Exp, 0)) {
		return claims, fmt.Errorf("%w: the token is expired or carries no exp", ErrBadAccessToken)
	}
	if claims.Iat == 0 || now.Before(time.Unix(claims.Iat, 0).Add(-dpopMaxSkew)) {
		return claims, fmt.Errorf("%w: the token iat is missing or too far in the future", ErrBadAccessToken)
	}
	if claims.CNF.Jkt == "" {
		return claims, fmt.Errorf("%w: the token carries no cnf.jkt binding", ErrBadAccessToken)
	}
	if claims.Sub == "" || claims.Sub != claims.DeviceID {
		return claims, fmt.Errorf("%w: the token sub does not name the device_id claim", ErrBadAccessToken)
	}
	// The identities are read from the signed token, so they must have the shapes the store's uuid
	// parameters require; a malformed claim is a bad credential, not a database error.
	for _, id := range []struct {
		name  string
		value string
	}{{"tenant_id", claims.TenantID}, {"device_id", claims.DeviceID}, {"credential_id", claims.CredentialID}} {
		if !contract.IsUUID(id.value) {
			return claims, fmt.Errorf("%w: token %s %q is not a uuid", ErrBadAccessToken, id.name, id.value)
		}
	}
	return claims, nil
}

// verifyProof checks the per-request proof and returns the thumbprint it demonstrates and its jti.
// expectedJKT is the token's cnf.jkt, which the proof's embedded key must match before the store is
// consulted.
func (a *DPoPAuthenticator) verifyProof(r *http.Request, accessToken, proof, expectedJKT string, now time.Time) (jkt, jti string, err error) {
	parts, err := splitCompactJWS(proof)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrBadProof, err)
	}
	var header jwsHeader
	if err := json.Unmarshal(parts.header, &header); err != nil {
		return "", "", fmt.Errorf("%w: unreadable proof header: %v", ErrBadProof, err)
	}
	if header.Typ != dpopProofType {
		return "", "", fmt.Errorf("%w: proof typ is %q, want %q", ErrBadProof, header.Typ, dpopProofType)
	}
	if header.Alg != "ES256" {
		return "", "", fmt.Errorf("%w: proof alg is %q, want ES256", ErrBadProof, header.Alg)
	}
	if len(header.JWK) == 0 {
		return "", "", fmt.Errorf("%w: proof header carries no jwk", ErrBadProof)
	}
	// A DPoP proof's jwk is the public half. A private key in the header is not a proof of
	// possession and must be refused rather than silently ignored by the JWK subset we parse.
	if hasJWKMember(header.JWK, "d") {
		return "", "", fmt.Errorf("%w: the proof jwk carries a private key", ErrBadProof)
	}
	var jwk protocol.JWK
	if err := json.Unmarshal(header.JWK, &jwk); err != nil {
		return "", "", fmt.Errorf("%w: unreadable proof jwk: %v", ErrBadProof, err)
	}
	pub, err := jwkToECDSA(jwk)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrBadProof, err)
	}
	if err := verifyES256(pub, parts.signingInput, parts.signature); err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrBadProof, err)
	}

	var claims struct {
		Htm string `json:"htm"`
		Htu string `json:"htu"`
		Iat int64  `json:"iat"`
		Jti string `json:"jti"`
		Ath string `json:"ath"`
	}
	if err := json.Unmarshal(parts.payload, &claims); err != nil {
		return "", "", fmt.Errorf("%w: unreadable proof claims: %v", ErrBadProof, err)
	}
	if claims.Htm != r.Method {
		return "", "", fmt.Errorf("%w: proof htm is %q, want %q", ErrBadProof, claims.Htm, r.Method)
	}
	if want := requestHTU(r); claims.Htu != want {
		return "", "", fmt.Errorf("%w: proof htu is %q, want %q", ErrBadProof, claims.Htu, want)
	}
	if claims.Jti == "" {
		return "", "", fmt.Errorf("%w: proof carries no jti", ErrBadProof)
	}
	if claims.Iat == 0 || now.Before(time.Unix(claims.Iat, 0).Add(-dpopMaxSkew)) ||
		now.After(time.Unix(claims.Iat, 0).Add(dpopMaxSkew)) {
		return "", "", fmt.Errorf("%w: proof iat is missing or outside the ±%s window", ErrBadProof, dpopMaxSkew)
	}
	if subtle.ConstantTimeCompare([]byte(claims.Ath), []byte(athOf(accessToken))) != 1 {
		return "", "", fmt.Errorf("%w: proof ath does not hash this access token", ErrBadProof)
	}
	thumbprint, err := jwk.Thumbprint()
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrBadProof, err)
	}
	if subtle.ConstantTimeCompare([]byte(thumbprint), []byte(expectedJKT)) != 1 {
		return "", "", fmt.Errorf("%w: the proof key does not match the token's cnf.jkt", ErrBadProof)
	}
	return thumbprint, claims.Jti, nil
}

// jwsHeader is the protected header shared by the access token and the DPoP proof. A token carries
// alg/typ/kid; a proof carries alg/typ/jwk.
type jwsHeader struct {
	Alg string          `json:"alg"`
	Typ string          `json:"typ"`
	Kid string          `json:"kid"`
	JWK json.RawMessage `json:"jwk"`
}

// jws is a parsed compact JWS: the decoded header and payload, the exact signing input, and the
// signature bytes.
type jws struct {
	header       []byte
	payload      []byte
	signingInput []byte
	signature    []byte
}

// splitCompactJWS parses the three dot-separated parts. It does not interpret them.
func splitCompactJWS(token string) (jws, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return jws{}, errors.New("not a compact JWS (want three dot-separated parts)")
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return jws{}, fmt.Errorf("header is not base64url: %w", err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return jws{}, fmt.Errorf("payload is not base64url: %w", err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return jws{}, fmt.Errorf("signature is not base64url: %w", err)
	}
	return jws{
		header:       header,
		payload:      payload,
		signingInput: []byte(parts[0] + "." + parts[1]),
		signature:    signature,
	}, nil
}

// verifyES256 verifies a JWS ECDSA P-256 signature over the signing input. The JOSE signature is
// the fixed-width R||S encoding, not the ASN.1 form x509 uses.
func verifyES256(pub *ecdsa.PublicKey, signingInput, signature []byte) error {
	if len(signature) != 64 {
		return fmt.Errorf("ES256 signature is %d bytes, want 64", len(signature))
	}
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:])
	digest := sha256.Sum256(signingInput)
	if !ecdsa.Verify(pub, digest[:], r, s) {
		return errors.New("ES256 signature does not verify")
	}
	return nil
}

// jwkToECDSA converts the EC public JWK of a proof into a verifiable key. Anything but P-256 is
// refused rather than coerced, because an ES256 proof is only meaningful over that curve.
func jwkToECDSA(j protocol.JWK) (*ecdsa.PublicKey, error) {
	if j.Kty != "EC" || j.Crv != "P-256" {
		return nil, fmt.Errorf("proof jwk is %s/%s, want EC/P-256", j.Kty, j.Crv)
	}
	xb, err := base64.RawURLEncoding.DecodeString(j.X)
	if err != nil {
		return nil, fmt.Errorf("proof jwk x is not base64url: %w", err)
	}
	yb, err := base64.RawURLEncoding.DecodeString(j.Y)
	if err != nil {
		return nil, fmt.Errorf("proof jwk y is not base64url: %w", err)
	}
	x := new(big.Int).SetBytes(xb)
	y := new(big.Int).SetBytes(yb)
	curve := elliptic.P256()
	if !curve.IsOnCurve(x, y) {
		return nil, errors.New("proof jwk point is not on P-256")
	}
	return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
}

// hasJWKMember reports whether the raw JWK object carries the named member. It is used only to
// reject a private "d" that the public-key subset would otherwise ignore.
func hasJWKMember(raw json.RawMessage, member string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	_, ok := m[member]
	return ok
}

// audienceContains reports whether the aud claim (a string or an array of strings) names want.
func audienceContains(raw json.RawMessage, want string) bool {
	if len(raw) == 0 || want == "" {
		return false
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return single == want
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		for _, v := range list {
			if v == want {
				return true
			}
		}
	}
	return false
}

// athOf returns base64url(sha256(access-token)) without padding, the RFC 9449 ath value.
func athOf(accessToken string) string {
	sum := sha256.Sum256([]byte(accessToken))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// requestHTU reconstructs the HTTP URI of the request without query or fragment, which is what the
// proof's htu claim must equal. The edge terminates TLS, so the scheme is taken from the forwarded
// proto when the edge supplies it and from the connection otherwise; the host honours a forwarded
// host for the same reason.
func requestHTU(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); p != "" {
		scheme = p
	}
	host := r.Host
	if h := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); h != "" {
		host = h
	}
	path := "/"
	if r.URL != nil {
		if p := r.URL.EscapedPath(); p != "" {
			path = p
		}
	}
	return scheme + "://" + host + path
}

// dpopAccessToken extracts the value of "Authorization: DPoP <token>". The scheme is compared
// case-insensitively, as HTTP schemes are.
func dpopAccessToken(r *http.Request) (string, bool) {
	value := strings.TrimSpace(r.Header.Get(protocol.HeaderAuthorization))
	scheme, token, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(scheme, "DPoP") {
		return "", false
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", false
	}
	return token, true
}
