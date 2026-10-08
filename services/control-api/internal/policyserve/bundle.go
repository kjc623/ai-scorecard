package policyserve

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// Bundle is the subset of the agent's policy bundle this service composes, with the same JSON
// names. The agent decodes the payload with unknown fields refused, so a field here it does not know
// would make every served bundle unenforceable; TestServedBundleVerifiesWithTheDevicesVerifier runs
// the agent's own verifier over this package's output to catch that drift.
type Bundle struct {
	Version     string    `json:"version"`
	EffectiveAt time.Time `json:"effective_at"`
	Actor       string    `json:"actor,omitempty"`

	TenantDefault string `json:"tenant_default_mode"`

	// ToolModes is the tenant's narrower per-tool collection modes, keyed by tool fingerprint.
	ToolModes map[string]string `json:"tool_modes,omitempty"`

	Interception Interception `json:"interception"`
	CLIShim      CLIShim      `json:"cli_shim"`
	Endpoint     Endpoint     `json:"endpoint"`

	// Rules is the tenant's enforcement rules in order; the first that matches decides. Never
	// omitted, so the drift test sees the name.
	Rules []Rule `json:"rules"`
	// SanctionedTools is the tool fingerprints the tenant has sanctioned, sorted: what a rule's
	// sanction list is decided against.
	SanctionedTools []string `json:"sanctioned_tools"`
}

// Rule is one enforcement rule. Action is allow, warn or block.
type Rule struct {
	RuleID  string    `json:"rule_id"`
	Action  string    `json:"action"`
	Match   RuleMatch `json:"match"`
	Message string    `json:"message"`
	Link    string    `json:"link,omitempty"`
}

// RuleMatch is a rule's conditions: every non-empty list must match, with OR inside a list. Every
// list is sent, empty when it matches anything.
type RuleMatch struct {
	Labels     []string `json:"labels"`
	Tools      []string `json:"tools"`
	Categories []string `json:"categories"`
	Sanction   []string `json:"sanction"`
	Routes     []string `json:"routes"`
}

// Interception is the decryption scope. The per-device root CA is deliberately absent: the device
// generates its own and the server never holds it. Enabled is the tenant's TLS inspection setting:
// without it the device runs no proxy, shim environment or desktop-app PAC and trusts no root of its
// own, whatever the other fields say.
type Interception struct {
	Enabled     bool     `json:"enabled"`
	SeedHosts   []string `json:"seed_hosts,omitempty"`
	Ports       []int    `json:"ports,omitempty"`
	ProxyListen string   `json:"proxy_listen,omitempty"`
	ProxyCanary string   `json:"proxy_canary,omitempty"`
}

// CLIShim is the CLI trust shim's configuration. The shim is always on and its managed directory is
// the device's own per-platform default, so one bundle serves every platform.
type CLIShim struct {
	ProxyAddr   string   `json:"proxy_addr,omitempty"`
	Runtimes    []string `json:"runtimes,omitempty"`
	NoProxy     []string `json:"no_proxy,omitempty"`
	NodeRequire bool     `json:"node_require,omitempty"`
}

// Endpoint is the endpoint collectors' switches, from the tenant's settings, and the values they run
// with, which are this service's constants. No field is omitted when false or empty, so the drift
// test sees every name.
type Endpoint struct {
	Inventory EndpointInventory `json:"inventory"`
	Processes EndpointSwitch    `json:"processes"`
	Flows     EndpointSwitch    `json:"flows"`
	OTel      EndpointOTel      `json:"otel"`
	Hooks     EndpointHooks     `json:"hooks"`
	// Tools is each tool's native collectors, keyed by tool key. A tool's switch takes effect only
	// while the collector it names is enabled.
	Tools                map[string]EndpointTool `json:"tools"`
	DiscoveryDailyBudget int                     `json:"discovery_daily_budget"`
}

// EndpointSwitch is a collector with no setting beyond on or off.
type EndpointSwitch struct {
	Enabled bool `json:"enabled"`
}

// EndpointInventory is the installed-app scanner.
type EndpointInventory struct {
	Enabled         bool `json:"enabled"`
	IntervalMinutes int  `json:"interval_minutes"`
}

// EndpointOTel is the device's OTLP receiver and its loopback listen addresses.
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

// SignedBundle is the envelope capture-core/policy verifies: the Ed25519 signature covers the
// payload bytes exactly as they appear here, never a re-serialisation.
type SignedBundle struct {
	KeyID     string          `json:"key_id"`
	Algorithm string          `json:"algorithm"`
	Payload   json.RawMessage `json:"payload"`
	Signature string          `json:"signature"`
}

// Sign marshals the bundle, signs the bytes, and returns the envelope and the payload. It mirrors
// capture-core/policy.Sign: json.Marshal's output (compact, HTML-escaped) is both what is signed
// and what is embedded, so re-encoding the envelope cannot change the signed bytes.
func Sign(keyID string, priv ed25519.PrivateKey, b Bundle) (envelope, payload []byte, err error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, nil, fmt.Errorf("policyserve: signing key is %d bytes, want %d", len(priv), ed25519.PrivateKeySize)
	}
	payload, err = json.Marshal(b)
	if err != nil {
		return nil, nil, fmt.Errorf("policyserve: marshal bundle: %w", err)
	}
	sig := ed25519.Sign(priv, payload)
	envelope, err = json.Marshal(SignedBundle{
		KeyID:     keyID,
		Algorithm: "ed25519",
		Payload:   payload,
		Signature: base64.StdEncoding.EncodeToString(sig),
	})
	if err != nil {
		return nil, nil, fmt.Errorf("policyserve: marshal envelope: %w", err)
	}
	return envelope, payload, nil
}

// content is the bundle with the fields that change on every minting cleared: what "the inputs
// changed" compares.
func content(b Bundle) ([]byte, error) {
	b.Version, b.EffectiveAt, b.Actor = "", time.Time{}, ""
	return json.Marshal(b)
}

// envelopeContent recovers the content of a stored envelope, for comparison with a candidate. A
// payload carrying a field this service no longer composes is an error, so a bundle the device
// would refuse is replaced rather than served again.
func envelopeContent(envelope []byte) (keyID string, c []byte, err error) {
	var sb SignedBundle
	if err := json.Unmarshal(envelope, &sb); err != nil {
		return "", nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(sb.Payload))
	dec.DisallowUnknownFields()
	var b Bundle
	if err := dec.Decode(&b); err != nil {
		return "", nil, err
	}
	c, err = content(b)
	return sb.KeyID, c, err
}
