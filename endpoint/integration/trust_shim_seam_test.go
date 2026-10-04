package integration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/cli"
	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/trust"
	"github.com/shadow-ai-capture/device/protocol"
)

// This file wires the real trust/CA manager and the real cli.shim provider into the real core
// Registry + Supervisor, so the seams between them are proven against the real code rather than
// against each package's own fakes. The only fakes are the two things that genuinely cannot run on
// this host: the platform command runner (update-ca-certificates, certutil, security) and the
// proxy.tls socket itself.
//
// What it proves: cli.shim starts before proxy.tls (§3.5 ordering), the system proxy is pointed at
// the proxy only when there is a SystemProxy to point, the shim's managed CA bundle is a real
// parseable root, the proxy.tls kill switch removes the shim's files and reports absent/killed, the
// trust manager's install/verify/remove round-trip is a real file lifecycle, and a shim without a
// root never reports healthy.
//
// What it does NOT prove: that any of this runs on a real endpoint (no /etc, no keychain, no real
// system proxy), or that the OS actually honours the installed root - the platform halves are behind
// interfaces with fakes here, exactly as documented in the module README.

const shimProxyAddr = "127.0.0.1:8843"

// newRootCert mints a real self-signed root CA and returns its DER and PEM forms, so the CA-bundle
// parse check and the trust round-trip both run against a certificate produced exactly as
// production would, and the two halves (shim bundle, trust store) agree on the same root.
func newRootCert(t *testing.T) (der, pemBytes []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "sac-device-ca", Organization: []string{"Shadow AI Capture"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	return derBytes, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
}

// newShimProvider builds a real cli.Provider wired entirely into a temp dir, with every path
// redirected off the host so no test writes to /etc/profile.d. The CA bundle path is the default
// (ManagedDir/ca-bundle.pem) so the assertion "the bundle exists in ManagedDir" exercises the
// provider's own resolution rather than a path the test invented.
func newShimProvider(t *testing.T, rootPEM []byte) (*cli.Provider, string) {
	t.Helper()
	managedDir := t.TempDir()
	p := cli.New(cli.Config{
		ManagedDir:  managedDir,
		ProfilePath: filepath.Join(managedDir, "profile.sh"),
		EnvFile:     filepath.Join(managedDir, "env"),
		ProxyAddr:   shimProxyAddr,
		NoProxy:     []string{"localhost", "127.0.0.1"},
		RootCAPEM:   rootPEM,
	})
	return p, managedDir
}

// fakeRunner satisfies both cli.Runner and trust.Runner (they are identical seams in different
// packages), recording every argv and scripting the output. The trust manager's Linux install
// succeeds only if "update-ca-certificates" returns nil, so this is what keeps the round-trip off
// the real /etc.
type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	fn    func(name string, args []string) (string, error)
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	r.mu.Unlock()
	if r.fn != nil {
		return r.fn(name, args)
	}
	return "", nil
}

// fakeProxyTLS stands in for proxy.tls: it reports a listen address and counts how many times the
// supervisor asked for it, which is the observable proof that the supervisor points the proxy only
// when it has a SystemProxy and only after the proxy is listening.
type fakeProxyTLS struct {
	mu          sync.Mutex
	listenAddr  string
	listenCalls int
}

func (p *fakeProxyTLS) Name() protocol.Route { return protocol.RouteProxyTLS }

func (p *fakeProxyTLS) ListenAddr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listenCalls++
	return p.listenAddr
}

func (p *fakeProxyTLS) Start(context.Context) error { return nil }
func (p *fakeProxyTLS) Stop(context.Context) error  { return nil }
func (p *fakeProxyTLS) Health() core.Health {
	return core.Healthy(protocol.DetailNone, time.Now(), time.Now(), core.NewCounterSet(time.Now()))
}
func (p *fakeProxyTLS) ApplyPolicy(policy.Bundle) error { return nil }

func (p *fakeProxyTLS) listenCallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.listenCalls
}

// recordingSystemProxy records PointAt so the test can assert the supervisor pointed the proxy at
// the proxy.tls listen address, and only that address.
type recordingSystemProxy struct {
	mu      sync.Mutex
	pointed []string
}

func (r *recordingSystemProxy) PointAt(_ context.Context, addr string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pointed = append(r.pointed, addr)
	return nil
}
func (r *recordingSystemProxy) Restore(context.Context) error            { return nil }
func (r *recordingSystemProxy) Effective(context.Context) (string, bool) { return "", false }
func (r *recordingSystemProxy) pointedAddrs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.pointed...)
}

func indexOf(haystack []string, needle string) int {
	for i, s := range haystack {
		if s == needle {
			return i
		}
	}
	return -1
}

