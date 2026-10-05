package testrig

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/content-vault/internal/auth"
)

// Issuer stands in for control-api's token issuer (contract §2): a P-256 key generated per test (no
// key is committed), a JWKS served over HTTP that counts its fetches, and a minter that builds the
// compact JWS by hand, so a test can make exactly the malformed token it names — a wrong alg, a
// missing typ, a DER signature, a foreign key under a published kid.
type Issuer struct {
	URL    string
	server *httptest.Server
	key    *ecdsa.PrivateKey

	mu        sync.Mutex
	published []map[string]any
	fetches   int
	status    int
}

// Absent, as a claim or header override, removes that member.
var Absent = struct{}{}

// IssuerSubject is the `sub` every minted token carries unless overridden.
const IssuerSubject = "5d1c0de0-0000-4000-8000-00000000c0aa:idp-subject-1"

// NewIssuer starts the issuer's JWKS endpoint for the life of the test.
func NewIssuer(t *testing.T) *Issuer {
	t.Helper()
	i := &Issuer{key: newP256(t), status: http.StatusOK}
	i.published = []map[string]any{publicJWK(&i.key.PublicKey, "k1")}
	i.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/jwks.json" {
			http.NotFound(w, r)
			return
		}
		i.mu.Lock()
		i.fetches++
		status, keys := i.status, append([]map[string]any(nil), i.published...)
		i.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	}))
	t.Cleanup(i.server.Close)
	i.URL = i.server.URL
	return i
}

// Verifier returns a verifier for this issuer with the vault's audience and the default limits.
func (i *Issuer) Verifier() *auth.TokenVerifier {
	return auth.NewTokenVerifier(i.URL, "", "")
}

// Fetches is how many times the JWKS was served or refused.
func (i *Issuer) Fetches() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.fetches
}

// SetJWKSStatus makes the JWKS endpoint answer with status, for an issuer outage.
func (i *Issuer) SetJWKSStatus(status int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.status = status
}

// Publish adds a new key under kid — a rotation — and returns its private half.
func (i *Issuer) Publish(t *testing.T, kid string) *ecdsa.PrivateKey {
	t.Helper()
	k := newP256(t)
	i.mu.Lock()
	defer i.mu.Unlock()
	i.published = append(i.published, publicJWK(&k.PublicKey, kid))
	return k
}

// PublishJWK adds an arbitrary member to the JWKS, for the key-shape tests.
func (i *Issuer) PublishJWK(member map[string]any) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.published = append(i.published, member)
}

// PublishedX is the published key's x coordinate, the classic HS256 alg-confusion secret.
func (i *Issuer) PublishedX() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.published[0]["x"].(string)
}

// ForeignKey is a P-256 key the issuer never published.
func ForeignKey(t *testing.T) *ecdsa.PrivateKey { return newP256(t) }

// Claims is the claim set a valid token carries, with overrides applied.
func (i *Issuer) Claims(overrides map[string]any) map[string]any {
	now := time.Now().Unix()
	c := map[string]any{
		"iss": i.URL, "aud": []string{"sac-query", "sac-vault", "sac-control"},
		"sub": IssuerSubject, "sac_tenant": TenantID, "actor": "reader@lab.test",
		"roles": []string{"content_reader"}, "idp": "oidc", "sid": "0a1b2c3d4e5f6071",
		"iat": now, "exp": now + 300, "jti": "jti-1",
	}
	return apply(c, overrides)
}

// MintOptions shape the token's header and signature.
type MintOptions struct {
	Header map[string]any
	Key    *ecdsa.PrivateKey
	DER    bool
	HMAC   []byte
	RSA    *rsa.PrivateKey
}

// Mint builds a compact JWS over Claims(overrides).
func (i *Issuer) Mint(t *testing.T, overrides map[string]any, o MintOptions) string {
	t.Helper()
	header := apply(map[string]any{"alg": "ES256", "typ": "at+jwt", "kid": "k1"}, o.Header)
	input := segment(t, header) + "." + segment(t, i.Claims(overrides))
	digest := sha256.Sum256([]byte(input))
	var sig []byte
	switch {
	case o.HMAC != nil:
		m := hmac.New(sha256.New, o.HMAC)
		m.Write([]byte(input))
		sig = m.Sum(nil)
	case o.RSA != nil:
		s, err := rsa.SignPKCS1v15(rand.Reader, o.RSA, crypto.SHA256, digest[:])
		if err != nil {
			t.Fatalf("rsa sign: %v", err)
		}
		sig = s
	case header["alg"] == "none":
		sig = nil
	default:
		key := o.Key
		if key == nil {
			key = i.key
		}
		if o.DER {
			s, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
			if err != nil {
				t.Fatalf("ecdsa sign: %v", err)
			}
			sig = s
		} else {
			r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
			if err != nil {
				t.Fatalf("ecdsa sign: %v", err)
			}
			sig = make([]byte, 64)
			r.FillBytes(sig[:32])
			s.FillBytes(sig[32:])
		}
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func apply(base, overrides map[string]any) map[string]any {
	for k, v := range overrides {
		if v == Absent {
			delete(base, k)
			continue
		}
		base[k] = v
	}
	return base
}

func segment(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func newP256(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a P-256 key: %v", err)
	}
	return k
}

func publicJWK(pub *ecdsa.PublicKey, kid string) map[string]any {
	b, err := pub.Bytes()
	if err != nil {
		panic(err)
	}
	return map[string]any{
		"kty": "EC", "crv": "P-256", "kid": kid, "use": "sig", "alg": "ES256",
		"x": base64.RawURLEncoding.EncodeToString(b[1:33]),
		"y": base64.RawURLEncoding.EncodeToString(b[33:65]),
	}
}

// BearerHeaders are the headers query-api sends with a forwarded token, agreeing with it.
func BearerHeaders(token string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("X-Sac-Service", "query-api")
	return h
}

// JoinRoles is the X-Sac-Roles spelling query-api uses.
func JoinRoles(roles ...string) string { return strings.Join(roles, ",") }
