package policyserve

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// Bundle is the server's view of the signed policy bundle: the subset of
// endpoint/capture-core/policy.Bundle this service composes, with the same JSON names.
//
// It is a mirror, not an import, because capture-core is a separate module and control-api builds
// offline with no replace into it. The device decodes the payload with unknown fields refused, so a
// field here that the device does not know would make every served bundle unenforceable; the test
// TestServedBundleVerifiesWithTheDevicesVerifier runs capture-core's own Verifier over this
// package's output to catch exactly that drift.
type Bundle struct {
	Version     string    `json:"version"`
	EffectiveAt time.Time `json:"effective_at"`
	Actor       string    `json:"actor,omitempty"`

	TenantDefault string            `json:"tenant_default_mode"`
	ToolModes     map[string]string `json:"tool_modes,omitempty"`

	Interception   Interception      `json:"interception"`
	Loopback       struct{}          `json:"loopback"`
	ProcDetect     struct{}          `json:"proc_detect"`
	Spool          SpoolBounds       `json:"spool"`
	ShapePredicate struct{}          `json:"shape_predicate"`
	Classifier     ClassifierRelease `json:"classifier"`
	CLIShim        CLIShim           `json:"cli_shim"`
}

// Interception is the decryption scope. The per-device root CA is deliberately absent: the device
// generates its own and the server never holds it.
type Interception struct {
	TenantHosts []string `json:"tenant_hosts,omitempty"`
	SeedHosts   []string `json:"seed_hosts,omitempty"`
	Ports       []int    `json:"ports,omitempty"`
	ProxyListen string   `json:"proxy_listen,omitempty"`
	ProxyCanary string   `json:"proxy_canary,omitempty"`
}

// SpoolBounds is left empty, so the device keeps its built-in §12 bound.
type SpoolBounds struct {
	MaxBytes             int64 `json:"max_bytes,omitempty"`
	MaxRows              int64 `json:"max_rows,omitempty"`
	DeviceRetentionHours int   `json:"device_retention_hours,omitempty"`
}

// ClassifierRelease names the release the bundle activates.
type ClassifierRelease struct {
	ReleaseID string `json:"release_id"`
	State     string `json:"state"`
}

// CLIShim is the CLI trust shim's configuration. ManagedDir is left to the device, because one
// bundle serves every platform and the directory is a platform path.
type CLIShim struct {
	Enabled     bool     `json:"enabled,omitempty"`
	ProxyAddr   string   `json:"proxy_addr,omitempty"`
	ManagedDir  string   `json:"managed_dir,omitempty"`
	Runtimes    []string `json:"runtimes,omitempty"`
	NoProxy     []string `json:"no_proxy,omitempty"`
	NodeRequire bool     `json:"node_require,omitempty"`
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

// envelopeContent recovers the content of a stored envelope, for comparison with a candidate.
func envelopeContent(envelope []byte) (keyID string, c []byte, err error) {
	var sb SignedBundle
	if err := json.Unmarshal(envelope, &sb); err != nil {
		return "", nil, err
	}
	var b Bundle
	if err := json.Unmarshal(sb.Payload, &b); err != nil {
		return "", nil, err
	}
	c, err = content(b)
	return sb.KeyID, c, err
}
