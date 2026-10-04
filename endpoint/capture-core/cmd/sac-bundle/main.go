// Command sac-bundle is the offline policy-bundle generator: it mints (or loads) the per-device
// root CA and a matching signed policy bundle, so a device can be provisioned without first
// reaching the policy service. It is stdlib-only and never touches the network.
//
// The bundle it signs is the same shape policy.Store enforces (docs/01-collectors.md §13.2):
// interception carries the device CA (root_ca_pem + root_ca_fingerprint) and the proxy listen
// address, and cli_shim carries the managed runtimes and the proxy they point at — so the proxy
// and the CLI trust shim are configured from one signed artefact rather than two.
//
// Outputs, written into --out:
//
//	bundle.json      the signed bundle (ed25519, key id --policy-key-id)
//	ca.pem           the per-device root CA certificate (mode 0600)
//	ca.key           the CA private key, PKCS#8 (mode 0600; never overwrites a loaded --ca-key/--ca-cert)
//	policy-key.pub   the generated policy public key as hex (mode 0600; written only when generated)
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/proxy/tlsproxy"
	"github.com/shadow-ai-capture/device/protocol"
)

func main() {
	os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
}

// Run is the in-process entry point. It returns a process exit code: 0 on success, non-zero on
// any error, with the error written to stderr.
func Run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "sac-bundle: %v\n", err)
		return 2
	}
	if err := generate(opts, stdout); err != nil {
		fmt.Fprintf(stderr, "sac-bundle: %v\n", err)
		return 1
	}
	return 0
}

// options is the resolved flag set. It is separated from the flag plumbing so generate is testable
// without re-parsing strings.
type options struct {
	out           string
	deviceID      string
	hosts         []string
	tenantHosts   []string
	ports         []int
	classifier    string
	caKeyPath     string
	caCertPath    string
	policyPriv    string // hex-encoded ed25519 private key; empty means generate
	policyKeyID   string
	listen        string
	canary        string
	proxyAddr     string
	runtimes      []string
	noProxy       []string
	tenantDefault string
	toolModes     map[string]protocol.CollectionMode
	version       string
}

// toolModesValue parses repeatable --tool-mode fingerprint=mode flags and refuses a mode outside
// the closed set at parse time, before any file is written.
type toolModesValue map[string]protocol.CollectionMode

func (v toolModesValue) String() string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k+"="+string(v[k]))
	}
	return strings.Join(keys, ",")
}

