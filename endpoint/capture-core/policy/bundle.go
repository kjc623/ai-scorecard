// Package policy is the signed policy bundle: its shape, its verification chain
// (docs/01-collectors.md §13.2), and the rule of §13.3 that a bundle failing verification
// never changes what the device is enforcing.
//
// The one sentence this package exists to enforce: **no code path lets the absence of a
// valid bundle widen what the device may do.** A rejected bundle leaves the previous one in
// force; with no previous bundle the device runs at M0 (metadata only); there is no third
// branch, and no "skip verification" flag anywhere in the package.
package policy

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// KillSwitchMode is the closed set of kill-switch modes. `disable` is the only one this
// document defines (§5.5): the field exists so a fleet regression can stop enforcement
// without a software release.
type KillSwitchMode string

// KillDisable stops enforcement and interception on the next policy poll.
const KillDisable KillSwitchMode = "disable"

// Valid reports whether the mode is in the closed set.
func (k KillSwitchMode) Valid() bool { return k == KillDisable }

// KillSwitch is the §5.5 server-side kill switch. ReasonCode rides the next health report,
// so an operator seeing a coverage cliff can attribute it in one step.
type KillSwitch struct {
	Provider    protocol.Route `json:"provider"`
	Mode        KillSwitchMode `json:"mode"`
	EffectiveAt time.Time      `json:"effective_at"`
	ReasonCode  string         `json:"reason_code"`
}

// Interception is §5.1's enumeration: it decides *what we are willing to decrypt*, never
// what counts as generative. A destination in none of these sets is blind-tunnelled.
type Interception struct {
	TenantHosts []string `json:"tenant_hosts,omitempty"`
	SeedHosts   []string `json:"seed_hosts,omitempty"`

	// Ports are the TLS ports that may be intercepted. Non-443 ports are not intercepted by
	// default (§5.3); the set is per-tenant configurable.
	Ports []int `json:"ports,omitempty"`

	// BodyCapBytes bounds a request body held for classification. An over-cap body is not
	// read into memory: the proxy records size plus a digest of the first N bytes and marks
	// the event `confidence: degraded` (§5.3).
	BodyCapBytes int64 `json:"body_cap_bytes,omitempty"`

	// PromotionWindowSeconds is A5's bounded, device-local dynamic promotion window. Zero
	// disables promotion.
	PromotionWindowSeconds int `json:"promotion_window_seconds,omitempty"`

	// RootCAPEM is the per-device root CA the intercepting proxy mints leaves under, carried in
	// the signed bundle so an offline provisioned device is told which root to trust and to hand
	// the proxy (§4.5, §14). It is the public half; the private half never leaves the generator.
	RootCAPEM string `json:"root_ca_pem,omitempty"`

	// RootCAFingerprint is the lowercase-hex sha256 of RootCAPEM's DER, so the device can check
	// the right root is installed before trusting it.
	RootCAFingerprint string `json:"root_ca_fingerprint,omitempty"`

	// ProxyListen is the loopback address the proxy binds; the CLI shim points runtimes at it.
	ProxyListen string `json:"proxy_listen,omitempty"`

	// ProxyCanary is the end-to-end probe destination (host:port) the proxy handshakes against.
	ProxyCanary string `json:"proxy_canary,omitempty"`
}

// LoopbackPort is one row of §6's port map: which tool, which port the broker holds, where
// the relocated upstream now listens, and the read-only preflight path the tool documents as
// safe (A8). Ports are policy data, never a compiled-in list.
type LoopbackPort struct {
	ToolFingerprint string                  `json:"tool_fingerprint"`
	Port            int                     `json:"port"`
	UpstreamPort    int                     `json:"upstream_port"`
	PreflightPath   string                  `json:"preflight_path"`
	Mode            protocol.CollectionMode `json:"mode"`

	// OriginalPort is the value the tool had before relocation, recorded so uninstall
	// restores it exactly (§6.1, §6.2 rule 6).
	OriginalPort int `json:"original_port,omitempty"`
}

