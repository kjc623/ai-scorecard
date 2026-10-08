package winproxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// The PAC is the desktop_proxy collector and a policy toggle: on only with TLS inspection on, a
// pac_listen to serve on, and no kill switch in force for proxy.tls.
func TestServerIsToggledByTLSInspectionAndPacListen(t *testing.T) {
	var p core.Provider = testServer(newSeam())
	if p.Name() != protocol.CollectorDesktopProxy {
		t.Fatalf("Name = %q, want desktop_proxy", p.Name())
	}
	toggled, ok := p.(core.Toggled)
	if !ok {
		t.Fatal("the PAC is not a policy toggle")
	}
	with := func(mutate func(*policy.Bundle)) *policy.Bundle {
		b := testBundle()
		mutate(b)
		return b
	}
	cases := []struct {
		name string
		b    *policy.Bundle
		want bool
	}{
		{"no bundle", nil, false},
		{"inspection on", testBundle(), true},
		{"inspection off", with(func(b *policy.Bundle) { b.Interception.Enabled = false }), false},
		{"no pac_listen", with(func(b *policy.Bundle) { b.Interception.PacListen = "" }), false},
		{"proxy killed", with(func(b *policy.Bundle) {
			b.KillSwitches = []policy.KillSwitch{{Provider: protocol.RouteProxyTLS, Mode: policy.KillDisable, EffectiveAt: time.Unix(1, 0)}}
		}), false},
		{"kill switch not yet in force", with(func(b *policy.Bundle) {
			b.KillSwitches = []policy.KillSwitch{{Provider: protocol.RouteProxyTLS, Mode: policy.KillDisable, EffectiveAt: time.Now().Add(time.Hour)}}
		}), true},
	}
	for _, c := range cases {
		if got := toggled.Enabled(c.b); got != c.want {
			t.Errorf("%s: Enabled = %t, want %t", c.name, got, c.want)
		}
	}
}

// Without a pac_listen the PAC does not start and changes no user's settings; the 8350 fallback is
// gone.
func TestServerWithoutPacListenDoesNotStart(t *testing.T) {
	s := newSeam()
	b := testBundle()
	b.Interception.PacListen = ""
	srv := testServer(s)
	srv.cfg.Bundles = func() *policy.Bundle { return b }
	if err := srv.Start(context.Background()); err == nil {
		t.Fatal("the PAC started with no pac_listen")
	}
	if len(s.regs) != 0 {
		t.Fatal("a user's settings were opened by a PAC that did not start")
	}
	if h := srv.Health(); h.State != protocol.StateAbsent {
		t.Fatalf("health = %s, want absent", h.State)
	}
}

// Each Start reads pac_listen from the bundle then in force, so a Stop and a Start (a policy toggle
// off and on) serve the PAC on a changed address and apply it to the signed-in users again.
func TestServerStartReadsPacListenEachTime(t *testing.T) {
	s := newSeam()
	ctx := context.Background()
	first, second := freeLoopbackPort(t), freeLoopbackPort(t)
	b := testBundle()
	b.Interception.PacListen = "127.0.0.1:" + first
	srv := testServer(s)
	srv.cfg.Bundles = func() *policy.Bundle { return b }

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	reg := s.regs["S-1-5-21-1000"]
	if got := reg.strings["AutoConfigURL"]; !strings.HasPrefix(got, "http://127.0.0.1:"+first+"/proxy.pac") {
		t.Fatalf("AutoConfigURL = %q, want the PAC on port %s", got, first)
	}
	if h := srv.Health(); h.State != protocol.StateHealthy {
		t.Fatalf("health while served = %s/%s, want healthy", h.State, h.Detail)
	}
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, ok := reg.strings["AutoConfigURL"]; ok {
		t.Fatal("AutoConfigURL not removed on Stop")
	}
	if h := srv.Health(); h.State != protocol.StateAbsent {
		t.Fatalf("health after Stop = %s, want absent", h.State)
	}

	b.Interception.PacListen = "127.0.0.1:" + second
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if got := reg.strings["AutoConfigURL"]; !strings.HasPrefix(got, "http://127.0.0.1:"+second+"/proxy.pac") {
		t.Fatalf("AutoConfigURL after the second Start = %q, want the PAC on port %s", got, second)
	}
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if _, ok := reg.strings["AutoConfigURL"]; ok {
		t.Fatal("AutoConfigURL not removed on the second Stop")
	}
}

