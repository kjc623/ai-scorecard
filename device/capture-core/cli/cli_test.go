package cli

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// The constructed provider satisfies the core contract; this is a compile-time assertion,
// not a runtime check, so a refactor that drops a method fails the build rather than a test.
var _ core.Provider = New(Config{})

// ---- fakes --------------------------------------------------------------------------------

type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	out   string
	err   error
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string{name}, args...))
	return r.out, r.err
}

func (r *fakeRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// ---- helpers ------------------------------------------------------------------------------

// testRootPEM mints a real self-signed root CA and returns it as PEM, so the CA-bundle
// parse check is exercised against a certificate produced exactly as production would.
func testRootPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Shadow AI Capture Device CA test", Organization: []string{"Shadow AI Capture"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// testConfig returns a Config wired entirely into a fresh temp directory, so no test ever
// touches /etc or the OS trust store.
func testConfig(t *testing.T, root []byte) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{
		ManagedDir:        dir,
		CABundlePath:      filepath.Join(dir, "ca-bundle.pem"),
		ProfilePath:       filepath.Join(dir, "profile.sh"),
		EnvFile:           filepath.Join(dir, "env"),
		NodeBootstrapPath: filepath.Join(dir, "node-proxy.cjs"),
		ProxyAddr:         "127.0.0.1:8080",
		NoProxy:           []string{"localhost", "127.0.0.1"},
		RootCAPEM:         root,
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

// ---- tests --------------------------------------------------------------------------------

func TestStartWritesFilesWithContentAndPermissions(t *testing.T) {
	root := testRootPEM(t)
	cfg := testConfig(t, root)
	p := New(cfg)

	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// CA bundle: the device root plus the system roots, readable by every user's runtime.
	if got, _ := os.ReadFile(cfg.CABundlePath); !bundleContainsRoot(got, root) {
		t.Fatalf("CA bundle does not carry the root PEM")
	}
	// POSIX modes: the bundle and the profile are world-readable. Windows has no mode bits; the
	// shim's directory is readable by users through its inherited ACL.
	if runtime.GOOS != "windows" {
		if got := fileMode(t, cfg.CABundlePath); got != 0o644 {
			t.Fatalf("CA bundle mode = %o, want 0644", got)
		}
		if got := fileMode(t, cfg.ProfilePath); got != 0o644 {
			t.Fatalf("profile mode = %o, want 0644", got)
		}
	}

	// Profile: both cases of the proxy variables plus the CA variables, in the platform's syntax.
	profile, _ := os.ReadFile(cfg.ProfilePath)
	for _, kv := range [][2]string{
		{"HTTP_PROXY", "http://127.0.0.1:8080"},
		{"HTTPS_PROXY", "http://127.0.0.1:8080"},
		{"http_proxy", "http://127.0.0.1:8080"},
		{"NO_PROXY", "localhost,127.0.0.1,::1"},
		{"NODE_EXTRA_CA_CERTS", cfg.CABundlePath},
		{"SSL_CERT_FILE", cfg.CABundlePath},
		{"REQUESTS_CA_BUNDLE", cfg.CABundlePath},
		{"CURL_CA_BUNDLE", cfg.CABundlePath},
		{"NODE_USE_ENV_PROXY", "1"},
	} {
		if want := profileLine(kv[0], kv[1]); !strings.Contains(string(profile), want) {
			t.Fatalf("profile missing %q\n---\n%s", want, profile)
		}
	}

	// Env file: KEY=VALUE lines, written on Linux only.
	if runtime.GOOS != "linux" {
		return
	}
	env, err := os.ReadFile(cfg.EnvFile)
	if err != nil {
		t.Fatalf("env file: %v", err)
	}
	for _, want := range []string{
		"HTTP_PROXY=http://127.0.0.1:8080",
		"NODE_EXTRA_CA_CERTS=" + cfg.CABundlePath,
	} {
		if !strings.Contains(string(env), want) {
			t.Fatalf("env file missing %q\n---\n%s", want, env)
		}
	}
}

// profileLine is how the platform's profile sets one variable: a batch "set" on Windows, a POSIX
// export elsewhere.
func profileLine(name, value string) string {
	if runtime.GOOS == "windows" {
		return "set \"" + name + "=" + value + "\""
	}
	return "export " + name + "='" + value + "'"
}

func TestCABundleContainsTheRoot(t *testing.T) {
	root := testRootPEM(t)
	cfg := testConfig(t, root)
	p := New(cfg)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	data, err := os.ReadFile(cfg.CABundlePath)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	if !bundleContainsRoot(data, root) {
		t.Fatalf("bundle does not contain the root cert")
	}

	// Health must be healthy: profile, bundle and (no probe) inherited-skip all pass.
	h := p.Health()
	if h.State != protocol.StateHealthy {
		t.Fatalf("state = %s, want healthy (detail=%s)", h.State, h.Detail)
	}
}

func TestHealthDegradedWhenRootEmpty(t *testing.T) {
	cfg := testConfig(t, nil) // no root
	p := New(cfg)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	h := p.Health()
	if h.State != protocol.StateDegraded {
		t.Fatalf("state = %s, want degraded", h.State)
	}
	if h.Detail != protocol.DetailShimCABundleUnreadable {
		t.Fatalf("detail = %s, want shim_ca_bundle_unreadable", h.Detail)
	}
}

func TestHealthDegradedWhenBundleUnreadable(t *testing.T) {
	root := testRootPEM(t)
	cfg := testConfig(t, root)
	p := New(cfg)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := os.WriteFile(cfg.CABundlePath, []byte("not a pem"), 0o600); err != nil {
		t.Fatalf("corrupt bundle: %v", err)
	}

	h := p.Health()
	if h.State != protocol.StateDegraded {
		t.Fatalf("state = %s, want degraded", h.State)
	}
	if h.Detail != protocol.DetailShimCABundleUnreadable {
		t.Fatalf("detail = %s, want shim_ca_bundle_unreadable", h.Detail)
	}
}

func TestHealthDegradedWhenProfileRemoved(t *testing.T) {
	root := testRootPEM(t)
	cfg := testConfig(t, root)
	p := New(cfg)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := os.Remove(cfg.ProfilePath); err != nil {
		t.Fatalf("remove profile: %v", err)
	}

	h := p.Health()
	if h.State != protocol.StateDegraded {
		t.Fatalf("state = %s, want degraded", h.State)
	}
	if h.Detail != protocol.DetailShimProfileMissing {
		t.Fatalf("detail = %s, want shim_profile_missing", h.Detail)
	}
}

func TestHealthEnvProbe(t *testing.T) {
	root := testRootPEM(t)

	t.Run("inherited", func(t *testing.T) {
		cfg := testConfig(t, root)
		cfg.EnvProbe = func(context.Context) (map[string]string, error) {
			return map[string]string{
				"NODE_EXTRA_CA_CERTS": cfg.CABundlePath,
				"HTTPS_PROXY":         "http://127.0.0.1:8080",
			}, nil
		}
		p := New(cfg)
		if err := p.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if h := p.Health(); h.State != protocol.StateHealthy {
			t.Fatalf("state = %s, want healthy (detail=%s)", h.State, h.Detail)
		}
	})

	t.Run("not inherited", func(t *testing.T) {
		cfg := testConfig(t, root)
		cfg.EnvProbe = func(context.Context) (map[string]string, error) {
			return map[string]string{}, nil // child saw nothing
		}
		p := New(cfg)
		if err := p.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		h := p.Health()
		if h.State != protocol.StateDegraded || h.Detail != protocol.DetailShimNotInherited {
			t.Fatalf("state/detail = %s/%s, want degraded/shim_not_inherited", h.State, h.Detail)
		}
	})

	t.Run("nil probe skipped", func(t *testing.T) {
		cfg := testConfig(t, root)
		cfg.EnvProbe = nil
		p := New(cfg)
		if err := p.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if h := p.Health(); h.State != protocol.StateHealthy {
			t.Fatalf("state = %s, want healthy (nil probe must not fail health)", h.State)
		}
	})
}

func TestKillSwitchRemovesFilesAndReportsAbsentKilled(t *testing.T) {
	root := testRootPEM(t)
	cfg := testConfig(t, root)
	p := New(cfg)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	b := &policy.Bundle{
		Version:     "1",
		EffectiveAt: time.Unix(1_700_000_000, 0),
		KillSwitches: []policy.KillSwitch{{
			Provider:    protocol.RouteProxyTLS, // proxy.tls disable must also stop the shim
			Mode:        policy.KillDisable,
			EffectiveAt: time.Unix(1_700_000_000, 0),
			ReasonCode:  "test",
		}},
	}
	if err := p.ApplyPolicy(*b); err != nil {
		t.Fatalf("ApplyPolicy: %v", err)
	}

	if _, err := os.Stat(cfg.ProfilePath); !os.IsNotExist(err) {
		t.Fatalf("profile still present after kill switch")
	}
	if _, err := os.Stat(cfg.CABundlePath); !os.IsNotExist(err) {
		t.Fatalf("CA bundle still present after kill switch")
	}

	h := p.Health()
	if h.State != protocol.StateAbsent || h.Detail != protocol.DetailKilled {
		t.Fatalf("state/detail = %s/%s, want absent/killed", h.State, h.Detail)
	}
}

func TestKillSwitchCLIShimRoute(t *testing.T) {
	root := testRootPEM(t)
	cfg := testConfig(t, root)
	p := New(cfg)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	b := &policy.Bundle{
		Version:     "1",
		EffectiveAt: time.Unix(1_700_000_000, 0),
		KillSwitches: []policy.KillSwitch{{
			Provider:    protocol.RouteCLIShim,
			Mode:        policy.KillDisable,
			EffectiveAt: time.Unix(1_700_000_000, 0),
			ReasonCode:  "test",
		}},
	}
	if err := p.ApplyPolicy(*b); err != nil {
		t.Fatalf("ApplyPolicy: %v", err)
	}
	if h := p.Health(); h.State != protocol.StateAbsent || h.Detail != protocol.DetailKilled {
		t.Fatalf("state/detail = %s/%s, want absent/killed", h.State, h.Detail)
	}
}

func TestNodeProxyScript(t *testing.T) {
	root := testRootPEM(t)
	cfg := testConfig(t, root)
	cfg.NodeRequire = true
	p := New(cfg)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	script, err := os.ReadFile(cfg.NodeBootstrapPath)
	if err != nil {
		t.Fatalf("read node bootstrap: %v", err)
	}
	if !strings.Contains(string(script), "createConnection") {
		t.Fatalf("node bootstrap lacks createConnection")
	}
	if !strings.Contains(string(script), "CONNECT") {
		t.Fatalf("node bootstrap lacks CONNECT")
	}
	if !strings.Contains(string(script), "https.globalAgent") {
		t.Fatalf("node bootstrap does not install the global agent")
	}

	// The profile must wire the bootstrap through NODE_OPTIONS.
	profile, _ := os.ReadFile(cfg.ProfilePath)
	if !strings.Contains(string(profile), profileLine("NODE_OPTIONS", "--require "+cfg.NodeBootstrapPath)) {
		t.Fatalf("profile does not wire NODE_OPTIONS\n---\n%s", profile)
	}

	// When Node is present, the file must parse as JavaScript.
	if node, err := exec.LookPath("node"); err == nil {
		if out, err := exec.Command(node, "--check", cfg.NodeBootstrapPath).CombinedOutput(); err != nil {
			t.Fatalf("node --check failed: %v\n%s", err, out)
		}
	}
}

func TestStartStopIdempotent(t *testing.T) {
	root := testRootPEM(t)
	cfg := testConfig(t, root)
	p := New(cfg)

	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if _, err := os.Stat(cfg.ProfilePath); err != nil {
		t.Fatalf("profile missing after double Start: %v", err)
	}

	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	for _, f := range []string{cfg.ProfilePath, cfg.CABundlePath, cfg.EnvFile} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("%s still present after Stop", f)
		}
	}
	if h := p.Health(); h.State != protocol.StateAbsent {
		t.Fatalf("state after Stop = %s, want absent", h.State)
	}
}