// LoopbackPolicy is §6.3's timings and §6.4's thresholds, all policy data so a bad
// configuration is fixed by a bundle rather than a release.
type LoopbackPolicy struct {
	Ports []LoopbackPort `json:"ports,omitempty"`

	ProbeIntervalSeconds     int `json:"probe_interval_seconds,omitempty"`     // cheap TCP liveness
	PreflightIntervalSeconds int `json:"preflight_interval_seconds,omitempty"` // full HTTP preflight
	PreflightTimeoutMS       int `json:"preflight_timeout_ms,omitempty"`       // single-digit seconds
	MaxConsecutiveFailures   int `json:"max_consecutive_failures,omitempty"`   // before cooling down
	CoolDownSeconds          int `json:"cool_down_seconds,omitempty"`          // long, bounded cool-down
}

// SpoolBounds is §12's bound: per-tenant tunable, enforced on write, defaulting to A16's
// ~25 MB / ~25,000 rows.
type SpoolBounds struct {
	MaxBytes             int64 `json:"max_bytes,omitempty"`
	MaxRows              int64 `json:"max_rows,omitempty"`
	DeviceRetentionHours int   `json:"device_retention_hours,omitempty"`
}

// ShapePredicate is §8.2's predicate parameters. They are bundle data because the predicate's
// scope decision (C7) must be changeable without shipping code.
type ShapePredicate struct {
	MinBodyBytes       int64    `json:"min_body_bytes,omitempty"`
	MaxBodyBytes       int64    `json:"max_body_bytes,omitempty"`
	Methods            []string `json:"methods,omitempty"`
	MediaTypes         []string `json:"media_types,omitempty"`
	UserAuthoredFields []string `json:"user_authored_fields,omitempty"`
}

// ClassifierRelease is §9.6's release state plus the artefact digests that must resolve
// before the release may be used (§13.2 step 4).
type ClassifierRelease struct {
	ReleaseID   string `json:"release_id"`
	State       string `json:"state"` // shadow | enforcing | rolled_back
	RulesDigest string `json:"rules_digest,omitempty"`
	ModelDigest string `json:"model_digest,omitempty"`
}

// Release states, §9.6.
const (
	ReleaseShadow     = "shadow"
	ReleaseEnforcing  = "enforcing"
	ReleaseRolledBack = "rolled_back"
)

// ArtefactRef names a signed artefact the bundle depends on (rules, model). Verification
// stops at the first unresolvable digest, so a bundle can never name a release the device
// cannot actually evaluate (§13.2).
type ArtefactRef struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

// CLIShimPolicy is §14's CLI trust shim configuration: it points the managed runtimes at the
// proxy and the per-device root CA, so command-line tooling is intercepted the same way browser
// traffic is, without a per-tool install step. It is bundle data, so the shim's target proxy and
// its runtime coverage are changed by a bundle rather than a release.
type CLIShimPolicy struct {
	Enabled bool `json:"enabled,omitempty"`

	// ProxyAddr is the loopback proxy the shim injects into managed runtimes.
	ProxyAddr string `json:"proxy_addr,omitempty"`

	// ManagedDir is the shim's managed directory, when the shim materialises its own profile
	// rather than editing the user's in place.
	ManagedDir string `json:"managed_dir,omitempty"`

	// Runtimes are the managed runtimes; a closed subset of {go, node, python}.
	Runtimes []string `json:"runtimes,omitempty"`

	// NoProxy lists destinations the shim must never route through the proxy.
	NoProxy []string `json:"no_proxy,omitempty"`

	// NodeRequire turns on the node require hook for runtime injection rather than environment
	// variables alone.
	NodeRequire bool `json:"node_require,omitempty"`
}