// A real core.Registry with a real cli.New provider and a fake proxy.tls, driven through the real
// core.Supervisor, must record start_cli.shim before start_proxy.tls, point the system proxy at the
// proxy only after it is listening, and leave the shim healthy with a real, parseable root in its
// CA bundle.
func TestTrustShimSeam_StartupOrdersShimBeforeProxyAndHealthiesTheBundle(t *testing.T) {
	der, rootPEM := newRootCert(t)
	shim, managedDir := newShimProvider(t, rootPEM)
	proxyTLS := &fakeProxyTLS{listenAddr: shimProxyAddr}
	sysProxy := &recordingSystemProxy{}

	reg := core.NewRegistry(time.Now, nil)
	if err := reg.Add(shim); err != nil {
		t.Fatalf("register cli.shim: %v", err)
	}
	if err := reg.Add(proxyTLS); err != nil {
		t.Fatalf("register proxy.tls: %v", err)
	}

	sup, err := core.NewSupervisor(reg, nil, time.Now)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}
	sup.SystemProxy = sysProxy

	if err := sup.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	order := sup.Order()
	cliIdx := indexOf(order, core.StepStartCLIShim)
	tlsIdx := indexOf(order, core.StepStartProxyTLS)
	pointIdx := indexOf(order, core.StepPointSystemProxy)
	if cliIdx == -1 {
		t.Fatalf("order does not contain %q: %v", core.StepStartCLIShim, order)
	}
	if tlsIdx == -1 {
		t.Fatalf("order does not contain %q: %v", core.StepStartProxyTLS, order)
	}
	if cliIdx >= tlsIdx {
		t.Fatalf("%q (%d) is not before %q (%d): %v", core.StepStartCLIShim, cliIdx, core.StepStartProxyTLS, tlsIdx, order)
	}
	if pointIdx <= tlsIdx {
		t.Fatalf("%q (%d) is not after %q (%d); the proxy may only be pointed at once it is listening", core.StepPointSystemProxy, pointIdx, core.StepStartProxyTLS, tlsIdx)
	}

	// The system proxy was pointed at the proxy's listen address, exactly once.
	if got := sysProxy.pointedAddrs(); len(got) != 1 || got[0] != shimProxyAddr {
		t.Fatalf("system proxy pointed at %v, want exactly [%s]", got, shimProxyAddr)
	}
	if proxyTLS.listenCallCount() == 0 {
		t.Fatal("the supervisor never asked proxy.tls for its listen address")
	}

	// The shim's row is healthy.
	row, ok := reg.HealthFor(protocol.RouteCLIShim)
	if !ok {
		t.Fatal("no health row for cli.shim")
	}
	if row.State != protocol.StateHealthy {
		t.Fatalf("cli.shim state = %s (detail=%s), want healthy", row.State, row.Detail)
	}

	// The CA bundle exists in ManagedDir, parses as x509, and is the root the test minted.
	bundlePath := filepath.Join(managedDir, "ca-bundle.pem")
	data, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatalf("read CA bundle %s: %v", bundlePath, err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatalf("CA bundle %s carries no PEM block", bundlePath)
	}
	bundleCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("CA bundle does not parse as x509: %v", err)
	}
	rootCert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("test root does not parse: %v", err)
	}
	if !bundleCert.Equal(rootCert) {
		t.Fatal("the CA bundle in ManagedDir does not contain the minted root")
	}
}

// Without a SystemProxy the supervisor must not consult the proxy's listen address at all: there is
// nothing to point, and asking the proxy where it listens is the first half of pointing at it.
func TestTrustShimSeam_NoSystemProxyLeavesProxyUnpointed(t *testing.T) {
	_, rootPEM := newRootCert(t)
	shim, _ := newShimProvider(t, rootPEM)
	proxyTLS := &fakeProxyTLS{listenAddr: shimProxyAddr}

	reg := core.NewRegistry(time.Now, nil)
	if err := reg.Add(shim); err != nil {
		t.Fatalf("register cli.shim: %v", err)
	}
	if err := reg.Add(proxyTLS); err != nil {
		t.Fatalf("register proxy.tls: %v", err)
	}

	sup, err := core.NewSupervisor(reg, nil, time.Now)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}
	// SystemProxy deliberately left nil.

	if err := sup.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	if n := proxyTLS.listenCallCount(); n != 0 {
		t.Fatalf("supervisor consulted proxy.tls ListenAddr %d time(s) with no SystemProxy; the proxy must not be pointed at without something to point", n)
	}
}

