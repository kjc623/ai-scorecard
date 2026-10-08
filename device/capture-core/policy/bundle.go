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
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
	// Enabled is the tenant's TLS inspection setting. While it is false the device runs no TLS
	// proxy, writes no CLI shim environment, sets no desktop-app PAC and keeps its root out of the
	// trust store, whatever the other fields say. A bundle without it is off.
	Enabled bool `json:"enabled"`

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

	// PacListen is the loopback address the desktop-app PAC is served on. The PAC URL is written
	// into each signed-in user's Internet Settings, so it must be a fixed loopback port, never
	// ":0". Empty means the desktop-app capture path is off.
	PacListen string `json:"pac_listen,omitempty"`
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

// EndpointToolKeys is the closed set of tools with native collectors, the keys of
// EndpointPolicy.Tools.
var EndpointToolKeys = []string{"claude_code", "codex", "copilot", "cursor"}

// MinInventoryIntervalMinutes is the shortest installed-app rescan interval a bundle may set.
const MinInventoryIntervalMinutes = 15

// EndpointPolicy switches the endpoint collectors on and off and carries the values they run with.
// A bundle without the section switches every one of them off.
type EndpointPolicy struct {
	Inventory EndpointInventory `json:"inventory"`
	Processes EndpointSwitch    `json:"processes"`
	Flows     EndpointSwitch    `json:"flows"`
	OTel      EndpointOTel      `json:"otel"`
	Hooks     EndpointHooks     `json:"hooks"`

	// Tools is each tool's native collectors, keyed by a key of EndpointToolKeys. A tool's switch
	// takes effect only while the collector it names is enabled.
	Tools map[string]EndpointTool `json:"tools"`

	// DiscoveryDailyBudget is how many discovery records may leave the device per UTC day.
	DiscoveryDailyBudget int `json:"discovery_daily_budget"`
}

// EndpointSwitch is a collector with no setting beyond on or off.
type EndpointSwitch struct {
	Enabled bool `json:"enabled"`
}

// EndpointInventory is the installed-app scanner and how often it rescans.
type EndpointInventory struct {
	Enabled         bool `json:"enabled"`
	IntervalMinutes int  `json:"interval_minutes"`
}

// EndpointOTel is the OTLP receiver. Both listen addresses are loopback host:port.
type EndpointOTel struct {
	Enabled    bool   `json:"enabled"`
	HTTPListen string `json:"http_listen"`
	GRPCListen string `json:"grpc_listen"`
}

// EndpointHooks is the hook relay. ManagedOnly makes tools run only the hooks the agent manages.
type EndpointHooks struct {
	Enabled     bool `json:"enabled"`
	ManagedOnly bool `json:"managed_only"`
}

// EndpointTool is one tool's native collectors.
type EndpointTool struct {
	OTel  bool `json:"otel"`
	Hooks bool `json:"hooks"`
}

// RuleAction is what an enforcement rule does to a matching submission.
type RuleAction string

// The closed set of rule actions. The envelope records allow as logged, warn as warned and block
// as blocked.
const (
	RuleAllow RuleAction = "allow"
	RuleWarn  RuleAction = "warn"
	RuleBlock RuleAction = "block"
)

// Valid reports whether the action is in the closed set.
func (a RuleAction) Valid() bool { return a == RuleAllow || a == RuleWarn || a == RuleBlock }

// MaxRuleMessage is the longest message a rule may carry, in characters.
const MaxRuleMessage = 280

var ruleIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)

// Rule is one enforcement rule. The first rule in the bundle's order whose match matches decides;
// no match records logged under the rule id policy.default.
type Rule struct {
	RuleID  string     `json:"rule_id"`
	Action  RuleAction `json:"action"`
	Match   RuleMatch  `json:"match"`
	Message string     `json:"message"`
	// Link is an https URL shown with the message, or empty.
	Link string `json:"link,omitempty"`
}

// RuleMatch is a rule's conditions. Every non-empty list must match; an empty list matches
// anything; inside a list any value matches. Labels are classifier classes, Tools tool
// fingerprints, Categories app catalog categories, and Sanction sanctioned or unsanctioned, decided
// against the bundle's SanctionedTools.
type RuleMatch struct {
	Labels     []string         `json:"labels"`
	Tools      []string         `json:"tools"`
	Categories []string         `json:"categories"`
	Sanction   []string         `json:"sanction"`
	Routes     []protocol.Route `json:"routes"`
}

