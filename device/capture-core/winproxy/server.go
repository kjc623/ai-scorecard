package winproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/core"
	"github.com/shadow-ai-capture/device/capture-core/policy"
	"github.com/shadow-ai-capture/device/protocol"
)

// Config carries the PAC server's inputs and its operating-system seams. Everything that is
// policy (the interception hosts, the listen address) or runtime state (the proxy's bound address)
// is read through functions so the PAC is rendered fresh on every request rather than cached stale.
type Config struct {
	// Bundles returns the bundle in force: the interception hosts, and the pac_listen address the
	// PAC is served on, read at each Start. Only its port is used; the PAC is always bound to
	// 127.0.0.1 so it is never reachable off the device.
	Bundles func() *policy.Bundle
	// ProxyAddr returns proxy.tls's bound loopback address; empty means it is not in the path.
	ProxyAddr func() string
	// Users, OpenSettings and FetchPAC are the operating-system seams; nil selects the platform
	// default (Windows). A test overrides them so nothing touches the machine.
	Users        func(context.Context) ([]string, error)
	OpenSettings func(sid string) (Registry, error)
	FetchPAC     func(ctx context.Context, u string) ([]byte, error)

	Log   core.Logger
	Clock func() time.Time
}

// Server is the desktop_proxy collector: it serves the PAC and keeps each signed-in user's
// AutoConfigURL pointed at it. It fails open: stopping it (or a crash of the whole agent) leaves
// the PAC returning the original route, and a clean Stop restores every user's previous
// AutoConfigURL. It observes nothing itself; the traffic it routes is observed by proxy.tls.
type Server struct {
	cfg      Config
	counters *core.CounterSet

	mu          sync.Mutex
	startedAt   time.Time
	lastSuccess time.Time
	listener    net.Listener
	http        *http.Server
	pacURL      string
	originals   map[string]Original
	restores    map[string]Restore
	skips       map[string]string
	started     bool
	stopped     bool
}

// New returns an unstarted server.
func New(cfg Config) *Server {
	if cfg.Log == nil {
		cfg.Log = nopLogger{}
	}
	if cfg.Bundles == nil {
		cfg.Bundles = func() *policy.Bundle { return nil }
	}
	if cfg.ProxyAddr == nil {
		cfg.ProxyAddr = func() string { return "" }
	}
	if cfg.Users == nil {
		cfg.Users = signedInUsers
	}
	if cfg.OpenSettings == nil {
		cfg.OpenSettings = openInternetSettings
	}
	if cfg.FetchPAC == nil {
		cfg.FetchPAC = fetchPAC
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	now := cfg.Clock()
	return &Server{
		cfg:       cfg,
		counters:  core.NewCounterSet(now),
		startedAt: now,
		originals: map[string]Original{},
		restores:  map[string]Restore{},
		skips:     map[string]string{},
	}
}

// nopLogger drops the provider's logs when none is configured.
type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// Name implements core.Provider.
func (s *Server) Name() protocol.Collector { return protocol.CollectorDesktopProxy }

// Enabled implements core.Toggled. The PAC runs only while the tenant's TLS inspection is on and the
// bundle names the address to serve it on (an empty pac_listen turns the desktop-app path off), and
// not while a kill switch suppresses proxy.tls: with nothing to route to, no user's proxy settings
// are changed.
func (s *Server) Enabled(b *policy.Bundle) bool {
	if b == nil || !b.Interception.Enabled || strings.TrimSpace(b.Interception.PacListen) == "" {
		return false
	}
	if ks, ok := b.KillSwitchFor(protocol.RouteProxyTLS); ok && ks.Mode == policy.KillDisable {
		return !ks.EffectiveAt.IsZero() && ks.EffectiveAt.After(s.cfg.Clock())
	}
	return true
}

// ApplyPolicy implements core.Provider. The hosts are read on every PAC request and the listen
// address at the next Start, so a bundle changes nothing in a running server.
func (s *Server) ApplyPolicy(policy.Bundle) error { return nil }

// Health implements core.Provider. The PAC is healthy while it is served and the proxy it routes
// to is in the path; with the proxy out of the path it serves every user's original route, which
// is degraded with not_effective_proxy.
func (s *Server) Health() core.Health {
	s.mu.Lock()
	running := s.started && !s.stopped && s.listener != nil
	since, last := s.startedAt, s.lastSuccess
	s.mu.Unlock()
	switch {
	case !running:
		return s.counters.Snapshot(protocol.StateAbsent, protocol.DetailNone, since, last)
	case s.cfg.ProxyAddr() == "":
		return s.counters.Snapshot(protocol.StateDegraded, protocol.DetailNotEffectiveProxy, since, last)
	default:
		return s.counters.Snapshot(protocol.StateHealthy, protocol.DetailNone, since, last)
	}
}

// Start binds the PAC listener on loopback at the bundle's pac_listen, begins serving, then points
// each signed-in user's AutoConfigURL at it. A user whose existing settings cannot be reproduced is
// left untouched (fail open) rather than risk breaking their proxy. A Start after a Stop applies
// the PAC again, on the address the bundle then in force names.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started && !s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	listen := ""
	if b := s.cfg.Bundles(); b != nil {
		listen = strings.TrimSpace(b.Interception.PacListen)
	}
	if listen == "" {
		return errors.New("winproxy: the bundle in force names no pac_listen, so the desktop-app PAC is off")
	}
	addr, err := loopbackListen(listen)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("winproxy: PAC listener: %w", err)
	}
	pacURL := pacURLFrom(ln)
	if pacURL == "" {
		_ = ln.Close()
		return fmt.Errorf("winproxy: PAC listener %s has no usable port", listen)
	}

	s.mu.Lock()
	s.listener = ln
	s.pacURL = pacURL
	s.mu.Unlock()

	// Serve before writing any setting, so a user's first PAC fetch is always answered.
	srv := &http.Server{Handler: http.HandlerFunc(s.handlePAC)}
	s.mu.Lock()
	s.http = srv
	s.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()

	if err := s.reconcile(ctx); err != nil {
		s.counters.Add(protocol.CounterErrors)
		s.cfg.Log.Printf("winproxy: applying the PAC to signed-in users: %v", err)
	}

	s.mu.Lock()
	s.started = true
	s.stopped = false
	s.startedAt = s.cfg.Clock()
	s.mu.Unlock()
	return nil
}