// With proxy.tls out of the path the PAC routes everything as before, and says so.
func TestServerHealthWithTheProxyOutOfPath(t *testing.T) {
	srv := testServer(newSeam())
	srv.cfg.ProxyAddr = func() string { return "" }
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = srv.Stop(context.Background()) }()
	if h := srv.Health(); h.State != protocol.StateDegraded || h.Detail != protocol.DetailNotEffectiveProxy {
		t.Fatalf("health = %s/%s, want degraded/not_effective_proxy", h.State, h.Detail)
	}
}

func freeLoopbackPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

func testBundle() *policy.Bundle {
	return &policy.Bundle{
		Version:       "1",
		TenantDefault: protocol.ModeM3,
		Interception: policy.Interception{
			Enabled:     true,
			SeedHosts:   []string{"chatgpt.com"},
			TenantHosts: []string{".corp.example"},
			PacListen:   "127.0.0.1:0",
		},
	}
}

type seam struct {
	users    []string
	regs     map[string]*fakeReg
	fetched  map[string]string
	fetchErr map[string]error
}

func (s *seam) open(sid string) (Registry, error) {
	if r, ok := s.regs[sid]; ok {
		return r, nil
	}
	r := newFakeReg()
	s.regs[sid] = r
	return r, nil
}

func (s *seam) fetch(ctx context.Context, u string) ([]byte, error) {
	if err, ok := s.fetchErr[u]; ok {
		return nil, err
	}
	if body, ok := s.fetched[u]; ok {
		return []byte(body), nil
	}
	return nil, nil
}

func newSeam() *seam {
	return &seam{
		users:    []string{"S-1-5-21-1000"},
		regs:     map[string]*fakeReg{},
		fetched:  map[string]string{},
		fetchErr: map[string]error{},
	}
}

func testServer(s *seam) *Server {
	return New(Config{
		Bundles:      func() *policy.Bundle { return testBundle() },
		ProxyAddr:    func() string { return "127.0.0.1:8843" },
		Users:        func(context.Context) ([]string, error) { return s.users, nil },
		OpenSettings: s.open,
		FetchPAC:     s.fetch,
	})
}

func TestServerStartStopAppliesAndRestores(t *testing.T) {
	s := newSeam()
	srv := testServer(s)
	ctx := context.Background()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	reg := s.regs["S-1-5-21-1000"]
	if reg == nil {
		t.Fatal("no settings written for the signed-in user")
	}
	u := reg.strings["AutoConfigURL"]
	if !strings.Contains(u, "/proxy.pac?u=S-1-5-21-1000") {
		t.Fatalf("AutoConfigURL = %q", u)
	}
	if !strings.HasPrefix(u, "http://127.0.0.1:") {
		t.Fatalf("AutoConfigURL is not a loopback PAC URL: %q", u)
	}

	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, ok := reg.strings["AutoConfigURL"]; ok {
		t.Fatal("AutoConfigURL not removed on Stop")
	}
	if !reg.deleted["AutoConfigURL"] {
		t.Fatal("Delete not called on Stop for an originally-absent value")
	}
}

func TestServerRestoresExistingPAC(t *testing.T) {
	s := newSeam()
	s.regs["S-1-5-21-1000"] = newFakeReg()
	s.regs["S-1-5-21-1000"].strings["AutoConfigURL"] = "http://corp/proxy.pac"
	s.fetched["http://corp/proxy.pac"] = `function FindProxyForURL(u,h){ return "DIRECT"; }`

	srv := testServer(s)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := s.regs["S-1-5-21-1000"].strings["AutoConfigURL"]; got != "http://corp/proxy.pac" {
		t.Fatalf("original AutoConfigURL not restored: %q", got)
	}
}