// Bundle is the device-side view of the signed policy bundle: the collection mode per scope,
// the interception allowlist, the loopback port map, kill switches, device retention, the
// CLI shim configuration, the endpoint collectors, the enforcement rules and the sanctioned
// tools.
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
	Endpoint     EndpointPolicy `json:"endpoint"`

	// Rules is the enforcement rules in order.
	Rules []Rule `json:"rules"`
	// SanctionedTools is the tool fingerprints the tenant has sanctioned.
	SanctionedTools []string `json:"sanctioned_tools"`
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
	// The PAC listen address is written into each user's Internet Settings, so a malformed one
	// would break desktop-app routing rather than merely degrade a probe.
	if b.Interception.PacListen != "" && !validHostPort(b.Interception.PacListen) {
		return fmt.Errorf("policy: interception pac_listen %q is not a usable host:port", b.Interception.PacListen)
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
	if err := validRules(b.Rules); err != nil {
		return err
	}
	return b.Endpoint.validate()
}

// validRules refuses a rule list the device could not apply as written. Positions in the errors
// are 1-based; no message or link text is quoted.
func validRules(rules []Rule) error {
	seen := make(map[string]bool, len(rules))
	for i, r := range rules {
		if !ruleIDPattern.MatchString(r.RuleID) {
			return fmt.Errorf("policy: rule %d has rule_id %q outside the pattern %s", i+1, r.RuleID, ruleIDPattern)
		}
		if seen[r.RuleID] {
			return fmt.Errorf("policy: rule %d repeats rule_id %q", i+1, r.RuleID)
		}
		seen[r.RuleID] = true
		if !r.Action.Valid() {
			return fmt.Errorf("policy: rule %s has action %q outside the set {allow,warn,block}", r.RuleID, r.Action)
		}
		if utf8.RuneCountInString(r.Message) > MaxRuleMessage {
			return fmt.Errorf("policy: rule %s has a message over %d characters", r.RuleID, MaxRuleMessage)
		}
		if r.Link != "" && !httpsURL(r.Link) {
			return fmt.Errorf("policy: rule %s has a link that is not an https URL", r.RuleID)
		}
		for _, route := range r.Match.Routes {
			if !route.Valid() {
				return fmt.Errorf("policy: rule %s matches route %q outside the closed vocabulary", r.RuleID, route)
			}
		}
	}
	return nil
}

func httpsURL(s string) bool {
	if !strings.HasPrefix(s, "https://") {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

func (e *EndpointPolicy) validate() error {
	inv := e.Inventory
	if (inv.Enabled || inv.IntervalMinutes != 0) && inv.IntervalMinutes < MinInventoryIntervalMinutes {
		return fmt.Errorf("policy: endpoint inventory interval_minutes %d is below %d", inv.IntervalMinutes, MinInventoryIntervalMinutes)
	}
	for _, l := range []struct{ name, addr string }{
		{"http_listen", e.OTel.HTTPListen},
		{"grpc_listen", e.OTel.GRPCListen},
	} {
		if l.addr == "" && !e.OTel.Enabled {
			continue
		}
		if !loopbackHostPort(l.addr) {
			return fmt.Errorf("policy: endpoint otel %s %q is not a loopback IP host:port", l.name, l.addr)
		}
	}
	keys := make([]string, 0, len(e.Tools))
	for k := range e.Tools {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !slices.Contains(EndpointToolKeys, k) {
			return fmt.Errorf("policy: endpoint tools names %q outside the set {%s}", k, strings.Join(EndpointToolKeys, ","))
		}
	}
	if e.DiscoveryDailyBudget < 0 {
		return fmt.Errorf("policy: endpoint discovery_daily_budget is negative")
	}
	return nil
}

// loopbackHostPort reports whether hp is a loopback IP literal and a TCP port in range. A name such
// as localhost is refused: what it resolves to is the host's configuration, not the bundle's.
func loopbackHostPort(hp string) bool {
	if !validHostPort(hp) {
		return false
	}
	host, _, _ := net.SplitHostPort(hp)
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