// Stop restores every user's previous AutoConfigURL, then stops serving. Restoring first is the
// fail-open ordering: no user is left pointing at a PAC that is about to go away.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	s.mu.Unlock()

	s.restoreAll(ctx)

	s.mu.Lock()
	srv := s.http
	s.listener = nil
	s.mu.Unlock()
	if srv != nil {
		_ = srv.Shutdown(ctx)
	}
	return nil
}

// Refresh reconciles the applied users with who is now signed in: a newly signed-in user gets the
// PAC applied, a signed-out user's previous AutoConfigURL is restored.
func (s *Server) Refresh(ctx context.Context) error {
	s.mu.Lock()
	started := s.started && !s.stopped
	s.mu.Unlock()
	if !started {
		return nil
	}
	return s.reconcile(ctx)
}

func (s *Server) reconcile(ctx context.Context) error {
	sids, err := s.cfg.Users(ctx)
	if err != nil {
		return err
	}
	now := make(map[string]bool, len(sids))
	for _, sid := range sids {
		now[sid] = true
	}

	s.mu.Lock()
	var toApply, toRestore []string
	for sid := range now {
		if _, done := s.restores[sid]; !done {
			toApply = append(toApply, sid)
		}
	}
	for sid := range s.restores {
		if !now[sid] {
			toRestore = append(toRestore, sid)
		}
	}
	pacURL := s.pacURL
	s.mu.Unlock()

	for _, sid := range toRestore {
		s.restoreOne(ctx, sid)
	}
	for _, sid := range toApply {
		if err := s.applyOne(ctx, sid, pacURL+"?u="+url.QueryEscape(sid)); err != nil {
			s.recordSkip(sid, err)
		}
	}
	return nil
}

// applyOne reads one user's existing proxy settings, fetches and inlines an existing PAC when the
// user has one, and points their AutoConfigURL at the PAC.
func (s *Server) applyOne(ctx context.Context, sid, pacURL string) error {
	reg, err := s.cfg.OpenSettings(sid)
	if err != nil {
		return err
	}
	defer reg.Close()

	orig, err := ReadOriginal(reg)
	if err != nil {
		return err
	}
	if orig.AutoConfigURL != "" {
		body, ferr := s.cfg.FetchPAC(ctx, orig.AutoConfigURL)
		if ferr != nil {
			return fmt.Errorf("cannot fetch existing PAC %q: %w", orig.AutoConfigURL, ferr)
		}
		if !delegatable(string(body)) {
			return fmt.Errorf("existing PAC %q does not declare FindProxyForURL; leaving the user's settings untouched", orig.AutoConfigURL)
		}
		orig.AutoConfigBody = string(body)
	}

	restore, err := NewSettings(reg).Apply(pacURL)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.originals[sid] = orig
	s.restores[sid] = restore
	delete(s.skips, sid)
	s.lastSuccess = s.cfg.Clock()
	s.mu.Unlock()
	s.counters.Add(protocol.CounterObserved)
	s.cfg.Log.Printf("winproxy: desktop apps for %s routed through the PAC", sid)
	return nil
}