func TestCounters(t *testing.T) {
	root := testRootPEM(t)
	cfg := testConfig(t, root)
	p := New(cfg)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// emitted: the CA bundle and the profile, plus the env file on Linux.
	want := uint64(2)
	if runtime.GOOS == "linux" {
		want = 3
	}
	if got := p.Counters().Cumulative()[protocol.CounterEmitted]; got != want {
		t.Fatalf("emitted = %d, want %d", got, want)
	}

	// One healthy check: observed increments, dropped stays zero.
	_ = p.Health()
	_ = p.Health()
	cum := p.Counters().Cumulative()
	if cum[protocol.CounterObserved] != 2 {
		t.Fatalf("observed = %d, want 2", cum[protocol.CounterObserved])
	}
	if cum[protocol.CounterDropped] != 0 {
		t.Fatalf("dropped = %d, want 0", cum[protocol.CounterDropped])
	}
	if cum[protocol.CounterNotCooperative] != 0 {
		t.Fatalf("not_cooperative = %d, want 0", cum[protocol.CounterNotCooperative])
	}

	// Remove the profile: the next check drops.
	if err := os.Remove(cfg.ProfilePath); err != nil {
		t.Fatalf("remove profile: %v", err)
	}
	_ = p.Health()
	if got := p.Counters().Cumulative()[protocol.CounterDropped]; got != 1 {
		t.Fatalf("dropped = %d, want 1", got)
	}
}

