// Package enrol implements POST /v1/enrol (docs/02-ingest-and-transport.md §5.1; ADR 0020
// decisions 3 and 4).
//
// The exchange is mode-agnostic: an x509 device submits a PKCS#10 CSR, a dpop device submits its
// public JWK and a proof of possession, and the private key never leaves the device. The tenant
// always comes from the bootstrap token or the current credential, never from the request body
// (§12); the region pin is checked here and fails closed; and hardware_identity_hash is the C11
// idempotency key, so a re-image returns the existing device_id and a revoked device is refused a
// fresh identity.
package enrol

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/dpop"
	"github.com/shadow-ai-capture/control-api/internal/jose"
	"github.com/shadow-ai-capture/control-api/internal/signer"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

// Config is the service's resolved settings.
type Config struct {
	// Region is the region this deployment serves. Empty disables the region check, visibly: the
	// deployment is then single-region (docs/02 §12).
	Region string
	// CredentialTTL is the life of an issued dpop credential and the upper bound for an x509 leaf.
	CredentialTTL time.Duration
	// ProofSkew bounds how far a proof's iat may be from the server clock.
	ProofSkew time.Duration
	Now       func() time.Time
}

// Service is the enrolment path.
type Service struct {
	store  store.Store
	signer signer.CertificateSigner
	cfg    Config
}