func TestServerSkipsUnreadableExistingPAC(t *testing.T) {
	s := newSeam()
	s.regs["S-1-5-21-1000"] = newFakeReg()
	s.regs["S-1-5-21-1000"].strings["AutoConfigURL"] = "http://corp/proxy.pac"
	s.fetchErr["http://corp/proxy.pac"] = errUnsupported

	srv := testServer(s)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The user's AutoConfigURL must be untouched, and nothing recorded to restore.
	if got := s.regs["S-1-5-21-1000"].strings["AutoConfigURL"]; got != "http://corp/proxy.pac" {
		t.Fatalf("AutoConfigURL was overwritten despite an unreadable existing PAC: %q", got)
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestServerSkipsNonDelegatableExistingPAC(t *testing.T) {
	s := newSeam()
	s.regs["S-1-5-21-1000"] = newFakeReg()
	s.regs["S-1-5-21-1000"].strings["AutoConfigURL"] = "http://corp/proxy.pac"
	s.fetched["http://corp/proxy.pac"] = "var FindProxyForURL = function(u,h){ return \"DIRECT\"; };"

	srv := testServer(s)
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := s.regs["S-1-5-21-1000"].strings["AutoConfigURL"]; got != "http://corp/proxy.pac" {
		t.Fatalf("AutoConfigURL was overwritten despite a non-delegatable existing PAC: %q", got)
	}
	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestServerReconcileAddsAndDropsUsers(t *testing.T) {
	s := newSeam()
	srv := testServer(s)
	ctx := context.Background()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// A second user signs in; the first signs out.
	s.users = []string{"S-1-5-21-2000"}
	if err := srv.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, ok := s.regs["S-1-5-21-1000"].strings["AutoConfigURL"]; ok {
		t.Fatal("signed-out user's AutoConfigURL not removed")
	}
	if got := s.regs["S-1-5-21-2000"].strings["AutoConfigURL"]; !strings.Contains(got, "u=S-1-5-21-2000") {
		t.Fatalf("newly signed-in user not applied: %q", got)
	}

	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestHandlePAC(t *testing.T) {
	srv := testServer(newSeam())
	srv.originals["S-1-5-21-1000"] = Original{}

	req := httptest.NewRequest(http.MethodGet, "/proxy.pac?u=S-1-5-21-1000", nil)
	rec := httptest.NewRecorder()
	srv.handlePAC(rec, req)

	body := rec.Body.String()
	if rec.Header().Get("Content-Type") != "application/x-ns-proxy-autoconfig" {
		t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(body, `return "PROXY 127.0.0.1:8843";`) {
		t.Fatalf("known user's PAC does not route to the proxy:\n%s", body)
	}
	if !strings.Contains(body, `"chatgpt.com":1`) || !strings.Contains(body, `".corp.example"`) {
		t.Fatalf("interception hosts missing:\n%s", body)
	}

	// An unknown user gets a PAC that goes direct for everything.
	req2 := httptest.NewRequest(http.MethodGet, "/proxy.pac?u=S-1-5-21-9999", nil)
	rec2 := httptest.NewRecorder()
	srv.handlePAC(rec2, req2)
	if strings.Contains(rec2.Body.String(), "PROXY 127.0.0.1:8843") {
		t.Fatalf("unknown user must not be intercepted:\n%s", rec2.Body.String())
	}
}

func TestHandlePACProxyOutOfPath(t *testing.T) {
	srv := testServer(newSeam())
	srv.originals["S-1-5-21-1000"] = Original{ProxyEnable: true, ProxyServer: "proxy.corp:8080"}
	// The proxy drops out of the path.
	srv.cfg.ProxyAddr = func() string { return "" }

	req := httptest.NewRequest(http.MethodGet, "/proxy.pac?u=S-1-5-21-1000", nil)
	rec := httptest.NewRecorder()
	srv.handlePAC(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "__sacIntercept") {
		t.Fatalf("proxy out of path must not intercept:\n%s", body)
	}
	if !strings.Contains(body, "PROXY proxy.corp:8080") {
		t.Fatalf("original route lost when proxy is out of path:\n%s", body)
	}
}