func TestNameAndSequence(t *testing.T) {
	root := testRootPEM(t)
	cfg := testConfig(t, root)
	p := New(cfg)
	if p.Name() != protocol.CollectorCLIShim {
		t.Fatalf("Name() = %s, want cli_shim", p.Name())
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	seq := p.Sequence()
	if len(seq) == 0 || seq[0] != "write:ca-bundle" {
		t.Fatalf("unexpected sequence: %v", seq)
	}
}

// A Node bootstrap path with a space cannot be expressed in NODE_OPTIONS (Node splits on
// whitespace and does not quote), so Start must refuse it rather than write a profile whose
// require hook silently fails to load.
func TestStartRefusesNodeBootstrapPathWithWhitespace(t *testing.T) {
	root := testRootPEM(t)
	dir := filepath.Join(t.TempDir(), "has space")
	p := New(Config{
		ManagedDir:  dir,
		ProxyAddr:   "127.0.0.1:8843",
		NodeRequire: true,
		RootCAPEM:   root,
		Clock:       time.Now,
	})
	if err := p.Start(context.Background()); err == nil {
		t.Fatal("Start accepted a Node bootstrap path containing whitespace")
	}
	if _, err := os.Stat(filepath.Join(dir, "ca-bundle.pem")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed Start left the CA bundle behind: %v", err)
	}
}

// A kill switch in the bundle already in force at startup must suppress the shim before it writes
// anything: the supervisor applies policy before providers start, so Start must not clear the
// killed flag and reinstall the profile.
func TestStartSuppressedByInitialKillSwitch(t *testing.T) {
	cfg := testConfig(t, testRootPEM(t))
	p := New(cfg)
	ks := policy.Bundle{
		Version:     "1",
		EffectiveAt: time.Unix(1, 0),
		KillSwitches: []policy.KillSwitch{{
			Provider: protocol.RouteProxyTLS, Mode: policy.KillDisable,
			EffectiveAt: time.Unix(1, 0), ReasonCode: "fleet_regression",
		}},
	}
	if err := p.ApplyPolicy(ks); err != nil {
		t.Fatalf("ApplyPolicy: %v", err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := os.Stat(cfg.CABundlePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the shim wrote the CA bundle despite the kill switch: %v", err)
	}
	if h := p.Health(); h.State != protocol.StateAbsent || h.Detail != protocol.DetailKilled {
		t.Fatalf("health = %s/%s, want absent/killed", h.State, h.Detail)
	}
}

// A future-dated kill switch must not fire before EffectiveAt.
func TestFutureKillSwitchDoesNotFireEarly(t *testing.T) {
	cfg := testConfig(t, testRootPEM(t))
	p := New(cfg)
	ks := policy.Bundle{
		Version:     "1",
		EffectiveAt: time.Unix(1, 0),
		KillSwitches: []policy.KillSwitch{{
			Provider: protocol.RouteProxyTLS, Mode: policy.KillDisable,
			EffectiveAt: time.Now().Add(time.Hour), ReasonCode: "scheduled",
		}},
	}
	if err := p.ApplyPolicy(ks); err != nil {
		t.Fatalf("ApplyPolicy: %v", err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := os.Stat(cfg.CABundlePath); err != nil {
		t.Errorf("a future-dated kill switch suppressed the shim early: %v", err)
	}
}

// The shim is a policy toggle, enabled only by interception.enabled, and a Start after a Stop
// writes the profile and the CA bundle again, as switching TLS inspection off and on needs.
func TestShimIsToggledByTLSInspection(t *testing.T) {
	var prov core.Provider = New(Config{})
	toggled, ok := prov.(core.Toggled)
	if !ok {
		t.Fatal("cli.shim is not a policy toggle")
	}
	if toggled.Enabled(nil) || toggled.Enabled(&policy.Bundle{}) ||
		!toggled.Enabled(&policy.Bundle{Interception: policy.Interception{Enabled: true}}) {
		t.Fatal("cli.shim is not enabled exactly by interception.enabled")
	}

	cfg := testConfig(t, testRootPEM(t))
	p := New(cfg)
	for round := 1; round <= 2; round++ {
		if err := p.Start(context.Background()); err != nil {
			t.Fatalf("round %d Start: %v", round, err)
		}
		for _, f := range []string{cfg.ProfilePath, cfg.CABundlePath} {
			if _, err := os.Stat(f); err != nil {
				t.Fatalf("round %d: %s missing after Start: %v", round, f, err)
			}
		}
		if h := p.Health(); h.State != protocol.StateHealthy {
			t.Fatalf("round %d health = %s/%s, want healthy", round, h.State, h.Detail)
		}
		if err := p.Stop(context.Background()); err != nil {
			t.Fatalf("round %d Stop: %v", round, err)
		}
		for _, f := range []string{cfg.ProfilePath, cfg.CABundlePath} {
			if _, err := os.Stat(f); !os.IsNotExist(err) {
				t.Fatalf("round %d: %s still present after Stop", round, f)
			}
		}
	}
}