// New builds the service. It refuses a nil store or signer rather than discovering the absence on the
// first device request.
func New(st store.Store, sg signer.CertificateSigner, cfg Config) (*Service, error) {
	if st == nil {
		return nil, errors.New("enrol: store is required")
	}
	if sg == nil {
		return nil, errors.New("enrol: certificate signer is required")
	}
	if cfg.CredentialTTL <= 0 {
		cfg.CredentialTTL = 90 * 24 * time.Hour
	}
	if cfg.ProofSkew <= 0 {
		cfg.ProofSkew = dpop.DefaultSkew
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Service{store: st, signer: sg, cfg: cfg}, nil
}

// Current is a device already authenticated by its existing credential, for a rotation re-enrolment.
// The transport resolves it (from a forwarded certificate or an access token and proof) and passes
// it in; the service re-checks its lifecycle state against the store. KeyThumbprint is the binding
// the transport proved possession of: a dpop re-enrolment may only re-register that same key, since
// the one proof header is spent on authenticating the current credential.
type Current struct {
	TenantID      string
	DeviceID      string
	CredentialID  string
	KeyThumbprint string
}

// Input is one /v1/enrol call. Exactly one of a body token or Current authenticates it.
type Input struct {
	Request protocol.EnrolmentRequest
	// Current is set for a re-enrolment authenticated with the existing credential instead of a
	// bootstrap token. Tests that call the service directly set it; the transport sets ResolveCurrent.
	Current *Current
	// ResolveCurrent resolves the current credential from the HTTP request. It is called only after
	// the body has passed schema validation, so a malformed body is a 400 even when the request also
	// carries no usable credential.
	ResolveCurrent func() (*Current, error)
	// Proof is the compact DPoP proof a fresh dpop enrolment presents to prove possession of the
	// private key behind Request.JWK. It is required for dpop and ignored for x509, where the CSR's
	// own signature is the proof.
	Proof string
	// HTM and HTU are the method and URL the proof was bound to, so a proof cannot be replayed
	// against a different request.
	HTM string
	HTU string
}

// Enrol performs the exchange and returns the credential.
func (s *Service) Enrol(ctx context.Context, in Input) (protocol.EnrolmentResponse, error) {
	req := in.Request
	now := s.cfg.Now().UTC()

	if req.SchemaVersion != protocol.EnrolmentSchemaVersion {
		return protocol.EnrolmentResponse{}, apierr.Detailed(400, apierr.CodeUnsupportedSchemaVersion,
			"the enrolment schema_version is not supported",
			map[string]any{"supported": []string{protocol.EnrolmentSchemaVersion}})
	}
	if !req.Mode.Valid() {
		return protocol.EnrolmentResponse{}, apierr.New(400, apierr.CodeSchemaViolation,
			"mode must be one of x509 or dpop")
	}
	// The closed OS set, kept in step with the ops.device CHECK constraint. Linux is a supported
	// endpoint platform, not a development accommodation: the agent builds and runs there, and the
	// enrolment vocabulary names it explicitly so a typo still fails here rather than in the schema.
	switch req.Device.OS {
	case "windows", "macos", "linux":
	default:
		return protocol.EnrolmentResponse{}, apierr.New(400, apierr.CodeSchemaViolation,
			"device.os must be windows, macos or linux")
	}

	var (
		tenant     store.Tenant
		device     store.Device
		tokenHash  string
		reenrolled bool
	)

	switch {
	case req.EnrolmentToken != "":
		t, err := s.resolveToken(ctx, req.EnrolmentToken, now)
		if err != nil {
			return protocol.EnrolmentResponse{}, err
		}
		if t.HardwareIdentityHash != "" && t.HardwareIdentityHash != req.Device.HardwareIdentityHash {
			return protocol.EnrolmentResponse{}, apierr.New(401, apierr.CodeEnrolmentTokenInvalid,
				"the enrolment token is bound to a different hardware identity")
		}
		tenant, err = s.checkTenant(ctx, t.TenantID)
		if err != nil {
			return protocol.EnrolmentResponse{}, err
		}
		device, reenrolled, err = s.reconcileDevice(ctx, tenant, req, now)
		if err != nil {
			return protocol.EnrolmentResponse{}, err
		}
		tokenHash = t.TokenHash

	case in.Current != nil || in.ResolveCurrent != nil:
		current := in.Current
		if current == nil {
			var err error
			current, err = in.ResolveCurrent()
			if err != nil {
				return protocol.EnrolmentResponse{}, err
			}
		}
		if current == nil {
			return protocol.EnrolmentResponse{}, apierr.New(401, apierr.CodeRevokedDevice,
				"a re-enrolment must present the current device credential")
		}
		var err error
		tenant, device, err = s.currentDevice(ctx, current, req)
		if err != nil {
			return protocol.EnrolmentResponse{}, err
		}
		reenrolled = true

	default:
		return protocol.EnrolmentResponse{}, apierr.New(401, apierr.CodeEnrolmentTokenInvalid,
			"an enrolment token or an existing device credential is required")
	}

	credential, err := s.issueCredential(ctx, tenant, device, req, in, now)
	if err != nil {
		return protocol.EnrolmentResponse{}, err
	}

	// §5.1: the token is single-use. It is settled only after the credential commit, so a failure
	// leaves the device able to retry with the same token rather than stranded without one.
	if tokenHash != "" {
		if err := s.store.MarkEnrolmentTokenUsed(ctx, tenant.TenantID, tokenHash, now); err != nil {
			if errors.Is(err, store.ErrTokenUsed) {
				return protocol.EnrolmentResponse{}, apierr.New(401, apierr.CodeEnrolmentTokenInvalid,
					"the enrolment token was already redeemed")
			}
			return protocol.EnrolmentResponse{}, apierr.Internal(fmt.Errorf("mark token used: %w", err))
		}
	}

	return protocol.EnrolmentResponse{
		SchemaVersion:  protocol.EnrolmentSchemaVersion,
		DeviceID:       device.DeviceID,
		TenantID:       tenant.TenantID,
		Region:         tenant.ResidencyRegion,
		Reenrolled:     reenrolled,
		Credential:     credential,
		DeviceIdentity: tenant.DeviceIdentity,
		ServerTime:     now,
	}, nil
}

// resolveToken validates the presented token and returns its row. The tenant comes from the token,
// the hash is compared in constant time, and each lifecycle failure maps to a distinct error so an
// operator can tell an expired token from a used one.
func (s *Service) resolveToken(ctx context.Context, plaintext string, now time.Time) (store.EnrolmentToken, error) {
	tenantID, err := ParseEnrolmentToken(plaintext)
	if err != nil {
		return store.EnrolmentToken{}, apierr.New(401, apierr.CodeEnrolmentTokenInvalid,
			"the enrolment token is malformed")
	}
	presented := HashEnrolmentToken(plaintext)
	t, err := s.store.ResolveEnrolmentToken(ctx, tenantID, presented)
	if errors.Is(err, store.ErrTokenUnknown) {
		return store.EnrolmentToken{}, apierr.New(401, apierr.CodeEnrolmentTokenInvalid,
			"the enrolment token is not recognised")
	}
	if err != nil {
		return store.EnrolmentToken{}, apierr.Internal(fmt.Errorf("resolve token: %w", err))
	}
	// The lookup already keys on the hash; comparing again in constant time is defence in depth
	// against a store that returned a different row, and costs nothing.
	if subtle.ConstantTimeCompare([]byte(presented), []byte(t.TokenHash)) != 1 {
		return store.EnrolmentToken{}, apierr.New(401, apierr.CodeEnrolmentTokenInvalid,
			"the enrolment token is not recognised")
	}
	switch err := t.Usable(now); {
	case errors.Is(err, store.ErrTokenExpired):
		return store.EnrolmentToken{}, apierr.New(410, apierr.CodeEnrolmentTokenExpired,
			"the enrolment token has expired; request a fresh enrolment profile")
	case errors.Is(err, store.ErrTokenUsed):
		return store.EnrolmentToken{}, apierr.New(401, apierr.CodeEnrolmentTokenInvalid,
			"the enrolment token was already redeemed")
	case errors.Is(err, store.ErrTokenRevoked):
		return store.EnrolmentToken{}, apierr.New(401, apierr.CodeEnrolmentTokenInvalid,
			"the enrolment token was revoked")
	case err != nil:
		return store.EnrolmentToken{}, apierr.Internal(err)
	}
	return t, nil
}

// checkTenant resolves the tenant and enforces the region pin. A tenant pinned to another region is
// refused rather than written cross-region, and the refusal fails closed (docs/02 §12).
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
		return store.Tenant{}, apierr.New(403, apierr.CodeUnknownTenant,
			"the tenant is not active")
	}
	if s.cfg.Region != "" && t.ResidencyRegion != "" && t.ResidencyRegion != s.cfg.Region {
		return store.Tenant{}, apierr.New(403, apierr.CodeRegionMismatch,
			"this deployment is not the tenant's pinned region")
	}
	return t, nil
}

