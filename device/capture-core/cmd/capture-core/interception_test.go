package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/capture-core/winproxy"
	"github.com/shadow-ai-capture/device/protocol"
)

// signedInterceptionBundle signs a bundle whose TLS inspection is on or off, with the proxy and the
// PAC on the given loopback addresses.
func signedInterceptionBundle(t *testing.T, priv ed25519.PrivateKey, version string, enabled bool, proxyListen, pacListen string) []byte {
	t.Helper()
	raw, err := policy.Sign("policy-key-1", priv, &policy.Bundle{
		Version:       version,
		EffectiveAt:   time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC),
		TenantDefault: protocol.ModeM1,
		Interception:  policy.Interception{Enabled: enabled, ProxyListen: proxyListen, PacListen: pacListen},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// fakeUserSettings is one signed-in user's Internet Settings, as the PAC reads and writes them.
type fakeUserSettings struct {
	mu     sync.Mutex
	values map[string]string
}

func (f *fakeUserSettings) GetString(name string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.values[name]
	return v, ok, nil
}

func (f *fakeUserSettings) GetDWORD(string) (uint32, bool, error) { return 0, false, nil }

func (f *fakeUserSettings) SetString(name, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[name] = value
	return nil
}

func (f *fakeUserSettings) Delete(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.values, name)
	return nil
}

func (f *fakeUserSettings) Close() error { return nil }

func (f *fakeUserSettings) autoConfigURL() string {
	v, _, _ := f.GetString("AutoConfigURL")
	return v
}

// withDesktopPAC gives the service a desktop-app PAC over one signed-in user's fake settings, as
// on Windows.
func withDesktopPAC(t *testing.T) *fakeUserSettings {
	t.Helper()
	user := &fakeUserSettings{values: map[string]string{}}
	prev := platform.desktopPAC
	platform.desktopPAC = func(cfg winproxy.Config) *winproxy.Server {
		cfg.Users = func(context.Context) ([]string, error) { return []string{"S-1-12-1-1000"}, nil }
		cfg.OpenSettings = func(string) (winproxy.Registry, error) { return user, nil }
		cfg.FetchPAC = func(context.Context, string) ([]byte, error) { return nil, os.ErrNotExist }
		return winproxy.New(cfg)
	}
	t.Cleanup(func() { platform.desktopPAC = prev })
	return user
}

// The desktop-app PAC is wired on Windows only; elsewhere desktop apps keep their own proxy
// behaviour and no desktop_proxy row is reported.
func TestDesktopPACIsWiredOnWindowsOnly(t *testing.T) {
	if got, want := desktopPAC() != nil, runtime.GOOS == "windows"; got != want {
		t.Fatalf("desktop-app PAC wired = %t on %s, want %t", got, runtime.GOOS, want)
	}
}

// TLS inspection is opt-in. With interception.enabled false the service starts no proxy, writes no
// CLI shim profile or CA bundle, sets no PAC and leaves the trust store empty, and those collectors
// report disabled_by_policy. A bundle that turns it on brings all four up without a restart; one
// that turns it off again removes them.
func TestTLSInspectionIsOptInAndTogglesWithoutARestart(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	proxyListen, pacListen := freeLoopbackAddr(t), freeLoopbackAddr(t)
	cloud := startFakeCloud(t, signedInterceptionBundle(t, priv, "5", false, proxyListen, pacListen))
	trust := sharedTrust(t)
	user := withDesktopPAC(t)
	profile := platform.shimProfile
	caBundle := filepath.Join(platform.shimDir, "ca-bundle.pem")

	svc, err := newService(context.Background(), testConfig(t, cloud, pub), testLogger(t))
	if err != nil {
		t.Fatalf("newService: %v", err)
	}
	if b := svc.currentBundle(); b == nil || b.Version != "5" || b.Interception.Enabled {
		t.Fatalf("bundle in force = %+v, want version 5 with inspection off", b)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = svc.Stop(context.Background())
		}
	}()

	interceptors := []protocol.Collector{protocol.CollectorEgressProxy, protocol.CollectorCLIShim, protocol.CollectorDesktopProxy}
	assertOff := func(when string) {
		t.Helper()
		trust.mu.Lock()
		installed := len(trust.installed) > 0
		trust.mu.Unlock()
		if installed {
			t.Errorf("%s: the per-device root is in the trust store", when)
		}
		for _, f := range []string{profile, caBundle} {
			if _, err := os.Stat(f); !os.IsNotExist(err) {
				t.Errorf("%s: the CLI shim's %s exists (%v)", when, filepath.Base(f), err)
			}
		}
		if addr := svc.tlsProv.ListenAddr(); addr != "" {
			t.Errorf("%s: proxy.tls is listening on %s", when, addr)
		}
		if conn, err := net.DialTimeout("tcp", proxyListen, time.Second); err == nil {
			conn.Close()
			t.Errorf("%s: something accepts connections on the proxy address %s", when, proxyListen)
		}
		if u := user.autoConfigURL(); u != "" {
			t.Errorf("%s: the user's AutoConfigURL is %q", when, u)
		}
		for _, c := range interceptors {
			if h, _ := svc.reg.HealthFor(c); h.State != protocol.StateAbsent || h.Detail != protocol.DetailDisabledByPolicy {
				t.Errorf("%s: %s row = %s/%q, want absent/disabled_by_policy", when, c, h.State, h.Detail)
			}
		}
	}
	apply := func(raw []byte) {
		t.Helper()
		res := svc.store.Apply(raw)
		if res.Err != nil || res.Outcome != policy.OutcomeAccepted {
			t.Fatalf("bundle not accepted: %+v", res)
		}
		svc.policyFetched(res)
	}

	assertOff("started with inspection off")
	trust.mu.Lock()
	installs := trust.installs
	trust.mu.Unlock()
	if installs != 0 {
		t.Fatalf("the root was installed %d times at a start with inspection off", installs)
	}

	apply(signedInterceptionBundle(t, priv, "6", true, proxyListen, pacListen))
	trust.mu.Lock()
	installed := len(trust.installed) > 0
	trust.mu.Unlock()
	if !installed {
		t.Error("inspection on: the per-device root is not in the trust store")
	}
	for _, f := range []string{profile, caBundle} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("inspection on: the CLI shim's %s is missing: %v", filepath.Base(f), err)
		}
	}
	if b, err := os.ReadFile(profile); err != nil || !strings.Contains(string(b), "http://"+proxyListen) {
		t.Errorf("inspection on: the shim profile does not point at the proxy %s: %v", proxyListen, err)
	}
	if addr := svc.tlsProv.ListenAddr(); addr != proxyListen {
		t.Errorf("inspection on: proxy.tls listens on %q, want %s", addr, proxyListen)
	}
	if u := user.autoConfigURL(); !strings.HasPrefix(u, "http://"+pacListen+"/proxy.pac") {
		t.Errorf("inspection on: the user's AutoConfigURL is %q, want the PAC on %s", u, pacListen)
	}
	for _, c := range interceptors {
		if h, _ := svc.reg.HealthFor(c); h.State == protocol.StateAbsent {
			t.Errorf("inspection on: %s row = %s/%q, want it running", c, h.State, h.Detail)
		}
	}

	apply(signedInterceptionBundle(t, priv, "7", false, proxyListen, pacListen))
	assertOff("inspection turned off again")
	trust.mu.Lock()
	removes := trust.removes
	trust.mu.Unlock()
	if removes == 0 {
		t.Error("inspection off: the root was not removed")
	}

	if err := svc.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	stopped = true
}
