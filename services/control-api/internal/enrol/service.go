// Package enrol implements POST /v1/enrol: it turns a deployment key or a device's current
// certificate into a new device certificate.
//
// A first enrolment presents the tenant's deployment key, which the MDM-delivered tenant package
// carries; a re-enrolment (certificate rotation) presents the device's current certificate on the
// transport instead. Either way the device sends a PKCS#10 CSR and its private key never leaves it.
// The tenant always comes from the key or the certificate, never from the request body; the
// deployment's region pin is checked and fails closed; and the hardware identity hash (with a
// verified Intune device id ahead of it for an Intune tenant) is the idempotency key, so a
// re-imaged device gets its existing device id back and a revoked device is refused a fresh one.
package enrol

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/deviceca"
	"github.com/shadow-ai-capture/control-api/internal/intune"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

// Config is the service's settings.
type Config struct {
	// Region is the Azure region this deployment serves. A tenant pinned elsewhere is refused.
	// Empty disables the check.
	Region string
	// Intune checks the devices of a tenant whose device_verification is 'intune'. Nil refuses
	// such an enrolment with a retryable 503; the check is never skipped.
	Intune intune.Checker
	// UserRefKeys supplies the tenant's user-reference key for the response. Nil omits it.
	UserRefKeys UserRefKeys
	// PolicyETag names the tenant's current policy bundle in the response, so the device can skip
	// its first policy fetch. Nil, or a failure, omits it; it never fails an enrolment.
	PolicyETag PolicyETagSource
	// KeyRate is the sustained enrolments per second one deployment key may make and KeyBurst the
	// bucket that absorbs a rollout wave. Zero takes the defaults. The bucket is per process.
	KeyRate  float64
	KeyBurst int
	Now      func() time.Time
}

// UserRefKeys hands out a tenant's 32-byte user-reference key, minting it on first need.
type UserRefKeys interface {
	Key(ctx context.Context, tenantID string) ([]byte, error)
}

// PolicyETagSource names the tenant's current policy bundle as GET /v1/policy's ETag does.
type PolicyETagSource interface {
	ETag(ctx context.Context, tenantID string) (string, error)
}

// The default per-key limit: a burst for a rollout wave of a few hundred devices, and a sustained
// rate above any fleet's enrolment rate that still bounds what a leaked key can do.
const (
	DefaultKeyRate  = 5.0
	DefaultKeyBurst = 300
)

// Service is the enrolment path.
type Service struct {
	store   store.Store
	ca      *deviceca.CA
	cfg     Config
	limiter *keyLimiter
}