// reconcileDevice implements C11: a hardware identity already present in the tenant returns the
// existing device, while a revoked device is refused rather than given a fresh identity.
func (s *Service) reconcileDevice(ctx context.Context, tenant store.Tenant, req protocol.EnrolmentRequest, now time.Time) (store.Device, bool, error) {
	if hwid := req.Device.HardwareIdentityHash; hwid != "" {
		existing, err := s.store.FindDeviceByHardwareIdentity(ctx, tenant.TenantID, hwid)
		switch {
		case err == nil:
			if existing.RevokedAt != nil {
				return store.Device{}, false, apierr.New(403, apierr.CodeRevokedDevice,
					"this device is revoked and cannot re-enrol; a revoked device does not get a fresh identity")
			}
			existing.OS = req.Device.OS
			existing.OSVersion = req.Device.OSVersion
			existing.MDMID = req.Device.MDMID
			existing.ResidencyRegion = tenant.ResidencyRegion
			applyEnrolmentIdentity(&existing, tenant, req.Device)
			updated, err := s.store.UpsertDevice(ctx, existing)
			if err != nil {
				return store.Device{}, false, apierr.Internal(fmt.Errorf("update device: %w", err))
			}
			return updated, true, nil
		case errors.Is(err, store.ErrDeviceUnknown):
			// Fall through to a new device.
		default:
			return store.Device{}, false, apierr.Internal(fmt.Errorf("find device: %w", err))
		}
	}
	deviceID, err := store.NewUUID()
	if err != nil {
		return store.Device{}, false, apierr.Internal(fmt.Errorf("mint device id: %w", err))
	}
	d := store.Device{
		TenantID:             tenant.TenantID,
		DeviceID:             deviceID,
		OS:                   req.Device.OS,
		OSVersion:            req.Device.OSVersion,
		MDMID:                req.Device.MDMID,
		HardwareIdentityHash: req.Device.HardwareIdentityHash,
		ResidencyRegion:      tenant.ResidencyRegion,
		EnrolledAt:           now,
	}
	applyEnrolmentIdentity(&d, tenant, req.Device)
	created, err := s.store.UpsertDevice(ctx, d)
	if err != nil {
		return store.Device{}, false, apierr.Internal(fmt.Errorf("insert device: %w", err))
	}
	return created, false, nil
}