// Bundle is the device-side view of the signed policy bundle: the brief's list —
// classifier version, collection mode per scope, retention class, destination allowlist,
// spool bounds, feature state per collector — plus §13.2's additions.
//
// Unknown fields are rejected rather than ignored (see Open): a device that does not
// understand a policy field must not enforce a policy it has only partly read, and the
// rejection direction is "retain the previous bundle", which never widens anything.
type Bundle struct {
	Version     string    `json:"version"`
	EffectiveAt time.Time `json:"effective_at"`
	Actor       string    `json:"actor,omitempty"` // C4: every mode change records who and when

	// TenantDefault is what an observation with no matching scope entry resolves to. There is
	// no value of "unset" that means "everything" (§11.3).
	TenantDefault protocol.CollectionMode `json:"tenant_default_mode"`

	ToolModes       map[string]protocol.CollectionMode `json:"tool_modes,omitempty"`
	PopulationModes map[string]protocol.CollectionMode `json:"population_modes,omitempty"`
	DeviceModes     map[string]protocol.CollectionMode `json:"device_modes,omitempty"`

	// ClassModes is the per-class ceiling; ClassPriors is A14's class-prior map (per tool,
	// which classes are expected). The prior is what lets the mode be chosen before the
	// content is read.
	ClassModes  map[string]protocol.CollectionMode `json:"class_modes,omitempty"`
	ClassPriors map[string][]string                `json:"class_priors,omitempty"`

	// RequiredNoticeVersion is §13.4's notice gate. A user who has not acknowledged it
	// resolves to M0, with the reason reported rather than the downgrade being silent.
	RequiredNoticeVersion string            `json:"required_notice_version,omitempty"`
	AcknowledgedNotices   map[string]string `json:"acknowledged_notices,omitempty"`

	KillSwitches   []KillSwitch      `json:"kill_switches,omitempty"`
	Interception   Interception      `json:"interception"`
	Loopback       LoopbackPolicy    `json:"loopback"`
	ProcDetect     ProcDetectPolicy  `json:"proc_detect"`
	Spool          SpoolBounds       `json:"spool"`
	ShapePredicate ShapePredicate    `json:"shape_predicate"`
	Classifier     ClassifierRelease `json:"classifier"`
	CLIShim        CLIShimPolicy     `json:"cli_shim"`
	Artefacts      []ArtefactRef     `json:"artefacts,omitempty"`
}

// ProcDetectPolicy is §4.4's signed seed set: the inference-runtime signatures proc.detect
// matches against. It is bundle data rather than compiled in, so a new runtime becomes
// detectable without a software release — and so a signature the vendor gets wrong is fixable
// without one.
type ProcDetectPolicy struct {
	// ImageSignatures match a process image path (case-insensitive substring).
	ImageSignatures []string `json:"image_signatures,omitempty"`
	// ModuleSignatures match a loaded module name (§4.4's module_signature basis).
	ModuleSignatures []string `json:"module_signatures,omitempty"`
	// PortMap is the configured loopback port map: a process holding one of these ports is a
	// candidate, which is the "listening loopback socket" half of the detection rule.
	PortMap []PortMapEntry `json:"port_map,omitempty"`
	// MinComputePermille is the coarse compute signature above which a loaded runtime module
	// counts as sustained compute rather than an idle import. Zero disables that path.
	MinComputePermille int `json:"min_compute_permille,omitempty"`
	// SignatureVersion is reported so an operator can see which seed set the device holds.
	SignatureVersion string `json:"signature_version,omitempty"`
	// CycleIntervalSeconds and MissWindowSeconds shape the sampling cycle: the provider is
	// `absent` when a cycle misses its window (§4.4).
	CycleIntervalSeconds int `json:"cycle_interval_seconds,omitempty"`
	MissWindowSeconds    int `json:"miss_window_seconds,omitempty"`
	// RollupWindowSeconds is the usage_rollup window. §4.4 asks for one rollup per device, per
	// tool, per day; the default is 24 h and a tenant can shorten it, never lengthen the record
	// into per-cycle telemetry.
	RollupWindowSeconds int `json:"rollup_window_seconds,omitempty"`
}