// New builds the service.
func New(st store.Store, ca *deviceca.CA, cfg Config) (*Service, error) {
	if st == nil || ca == nil {
		return nil, errors.New("enrol: a store and a device CA are required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.KeyRate == 0 {
		cfg.KeyRate = DefaultKeyRate
	}
	if cfg.KeyBurst <= 0 {
		cfg.KeyBurst = DefaultKeyBurst
	}
	return &Service{store: st, ca: ca, cfg: cfg, limiter: newKeyLimiter(cfg.KeyRate, cfg.KeyBurst)}, nil
}

// Current is a device authenticated by its current, live certificate: the transport has verified
// the certificate against the device CA and matched it to the device's credential.
type Current struct {
	TenantID     string
	DeviceID     string
	CredentialID string
}

// Input is one /v1/enrol call.
type Input struct {
	Request protocol.EnrolmentRequest
	// Current resolves the certificate a re-enrolment presents. It is called only when the request
	// carries no deployment key, after the body has been validated, so a device whose expired
	// certificate is still on the connection can enrol again with its deployment key.
	Current func() (Current, error)
}

// Enrol performs the exchange and returns the new certificate.
func (s *Service) Enrol(ctx context.Context, in Input) (protocol.EnrolmentResponse, error) {
	req := in.Request
	now := s.cfg.Now().UTC()

	if req.SchemaVersion != protocol.EnrolmentSchemaVersion {
		return protocol.EnrolmentResponse{}, apierr.Detailed(http.StatusBadRequest, apierr.CodeUnsupportedSchemaVersion,
			"the enrolment schema_version is not supported",
			map[string]any{"supported": []string{protocol.EnrolmentSchemaVersion}})
	}
	switch req.Device.OS {
	case "windows", "macos", "linux":
	default:
		return protocol.EnrolmentResponse{}, apierr.New(http.StatusBadRequest, apierr.CodeSchemaViolation,
			"device.os must be windows, macos or linux")
	}
	// The CSR is checked before anything is written, so a bad request leaves no device row behind.
	csr, err := parseCSR(req.CSR)
	if err != nil {
		return protocol.EnrolmentResponse{}, err
	}

	var (
		tenant     store.Tenant
		device     store.Device
		reenrolled bool
		keyUse     *deploymentUse
	)
	switch {
	case req.DeploymentKey != "":
		tenant, device, reenrolled, keyUse, err = s.deploymentBootstrap(ctx, req, now)
	case in.Current != nil:
		var cur Current
		if cur, err = in.Current(); err == nil {
			tenant, device, err = s.currentDevice(ctx, cur, req)
			reenrolled = true
		}
	default:
		err = apierr.New(http.StatusUnauthorized, apierr.CodeUnauthenticated,
			"a first enrolment presents the tenant's deployment key; a re-enrolment presents the current device certificate")
	}
	if err != nil {
		return protocol.EnrolmentResponse{}, err
	}

	// The user-reference key is read before the certificate is issued, so a key source that cannot
	// answer leaves the device to retry rather than holding a certificate and no key.
	userRefKey, err := s.userRefKey(ctx, tenant.TenantID)
	if err != nil {
		return protocol.EnrolmentResponse{}, err
	}
	credential, err := s.issue(ctx, tenant, device, csr, now)
	if err != nil {
		return protocol.EnrolmentResponse{}, err
	}

	// The key's use is counted and audited after the certificate commits: a failure here is a
	// retry, and a retry is idempotent on the device identity.
	if keyUse != nil {
		if err := s.store.RecordDeploymentEnrolment(ctx, tenant.TenantID, keyUse.keyID, now, store.AuditEntry{
			TenantID:   tenant.TenantID,
			ActorType:  store.ActorDevice,
			ActorID:    device.DeviceID,
			Action:     "device.enrol",
			ObjectType: "device",
			ObjectID:   device.DeviceID,
			OccurredAt: now,
			Detail: map[string]any{
				"key_id":       keyUse.keyID,
				"reenrolled":   reenrolled,
				"verification": keyUse.verification,
			},
		}); err != nil {
			return protocol.EnrolmentResponse{}, apierr.Internal(fmt.Errorf("record deployment enrolment: %w", err))
		}
	}

	resp := protocol.EnrolmentResponse{
		SchemaVersion:  protocol.EnrolmentSchemaVersion,
		DeviceID:       device.DeviceID,
		TenantID:       tenant.TenantID,
		Region:         tenant.ResidencyRegion,
		Reenrolled:     reenrolled,
		Credential:     credential,
		DeviceIdentity: tenant.DeviceIdentity,
		UserRefKey:     userRefKey,
		ServerTime:     now,
	}
	if s.cfg.PolicyETag != nil {
		if etag, err := s.cfg.PolicyETag.ETag(ctx, tenant.TenantID); err == nil {
			resp.PolicyETag = etag
		}
	}
	return resp, nil
}

// userRefKey reads the tenant's user-reference key as the response carries it: base64url, no
// padding.
func (s *Service) userRefKey(ctx context.Context, tenantID string) (string, error) {
	if s.cfg.UserRefKeys == nil {
		return "", nil
	}
	key, err := s.cfg.UserRefKeys.Key(ctx, tenantID)
	if err != nil {
		return "", apierr.Internal(fmt.Errorf("user_ref key: %w", err))
	}
	if len(key) != protocol.UserRefKeySize {
		return "", apierr.Internal(fmt.Errorf("user_ref key is %d bytes, want %d", len(key), protocol.UserRefKeySize))
	}
	return base64.RawURLEncoding.EncodeToString(key), nil
}

// checkTenant resolves the tenant and enforces the region pin.
func (s *Service) checkTenant(ctx context.Context, tenantID string) (store.Tenant, error) {
	t, err := s.store.Tenant(ctx, tenantID)
	if errors.Is(err, store.ErrUnknownTenant) {
		return store.Tenant{}, apierr.New(http.StatusForbidden, apierr.CodeUnknownTenant,
			"the authenticated tenant is unknown to this deployment")
	}
	if err != nil {
		return store.Tenant{}, apierr.Internal(fmt.Errorf("tenant: %w", err))
	}
	if !t.Active() {
		return store.Tenant{}, apierr.New(http.StatusForbidden, apierr.CodeUnknownTenant, "the tenant is not active")
	}
	if s.cfg.Region != "" && t.ResidencyRegion != "" && t.ResidencyRegion != s.cfg.Region {
		return store.Tenant{}, apierr.New(http.StatusForbidden, apierr.CodeRegionMismatch,
			"this deployment is not the tenant's pinned region")
	}
	return t, nil
}

func refuseRevoked() error {
	return apierr.New(http.StatusForbidden, apierr.CodeRevokedDevice,
		"this device is revoked and cannot enrol again; a revoked device does not get a fresh identity")
}

// reconcileDevice finds or creates the device the request describes. intuneID, when set, is the
// Intune managed-device id the request was verified against, and it is the stronger key: one Intune
// device is one product device, whatever hardware hash it now presents.
func (s *Service) reconcileDevice(ctx context.Context, tenant store.Tenant, req protocol.EnrolmentRequest, now time.Time, intuneID string) (store.Device, bool, error) {
	if intuneID == "" {
		return s.reconcileByHardware(ctx, tenant, req, now)
	}
	existing, err := s.store.FindDeviceByIntuneID(ctx, tenant.TenantID, intuneID)
	switch {
	case err == nil:
		if existing.RevokedAt != nil {
			return store.Device{}, false, refuseRevoked()
		}
		// The stored hardware identity stands. One the row lacks is adopted only when no other
		// device holds it, so this path never trips the per-tenant hardware index.
		if hwid := req.Device.HardwareIdentityHash; existing.HardwareIdentityHash == "" && hwid != "" {
			if _, err := s.store.FindDeviceByHardwareIdentity(ctx, tenant.TenantID, hwid); errors.Is(err, store.ErrDeviceUnknown) {
				existing.HardwareIdentityHash = hwid
			} else if err != nil {
				return store.Device{}, false, apierr.Internal(fmt.Errorf("find device: %w", err))
			}
		}
		updated, err := s.store.UpsertDevice(ctx, refresh(existing, tenant, req.Device))
		if err != nil {
			return store.Device{}, false, apierr.Internal(fmt.Errorf("update device: %w", err))
		}
		return updated, true, nil
	case !errors.Is(err, store.ErrDeviceUnknown):
		return store.Device{}, false, apierr.Internal(fmt.Errorf("find device by intune id: %w", err))
	}
	device, reenrolled, err := s.reconcileByHardware(ctx, tenant, req, now)
	if err != nil {
		return store.Device{}, false, err
	}
	// A device found by its hardware identity under a different Intune id was enrolled into Intune
	// again (a re-image): the new id replaces the old one on the same device.
	if err := s.store.SetDeviceIntuneID(ctx, tenant.TenantID, device.DeviceID, intuneID); err != nil {
		return store.Device{}, false, apierr.Internal(fmt.Errorf("bind intune id: %w", err))
	}
	device.IntuneDeviceID = intuneID
	return device, reenrolled, nil
}

func (s *Service) reconcileByHardware(ctx context.Context, tenant store.Tenant, req protocol.EnrolmentRequest, now time.Time) (store.Device, bool, error) {
	if hwid := req.Device.HardwareIdentityHash; hwid != "" {
		existing, err := s.store.FindDeviceByHardwareIdentity(ctx, tenant.TenantID, hwid)
		switch {
		case err == nil:
			if existing.RevokedAt != nil {
				return store.Device{}, false, refuseRevoked()
			}
			updated, err := s.store.UpsertDevice(ctx, refresh(existing, tenant, req.Device))
			if err != nil {
				return store.Device{}, false, apierr.Internal(fmt.Errorf("update device: %w", err))
			}
			return updated, true, nil
		case !errors.Is(err, store.ErrDeviceUnknown):
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
		HardwareIdentityHash: req.Device.HardwareIdentityHash,
		EnrolledAt:           now,
	}
	created, err := s.store.UpsertDevice(ctx, refresh(d, tenant, req.Device))
	if err != nil {
		return store.Device{}, false, apierr.Internal(fmt.Errorf("insert device: %w", err))
	}
	return created, false, nil
}

// refresh applies what an enrolment reports about the device. Which of hostname and hostname hash
// is stored is the tenant's setting, not the device's, so a device sending the wrong one cannot make
// the server store a value the tenant forbids.
func refresh(d store.Device, tenant store.Tenant, info protocol.DeviceInfo) store.Device {
	d.OS = info.OS
	d.OSVersion = info.OSVersion
	d.ResidencyRegion = tenant.ResidencyRegion
	if info.AgentVersion != "" {
		d.AgentVersion = info.AgentVersion
	}
	if protocol.ManagedState(info.ManagedState).Valid() {
		d.ManagedState = info.ManagedState
	}
	if tenant.DeviceIdentity == protocol.DeviceIdentityHashed {
		if info.HostnameHash != "" {
			d.HostnameHash = info.HostnameHash
		}
	} else if info.Hostname != "" {
		d.Hostname = info.Hostname
	}
	return d
}

// currentDevice resolves a rotation: the tenant must still be served here and the device must
// exist and not be revoked. A hardware identity the device row lacks is recorded.
func (s *Service) currentDevice(ctx context.Context, cur Current, req protocol.EnrolmentRequest) (store.Tenant, store.Device, error) {
	tenant, err := s.checkTenant(ctx, cur.TenantID)
	if err != nil {
		return store.Tenant{}, store.Device{}, err
	}
	device, err := s.store.Device(ctx, tenant.TenantID, cur.DeviceID)
	if errors.Is(err, store.ErrDeviceUnknown) {
		return store.Tenant{}, store.Device{}, apierr.New(http.StatusUnauthorized, apierr.CodeRevokedDevice, "the device is unknown")
	}
	if err != nil {
		return store.Tenant{}, store.Device{}, apierr.Internal(fmt.Errorf("current device: %w", err))
	}
	if device.RevokedAt != nil {
		return store.Tenant{}, store.Device{}, refuseRevoked()
	}
	if hwid := req.Device.HardwareIdentityHash; hwid != "" {
		if device.HardwareIdentityHash != "" && device.HardwareIdentityHash != hwid {
			return store.Tenant{}, store.Device{}, apierr.New(http.StatusConflict, apierr.CodeHardwareConflict,
				"the presented hardware identity does not match the enrolled device")
		}
		device.HardwareIdentityHash = hwid
	}
	updated, err := s.store.UpsertDevice(ctx, refresh(device, tenant, req.Device))
	if err != nil {
		return store.Tenant{}, store.Device{}, apierr.Internal(fmt.Errorf("update device: %w", err))
	}
	return tenant, updated, nil
}

// issue signs the device certificate and records it as the device's one live credential.
func (s *Service) issue(ctx context.Context, tenant store.Tenant, device store.Device, csr *x509.CertificateRequest, now time.Time) (protocol.IssuedCredential, error) {
	issued, err := s.ca.Sign(csr, tenant.TenantID, device.DeviceID, now)
	if err != nil {
		return protocol.IssuedCredential{}, apierr.Internal(fmt.Errorf("sign certificate: %w", err))
	}
	// The credential id is derived from the certificate, so every verifier recomputes it from the
	// presented certificate without shared state, and a re-issued certificate is a new credential.
	if err := s.store.IssueCredential(ctx, store.Credential{
		TenantID:            tenant.TenantID,
		CredentialID:        protocol.CredentialID(issued.DER),
		DeviceID:            device.DeviceID,
		PublicKeyThumbprint: SPKIThumbprint(csr.RawSubjectPublicKeyInfo),
		IssuedAt:            now,
		ExpiresAt:           issued.NotAfter,
	}); err != nil {
		return protocol.IssuedCredential{}, apierr.Internal(fmt.Errorf("issue credential: %w", err))
	}
	return protocol.IssuedCredential{CertPEM: issued.CertPEM, ChainPEM: issued.ChainPEM, NotAfter: issued.NotAfter}, nil
}

// parseCSR decodes a PKCS#10 request and verifies its signature, the device's proof that it holds
// the key it asks a certificate for.
func parseCSR(pemText string) (*x509.CertificateRequest, error) {
	if pemText == "" {
		return nil, apierr.New(http.StatusBadRequest, apierr.CodeInvalidCSR, "an enrolment carries a PKCS#10 csr")
	}
	block, _ := pem.Decode([]byte(pemText))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, apierr.New(http.StatusBadRequest, apierr.CodeInvalidCSR, "the csr is not a PKCS#10 PEM block")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, apierr.New(http.StatusBadRequest, apierr.CodeInvalidCSR, "the csr could not be parsed")
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, apierr.New(http.StatusBadRequest, apierr.CodeInvalidCSR, "the csr signature does not verify")
	}
	return csr, nil
}

// SPKIThumbprint is SHA-256 over a SubjectPublicKeyInfo, base64url without padding: the binding a
// credential records and a verifier recomputes from the presented certificate.
func SPKIThumbprint(rawSPKI []byte) string {
	sum := sha256.Sum256(rawSPKI)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