// applyEnrolmentIdentity records the device identity fields an enrolment carries (ADR 0021). Which
// of hostname/hostname_hash is stored is the tenant's choice, not the device's: a 'clear' tenant
// stores the hostname and a 'hashed' tenant stores only the hash, so a device that sends the wrong
// one cannot make the server store a value the tenant forbade. AgentVersion and ManagedState are
// overwritten only when supplied, so a re-enrolment that omits them keeps the last known value.
func applyEnrolmentIdentity(d *store.Device, tenant store.Tenant, info protocol.DeviceInfo) {
	if info.AgentVersion != "" {
		d.AgentVersion = info.AgentVersion
	}
	if info.ManagedState != "" && protocol.ManagedState(info.ManagedState).Valid() {
		d.ManagedState = info.ManagedState
	}
	if tenant.DeviceIdentity == protocol.DeviceIdentityHashed {
		if info.HostnameHash != "" {
			d.HostnameHash = info.HostnameHash
		}
		return
	}
	if info.Hostname != "" {
		d.Hostname = info.Hostname
	}
}

// currentDevice authenticates a rotation: the existing credential must be live, and the device may
// not be revoked. The hardware identity is reconciled so a rotation also repairs a missing hash.
func (s *Service) currentDevice(ctx context.Context, current *Current, req protocol.EnrolmentRequest) (store.Tenant, store.Device, error) {
	tenant, err := s.checkTenant(ctx, current.TenantID)
	if err != nil {
		return store.Tenant{}, store.Device{}, err
	}
	cred, err := s.store.DeviceCredential(ctx, current.TenantID, current.CredentialID)
	if errors.Is(err, store.ErrCredentialUnknown) {
		return store.Tenant{}, store.Device{}, apierr.New(401, apierr.CodeRevokedDevice,
			"the current device credential is unknown")
	}
	if err != nil {
		return store.Tenant{}, store.Device{}, apierr.Internal(fmt.Errorf("current credential: %w", err))
	}
	if cred.DeviceID != current.DeviceID {
		return store.Tenant{}, store.Device{}, apierr.New(401, apierr.CodeRevokedDevice,
			"the current credential does not belong to this device")
	}
	if err := cred.Active(s.cfg.Now().UTC()); err != nil {
		return store.Tenant{}, store.Device{}, apierr.New(401, apierr.CodeRevokedDevice,
			"the current device credential is revoked or expired")
	}
	device, err := s.store.Device(ctx, tenant.TenantID, current.DeviceID)
	if errors.Is(err, store.ErrDeviceUnknown) {
		return store.Tenant{}, store.Device{}, apierr.New(401, apierr.CodeRevokedDevice,
			"the device is unknown")
	}
	if err != nil {
		return store.Tenant{}, store.Device{}, apierr.Internal(fmt.Errorf("current device: %w", err))
	}
	if device.RevokedAt != nil {
		return store.Tenant{}, store.Device{}, apierr.New(403, apierr.CodeRevokedDevice,
			"this device is revoked and cannot re-enrol")
	}
	if hwid := req.Device.HardwareIdentityHash; hwid != "" {
		if device.HardwareIdentityHash != "" && device.HardwareIdentityHash != hwid {
			return store.Tenant{}, store.Device{}, apierr.New(409, apierr.CodeHardwareConflict,
				"the presented hardware identity does not match the enrolled device")
		}
		device.HardwareIdentityHash = hwid
	}
	device.OS = req.Device.OS
	device.OSVersion = req.Device.OSVersion
	device.MDMID = req.Device.MDMID
	device.ResidencyRegion = tenant.ResidencyRegion
	applyEnrolmentIdentity(&device, tenant, req.Device)
	updated, err := s.store.UpsertDevice(ctx, device)
	if err != nil {
		return store.Tenant{}, store.Device{}, apierr.Internal(fmt.Errorf("update device: %w", err))
	}
	return tenant, updated, nil
}

