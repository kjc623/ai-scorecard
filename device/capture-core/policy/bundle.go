// Package policy is the signed policy bundle: its shape, its verification chain, and the rule
// that a bundle failing verification never changes what the device is enforcing.
//
// No code path lets the absence of a valid bundle widen what the device may do. A rejected
// bundle leaves the previous one in force; with no previous bundle the device runs at M0
// (metadata only); there is no third branch and no way to skip verification.
package policy

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// KillSwitchMode is the closed set of kill-switch modes. `disable` is the only one this
// document defines: the field exists so a fleet regression can stop enforcement
// without a software release.
type KillSwitchMode string

// KillDisable stops enforcement and interception on the next policy poll.
const KillDisable KillSwitchMode = "disable"

// Valid reports whether the mode is in the closed set.
func (k KillSwitchMode) Valid() bool { return k == KillDisable }

// KillSwitch is the server-side kill switch. ReasonCode rides the next health report,
// so an operator seeing a coverage cliff can attribute it in one step.
type KillSwitch struct {
	Provider    protocol.Route `json:"provider"`
	Mode        KillSwitchMode `json:"mode"`
	EffectiveAt time.Time      `json:"effective_at"`
	ReasonCode  string         `json:"reason_code"`
}

// Interception decides what the device is willing to decrypt, never
// what counts as generative. A destination in none of these sets is blind-tunnelled.
type Interception struct {
	TenantHosts []string `json:"tenant_hosts,omitempty"`
	SeedHosts   []string `json:"seed_hosts,omitempty"`

	// Ports are the TLS ports that may be intercepted. Non-443 ports are not intercepted by
	// default; the set is per-tenant configurable.
	Ports []int `json:"ports,omitempty"`

	// BodyCapBytes bounds a request body held for classification. An over-cap body is not
	// read into memory: the proxy records size plus a digest of the first N bytes and marks
	// the event `confidence: degraded`.
	BodyCapBytes int64 `json:"body_cap_bytes,omitempty"`

	// ProxyListen is the loopback address the proxy binds; the CLI shim points runtimes at it.
	ProxyListen string `json:"proxy_listen,omitempty"`

	// ProxyCanary is the end-to-end probe destination (host:port) the proxy handshakes against.
	ProxyCanary string `json:"proxy_canary,omitempty"`
}

// LoopbackPort is one row of the loopback port map: which tool, which port the broker holds, where
// the relocated upstream now listens, and the read-only preflight path the tool documents as
// safe. Ports are policy data, never a compiled-in list.
type LoopbackPort struct {
	ToolFingerprint string                  `json:"tool_fingerprint"`
	Port            int                     `json:"port"`
	UpstreamPort    int                     `json:"upstream_port"`
	PreflightPath   string                  `json:"preflight_path"`
	Mode            protocol.CollectionMode `json:"mode"`

	// OriginalPort is the value the tool had before relocation, recorded so uninstall
	// restores it exactly.
	OriginalPort int `json:"original_port,omitempty"`
}

// LoopbackPolicy is the broker's timings and failure thresholds, all policy data so a bad
// configuration is fixed by a bundle rather than a release.
type LoopbackPolicy struct {
	Ports []LoopbackPort `json:"ports,omitempty"`

	ProbeIntervalSeconds     int `json:"probe_interval_seconds,omitempty"`     // cheap TCP liveness
	PreflightIntervalSeconds int `json:"preflight_interval_seconds,omitempty"` // full HTTP preflight
	PreflightTimeoutMS       int `json:"preflight_timeout_ms,omitempty"`       // single-digit seconds
	MaxConsecutiveFailures   int `json:"max_consecutive_failures,omitempty"`   // before cooling down
	CoolDownSeconds          int `json:"cool_down_seconds,omitempty"`          // long, bounded cool-down
}

// SpoolPolicy is the tenant's device-side retention for spooled observations and held content.
// Zero keeps the agent's default.
type SpoolPolicy struct {
	DeviceRetentionHours int `json:"device_retention_hours,omitempty"`
}

