// Package policyserve serves GET /v1/policy and writes ops.policy_bundle.
//
// A tenant's bundle is composed from what the database says about it: the tenant's ceiling as the
// default collection mode and the tool catalogue's TLS hosts as the interception scope. It is signed with the vendor's Ed25519 policy key in the envelope
// the agent verifies, and stored with the exact signed bytes. A new version is minted only when the
// composition or the signing key differs from the latest stored bundle; otherwise the stored bytes
// are served again, so the ETag a device holds stays valid until something it would enforce
// changes. Composition happens on the read path under a per-tenant lock, so the first poll after an
// input changes mints the new version.
package policyserve

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/protocol"

	"github.com/shadow-ai-capture/control-api/internal/apierr"
	"github.com/shadow-ai-capture/control-api/internal/store"
)

// Config is the service's composition defaults and timing.
type Config struct {
	// KeyID is the key id the envelope names; the device pins it beside the public key.
	KeyID string
	// ProxyListen is the loopback address the device's TLS proxy binds and the CLI shim exports.
	ProxyListen string
	// PreferredCanary is the canary host when the interception scope includes it; otherwise the
	// first host in the scope is used. The canary is what the proxy's end-to-end probe dials.
	PreferredCanary string
	// Runtimes and NoProxy configure the CLI trust shim.
	Runtimes []string
	NoProxy  []string
	// RecheckInterval is how long a tenant's served bundle is reused before its inputs are read
	// again. Zero takes the default; a negative value re-reads on every request.
	RecheckInterval time.Duration
	Now             func() time.Time
	Logger          *slog.Logger
}

// Defaults for Config.
const (
	DefaultProxyListen     = "127.0.0.1:8843"
	DefaultPreferredCanary = "api.anthropic.com"
	DefaultRecheckInterval = 30 * time.Second
	// retentionClass is ops.policy_bundle.retention_class for every composed bundle: the event
	// retention class.
	retentionClass = "standard"
	actorID        = "control-api"
)

// Served is a tenant's bundle as GET /v1/policy serves it.
type Served struct {
	Version  string
	ETag     string
	Envelope []byte
}

type cached struct {
	served    Served
	checkedAt time.Time
}

// Service composes, signs, stores and serves policy bundles.
type Service struct {
	store store.Store
	key   ed25519.PrivateKey
	cfg   Config

	mu    sync.Mutex
	cache map[string]cached
}

// New builds the service. Only signed bundles are ever served, so a key is required.
func New(st store.Store, key ed25519.PrivateKey, cfg Config) (*Service, error) {
	if st == nil {
		return nil, errors.New("policyserve: store is required")
	}
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("policyserve: an Ed25519 signing key is required")
	}
	if cfg.KeyID == "" {
		cfg.KeyID = DefaultKeyID
	}
	if cfg.ProxyListen == "" {
		cfg.ProxyListen = DefaultProxyListen
	}
	if cfg.PreferredCanary == "" {
		cfg.PreferredCanary = DefaultPreferredCanary
	}
	if cfg.Runtimes == nil {
		cfg.Runtimes = []string{"go", "node", "python"}
	}
	if cfg.NoProxy == nil {
		cfg.NoProxy = []string{"localhost", "127.0.0.1", "::1"}
	}
	if cfg.RecheckInterval == 0 {
		cfg.RecheckInterval = DefaultRecheckInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Service{store: st, key: key, cfg: cfg, cache: map[string]cached{}}, nil
}

// KeyID is the key id served bundles name.
func (s *Service) KeyID() string { return s.cfg.KeyID }

// PublicKeyHex is the public half of the signing key, for the startup log and the generic MSI's
// trust anchor. It is not a secret.
func (s *Service) PublicKeyHex() string { return PublicKeyHex(s.key) }

// ETag names the tenant's current bundle as GET /v1/policy would; it lets an enrolment response
// carry policy_etag.
func (s *Service) ETag(ctx context.Context, tenantID string) (string, error) {
	sv, err := s.Current(ctx, tenantID)
	if err != nil {
		return "", err
	}
	return sv.ETag, nil
}