// PortMapEntry is one loopback port a known inference runtime listens on.
type PortMapEntry struct {
	Port            int    `json:"port"`
	ToolFingerprint string `json:"tool_fingerprint"`
}

// KillSwitchFor returns the kill switch for a route, if one is in force.
func (b *Bundle) KillSwitchFor(r protocol.Route) (KillSwitch, bool) {
	if b == nil {
		return KillSwitch{}, false
	}
	for _, k := range b.KillSwitches {
		if k.Provider == r {
			return k, true
		}
	}
	return KillSwitch{}, false
}

// Intercepts reports whether a host:port is eligible for decryption (§5.1). The seed set is
// an interception scope, not a discovery mechanism: this decides only what we are willing to
// decrypt.
func (b *Bundle) Intercepts(host string, port int) bool {
	if b == nil {
		return false
	}
	if !b.portIntercepted(port) {
		return false
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	for _, cand := range b.Interception.TenantHosts {
		if hostMatches(h, cand) {
			return true
		}
	}
	for _, cand := range b.Interception.SeedHosts {
		if hostMatches(h, cand) {
			return true
		}
	}
	return false
}

func (b *Bundle) portIntercepted(port int) bool {
	if len(b.Interception.Ports) == 0 {
		return port == 443 // §5.3: 443 is the default; non-443 is opt-in per tenant
	}
	for _, p := range b.Interception.Ports {
		if p == port {
			return true
		}
	}
	return false
}

// hostMatches accepts an exact host or a leading-dot / wildcard suffix, because a customer
// allowlist has to be able to say ".example.com" and mean it.
func hostMatches(host, pattern string) bool {
	p := strings.ToLower(strings.TrimSpace(pattern))
	if p == "" {
		return false
	}
	p = strings.TrimPrefix(p, "*")
	switch {
	case strings.HasPrefix(p, "."):
		return strings.HasSuffix(host, p) || host == strings.TrimPrefix(p, ".")
	default:
		return host == p
	}
}

// LoopbackPortFor returns the policy row for a tool.
func (b *Bundle) LoopbackPortFor(tool string) (LoopbackPort, bool) {
	if b == nil {
		return LoopbackPort{}, false
	}
	for _, p := range b.Loopback.Ports {
		if p.ToolFingerprint == tool {
			return p, true
		}
	}
	return LoopbackPort{}, false
}

// Validate is §13.2's "schema valid?" step. It rejects anything the device cannot interpret
// rather than guessing: an unparseable mode is a scope entry that "fails to resolve", and
// §11.3 says a matrix that fails to resolve resolves downward — which this package enforces
// by refusing the bundle outright, so the previous bundle (or M0) stays in force.
func (b *Bundle) Validate() error {
	if b == nil {
		return fmt.Errorf("policy: nil bundle")
	}
	if strings.TrimSpace(b.Version) == "" {
		return fmt.Errorf("policy: bundle has no version; an unversioned bundle cannot be ordered against the one in force")
	}
	if b.EffectiveAt.IsZero() {
		return fmt.Errorf("policy: bundle has no effective_at; C4 requires every mode change to record who and when")
	}
	if err := validMode("tenant_default_mode", b.TenantDefault); err != nil {
		return err
	}
	for _, m := range []struct {
		name string
		set  map[string]protocol.CollectionMode
	}{
		{"tool_modes", b.ToolModes},
		{"population_modes", b.PopulationModes},
		{"device_modes", b.DeviceModes},
		{"class_modes", b.ClassModes},
	} {
		keys := make([]string, 0, len(m.set))
		for k := range m.set {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic error for a deterministic test
		for _, k := range keys {
			if strings.TrimSpace(k) == "" {
				return fmt.Errorf("policy: %s has an empty scope key", m.name)
			}
			if err := validMode(m.name+"["+k+"]", m.set[k]); err != nil {
				return err
			}
		}
	}
	for tool, classes := range b.ClassPriors {
		if strings.TrimSpace(tool) == "" {
			return fmt.Errorf("policy: class_priors has an empty tool key")
		}
		for _, c := range classes {
			if strings.TrimSpace(c) == "" {
				return fmt.Errorf("policy: class_priors[%s] names an empty class", tool)
			}
		}
	}
	for _, k := range b.KillSwitches {
		if !k.Provider.Valid() {
			return fmt.Errorf("policy: kill switch names route %q outside the closed vocabulary", k.Provider)
		}
		if !k.Mode.Valid() {
			return fmt.Errorf("policy: kill switch for %s has mode %q outside the closed set", k.Provider, k.Mode)
		}
		if k.EffectiveAt.IsZero() {
			return fmt.Errorf("policy: kill switch for %s has no effective_at", k.Provider)
		}
		if strings.TrimSpace(k.ReasonCode) == "" {
			return fmt.Errorf("policy: kill switch for %s has no reason_code; the operator has to be able to attribute the cliff", k.Provider)
		}
	}
	for _, p := range b.Interception.Ports {
		if !validPort(p) {
			return fmt.Errorf("policy: interception port %d is not a usable TCP port", p)
		}
	}
	if b.Interception.BodyCapBytes < 0 {
		return fmt.Errorf("policy: interception body cap is negative")
	}
	if b.Interception.PromotionWindowSeconds < 0 {
		return fmt.Errorf("policy: promotion window is negative")
	}
	if b.Interception.RootCAPEM != "" || b.Interception.RootCAFingerprint != "" {
		der, err := rootCADER(b.Interception.RootCAPEM)
		if err != nil {
			return err
		}
		if b.Interception.RootCAFingerprint != "" {
			want := sha256hex(der)
			if b.Interception.RootCAFingerprint != want {
				return fmt.Errorf("policy: interception root_ca_fingerprint %q does not match the sha256 of root_ca_pem (%q)", b.Interception.RootCAFingerprint, want)
			}
		}
	}
	if b.Interception.ProxyListen != "" && !validHostPort(b.Interception.ProxyListen) {
		return fmt.Errorf("policy: interception proxy_listen %q is not a usable host:port", b.Interception.ProxyListen)
	}
	if b.CLIShim.ProxyAddr != "" && !validHostPort(b.CLIShim.ProxyAddr) {
		return fmt.Errorf("policy: cli_shim proxy_addr %q is not a usable host:port", b.CLIShim.ProxyAddr)
	}
	for _, r := range b.CLIShim.Runtimes {
		switch r {
		case "go", "node", "python":
		default:
			return fmt.Errorf("policy: cli_shim names runtime %q outside the set {go,node,python}", r)
		}
	}
	for _, p := range b.Loopback.Ports {
		if strings.TrimSpace(p.ToolFingerprint) == "" {
			return fmt.Errorf("policy: loopback port entry has no tool fingerprint")
		}
		if !validPort(p.Port) {
			return fmt.Errorf("policy: loopback port %d for %s is not a usable TCP port", p.Port, p.ToolFingerprint)
		}
		if !validPort(p.UpstreamPort) {
			return fmt.Errorf("policy: loopback upstream port %d for %s is not a usable TCP port", p.UpstreamPort, p.ToolFingerprint)
		}
		if p.Port == p.UpstreamPort {
			return fmt.Errorf("policy: loopback entry for %s holds and forwards to port %d; a broker that forwards to itself is a loop", p.ToolFingerprint, p.Port)
		}
		if err := validMode("loopback["+p.ToolFingerprint+"]", p.Mode); err != nil {
			return err
		}
	}
	if b.Spool.MaxBytes < 0 || b.Spool.MaxRows < 0 || b.Spool.DeviceRetentionHours < 0 {
		return fmt.Errorf("policy: spool bounds are negative")
	}
	for _, sig := range append(append([]string(nil), b.ProcDetect.ImageSignatures...), b.ProcDetect.ModuleSignatures...) {
		if strings.TrimSpace(sig) == "" {
			return fmt.Errorf("policy: proc_detect has an empty runtime signature; an empty substring matches every process")
		}
	}
	for _, pm := range b.ProcDetect.PortMap {
		if !validPort(pm.Port) {
			return fmt.Errorf("policy: proc_detect port map names port %d, which is not a usable TCP port", pm.Port)
		}
		if strings.TrimSpace(pm.ToolFingerprint) == "" {
			return fmt.Errorf("policy: proc_detect port map entry for port %d has no tool fingerprint", pm.Port)
		}
	}
	if b.ProcDetect.MinComputePermille < 0 || b.ProcDetect.MinComputePermille > 1000 {
		return fmt.Errorf("policy: proc_detect min_compute_permille %d is outside 0..1000", b.ProcDetect.MinComputePermille)
	}
	if b.ProcDetect.CycleIntervalSeconds < 0 || b.ProcDetect.MissWindowSeconds < 0 || b.ProcDetect.RollupWindowSeconds < 0 {
		return fmt.Errorf("policy: proc_detect intervals are negative")
	}
	switch b.Classifier.State {
	case ReleaseShadow, ReleaseEnforcing, ReleaseRolledBack:
	default:
		return fmt.Errorf("policy: classifier release state %q outside the closed set", b.Classifier.State)
	}
	if strings.TrimSpace(b.Classifier.ReleaseID) == "" {
		return fmt.Errorf("policy: classifier release has no release_id; a classification must be attributable to a release")
	}
	for _, a := range b.Artefacts {
		if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Path) == "" || strings.TrimSpace(a.Digest) == "" {
			return fmt.Errorf("policy: artefact %q must carry a name, a path and a digest", a.Name)
		}
	}
	return nil
}

func validMode(where string, m protocol.CollectionMode) error {
	if !m.Valid() {
		return fmt.Errorf("policy: %s has mode %q outside the closed set {m0,m1,m2,m3}", where, m)
	}
	return nil
}

func validPort(p int) bool {
	return p > 0 && p <= 65535 && net.JoinHostPort("127.0.0.1", fmt.Sprint(p)) != ""
}

// validHostPort reports whether hp is a usable host:port: a non-empty host and a TCP port in
// range. net.SplitHostPort also accepts a bracketed IPv6 literal.
func validHostPort(hp string) bool {
	host, portStr, err := net.SplitHostPort(hp)
	if err != nil {
		return false
	}
	if strings.TrimSpace(host) == "" {
		return false
	}
	port, err := strconv.Atoi(portStr)
	return err == nil && port > 0 && port <= 65535
}

// sha256hex is the lowercase-hex sha256 used for the root CA fingerprint.
func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:])
}

