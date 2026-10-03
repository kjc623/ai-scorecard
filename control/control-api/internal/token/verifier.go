package token

import (
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/jose"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

// AccessToken is the verified content of an issued token. It is what a re-enrolment authenticates
// with, and what a future resource server would read after checking the DPoP proof's `ath`.
type AccessToken struct {
	Subject      string
	TenantID     string
	DeviceID     string
	CredentialID string
	JKT          string
	JTI          string
	IssuedAt     time.Time
	ExpiresAt    time.Time
}

// Verifier checks the compact access token this service issues. It is exposed so the transport can
// authenticate a re-enrolment that presents its existing credential as a token-and-proof rather than
// as a certificate. The verifier checks the signature, the token type, the lifetime and, when
// configured, the issuer and audience.
type Verifier struct {
	key      *ecdsa.PublicKey
	issuer   string
	audience string
	now      func() time.Time
	leeway   time.Duration
}

// NewVerifier builds a verifier for the token-signing key's public half.
func NewVerifier(pub *ecdsa.PublicKey, issuer, audience string, now func() time.Time) (*Verifier, error) {
	if pub == nil {
		return nil, errors.New("token: verifier needs a public key")
	}
	if now == nil {
		now = time.Now
	}
	return &Verifier{key: pub, issuer: issuer, audience: audience, now: now, leeway: time.Minute}, nil
}

// Verify parses and fully checks a compact access token.
func (v *Verifier) Verify(compact string) (AccessToken, error) {
	j, err := jose.Parse(compact)
	if err != nil {
		return AccessToken{}, err
	}
	if typ, _ := j.HeaderString("typ"); typ != jose.TypAccessToken {
		return AccessToken{}, fmt.Errorf("%w: typ %q, want %q", jose.ErrMalformed, typ, jose.TypAccessToken)
	}
	if err := j.Verify(v.key); err != nil {
		return AccessToken{}, err
	}
	sub, ok := j.Claims.String("sub")
	if !ok || !store.IsUUID(sub) {
		return AccessToken{}, fmt.Errorf("%w: sub is not a device uuid", jose.ErrMalformed)
	}
	tenantID, _ := j.Claims.String("tenant_id")
	deviceID, _ := j.Claims.String("device_id")
	credentialID, _ := j.Claims.String("credential_id")
	if !store.IsUUID(tenantID) || !store.IsUUID(deviceID) || !store.IsUUID(credentialID) {
		return AccessToken{}, fmt.Errorf("%w: token is missing a uuid claim", jose.ErrMalformed)
	}
	if v.issuer != "" {
		if iss, _ := j.Claims.String("iss"); iss != v.issuer {
			return AccessToken{}, fmt.Errorf("token: iss %q, want %q", iss, v.issuer)
		}
	}
	if v.audience != "" {
		if aud, _ := j.Claims.String("aud"); aud != v.audience {
			return AccessToken{}, fmt.Errorf("token: aud %q, want %q", aud, v.audience)
		}
	}
	expUnix, ok := j.Claims.Int64("exp")
	if !ok {
		return AccessToken{}, fmt.Errorf("%w: token has no exp", jose.ErrMalformed)
	}
	exp := time.Unix(expUnix, 0).UTC()
	if !v.now().UTC().Before(exp.Add(v.leeway)) {
		return AccessToken{}, errors.New("token: access token has expired")
	}
	iatUnix, _ := j.Claims.Int64("iat")
	var cnf struct {
		JKT string `json:"jkt"`
	}
	if raw, ok := j.Claims["cnf"]; ok {
		_ = json.Unmarshal(raw, &cnf)
	}
	if cnf.JKT == "" {
		return AccessToken{}, fmt.Errorf("%w: token carries no cnf.jkt binding", jose.ErrMalformed)
	}
	jti, _ := j.Claims.String("jti")
	return AccessToken{
		Subject:      sub,
		TenantID:     tenantID,
		DeviceID:     deviceID,
		CredentialID: credentialID,
		JKT:          cnf.JKT,
		JTI:          jti,
		IssuedAt:     time.Unix(iatUnix, 0).UTC(),
		ExpiresAt:    exp,
	}, nil
}

// JKT returns the cnf.jkt binding of a verified token, for a caller that wants to check the proof.
func (t AccessToken) Binding() string { return t.JKT }