// Current returns the tenant's bundle in force, minting a new version first when its inputs have
// changed. The errors are apierr values: 403 for an unknown or inactive tenant, 404 when the tenant
// has no bundle and none can be composed, 503 otherwise.
func (s *Service) Current(ctx context.Context, tenantID string) (Served, error) {
	now := s.cfg.Now().UTC()
	s.mu.Lock()
	c, ok := s.cache[tenantID]
	s.mu.Unlock()
	if ok && s.cfg.RecheckInterval > 0 && now.Sub(c.checkedAt) < s.cfg.RecheckInterval {
		return c.served, nil
	}

	in, err := s.store.PolicyInputs(ctx, tenantID)
	if errors.Is(err, store.ErrUnknownTenant) {
		return Served{}, apierr.New(403, apierr.CodeUnknownTenant, "the authenticated tenant is unknown to this deployment")
	}
	if err != nil {
		return Served{}, apierr.Internal(fmt.Errorf("policy inputs: %w", err))
	}
	if !in.Tenant.Active() {
		return Served{}, apierr.New(403, apierr.CodeUnknownTenant, "the tenant is not active")
	}
	candidate, err := s.compose(in)
	if err != nil {
		return Served{}, apierr.Internal(err)
	}

	// The common case reads the latest row without the lock and serves it when nothing changed;
	// only a change takes the per-tenant lock, where the comparison is made again.
	latest, err := s.store.LatestPolicyBundle(ctx, tenantID)
	switch {
	case err == nil:
		if s.current(&latest, candidate) {
			return s.remember(tenantID, latest, now)
		}
	case !errors.Is(err, store.ErrNoPolicyBundle):
		return Served{}, apierr.Internal(fmt.Errorf("latest policy bundle: %w", err))
	}

	var published *store.PolicyBundle
	minted, err := s.store.MintPolicyBundle(ctx, tenantID, func(latest *store.PolicyBundle) (store.MintDecision, error) {
		if s.current(latest, candidate) {
			return store.MintDecision{}, nil
		}
		d, err := s.mint(tenantID, latest, *candidate, now)
		published = d.Bundle
		return d, err
	})
	if errors.Is(err, store.ErrNoPolicyBundle) {
		return Served{}, apierr.New(404, apierr.CodeNoPolicyBundle,
			"no policy bundle exists for this tenant; the device stays at M0")
	}
	if err != nil {
		return Served{}, apierr.Internal(fmt.Errorf("mint policy bundle: %w", err))
	}
	if published != nil && published.Version == minted.Version {
		s.cfg.Logger.Info("control: policy bundle published", "tenant", tenantID, "version", minted.Version,
			"kid", minted.SignatureKID)
	}
	return s.remember(tenantID, minted, now)
}

// current reports whether latest may be served for this candidate: it is servable, signed with
// the current key, and composed from the same inputs.
func (s *Service) current(latest *store.PolicyBundle, candidate *Bundle) bool {
	if latest == nil || len(latest.SignedEnvelope) == 0 {
		return false
	}
	if latest.SignatureKID != s.cfg.KeyID {
		return false
	}
	kid, stored, err := envelopeContent(latest.SignedEnvelope)
	if err != nil || kid != s.cfg.KeyID {
		return false
	}
	want, err := content(*candidate)
	return err == nil && string(stored) == string(want)
}

func (s *Service) remember(tenantID string, b store.PolicyBundle, now time.Time) (Served, error) {
	if len(b.SignedEnvelope) == 0 {
		return Served{}, apierr.New(404, apierr.CodeNoPolicyBundle,
			"no signed policy bundle exists for this tenant; the device stays at M0")
	}
	version := strconv.FormatInt(b.Version, 10)
	sv := Served{Version: version, ETag: protocol.PolicyETag(version), Envelope: b.SignedEnvelope}
	s.mu.Lock()
	s.cache[tenantID] = cached{served: sv, checkedAt: now}
	s.mu.Unlock()
	return sv, nil
}

