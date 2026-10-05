package auth

// The product access token (contract §2), verified by the vault itself.
//
// query-api forwards the person's `Authorization: Bearer` with every content request. The vault
// does not take query-api's word for who that person is: it is the one component that returns
// content, so it checks the signature, the issuer, its own audience and the clock, and reads the
// tenant, the actor and the roles from the claims. A header that disagrees with the token is
// refused, not merged (TokenAuthenticator).
//
// Standard library only (crypto/ecdsa, crypto/sha256, encoding/json): the module stays offline
// and dependency-free. ES256 is small enough to verify by hand when the rules are fixed:
//
//   - the token is a compact JWS of bounded size, strictly base64url (no padding, canonical);
//   - `alg` is ES256 and nothing else, `typ` is at+jwt, `kid` is named, and any `crit` is
//     refused because this verifier implements no extension;
//   - the signature is the raw 64-byte R||S of RFC 7518 §3.4, never DER;
//   - the key comes only from the configured JWKS — an embedded jwk/jku/x5u is never read — and
//     must be an EC P-256 signing key whose point is on the curve;
//   - `iss` is exact, `aud` names the vault, `exp`/`nbf`/`iat` hold within 60 s of leeway, and
//     the token is no older than ten minutes;
//   - the tenant is a uuid, the actor a printable name, and at least one role a product role.
//
// query-api (src/http/auth.js) applies the same rules through the `jose` library; the two are
// tested against the same shapes of bad token.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// TokenAlg is the one signature algorithm the product issuer uses.
	TokenAlg = "ES256"
	// TokenTyp is RFC 9068's JWT access token type; the issuer's header carries it.
	TokenTyp = "at+jwt"
	// DefaultAudience is the vault's own audience in the product token.
	DefaultAudience = "sac-vault"
	// ClockLeeway bounds clock skew for exp, nbf and iat. The contract allows at most 60 s.
	ClockLeeway = 60 * time.Second
	// MaxTokenAge is the contract's ten-minute lifetime: an older token is refused whatever its
	// exp says, so a revoked session cannot outlive it here.
	MaxTokenAge = 10 * time.Minute
	// MaxTokenBytes bounds what is parsed at all. A product token is a few hundred bytes.
	MaxTokenBytes = 8 << 10

	maxJWKSBytes      = 64 << 10
	maxActorRunes     = 256
	defaultCacheTTL   = 10 * time.Minute
	defaultMinRefetch = 30 * time.Second
	// defaultMaxStale is how long a cached JWKS keeps verifying while every refresh fails. The
	// issuer being briefly unreachable must not stop reads; a key it withdrew must not verify for
	// ever because the withdrawal could not be fetched.
	defaultMaxStale  = time.Hour
	jwksFetchTimeout = 5 * time.Second
)

// knownRoles is the closed product role set (docs/04 §2.2). A role outside it is dropped.
var knownRoles = map[string]bool{"viewer": true, "analyst": true, "content_reader": true, "admin": true}

// TokenClaims are the session facts a verified token carries.
type TokenClaims struct {
	TenantID  string
	Actor     string
	Subject   string
	Roles     []string
	SessionID string
	IdP       string
}

// TokenVerifier verifies product access tokens against the issuer's JWKS.
type TokenVerifier struct {
	Issuer   string
	Audience string
	JWKSURL  string
	// Client fetches the JWKS. Redirects are not followed: the key set is read from exactly the
	// configured URL.
	Client *http.Client
	Now    func() time.Time
	// CacheTTL is how long a fetched JWKS is used before it is refreshed.
	CacheTTL time.Duration
	// MinRefetch is the shortest gap between two fetches, so a stream of forged kids cannot become
	// a stream of fetches. A rotated key is picked up within this window. Zero is the 30 s
	// default; a negative value removes the limit, which only a test wants.
	MinRefetch time.Duration
	// MaxStale bounds how long a JWKS that cannot be refreshed keeps verifying.
	MaxStale time.Duration

	mu          sync.Mutex
	keys        map[string][]*ecdsa.PublicKey
	fetchedAt   time.Time
	lastAttempt time.Time
	lastErr     error
	fetches     int
}