// restoreOne puts a signed-out user's previous AutoConfigURL back.
func (s *Server) restoreOne(ctx context.Context, sid string) {
	s.mu.Lock()
	restore, ok := s.restores[sid]
	delete(s.restores, sid)
	delete(s.originals, sid)
	delete(s.skips, sid)
	s.mu.Unlock()
	if !ok {
		return
	}
	reg, err := s.cfg.OpenSettings(sid)
	if err != nil {
		s.cfg.Log.Printf("winproxy: restoring %s's AutoConfigURL: %v", sid, err)
		return
	}
	defer reg.Close()
	if err := NewSettings(reg).Restore(restore); err != nil {
		s.cfg.Log.Printf("winproxy: restoring %s's AutoConfigURL: %v", sid, err)
	}
}

// restoreAll restores every applied user, used at Stop.
func (s *Server) restoreAll(ctx context.Context) {
	s.mu.Lock()
	restores := make(map[string]Restore, len(s.restores))
	for sid, r := range s.restores {
		restores[sid] = r
	}
	s.restores = map[string]Restore{}
	s.originals = map[string]Original{}
	s.skips = map[string]string{}
	s.mu.Unlock()

	for sid, restore := range restores {
		reg, err := s.cfg.OpenSettings(sid)
		if err != nil {
			s.cfg.Log.Printf("winproxy: restoring %s's AutoConfigURL: %v", sid, err)
			continue
		}
		if err := NewSettings(reg).Restore(restore); err != nil {
			s.cfg.Log.Printf("winproxy: restoring %s's AutoConfigURL: %v", sid, err)
		}
		_ = reg.Close()
	}
}

// recordSkip logs a per-user skip once per reason, so a user with an unreadable existing PAC does
// not produce a warning every refresh interval.
func (s *Server) recordSkip(sid string, err error) {
	s.mu.Lock()
	if prev, seen := s.skips[sid]; !seen || prev != err.Error() {
		s.skips[sid] = err.Error()
		s.mu.Unlock()
		s.counters.Add(protocol.CounterNotCooperative)
		s.cfg.Log.Printf("winproxy: leaving %s's proxy settings untouched: %v", sid, err)
		return
	}
	s.mu.Unlock()
}

// handlePAC serves the PAC for one user. A request for a user we did not configure returns a PAC
// that goes direct for every host: fail open, never intercepting a user we do not know.
func (s *Server) handlePAC(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("u")
	s.mu.Lock()
	orig, ok := s.originals[sid]
	s.mu.Unlock()

	spec := Spec{}
	if ok {
		spec = Spec{
			ProxyAddr:      s.cfg.ProxyAddr(),
			InterceptHosts: s.interceptHosts(),
			Original:       orig,
		}
	}
	w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
	_, _ = w.Write(Render(spec))
}

// interceptHosts is the bundle's interception scope: tenant hosts then seed hosts.
func (s *Server) interceptHosts() []string {
	b := s.cfg.Bundles()
	if b == nil {
		return nil
	}
	out := make([]string, 0, len(b.Interception.TenantHosts)+len(b.Interception.SeedHosts))
	out = append(out, b.Interception.TenantHosts...)
	out = append(out, b.Interception.SeedHosts...)
	return out
}

// loopbackListen rewrites a host:port to bind on 127.0.0.1 with the same port, so the PAC is never
// reachable off the device whatever the policy names.
func loopbackListen(listen string) (string, error) {
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("winproxy: pac_listen %q is not a host:port: %w", listen, err)
	}
	return net.JoinHostPort("127.0.0.1", port), nil
}

// pacURLFrom returns the PAC base URL for a bound loopback listener.
func pacURLFrom(ln net.Listener) string {
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return ""
	}
	return "http://127.0.0.1:" + port + "/proxy.pac"
}