// compose builds the candidate bundle from the inputs.
func (s *Service) compose(in store.PolicyInputs) (*Bundle, error) {
	mode := protocol.CollectionMode(in.Tenant.CollectionMode)
	if !mode.Valid() {
		// The ceiling CHECK makes this unreachable from the database; refusing beats guessing.
		return nil, fmt.Errorf("tenant collection_mode %q is outside {m0,m1,m2,m3}", in.Tenant.CollectionMode)
	}
	hosts := append([]string(nil), in.InterceptionHosts...)
	sort.Strings(hosts)
	canary := ""
	for _, h := range hosts {
		if h == s.cfg.PreferredCanary {
			canary = h + ":443"
		}
	}
	if canary == "" && len(hosts) > 0 {
		canary = hosts[0] + ":443"
	}
	toolModes := map[string]string{}
	for fp, m := range in.ScopeOverrides {
		if !protocol.CollectionMode(m).Valid() {
			return nil, fmt.Errorf("scope override %q has mode %q outside {m0,m1,m2,m3}", fp, m)
		}
		toolModes[fp] = m
	}
	return &Bundle{
		TenantDefault: string(mode),
		ToolModes:     toolModes,
		Interception: Interception{
			SeedHosts:   hosts,
			Ports:       []int{443},
			ProxyListen: s.cfg.ProxyListen,
			ProxyCanary: canary,
		},
		CLIShim: CLIShim{
			ProxyAddr:   s.cfg.ProxyListen,
			Runtimes:    append([]string(nil), s.cfg.Runtimes...),
			NoProxy:     append([]string(nil), s.cfg.NoProxy...),
			NodeRequire: true,
		},
	}, nil
}

// mint signs the candidate as the next version: the larger of the previous version plus one and
// the current Unix time, so versions are monotonic per tenant and order after any bundle minted
// earlier with a Unix-time version.
func (s *Service) mint(tenantID string, latest *store.PolicyBundle, b Bundle, now time.Time) (store.MintDecision, error) {
	version := now.Unix()
	var previous int64
	if latest != nil {
		previous = latest.Version
		if latest.Version >= version {
			version = latest.Version + 1
		}
	}
	b.Version = strconv.FormatInt(version, 10)
	b.EffectiveAt = now
	b.Actor = actorID
	envelope, _, err := Sign(s.cfg.KeyID, s.key, b)
	if err != nil {
		return store.MintDecision{}, err
	}
	// The digest names the served bytes, the envelope, not the payload inside it: it is the identity
	// the audit trail and the schema's policy_bundle_digest_names_envelope check agree on.
	sum := sha256.Sum256(envelope)
	scope := map[string]any{"tenant_default": b.TenantDefault}
	if len(b.ToolModes) > 0 {
		scope["tool_modes"] = b.ToolModes
	}
	scopeJSON, err := json.Marshal(scope)
	if err != nil {
		return store.MintDecision{}, err
	}
	hosts := b.Interception.SeedHosts
	if hosts == nil {
		hosts = []string{}
	}
	allowJSON, err := json.Marshal(hosts)
	if err != nil {
		return store.MintDecision{}, err
	}
	featureJSON, err := json.Marshal(map[string]any{"cli_shim": true, "proxy_tls": true})
	if err != nil {
		return store.MintDecision{}, err
	}
	row := &store.PolicyBundle{
		TenantID:             tenantID,
		Version:              version,
		ScopeMatrix:          scopeJSON,
		DestinationAllowlist: allowJSON,
		FeatureState:         featureJSON,
		SpoolBounds:          json.RawMessage(`{}`),
		RetentionClass:       retentionClass,
		SignatureKID:         s.cfg.KeyID,
		SignedDigest:         "sha256:" + hex.EncodeToString(sum[:]),
		SignedEnvelope:       envelope,
		EffectiveFrom:        now,
		CreatedBy:            actorID,
	}
	audit := &store.AuditEntry{
		TenantID:   tenantID,
		ActorType:  store.ActorService,
		ActorID:    actorID,
		Action:     "policy_bundle.publish",
		ObjectType: "policy_bundle",
		ObjectID:   b.Version,
		OccurredAt: now,
		Detail: map[string]any{
			"previous_version":    previous,
			"tenant_default_mode": b.TenantDefault,
			"interception_hosts":  len(hosts),
			"signature_kid":       s.cfg.KeyID,
			"signed_digest":       row.SignedDigest,
		},
	}
	return store.MintDecision{Bundle: row, Audit: audit}, nil
}