// NewTokenVerifier returns a verifier with the contract's defaults. An empty audience is the vault's
// own, and an empty JWKS URL is {issuer}/.well-known/jwks.json.
func NewTokenVerifier(issuer, audience, jwksURL string) *TokenVerifier {
	if audience == "" {
		audience = DefaultAudience
	}
	if jwksURL == "" {
		jwksURL = DefaultJWKSURL(issuer)
	}
	return &TokenVerifier{Issuer: issuer, Audience: audience, JWKSURL: jwksURL}
}

// DefaultJWKSURL is the contract's default key-set location for an issuer.
func DefaultJWKSURL(issuer string) string {
	return strings.TrimRight(issuer, "/") + "/.well-known/jwks.json"
}

// Fetches reports how many JWKS fetches were attempted. It exists for tests of the rate limit.
func (v *TokenVerifier) Fetches() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.fetches
}

type tokenHeader struct {
	Alg  string          `json:"alg"`
	Typ  string          `json:"typ"`
	Kid  string          `json:"kid"`
	Crit json.RawMessage `json:"crit"`
}

// audience accepts the two shapes RFC 7519 allows: one string or an array of strings.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = []string{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("aud is neither a string nor an array of strings")
	}
	*a = many
	return nil
}

type tokenClaims struct {
	Iss       string          `json:"iss"`
	Aud       audience        `json:"aud"`
	Sub       string          `json:"sub"`
	Exp       *float64        `json:"exp"`
	Nbf       *float64        `json:"nbf"`
	Iat       *float64        `json:"iat"`
	SacTenant string          `json:"sac_tenant"`
	Actor     string          `json:"actor"`
	Roles     json.RawMessage `json:"roles"`
	Sid       json.RawMessage `json:"sid"`
	IdP       json.RawMessage `json:"idp"`
}

// Verify checks one token and returns its session facts, or an error whose text is safe to log:
// it never contains the token.
func (v *TokenVerifier) Verify(ctx context.Context, raw string) (TokenClaims, error) {
	if len(raw) > MaxTokenBytes {
		return TokenClaims{}, errors.New("the bearer is longer than any product token")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return TokenClaims{}, errors.New("the bearer is not a compact JWS")
	}
	b64 := base64.RawURLEncoding.Strict()
	headerJSON, err := b64.DecodeString(parts[0])
	if err != nil {
		return TokenClaims{}, errors.New("the token header is not base64url")
	}
	var h tokenHeader
	if err := json.Unmarshal(headerJSON, &h); err != nil {
		return TokenClaims{}, fmt.Errorf("the token header is not a JSON object: %v", err)
	}
	// Each header rule is checked before any key is looked up, so a token naming the wrong
	// algorithm, or no key, never causes a JWKS fetch.
	if h.Alg != TokenAlg {
		return TokenClaims{}, fmt.Errorf("token alg %q is not %s", h.Alg, TokenAlg)
	}
	if !typIsAccessToken(h.Typ) {
		return TokenClaims{}, fmt.Errorf("token typ %q is not %s", h.Typ, TokenTyp)
	}
	if h.Kid == "" {
		return TokenClaims{}, errors.New("the token header names no kid")
	}
	if h.Crit != nil {
		return TokenClaims{}, errors.New("the token header names a crit extension this verifier does not implement")
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return TokenClaims{}, errors.New("the token signature is not a 64-byte ES256 R||S value")
	}
	payload, err := b64.DecodeString(parts[1])
	if err != nil {
		return TokenClaims{}, errors.New("the token payload is not base64url")
	}

	keys, err := v.keysFor(ctx, h.Kid)
	if err != nil {
		return TokenClaims{}, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	verified := false
	for _, k := range keys {
		if ecdsa.Verify(k, digest[:], r, s) {
			verified = true
			break
		}
	}
	if !verified {
		return TokenClaims{}, errors.New("the token signature does not verify")
	}

	var c tokenClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return TokenClaims{}, fmt.Errorf("the token claims are malformed: %v", err)
	}
	return v.check(c)
}

