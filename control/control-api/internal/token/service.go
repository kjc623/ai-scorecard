// Package token implements POST /v1/token: RFC 7523 (a signed assertion identifies the device) plus
// RFC 9449 (a DPoP proof binds the request, and the issued token is bound to the proof's key).
//
// The assertion is a compact ES256 JWS signed by the device's registered key; its `sub` is the
// device_id and its `tenant_id` names the tenant the credential lives in, which the server needs to
// open the row-level-security session before it can read that credential. The tenant claim is not
// trusted on sight: the signature is checked against the credential stored under it, so a forged
// tenant cannot select a key it does not hold.
//
// The access token this service issues is short-lived and sender-constrained. Its header is exactly
// `{"alg":"ES256","typ":"at+jwt","kid":...}`; its claims are iss, aud, sub, tenant_id, device_id,
// credential_id, cnf.jkt, iat, exp and jti. A token that is not DPoP-bound is a bearer token, which
// ADR 0005 forbids.
package token

import (
	"context"
	"crypto/ecdsa"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/dpop"
	"github.com/shadow-ai-capture/control-api/internal/jose"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

// GrantTypeJWTBearer is the RFC 7523 grant type this endpoint accepts.
const GrantTypeJWTBearer = "urn:ietf:params:oauth:grant-type:jwt-bearer"

// Config is the service's resolved settings.
type Config struct {
	// Issuer and Audience are the `iss` and `aud` of the issued token (and what the verifier expects).
	Issuer   string
	Audience string
	// TokenTTL is the access-token life. Short by design: a stolen token is useful only until it
	// expires, and the device can mint another because it holds the key.
	TokenTTL time.Duration
	// ProofSkew bounds how far a proof's iat may be from the server clock.
	ProofSkew time.Duration
	// Region is the deployment's pinned region for the §12 fail-closed check.
	Region string
	Now    func() time.Time
}

// Service issues DPoP-bound access tokens.
type Service struct {
	store store.Store
	key   *ecdsa.PrivateKey
	kid   string
	cfg   Config
}

// New builds the token service. key is required: refusing to start without it is the point, because
// an endpoint that silently issued unsigned or locally-keyed tokens would be a different authority
// from the one the deployment configured.
func New(st store.Store, key *ecdsa.PrivateKey, cfg Config) (*Service, error) {
	if st == nil {
		return nil, errors.New("token: store is required")
	}
	if key == nil {
		return nil, errors.New("token: a signing key is required")
	}
	if cfg.TokenTTL <= 0 {
		cfg.TokenTTL = 15 * time.Minute
	}
	if cfg.ProofSkew <= 0 {
		cfg.ProofSkew = dpop.DefaultSkew
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	jwk, err := jose.JWKFromPublic(&key.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("token: signing key is unusable: %w", err)
	}
	kid, err := jwk.Thumbprint()
	if err != nil {
		return nil, fmt.Errorf("token: signing key thumbprint: %w", err)
	}
	return &Service{store: st, key: key, kid: kid, cfg: cfg}, nil
}

// KeyID is the `kid` the issued tokens carry: the RFC 7638 thumbprint of the signing key, so a
// rotation changes the kid and a verifier can select the right key without a registry.
func (s *Service) KeyID() string { return s.kid }

// Issue authenticates the assertion and the proof and returns the access token.
func (s *Service) Issue(ctx context.Context, req protocol.TokenRequest, proofCompact, htm, htu string) (protocol.TokenResponse, error) {
	now := s.cfg.Now().UTC()
	if req.GrantType != GrantTypeJWTBearer {
		return protocol.TokenResponse{}, apierr.New(400, apierr.CodeInvalidRequest,
			"grant_type must be "+GrantTypeJWTBearer)
	}
	assertion, err := jose.Parse(req.Assertion)
	if err != nil {
		return protocol.TokenResponse{}, apierr.New(401, apierr.CodeInvalidAssertion,
			"the assertion is not a compact ES256 JWS")
	}
	deviceID, ok := assertion.Claims.String("sub")
	if !ok || !store.IsUUID(deviceID) {
		return protocol.TokenResponse{}, apierr.New(401, apierr.CodeInvalidAssertion,
			"the assertion sub must be a device uuid")
	}
	if req.DeviceID != "" && req.DeviceID != deviceID {
		return protocol.TokenResponse{}, apierr.New(401, apierr.CodeInvalidAssertion,
			"the assertion sub does not match the requested device_id")
	}
	tenantID, ok := assertion.Claims.String("tenant_id")
	if !ok || !store.IsUUID(tenantID) {
		return protocol.TokenResponse{}, apierr.New(401, apierr.CodeInvalidAssertion,
			"the assertion must name its tenant in the tenant_id claim")
	}

	if _, err := s.checkTenant(ctx, tenantID); err != nil {
		return protocol.TokenResponse{}, err
	}

	cred, err := s.store.DeviceCredentialByDevice(ctx, tenantID, deviceID)
	if errors.Is(err, store.ErrCredentialUnknown) {
		return protocol.TokenResponse{}, apierr.New(401, apierr.CodeInvalidAssertion,
			"no live device credential is registered for this device")
	}
	if err != nil {
		return protocol.TokenResponse{}, apierr.Internal(fmt.Errorf("device credential: %w", err))
	}
	if cred.Type != protocol.AuthModeDPoP {
		return protocol.TokenResponse{}, apierr.New(401, apierr.CodeInvalidAssertion,
			"this device credential is not a dpop credential")
	}
	if err := cred.Active(now); err != nil {
		return protocol.TokenResponse{}, apierr.New(401, apierr.CodeRevokedDevice,
			"the device credential is revoked or expired")
	}
	if len(cred.PublicKeyJWK) == 0 {
		return protocol.TokenResponse{}, apierr.Internal(errors.New("dpop credential carries no jwk"))
	}
	var registered protocol.JWK
	if err := json.Unmarshal(cred.PublicKeyJWK, &registered); err != nil {
		return protocol.TokenResponse{}, apierr.Internal(fmt.Errorf("decode registered jwk: %w", err))
	}
	pub, err := jose.PublicFromJWK(registered)
	if err != nil {
		return protocol.TokenResponse{}, apierr.Internal(fmt.Errorf("registered jwk: %w", err))
	}
	// The assertion is trusted only after this point: the signature is checked against the key the
	// credential registered, which is the tenant-scoped fact the tenant claim selected.
	if err := assertion.Verify(pub); err != nil {
		return protocol.TokenResponse{}, apierr.New(401, apierr.CodeInvalidAssertion,
			"the assertion signature does not verify against the registered device key")
	}

	proof, err := dpop.Verify(proofCompact, htm, htu, "", now, s.cfg.ProofSkew)
	if err != nil {
		return protocol.TokenResponse{}, apierr.New(401, apierr.CodeInvalidProof,
			"the dpop proof did not verify")
	}
	if subtle.ConstantTimeCompare([]byte(proof.Thumbprint), []byte(cred.PublicKeyThumbprint)) != 1 {
		return protocol.TokenResponse{}, apierr.New(401, apierr.CodeInvalidProof,
			"the dpop proof key is not the registered device key")
	}

	jti, err := store.NewUUID()
	if err != nil {
		return protocol.TokenResponse{}, apierr.Internal(fmt.Errorf("mint token jti: %w", err))
	}
	exp := now.Add(s.cfg.TokenTTL)
	header := map[string]any{"alg": jose.AlgES256, "typ": jose.TypAccessToken, "kid": s.kid}
	claims := map[string]any{
		"iss":           s.cfg.Issuer,
		"aud":           s.cfg.Audience,
		"sub":           deviceID,
		"tenant_id":     tenantID,
		"device_id":     deviceID,
		"credential_id": cred.CredentialID,
		"cnf":           map[string]any{"jkt": proof.Thumbprint},
		"iat":           now.Unix(),
		"exp":           exp.Unix(),
		"jti":           jti,
	}
	compact, err := jose.SignES256(header, claims, s.key)
	if err != nil {
		return protocol.TokenResponse{}, apierr.Internal(fmt.Errorf("sign access token: %w", err))
	}
	resp := protocol.TokenResponse{
		AccessToken: compact,
		TokenType:   protocol.TokenTypeDPoP,
		ExpiresIn:   int(s.cfg.TokenTTL / time.Second),
		ServerTime:  now,
	}
	if err := resp.Validate(); err != nil {
		return protocol.TokenResponse{}, apierr.Internal(err)
	}
	return resp, nil
}

func (s *Service) checkTenant(ctx context.Context, tenantID string) (store.Tenant, error) {
	t, err := s.store.Tenant(ctx, tenantID)
	if errors.Is(err, store.ErrUnknownTenant) {
		return store.Tenant{}, apierr.New(403, apierr.CodeUnknownTenant,
			"the authenticated tenant is unknown to this deployment")
	}
	if err != nil {
		return store.Tenant{}, apierr.Internal(fmt.Errorf("tenant: %w", err))
	}
	if !t.Active() {
		return store.Tenant{}, apierr.New(403, apierr.CodeUnknownTenant, "the tenant is not active")
	}
	if s.cfg.Region != "" && t.ResidencyRegion != "" && t.ResidencyRegion != s.cfg.Region {
		return store.Tenant{}, apierr.New(403, apierr.CodeRegionMismatch,
			"this deployment is not the tenant's pinned region")
	}
	return t, nil
}