// issueCredential signs or registers the new credential and revokes the previous one in one
// transaction (the store's IssueCredential).
func (s *Service) issueCredential(ctx context.Context, tenant store.Tenant, device store.Device, req protocol.EnrolmentRequest, in Input, now time.Time) (protocol.IssuedCredential, error) {
	credentialID, err := store.NewUUID()
	if err != nil {
		return protocol.IssuedCredential{}, apierr.Internal(fmt.Errorf("mint credential id: %w", err))
	}

	switch req.Mode {
	case protocol.AuthModeX509:
		csr, err := parseCSR(req.CSR)
		if err != nil {
			return protocol.IssuedCredential{}, err
		}
		issued, err := s.signer.Sign(ctx, signer.Request{
			CSR: csr, DeviceID: device.DeviceID, TenantID: tenant.TenantID,
		})
		if err != nil {
			return protocol.IssuedCredential{}, apierr.Internal(fmt.Errorf("sign certificate: %w", err))
		}
		// The credential id is derived from the issued certificate, not minted randomly: ingest-api
		// recomputes it from the presented leaf (protocol.CredentialID), so both sides agree without
		// sharing state, and a re-issued certificate is a distinct credential.
		block, _ := pem.Decode([]byte(issued.CertPEM))
		if block == nil || block.Type != "CERTIFICATE" {
			return protocol.IssuedCredential{}, apierr.Internal(errors.New("the signer returned no PEM certificate"))
		}
		credentialID = protocol.CredentialID(block.Bytes)
		_, err = s.store.IssueCredential(ctx, store.IssueCredential{
			TenantID:            tenant.TenantID,
			DeviceID:            device.DeviceID,
			CredentialID:        credentialID,
			Type:                protocol.AuthModeX509,
			PublicKeyThumbprint: spkiThumbprint(csr),
			IssuedAt:            now,
			ExpiresAt:           issued.NotAfter,
			RevokedAt:           now,
		})
		if err != nil {
			return protocol.IssuedCredential{}, apierr.Internal(fmt.Errorf("issue credential: %w", err))
		}
		return protocol.IssuedCredential{
			Mode:     protocol.AuthModeX509,
			CertPEM:  issued.CertPEM,
			ChainPEM: issued.ChainPEM,
			NotAfter: issued.NotAfter,
		}, nil

	case protocol.AuthModeDPoP:
		if req.JWK == nil {
			return protocol.IssuedCredential{}, apierr.New(400, apierr.CodeSchemaViolation,
				"a dpop enrolment carries a public jwk")
		}
		if _, err := jose.PublicFromJWK(*req.JWK); err != nil {
			return protocol.IssuedCredential{}, apierr.New(400, apierr.CodeSchemaViolation,
				"the dpop jwk is not a usable P-256 public key")
		}
		thumbprint, err := req.JWK.Thumbprint()
		if err != nil {
			return protocol.IssuedCredential{}, apierr.New(400, apierr.CodeSchemaViolation,
				"the dpop jwk is missing a member its thumbprint needs")
		}
		if in.Current != nil {
			// Re-enrolment: the one proof header authenticated the *current* credential, so the new
			// registration must be the same key. Switching keys would need a proof the wire contract
			// has no field for, and refusing is safer than accepting an unproven change.
			if in.Current.KeyThumbprint == "" || subtle.ConstantTimeCompare([]byte(thumbprint), []byte(in.Current.KeyThumbprint)) != 1 {
				return protocol.IssuedCredential{}, apierr.New(400, apierr.CodeInvalidProof,
					"a dpop re-enrolment must re-register the credential's existing key")
			}
		} else {
			proof, err := s.verifyPossession(in, now)
			if err != nil {
				return protocol.IssuedCredential{}, err
			}
			if subtle.ConstantTimeCompare([]byte(proof.Thumbprint), []byte(thumbprint)) != 1 {
				return protocol.IssuedCredential{}, apierr.New(400, apierr.CodeInvalidProof,
					"the proof of possession is for a different key than the registered jwk")
			}
		}
		jwkJSON, err := json.Marshal(req.JWK)
		if err != nil {
			return protocol.IssuedCredential{}, apierr.Internal(fmt.Errorf("marshal jwk: %w", err))
		}
		expires := now.Add(s.cfg.CredentialTTL)
		if _, err := s.store.IssueCredential(ctx, store.IssueCredential{
			TenantID:            tenant.TenantID,
			DeviceID:            device.DeviceID,
			CredentialID:        credentialID,
			Type:                protocol.AuthModeDPoP,
			PublicKeyThumbprint: thumbprint,
			PublicKeyJWK:        jwkJSON,
			IssuedAt:            now,
			ExpiresAt:           expires,
			RevokedAt:           now,
		}); err != nil {
			return protocol.IssuedCredential{}, apierr.Internal(fmt.Errorf("issue credential: %w", err))
		}
		return protocol.IssuedCredential{
			Mode:     protocol.AuthModeDPoP,
			JWK:      req.JWK,
			NotAfter: expires,
		}, nil
	}
	return protocol.IssuedCredential{}, apierr.New(400, apierr.CodeSchemaViolation, "unsupported mode")
}

