// Package dpop verifies an RFC 9449 proof of possession: a compact ES256 JWS whose protected header
// carries the public key (`typ: dpop+jwt`, `jwk`), whose signature proves the caller holds the
// private half, and whose claims bind the proof to one HTTP method and URL (and, for a resource
// request, to one access token through `ath`).
//
// It is shared by enrolment (a fresh dpop device proves possession of the key it is registering) and
// by the token endpoint (the device proves possession of the key the access token will be bound to).
package dpop

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/jose"
)

// DefaultSkew bounds how far a proof's `iat` may be from the server's clock. RFC 9449 leaves the
// window to the server; five minutes matches the short access-token life. It is the only replay
// bound this build enforces, because the jti is not persisted (see the store's note on
// ops.dpop_replay).
const DefaultSkew = 5 * time.Minute

// Proof is the verified result: the public key the proof demonstrated possession of, and its
// RFC 7638 thumbprint, which is the value the issued access token is bound to.
type Proof struct {
	JWK        protocol.JWK
	Thumbprint string
	JTI        string
	IAT        time.Time
}

// Verify checks a proof against the request method, URL and (optionally) access token.
//
// accessToken is empty at the token endpoint, where there is no token yet and RFC 9449 omits `ath`.
// When it is non-empty -- a future resource request -- the proof must carry
// `ath` = base64url(sha256(ascii(access token))), which is what prevents a proof captured for one
// request from being replayed against another token.
func Verify(compact, htm, htu, accessToken string, now time.Time, skew time.Duration) (Proof, error) {
	if skew <= 0 {
		skew = DefaultSkew
	}
	j, err := jose.Parse(compact)
	if err != nil {
		return Proof{}, err
	}
	if typ, _ := j.HeaderString("typ"); typ != jose.TypDPoP {
		return Proof{}, fmt.Errorf("%w: typ %q, want %q", jose.ErrMalformed, typ, jose.TypDPoP)
	}
	k, err := j.HeaderJWK()
	if err != nil {
		return Proof{}, err
	}
	pub, err := jose.PublicFromJWK(k)
	if err != nil {
		return Proof{}, err
	}
	if err := j.Verify(pub); err != nil {
		return Proof{}, err
	}
	gotHTM, _ := j.Claims.String("htm")
	if !strings.EqualFold(gotHTM, htm) {
		return Proof{}, fmt.Errorf("dpop: proof htm %q does not match %q", gotHTM, htm)
	}
	gotHTU, _ := j.Claims.String("htu")
	if gotHTU != htu {
		return Proof{}, fmt.Errorf("dpop: proof htu %q does not match %q", gotHTU, htu)
	}
	jti, ok := j.Claims.String("jti")
	if !ok || jti == "" {
		return Proof{}, fmt.Errorf("dpop: proof carries no jti")
	}
	iatUnix, ok := j.Claims.Int64("iat")
	if !ok {
		return Proof{}, fmt.Errorf("dpop: proof carries no iat")
	}
	iat := time.Unix(iatUnix, 0).UTC()
	if d := now.Sub(iat); d > skew || d < -skew {
		return Proof{}, fmt.Errorf("dpop: proof iat %s is outside the %s window", iat.Format(time.RFC3339), skew)
	}
	if accessToken != "" {
		want := base64.RawURLEncoding.EncodeToString(sum256(accessToken))
		got, ok := j.Claims.String("ath")
		if !ok || got != want {
			return Proof{}, fmt.Errorf("dpop: proof ath does not bind to the presented access token")
		}
	}
	thumb, err := k.Thumbprint()
	if err != nil {
		return Proof{}, fmt.Errorf("dpop: thumbprint: %w", err)
	}
	return Proof{JWK: k, Thumbprint: thumb, JTI: jti, IAT: iat}, nil
}

// HTU computes the RFC 9449 `htu` for a request: scheme://host/path, honouring the reverse-proxy
// headers the edge sets. The device signs the URL it called; the origin reconstructs the same URL.
func HTU(r *http.Request) string {
	scheme := "https"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = strings.Split(proto, ",")[0]
	} else if r.TLS == nil {
		scheme = "http"
	}
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = strings.Split(fwd, ",")[0]
	}
	path := r.URL.Path
	return scheme + "://" + host + path
}

func sum256(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