// A proxy.tls kill switch must stop the shim too: the shim feeds a proxy that has stopped
// enforcing, so its managed files must go and its row must report absent with detail=killed.
func TestTrustShimSeam_ProxyTLSKillSwitchRemovesShimFilesAndReportsKilled(t *testing.T) {
	_, rootPEM := newRootCert(t)
	shim, managedDir := newShimProvider(t, rootPEM)
	proxyTLS := &fakeProxyTLS{listenAddr: shimProxyAddr}

	reg := core.NewRegistry(time.Now, nil)
	if err := reg.Add(shim); err != nil {
		t.Fatalf("register cli.shim: %v", err)
	}
	if err := reg.Add(proxyTLS); err != nil {
		t.Fatalf("register proxy.tls: %v", err)
	}

	sup, err := core.NewSupervisor(reg, nil, time.Now)
	if err != nil {
		t.Fatalf("new supervisor: %v", err)
	}
	if err := sup.Startup(context.Background()); err != nil {
		t.Fatalf("Startup: %v", err)
	}

	b := &policy.Bundle{
		Version:     "integration-ks-1",
		EffectiveAt: time.Now().Add(-time.Hour),
		KillSwitches: []policy.KillSwitch{{
			Provider:    protocol.RouteProxyTLS, // proxy.tls disable must also stop the shim
			Mode:        policy.KillDisable,
			EffectiveAt: time.Now().Add(-time.Hour),
			ReasonCode:  "integration-kill",
		}},
	}
	reg.ApplyPolicy(*b)

	bundlePath := filepath.Join(managedDir, "ca-bundle.pem")
	profilePath := filepath.Join(managedDir, "profile.sh")
	for _, f := range []string{bundlePath, profilePath} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("%s still present after the proxy.tls kill switch", f)
		}
	}

	row, ok := reg.HealthFor(protocol.RouteCLIShim)
	if !ok {
		t.Fatal("no health row for cli.shim after the kill switch")
	}
	if row.State != protocol.StateAbsent || row.Detail != protocol.DetailKilled {
		t.Fatalf("cli.shim state/detail = %s/%s, want absent/killed", row.State, row.Detail)
	}
}

// The trust manager's Linux round-trip is a real file lifecycle: Install writes the PEM, Verify
// reads it back, Remove deletes it, and Verify then says it is gone. The platform command is a fake
// that reports success, which is what keeps the test off the real trust store.
func TestTrustRoundTrip_InstallVerifyRemove(t *testing.T) {
	der, _ := newRootCert(t)
	certDir := t.TempDir()
	runner := &fakeRunner{} // returns success for update-ca-certificates and update-ca-trust

	m := trust.New(trust.Config{
		OS:      trust.OSLinux,
		Runner:  runner,
		CertDir: certDir,
		Name:    "sac-device-ca",
	})
	ctx := context.Background()

	if err := m.Install(ctx, der); err != nil {
		t.Fatalf("Install: %v", err)
	}

	crtPath := filepath.Join(certDir, "sac-device-ca.crt")
	got, err := os.ReadFile(crtPath)
	if err != nil {
		t.Fatalf("Install did not write %s: %v", crtPath, err)
	}
	wantPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if string(got) != string(wantPEM) {
		t.Fatalf("installed file content does not match the DER's PEM")
	}

	if ok, err := m.Verify(ctx, der); err != nil || !ok {
		t.Fatalf("Verify after Install = %v, %v; want true, nil", ok, err)
	}

	if err := m.Remove(ctx); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(crtPath); !os.IsNotExist(err) {
		t.Fatalf("%s still present after Remove: %v", crtPath, err)
	}

	if ok, err := m.Verify(ctx, der); err != nil || ok {
		t.Fatalf("Verify after Remove = %v, %v; want false, nil", ok, err)
	}
}

// A shim configured with no root CA must degrade after Start and never report healthy: a shim that
// points runtimes at a proxy without a root to trust is a coverage lie, and §4.5 requires a root to
// be healthy.
func TestTrustShimSeam_EmptyRootDegradesNeverHealthy(t *testing.T) {
	managedDir := t.TempDir()
	p := cli.New(cli.Config{
		ManagedDir:  managedDir,
		ProfilePath: filepath.Join(managedDir, "profile.sh"),
		EnvFile:     filepath.Join(managedDir, "env"),
		ProxyAddr:   shimProxyAddr,
		// RootCAPEM deliberately empty: "no root to install".
	})

	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	h := p.Health()
	if h.State != protocol.StateDegraded {
		t.Fatalf("state = %s (detail=%s), want degraded", h.State, h.Detail)
	}
	if h.Detail != protocol.DetailShimCABundleUnreadable && h.Detail != protocol.DetailShimProfileMissing {
		t.Fatalf("detail = %s, want shim_ca_bundle_unreadable (or shim_profile_missing)", h.Detail)
	}

	// A second read must not flip to healthy: there is no root, so healthy is unreachable.
	if h2 := p.Health(); h2.State == protocol.StateHealthy {
		t.Fatalf("shim without a root reported healthy on a later read: %+v", h2)
	}
}