// check applies the claim rules to a payload whose signature has verified.
func (v *TokenVerifier) check(c tokenClaims) (TokenClaims, error) {
	if c.Iss != v.Issuer {
		return TokenClaims{}, fmt.Errorf("token iss %q is not %q", c.Iss, v.Issuer)
	}
	named := false
	for _, a := range c.Aud {
		if a == v.Audience {
			named = true
			break
		}
	}
	if !named {
		return TokenClaims{}, fmt.Errorf("token aud %v does not name %q", []string(c.Aud), v.Audience)
	}

	// Whole seconds, as jose counts them, so the two verifiers agree on the edges.
	now := float64(v.now().Unix())
	leeway := ClockLeeway.Seconds()
	if c.Exp == nil {
		return TokenClaims{}, errors.New("the token has no exp")
	}
	if *c.Exp <= now-leeway {
		return TokenClaims{}, errors.New("the token has expired")
	}
	if c.Nbf != nil && *c.Nbf > now+leeway {
		return TokenClaims{}, errors.New("the token is not valid yet (nbf)")
	}
	if c.Iat == nil {
		return TokenClaims{}, errors.New("the token has no iat")
	}
	age := now - *c.Iat
	if age-leeway > MaxTokenAge.Seconds() {
		return TokenClaims{}, errors.New("the token is older than the ten-minute lifetime (iat)")
	}
	if age < -leeway {
		return TokenClaims{}, errors.New("the token was issued in the future (iat)")
	}

	out := TokenClaims{
		TenantID: strings.ToLower(strings.TrimSpace(c.SacTenant)),
		Actor:    strings.TrimSpace(c.Actor),
		Subject:  strings.TrimSpace(c.Sub),
	}
	if !looksLikeUUID(out.TenantID) {
		return TokenClaims{}, errors.New("the token's sac_tenant claim is not a tenant uuid")
	}
	if out.Actor == "" || utf8.RuneCountInString(out.Actor) > maxActorRunes || hasControl(out.Actor) {
		return TokenClaims{}, errors.New("the token's actor claim is missing or not a printable name")
	}
	if out.Subject == "" {
		return TokenClaims{}, errors.New("the token names no sub")
	}
	// A role this verifier does not know is dropped: a newer issuer may name one. A token left
	// with no product role is refused. A roles claim that is not an array grants nothing.
	var listed []any
	_ = json.Unmarshal(c.Roles, &listed)
	seen := map[string]bool{}
	for _, item := range listed {
		if role, ok := item.(string); ok && knownRoles[role] && !seen[role] {
			seen[role] = true
			out.Roles = append(out.Roles, role)
		}
	}
	if len(out.Roles) == 0 {
		return TokenClaims{}, errors.New("the token's roles claim names no product role")
	}
	// The session id only correlates audit rows, so a malformed one is dropped, not refused.
	var sid string
	if json.Unmarshal(c.Sid, &sid) == nil && isHexSessionID(sid) {
		out.SessionID = strings.ToLower(sid)
	}
	var idp string
	if json.Unmarshal(c.IdP, &idp) == nil && (idp == "entra" || idp == "oidc") {
		out.IdP = idp
	}
	return out, nil
}

// keysFor returns the published keys for kid, refreshing the cache when it is stale and, once per
// MinRefetch, when the kid is unknown.
func (v *TokenVerifier) keysFor(ctx context.Context, kid string) ([]*ecdsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	if (v.keys == nil || now.Sub(v.fetchedAt) >= v.cacheTTL()) && v.mayFetch(now) {
		v.refresh(ctx, now)
	}
	if v.keys != nil && now.Sub(v.fetchedAt) >= v.maxStale() {
		// Every refresh has failed for longer than a stale set may be trusted. Fail closed.
		v.keys = nil
	}
	if ks := v.keys[kid]; len(ks) > 0 {
		return ks, nil
	}
	// A kid the cache does not hold: a rotation, or a forgery. One refetch, rate-limited.
	if v.mayFetch(now) {
		v.refresh(ctx, now)
		if ks := v.keys[kid]; len(ks) > 0 {
			return ks, nil
		}
	}
	if v.keys == nil {
		return nil, fmt.Errorf("the issuer's JWKS could not be loaded: %v", v.lastErr)
	}
	return nil, fmt.Errorf("token kid %q is not in the issuer's JWKS", kid)
}