// CLIShimPolicy is the CLI trust shim configuration: it points the managed runtimes at the
// proxy and the per-device root CA, so command-line tooling is intercepted the same way browser
// traffic is, without a per-tool install step. The shim's directory is the agent's own,
// per platform; the bundle decides only what the shim exports.
type CLIShimPolicy struct {
	// ProxyAddr is the loopback proxy the shim injects into managed runtimes.
	ProxyAddr string `json:"proxy_addr,omitempty"`

	// Runtimes are the managed runtimes; a closed subset of {go, node, python}.
	Runtimes []string `json:"runtimes,omitempty"`

	// NoProxy lists destinations the shim must never route through the proxy.
	NoProxy []string `json:"no_proxy,omitempty"`

	// NodeRequire turns on the node require hook for runtime injection rather than environment
	// variables alone.
	NodeRequire bool `json:"node_require,omitempty"`
}

// Bundle is the device-side view of the signed policy bundle: the collection mode per scope,
// the interception allowlist, the loopback port map, kill switches, device retention and the
// CLI shim configuration.
//
// Unknown fields are rejected rather than ignored (see Open): a device that does not
// understand a policy field must not enforce a policy it has only partly read, and the
// rejection direction is "retain the previous bundle", which never widens anything.
type Bundle struct {
	Version     string    `json:"version"`
	EffectiveAt time.Time `json:"effective_at"`
	Actor       string    `json:"actor,omitempty"` // Every mode change records who and when

	// TenantDefault is what an observation with no matching scope entry resolves to. There is
	// no value of "unset" that means "everything".
	TenantDefault protocol.CollectionMode `json:"tenant_default_mode"`

	ToolModes       map[string]protocol.CollectionMode `json:"tool_modes,omitempty"`
	PopulationModes map[string]protocol.CollectionMode `json:"population_modes,omitempty"`
	DeviceModes     map[string]protocol.CollectionMode `json:"device_modes,omitempty"`

	// ClassModes is the per-class ceiling; ClassPriors is the class-prior map (per tool,
	// which classes are expected). The prior is what lets the mode be chosen before the
	// content is read.
	ClassModes  map[string]protocol.CollectionMode `json:"class_modes,omitempty"`
	ClassPriors map[string][]string                `json:"class_priors,omitempty"`

	// RequiredNoticeVersion is the notice gate. A user who has not acknowledged it
	// resolves to M0, with the reason reported rather than the downgrade being silent.
	RequiredNoticeVersion string            `json:"required_notice_version,omitempty"`
	AcknowledgedNotices   map[string]string `json:"acknowledged_notices,omitempty"`

	KillSwitches []KillSwitch   `json:"kill_switches,omitempty"`
	Interception Interception   `json:"interception"`
	Loopback     LoopbackPolicy `json:"loopback"`
	Spool        SpoolPolicy    `json:"spool"`
	CLIShim      CLIShimPolicy  `json:"cli_shim"`
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

// Intercepts reports whether a host:port is eligible for decryption. The seed set is
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
		return port == 443 // 443 is the default; non-443 is opt-in per tenant
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

// Validate is the schema step of verification. It rejects anything the device cannot interpret
// rather than guessing: an unparseable mode is a scope entry that fails to resolve, and a matrix
// that fails to resolve must resolve downward, which this package enforces by refusing the
// bundle outright, so the previous bundle (or M0) stays in force.
func (b *Bundle) Validate() error {
	if b == nil {
		return fmt.Errorf("policy: nil bundle")
	}
	if strings.TrimSpace(b.Version) == "" {
		return fmt.Errorf("policy: bundle has no version; an unversioned bundle cannot be ordered against the one in force")
	}
	if b.EffectiveAt.IsZero() {
		return fmt.Errorf("policy: bundle has no effective_at; every mode change records who and when")
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
	if b.Interception.ProxyListen != "" && !validHostPort(b.Interception.ProxyListen) {
		return fmt.Errorf("policy: interception proxy_listen %q is not a usable host:port", b.Interception.ProxyListen)
	}
	// A malformed canary is not a rejection of interception, but it silently degrades the
	// end-to-end probe, so the bundle that names it must be well formed.
	if b.Interception.ProxyCanary != "" && !validHostPort(b.Interception.ProxyCanary) {
		return fmt.Errorf("policy: interception proxy_canary %q is not a usable host:port", b.Interception.ProxyCanary)
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
	if b.Spool.DeviceRetentionHours < 0 {
		return fmt.Errorf("policy: spool device_retention_hours is negative")
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
