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

// The real trust manager and the real cli.shim provider in the real core registry and supervisor.
// The only fakes are the platform command runner (update-ca-certificates, certutil, security) and
// the proxy.tls socket. Proven here: cli.shim starts before proxy.tls, the shim's managed CA bundle
// carries a real parseable root, the proxy.tls kill switch removes the shim's files and reports
// absent/killed, the trust manager's install/verify/remove is a real file lifecycle, and a shim
// without a root never reports healthy.

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

// fakeProxyTLS stands in for proxy.tls.
type fakeProxyTLS struct{}

func (p *fakeProxyTLS) Name() protocol.Route { return protocol.RouteProxyTLS }

func (p *fakeProxyTLS) Start(context.Context) error { return nil }
func (p *fakeProxyTLS) Stop(context.Context) error  { return nil }
func (p *fakeProxyTLS) Health() core.Health {
	return core.Healthy(protocol.DetailNone, time.Now(), time.Now(), core.NewCounterSet(time.Now()))
}
func (p *fakeProxyTLS) ApplyPolicy(policy.Bundle) error { return nil }

func indexOf(haystack []string, needle string) int {
	for i, s := range haystack {
		if s == needle {
			return i
		}
	}
	return -1
}

// The supervisor starts cli.shim before proxy.tls and leaves the shim healthy with a real,
// parseable root in its CA bundle.
func TestTrustShimSeamStartsTheShimBeforeTheProxy(t *testing.T) {
	der, rootPEM := newRootCert(t)
	shim, managedDir := newShimProvider(t, rootPEM)
	proxyTLS := &fakeProxyTLS{}

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

	order := sup.Order()
	cliIdx := indexOf(order, core.StepStartCLIShim)
	tlsIdx := indexOf(order, core.StepStartProxyTLS)
	if cliIdx == -1 {
		t.Fatalf("order does not contain %q: %v", core.StepStartCLIShim, order)
	}
	if tlsIdx == -1 {
		t.Fatalf("order does not contain %q: %v", core.StepStartProxyTLS, order)
	}
	if cliIdx >= tlsIdx {
		t.Fatalf("%q (%d) is not before %q (%d): %v", core.StepStartCLIShim, cliIdx, core.StepStartProxyTLS, tlsIdx, order)
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

// A proxy.tls kill switch must stop the shim too: the shim feeds a proxy that has stopped
// enforcing, so its managed files must go and its row must report absent with detail=killed.
func TestTrustShimSeamKillSwitchRemovesShimFilesAndReportsKilled(t *testing.T) {
	_, rootPEM := newRootCert(t)
	shim, managedDir := newShimProvider(t, rootPEM)
	proxyTLS := &fakeProxyTLS{}

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
func TestTrustRoundTripInstallVerifyRemove(t *testing.T) {
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
// points runtimes at a proxy without a root to trust is a coverage lie, so a root is required to
// be healthy.
func TestTrustShimSeamEmptyRootIsNeverHealthy(t *testing.T) {
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