// rootCADER parses RootCAPEM strictly: exactly one PEM x509 certificate, nothing else. It returns
// the DER, which the fingerprint rule checks against.
func rootCADER(pemData string) ([]byte, error) {
	rest := []byte(pemData)
	var der []byte
	blocks := 0
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		blocks++
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("policy: root_ca_pem block %d is %q, not a certificate", blocks, block.Type)
		}
		der = block.Bytes
	}
	if blocks == 0 {
		return nil, fmt.Errorf("policy: root_ca_pem carries no PEM certificate")
	}
	if blocks > 1 {
		return nil, fmt.Errorf("policy: root_ca_pem carries %d certificates; exactly one is required", blocks)
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("policy: root_ca_pem has trailing data after the certificate")
	}
	if _, err := x509.ParseCertificate(der); err != nil {
		return nil, fmt.Errorf("policy: root_ca_pem is not a parseable x509 certificate: %w", err)
	}
	return der, nil
}

// decode parses a bundle payload strictly. Unknown fields are an error: a device that does
// not understand a policy field cannot claim to be enforcing the bundle.
func decode(payload []byte) (*Bundle, error) {
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.DisallowUnknownFields()
	var b Bundle
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("policy: bundle payload is not a bundle this device understands: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("policy: bundle payload has trailing data after the bundle object")
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return &b, nil
}