func (v toolModesValue) Set(s string) error {
	fp, mode, ok := strings.Cut(s, "=")
	if !ok || strings.TrimSpace(fp) == "" {
		return fmt.Errorf("--tool-mode must be fingerprint=mode, got %q", s)
	}
	m := protocol.CollectionMode(strings.TrimSpace(mode))
	if !m.Valid() {
		return fmt.Errorf("--tool-mode mode %q is outside {m0,m1,m2,m3}", mode)
	}
	v[fp] = m
	return nil
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	o := options{
		policyKeyID:   "policy-key-1",
		listen:        "127.0.0.1:8843",
		runtimes:      []string{"go", "node", "python"},
		tenantDefault: "m1",
		version:       "1",
		toolModes:     map[string]protocol.CollectionMode{},
	}
	fs := flag.NewFlagSet("sac-bundle", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var hostsCSV, tenantHostsCSV, portsCSV, runtimesCSV, noProxyCSV, proxyAddr string
	var toolModes toolModesValue = map[string]protocol.CollectionMode{}

	fs.StringVar(&o.out, "out", "", "output directory (required)")
	fs.StringVar(&o.deviceID, "device-id", "", "device id stamped into the CA subject")
	fs.StringVar(&hostsCSV, "hosts", "", "comma-separated seed hosts to intercept")
	fs.StringVar(&tenantHostsCSV, "tenant-hosts", "", "comma-separated tenant hosts to intercept")
	fs.StringVar(&portsCSV, "ports", "443", "comma-separated interceptable ports")
	fs.StringVar(&o.classifier, "classifier", "", "classifier host as transport:path")
	fs.StringVar(&o.caKeyPath, "ca-key", "", "existing EC private key (PKCS#8 or SEC1 PEM) to load instead of generating")
	fs.StringVar(&o.caCertPath, "ca-cert", "", "existing CA certificate PEM to load instead of generating")
	fs.StringVar(&o.policyPriv, "policy-priv", "", "hex-encoded 64-byte ed25519 private key to sign with; generate when empty")
	fs.StringVar(&o.policyKeyID, "policy-key-id", o.policyKeyID, "key id the signed bundle names")
	fs.StringVar(&o.listen, "listen", o.listen, "proxy listen address")
	fs.StringVar(&o.canary, "canary", "", "end-to-end probe canary host:port")
	fs.StringVar(&proxyAddr, "proxy-addr", "", "CLI shim proxy address (default: --listen)")
	fs.StringVar(&runtimesCSV, "runtimes", strings.Join(o.runtimes, ","), "comma-separated CLI shim runtimes, subset of go,node,python")
	fs.StringVar(&noProxyCSV, "no-proxy", "", "comma-separated no_proxy entries for the CLI shim")
	fs.StringVar(&o.tenantDefault, "tenant-default", o.tenantDefault, "tenant default collection mode (m0..m3)")
	fs.Var(toolModes, "tool-mode", "fingerprint=mode override (repeatable)")
	fs.StringVar(&o.version, "version", o.version, "bundle version")

	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if strings.TrimSpace(o.out) == "" {
		return o, fmt.Errorf("--out is required")
	}

	o.hosts = splitCSV(hostsCSV)
	o.tenantHosts = splitCSV(tenantHostsCSV)
	o.noProxy = splitCSV(noProxyCSV)
	o.runtimes = splitCSV(runtimesCSV)
	o.proxyAddr = proxyAddr
	if o.proxyAddr == "" {
		o.proxyAddr = o.listen
	}
	o.toolModes = map[string]protocol.CollectionMode(toolModes)

	ports, err := parsePorts(portsCSV)
	if err != nil {
		return o, err
	}
	o.ports = ports

	if !protocol.CollectionMode(o.tenantDefault).Valid() {
		return o, fmt.Errorf("--tenant-default %q is outside {m0,m1,m2,m3}", o.tenantDefault)
	}
	for _, r := range o.runtimes {
		switch r {
		case "go", "node", "python":
		default:
			return o, fmt.Errorf("--runtimes %q is outside the set {go,node,python}", r)
		}
	}
	return o, nil
}

func generate(o options, stdout io.Writer) error {
	now := time.Now()

	if err := os.MkdirAll(o.out, 0o755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	ca, err := loadOrMintCA(o, now)
	if err != nil {
		return err
	}
	certPEM := ca.PEM()
	keyPEM, err := ca.KeyPEM()
	if err != nil {
		return err
	}
	fingerprint := sha256hex(ca.DER())

	priv, pub, generatedKey, err := loadOrMintPolicyKey(o)
	if err != nil {
		return err
	}

	if err := os.WriteFile(filepath.Join(o.out, "ca.pem"), certPEM, 0o600); err != nil {
		return fmt.Errorf("writing ca.pem: %w", err)
	}
	if err := os.WriteFile(filepath.Join(o.out, "ca.key"), keyPEM, 0o600); err != nil {
		return fmt.Errorf("writing ca.key: %w", err)
	}
	if generatedKey {
		if err := os.WriteFile(filepath.Join(o.out, "policy-key.pub"), []byte(hex.EncodeToString(pub)), 0o600); err != nil {
			return fmt.Errorf("writing policy-key.pub: %w", err)
		}
	}

	bundle := &policy.Bundle{
		Version:       o.version,
		EffectiveAt:   now,
		Actor:         "sac-bundle",
		TenantDefault: protocol.CollectionMode(o.tenantDefault),
		ToolModes:     o.toolModes,
		Interception: policy.Interception{
			TenantHosts:       o.tenantHosts,
			SeedHosts:         o.hosts,
			Ports:             o.ports,
			RootCAPEM:         string(certPEM),
			RootCAFingerprint: fingerprint,
			ProxyListen:       o.listen,
			ProxyCanary:       o.canary,
		},
		CLIShim: policy.CLIShimPolicy{
			Enabled:   true,
			ProxyAddr: o.proxyAddr,
			Runtimes:  o.runtimes,
			NoProxy:   o.noProxy,
		},
		Classifier: policy.ClassifierRelease{ReleaseID: "sac-bundle", State: policy.ReleaseEnforcing},
	}

	signed, err := policy.Sign(o.policyKeyID, priv, bundle)
	if err != nil {
		return fmt.Errorf("signing bundle: %w", err)
	}
	if err := os.WriteFile(filepath.Join(o.out, "bundle.json"), signed, 0o644); err != nil {
		return fmt.Errorf("writing bundle.json: %w", err)
	}

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(summaryJSON(o, pub, generatedKey))
}

// loadOrMintCA returns the device CA: the loaded pair when both --ca-cert and --ca-key are given,
// a freshly minted one when neither is. Exactly one is refused, and the loaded inputs are never
// overwritten by the outputs.
func loadOrMintCA(o options, now time.Time) (*tlsproxy.CA, error) {
	haveCert, haveKey := o.caCertPath != "", o.caKeyPath != ""
	switch {
	case haveCert && haveKey:
		certPEM, err := os.ReadFile(o.caCertPath)
		if err != nil {
			return nil, fmt.Errorf("reading --ca-cert: %w", err)
		}
		keyPEM, err := os.ReadFile(o.caKeyPath)
		if err != nil {
			return nil, fmt.Errorf("reading --ca-key: %w", err)
		}
		if err := guardNotSame(o.caCertPath, filepath.Join(o.out, "ca.pem"), "--ca-cert"); err != nil {
			return nil, err
		}
		if err := guardNotSame(o.caKeyPath, filepath.Join(o.out, "ca.key"), "--ca-key"); err != nil {
			return nil, err
		}
		return tlsproxy.NewCAFromPEM(certPEM, keyPEM, now)
	case haveCert || haveKey:
		return nil, fmt.Errorf("--ca-cert and --ca-key must be supplied together (or neither, to generate)")
	default:
		return tlsproxy.NewCA(o.deviceID, nil, now)
	}
}

func guardNotSame(inPath, outPath, flagName string) error {
	absIn, errIn := filepath.Abs(inPath)
	absOut, errOut := filepath.Abs(outPath)
	if errIn == nil && errOut == nil && filepath.Clean(absIn) == filepath.Clean(absOut) {
		return fmt.Errorf("%s %q is the output path %q; refusing to overwrite an input", flagName, inPath, outPath)
	}
	return nil
}

// loadOrMintPolicyKey returns the signing key: the supplied --policy-priv, or a freshly generated
// pair. generatedKey reports whether policy-key.pub should be written.
func loadOrMintPolicyKey(o options) (ed25519.PrivateKey, ed25519.PublicKey, bool, error) {
	if o.policyPriv == "" {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, nil, false, fmt.Errorf("generating policy key: %w", err)
		}
		return priv, pub, true, nil
	}
	raw, err := hex.DecodeString(strings.TrimSpace(o.policyPriv))
	if err != nil {
		return nil, nil, false, fmt.Errorf("--policy-priv is not hex: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, nil, false, fmt.Errorf("--policy-priv is %d bytes, want %d", len(raw), ed25519.PrivateKeySize)
	}
	priv := ed25519.PrivateKey(raw)
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok || len(pub) != ed25519.PublicKeySize {
		return nil, nil, false, fmt.Errorf("--policy-priv is not a valid ed25519 private key")
	}
	return priv, pub, false, nil
}

func summaryJSON(o options, pub ed25519.PublicKey, generatedKey bool) map[string]any {
	files := map[string]string{
		"bundle.json": filepath.Join(o.out, "bundle.json"),
		"ca.pem":      filepath.Join(o.out, "ca.pem"),
		"ca.key":      filepath.Join(o.out, "ca.key"),
	}
	if generatedKey {
		files["policy-key.pub"] = filepath.Join(o.out, "policy-key.pub")
	}
	flags := map[string]string{
		"--bundle":           files["bundle.json"],
		"--policy-key":       hex.EncodeToString(pub),
		"--policy-key-id":    o.policyKeyID,
		"--ca-cert":          files["ca.pem"],
		"--ca-key":           files["ca.key"],
		"--proxy-tls-listen": o.listen,
		"--trust-install":    "true",
		"--cli-shim":         "true",
		"--shim-dir":         filepath.Join(o.out, "shim"),
	}
	if o.canary != "" {
		flags["--proxy-tls-canary"] = o.canary
	}
	if o.classifier != "" {
		flags["--classifier-address"] = o.classifier
	}
	return map[string]any{
		"out":                o.out,
		"files":              files,
		"capture_core_flags": flags,
	}
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func parsePorts(csv string) ([]int, error) {
	if strings.TrimSpace(csv) == "" {
		return nil, nil
	}
	var out []int
	for _, p := range strings.Split(csv, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n <= 0 || n > 65535 {
			return nil, fmt.Errorf("--ports value %q is not a usable TCP port", p)
		}
		out = append(out, n)
	}
	return out, nil
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
