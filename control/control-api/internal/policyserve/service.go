// Package policyserve is GET /v1/policy (docs/02-ingest-and-transport.md §5.2) and the writer of
// ops.policy_bundle that the backlog has lacked (FOLLOWUPS: "nothing writes the signed policy
// bundle").
//
// A tenant's bundle is composed from what the database says about it -- the tenant's ceiling as the
// default collection mode, the tool catalogue's TLS hosts as the interception scope, the servable
// classifier release -- in the shape the lab's sac-bundle has always produced, signed with the
// vendor's Ed25519 policy key in the envelope capture-core/policy verifies, and stored with the
// exact signed bytes in ops.policy_bundle.signed_envelope. A new version is minted only when that
// composition, or the signing key, differs from the latest stored bundle; otherwise the stored bytes
// are served again, so the ETag a device holds stays valid until something it would enforce changes.
//
// Composition happens on the read path, under a per-tenant lock, because no settings writer exists
// yet: the first poll after an input changes mints the new version. A settings writer can call
// Refresh after its own write to mint eagerly; the outcome is the same version either way.
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

// Config is the service's composition defaults and timing. Every default is what the lab's
// installer passes sac-bundle today, so an enterprise device and a lab device are configured alike.
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
	// default, until a retention setting exists to choose another.
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
	store store.PolicyStore
	key   ed25519.PrivateKey
	cfg   Config

	mu    sync.Mutex
	cache map[string]cached
}

// New builds the service. It refuses a missing store or key rather than serving unsigned policy:
// only signed bundles are ever served (docs/02 §5.2).
func New(st store.PolicyStore, key ed25519.PrivateKey, cfg Config) (*Service, error) {
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
	if s == nil {
		// A nil *Service stored in an interface is not a nil interface; answering here keeps a
		// deployment without a signing key from panicking on every enrolment.
		return "", errors.New("policyserve: policy signing is not configured")
	}
	sv, err := s.Current(ctx, tenantID)
	if err != nil {
		return "", err
	}
	return sv.ETag, nil
}

// Refresh re-reads the tenant's inputs now, minting a new version if they changed. A settings
// writer calls it after its own commit so devices see the change on their next poll.
func (s *Service) Refresh(ctx context.Context, tenantID string) (Served, error) {
	s.mu.Lock()
	delete(s.cache, tenantID)
	s.mu.Unlock()
	return s.Current(ctx, tenantID)
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
		if s.current(latest, candidate) || candidate == nil {
			return store.MintDecision{}, nil
		}
		d, err := s.mint(tenantID, latest, *candidate, in, now)
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
			"kid", minted.SignatureKID, "classifier", minted.ClassifierRelease)
	}
	return s.remember(tenantID, minted, now)
}

// current reports whether latest may be served for this candidate: it is servable, signed with
// the current key, and composed from the same inputs. A nil candidate (nothing composable) keeps
// any servable latest rather than withdrawing policy.
func (s *Service) current(latest *store.PolicyBundle, candidate *Bundle) bool {
	if latest == nil || len(latest.SignedEnvelope) == 0 {
		return false
	}
	if candidate == nil {
		return true
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

// compose builds the candidate bundle from the inputs, or nil when no bundle can be written (no
// servable classifier release: ops.policy_bundle names one and the device requires one).
func (s *Service) compose(in store.PolicyInputs) (*Bundle, error) {
	if in.Classifier == nil {
		return nil, nil
	}
	mode := protocol.CollectionMode(in.Tenant.CeilingMode)
	if !mode.Valid() {
		// The ceiling CHECK makes this unreachable from the database; refusing beats guessing.
		return nil, fmt.Errorf("tenant ceiling_mode %q is outside {m0,m1,m2,m3}", in.Tenant.CeilingMode)
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
	state := in.Classifier.State
	if state != "shadow" && state != "enforcing" && state != "rolled_back" {
		return nil, fmt.Errorf("classifier release %q is in state %q, which a bundle cannot name", in.Classifier.Version, state)
	}
	return &Bundle{
		TenantDefault: string(mode),
		Interception: Interception{
			SeedHosts:   hosts,
			Ports:       []int{443},
			ProxyListen: s.cfg.ProxyListen,
			ProxyCanary: canary,
		},
		Classifier: ClassifierRelease{ReleaseID: in.Classifier.Version, State: state},
		CLIShim: CLIShim{
			Enabled:     true,
			ProxyAddr:   s.cfg.ProxyListen,
			Runtimes:    append([]string(nil), s.cfg.Runtimes...),
			NoProxy:     append([]string(nil), s.cfg.NoProxy...),
			NodeRequire: true,
		},
	}, nil
}

// mint signs the candidate as the next version. The version is the larger of the previous one plus
// one and the current Unix time, so it is monotonic per tenant and also orders after a bundle the
// lab's installer minted (sac-bundle versions are Unix seconds) -- a device moving from a lab
// bundle to a served one sees an upgrade, never a regression it must refuse.
func (s *Service) mint(tenantID string, latest *store.PolicyBundle, b Bundle, in store.PolicyInputs, now time.Time) (store.MintDecision, error) {
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
	featureJSON, err := json.Marshal(map[string]any{"cli_shim": b.CLIShim.Enabled, "proxy_tls": true})
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
		ClassifierRelease:    in.Classifier.Version,
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
			"classifier_release":  in.Classifier.Version,
			"signature_kid":       s.cfg.KeyID,
			"signed_digest":       row.SignedDigest,
		},
	}
	return store.MintDecision{Bundle: row, Audit: audit}, nil
}
