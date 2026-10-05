package enrol

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/intune"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

// The deployment-key bootstrap (contract §5). A customer's MDM delivers one package to every
// device, so the package cannot carry a per-device single-use token; it carries a per-tenant key
// instead. What keeps a reusable key from being a skeleton key is everything around it: its hash is
// the only stored form, it can be revoked or expire, each key is rate-limited, and a tenant that
// manages its devices with Intune also requires each device to name a managed device in the
// customer's own Intune.

// deploymentUse is what the enrolment needs to settle after the credential commit.
type deploymentUse struct {
	keyID        string
	verification string
}

// deploymentBootstrap authenticates a deployment-key enrolment and resolves its device.
func (s *Service) deploymentBootstrap(ctx context.Context, req protocol.EnrolmentRequest, now time.Time) (store.Tenant, store.Device, bool, *deploymentUse, error) {
	fail := func(err error) (store.Tenant, store.Device, bool, *deploymentUse, error) {
		return store.Tenant{}, store.Device{}, false, nil, err
	}
	if s.cfg.Deployment == nil {
		return fail(apierr.New(401, apierr.CodeDeploymentKeyInvalid,
			"this deployment does not accept deployment keys"))
	}
	key, err := s.resolveDeploymentKey(ctx, req.DeploymentKey, now)
	if err != nil {
		return fail(err)
	}
	if !s.limiter.allow(key.TenantID+"|"+key.KeyID, now) {
		return fail(apierr.New(429, apierr.CodeRateLimited,
			"this deployment key is enrolling devices faster than its limit; retry shortly"))
	}
	tenant, err := s.checkTenant(ctx, key.TenantID)
	if err != nil {
		return fail(err)
	}
	v, err := s.cfg.Deployment.DeviceVerification(ctx, tenant.TenantID)
	if err != nil {
		return fail(apierr.Internal(fmt.Errorf("device verification: %w", err)))
	}

	var intuneID string
	switch v.Mode {
	case store.VerificationNone, "":
	case store.VerificationIntune:
		md, err := s.verifyIntune(ctx, tenant, key, v, req.Attestation, now)
		if err != nil {
			return fail(err)
		}
		intuneID = strings.ToLower(strings.Trim(md.ID, "{}"))
	default:
		// A mode this build does not know is not a mode it may skip: fail closed, visibly.
		return fail(apierr.Internal(fmt.Errorf("tenant device_verification %q is not one this build implements", v.Mode)))
	}

	device, reenrolled, err := s.reconcileDevice(ctx, tenant, req, now, intuneID)
	if err != nil {
		return fail(err)
	}
	mode := v.Mode
	if mode == "" {
		mode = store.VerificationNone
	}
	return tenant, device, reenrolled, &deploymentUse{keyID: key.KeyID, verification: mode}, nil
}

// resolveDeploymentKey validates a presented key: shape, then the row by (tenant, hash), then the
// hash in constant time, then the key's lifecycle, each failure with its own code.
func (s *Service) resolveDeploymentKey(ctx context.Context, plaintext string, now time.Time) (store.DeploymentKey, error) {
	tenantID, err := ParseDeploymentKey(plaintext)
	if err != nil {
		return store.DeploymentKey{}, apierr.New(401, apierr.CodeDeploymentKeyInvalid,
			"the deployment key is malformed")
	}
	presented := HashDeploymentKey(plaintext)
	key, err := s.cfg.Deployment.DeploymentKeyByHash(ctx, tenantID, presented)
	if errors.Is(err, store.ErrDeploymentKeyUnknown) {
		return store.DeploymentKey{}, apierr.New(401, apierr.CodeDeploymentKeyInvalid,
			"the deployment key is not recognised")
	}
	if err != nil {
		return store.DeploymentKey{}, apierr.Internal(fmt.Errorf("resolve deployment key: %w", err))
	}
	if subtle.ConstantTimeCompare([]byte(presented), []byte(key.KeyHash)) != 1 || !strings.EqualFold(key.TenantID, tenantID) {
		return store.DeploymentKey{}, apierr.New(401, apierr.CodeDeploymentKeyInvalid,
			"the deployment key is not recognised")
	}
	switch err := key.Usable(now); {
	case errors.Is(err, store.ErrDeploymentKeyRevoked):
		return store.DeploymentKey{}, apierr.New(401, apierr.CodeDeploymentKeyRevoked,
			"the deployment key was revoked; install a package with a current key")
	case errors.Is(err, store.ErrDeploymentKeyExpired):
		return store.DeploymentKey{}, apierr.New(410, apierr.CodeDeploymentKeyExpired,
			"the deployment key has expired; install a package with a current key")
	case err != nil:
		return store.DeploymentKey{}, apierr.Internal(err)
	}
	return key, nil
}

// verifyIntune runs the tenant's Intune check. A refusal is the device's problem and is answered
// 403 device_not_managed with the reason, and audited so the customer's admin can see a copied
// package being tried; anything else is the service's problem and is a retryable 503.
func (s *Service) verifyIntune(ctx context.Context, tenant store.Tenant, key store.DeploymentKey, v store.DeviceVerification, att *protocol.DeviceAttestation, now time.Time) (intune.ManagedDevice, error) {
	if s.cfg.Intune == nil {
		return intune.ManagedDevice{}, apierr.Internal(errors.New("the tenant requires Intune verification and no Intune checker is configured"))
	}
	if v.EntraTenantID == "" {
		return intune.ManagedDevice{}, apierr.Internal(errors.New("the tenant requires Intune verification and has no active Entra connection"))
	}
	md, err := s.cfg.Intune.Check(ctx, v.EntraTenantID, att)
	if refusal, ok := intune.IsRefusal(err); ok {
		// Best effort: the refusal stands even if its audit row cannot be written, because turning
		// a refusal into a retryable failure would invite the retry.
		_ = s.cfg.Deployment.Audit(ctx, store.AuditEntry{
			TenantID:   tenant.TenantID,
			ActorType:  store.ActorSystem,
			ActorID:    "control-api:enrol",
			Action:     "device.enrol_refused",
			ObjectType: "deployment_key",
			ObjectID:   key.KeyID,
			OccurredAt: now,
			Detail:     map[string]any{"reason": refusal.Reason, "check": "intune"},
		})
		return intune.ManagedDevice{}, apierr.Detailed(403, apierr.CodeDeviceNotManaged,
			"the customer's Intune does not vouch for this device", map[string]any{"reason": refusal.Reason})
	}
	if err != nil {
		return intune.ManagedDevice{}, apierr.Internal(err)
	}
	return md, nil
}

// keyLimiter is a token bucket per deployment key, in process memory. It bounds what one key can
// do; it is not a fleet-wide quota, and N replicas admit N times the rate.
type keyLimiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newKeyLimiter(rate float64, burst int) *keyLimiter {
	return &keyLimiter{rate: rate, burst: float64(burst), buckets: map[string]*bucket{}}
}

// allow takes one token for key. The map holds one entry per key that has enrolled, which is
// bounded by the keys a tenant's admins minted, not by what callers present.
func (l *keyLimiter) allow(key string, now time.Time) bool {
	if l == nil || l.rate < 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * l.rate
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