// verifyPossession checks the DPoP proof that a fresh dpop enrolment presents. The proof proves the
// caller holds the private half of the key being registered; the jti is read as part of the wire
// format but not persisted (see the store's note on ops.dpop_replay).
func (s *Service) verifyPossession(in Input, now time.Time) (dpop.Proof, error) {
	if in.Proof == "" {
		return dpop.Proof{}, apierr.New(400, apierr.CodeInvalidProof,
			"a dpop enrolment must prove possession of the key it registers")
	}
	proof, err := dpop.Verify(in.Proof, in.HTM, in.HTU, "", now, s.cfg.ProofSkew)
	if err != nil {
		return dpop.Proof{}, apierr.New(400, apierr.CodeInvalidProof,
			"the dpop proof of possession did not verify")
	}
	return proof, nil
}

// parseCSR decodes and verifies a PKCS#10 request. A CSR whose signature does not verify is refused
// before the signer sees it, so a caller cannot register a key it cannot prove it holds.
func parseCSR(pemText string) (*x509.CertificateRequest, error) {
	if pemText == "" {
		return nil, apierr.New(400, apierr.CodeInvalidCSR, "an x509 enrolment carries a pkcs#10 csr")
	}
	block, _ := pem.Decode([]byte(pemText))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, apierr.New(400, apierr.CodeInvalidCSR, "the csr is not a PKCS#10 PEM block")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, apierr.New(400, apierr.CodeInvalidCSR, "the csr could not be parsed")
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, apierr.New(400, apierr.CodeInvalidCSR, "the csr signature does not verify")
	}
	return csr, nil
}

// spkiThumbprint is SHA-256 over the CSR's SubjectPublicKeyInfo, base64url without padding. It is
// the x509 half of ADR 0020 decision 4: the same value the authenticator computes from a presented
// certificate's SPKI.
func spkiThumbprint(csr *x509.CertificateRequest) string {
	sum := sha256.Sum256(csr.RawSubjectPublicKeyInfo)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