func (v *TokenVerifier) mayFetch(now time.Time) bool {
	return v.lastAttempt.IsZero() || now.Sub(v.lastAttempt) >= v.minRefetch()
}

// refresh fetches the JWKS. A failure keeps the previous keys (bounded by MaxStale) and records
// why, so a brief issuer outage does not stop reads.
func (v *TokenVerifier) refresh(ctx context.Context, now time.Time) {
	v.lastAttempt = now
	v.fetches++
	keys, err := v.fetch(ctx)
	if err != nil {
		v.lastErr = err
		return
	}
	v.keys, v.fetchedAt, v.lastErr = keys, now, nil
}

type jwk struct {
	Kty    string   `json:"kty"`
	Crv    string   `json:"crv"`
	X      string   `json:"x"`
	Y      string   `json:"y"`
	Kid    string   `json:"kid"`
	Use    string   `json:"use"`
	Alg    string   `json:"alg"`
	KeyOps []string `json:"key_ops"`
}

func (v *TokenVerifier) fetch(ctx context.Context) (map[string][]*ecdsa.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, jwksFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.JWKSURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, application/jwk-set+json")
	resp, err := v.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", v.JWKSURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching %s: status %d", v.JWKSURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", v.JWKSURL, err)
	}
	if len(body) > maxJWKSBytes {
		return nil, fmt.Errorf("the JWKS at %s is larger than %d bytes", v.JWKSURL, maxJWKSBytes)
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("the JWKS at %s is not JSON: %v", v.JWKSURL, err)
	}
	out := map[string][]*ecdsa.PublicKey{}
	for _, k := range set.Keys {
		// A key this verifier could not use for ES256 is skipped, not fatal: the set may carry
		// other keys for other purposes.
		if pub, ok := es256Key(k); ok {
			out[k.Kid] = append(out[k.Kid], pub)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("the JWKS at %s holds no EC P-256 signing key with a kid", v.JWKSURL)
	}
	return out, nil
}

// es256Key turns a JWK into a P-256 public key when it is one, checking the point is on the curve.
func es256Key(k jwk) (*ecdsa.PublicKey, bool) {
	if k.Kid == "" || k.Kty != "EC" || k.Crv != "P-256" {
		return nil, false
	}
	if (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != TokenAlg) {
		return nil, false
	}
	if k.KeyOps != nil {
		verify := false
		for _, op := range k.KeyOps {
			verify = verify || op == "verify"
		}
		if !verify {
			return nil, false
		}
	}
	x, errX := base64.RawURLEncoding.DecodeString(k.X)
	y, errY := base64.RawURLEncoding.DecodeString(k.Y)
	if errX != nil || errY != nil || len(x) != 32 || len(y) != 32 {
		return nil, false
	}
	point := append(append([]byte{4}, x...), y...)
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point)
	if err != nil {
		return nil, false
	}
	return pub, true
}

func (v *TokenVerifier) client() *http.Client {
	if v.Client != nil {
		return v.Client
	}
	return &http.Client{
		Timeout: jwksFetchTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (v *TokenVerifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func (v *TokenVerifier) cacheTTL() time.Duration {
	if v.CacheTTL > 0 {
		return v.CacheTTL
	}
	return defaultCacheTTL
}

func (v *TokenVerifier) minRefetch() time.Duration {
	if v.MinRefetch > 0 {
		return v.MinRefetch
	}
	if v.MinRefetch < 0 {
		return 0
	}
	return defaultMinRefetch
}

func (v *TokenVerifier) maxStale() time.Duration {
	if v.MaxStale > 0 {
		return v.MaxStale
	}
	return defaultMaxStale
}

// typIsAccessToken compares typ as RFC 7515 §4.1.9 says: case-insensitively, with an omitted
// "application/" prefix.
func typIsAccessToken(typ string) bool {
	t := strings.ToLower(strings.TrimSpace(typ))
	return strings.TrimPrefix(t, "application/") == TokenTyp
}

func isHexSessionID(s string) bool {
	if len(s) < 8 || len(s) > 64 {
		return false
	}
	for _, c := range strings.ToLower(s) {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func hasControl(s string) bool {
	for _, c := range s {
		if c < 0x20 || c == 0x7f {
			return true
		}
	}
	return false
}
